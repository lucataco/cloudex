package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://cloudex-api.x.workers.dev":  "https://cloudex-api.x.workers.dev",
		"https://cloudex-api.x.workers.dev/": "https://cloudex-api.x.workers.dev",
		"cloudex-api.x.workers.dev":          "https://cloudex-api.x.workers.dev",
		"127.0.0.1:8787/":                    "https://127.0.0.1:8787",
		"http://127.0.0.1:8787":              "http://127.0.0.1:8787",
		" https://x.dev \n":                  "https://x.dev",
		"":                                   "",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownRouteMeansTheDeploymentIsOlder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"ok":true,"version":"v0.1.15","previewZone":null}`))
		case "/v1/sandboxes/web":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no sandbox named web"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "key")
	c.CLIVersion = "v0.1.17"
	ctx := context.Background()

	_, err := c.ListSaves(ctx)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 404 ||
		apiErr.Message != "this deployment is older than the CLI (v0.1.15 vs v0.1.17): update it with cloudex deploy" {
		t.Fatalf("unknown route: %v", err)
	}
	if _, err := c.Get(ctx, "web"); err == nil || err.Error() != "no sandbox named web" {
		t.Fatalf("missing sandbox: %v", err)
	}
	if _, err := c.request(ctx, http.MethodGet, "/v1/sandboxes/web/commands", nil, ""); err == nil || !strings.Contains(err.Error(), "older than the CLI") {
		t.Fatalf("request: %v", err)
	}
}

func TestACancelledCallIsNotAConnectionProblem(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := New(srv.URL, "key").List(ctx)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "can't reach") {
		t.Fatalf("got %v", err)
	}
}
