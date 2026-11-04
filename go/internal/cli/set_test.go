package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
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
	f := &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/web":   sandboxJSON,
		"PATCH /v1/sandboxes/web": strings.Replace(sandboxJSON, `"idleMinutes":30`, `"idleMinutes":120`, 1),
	}}
	serveFake(t, f)
	code, out, errOut := run(t, "set", "web", "--idle", "2h")
	if code != 0 || !strings.Contains(out, "after 120 minutes") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.bodies[0] != `{"idleMinutes":120}` {
		t.Fatalf("body %q", f.bodies[0])
	}
	if code, _, _ := run(t, "set", "web", "--name", "site", "--idle", "0"); code != 0 || f.bodies[len(f.bodies)-1] != `{"name":"site","idleMinutes":0}` {
		t.Fatalf("code %d, bodies %q", code, f.bodies)
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

func TestStopSaysWhatBecameOfTheSandbox(t *testing.T) {
	stopped := strings.Replace(sandboxJSON, `"state":"running"`, `"state":"stopped","notice":"Restored /home from the backup of 2026-10-29."`, 1)
	failed := strings.Replace(sandboxJSON, `"state":"running"`, `"state":"error"`, 1)
	failed = strings.Replace(failed, `"error":null`, `"error":"cx-abc123 didn't start: no capacity"`, 1)
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/cx-abc123":         sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/stop":   stopped,
		"POST /v1/sandboxes/cx-err111/stop":   failed,
		"POST /v1/sandboxes/cx-abc123/resume": strings.Replace(sandboxJSON, `"error":null`, `"notice":"Restored /home from the backup.","error":null`, 1),
	}})
	if err := (&config.Config{Current: "cx-abc123"}).Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "stop")
	if code != 0 || out != "web is stopped; its disk is saved.\n" || !strings.HasPrefix(errOut, "Saving and stopping web...") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, _ = run(t, "stop", "cx-err111")
	if code != 0 || strings.Contains(out, "disk is saved") || !strings.Contains(out, "web is in an error state.") || !strings.Contains(out, "no capacity") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	if code, out, _ = run(t, "resume"); code != 0 || !strings.Contains(out, "web is running.") || !strings.Contains(out, "Restored /home") {
		t.Fatalf("resume: code %d, stdout %q", code, out)
	}
}

func TestStopGivesUpWaitingWithoutGivingUpOnTheStop(t *testing.T) {
	defer func(d time.Duration) { lifecycleTimeout = d }(lifecycleTimeout)
	lifecycleTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	code, _, errOut := run(t, "stop", "web")
	if code != 1 || !strings.Contains(errOut, "the work on web goes on") || !strings.Contains(errOut, "cloudex info web") || strings.Contains(errOut, "can't reach") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestInfoLabelsAnErrorAsAnError(t *testing.T) {
	failed := strings.Replace(sandboxJSON, `"error":null`, `"notice":"Restored.","error":"didn't start: no capacity"`, 1)
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{"GET /v1/sandboxes/web": failed}})
	code, out, _ := run(t, "info", "web")
	if code != 0 || !strings.Contains(out, "Note       Restored.") || !strings.Contains(out, "Error      didn't start: no capacity") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestUsePrintsJSONInJSONMode(t *testing.T) {
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{"GET /v1/sandboxes/web": sandboxJSON}})
	var out, errOut bytes.Buffer
	if code := Run("test", []string{"--json", "use", "web"}, &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	var s api.Sandbox
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || s.ID != "cx-abc123" {
		t.Fatalf("%v: %q", err, out.String())
	}
	if cfg, _ := config.Load(); cfg.Current != "cx-abc123" {
		t.Fatalf("current is %q", cfg.Current)
	}
}

func TestADurationWithoutAUnitSaysToAddOne(t *testing.T) {
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{}})
	for _, args := range [][]string{{"new", "--idle", "30"}, {"set", "web", "--idle", "30"}, {"fork", "web", "--idle", "30"}, {"exec", "--timeout", "30", "--", "true"}} {
		code, _, errOut := run(t, args...)
		if code != 1 || !strings.Contains(errOut, "add a unit, such as 30m or 30h") {
			t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	if code, _, errOut := run(t, "new", "--idle", "soon"); code != 1 || !strings.Contains(errOut, `invalid duration "soon"`) {
		t.Fatalf("a word: code %d, stderr %q", code, errOut)
	}
}

func TestSSHAndDesktopSayWhenASandboxIsStillChangingState(t *testing.T) {
	for state, want := range map[string]string{
		"starting": "web is starting; try again in a moment",
		"stopping": "web is stopping; try again in a moment",
		"stopped":  "web is stopped; start it with: cloudex resume web",
	} {
		serveFake(t, &fakeAPI{t: t, routes: map[string]string{
			"GET /v1/sandboxes/web": strings.Replace(sandboxJSON, `"state":"running"`, `"state":"`+state+`"`, 1),
		}})
		for _, cmd := range []string{"ssh", "desktop", "attach"} {
			if code, _, errOut := run(t, cmd, "web"); code != 1 || !strings.Contains(errOut, want) {
				t.Fatalf("%s on a %s sandbox: code %d, stderr %q", cmd, state, code, errOut)
			}
		}
	}
}

func TestForkNamesTheCurrentSandboxByItsName(t *testing.T) {
	serveFake(t, &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/cx-abc123":       sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/fork": "!201 [" + forkJSON("cx-fff111", "try-2", "running", "") + "]",
	}})
	if err := (&config.Config{Current: "cx-abc123"}).Save(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "fork", "--name", "try-2")
	if code != 0 || !strings.Contains(errOut, "Making a fork of web.") || !strings.Contains(out, "a fork of web.") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestExecHelpSaysStandardInputIsNotSent(t *testing.T) {
	_, out, _ := run(t, "exec", "--help")
	for _, want := range []string{"Standard input isn't sent", "cloudex cp <sandbox>:file -", "binary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("exec --help lacks %q:\n%s", want, out)
		}
	}
}

func TestDesktopSendsASizeOnlyWhenGivenOne(t *testing.T) {
	f := &fakeAPI{t: t, routes: map[string]string{
		"GET /v1/sandboxes/web":                sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/desktop": desktopJSON,
		"POST /v1/sandboxes/cx-abc123/hosts":   `{"url":"https://6080-cx-abc123-tok.example.com"}`,
	}}
	serveFake(t, f)
	defer func(old func(string)) { openBrowser = old }(openBrowser)
	openBrowser = func(string) {}
	if code, _, errOut := run(t, "desktop", "web", "--link"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if f.bodies[1] != `{}` {
		t.Fatalf("a bare desktop sent %s", f.bodies[1])
	}
	if code, _, errOut := run(t, "desktop", "web", "--link", "--size", "1920x1080"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if f.bodies[4] != `{"size":"1920x1080"}` {
		t.Fatalf("--size sent %s", f.bodies[4])
	}
}
