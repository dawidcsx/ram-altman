//go:build windows

// RAM Altman - a minimal Windows tray agent. Standard library only.
// It observes only physical RAM totals/availability via GlobalMemoryStatusEx.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	appName = "RAM Altman"
	exeName = "RAMAltman.exe"
	// Change this once you deploy the matching /api/ram/report backend.
	defaultEndpoint = "https://mochisoftware.com/api/ram/report"
	interval        = 5 * time.Minute
	uiFile          = "ui-session.json"
)

//go:embed assets/ramaltman.ico
var iconFile []byte

var (
	installedDir   string
	configPath     string
	startupPath    string
	instanceHandle uintptr
	app            = &agentState{}
)

type config struct {
	DeviceID         string `json:"device_id"`
	Endpoint         string `json:"endpoint"`
	Enabled          bool   `json:"enabled"`
	StartWithWindows bool   `json:"start_with_windows"`
}

type agentState struct {
	mu            sync.Mutex
	cfg           config
	lastAttempt   time.Time
	lastSuccess   time.Time
	lastError     string
	lastTotal     uint64
	lastAvailable uint64
	pending       context.CancelFunc
	reporting     bool
	url           string
	token         string
}

type snapshot struct {
	Enabled          bool   `json:"enabled"`
	StartWithWindows bool   `json:"start_with_windows"`
	Endpoint         string `json:"endpoint"`
	LastAttempt      string `json:"last_attempt"`
	LastSuccess      string `json:"last_success"`
	LastError        string `json:"last_error"`
	TotalBytes       uint64 `json:"total_bytes"`
	AvailableBytes   uint64 `json:"available_bytes"`
	IntervalMinutes  int    `json:"interval_minutes"`
	Reporting        bool   `json:"reporting"`
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomSession() string { return newID() + newID() }

func paths() error {
	home := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")
	if home == "" || roaming == "" {
		return errors.New("Windows LOCALAPPDATA or APPDATA not available")
	}
	installedDir = filepath.Join(home, "RAM Altman")
	configPath = filepath.Join(installedDir, "settings.json")
	startupPath = filepath.Join(roaming, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "RAM Altman.lnk")
	return os.MkdirAll(installedDir, 0700)
}

func saveConfig(c config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func loadConfig() config {
	c := config{Endpoint: defaultEndpoint}
	if b, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.DeviceID == "" {
		c.DeviceID = newID()
	}
	if c.Endpoint == "" {
		c.Endpoint = defaultEndpoint
	}
	// No reporting on first launch until the user explicitly opts in.
	_ = saveConfig(c)
	return c
}

func installSelf() (bool, error) {
	current, err := os.Executable()
	if err != nil {
		return false, err
	}
	target := filepath.Join(installedDir, exeName)
	a, err := filepath.Abs(current)
	if err != nil {
		return false, err
	}
	b, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(a, b) {
		return false, nil
	}
	// Installer is one EXE: copy into per-user storage; no elevation required.
	in, err := os.Open(current)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp := target + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, err
	}
	if err = out.Close(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	// Windows will reject replacing a currently running executable, so preserve the old installed copy if active.
	if err = os.Rename(tmp, target); err != nil {
		if _, oldErr := os.Stat(target); oldErr != nil {
			return false, err
		}
		os.Remove(tmp)
	}
	if err := os.WriteFile(filepath.Join(installedDir, "ramaltman.ico"), iconFile, 0600); err != nil {
		return false, err
	}
	cmd := exec.Command(target, "--first-run")
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return true, nil
}

func createShortcut(path, arguments string) error {
	target := filepath.Join(installedDir, exeName)
	psString := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	script := "$w = New-Object -ComObject WScript.Shell; " +
		"$s = $w.CreateShortcut(" + psString(path) + "); " +
		"$s.TargetPath = " + psString(target) + "; " +
		"$s.Arguments = " + psString(arguments) + "; " +
		"$s.WorkingDirectory = " + psString(installedDir) + "; " +
		"$s.IconLocation = " + psString(filepath.Join(installedDir, "ramaltman.ico")) + "; " +
		"$s.Save()"
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Windows shortcut: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func startMenuPath() string {
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "RAM Altman.lnk")
}

func ensureStartMenu() error {
	if _, err := os.Stat(startMenuPath()); err == nil {
		return nil
	}
	return createShortcut(startMenuPath(), "")
}

func enableStartup(enable bool) error {
	legacy := strings.TrimSuffix(startupPath, ".lnk") + ".vbs"
	_ = os.Remove(legacy)
	if !enable {
		if err := os.Remove(startupPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if _, err := os.Stat(startupPath); err == nil {
		return nil
	}
	// One-time PowerShell creates an ordinary Startup .lnk; no VBS or registry writes.
	return createShortcut(startupPath, "--background")
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("enter a valid server URL")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return errors.New("HTTPS is required (HTTP only allowed for localhost testing)")
	}
	if u.RawQuery != "" {
		return errors.New("remove the query string from the endpoint URL")
	}
	return nil
}

// MEMORYSTATUSEX mirrors the 64-byte Win32 struct exactly.
type memoryStatus struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	kernel32   = syscall.NewLazyDLL("kernel32.dll")
	procMemory = kernel32.NewProc("GlobalMemoryStatusEx")
)

func physicalRAM() (total, available uint64, err error) {
	var s memoryStatus
	s.Length = uint32(unsafe.Sizeof(s))
	ok, _, callErr := procMemory.Call(uintptr(unsafe.Pointer(&s)))
	if ok == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx: %v", callErr)
	}
	return s.TotalPhys, s.AvailPhys, nil
}

func setStatus(total, avail uint64, err error, successful bool) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.reporting = false
	app.pending = nil
	app.lastTotal = total
	app.lastAvailable = avail
	if err != nil {
		app.lastError = err.Error()
	} else {
		app.lastError = ""
		if successful {
			app.lastSuccess = time.Now()
		}
	}
}

