package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Sandboxes on nodes (v0.2.2 M5).

func nodeSandboxJSON(state, nodeState string, online bool, iso string) string {
	return `{"id":"cx-nnn111","name":"n1","size":"standard-2","state":"` + state + `","idleMinutes":0,"createdAt":1,"updatedAt":1,
"startedAt":1,"stoppedAt":null,"savedAt":null,"savedBytes":null,"error":null,
"placement":{"provider":"node","node":{"id":"nd-abcd1234","name":"box","online":` + jsonBool(online) + `,"state":"` + nodeState + `","lastSeenAt":1},"cpus":2,"memoryMb":2048},
"isolation":"` + iso + `"}`
}

func jsonBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestNewOnANodeSendsTheNodeAndLimitsAndWarnsOnPlainDocker(t *testing.T) {
	f := withFakeAPI(t, map[string]string{"POST /v1/sandboxes": "!201 " + nodeSandboxJSON("running", "online", true, "docker")})
	code, out, errOut := run(t, "new", "--on", "box", "--cpus", "2", "--memory", "2G", "--name", "n1")
	if code != 0 || !strings.Contains(out, "n1 is running on node box (2 CPUs, 2.0 GiB)") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "run under plain Docker") || !strings.Contains(errOut, "only for code you trust") {
		t.Fatalf("no plain-Docker warning: %q", errOut)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &body)
	if body["node"] != "box" || body["cpus"] != float64(2) || body["memoryMb"] != float64(2048) || body["name"] != "n1" {
		t.Fatalf("request body %v", body)
	}
}

