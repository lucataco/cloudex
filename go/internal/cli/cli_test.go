package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
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
