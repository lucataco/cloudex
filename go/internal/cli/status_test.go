package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := New("test")
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestStatusReportsAPIVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ok":true,"version":"1.2.3"}`))
	}))
	defer srv.Close()

	out, err := run(t, "status", "--api-url", srv.URL+"/", "--json=false")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "version 1.2.3") {
		t.Fatalf("output %q does not mention the API version", out)
	}
}

func TestStatusFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := run(t, "status", "--api-url", srv.URL); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("got %v, want a 500 error", err)
	}
}

func TestStatusNeedsAPIURL(t *testing.T) {
	t.Setenv("CLOUDEX_API_URL", "")
	if _, err := run(t, "status", "--api-url", ""); err == nil || !strings.Contains(err.Error(), "no API URL") {
		t.Fatalf("got %v, want a missing-URL error", err)
	}
}
