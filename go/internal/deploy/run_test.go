package deploy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/cloudflare"
)

const (
	apiHost    = "cloudex-api.test.workers.dev"
	oldKey     = "cxk_old"
	sourceRepo = "ghcr.io/lucataco/cloudex-base"
)

// world stands in for everything Run talks to: the Cloudflare API, the
// registries, and the deployed API. Requests never leave the process; they go
// to handlers by host name.
type world struct {
	t *testing.T
	// cf answers Cloudflare calls by "METHOD /path"; a call it doesn't know fails the test.
	cf    map[string]func(r *http.Request) (int, any)
	calls []string
	// The deployed API. Until deployed, its host answers 404.
	deployed  bool
	key       string
	zone      string
	sandboxes []map[string]any
	apiCalls  []string
	// source serves the public image.
	source http.Handler
	// apiDeletes are the DELETE calls the API got, in order.
	apiDeletes []string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newWorld(t *testing.T) *world {
	w := &world{t: t}
	reg := registry.New(registry.Logger(stdlog.New(io.Discard, "", 0)))
	w.source = reg
	handlers := map[string]http.Handler{
		"api.cloudflare.com":      http.HandlerFunc(w.serveCloudflare),
		"registry.cloudflare.com": reg,
		"ghcr.io":                 http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.source.ServeHTTP(rw, r) }),
		apiHost:                   http.HandlerFunc(w.serveAPI),
	}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h, ok := handlers[req.URL.Host]
		if !ok {
			w.t.Errorf("a request to %s", req.URL)
			return nil, errors.New("no such host in the test")
		}
		in := req.Clone(req.Context())
		in.Host, in.RequestURI = req.URL.Host, req.URL.RequestURI()
		if in.Body == nil {
			in.Body = http.NoBody // as a server always has
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, in)
		res := rec.Result()
		res.Request = req
		return res, nil
	})
	oldDefault, oldRemote, oldAsset := http.DefaultTransport, remote.DefaultTransport, readAsset
	http.DefaultTransport, remote.DefaultTransport = rt, rt
	readAsset = func(name string) ([]byte, error) {
		if name == "assets/meta.json" {
			return []byte(`{"compatibility_date":"2026-10-01"}`), nil
		}
		return []byte("export default {}"), nil
	}
	t.Cleanup(func() { http.DefaultTransport, remote.DefaultTransport, readAsset = oldDefault, oldRemote, oldAsset })

	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference(sourceRepo + ":edge")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}

	ok := func(v any) func(*http.Request) (int, any) { return func(*http.Request) (int, any) { return 200, v } }
	const script = "/accounts/acc/workers/scripts/cloudex-api"
	w.cf = map[string]func(*http.Request) (int, any){
		"GET /accounts": ok([]any{map[string]any{"id": "acc", "name": "Test"}}),
		"GET /accounts/acc/containers/applications":                                    ok([]any{}),
		"GET /accounts/acc/workers/subdomain":                                          ok(map[string]any{"subdomain": "test"}),
		"POST /accounts/acc/containers/registries/registry.cloudflare.com/credentials": ok(map[string]any{"username": "u", "password": "p"}),
		"POST /accounts/acc/containers/image-preparations":                             ok(map[string]any{"status": "ready"}),
		"GET /accounts/acc/d1/database":                                                ok([]any{map[string]any{"uuid": "db1", "name": "cloudex"}}),
		"GET /accounts/acc/r2/buckets/cloudex-backups":                                 ok(map[string]any{}),
		"PUT " + script: func(r *http.Request) (int, any) {
			r.ParseMultipartForm(1 << 20)
			var md struct {
				Bindings []map[string]any `json:"bindings"`
			}
			json.Unmarshal([]byte(r.FormValue("metadata")), &md)
			for _, b := range md.Bindings {
				switch b["name"] {
				case "ADMIN_KEY":
					w.key = b["text"].(string)
				case "PREVIEW_ZONE":
					w.zone = b["text"].(string)
				}
			}
			w.deployed = true
			return 200, map[string]any{}
		},
		"GET " + script + "/settings": ok(map[string]any{"bindings": []any{
			map[string]any{"type": "durable_object_namespace", "class_name": "Sandbox", "namespace_id": "ns1"}}}),
		"GET /accounts/acc/containers/applications/ns1": ok(map[string]any{"name": "cloudex-api-sandbox", "scheduling_policy": "durable_object"}),
		"POST " + script + "/subdomain":                 ok(map[string]any{"enabled": true}),
	}
	return w
}

