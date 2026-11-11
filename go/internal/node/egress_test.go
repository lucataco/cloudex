package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDeployment stands in for the Worker's /v1/node/egress: it checks the
// node's signature as nodeauth.ts does and records what arrived.
type fakeDeployment struct {
	t    *testing.T
	pub  ed25519.PublicKey
	mu   sync.Mutex
	seen []*http.Request
	body [][]byte
	srv  *httptest.Server
	// answer writes the response; by default 200 "from the deployment".
	answer func(w http.ResponseWriter, r *http.Request)
}

func newFakeDeployment(t *testing.T, pub ed25519.PublicKey) *fakeDeployment {
	f := &fakeDeployment{t: t, pub: pub}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSig))
		msg := SigningString(r.Method, r.URL.EscapedPath()+"?"+r.URL.RawQuery, r.Header.Get(HeaderSandbox), r.Header.Get(HeaderTS), r.Header.Get(HeaderNonce))
		if r.URL.Path != EgressRoute || !ed25519.Verify(f.pub, []byte(msg), sig) {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, r)
		f.body = append(f.body, b)
		f.mu.Unlock()
		if f.answer != nil {
			f.answer(w, r)
			return
		}
		io.WriteString(w, "from the deployment")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDeployment) last() (*http.Request, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return nil, nil
	}
	return f.seen[len(f.seen)-1], f.body[len(f.body)-1]
}