func TestNewOnASysboxNodeDoesntWarn(t *testing.T) {
	withFakeAPI(t, map[string]string{"POST /v1/sandboxes": "!201 " + nodeSandboxJSON("running", "online", true, "sysbox")})
	code, _, errOut := run(t, "new", "--on", "box")
	if code != 0 || strings.Contains(errOut, "plain Docker") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestNewRefusesNodeFlagsThatDontGoTogether(t *testing.T) {
	f := withFakeAPI(t, map[string]string{})
	for _, args := range [][]string{
		{"new", "--cpus", "2"},
		{"new", "--memory", "4G"},
		{"new", "--on", "box", "--from", "base"},
		{"new", "--on", "box", "--memory", "4"},
	} {
		code, _, errOut := run(t, args...)
		if code == 0 || errOut == "" {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("sent %v", f.requests)
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int{"4G": 4096, "4g": 4096, "4GiB": 4096, "1.5G": 1536, "512M": 512, "512MiB": 512, "2GB": 2048} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"4", "", "G", "-1G", "4T"} {
		if _, err := parseMemory(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestListShowsWhereEachRuns(t *testing.T) {
	removed := strings.Replace(nodeSandboxJSON("error", "revoked", false, "docker"), `"id":"cx-nnn111","name":"n1"`, `"id":"cx-nnn333","name":"n3"`, 1)
	withFakeAPI(t, map[string]string{"GET /v1/sandboxes": "[" + sandboxJSON + "," + nodeSandboxJSON("running", "online", true, "sysbox") + "," +
		strings.Replace(nodeSandboxJSON("running", "offline", false, "sysbox"), `"id":"cx-nnn111","name":"n1"`, `"id":"cx-nnn222","name":"n2"`, 1) + "," + removed + "]"})
	code, out, errOut := run(t, "list")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "WHERE") {
		t.Fatalf("%q", out)
	}
	for i, want := range []string{"cx-abc123 web running cloudflare standard-2", "cx-nnn111 n1 running box -", "cx-nnn222 n2 running box (offline) -", "cx-nnn333 n3 error box (removed) -"} {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); !strings.HasPrefix(got, want) {
			t.Errorf("line %d: %q, want it to start %q", i+1, got, want)
		}
	}
}

func TestInfoShowsTheNodeTheMachineAndTheIsolation(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/sandboxes/n1": nodeSandboxJSON("running", "offline", false, "docker")})
	code, out, _ := run(t, "info", "n1", "--json=false")
	for _, want := range []string{"Where", "node box (offline)", "2 CPUs, 2.0 GiB", "docker (trusted code only)", "Idle stop  off\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %q (code %d)", want, out, code)
		}
	}
	if strings.Contains(out, "6-hour") || strings.Contains(out, "Size") {
		t.Errorf("a node sandbox has no size or 6-hour limit: %q", out)
	}
}

func TestInfoOnCloudflareKeepsItsSize(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/sandboxes/web": strings.Replace(sandboxJSON, `"error":null}`, `"error":null,"placement":{"provider":"cloudflare"},"isolation":"vm"}`, 1)})
	code, out, _ := run(t, "info", "web", "--json=false")
	if code != 0 || !strings.Contains(out, "standard-2") || !strings.Contains(out, "vm (Cloudflare)") || !strings.Contains(out, "Where      cloudflare") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestDeleteForcePassesForce(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/n1":
			w.Write([]byte(nodeSandboxJSON("running", "offline", false, "sysbox")))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/cx-nnn111":
			queries = append(queries, r.URL.RawQuery)
			if r.URL.RawQuery == "" {
				w.WriteHeader(503)
				w.Write([]byte(`{"error":"node box is offline (last seen 2026-11-11 14:02 UTC), so n1's machine can't be removed now. Delete it anyway with --force (force=1); node box removes the machine when it's back"}`))
				return
			}
			w.Write([]byte(`{"deleted":"cx-nnn111"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "k")
	code, _, errOut := run(t, "delete", "n1", "--yes")
	if code != 1 || !strings.Contains(errOut, "node box is offline") || !strings.Contains(errOut, "--force") {
		t.Fatalf("without --force: %d %q", code, errOut)
	}
	code, out, errOut := run(t, "delete", "n1", "--yes", "--force")
	if code != 0 || !strings.Contains(out, "Deleted n1") || queries[len(queries)-1] != "force=1" {
		t.Fatalf("--force: %d %q %q, queries %v", code, out, errOut, queries)
	}
}

func TestNodeInfoShowsOrphansPendingDeletesAndASkewedClock(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/nodes/box": strings.Replace(node(time.Now().UnixMilli()), `"revokedAt":null`,
		`"revokedAt":null,"orphans":["cx-ooo111"],"pendingDeletes":["cx-ddd111"],"clockSkewSec":-42`, 1)})
	code, out, _ := run(t, "node", "info", "box")
	for _, want := range []string{"cx-ooo111", "left alone", "cx-ddd111", "removes their machines when it's back", "42 s behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %q (code %d)", want, out, code)
		}
	}
}

func TestDoctorWarnsAboutNodesWithoutFailing(t *testing.T) {
	offline := strings.Replace(node(1), `"state":"online"`, `"state":"offline"`, 1)
	skewed := strings.NewReplacer(`"id":"nd-abcd1234","name":"box"`, `"id":"nd-skew0001","name":"far"`, `"version":"v0.2.2"`, `"version":"v0.2.1"`,
		`"firewall":"unchecked","unisolated":true`, `"firewall":"fail","unisolated":false`, `"diskFreeGb":null`, `"diskFreeGb":9`).Replace(node(time.Now().UnixMilli()))
	withFakeAPI(t, map[string]string{
		"GET /v1/health":            `{"ok":true,"version":"v0.2.2","previewZone":null}`,
		"GET /v1/sandboxes":         `[]`,
		"GET /v1/nodes":             "[" + offline + "," + skewed + "]",
		"GET /v1/nodes/nd-skew0001": strings.Replace(skewed, `"revokedAt":null`, `"revokedAt":null,"clockSkewSec":45`, 1),
	})
	code, out, _ := runVersion(t, "v0.2.2", "--json=false", "doctor")
	for _, want := range []string{"node box is offline", "node far runs agent v0.2.1", "node far fails its isolation self-test", "9 GB of Docker disk free", "45 s ahead"} {
		if !strings.Contains(out, "warn  "+want) && !strings.Contains(out, want) {
			t.Errorf("no %q in %q", want, out)
		}
	}
	if strings.Contains(out, "fix  ") {
		t.Errorf("a node problem is a warning, not a fix: %q (code %d)", out, code)
	}
}
