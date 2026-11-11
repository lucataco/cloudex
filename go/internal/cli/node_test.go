package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const nodeJSON = `{"id":"nd-abcd1234","name":"box","state":"online","createdAt":1,"lastSeenAt":%d,"version":"v0.2.2","protocol":1,
"arch":"amd64","runtime":"docker","isolation":{"firewall":"unchecked","unisolated":true},"cpus":4,"memoryMb":16384,"diskFreeGb":null,
"colo":"EWR","continent":"NA","locationHint":"enam","machines":2,"revokedAt":null}`

func node(lastSeen int64) string {
	return strings.Replace(nodeJSON, "%d", jsonInt(lastSeen), 1)
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestNodeAddPrintsTheInstallCommandAndTheToken(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"POST /v1/nodes": `!201 {"node":` + strings.Replace(node(0), `"state":"online"`, `"state":"pending"`, 1) +
			`,"joinToken":"cxj_tok","expiresAt":1762819200000,"install":"curl -fsSL https://cloudex.build/node | sudo bash -s -- --api https://x --token cxj_tok --version v0.2.2"}`,
	})
	code, out, errOut := run(t, "node", "add", "box")
	if code != 0 || !strings.Contains(out, "curl -fsSL https://cloudex.build/node | sudo bash -s -- --api https://x --token cxj_tok") ||
		!strings.Contains(out, "works once") || !strings.Contains(out, "Root in a machine\nis root on box") || !strings.Contains(out, "only for code you trust") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.bodies[0] != `{"name":"box"}` {
		t.Fatalf("body %s", f.bodies[0])
	}
	code, out, _ = run(t, "node", "add", "box", "--json")
	var n map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &n) != nil || n["joinToken"] != "cxj_tok" || n["install"] == nil {
		t.Fatalf("--json: %d %q", code, out)
	}
}

func TestNodeListShowsEachNodesColumns(t *testing.T) {
	revoked := `{"id":"nd-zzzz9999","name":"old\u001b[31m","state":"revoked","createdAt":1,"lastSeenAt":null,"version":null,"protocol":null,
"arch":null,"runtime":null,"isolation":null,"cpus":null,"memoryMb":null,"diskFreeGb":null,"colo":null,"continent":null,"locationHint":null,"machines":0,"revokedAt":5}`
	withFakeAPI(t, map[string]string{"GET /v1/nodes": "[" + node(time.Now().UnixMilli()) + "," + revoked + "]"})
	code, out, errOut := run(t, "node", "list")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("%q", out)
	}
	for _, col := range []string{"NAME", "STATE", "RUNTIME", "ISOLATION", "CPUS", "MEMORY", "DISK FREE", "MACHINES", "SEEN"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("no %s column: %q", col, lines[0])
		}
	}
	box := strings.Join(strings.Fields(lines[1]), " ")
	if box != "box online docker none (testing) 4 16.0 GiB - 2 now" {
		t.Errorf("box: %q", box)
	}
	if strings.Contains(out, "\x1b") || !strings.Contains(lines[2], "old?[31m") || !strings.Contains(lines[2], "revoked") {
		t.Errorf("a name with an escape in it: %q", lines[2])
	}
	// With no subcommand, the same list.
	if code, out2, _ := run(t, "node"); code != 0 || out2 != out {
		t.Errorf("cloudex node: %d %q", code, out2)
	}
}

func TestNodeListWithNoNodesSaysHowToAddOne(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/nodes": `[]`})
	code, out, _ := run(t, "node", "list")
	if code != 0 || !strings.Contains(out, "cloudex node add <name>") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "node", "list", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("--json: %d %q", code, out)
	}
}

func TestNodeInfoShowsWhereItConnectsAndTheRoundTrip(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/nodes/box": strings.Replace(node(time.Now().UnixMilli()), `"revokedAt":null`, `"revokedAt":null,"rttMs":38`, 1)})
	code, out, errOut := run(t, "node", "info", "box")
	if code != 0 || !strings.Contains(out, "EWR, sandboxes in enam") || !strings.Contains(out, "38 ms") || !strings.Contains(out, "v0.2.2") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestNodeRmAsksFirstAndPassesForce(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/nodes/box" {
			w.WriteHeader(404)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.RawQuery == "" {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"node box still has 2 sandboxes: delete them first, or remove the node anyway with --force"}`))
			return
		}
		w.Write([]byte(`{"revoked":"nd-abcd1234"}`))
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "k")
	code, _, errOut := run(t, "node", "rm", "box")
	if code != 1 || !strings.Contains(errOut, "--yes") || len(queries) != 0 {
		t.Fatalf("without --yes: %d %q, sent %v", code, errOut, queries)
	}
	code, _, errOut = run(t, "node", "rm", "box", "--yes")
	if code != 1 || !strings.Contains(errOut, "still has 2 sandboxes") {
		t.Fatalf("with sandboxes: %d %q", code, errOut)
	}
	code, out, errOut := run(t, "node", "rm", "box", "--yes", "--force")
	if code != 0 || !strings.Contains(out, "can't connect again") || queries[len(queries)-1] != "force=1" {
		t.Fatalf("--force: %d %q %q, queries %v", code, out, errOut, queries)
	}
}
