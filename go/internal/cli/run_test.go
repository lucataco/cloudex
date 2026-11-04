package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
)

func TestCommandLineQuotesSeveralArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"cd app && make"}, "cd app && make"},
		{[]string{"echo", "a  b", "it's"}, `echo 'a  b' 'it'\''s'`},
		{[]string{"python3", "-c", "print(1)"}, "python3 -c 'print(1)'"},
		{[]string{"ls", "-la", "/tmp/x.y"}, "ls -la /tmp/x.y"},
	} {
		if got := commandLine(c.args); got != c.want {
			t.Errorf("commandLine(%q) = %s, want %s", c.args, got, c.want)
		}
	}
}

func TestRemotePath(t *testing.T) {
	for _, c := range []struct {
		in, box, path string
		ok            bool
	}{
		{"web:app/dist", "web", "app/dist", true},
		{":notes.txt", "", "notes.txt", true},
		{"cx-abc123:", "cx-abc123", ".", true},
		{"./a:b", "", "", false},
		{"C:\\x", "", "", false},
		{"report.csv", "", "", false},
		{"Web:x", "", "", false},
	} {
		box, p, ok := remotePath(c.in)
		if box != c.box || p != c.path || ok != c.ok {
			t.Errorf("remotePath(%q) = %q, %q, %v", c.in, box, p, ok)
		}
	}
}

func TestPackAndUnpackKeepFilesModesAndLinks(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("a.txt", filepath.Join(src, "link"))
	var buf bytes.Buffer
	n, size, err := packDir(&buf, src, nil)
	if err != nil || n != 4 || size != 15 {
		t.Fatalf("packed %d entries, %d bytes, %v", n, size, err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if n, _, err := unpack(&buf, dst); err != nil || n != 4 {
		t.Fatalf("unpacked %d, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "hello" {
		t.Fatalf("a.txt is %q", b)
	}
	if info, _ := os.Stat(filepath.Join(dst, "sub", "run.sh")); info.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode %v", info.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "a.txt" {
		t.Fatalf("link points at %q", target)
	}
}

func tarGz(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h.Mode = 0o644
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = "x"
			h.Size = 1
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestUnpackStaysInItsDirectory(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if _, _, err := unpack(tarGz(t, tar.Header{Name: "../evil", Typeflag: tar.TypeReg}), out); err == nil {
		t.Fatal("unpacked ../evil")
	}
	if _, _, err := unpack(tarGz(t, tar.Header{Name: "/etc/evil", Typeflag: tar.TypeReg}), out); err == nil {
		t.Fatal("unpacked /etc/evil")
	}
	// A symlink out of the directory is left out, so the file after it lands inside.
	_, skipped, err := unpack(tarGz(t,
		tar.Header{Name: "up", Typeflag: tar.TypeSymlink, Linkname: dir},
		tar.Header{Name: "up/evil", Typeflag: tar.TypeReg},
		tar.Header{Name: "rel", Typeflag: tar.TypeSymlink, Linkname: "../../x"},
		tar.Header{Name: "ok", Typeflag: tar.TypeSymlink, Linkname: "up/evil"},
	), out)
	if err != nil || skipped != 2 {
		t.Fatalf("skipped %d, %v", skipped, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("evil exists outside the directory")
	}
	if target, _ := os.Readlink(filepath.Join(out, "ok")); target != "up/evil" {
		t.Fatalf("ok points at %q", target)
	}
	// A symlink already there isn't written or chmodded through.
	secret := filepath.Join(dir, "secret")
	os.MkdirAll(secret, 0o700)
	os.Symlink(secret, filepath.Join(out, "link"))
	if _, _, err := unpack(tarGz(t, tar.Header{Name: "link/", Typeflag: tar.TypeDir}), out); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(secret); info.Mode().Perm() != 0o700 {
		t.Fatalf("chmodded through a symlink: %v", info.Mode())
	}
	// A file never replaces a directory, and .git stays behind.
	os.MkdirAll(filepath.Join(out, "keep", "inside"), 0o755)
	if _, _, err := unpack(tarGz(t, tar.Header{Name: "keep", Typeflag: tar.TypeReg}), out); err == nil {
		t.Fatal("replaced a directory with a file")
	}
	if _, err := os.Stat(filepath.Join(out, "keep", "inside")); err != nil {
		t.Fatal("the directory is gone")
	}
	_, skipped, err = unpack(tarGz(t, tar.Header{Name: ".git/hooks/pre-commit", Typeflag: tar.TypeReg}), out)
	if err != nil || skipped != 1 {
		t.Fatalf("skipped %d, %v", skipped, err)
	}
	if _, err := os.Stat(filepath.Join(out, ".git")); err == nil {
		t.Fatal("unpacked .git")
	}
}

func TestGitFilesListsWhatGitWouldCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "x"), []byte("x"), 0o644)
	git("add", ".")
	git("commit", "-qm", "init")
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644)
	files, ok, err := gitFiles(dir)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if want := []string{".gitignore", "main.go", "new.txt"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files %q, want %q", files, want)
	}
	if _, ok, _ := gitFiles(t.TempDir()); ok {
		t.Fatal("an empty directory counted as a git work tree")
	}
}

func TestExecStreamsOutputAndEndsWithTheExitCode(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/web/exec" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"type":"stdout","data":"one\n"}`+"\n"+`{"type":"stderr","data":"oops\n"}`+"\n"+
			`{"type":"stdout","data":"two\n"}`+"\n"+`{"type":"exit","exitCode":7,"timedOut":false,"ms":12}`+"\n")
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	code, out, errOut := run(t, "exec", "web", "--", "echo", "a b")
	if code != 7 || out != "one\ntwo\n" || errOut != "oops\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if body["stream"] != true || body["cmd"] != "echo 'a b'" {
		t.Fatalf("body %v", body)
	}
}

