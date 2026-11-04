package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// releasesFailing serves a GitHub that errors, and counts what it's asked.
func releasesFailing(t *testing.T) *atomic.Int32 {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_RELEASES_URL", srv.URL+"/releases")
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	return &asked
}

func TestAFailedUpdateCheckIsNotRetriedForADay(t *testing.T) {
	asked := releasesFailing(t)
	var out bytes.Buffer
	checkForUpdate("v0.1.17", &out)()
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
	p, _ := updateCheckPath()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the attempt wasn't recorded: %v", err)
	}
	checkForUpdate("v0.1.17", &out)()
	if asked.Load() != 1 || out.Len() != 0 {
		t.Fatalf("asked %d times, printed %q", asked.Load(), out.String())
	}
}

func TestAnUpdateCheckThatNeverAnswersSlowsACommandOnce(t *testing.T) {
	var asked atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("CLOUDEX_RELEASES_URL", srv.URL+"/releases")
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	notice := checkForUpdate("v0.1.17", &out)
	start := time.Now()
	notice()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the first command waited %s", time.Since(start))
	}
	start = time.Now()
	checkForUpdate("v0.1.17", &out)()
	if asked.Load() != 1 || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("asked %d times; the second command waited %s", asked.Load(), time.Since(start))
	}
}

func TestNoUpdateCheckZeroAndFalseLeaveTheCheckOn(t *testing.T) {
	for value, off := range map[string]bool{"": false, "0": false, "false": false, "FALSE": false, "1": true, "true": true, "yes": true} {
		t.Setenv("CLOUDEX_NO_UPDATE_CHECK", value)
		if got := updateCheckOff(); got != off {
			t.Errorf("CLOUDEX_NO_UPDATE_CHECK=%q: off is %v, want %v", value, got, off)
		}
	}
}