func reportOnce() {
	app.mu.Lock()
	if !app.cfg.Enabled || app.reporting {
		app.mu.Unlock()
		return
	}
	app.reporting = true
	app.lastAttempt = time.Now()
	c := app.cfg
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	app.pending = cancel
	app.mu.Unlock()
	defer cancel()

	total, available, err := physicalRAM()
	if err != nil {
		setStatus(0, 0, err, false)
		return
	}
	report := struct {
		DeviceID       string `json:"device_id"`
		TotalBytes     uint64 `json:"total_bytes"`
		AvailableBytes uint64 `json:"available_bytes"`
		ReportedAt     string `json:"reported_at"`
	}{c.DeviceID, total, available, time.Now().UTC().Format(time.RFC3339)}
	data, _ := json.Marshal(report)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(data))
	if err != nil {
		setStatus(total, available, err, false)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RAMAltman/1.0 (+Mochi Software; opt-in)")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		setStatus(total, available, err, false)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		setStatus(total, available, fmt.Errorf("server returned HTTP %d", resp.StatusCode), false)
		return
	}
	setStatus(total, available, nil, true)
}

func stopReporting() {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.pending != nil {
		app.pending()
	}
}

func reportingLoop() {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	app.mu.Lock()
	enabled := app.cfg.Enabled
	app.mu.Unlock()
	if enabled {
		go reportOnce()
	}
	for range ticker.C {
		go reportOnce()
	}
}

func viewState() snapshot {
	currentTotal, currentAvailable, readErr := physicalRAM()
	app.mu.Lock()
	defer app.mu.Unlock()
	result := snapshot{Enabled: app.cfg.Enabled, StartWithWindows: app.cfg.StartWithWindows,
		Endpoint: app.cfg.Endpoint, LastError: app.lastError, TotalBytes: app.lastTotal,
		AvailableBytes: app.lastAvailable, IntervalMinutes: 5, Reporting: app.reporting}
	if readErr == nil {
		result.TotalBytes = currentTotal
		result.AvailableBytes = currentAvailable
	}
	if !app.lastAttempt.IsZero() {
		result.LastAttempt = app.lastAttempt.Format(time.RFC3339)
	}
	if !app.lastSuccess.IsZero() {
		result.LastSuccess = app.lastSuccess.Format(time.RFC3339)
	}
	return result
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
}

