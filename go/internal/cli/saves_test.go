package cli

import (
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
)

const saveJSON = `{"name":"base","id":"sv-aaaaaaaaaa","sandbox":"cx-abc123","kind":"disk","size":"standard-2",
"idleMinutes":30,"files":1234,"rawBytes":2147483648,"bytes":734003200,"createdAt":1,"ms":20000}`

func TestSaveSavesTheCurrentSandbox(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/cx-abc123":        sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/saves": "!201 " + strings.Replace(saveJSON, `"ms":20000`, `"ms":20000,"replaced":true`, 1),
	})
	cfg, _ := config.Load()
	cfg.Current = "cx-abc123"
	cfg.Save()
	code, out, errOut := run(t, "save", "base")
	if code != 0 || f.bodies[1] != `{"name":"base"}` {
		t.Fatalf("code %d, body %v, stderr %q", code, f.bodies, errOut)
	}
	for _, want := range []string{"Saved web as base: 1234 paths in 2.0 GiB, 700.0 MiB stored.", "replaces the earlier save", "cloudex new --from base"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q lacks %q", out, want)
		}
	}
	if code, _, errOut := run(t, "save", "web", "base", "extra"); code == 0 || errOut == "" {
		t.Fatalf("three arguments: code %d", code)
	}
}

func TestSaveNamesTheSandbox(t *testing.T) {
	f := withFakeAPI(t, map[string]string{"POST /v1/sandboxes/web/saves": "!201 " + saveJSON})
	if code, out, errOut := run(t, "save", "web", "base"); code != 0 || strings.Contains(out, "replaces") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.requests[0] != "POST /v1/sandboxes/web/saves" {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestSavesListsAndDeletes(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"GET /v1/saves":         "[" + saveJSON + "]",
		"DELETE /v1/saves/base": `{"deleted":"base"}`,
	})
	code, out, errOut := run(t, "saves")
	if code != 0 || !strings.Contains(out, "NAME  HOLDS  SIZE") || !strings.Contains(out, "base  disk   2.0 GiB") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut := run(t, "saves", "rm", "base"); code == 0 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("rm without --yes: code %d, stderr %q", code, errOut)
	}
	if code, out, _ := run(t, "saves", "rm", "base", "--yes"); code != 0 || !strings.Contains(out, "Deleted the save base.") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
	if f.requests[len(f.requests)-1] != "DELETE /v1/saves/base" {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestNewFromASave(t *testing.T) {
	started := strings.Replace(forkJSON("cx-fff111", "task-1", "running", ""), `"forkedFrom":"cx-abc123"`,
		`"fromSave":"base","notice":"Restored /home from the save base."`, 1)
	f := withFakeAPI(t, map[string]string{
		"GET /v1/saves/base":            saveJSON,
		"POST /v1/saves/base/sandboxes": "!201 [" + started + "]",
	})
	code, out, errOut := run(t, "new", "--from", "base", "--name", "task-1")
	if code != 0 || !strings.Contains(out, "task-1 is running (standard-2), from the save base.") || !strings.Contains(out, "Restored /home") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if strings.Join(f.requests, ",") != "GET /v1/sandboxes/base,GET /v1/saves/base,POST /v1/saves/base/sandboxes" {
		t.Fatalf("requests %v", f.requests)
	}
	if f.bodies[2] != `{"name":"task-1","count":1}` {
		t.Fatalf("body %q", f.bodies[2])
	}
	if cfg, _ := config.Load(); cfg.Current != "cx-fff111" {
		t.Fatalf("current is %q", cfg.Current)
	}
}

func TestNewFromSavePrefixSkipsTheSandbox(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/base":        sandboxJSON,
		"POST /v1/saves/base/sandboxes": "!201 [" + forkJSON("cx-fff111", "x", "running", "") + "]",
	})
	if code, _, errOut := run(t, "new", "--from", "save:base", "--no-use"); code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if strings.Join(f.requests, ",") != "POST /v1/saves/base/sandboxes" {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestNewFromNothing(t *testing.T) {
	withFakeAPI(t, map[string]string{})
	if code, _, errOut := run(t, "new", "--from", "nope"); code == 0 || !strings.Contains(errOut, "no sandbox or save named nope") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestSavesNamesTheSandboxesTheyCameFrom(t *testing.T) {
	withFakeAPI(t, map[string]string{
		"GET /v1/saves":     "[" + saveJSON + "," + strings.NewReplacer(`"name":"base"`, `"name":"old"`, `"sandbox":"cx-abc123"`, `"sandbox":"cx-gone99"`).Replace(saveJSON) + "]",
		"GET /v1/sandboxes": "[" + sandboxJSON + "]",
	})
	code, out, errOut := run(t, "saves")
	if code != 0 || !strings.Contains(out, "web        cloudflare\n") || !strings.Contains(out, "cx-gone99  cloudflare\n") || strings.Contains(out, "cx-abc123") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}
