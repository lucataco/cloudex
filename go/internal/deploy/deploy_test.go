package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
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
