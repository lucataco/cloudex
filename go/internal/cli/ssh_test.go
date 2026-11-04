package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
)

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SzG/cT35aaaaaaaaaaaaaaaaaaaaaaa1"

func knownHosts(t *testing.T) string {
	t.Helper()
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "known_hosts")
}

func writeKnownHosts(t *testing.T, hosts ...string) {
	t.Helper()
	var b strings.Builder
	for _, h := range hosts {
		b.WriteString(h + " " + hostKey + "\n")
	}
	if err := os.WriteFile(knownHosts(t), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func knownHostNames(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(knownHosts(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			names = append(names, strings.Fields(line)[0])
		}
	}
	return names
}

func TestRemoveKnownHostForgetsEveryFormOfTheName(t *testing.T) {
	// The hashed entries are what ssh-keygen -H wrote for web.cloudex and other.cloudex.
	const hashedWeb = "|1|6HaDvgN7Fs5UqKXPKQVBO6iRAFc=|s7yHVnwbq2TppdhhPjRFXVtZ2Vk="
	const hashedOther = "|1|NvGkAkhw3uRhQeF/8fsFjkNmcLs=|wbrEpeoyH0fkk0k4pC4isrB9E4A="
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	writeKnownHosts(t, "web.cloudex", "[web.cloudex]:2222", "web.cloudex,10.0.0.1", hashedWeb, hashedOther,
		"webby.cloudex", "xweb.cloudex", "cx-abc123.cloudex", "github.com")
	if err := removeKnownHost(knownHosts(t), "web.cloudex"); err != nil {
		t.Fatal(err)
	}
	want := []string{hashedOther, "webby.cloudex", "xweb.cloudex", "cx-abc123.cloudex", "github.com"}
	if got := knownHostNames(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	if err := removeKnownHost(filepath.Join(t.TempDir(), "missing"), "web.cloudex"); err != nil {
		t.Fatalf("a missing file: %v", err)
	}
}

func proxyAPI(t *testing.T, id string) *fakeAPI {
	return withFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web": strings.Replace(sandboxJSON, "cx-abc123", id, 1),
	})
}

func TestProxyForgetsANameThatMovedBeforeDialing(t *testing.T) {
	f := proxyAPI(t, "cx-abc123")
	writeKnownHosts(t, "web.cloudex", "[web.cloudex]:2222", "cx-abc123.cloudex", "cx-old111.cloudex")

	if code, _, _ := run(t, "proxy", "web.cloudex", "22"); code == 0 {
		t.Fatal("the dial should have failed")
	}
	if got, want := knownHostNames(t), []string{"cx-abc123.cloudex", "cx-old111.cloudex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("known_hosts has %v, want %v", got, want)
	}
	if want := []string{"GET /v1/sandboxes/web", "GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v, want %v: the tunnel is dialed by id", f.requests, want)
	}
	dir, _ := config.Dir()
	if info, err := os.Stat(sshNamesPath(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ssh-names.json: %v %v", info, err)
	}
	if got := readSSHNames(dir); !reflect.DeepEqual(got, map[string]string{"web": "cx-abc123"}) {
		t.Fatalf("names %v", got)
	}

	writeKnownHosts(t, "web.cloudex")
	run(t, "proxy", "web.cloudex", "22")
	if got := knownHostNames(t); !reflect.DeepEqual(got, []string{"web.cloudex"}) {
		t.Fatalf("a name that didn't move lost its key: %v", got)
	}

	f.routes["GET /v1/sandboxes/web"] = strings.Replace(sandboxJSON, "cx-abc123", "cx-new222", 1)
	run(t, "proxy", "web.cloudex", "22")
	if got := knownHostNames(t); len(got) != 0 {
		t.Fatalf("a name that moved kept its key: %v", got)
	}
	if got := readSSHNames(dir)["web"]; got != "cx-new222" {
		t.Fatalf("recorded %q", got)
	}
}

func TestProxyByIDKeepsNoRecord(t *testing.T) {
	f := proxyAPI(t, "cx-abc123")
	writeKnownHosts(t, "cx-abc123.cloudex")
	run(t, "proxy", "cx-abc123.cloudex", "22")
	if want := []string{"GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v", f.requests)
	}
	dir, _ := config.Dir()
	if _, err := os.Stat(sshNamesPath(dir)); err == nil {
		t.Fatal("an id was recorded as a name")
	}
	if got := knownHostNames(t); !reflect.DeepEqual(got, []string{"cx-abc123.cloudex"}) {
		t.Fatalf("known_hosts %v", got)
	}
}

func TestDeleteAndRenameForgetTheOldName(t *testing.T) {
	withFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web":          sandboxJSON,
		"PATCH /v1/sandboxes/web":        strings.Replace(sandboxJSON, `"name":"web"`, `"name":"site"`, 1),
		"DELETE /v1/sandboxes/cx-abc123": `{"deleted":"cx-abc123"}`,
	})
	dir, _ := config.Dir()
	writeKnownHosts(t, "web.cloudex", "cx-abc123.cloudex")
	writeSSHNames(dir, map[string]string{"web": "cx-abc123", "other": "cx-zzz999"})

	if code, _, errOut := run(t, "set", "web", "--name", "site"); code != 0 {
		t.Fatalf("set: %d %s", code, errOut)
	}
	if got := knownHostNames(t); !reflect.DeepEqual(got, []string{"cx-abc123.cloudex"}) {
		t.Fatalf("after set --name, known_hosts has %v", got)
	}
	if got := readSSHNames(dir); !reflect.DeepEqual(got, map[string]string{"other": "cx-zzz999"}) {
		t.Fatalf("after set --name, names %v", got)
	}

	writeKnownHosts(t, "web.cloudex", "cx-abc123.cloudex")
	writeSSHNames(dir, map[string]string{"web": "cx-abc123"})
	if code, _, errOut := run(t, "delete", "web", "--yes"); code != 0 {
		t.Fatalf("delete: %d %s", code, errOut)
	}
	if got := knownHostNames(t); !reflect.DeepEqual(got, []string{"cx-abc123.cloudex"}) {
		t.Fatalf("after delete, known_hosts has %v", got)
	}
	if got := readSSHNames(dir); len(got) != 0 {
		t.Fatalf("after delete, names %v", got)
	}
}
