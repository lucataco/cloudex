package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestTokenURL(t *testing.T) {
	u, err := url.Parse(TokenURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "dash.cloudflare.com" || q.Get("to") != "/:account/api-tokens" || q.Get("name") != "Cloudex" {
		t.Fatalf("unexpected link %s", TokenURL)
	}
	var perms []map[string]string
	if err := json.Unmarshal([]byte(q.Get("permissionGroupKeys")), &perms); err != nil {
		t.Fatal(err)
	}
	if len(perms) != 8 {
		t.Fatalf("got %d permissions, want 8", len(perms))
	}
	for _, p := range perms {
		if p["key"] == "" || (p["type"] != "edit" && p["type"] != "read") {
			t.Errorf("bad permission %v", p)
		}
	}
}

func TestInspect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/health":
			w.Write([]byte(`{"ok":true,"version":"v0.1.1","previewZone":"example.dev"}`))
		case r.Header.Get("Authorization") == "Bearer good":
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	live, err := inspect(ctx, srv.URL, srv.URL, "good")
	if err != nil || !live.running || !live.keyWorks || live.previewZone != "example.dev" {
		t.Fatalf("good key: %+v, %v", live, err)
	}
	live, err = inspect(ctx, srv.URL, srv.URL, "stale")
	if err != nil || !live.running || live.keyWorks {
		t.Fatalf("stale key: %+v, %v", live, err)
	}
	live, err = inspect(ctx, srv.URL, "", "")
	if err != nil || !live.running || live.keyWorks {
		t.Fatalf("no key: %+v, %v", live, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	live, err = inspect(ctx, gone.URL, gone.URL, "good")
	if err != nil || live.running {
		t.Fatalf("nothing deployed: %+v, %v", live, err)
	}
}

func TestWranglerConfigBindsBackupsOnlyWhenAvailable(t *testing.T) {
	if _, ok := wranglerConfig("2026-10-01", nil, "img", "db", "", false)["r2_buckets"]; ok {
		t.Fatal("bound R2 without a bucket")
	}
	b, ok := wranglerConfig("2026-10-01", nil, "img", "db", "", true)["r2_buckets"].([]any)
	if !ok || b[0].(map[string]any)["binding"] != "BACKUPS" {
		t.Fatalf("r2_buckets = %v", b)
	}
}

func TestWriteImageRetriesAFailedUpload(t *testing.T) {
	reg := registry.New(registry.Logger(stdlog.New(io.Discard, "", 0)))
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail the first layer upload with an error the registry client doesn't
		// retry by itself, like the TLS error seen in the wild.
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/blobs/uploads/") && dropped.CompareAndSwap(false, true) {
			http.Error(w, "upload broke", http.StatusBadRequest)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	defer func(n int, p time.Duration) { copyAttempts, copyPause = n, p }(copyAttempts, copyPause)
	copyPause = time.Millisecond

	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/cloudex-base:test")
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	log := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	if err := writeImage(context.Background(), dst, img, nil, log); err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	if !dropped.Load() || len(logged) != 1 || !strings.Contains(logged[0], "trying again") {
		t.Fatalf("expected one dropped upload and one retry, got dropped=%v log=%q", dropped.Load(), logged)
	}
	want, _ := img.Digest()
	got, err := remote.Head(dst)
	if err != nil || got.Digest != want {
		t.Fatalf("pushed image: %v %v, want %v", got, err, want)
	}

	// A registry that keeps failing gives up after copyAttempts.
	copyAttempts = 2
	logged = nil
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusForbidden)
	}))
	defer broken.Close()
	dst, _ = name.ParseReference(strings.TrimPrefix(broken.URL, "http://") + "/cloudex-base:test")
	if err := writeImage(context.Background(), dst, img, nil, log); err == nil || len(logged) != 1 {
		t.Fatalf("expected a failure after one retry, got %v, log %q", err, logged)
	}
}
