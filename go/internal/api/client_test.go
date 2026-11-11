package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://cloudex-api.x.workers.dev":  "https://cloudex-api.x.workers.dev",
		"https://cloudex-api.x.workers.dev/": "https://cloudex-api.x.workers.dev",
		"cloudex-api.x.workers.dev":          "https://cloudex-api.x.workers.dev",
		"127.0.0.1:8787/":                    "https://127.0.0.1:8787",
		"http://127.0.0.1:8787":              "http://127.0.0.1:8787",
		" https://x.dev \n":                  "https://x.dev",
		"HTTPS://Cloudex-API.X.Workers.dev/": "https://cloudex-api.x.workers.dev",
		"https://x.dev:443/":                 "https://x.dev",
		"x.dev:443":                          "https://x.dev",
		"http://x.dev:443":                   "http://x.dev:443",
		"https://x.dev:8443/Path":            "https://x.dev:8443/Path",
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

func TestAnHTMLErrorPageNamesTheStatusOnce(t *testing.T) {
	for page, want := range map[string]string{
		"<html><head><title>502 Bad Gateway</title></head></html>":       "502 Bad Gateway",
		"<html><head><title>\n 502   Bad Gateway </title></head></html>": "502 Bad Gateway",
		"<html><head><title>Just a moment...</title></head></html>":      "502 Bad Gateway Just a moment...",
		"<html><body>no title</body></html>":                             "502 Bad Gateway",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(page))
		}))
		_, err := New(srv.URL, "key").List(context.Background())
		srv.Close()
		if err == nil || err.Error() != want {
			t.Errorf("page %q: got %v, want %q", page, err, want)
		}
	}
}

// 2026-11-12 review, W3: what a node wrote reaches the terminal without its
// control characters.
func TestAnAPIErrorLosesItsControlCharacters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"node vm: the self-test failed\u001b[2J\u001b]0;pwned\u0007 here‮\nsecond line\ttab"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k").Get(context.Background(), "x")
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("%v", err)
	}
	if strings.ContainsAny(ae.Message, "\x1b\x07‮") || !strings.Contains(ae.Message, "\nsecond line\ttab") {
		t.Fatalf("%q", ae.Message)
	}
}

// 2026-11-12 review, R2: a request for the deployment's records that gets no
// answer ends; one that starts something in a sandbox may take longer.
func TestARecordRequestTimesOutButAStartDoesNot(t *testing.T) {
	old := ShortTimeout
	ShortTimeout = 300 * time.Millisecond
	defer func() { ShortTimeout = old }()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes" && r.Method == http.MethodPost {
			time.Sleep(time.Second)
			w.Write([]byte(`{"id":"cx-abc123","state":"running"}`))
			return
		}
		<-hold // a connection a deploy cut without closing it
	}))
	defer srv.Close()
	defer close(hold)
	c := New(srv.URL, "k")
	t0 := time.Now()
	_, err := c.ListNodes(context.Background())
	if err == nil || time.Since(t0) > 5*time.Second || !strings.Contains(err.Error(), "didn't answer within") {
		t.Fatalf("after %v: %v", time.Since(t0), err)
	}
	if s, err := c.Create(context.Background(), CreateRequest{}); err != nil || s.ID != "cx-abc123" {
		t.Fatalf("a create that takes longer than ShortTimeout: %v", err)
	}
}

// Review R2: removing a node gets an answer within ShortTimeout, or with
// --force (which removes its machines first) a little longer; it never hangs.
func TestDeleteNodeTimesOut(t *testing.T) {
	old, oldForce := ShortTimeout, ForceNodeTimeout
	ShortTimeout, ForceNodeTimeout = 200*time.Millisecond, 200*time.Millisecond
	defer func() { ShortTimeout, ForceNodeTimeout = old, oldForce }()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold // a connection a deploy cut without closing it
	}))
	defer srv.Close()
	defer close(hold)
	c := New(srv.URL, "k")
	for _, force := range []bool{false, true} {
		t0 := time.Now()
		_, err := c.DeleteNode(context.Background(), "vm", force)
		if err == nil || time.Since(t0) > 5*time.Second || !strings.Contains(err.Error(), "didn't answer within") {
			t.Fatalf("force %v, after %v: %v", force, time.Since(t0), err)
		}
	}
}

// Review W3: CleanText keeps newlines, tabs and ordinary text, and makes
// every other control character and bidirectional override a "?".
func TestCleanText(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"plain text, ünïcode and 日本語": "plain text, ünïcode and 日本語",
		"two\nlines\tand a tab":       "two\nlines\tand a tab",
		"\x1b[2Jcleared\x07":          "?[2Jcleared?",
		"\r\x00\x7f\u0085\u009b":      "?????",
		"abc‮def⁦x⁩‪":                 "abc?def?x??",
		"  and ​ stay":                "  and ​ stay",
	} {
		if got := CleanText(in); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
		}
	}
}