func (f *fakeDeployment) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func testEgress(t *testing.T) (*Egress, *fakeDeployment, *egressNet) {
	pub, key, _ := ed25519.GenerateKey(nil)
	f := newFakeDeployment(t, pub)
	ca, err := NewCA("nd-test0001", testHosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &Egress{API: f.srv.URL, Node: "nd-test0001", Key: key, HTTP: f.srv.Client(), CA: func() *CA { return ca }}
	n, _ := NetFor("cx-abc123", netip.MustParsePrefix("10.250.0.16/28"))
	return e, f, &egressNet{sandbox: "cx-abc123", net: n}
}

func TestEgressInternal(t *testing.T) {
	e, f, en := testEgress(t)
	do := func(method, target string, body io.Reader, h map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, body)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.serveInternal(en, w, r)
		return w
	}

	// Answered on the node: the deployment never hears of it.
	w := do("POST", "http://cloudex.internal/save-on-term", nil, nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"saved":false}` || f.count() != 0 {
		t.Fatalf("save-on-term: %d %q, sent %d", w.Code, w.Body, f.count())
	}

	// The self API goes on, signed for this node and sandbox, with the
	// machine's headers but none of its own x-cx-* (it can't name another
	// sandbox), and the target from the path alone.
	w = do("PATCH", "http://anything.example/v1/sandboxes/self?x=1", strings.NewReader(`{"idleMinutes":5}`),
		map[string]string{"x-cloudex-self": "tok", "x-cx-sandbox": "cx-zzz999", "x-cx-node": "nd-other000", "x-cx-guest-ua": "forged", "content-type": "application/json", "User-Agent": "Python-urllib/3.12"})
	r, body := f.last()
	if w.Code != 200 || r == nil {
		t.Fatalf("self API: %d %q", w.Code, w.Body)
	}
	if r.Method != "PATCH" || r.Header.Get(HeaderSandbox) != "cx-abc123" || r.Header.Get(HeaderNode) != "nd-test0001" ||
		r.Header.Get("x-cloudex-self") != "tok" || string(body) != `{"idleMinutes":5}` {
		t.Fatalf("forwarded: %s sandbox %q node %q self %q body %q", r.Method, r.Header.Get(HeaderSandbox), r.Header.Get(HeaderNode), r.Header.Get("x-cloudex-self"), body)
	}
	// The machine's User-Agent rides in a header of the agent's (the edge
	// refuses some), which the machine can't set itself.
	if r.Header.Get("User-Agent") != "cloudex-node/dev" || r.Header.Get(HeaderGuestUA) != "Python-urllib/3.12" {
		t.Fatalf("user agents: %q, guest %q", r.Header.Get("User-Agent"), r.Header.Get(HeaderGuestUA))
	}
	if got := r.URL.Query().Get("target"); got != "http://cloudex.internal/v1/sandboxes/self?x=1" {
		t.Fatalf("target %q", got)
	}

	// Activity: the first goes on, the next within 30 s is answered here.
	n0 := f.count()
	do("POST", "http://cloudex.internal/activity", nil, nil)
	do("POST", "http://cloudex.internal/activity", nil, nil)
	if f.count() != n0+1 {
		t.Fatalf("activity sent %d times, want 1", f.count()-n0)
	}

	// Refused here: CONNECT, WebSockets.
	if w := do("CONNECT", "http://cloudex.internal/", nil, nil); w.Code != 405 {
		t.Fatalf("CONNECT: %d", w.Code)
	}
	if w := do("GET", "http://cloudex.internal/x", nil, map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}); w.Code != 426 {
		t.Fatalf("upgrade: %d", w.Code)
	}
}

func TestEgressBodyLimit(t *testing.T) {
	e, f, en := testEgress(t)
	// Declared too big: refused before anything is sent.
	r := httptest.NewRequest("PUT", "http://cloudex.internal/saves/sv-abcdefghij/0", strings.NewReader("x"))
	r.ContentLength = EgressBodyMax + 1
	w := httptest.NewRecorder()
	e.serveInternal(en, w, r)
	if w.Code != 413 || !strings.Contains(w.Body.String(), "100 MB") || f.count() != 0 {
		t.Fatalf("declared: %d %q sent %d", w.Code, w.Body, f.count())
	}
	// Chunked, and over the limit while it's sent: refused, and the
	// deployment never gets a whole body.
	r = httptest.NewRequest("PUT", "http://cloudex.internal/saves/sv-abcdefghij/0", io.LimitReader(zeros{}, EgressBodyMax+10))
	r.ContentLength = -1
	w = httptest.NewRecorder()
	e.serveInternal(en, w, r)
	if w.Code != 413 {
		t.Fatalf("chunked: %d %q", w.Code, w.Body)
	}
	// A save part (64 MiB) fits.
	if PartSize := 64 << 20; PartSize > EgressBodyMax {
		t.Fatalf("save parts (%d) are bigger than the limit", PartSize)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// The provider listener over real TLS: the node CA's certificate for a
// provider host, nothing for any other name, and the Host must be the name.
func TestEgressProviderTLS(t *testing.T) {
	e, f, en := testEgress(t)
	f.answer = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
		}
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.serveProvider(en, w, r) }))
	srv.TLS = e.tlsConfig()
	srv.StartTLS()
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(e.CA().PEM)
	dial := func(name, host string) (*http.Response, error) {
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name}}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		}
		req, _ := http.NewRequest("POST", "https://"+host+"/v1/messages", bytes.NewReader([]byte(`{"x":1}`)))
		req.Header.Set("x-api-key", "sk-ant-cloudex-placeholder")
		return (&http.Client{Transport: tr}).Do(req)
	}
	res, err := dial("api.anthropic.com", "api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	r, _ := f.last()
	if res.StatusCode != 200 || string(b) != "data: 0\n\ndata: 1\n\ndata: 2\n\n" || r.URL.Query().Get("target") != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("provider call: %d %q target %v", res.StatusCode, b, r)
	}
	if r.Header.Get("x-api-key") != "sk-ant-cloudex-placeholder" {
		t.Fatalf("the placeholder didn't go on as it came: %q", r.Header.Get("x-api-key"))
	}
	// Another name: no certificate, so no request.
	n := f.count()
	if _, err := dial("github.com", "github.com"); err == nil {
		t.Fatal("a handshake for github.com worked")
	}
	if _, err := dial("api.anthropic.com.evil.example", "api.anthropic.com.evil.example"); err == nil {
		t.Fatal("a handshake for a look-alike worked")
	}
	// The right name in TLS, another in Host: refused, not sent.
	res, err = dial("api.anthropic.com", "api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest || f.count() != n {
		t.Fatalf("Host unlike SNI: %d, sent %d", res.StatusCode, f.count()-n)
	}
}

// What a machine can't make the agent do: its x-cx-* headers never reach the
// deployment, and the URL it gives is only ever a path on the target.
func TestEgressTargetFromPathOnly(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{"/v1/sandboxes/self", "http://cloudex.internal/v1/sandboxes/self"},
		{"http://evil.example/steal?x=1", "http://cloudex.internal/steal?x=1"},
		{"/a%2Fb", "http://cloudex.internal/a%2Fb"},
	} {
		r := httptest.NewRequest("GET", c.line, nil)
		if got := "http://cloudex.internal" + requestPath(r); got != c.want {
			t.Fatalf("%s: %s, want %s", c.line, got, c.want)
		}
	}
	full, signed, err := EgressURL("https://api.example.dev/", "https://api.openai.com/v1/responses?a=b&c=d")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(full)
	if u.Query().Get("target") != "https://api.openai.com/v1/responses?a=b&c=d" || signed != u.EscapedPath()+"?"+u.RawQuery {
		t.Fatalf("%s %s", full, signed)
	}
}

// A real listener on loopback: only a peer inside the machine's /28 gets an
// answer. Needs 127.0.0.2 (Linux); skipped where it can't be bound.
func TestEgressListenerIdentity(t *testing.T) {
	e, f, _ := testEgress(t)
	_ = f
	n, _ := NetFor("cx-abc123", netip.MustParsePrefix("127.0.0.0/28"))
	e.Ports = []int{0, 0}
	en, err := e.listen("cx-abc123", n)
	if err != nil {
		t.Skipf("can't bind %s here: %v", n.Agent, err)
	}
	defer en.close()
	get := func(src string) error {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
		c, err := d.Dial("tcp4", en.addrs[0])
		if err != nil {
			return err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(c, "POST /save-on-term HTTP/1.1\r\nHost: cloudex.internal\r\nContent-Length: 0\r\n\r\n")
		b, err := io.ReadAll(io.LimitReader(c, 300))
		if err != nil && len(b) == 0 {
			return err
		}
		if !strings.Contains(string(b), `{"saved":false}`) {
			return fmt.Errorf("answer %q", b)
		}
		return nil
	}
	if err := get("127.0.0.3"); err != nil {
		t.Fatalf("the machine's own address: %v", err)
	}
	if err := get("127.0.0.20"); err == nil {
		t.Fatal("a peer outside the /28 was answered")
	}
	if err := get("127.0.0.1"); err == nil {
		t.Fatal("the network's own address (the host's) was answered")
	}
}
