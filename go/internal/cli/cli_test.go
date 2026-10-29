package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/update/updatetest"
)

// fakeAPI records requests and answers with canned JSON per "METHOD /path".
type fakeAPI struct {
	t        *testing.T
	routes   map[string]string
	requests []string
	bodies   []string
}

func newFakeAPI(t *testing.T, routes map[string]string) *httptest.Server {
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	return srv
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, key)
	f.bodies = append(f.bodies, string(body))
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	resp, ok := f.routes[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no route ` + key + `"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// "!404 {...}" answers with that status.
	if strings.HasPrefix(resp, "!") {
		code, _ := strconv.Atoi(resp[1:4])
		w.WriteHeader(code)
		resp = resp[5:]
	}
	w.Write([]byte(resp))
}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run("test", append([]string{"--json=false"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

const sandboxJSON = `{"id":"cx-abc123","name":"web","size":"standard-2","state":"running","idleMinutes":30,
"createdAt":1,"updatedAt":1,"startedAt":1,"stoppedAt":null,"savedAt":null,"savedBytes":null,"error":null}`

func TestStatusShowsAPIVersion(t *testing.T) {
	newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"1.2.3","previewZone":null}`})
	code, out, errOut := run(t, "status")
	if code != 0 || !strings.Contains(out, "version 1.2.3") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestNoDeploymentPointsAtDeploy(t *testing.T) {
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", "")
	code, _, errOut := run(t, "list")
	if code != 1 || !strings.Contains(errOut, "cloudex deploy") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestNewMakesTheSandboxCurrent(t *testing.T) {
	newFakeAPI(t, map[string]string{"POST /v1/sandboxes": sandboxJSON})
	if code, _, errOut := run(t, "new", "--name", "web", "--idle", "45m"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Current != "cx-abc123" {
		t.Fatalf("current is %q", cfg.Current)
	}
}

func TestNewSendsIdleMinutes(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{"POST /v1/sandboxes": sandboxJSON}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	if code, _, errOut := run(t, "new", "--idle", "45m"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["idleMinutes"] != float64(45) {
		t.Fatalf("body %v", body)
	}
}

func TestExecUsesCurrentSandboxAndExitCode(t *testing.T) {
	newFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/cx-abc123/exec": `{"exitCode":3,"stdout":"hello\n","stderr":"","timedOut":false,"ms":5}`,
	})
	cfg := &config.Config{Current: "cx-abc123"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "exec", "--", "echo", "hello")
	if code != 3 || out != "hello\n" {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestCommandsNeedASandbox(t *testing.T) {
	newFakeAPI(t, map[string]string{})
	code, _, errOut := run(t, "stop")
	if code != 1 || !strings.Contains(errOut, "cloudex use") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestAPIErrorsArePrinted(t *testing.T) {
	newFakeAPI(t, map[string]string{})
	code, _, errOut := run(t, "info", "nope")
	if code != 1 || !strings.Contains(errOut, "no route GET /v1/sandboxes/nope") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`it's "fine"`); got != `'it'\''s "fine"'` {
		t.Fatalf("got %s", got)
	}
}

func TestLoginSavesTheDeployment(t *testing.T) {
	srv := newFakeAPI(t, map[string]string{
		"GET /v1/health":    `{"ok":true,"version":"0.1.2","previewZone":"example.dev"}`,
		"GET /v1/sandboxes": `[]`,
	})
	t.Setenv("CLOUDEX_API_URL", "")
	code, out, errOut := run(t, "login", "--url", srv.URL+"/")
	if code != 0 || !strings.Contains(out, "Logged in to "+srv.URL) {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != srv.URL || cfg.APIKey != "test-key" || cfg.PreviewZone != "example.dev" {
		t.Fatalf("saved %+v", cfg)
	}
}

func TestLoginRejectsAWrongKey(t *testing.T) {
	srv := newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"0.1.2","previewZone":null}`})
	t.Setenv("CLOUDEX_API_KEY", "wrong")
	// The fake API wants a key even for health, so the health call fails first.
	code, _, errOut := run(t, "login", "--url", srv.URL)
	if code != 1 || errOut == "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	cfg, _ := config.Load()
	if cfg.APIKey != "" {
		t.Fatalf("saved a key that doesn't work: %+v", cfg)
	}
}

func TestStatusHidesTheKeyUnlessAsked(t *testing.T) {
	newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"1.2.3","previewZone":null}`})
	_, out, _ := run(t, "status")
	if strings.Contains(out, "test-key") {
		t.Fatalf("status printed the key: %q", out)
	}
	_, out, _ = run(t, "status", "--show-key")
	if !strings.Contains(out, "test-key") {
		t.Fatalf("status --show-key didn't print the key: %q", out)
	}
}

