package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
)

func newKeyedAPI(t *testing.T, version string, routes map[string]string) *httptest.Server {
	t.Helper()
	routes["GET /v1/health"] = `{"ok":true,"version":"` + version + `","previewZone":null,"backups":true}`
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			r.Header.Set("Authorization", "Bearer test-key")
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	return srv
}

func TestStatusExitsNonZeroWhenTheKeyIsRejected(t *testing.T) {
	srv := newKeyedAPI(t, "v0.1.17", map[string]string{"GET /v1/sandboxes": `[]`})
	t.Setenv("CLOUDEX_API_KEY", "stale")
	code, out, errOut := run(t, "status")
	if code != 1 || out != "" || !strings.Contains(errOut, "rejected the key") ||
		!strings.Contains(errOut, "cloudex login --url "+srv.URL) || !strings.Contains(errOut, "cloudex status --show-key") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestStatusNamesTheCurrentSandbox(t *testing.T) {
	newKeyedAPI(t, "v0.1.17", map[string]string{"GET /v1/sandboxes": "[" + sandboxJSON + "]"})
	if err := (&config.Config{Current: "cx-abc123"}).Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "status")
	if code != 0 || !strings.Contains(out, "Current  web (cx-abc123)\n") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestStatusAndDoctorSayWhenTheCLIIsNewer(t *testing.T) {
	newKeyedAPI(t, "v0.1.15", map[string]string{"GET /v1/sandboxes": `[]`})
	want := "older than this CLI (v0.1.17)"
	if code, out, errOut := runVersion(t, "v0.1.17", "--json=false", "status"); code != 0 || !strings.Contains(out, want) || !strings.Contains(out, "cloudex deploy") {
		t.Fatalf("status: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runVersion(t, "v0.1.17", "--json=false", "doctor"); code != 1 || !strings.Contains(out, "fix   the deployment runs v0.1.15, "+want) {
		t.Fatalf("doctor: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if _, out, _ := runVersion(t, "v0.1.15", "--json=false", "status"); strings.Contains(out, "older") {
		t.Fatalf("a matching CLI got a warning: %q", out)
	}
	if _, out, _ := runVersion(t, "dev", "--json=false", "doctor"); strings.Contains(out, "older") {
		t.Fatalf("a source build got a warning: %q", out)
	}
}

func TestDoctorSendsARejectedKeyToLogin(t *testing.T) {
	srv := newKeyedAPI(t, "v0.1.17", map[string]string{"GET /v1/sandboxes": `[]`})
	t.Setenv("CLOUDEX_API_KEY", "stale")
	code, out, _ := run(t, "doctor")
	if code != 1 || !strings.Contains(out, "fix   the API rejected the key. Run cloudex login --url "+srv.URL+" with the key from cloudex status --show-key") ||
		strings.Contains(out, "run cloudex deploy again") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestDoctorNamesACorruptConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLOUDEX_CONFIG_DIR", dir)
	t.Setenv("CLOUDEX_API_URL", "")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "doctor")
	if code != 1 || !strings.Contains(out, "fix   "+filepath.Join(dir, "config.json")+" can't be read") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}