func authorized(r *http.Request) bool {
	app.mu.Lock()
	token := app.token
	app.mu.Unlock()
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		return r.URL.Query().Get("token") == token
	}
	return r.Header.Get("X-RAM-ALTMAN-TOKEN") == token
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func uiHandler(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if !authorized(r) {
		http.Error(w, "Not authorised", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		app.mu.Lock()
		token := app.token
		app.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = template.Must(template.New("ui").Parse(settingsHTML)).Execute(w, struct{ Token string }{token})
	case r.Method == http.MethodGet && r.URL.Path == "/api/state":
		writeJSON(w, viewState())
	case r.Method == http.MethodPost && r.URL.Path == "/api/settings":
		var in struct {
			Enabled          bool   `json:"enabled"`
			StartWithWindows bool   `json:"start_with_windows"`
			Endpoint         string `json:"endpoint"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Invalid settings", 400)
			return
		}
		in.Endpoint = strings.TrimSpace(in.Endpoint)
		if err := validateEndpoint(in.Endpoint); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		app.mu.Lock()
		before := app.cfg
		next := before
		next.Enabled = in.Enabled
		next.StartWithWindows = in.StartWithWindows
		next.Endpoint = in.Endpoint
		if err := enableStartup(next.StartWithWindows); err != nil {
			app.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		if err := saveConfig(next); err != nil {
			_ = enableStartup(before.StartWithWindows)
			app.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		app.cfg = next
		if !next.Enabled && app.pending != nil {
			app.pending()
		}
		app.mu.Unlock()
		if next.Enabled && (!before.Enabled || next.Endpoint != before.Endpoint) {
			go reportOnce()
		}
		writeJSON(w, map[string]string{"status": "saved"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/send":
		app.mu.Lock()
		enabled := app.cfg.Enabled
		app.mu.Unlock()
		if !enabled {
			http.Error(w, "Enable contribution first", 400)
			return
		}
		go reportOnce()
		writeJSON(w, map[string]string{"status": "sending"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/uninstall":
		writeJSON(w, map[string]string{"status": "uninstalling"})
		go func() { time.Sleep(250 * time.Millisecond); uninstall() }()
	case r.Method == http.MethodPost && r.URL.Path == "/api/exit":
		writeJSON(w, map[string]string{"status": "exiting"})
		go func() { time.Sleep(100 * time.Millisecond); postExit() }()
	default:
		http.NotFound(w, r)
	}
}

func runWebUI() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	token := randomSession()
	address := "http://" + listener.Addr().String() + "/?token=" + token
	app.mu.Lock()
	app.token = token
	app.url = address
	app.mu.Unlock()
	sessionBytes, _ := json.Marshal(map[string]string{"url": address})
	if err := os.WriteFile(filepath.Join(installedDir, uiFile), sessionBytes, 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: http.HandlerFunc(uiHandler), ReadHeaderTimeout: 3 * time.Second, MaxHeaderBytes: 8192}
	go func() { _ = server.Serve(listener) }()
	return nil
}

func openUI() {
	app.mu.Lock()
	u := app.url
	app.mu.Unlock()
	if u != "" {
		_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u).Start()
	}
}

func openExistingUI() {
	b, err := os.ReadFile(filepath.Join(installedDir, uiFile))
	if err != nil {
		return
	}
	var v struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(b, &v) == nil && strings.HasPrefix(v.URL, "http://127.0.0.1:") {
		_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", v.URL).Start()
	}
}

func uninstall() {
	stopReporting()
	_ = enableStartup(false)
	_ = os.Remove(startMenuPath())
	// The running EXE cannot delete itself on Windows. Use a temporary
	// one-shot batch file to remove the app folder after this process exits.
	dir := installedDir
	batch := filepath.Join(os.TempDir(), "ram-altman-uninstall-"+newID()+".cmd")
	// %~f0 refers to this temporary script; no user-supplied shell commands.
	script := "@echo off\r\n" +
		"timeout /t 2 /nobreak >nul\r\n" +
		"rmdir /s /q \"" + dir + "\"\r\n" +
		"del /f /q \"%~f0\"\r\n"
	// Windows path quoting: quotes are prohibited in Windows file names.
	if err := os.WriteFile(batch, []byte(script), 0600); err != nil {
		showMessage("Uninstall could not start: " + err.Error())
		return
	}
	cmd := exec.Command("cmd.exe", "/D", "/C", "call \""+batch+"\"")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		showMessage("Uninstall could not start: " + err.Error())
		return
	}
	postExit()
}

func main() {
	runtime.LockOSThread()
	if err := paths(); err != nil {
		showMessage("Unable to start: " + err.Error())
		return
	}
	copied, err := installSelf()
	if err != nil {
		showMessage("Unable to install: " + err.Error())
		return
	}
	if copied {
		return
	}
	single, err := acquireSingleInstance()
	if err != nil {
		showMessage("Unable to start: " + err.Error())
		return
	}
	if !single {
		openExistingUI()
		return
	}
	defer closeInstance()
	app.cfg = loadConfig()
	_ = ensureStartMenu()
	// Respect the on-disk toggle from a previous session.
	if err := enableStartup(app.cfg.StartWithWindows); err != nil {
		app.cfg.StartWithWindows = false
		_ = saveConfig(app.cfg)
	}
	if err := runWebUI(); err != nil {
		showMessage("Unable to launch settings: " + err.Error())
		return
	}
	go reportingLoop()
	// --background means the browser is not opened at Windows startup.
	background := false
	for _, a := range os.Args[1:] {
		if a == "--background" {
			background = true
		}
	}
	if !background {
		go func() { time.Sleep(250 * time.Millisecond); openUI() }()
	}
	runTray()
	_ = os.Remove(filepath.Join(installedDir, uiFile))
}
