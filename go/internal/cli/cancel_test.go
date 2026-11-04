//go:build !windows

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestCtrlCDuringAnAPICallEndsQuietly(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(sandboxJSON))
			return
		}
		io.Copy(io.Discard, r.Body) // the server only notices a closed connection once the body is read
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	go func() {
		<-started
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	code, _, errOut := run(t, "desktop", "web", "--no-open")
	if code != 130 || strings.Contains(errOut, "can't reach") || strings.Contains(errOut, "error:") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}
