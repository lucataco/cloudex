package update_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/update"
	"github.com/lucataco/cloudex/go/internal/update/updatetest"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.3", "v0.1.2", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.1.10", "v0.1.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.2", "v0.1.2", false},
		{"v0.1.2", "v0.1.3", false},
		{"v0.1.2", "v0.1.2-dev", true},
		{"v0.1.2-dev", "v0.1.2", false},
		{"v0.1.2", "dev", true},
	} {
		if got := update.Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestLatestFollowsTheRedirect(t *testing.T) {
	updatetest.Server(t, "v9.9.9", []byte("new"), "")
	tag, err := update.Latest(context.Background())
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
}

func TestDownloadAndReplace(t *testing.T) {
	updatetest.Server(t, "v9.9.9", []byte("new build"), "")
	exe, err := update.Download(context.Background(), "v9.9.9")
	if err != nil || string(exe) != "new build" {
		t.Fatalf("Download = %q, %v", exe, err)
	}
	target := filepath.Join(t.TempDir(), "cloudex")
	os.WriteFile(target, []byte("old build"), 0o755)
	if err := update.Replace(target, exe); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(got) != "new build" || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("after Replace: %q, mode %v", got, info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".cloudex-update-*")); len(left) != 0 {
		t.Fatalf("left temporary files: %v", left)
	}
}

func TestDownloadRefusesABadChecksum(t *testing.T) {
	updatetest.Server(t, "v9.9.9", []byte("tampered"), strings.Repeat("ab", 32))
	if _, err := update.Download(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
}
