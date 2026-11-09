// RAM Altman Linux agent: intentionally small, explicitly opt-in, no dependencies.
// Collects only MemTotal and MemAvailable from /proc/meminfo.
// Compatible with the RAM Altman v1.0 server and the Windows tray application.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	version           = "1.0.0"
	defaultEndpoint   = "https://mochisoftware.com/api/ram/report"
	reportingInterval = 5 * time.Minute
	serviceName       = "ram-altman.service"
)

//go:embed settings.html
var settingsHTML string

type settings struct {
	DeviceID       string `json:"device_id"`
	Endpoint       string `json:"endpoint"`
	Enabled        bool   `json:"enabled"`
	StartWithLinux bool   `json:"start_with_linux"`
}

type ramReport struct {
	DeviceID       string `json:"device_id"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	ReportedAt     string `json:"reported_at"`
}

type session struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type snapshot struct {
	Enabled         bool   `json:"enabled"`
	StartWithLinux  bool   `json:"start_with_linux"`
	Endpoint        string `json:"endpoint"`
	TotalBytes      uint64 `json:"total_bytes"`
	AvailableBytes  uint64 `json:"available_bytes"`
	LastAttempt     string `json:"last_attempt"`
	LastSuccess     string `json:"last_success"`
	LastError       string `json:"last_error"`
	Reporting       bool   `json:"reporting"`
	IntervalMinutes int    `json:"interval_minutes"`
}

type paths struct {
	configDir   string
	configFile  string
	stateDir    string
	sessionFile string
	lockFile    string
	exeFile     string
	unitFile    string
	desktopFile string
	iconFile    string
}

type agent struct {
	mu          sync.Mutex
	cfg         settings
	paths       paths
	token       string
	server      *http.Server
	listener    net.Listener
	running     bool
	pending     context.CancelFunc
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func configLocation() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	cd := os.Getenv("XDG_CONFIG_HOME")
	if cd == "" {
		cd = filepath.Join(home, ".config")
	}
	sd := os.Getenv("XDG_STATE_HOME")
	if sd == "" {
		sd = filepath.Join(home, ".local", "state")
	}
	p := paths{configDir: filepath.Join(cd, "ram-altman"), stateDir: filepath.Join(sd, "ram-altman"), exeFile: filepath.Join(home, ".local", "bin", "ram-altman"), unitFile: filepath.Join(cd, "systemd", "user", serviceName)}
	p.desktopFile = filepath.Join(home, ".local", "share", "applications", "ram-altman.desktop")
	p.iconFile = filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps", "ram-altman.svg")
	p.configFile = filepath.Join(p.configDir, "settings.json")
	p.sessionFile = filepath.Join(p.stateDir, "session.json")
	p.lockFile = filepath.Join(p.stateDir, "agent.lock")
	return p, nil
}

func atomicJSON(filename string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(filename), ".ram-altman-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err = temp.Write(b); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filename)
}

func loadSettings(p paths) (settings, error) {
	b, err := os.ReadFile(p.configFile)
	if errors.Is(err, os.ErrNotExist) {
		id, err := randomHex(16)
		if err != nil {
			return settings{}, err
		}
		c := settings{DeviceID: id, Endpoint: defaultEndpoint}
		if err := atomicJSON(p.configFile, c); err != nil {
			return settings{}, err
		}
		return c, nil
	}
	if err != nil {
		return settings{}, err
	}
	var c settings
	if err := json.Unmarshal(b, &c); err != nil {
		return settings{}, fmt.Errorf("invalid settings JSON: %w", err)
	}
	if len(c.DeviceID) != 32 {
		return settings{}, fmt.Errorf("unexpected device ID length in settings; fix or reinstall")
	}
	if _, err := hex.DecodeString(c.DeviceID); err != nil {
		return settings{}, fmt.Errorf("invalid device ID: %w", err)
	}
	if c.Endpoint == "" {
		c.Endpoint = defaultEndpoint
	}
	if err := validateEndpoint(c.Endpoint); err != nil {
		return settings{}, err
	}
	return c, nil
}

func validateEndpoint(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" {
		return errors.New("please enter a valid HTTPS reporting URL")
	}
	h := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (h == "127.0.0.1" || h == "localhost" || h == "::1")) {
		return errors.New("HTTPS required (HTTP allowed only on localhost)")
	}
	return nil
}

// MemAvailable includes reclaimable caches. This is a playful estimate of
// available RAM, not a claim that Linux has actually wasted the memory.
func parseMemInfo(r io.Reader) (uint64, uint64, error) {
	b, err := io.ReadAll(io.LimitReader(r, 65536))
	if err != nil {
		return 0, 0, err
	}
	var total, available uint64
	var foundTotal, foundAvailable bool
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		if fields[0] != "MemTotal:" && fields[0] != "MemAvailable:" {
			continue
		}
		x, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || x > ^uint64(0)/1024 {
			return 0, 0, errors.New("bad /proc/meminfo value")
		}
		if fields[0] == "MemTotal:" {
			total = x * 1024
			foundTotal = true
		} else {
			available = x * 1024
			foundAvailable = true
		}
	}
	if !foundTotal || !foundAvailable || total == 0 || available > total {
		return 0, 0, errors.New("MemTotal/MemAvailable unavailable or invalid")
	}
	return total, available, nil
}
func physicalRAM() (uint64, uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseMemInfo(f)
}

func (a *agent) setResult(err error, successful bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running = false
	a.pending = nil
	if err != nil {
		a.lastError = err.Error()
		return
	}
	a.lastError = ""
	if successful {
		a.lastSuccess = time.Now()
	}
}

func (a *agent) reportOnce() {
	a.mu.Lock()
	if a.running || !a.cfg.Enabled {
		a.mu.Unlock()
		return
	}
	c := a.cfg
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	a.running = true
	a.pending = cancel
	a.lastAttempt = time.Now()
	a.mu.Unlock()
	defer cancel()
	total, available, err := physicalRAM()
	if err != nil {
		a.setResult(err, false)
		return
	}
	b, err := json.Marshal(ramReport{c.DeviceID, total, available, time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		a.setResult(err, false)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(b))
	if err != nil {
		a.setResult(err, false)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RAMAltman-Linux/1.0 (+Mochi Software; opt-in)")
	client := http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.setResult(err, false)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		a.setResult(fmt.Errorf("server returned HTTP %d", resp.StatusCode), false)
		return
	}
	a.setResult(nil, true)
}

func (a *agent) state() snapshot {
	total, available, _ := physicalRAM()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := snapshot{Enabled: a.cfg.Enabled, StartWithLinux: a.cfg.StartWithLinux, Endpoint: a.cfg.Endpoint, TotalBytes: total, AvailableBytes: available, LastError: a.lastError, Reporting: a.running, IntervalMinutes: 5}
	if !a.lastAttempt.IsZero() {
		s.LastAttempt = a.lastAttempt.Format(time.RFC3339)
	}
	if !a.lastSuccess.IsZero() {
		s.LastSuccess = a.lastSuccess.Format(time.RFC3339)
	}
	return s
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
}
func (a *agent) authorized(r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		return r.URL.Query().Get("token") == a.token
	}
	return r.Header.Get("X-RAM-ALTMAN-TOKEN") == a.token
}
func writeJSON(w http.ResponseWriter, val any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(val)
}
func (a *agent) handler(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if !a.authorized(r) {
		http.Error(w, "Not authorized", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = template.Must(template.New("ui").Parse(settingsHTML)).Execute(w, struct{ Token string }{a.token})
	case r.Method == http.MethodGet && r.URL.Path == "/api/state":
		writeJSON(w, a.state())
	case r.Method == http.MethodPost && r.URL.Path == "/api/settings":
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var in struct {
			Enabled        bool   `json:"enabled"`
			StartWithLinux bool   `json:"start_with_linux"`
			Endpoint       string `json:"endpoint"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			http.Error(w, "Invalid settings", 400)
			return
		}
		in.Endpoint = strings.TrimSpace(in.Endpoint)
		if err := validateEndpoint(in.Endpoint); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.mu.Lock()
		previous := a.cfg
		updated := previous
		updated.Enabled = in.Enabled
		updated.StartWithLinux = in.StartWithLinux
		updated.Endpoint = in.Endpoint
		if updated.StartWithLinux != previous.StartWithLinux {
			verb := "disable"
			if updated.StartWithLinux {
				verb = "enable"
			}
			cmd := exec.Command("systemctl", "--user", verb, serviceName)
			if output, err := cmd.CombinedOutput(); err != nil {
				a.mu.Unlock()
				http.Error(w, fmt.Sprintf("Autostart setting failed: %v: %s", err, strings.TrimSpace(string(output))), 500)
				return
			}
		}
		if err := atomicJSON(a.paths.configFile, updated); err != nil {
			if updated.StartWithLinux != previous.StartWithLinux {
				verb := "enable"
				if updated.StartWithLinux {
					verb = "disable"
				}
				_ = exec.Command("systemctl", "--user", verb, serviceName).Run()
			}
			a.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		a.cfg = updated
		if !updated.Enabled && a.pending != nil {
			a.pending()
		}
		a.mu.Unlock()
		if updated.Enabled && (!previous.Enabled || previous.Endpoint != updated.Endpoint) {
			go a.reportOnce()
		}
		writeJSON(w, map[string]string{"status": "saved"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/send":
		a.mu.Lock()
		enabled := a.cfg.Enabled
		a.mu.Unlock()
		if !enabled {
			http.Error(w, "Enable contribution first", 400)
			return
		}
		go a.reportOnce()
		writeJSON(w, map[string]string{"status": "sending"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/uninstall":
		if out, err := exec.Command("systemctl", "--user", "disable", serviceName).CombinedOutput(); err != nil {
			http.Error(w, fmt.Sprintf("Cannot disable autostart: %v: %s", err, strings.TrimSpace(string(out))), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "uninstalling"})
		// Complete cleanup from inside the agent, so no root/installer is needed.
		go func() { time.Sleep(250 * time.Millisecond); removeOwnFiles(a.paths); os.Exit(0) }()
	case r.Method == http.MethodPost && r.URL.Path == "/api/exit":
		writeJSON(w, map[string]string{"status": "stopped"})
		go func() { time.Sleep(250 * time.Millisecond); os.Exit(0) }()
	default:
		http.NotFound(w, r)
	}
}

func removeOwnFiles(p paths) {
	_ = os.Remove(p.sessionFile)
	_ = os.Remove(p.lockFile)
	_ = os.Remove(p.configFile)
	_ = os.Remove(p.unitFile)
	_ = os.Remove(p.exeFile)
	_ = os.Remove(p.desktopFile)
	_ = os.Remove(p.iconFile)
	_ = os.Remove(p.configDir)
	_ = os.Remove(p.stateDir)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
}

func (a *agent) run() error {
	if err := os.MkdirAll(a.paths.stateDir, 0700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(a.paths.lockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("RAM Altman is already running for this user")
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	lis, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer lis.Close()
	token, err := randomHex(32)
	if err != nil {
		return err
	}
	a.token = token
	a.listener = lis
	a.server = &http.Server{Handler: http.HandlerFunc(a.handler), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second, WriteTimeout: 8 * time.Second, IdleTimeout: 15 * time.Second}
	s := session{URL: "http://" + lis.Addr().String() + "/?token=" + token, Token: token}
	if err := atomicJSON(a.paths.sessionFile, s); err != nil {
		return err
	}
	defer os.Remove(a.paths.sessionFile)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	go func() {
		ticker := time.NewTicker(reportingInterval)
		defer ticker.Stop()
		a.mu.Lock()
		enabled := a.cfg.Enabled
		a.mu.Unlock()
		if enabled {
			go a.reportOnce()
		}
		for {
			select {
			case <-ticker.C:
				go a.reportOnce()
			case <-stop:
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = a.server.Shutdown(ctx)
				cancel()
				return
			}
		}
	}()
	log.Printf("RAM Altman Linux agent active; local settings at %s (no RAM reports until opted in)", "http://"+lis.Addr().String()+"/")
	err = a.server.Serve(lis)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func openSettings(p paths) error {
	b, err := os.ReadFile(p.sessionFile)
	if err != nil {
		if output, startErr := exec.Command("systemctl", "--user", "start", serviceName).CombinedOutput(); startErr != nil {
			return fmt.Errorf("agent is not running; could not start service: %v: %s", startErr, strings.TrimSpace(string(output)))
		}
		for i := 0; i < 25; i++ {
			time.Sleep(100 * time.Millisecond)
			b, err = os.ReadFile(p.sessionFile)
			if err == nil {
				break
			}
		}
		if err != nil {
			return errors.New("agent could not open its local settings page; check: journalctl --user -u ram-altman -n 40")
		}
	}
	var sess session
	if json.Unmarshal(b, &sess) != nil || !strings.HasPrefix(sess.URL, "http://127.0.0.1:") {
		return errors.New("invalid session file")
	}
	fmt.Println("Settings:", sess.URL)
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		if exe, err := exec.LookPath("xdg-open"); err == nil {
			cmd := exec.Command(exe, sess.URL)
			if err := cmd.Start(); err == nil {
				go cmd.Wait()
				return nil
			}
		}
	}
	fmt.Println("On a headless Linux machine, use SSH port forwarding or run the CLI locally. Never expose the settings port publicly.")
	return nil
}

func uninstall(p paths) error {
	if out, err := exec.Command("systemctl", "--user", "disable", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("disable autostart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "stop", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("stop service: %v: %s", err, strings.TrimSpace(string(out)))
	}
	removeOwnFiles(p)
	fmt.Println("RAM Altman uninstalled (app, user service, settings, and random device ID). Thanks for wasting responsibly.")
	return nil
}

// postLocal sends a command only to the current user's loopback settings server.
// A 256-bit session token is required; the endpoint is never publicly exposed.
func postLocal(p paths, path string, payload any) error {
	b, err := os.ReadFile(p.sessionFile)
	if err != nil {
		return err
	}
	var sess session
	if err = json.Unmarshal(b, &sess); err != nil {
		return err
	}
	base, err := url.Parse(sess.URL)
	if err != nil || base.Scheme != "http" || base.Hostname() != "127.0.0.1" {
		return errors.New("invalid local agent session")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+base.Host+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-RAM-ALTMAN-TOKEN", sess.Token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("local agent returned HTTP %d: %s", response.StatusCode, string(body))
	}
	return nil
}
func contributeCLI(p paths, enabled bool) error {
	c, err := loadSettings(p)
	if err != nil {
		return err
	}
	c.Enabled = enabled
	if err = postLocal(p, "/api/settings", struct {
		Enabled        bool   `json:"enabled"`
		StartWithLinux bool   `json:"start_with_linux"`
		Endpoint       string `json:"endpoint"`
	}{c.Enabled, c.StartWithLinux, c.Endpoint}); err == nil {
		fmt.Printf("RAM reporting enabled: %t (saved to the running agent)\n", enabled)
		return nil
	}
	// When the agent isn't running, save the preference for its next start.
	// Never silently overwrite settings when an active agent was reachable.
	if _, readErr := os.Stat(p.sessionFile); readErr == nil {
		return fmt.Errorf("cannot reach active agent: %w; try systemctl --user restart ram-altman", err)
	}
	if err := atomicJSON(p.configFile, c); err != nil {
		return err
	}
	if enabled {
		if out, err := exec.Command("systemctl", "--user", "start", serviceName).CombinedOutput(); err != nil {
			return fmt.Errorf("reporting enabled, but failed to start agent: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("RAM reporting enabled: %t\n", enabled)
	return nil
}

func cliStatus(p paths) error {
	c, err := loadSettings(p)
	if err != nil {
		return err
	}
	total, available, err := physicalRAM()
	if err != nil {
		return err
	}
	fmt.Printf("RAM Altman v%s\n", version)
	fmt.Printf("Reporting: %t | Start at login: %t\n", c.Enabled, c.StartWithLinux)
	fmt.Printf("Total: %.2f GiB | Available: %.2f GiB\n", float64(total)/(1<<30), float64(available)/(1<<30))
	fmt.Printf("Endpoint: %s\n", c.Endpoint)
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "RAM Altman Linux: run | open | status | contribute on|off | send | uninstall | version\nTo install, run ./install.sh from the release archive.")
	}
	flag.Parse()
	cmd := "status"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}
	p, err := configLocation()
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "run":
		cfg, err := loadSettings(p)
		if err != nil {
			log.Fatal(err)
		}
		if err := (&agent{cfg: cfg, paths: p}).run(); err != nil {
			log.Fatal(err)
		}
	case "open":
		if err := openSettings(p); err != nil {
			log.Fatal(err)
		}
	case "contribute":
		if flag.NArg() != 2 || (flag.Arg(1) != "on" && flag.Arg(1) != "off") {
			flag.Usage()
			os.Exit(2)
		}
		if err := contributeCLI(p, flag.Arg(1) == "on"); err != nil {
			log.Fatal(err)
		}
	case "send":
		if err := postLocal(p, "/api/send", map[string]string{}); err != nil {
			log.Fatal(err)
		}
		fmt.Println("RAM report queued. Sam is reviewing his options.")
	case "status":
		if err := cliStatus(p); err != nil {
			log.Fatal(err)
		}
	case "uninstall":
		if err := uninstall(p); err != nil {
			log.Fatal(err)
		}
	case "version":
		fmt.Println("RAM Altman", version)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
