package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lucataco/cloudex/go/internal/config"
)

// A deployment that approves a device login on the second poll.
func TestLoginWithoutAKeySignsInWithACode(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/auth/device":
			if r.Header.Get("Authorization") != "" || !strings.Contains(string(body), `"name":"ci box"`) {
				t.Errorf("device start: auth %q, body %s", r.Header.Get("Authorization"), body)
			}
			w.Write([]byte(`{"device_code":"dc1","user_code":"BCDF-GHJK","verification_uri":"x","verification_uri_complete":"x","expires_in":600,"interval":1}`))
		case "POST /v1/auth/token":
			if polls.Add(1) < 2 {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			w.Write([]byte(`{"access_token":"cx_aaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","token_type":"Bearer","token":{"id":"tk-aaaaaaaa","name":"ci box","kind":"token","role":"user","ownOnly":false,"createdAt":1,"createdBy":"admin","expiresAt":null,"lastUsedAt":null}}`))
		case "GET /v1/health":
			w.Write([]byte(`{"ok":true,"version":"0.2.0","previewZone":null}`))
		case "GET /v1/sandboxes":
			if r.Header.Get("Authorization") != "Bearer cx_aaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", "")
	t.Setenv("CLOUDEX_API_KEY", "")
	code, out, errOut := run(t, "login", "--url", srv.URL, "--name", "ci box", "--json")
	if code != 0 {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	first, _, _ := strings.Cut(out, "\n")
	var shown map[string]any
	if json.Unmarshal([]byte(first), &shown) != nil || shown["userCode"] != "BCDF-GHJK" || shown["approveWith"] != "cloudex tokens approve BCDF-GHJK" {
		t.Fatalf("first line %q", first)
	}
	cfg, _ := config.Load()
	if cfg.APIKey != "cx_aaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || cfg.APIURL != srv.URL {
		t.Fatalf("saved %+v", cfg)
	}
}

func TestLoginStopsWhenTheSignInIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth/device" {
			w.Write([]byte(`{"device_code":"dc1","user_code":"BCDF-GHJK","verification_uri":"x","verification_uri_complete":"x","expires_in":600,"interval":1}`))
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_KEY", "")
	code, _, errOut := run(t, "login", "--url", srv.URL)
	if code != 1 || !strings.Contains(errOut, "cloudex tokens approve BCDF-GHJK") || !strings.Contains(errOut, "the sign-in was refused") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestTokensCreateSendsTheRoleAndExpiry(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"POST /v1/tokens": `!201 {"id":"tk-aaaaaaaa","name":"ci","kind":"token","role":"user","ownOnly":true,"createdAt":1,"createdBy":"admin","expiresAt":null,"lastUsedAt":null,"secret":"cx_aaaaaaaa_s"}`,
	})
	code, out, errOut := run(t, "tokens", "create", "ci", "--own", "--expires", "90d")
	if code != 0 || !strings.Contains(out, "cx_aaaaaaaa_s") || !strings.Contains(out, "only the sandboxes it makes") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.bodies[0]), &body)
	if body["name"] != "ci" || body["role"] != "user" || body["ownOnly"] != true || body["expiresInSec"] != float64(90*86400) {
		t.Fatalf("body %v", body)
	}
}

func TestTokensApproveNeedsAWaitingCode(t *testing.T) {
	withFakeAPI(t, map[string]string{"GET /v1/auth/device": `[{"userCode":"BCDF-GHJK","name":"laptop","role":null,"ip":"1.2.3.4","country":"US","userAgent":null,"createdAt":1,"expiresAt":2}]`})
	code, _, errOut := run(t, "tokens", "approve", "zzzz-zzzz", "--yes")
	if code != 1 || !strings.Contains(errOut, "no sign-in is waiting with the code ZZZZ-ZZZZ") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestExpiry(t *testing.T) {
	for in, want := range map[string]int64{"never": 0, "": 0, "12h": 43200, "30d": 2592000, "90m": 5400} {
		if got, err := expiry(in); err != nil || got != want {
			t.Errorf("expiry(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"30s", "soon", "4000d"} {
		if _, err := expiry(bad); err == nil {
			t.Errorf("expiry(%q) accepted", bad)
		}
	}
	if normalizeCode(" bcdf ghjk ") != "BCDF-GHJK" {
		t.Error("normalizeCode")
	}
}
