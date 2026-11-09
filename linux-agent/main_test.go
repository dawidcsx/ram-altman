package main

import (
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "sync/atomic"
    "testing"
)

func TestParseMemInfo(t *testing.T) {
    input:="MemTotal:        32768000 kB\nMemFree:         123 kB\nMemAvailable:   16000000 kB\nCached: 200 kB\n"
    total,available,err:=parseMemInfo(strings.NewReader(input))
    if err!=nil {t.Fatal(err)}
    if total!=32768000*1024 || available!=16000000*1024 {t.Fatalf("got %d, %d",total,available)}
}
func TestParseMemInfoMissingAndInvalid(t *testing.T) {
    for _,s:=range []string{
        "MemTotal: 10000 kB\n",
        "MemTotal: 10000 kB\nMemAvailable: 20000 kB\n",
        "MemTotal: nope kB\nMemAvailable: 10 kB\n",
    } { if _,_,err:=parseMemInfo(strings.NewReader(s));err==nil {t.Errorf("accepted invalid memory info %q",s)} }
}
func TestEndpointValidation(t *testing.T) {
    for _,s:=range []string{"https://mochisoftware.com/api/ram/report","http://localhost:1234/api/ram/report","http://127.0.0.1:1234/report"} {
        if err:=validateEndpoint(s);err!=nil {t.Errorf("rejected %q: %v",s,err)}
    }
    for _,s:=range []string{"http://example.com/report","file:///tmp/report","https://user:password@example.com/report","https://example.com/report?token=hello","https://example.com/report#bad","javascript:alert(1)"} {
        if err:=validateEndpoint(s);err==nil {t.Errorf("accepted %q",s)}
    }
}
func TestDefaultOptInOffAndStoredID(t *testing.T) {
    tmp:=t.TempDir()
    p:=paths{configDir:filepath.Join(tmp,"config"), configFile:filepath.Join(tmp,"config","settings.json")}
    c,err:=loadSettings(p);if err!=nil {t.Fatal(err)}
    if c.Enabled || c.StartWithLinux || len(c.DeviceID)!=32 {t.Fatalf("unsafe initial config: %+v",c)}
    again,err:=loadSettings(p);if err!=nil{t.Fatal(err)}
    if again.DeviceID!=c.DeviceID {t.Fatal("device ID changed unexpectedly")}
    st,err:=os.Stat(p.configFile);if err!=nil{t.Fatal(err)}
    if st.Mode().Perm()&0077!=0 {t.Fatalf("file permissions not private: %v",st.Mode())}
}
func TestReportingProtocolAndOptIn(t *testing.T) {
    var requests atomic.Int32
    var got ramReport
    mock:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        requests.Add(1)
        if r.URL.Path!="/api/ram/report" || r.Method!="POST" || r.Header.Get("Content-Type")!="application/json" {t.Errorf("wrong method/path/headers: %s %s",r.Method,r.URL.Path)}
        if err:=json.NewDecoder(r.Body).Decode(&got);err!=nil {t.Errorf("bad report JSON: %v",err)}
        w.WriteHeader(http.StatusNoContent)
    }))
    defer mock.Close()
    c:=settings{DeviceID:"0123456789abcdef0123456789abcdef",Endpoint:mock.URL+"/api/ram/report"}
    a:=&agent{cfg:c}
    a.reportOnce()
    if requests.Load()!=0 {t.Fatal("agent sent data without opt-in")}
    a.cfg.Enabled=true
    a.reportOnce()
    if requests.Load()!=1 || got.DeviceID!=c.DeviceID || got.TotalBytes==0 || got.AvailableBytes>got.TotalBytes || got.ReportedAt=="" {
        t.Fatalf("incompatible report: %+v requests=%d",got,requests.Load())
    }
    if a.lastError!="" || a.lastSuccess.IsZero() {t.Fatalf("report failed: %s",a.lastError)}
}
func TestWebUIPermissions(t *testing.T) {
    a:=&agent{cfg:settings{DeviceID:"0123456789abcdef0123456789abcdef",Endpoint:defaultEndpoint},token:"secret"}
    // State cannot be read or modified without the per-session capability.
    rr:=httptest.NewRecorder()
    a.handler(rr,httptest.NewRequest("GET","http://localhost/api/state",nil))
    if rr.Code!=403 {t.Fatalf("unauthenticated GET returned %d",rr.Code)}
    rr=httptest.NewRecorder()
    a.handler(rr,httptest.NewRequest("POST","http://localhost/api/send",nil))
    if rr.Code!=403 {t.Fatalf("unauthenticated POST returned %d",rr.Code)}
    rr=httptest.NewRecorder()
    r:=httptest.NewRequest("GET","http://localhost/api/state",nil)
    r.Header.Set("X-RAM-ALTMAN-TOKEN","secret")
    a.handler(rr,r)
    if rr.Code!=200 || !strings.Contains(rr.Body.String(),"start_with_linux") {t.Fatalf("authorized GET failed: %d %s",rr.Code,rr.Body.String())}
}
