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
	"github.com/lucataco/cloudex/go/internal/cloudflare"
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

	live, err := inspect(ctx, srv.URL, "good", true)
	if err != nil || !live.running || !live.keyWorks || live.previewZone != "example.dev" {
		t.Fatalf("good key: %+v, %v", live, err)
	}
	live, err = inspect(ctx, srv.URL, "stale", true)
	if err != nil || !live.running || live.keyWorks {
		t.Fatalf("stale key: %+v, %v", live, err)
	}
	live, err = inspect(ctx, srv.URL, "", true)
	if err != nil || !live.running || live.keyWorks {
		t.Fatalf("no key: %+v, %v", live, err)
	}
	// Another deployment's API never sees this computer's key.
	live, err = inspect(ctx, srv.URL, "good", false)
	if err != nil || !live.running || live.keyWorks {
		t.Fatalf("not ours: %+v, %v", live, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	live, err = inspect(ctx, gone.URL, "good", true)
	if err != nil || live.running {
		t.Fatalf("nothing deployed: %+v, %v", live, err)
	}
}

func TestIsWorkersDevAPI(t *testing.T) {
	for url, want := range map[string]bool{
		"https://cloudex-api.old-name.workers.dev": true,
		"https://cloudex-api.example.com":          false,
		"https://other.old-name.workers.dev":       false,
		"":                                         false,
	} {
		if got := isWorkersDevAPI(url); got != want {
			t.Errorf("%q: %v", url, got)
		}
	}
}

func TestWorkerMetadataBindsBackupsOnlyWhenAvailable(t *testing.T) {
	has := func(md map[string]any, name string) bool {
		for _, b := range md["bindings"].([]map[string]any) {
			if b["name"] == name {
				return true
			}
		}
		return false
	}
	if has(workerMetadata("2026-10-01", nil, "img", "db", "", "k", false), "BACKUPS") {
		t.Fatal("bound R2 without a bucket")
	}
	if !has(workerMetadata("2026-10-01", nil, "img", "db", "", "k", true), "BACKUPS") {
		t.Fatal("no BACKUPS binding")
	}
}

// fakeCloudflare answers like the Cloudflare API, from a handler that returns
// a status and a result for each "METHOD /path" it knows.
func fakeCloudflare(t *testing.T, handle func(r *http.Request) (int, any)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s without the token", r.Method, r.URL.Path)
		}
		status, result := handle(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 300 {
			code, _ := result.(int)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": "no"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallWorker(t *testing.T) {
	const base = "/accounts/acc/workers/scripts/cloudex-api"
	var calls []string
	appExists := false
	srv := fakeCloudflare(t, func(r *http.Request) (int, any) {
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		switch call {
		case "PUT " + base:
			if r.URL.Query().Get("bindings_inherit") != "strict" {
				t.Errorf("upload query %q", r.URL.RawQuery)
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("upload isn't a form: %v", err)
			}
			var md map[string]any
			if err := json.Unmarshal([]byte(r.FormValue("metadata")), &md); err != nil {
				t.Fatalf("metadata: %v", err)
			}
			c := md["containers"].([]any)[0].(map[string]any)
			if md["main_module"] != "worker.js" || c["name"] != "cloudex-api-sandbox" || c["images"].(map[string]any)["base"] != "img@sha256:1" {
				t.Errorf("metadata %v", md)
			}
			f, h, err := r.FormFile("worker.js")
			if err != nil {
				t.Fatalf("no worker.js part: %v", err)
			}
			code, _ := io.ReadAll(f)
			if string(code) != "export default {}" || h.Header.Get("Content-Type") != "application/javascript+module" {
				t.Errorf("worker.js %q (%s)", code, h.Header.Get("Content-Type"))
			}
			return 200, map[string]any{"id": "cloudex-api"}
		case "GET " + base + "/settings":
			return 200, map[string]any{"bindings": []any{
				map[string]any{"type": "d1", "name": "DB"},
				map[string]any{"type": "durable_object_namespace", "name": "SANDBOX", "class_name": "Sandbox", "namespace_id": "ns1"},
			}}
		case "GET /accounts/acc/containers/applications/ns1":
			if appExists {
				return 200, map[string]any{"id": "ns1", "name": "cloudex-api-sandbox", "scheduling_policy": "durable_object"}
			}
			return 404, 0
		case "POST /accounts/acc/containers/applications":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "cloudex-api-sandbox" || body["scheduling_policy"] != "durable_object" || body["durable_objects"].(map[string]any)["namespace_id"] != "ns1" {
				t.Errorf("application %v", body)
			}
			appExists = true
			return 200, map[string]any{"id": "ns1"}
		case "POST " + base + "/subdomain":
			if r.Header.Get("Cloudflare-Workers-Script-Api-Date") == "" {
				t.Error("subdomain call without the API date header")
			}
			return 200, map[string]any{"enabled": true}
		case "PUT " + base + "/routes":
			return 403, 10000 // a token limited to some zones
		case "GET /zones/z1/workers/routes":
			return 200, []any{map[string]any{"pattern": "other.example.dev/*", "script": "x"}}
		case "POST /zones/z1/workers/routes":
			return 200, map[string]any{"id": "r1"}
		}
		t.Errorf("unexpected call %s", call)
		return 404, 0
	})
	cf := cloudflare.NewAt("tok", srv.URL)
	md := workerMetadata("2026-10-01", nil, "img@sha256:1", "db", "example.dev", "key", true)
	var logged []string
	log := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	ctx := context.Background()

	if err := installWorker(ctx, cf, "acc", md, []byte("export default {}"), "z1", "example.dev", log); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT " + base, "GET " + base + "/settings",
		"GET /accounts/acc/containers/applications/ns1", "POST /accounts/acc/containers/applications",
		"POST " + base + "/subdomain",
		"PUT " + base + "/routes", "GET /zones/z1/workers/routes", "POST /zones/z1/workers/routes",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(strings.Join(logged, "\n"), "Created the container application") {
		t.Errorf("log %q", logged)
	}

	// Deploying again keeps the application, and without previews sets no route.
	calls = nil
	if err := installWorker(ctx, cf, "acc", md, []byte("export default {}"), "", "", log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(calls, " "), "POST /accounts/acc/containers/applications") || strings.Contains(strings.Join(calls, " "), "routes") {
		t.Fatalf("second deploy calls %q", calls)
	}
}

func TestPrepareImageWaitsUntilReady(t *testing.T) {
	defer func(d time.Duration) { preparePause = d }(preparePause)
	preparePause = time.Millisecond
	var answers []map[string]any
	srv := fakeCloudflare(t, func(r *http.Request) (int, any) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/accounts/acc/containers/image-preparations" || body["image"] != "img@sha256:1" {
			t.Errorf("%s %s %v", r.Method, r.URL.Path, body)
		}
		a := answers[0]
		answers = answers[1:]
		return 200, a
	})
	cf := cloudflare.NewAt("tok", srv.URL)
	var logged []string
	log := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	answers = []map[string]any{{"status": "pending"}, {"status": "pending"}, {"status": "ready"}}
	if err := prepareImage(context.Background(), cf, "acc", "img@sha256:1", log); err != nil || len(answers) != 0 || len(logged) != 1 {
		t.Fatalf("err %v, %d answers left, log %q", err, len(answers), logged)
	}
	answers = []map[string]any{{"status": "error", "reason": "image too large"}}
	if err := prepareImage(context.Background(), cf, "acc", "img@sha256:1", log); err == nil || !strings.Contains(err.Error(), "image too large") {
		t.Fatalf("error status gave %v", err)
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
