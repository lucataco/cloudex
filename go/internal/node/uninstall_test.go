package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerCLI answers the uninstaller's docker commands from files in dir:
// machines, forks (image ids), and a count of fork listings; the fork image
// "late" appears at the third listing, as one whose commit dockerd finished
// after the agent was killed would.
const fakeDockerCLI = `#!/usr/bin/env bash
d=$FAKE_DOCKER_DIR
echo "$*" >>"$d/log"
case "$1 $2" in
"info "*|"version "*) exit 0 ;;
"ps "*) [[ "$*" == *status=paused* ]] && exit 0; cat "$d/machines" ;;
"rm "*) : >"$d/machines" ;;
"network ls"|"volume rm") exit 0 ;;
"image ls")
	if [[ "$*" == *role=fork* ]]; then
		n=$(( $(cat "$d/listings" 2>/dev/null || echo 0) + 1 )); echo $n >"$d/listings"
		[[ $n == 3 ]] && echo late >>"$d/forks"
		cat "$d/forks"
	fi ;;
"rmi "*) grep -vx "$2" "$d/forks" >"$d/forks.new"; mv "$d/forks.new" "$d/forks" ;;
*) exit 0 ;;
esac
`

// Review F-L3: a fork image that appears after the uninstaller removed the
// fork images (a commit dockerd finished with the agent gone) is removed by
// its last check.
func TestUninstallRemovesALateForkImage(t *testing.T) {
	script, err := filepath.Abs("../../../scripts/node-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := os.Stat("/etc/cloudex-node/install.env"); err == nil {
		t.Skip("this host has a node installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDockerCLI), 0o755)
	os.WriteFile(filepath.Join(dir, "machines"), []byte("c1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "forks"), []byte("early\n"), 0o644)
	cmd := exec.Command("bash", script, "--uninstall", "--yes", "--keep-images")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DOCKER_DIR="+dir, "XDG_STATE_HOME="+filepath.Join(dir, "state"), "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil && os.Geteuid() == 0 {
		t.Skipf("as root the uninstaller removes the firewall too: %v", err)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	left, _ := os.ReadFile(filepath.Join(dir, "forks"))
	if strings.TrimSpace(string(left)) != "" {
		t.Fatalf("fork images left: %q\n%s", left, out)
	}
}
