package cli

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func dashboardAPI(t *testing.T, resp string) (*fakeAPI, func()) {
	f := &fakeAPI{t: t, routes: map[string]string{"POST /v1/auth/links": resp}}
	srv := httptest.NewServer(f)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	return f, srv.Close
}

func TestDashboardOpensASignedInLink(t *testing.T) {
	f, done := dashboardAPI(t, `{"url":"https://api.example/login#link=abc&next=%2Fs%2Fweb","expiresIn":60}`)
	defer done()
	opened := ""
	defer func(old func(string)) { openBrowser = old }(openBrowser)
	openBrowser = func(url string) { opened = url }

	code, out, errOut := run(t, "dashboard", "web", "--json=false")
	if code != 0 || opened != "https://api.example/login#link=abc&next=%2Fs%2Fweb" || !strings.Contains(out, "within 60 seconds") {
		t.Fatalf("code %d, opened %q, stdout %q, stderr %q", code, opened, out, errOut)
	}
	if f.bodies[0] != `{"path":"/s/web"}` {
		t.Fatalf("body %q", f.bodies[0])
	}
}

func TestDashboardJSONDoesntOpen(t *testing.T) {
	f, done := dashboardAPI(t, `{"url":"https://api.example/login#link=abc","expiresIn":60}`)
	defer done()
	opened := ""
	defer func(old func(string)) { openBrowser = old }(openBrowser)
	openBrowser = func(url string) { opened = url }

	code, out, errOut := run(t, "dashboard", "--json")
	if code != 0 || opened != "" || !strings.Contains(out, `"url": "https://api.example/login#link=abc"`) {
		t.Fatalf("code %d, opened %q, stdout %q, stderr %q", code, opened, out, errOut)
	}
	if f.bodies[0] != `{"path":"/"}` {
		t.Fatalf("body %q", f.bodies[0])
	}
}

func TestDashboardOnAnOldDeployment(t *testing.T) {
	_, done := dashboardAPI(t, `!404 {"error":"not found"}`)
	defer done()
	code, _, errOut := run(t, "dashboard", "--json=false", "--no-open")
	if code == 0 || !strings.Contains(errOut, "update it with cloudex deploy") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}