const commandJSON = `{"id":"cm-1a2b3c4d","cmd":"make","root":false,"cwd":"/home/cloudex/proj","state":"%s","exitCode":%s,
"signal":null,"timedOut":false,"timeoutSec":0,"startedAt":1,"endedAt":%s,"outputBytes":6}`

func command(state, code, ended string) string {
	return strings.ReplaceAll(fmt.Sprintf(commandJSON, state, code, ended), "\n", "")
}

func TestCommandsLogsFollowEndsWithTheExitCode(t *testing.T) {
	newFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web/commands/cm-1a2b3c4d/output": `{"data":"built\n","offset":0,"next":6,"command":` + command("exited", "2", "5") + `}`,
	})
	code, out, _ := run(t, "commands", "logs", "-f", "web", "cm-1a2b3c4d")
	if code != 2 || out != "built\n" {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestExecDetachStartsABackgroundCommand(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes/cx-abc123/commands": command("running", "null", "null"),
	})
	(&config.Config{Current: "cx-abc123"}).Save()
	code, out, errOut := run(t, "exec", "-d", "--", "npm", "run", "dev")
	if code != 0 || !strings.Contains(out, "cloudex commands logs -f cm-1a2b3c4d") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if f.bodies[0] != `{"cmd":"npm run dev"}` {
		t.Fatalf("body %s", f.bodies[0])
	}
}

func TestRunUploadsRunsCopiesBackAndDeletes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644)
	t.Chdir(dir)
	f := withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes":                                      sandboxJSON,
		"PUT /v1/sandboxes/cx-abc123/files/archive":               `{"path":"/home/cloudex/proj","entries":1}`,
		"POST /v1/sandboxes/cx-abc123/commands":                   command("running", "null", "null"),
		"GET /v1/sandboxes/cx-abc123/commands/cm-1a2b3c4d/output": `{"data":"built\n","offset":0,"next":6,"command":` + command("exited", "0", "5") + `}`,
		"GET /v1/sandboxes/cx-abc123/files":                       `{"path":"/home/cloudex/proj/out.txt","type":"file","size":3,"mode":"600","mtime":1}`,
		"GET /v1/sandboxes/cx-abc123/files/content":               "abc",
		"DELETE /v1/sandboxes/cx-abc123":                          `{"deleted":"cx-abc123"}`,
	})
	code, out, errOut := run(t, "run", "--out", "out.txt", "--", "make")
	if code != 0 || out != "built\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	want := []string{
		"POST /v1/sandboxes",
		"PUT /v1/sandboxes/cx-abc123/files/archive",
		"POST /v1/sandboxes/cx-abc123/commands",
		"GET /v1/sandboxes/cx-abc123/commands/cm-1a2b3c4d/output",
		"GET /v1/sandboxes/cx-abc123/files",
		"GET /v1/sandboxes/cx-abc123/files",
		"GET /v1/sandboxes/cx-abc123/files/content",
		"DELETE /v1/sandboxes/cx-abc123",
	}
	if !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %q", f.requests)
	}
	var start map[string]any
	json.Unmarshal([]byte(f.bodies[2]), &start)
	if start["cmd"] != "make" || start["cwd"] != "/home/cloudex/proj" {
		t.Fatalf("start %v", start)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(b) != "abc" {
		t.Fatalf("out.txt is %q", b)
	}
}

func TestRunDeletesTheSandboxWhenTheCommandFails(t *testing.T) {
	t.Chdir(t.TempDir())
	f := withFakeAPI(t, map[string]string{
		"POST /v1/sandboxes":                                      sandboxJSON,
		"POST /v1/sandboxes/cx-abc123/commands":                   command("running", "null", "null"),
		"GET /v1/sandboxes/cx-abc123/commands/cm-1a2b3c4d/output": `{"data":"","offset":0,"next":6,"command":` + command("exited", "3", "5") + `}`,
		"DELETE /v1/sandboxes/cx-abc123":                          `{"deleted":"cx-abc123"}`,
	})
	code, _, _ := run(t, "run", "--no-upload", "--", "false")
	if code != 3 || f.requests[len(f.requests)-1] != "DELETE /v1/sandboxes/cx-abc123" {
		t.Fatalf("code %d, requests %q", code, f.requests)
	}
	f.requests = nil
	code, _, errOut := run(t, "run", "--no-upload", "--keep-on-failure", "--", "false")
	if code != 3 || strings.Contains(strings.Join(f.requests, " "), "DELETE") || !strings.Contains(errOut, "kept web") {
		t.Fatalf("code %d, requests %q, stderr %q", code, f.requests, errOut)
	}
}

func TestCpNamesADownloadAfterWhatWasAskedFor(t *testing.T) {
	withFakeAPI(t, map[string]string{
		// The sandbox answers with another file's path, as a symlink would.
		"GET /v1/sandboxes/web/files":         `{"path":"/tmp/x/.zshrc","type":"file","size":3,"mode":"644","mtime":1}`,
		"GET /v1/sandboxes/web/files/content": "abc",
	})
	dir := t.TempDir()
	if code, _, errOut := run(t, "cp", "web:report.txt", dir+"/"); code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "report.txt")); err != nil || string(b) != "abc" {
		t.Fatalf("report.txt: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".zshrc")); err == nil {
		t.Fatal("wrote .zshrc")
	}
}