func TestResumeFromBackupAsksForIt(t *testing.T) {
	restored := strings.Replace(sandboxJSON, `"error":null`, `"notice":"Restored /home from the backup of 2026-10-29 03:12 UTC (as asked).","error":null`, 1)
	f := &fakeAPI{t: t, routes: map[string]string{"POST /v1/sandboxes/web/resume": restored}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	code, out, errOut := run(t, "resume", "web", "--from-backup")
	if code != 0 || !strings.Contains(out, "Restored /home") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.bodies[0] != `{"fromBackup":true}` {
		t.Fatalf("body %q", f.bodies[0])
	}
	if code, _, _ := run(t, "resume", "web"); code != 0 || f.bodies[1] != "" {
		t.Fatalf("a plain resume sent %q", f.bodies[1])
	}
}

const desktopJSON = `{"port":6080,"path":"vnc.html?autoconnect=1","size":"1280x720","installed":false,"started":true}`

func TestDesktopLinkPrintsTheViewer(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/web":                sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/desktop": desktopJSON,
		"POST /v1/sandboxes/cx-abc123/hosts":   `{"url":"https://6080-cx-abc123-0123456789.example.com"}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	opened := ""
	defer func(old func(string)) { openBrowser = old }(openBrowser)
	openBrowser = func(url string) { opened = url }

	code, out, errOut := run(t, "desktop", "web", "--link", "--size", "1920x1080")
	want := "https://6080-cx-abc123-0123456789.example.com/vnc.html?autoconnect=1"
	if code != 0 || strings.TrimSpace(out) != want || opened != want {
		t.Fatalf("code %d, stdout %q, opened %q, stderr %q", code, out, opened, errOut)
	}
	if f.bodies[1] != `{"size":"1920x1080"}` || f.bodies[2] != `{"port":6080}` {
		t.Fatalf("bodies %q", f.bodies)
	}
}

func TestDesktopStop(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/web":                  sandboxJSON,
		"DELETE /v1/sandboxes/cx-abc123/desktop": `{"stopped":true}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	code, out, errOut := run(t, "desktop", "web", "--stop")
	if code != 0 || !strings.Contains(out, "Stopped the desktop on web") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestDesktopOnAnOlderDeploymentSaysToUpdate(t *testing.T) {
	newFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web":                sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/desktop": `!404 {"error":"not found"}`,
	})
	code, _, errOut := run(t, "desktop", "web", "--no-open")
	if code != 1 || !strings.Contains(errOut, "cloudex deploy") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestDesktopRejectsABadSize(t *testing.T) {
	newFakeAPI(t, nil)
	if code, _, errOut := run(t, "desktop", "web", "--size", "big"); code != 1 || !strings.Contains(errOut, "1280x720") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

// forward should carry each local connection over its own bridge WebSocket.
func TestForwardBridgesEachConnection(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/cx-abc123/tcp/6080" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		dials.Add(1)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		guest := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		defer guest.Close()
		// Answer like websockify's web server: one HTTP/1.0 response, then close.
		bufio.NewReader(guest).ReadString('\n')
		io.WriteString(guest, "HTTP/1.0 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- forward(ctx, ln, api.New(srv.URL, "test-key"), "cx-abc123", 6080, io.Discard) }()
	for i := 0; i < 2; i++ {
		res, err := http.Get("http://" + ln.Addr().String() + "/vnc.html")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(body) != "hello" {
			t.Fatalf("got %q", body)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("%d bridge connections, want 2", n)
	}
}

// fakeExecutable points update at a temporary "cloudex" holding old.
func fakeExecutable(t *testing.T, old string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "cloudex")
	if err := os.WriteFile(target, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := executable
	t.Cleanup(func() { executable = prev })
	executable = func() (string, error) { return target, nil }
	return target
}

func runVersion(t *testing.T, version string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(version, append([]string{"--json=false"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUpdateReplacesTheCLIAndPointsAtDeploy(t *testing.T) {
	newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"v0.1.0","previewZone":null}`})
	updatetest.Server(t, "v9.9.9", []byte("new build"), "")
	target := fakeExecutable(t, "old build")

	code, out, errOut := runVersion(t, "v0.1.0", "update")
	if code != 0 || !strings.Contains(out, "Updated cloudex from v0.1.0 to v9.9.9") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, _ := os.ReadFile(target); string(got) != "new build" {
		t.Fatalf("executable holds %q", got)
	}
	// Without a terminal it doesn't run deploy, but says to.
	if !strings.Contains(out, "Your deployment runs v0.1.0. Update it with: cloudex deploy") {
		t.Fatalf("stdout %q", out)
	}
}

func TestUpdateCheckChangesNothing(t *testing.T) {
	newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"v9.9.9","previewZone":null}`})
	updatetest.Server(t, "v9.9.9", []byte("new build"), "")
	target := fakeExecutable(t, "old build")

	code, out, errOut := runVersion(t, "v0.1.0", "update", "--check")
	if code != 0 || !strings.Contains(out, "cloudex v9.9.9 is out; this is v0.1.0") || !strings.Contains(out, "Your deployment runs v9.9.9.") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, _ := os.ReadFile(target); string(got) != "old build" {
		t.Fatalf("--check replaced the executable: %q", got)
	}
}

func TestUpdateWhenCurrent(t *testing.T) {
	newFakeAPI(t, map[string]string{"GET /v1/health": `{"ok":true,"version":"v9.9.9","previewZone":null}`})
	updatetest.Server(t, "v9.9.9", []byte("new build"), "")
	fakeExecutable(t, "same build")
	code, out, _ := runVersion(t, "v9.9.9", "update")
	if code != 0 || !strings.Contains(out, "cloudex v9.9.9 is the latest version.") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestUpdateRefusesASourceBuild(t *testing.T) {
	newFakeAPI(t, nil)
	updatetest.Server(t, "v9.9.9", []byte("new build"), "")
	target := fakeExecutable(t, "dev build")
	code, _, errOut := runVersion(t, "dev", "update")
	if got, _ := os.ReadFile(target); code != 1 || !strings.Contains(errOut, "built from source") || string(got) != "dev build" {
		t.Fatalf("code %d, stderr %q, executable %q", code, errOut, got)
	}
}

func TestDesktopQualitySetsTheViewerOption(t *testing.T) {
	newFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web":                sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/desktop": `{"port":6080,"path":"vnc.html?autoconnect=1&quality=3","size":"1280x720","installed":false,"started":false}`,
		"POST /v1/sandboxes/cx-abc123/hosts":   `{"url":"https://6080-cx-abc123-0123456789.example.com"}`,
	})
	code, out, errOut := run(t, "desktop", "web", "--link", "--no-open", "--quality", "0")
	want := "https://6080-cx-abc123-0123456789.example.com/vnc.html?autoconnect=1&quality=0"
	if code != 0 || strings.TrimSpace(out) != want {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := run(t, "desktop", "web", "--quality", "12"); code != 1 || !strings.Contains(errOut, "0 (least bandwidth) to 9") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func forkJSON(id, name, state, errMsg string) string {
	e := "null"
	if errMsg != "" {
		e = strconv.Quote(errMsg)
	}
	return `{"id":"` + id + `","name":"` + name + `","size":"standard-2","state":"` + state + `","idleMinutes":30,
"createdAt":2,"updatedAt":2,"startedAt":2,"stoppedAt":null,"savedAt":null,"savedBytes":null,"forkedFrom":"cx-abc123","error":` + e + `}`
}

func withFakeAPI(t *testing.T, routes map[string]string) *fakeAPI {
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	return f
}

func TestForkMakesTheForkCurrent(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/web/fork": "!201 [" + forkJSON("cx-fff111", "try-2", "running", "") + "]",
	})
	code, out, errOut := run(t, "fork", "web", "--name", "try-2")
	if code != 0 || !strings.Contains(out, "try-2 is running (standard-2), a fork of web") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &body)
	if body["name"] != "try-2" || body["count"] != float64(1) || body["idleMinutes"] != nil || body["size"] != nil {
		t.Fatalf("request body %v", body)
	}
	cfg, _ := config.Load()
	if cfg.Current != "cx-fff111" {
		t.Fatalf("current is %q", cfg.Current)
	}
}

func TestNewFromMakesSeveralForks(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/web/fork": "!201 [" + forkJSON("cx-fff111", "agent-1", "running", "") + "," +
			forkJSON("cx-fff222", "agent-2", "running", "") + "]",
	})
	code, out, errOut := run(t, "new", "--from", "web", "--count", "2", "--name", "agent", "--idle", "1h")
	if code != 0 || !strings.Contains(out, "cx-fff111  agent-1  running") || !strings.Contains(out, "cx-fff222  agent-2  running") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &body)
	if body["count"] != float64(2) || body["idleMinutes"] != float64(60) {
		t.Fatalf("request body %v", body)
	}
	// Several forks leave the current sandbox alone.
	if cfg, _ := config.Load(); cfg.Current != "" {
		t.Fatalf("current is %q", cfg.Current)
	}
}

func TestForkReportsForksThatDidntStart(t *testing.T) {
	withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/web/fork": "!201 [" + forkJSON("cx-fff111", "a-1", "running", "") + "," +
			forkJSON("cx-fff222", "a-2", "error", "start failed: no capacity") + "]",
	})
	code, out, errOut := run(t, "fork", "web", "--count", "2", "--name", "a")
	if code != 1 || !strings.Contains(out, "error: start failed: no capacity") || !strings.Contains(errOut, "1 of 2 forks didn't start") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestForkJSONIsAList(t *testing.T) {
	withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/web/fork": "!201 [" + forkJSON("cx-fff111", "x", "running", "") + "]",
	})
	var out, errOut bytes.Buffer
	if code := Run("test", []string{"--json", "fork", "web", "--no-use"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	var forks []api.Sandbox
	if err := json.Unmarshal(out.Bytes(), &forks); err != nil || len(forks) != 1 || *forks[0].ForkedFrom != "cx-abc123" {
		t.Fatalf("%v: %s", err, out.String())
	}
}

func TestForkChecksCount(t *testing.T) {
	withFakeAPI(t, nil)
	if code, _, errOut := run(t, "fork", "web", "--count", "11"); code != 1 || !strings.Contains(errOut, "--count must be 1 to 10") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if code, _, errOut := run(t, "new", "--count", "2"); code != 1 || !strings.Contains(errOut, "--count needs --from") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}