func (w *world) serveCloudflare(rw http.ResponseWriter, r *http.Request) {
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/client/v4")
	call := r.Method + " " + r.URL.Path
	w.calls = append(w.calls, call)
	handle, ok := w.cf[call]
	if !ok {
		w.t.Errorf("unexpected Cloudflare call %s", call)
		handle = func(*http.Request) (int, any) { return 404, 0 }
	}
	status, result := handle(r)
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	if status >= 300 {
		code, _ := result.(int)
		json.NewEncoder(rw).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": "no"}}})
		return
	}
	json.NewEncoder(rw).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func (w *world) serveAPI(rw http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	w.apiCalls = append(w.apiCalls, call)
	rw.Header().Set("Content-Type", "application/json")
	reply := func(status int, v any) {
		rw.WriteHeader(status)
		json.NewEncoder(rw).Encode(v)
	}
	switch {
	case !w.deployed:
		reply(404, map[string]string{"error": "not found"})
	case call == "GET /v1/health":
		var zone any
		if w.zone != "" {
			zone = w.zone
		}
		reply(200, map[string]any{"ok": true, "version": "v0.1.15", "previewZone": zone})
	case r.Header.Get("Authorization") != "Bearer "+w.key:
		reply(401, map[string]string{"error": "unauthorized"})
	case call == "GET /v1/sandboxes":
		reply(200, append([]map[string]any{}, w.sandboxes...))
	case call == "POST /v1/sandboxes":
		reply(201, map[string]any{"id": "cx-check1", "state": "running"})
	case call == "POST /v1/sandboxes/cx-check1/exec":
		reply(200, map[string]any{"exitCode": 0, "stdout": "ok\n"})
	case r.Method == http.MethodDelete:
		w.apiDeletes = append(w.apiDeletes, call)
		reply(200, map[string]any{})
	default:
		w.t.Errorf("unexpected API call %s", call)
		reply(404, map[string]string{"error": "no route " + call})
	}
}

func (w *world) called(call string) bool {
	for _, c := range w.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (w *world) options(extra func(*Options)) Options {
	o := Options{Token: "tok", Image: sourceRepo + ":edge", Log: io.Discard, SkipCheck: true}
	if extra != nil {
		extra(&o)
	}
	return o
}

func TestRunDeploysAFirstTime(t *testing.T) {
	w := newWorld(t)
	res, err := Run(context.Background(), w.options(func(o *Options) { o.SkipCheck = false }))
	if err != nil {
		t.Fatal(err)
	}
	if res.APIURL != "https://"+apiHost || !strings.HasPrefix(res.APIKey, "cxk_") || res.APIKey != w.key || res.AccountID != "acc" {
		t.Fatalf("result %+v, the Worker holds %q", res, w.key)
	}
	if strings.Join(w.apiDeletes, " ") != "DELETE /v1/sandboxes/cx-check1" {
		t.Fatalf("the test sandbox wasn't deleted: %v", w.apiDeletes)
	}
}

func TestRunReportsTheKeyBeforeTheSlowSteps(t *testing.T) {
	w := newWorld(t)
	w.cf["GET /zones"] = func(*http.Request) (int, any) {
		return 200, []any{map[string]any{"id": "z1", "name": "example.dev"}}
	}
	w.cf["PUT /accounts/acc/workers/scripts/cloudex-api/routes"] = func(*http.Request) (int, any) { return 200, []any{} }
	stop := errors.New("interrupted")
	var saved Result
	res, err := Run(context.Background(), w.options(func(o *Options) {
		o.PreviewDomain, o.PreviewDomainSet, o.SkipCheck = "example.dev", true, false
		o.OnInstalled = func(r Result) error {
			saved = r
			return stop
		}
	}))
	if !errors.Is(err, stop) || res == nil {
		t.Fatalf("got %v, %v", res, err)
	}
	if saved.APIKey == "" || saved.APIKey != w.key || saved.APIURL != "https://"+apiHost || saved.PreviewZone != "example.dev" {
		t.Fatalf("OnInstalled got %+v, the Worker holds %q", saved, w.key)
	}
	if w.called("POST /zones/z1/dns_records") || len(w.apiCalls) != 1 || w.apiCalls[0] != "GET /v1/health" {
		t.Fatalf("Cloudflare calls %v, API calls %v", w.calls, w.apiCalls)
	}
}

func TestRunKeepsTheKeyWhenTheDNSRecordIsRefused(t *testing.T) {
	w := newWorld(t)
	w.cf["GET /zones"] = func(*http.Request) (int, any) {
		return 200, []any{map[string]any{"id": "z1", "name": "example.dev"}}
	}
	w.cf["PUT /accounts/acc/workers/scripts/cloudex-api/routes"] = func(*http.Request) (int, any) { return 200, []any{} }
	w.cf["GET /zones/z1/dns_records"] = func(*http.Request) (int, any) { return 200, []any{} }
	w.cf["POST /zones/z1/dns_records"] = func(*http.Request) (int, any) { return 403, 10000 }
	var saved Result
	_, err := Run(context.Background(), w.options(func(o *Options) {
		o.PreviewDomain, o.PreviewDomainSet = "example.dev", true
		o.OnInstalled = func(r Result) error { saved = r; return nil }
	}))
	if saved.APIKey == "" || saved.APIKey != w.key {
		t.Fatalf("the key wasn't reported: %+v", saved)
	}
	for _, want := range []string{"preview DNS record", "DNS Write", "cloudex deploy --help"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in %v", want, err)
		}
	}
}

func TestReplacingTheKeySaysWhatStopsWorking(t *testing.T) {
	for _, yes := range []bool{false, true} {
		w := newWorld(t)
		w.deployed, w.key = true, oldKey
		var question string
		var logged strings.Builder
		_, err := Run(context.Background(), w.options(func(o *Options) {
			o.Log = &logged
			o.Confirm = func(q string) bool { question = q; return yes }
		}))
		for _, want := range []string{"environment variables and files", "provider keys", "preview links"} {
			if !strings.Contains(question, want) {
				t.Errorf("the question lacks %q: %s", want, question)
			}
			if yes && !strings.Contains(logged.String(), want) {
				t.Errorf("replacing the key said nothing of %q: %s", want, logged.String())
			}
		}
		if yes != (err == nil) || (w.key != oldKey) != yes {
			t.Fatalf("yes=%v: error %v, key now %q", yes, err, w.key)
		}
	}
}

// previewsOn sets up a deployment whose previews are on old.dev, with a
// Cloudex route and record there and some of the user's own.
func previewsOn(t *testing.T) (w *world, deleted *[]string) {
	w = newWorld(t)
	w.deployed, w.key, w.zone = true, oldKey, "old.dev"
	deleted = new([]string)
	w.cf["GET /zones"] = func(r *http.Request) (int, any) {
		return 200, []any{map[string]any{"id": "z0", "name": r.URL.Query().Get("name")}}
	}
	w.cf["GET /zones/z0/workers/routes"] = func(*http.Request) (int, any) {
		return 200, []any{
			map[string]any{"id": "r1", "pattern": "*.old.dev/*", "script": "cloudex-api"},
			map[string]any{"id": "r2", "pattern": "www.old.dev/*", "script": "mine"},
			map[string]any{"id": "r3", "pattern": "*.old.dev/*", "script": "someone-else"},
		}
	}
	w.cf["GET /zones/z0/dns_records"] = func(*http.Request) (int, any) {
		return 200, []any{
			map[string]any{"id": "d1", "type": "AAAA", "content": "100::", "comment": "Cloudex preview links"},
			map[string]any{"id": "d2", "type": "AAAA", "content": "100::", "comment": "mine"},
			map[string]any{"id": "d3", "type": "TXT", "content": "hello", "comment": "Cloudex preview links"},
		}
	}
	remove := func(r *http.Request) (int, any) {
		*deleted = append(*deleted, r.URL.Path)
		return 200, map[string]any{}
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		w.cf["DELETE /zones/z0/workers/routes/"+id] = remove
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		w.cf["DELETE /zones/z0/dns_records/"+id] = remove
	}
	return w, deleted
}

func TestTurningPreviewsOffRemovesCloudexsRouteAndRecord(t *testing.T) {
	w, deleted := previewsOn(t)
	var logged strings.Builder
	_, err := Run(context.Background(), w.options(func(o *Options) {
		o.Log = &logged
		o.ExistingURL, o.ExistingKey = "https://"+apiHost, oldKey
		o.PreviewDomain, o.PreviewDomainSet = "", true
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/zones/z0/workers/routes/r1", "/zones/z0/dns_records/d1"}
	if strings.Join(*deleted, " ") != strings.Join(want, " ") {
		t.Fatalf("deleted %v, want %v", *deleted, want)
	}
	if w.zone != "" {
		t.Fatalf("the Worker still names the zone %q", w.zone)
	}
	if !strings.Contains(logged.String(), "no longer reach the API") {
		t.Fatalf("log %q", logged.String())
	}
}

func TestSwitchingPreviewDomainRemovesTheOldRoute(t *testing.T) {
	w, deleted := previewsOn(t)
	w.cf["GET /zones"] = func(r *http.Request) (int, any) {
		if name := r.URL.Query().Get("name"); name == "old.dev" {
			return 200, []any{map[string]any{"id": "z0", "name": name}}
		}
		return 200, []any{map[string]any{"id": "z1", "name": "new.dev"}}
	}
	w.cf["PUT /accounts/acc/workers/scripts/cloudex-api/routes"] = func(*http.Request) (int, any) { return 200, []any{} }
	w.cf["GET /zones/z1/dns_records"] = func(*http.Request) (int, any) { return 200, []any{} }
	w.cf["POST /zones/z1/dns_records"] = func(*http.Request) (int, any) { return 200, map[string]any{} }
	_, err := Run(context.Background(), w.options(func(o *Options) {
		o.ExistingURL, o.ExistingKey = "https://"+apiHost, oldKey
		o.PreviewDomain, o.PreviewDomainSet = "new.dev", true
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(*deleted, " ") != "/zones/z0/workers/routes/r1 /zones/z0/dns_records/d1" || w.zone != "new.dev" {
		t.Fatalf("deleted %v, the Worker's zone is %q", *deleted, w.zone)
	}
}

func TestDeployingAgainKeepsThePreviewRoute(t *testing.T) {
	w, deleted := previewsOn(t)
	w.cf["PUT /accounts/acc/workers/scripts/cloudex-api/routes"] = func(*http.Request) (int, any) { return 200, []any{} }
	w.cf["GET /zones/z0/dns_records"] = func(*http.Request) (int, any) { return 200, []any{map[string]any{"id": "d1"}} }
	_, err := Run(context.Background(), w.options(func(o *Options) {
		o.ExistingURL, o.ExistingKey = "https://"+apiHost, oldKey
	}))
	if err != nil || len(*deleted) != 0 {
		t.Fatalf("error %v, deleted %v", err, *deleted)
	}
}

func TestTurningPreviewsOffLeavesAZoneTheTokenCantSee(t *testing.T) {
	w, deleted := previewsOn(t)
	w.cf["GET /zones"] = func(*http.Request) (int, any) { return 200, []any{} }
	var logged strings.Builder
	_, err := Run(context.Background(), w.options(func(o *Options) {
		o.Log = &logged
		o.ExistingURL, o.ExistingKey = "https://"+apiHost, oldKey
		o.PreviewDomainSet = true
	}))
	if err != nil || len(*deleted) != 0 || !strings.Contains(logged.String(), "old.dev, which isn't a zone this token can reach") {
		t.Fatalf("error %v, deleted %v, log %q", err, *deleted, logged.String())
	}
}

func TestRunSaysWhichPermissionTheTokenLacks(t *testing.T) {
	for call, want := range map[string]string{
		"GET /accounts": "Account Settings Read",
		"GET /accounts/acc/containers/applications":        "Workers Containers Write",
		"GET /accounts/acc/workers/subdomain":              "Workers Scripts Write",
		"GET /accounts/acc/d1/database":                    "D1 Write",
		"PUT /accounts/acc/workers/scripts/cloudex-api":    "Workers Scripts Write",
		"POST /accounts/acc/containers/image-preparations": "Workers Containers Write",
	} {
		w := newWorld(t)
		w.cf[call] = func(*http.Request) (int, any) { return 403, 10000 }
		_, err := Run(context.Background(), w.options(nil))
		if err == nil || !strings.Contains(err.Error(), "needs the "+want+" permission") || !strings.Contains(err.Error(), "cloudex deploy --help") {
			t.Errorf("%s: %v", call, err)
		}
	}
	w := newWorld(t)
	w.cf["GET /accounts/acc/containers/applications"] = func(*http.Request) (int, any) { return 400, 10000 }
	if _, err := Run(context.Background(), w.options(nil)); err == nil || !strings.Contains(err.Error(), "needs the Workers Containers Write permission") {
		t.Errorf("code 10000: %v", err)
	}
	w = newWorld(t)
	w.cf["GET /accounts/acc/containers/applications"] = func(*http.Request) (int, any) { return 400, 1234 }
	if _, err := Run(context.Background(), w.options(nil)); err == nil || !strings.Contains(err.Error(), "can't run containers yet") {
		t.Errorf("another failure: %v", err)
	}
}

func TestR2ProblemNamesThePermission(t *testing.T) {
	got := r2Problem(&cloudflare.Error{Status: 403, Message: "no"})
	if !strings.Contains(got, "Workers R2 Storage Write") || strings.Contains(got, "now includes") {
		t.Fatalf("got %q", got)
	}
}

func TestCheckSandboxDeletesLeftoverTestSandboxes(t *testing.T) {
	w := newWorld(t)
	w.deployed, w.key = true, oldKey
	w.sandboxes = []map[string]any{
		{"id": "cx-old1", "name": "deploy-check-ab12"},
		{"id": "cx-mine", "name": "web"},
		{"id": "cx-none", "name": nil},
	}
	client := apiClient(w)
	var logged []string
	log := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	if err := checkSandbox(context.Background(), client, log); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.apiDeletes, " ") != "DELETE /v1/sandboxes/cx-old1 DELETE /v1/sandboxes/cx-check1" {
		t.Fatalf("deleted %v", w.apiDeletes)
	}
}

func TestReadImageIgnoresAStaleDockerLogin(t *testing.T) {
	for _, c := range []struct {
		name    string
		private bool
		login   string
		wantErr bool
	}{
		{"public, stale login", false, "stale", false},
		{"public, no login", false, "", false},
		{"private, good login", true, "good", false},
		{"private, stale login", true, "stale", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.source = registryWithLogins(w.source, c.private)
			dir := t.TempDir()
			t.Setenv("DOCKER_CONFIG", dir)
			if c.login != "" {
				auth := base64.StdEncoding.EncodeToString([]byte("me:" + c.login))
				cfg := fmt.Sprintf(`{"auths":{"ghcr.io":{"auth":%q}}}`, auth)
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ref, _ := name.ParseReference(sourceRepo + ":edge")
			_, err := readImage(ref)
			if (err != nil) != c.wantErr {
				t.Fatalf("error %v, want error %v", err, c.wantErr)
			}
		})
	}
}

// registryWithLogins wraps a registry like ghcr.io: every call needs a token;
// anyone gets one for a public image, and only the login "me:good" for a private one.
func registryWithLogins(reg http.Handler, private bool) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			user, pass, has := r.BasicAuth()
			switch {
			case has && (user != "me" || pass != "good"):
				http.Error(rw, "bad login", http.StatusUnauthorized)
			case has:
				rw.Write([]byte(`{"token":"member"}`))
			default:
				rw.Write([]byte(`{"token":"anonymous"}`))
			}
		case r.Header.Get("Authorization") == "Bearer member" || (!private && r.Header.Get("Authorization") == "Bearer anonymous"):
			reg.ServeHTTP(rw, r)
		default:
			rw.Header().Set("WWW-Authenticate", `Bearer realm="https://ghcr.io/token",service="ghcr.io"`)
			rw.WriteHeader(http.StatusUnauthorized)
			rw.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
		}
	})
}

func apiClient(w *world) *api.Client { return api.New("https://"+apiHost, w.key) }
