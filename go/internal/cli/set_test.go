package cli

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// serveFake starts f as the deployment for one test.
func serveFake(t *testing.T, f *fakeAPI) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
}

func TestSetSendsOnlyWhatChanged(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{"PATCH /v1/sandboxes/web": strings.Replace(sandboxJSON, `"idleMinutes":30`, `"idleMinutes":120`, 1)}}
	serveFake(t, f)
	code, out, errOut := run(t, "set", "web", "--idle", "2h")
	if code != 0 || !strings.Contains(out, "after 120 minutes") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.bodies[0] != `{"idleMinutes":120}` {
		t.Fatalf("body %q", f.bodies[0])
	}
	if code, _, _ := run(t, "set", "web", "--name", "site", "--idle", "0"); code != 0 || f.bodies[1] != `{"name":"site","idleMinutes":0}` {
		t.Fatalf("code %d, body %q", code, f.bodies[1])
	}
}

func TestSetNeedsAChange(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{}}
	serveFake(t, f)
	for _, args := range [][]string{{"set", "web"}, {"set", "web", "--idle", "7h"}, {"set", "web", "--idle", "30s"}, {"new", "--idle", "30s"}} {
		if code, _, errOut := run(t, args...); code == 0 || errOut == "" {
			t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("sent %v", f.requests)
	}
}

func TestResumeOnAnotherSize(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{"POST /v1/sandboxes/web/resume": strings.Replace(sandboxJSON, "standard-2", "standard-4", 1)}}
	serveFake(t, f)
	if code, _, errOut := run(t, "resume", "web", "--size", "standard-4"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if f.bodies[0] != `{"size":"standard-4"}` {
		t.Fatalf("body %q", f.bodies[0])
	}
}

func TestResumeSizeOnAnOlderDeploymentFails(t *testing.T) {
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{"POST /v1/sandboxes/web/resume": sandboxJSON}})
	if code, _, errOut := run(t, "resume", "web", "--size", "standard-4"); code == 0 || !strings.Contains(errOut, "cloudex deploy") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestHostRevokeAndList(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{
		"DELETE /v1/sandboxes/web/hosts/3000": `{"revoked":[3000],"all":false}`,
		"DELETE /v1/sandboxes/web/hosts":      `{"revoked":[3000,6080],"all":true}`,
		"GET /v1/sandboxes/web/hosts":         `[{"port":5173,"url":"https://5173-cx-abc123-tok.example.com","createdAt":1}]`,
	}}
	serveFake(t, f)
	if code, out, errOut := run(t, "host", "web", "3000", "--revoke"); code != 0 || !strings.Contains(out, "stopped working") {
		t.Fatalf("--revoke: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := run(t, "host", "web", "--revoke-all"); code != 0 || !strings.Contains(out, "Every link to web") {
		t.Fatalf("--revoke-all: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := run(t, "host", "web", "--list"); code != 0 || !strings.Contains(out, "5173  https://5173-cx-abc123-tok.example.com") {
		t.Fatalf("--list: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := []string{"DELETE /v1/sandboxes/web/hosts/3000", "DELETE /v1/sandboxes/web/hosts", "GET /v1/sandboxes/web/hosts"}
	if strings.Join(f.requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests %v", f.requests)
	}
	for _, args := range [][]string{{"host", "web", "3000", "--list"}, {"host", "--list", "--revoke-all"}, {"host"}} {
		if code, _, _ := run(t, args...); code == 0 {
			t.Fatalf("%v should fail", args)
		}
	}
}
