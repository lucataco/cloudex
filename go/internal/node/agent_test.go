package node

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeWorker checks a node's signatures as apps/api/src/nodeauth.ts does and
// runs each control connection with session.
type fakeWorker struct {
	t       *testing.T
	key     ed25519.PublicKey
	mu      sync.Mutex
	hellos  []Hello
	conns   int
	refuse  int // answer this status to connects, when set
	session func(n int, c *websocket.Conn)
}

func (f *fakeWorker) verify(r *http.Request, wantNode string) bool {
	ts, _ := strconv.ParseInt(r.Header.Get(HeaderTS), 10, 64)
	if d := time.Since(time.Unix(ts, 0)); d > time.Minute || d < -time.Minute {
		return false
	}
	if r.Header.Get(HeaderNode) != wantNode || len(r.Header.Get(HeaderNonce)) != 32 {
		return false
	}
	sig, _ := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSig))
	msg := SigningString(r.Method, r.URL.RequestURI(), r.Header.Get(HeaderSandbox), r.Header.Get(HeaderTS), r.Header.Get(HeaderNonce))
	return ed25519.Verify(f.key, []byte(msg), sig)
}

func (f *fakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/node/connect" {
		http.NotFound(w, r)
		return
	}
	if f.refuse != 0 {
		w.WriteHeader(f.refuse)
		w.Write([]byte(`{"error":"unauthorized: no"}`))
		return
	}
	if !f.verify(r, "nd-test0001") || r.Header.Get(HeaderProtocol) != "1" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad signature"}`))
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns++
	n := f.conns
	f.mu.Unlock()
	_, data, err := c.Read(r.Context())
	if err != nil {
		return
	}
	var h Hello
	if err := json.Unmarshal(data, &h); err != nil || h.Type != "hello" {
		f.t.Errorf("first message %s", data)
	}
	f.mu.Lock()
	f.hellos = append(f.hellos, h)
	f.mu.Unlock()
	f.session(n, c)
}

// answerPings is the Node object's auto-response: "ping" gets "pong".
func answerPings(ctx context.Context, c *websocket.Conn, other func(data []byte)) {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if string(data) == "ping" {
			c.Write(ctx, websocket.MessageText, []byte("pong"))
			continue
		}
		if other != nil {
			other(data)
		}
	}
}

func newAgent(t *testing.T, url string) (*Agent, ed25519.PublicKey, chan time.Duration) {
	pub, key, _ := ed25519.GenerateKey(nil)
	waits := make(chan time.Duration, 100)
	a := &Agent{
		API: url, ID: "nd-test0001", Key: key, Version: "test", State: State{Dir: t.TempDir()},
		PingEvery: 20 * time.Millisecond, PongDead: 80 * time.Millisecond,
		Facts: func() Facts {
			return Facts{Arch: "amd64", Runtime: "docker", Isolation: Isolation{Firewall: "unchecked"}}
		},
		Wait: func(ctx context.Context, d time.Duration) error {
			waits <- d
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return nil
			}
		},
	}
	return a, pub, waits
}

func TestTheAgentSaysHelloAndAnswersOps(t *testing.T) {
	results := make(chan Result, 2)
	f := &fakeWorker{t: t}
	f.session = func(n int, c *websocket.Conn) {
		ctx := context.Background()
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"op","id":"op-1","name":"ping","args":{}}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"op","id":"op-2","name":"exec","args":{"argv":["sh"]}}`))
		answerPings(ctx, c, func(data []byte) {
			var r Result
			json.Unmarshal(data, &r)
			results <- r
		})
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a, pub, _ := newAgent(t, srv.URL)
	f.key = pub
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	got := map[string]Result{}
	for len(got) < 2 {
		select {
		case r := <-results:
			got[r.ID] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("answers so far: %v", got)
		}
	}
	if !got["op-1"].OK || string(got["op-1"].Value) != "{}" {
		t.Errorf("ping: %+v", got["op-1"])
	}
	if got["op-2"].OK || !strings.Contains(got["op-2"].Error, "unknown op exec") {
		t.Errorf("an op outside the set: %+v", got["op-2"])
	}
	f.mu.Lock()
	h := f.hellos[0]
	f.mu.Unlock()
	if h.Protocol != Protocol || h.Version != "test" || h.Machines == nil {
		t.Errorf("hello %+v", h)
	}
	// Pongs keep it connected, and status.json says so.
	time.Sleep(200 * time.Millisecond)
	st, err := a.State.ReadStatus()
	if err != nil || !st.Connected || st.NodeID != "nd-test0001" || time.Since(time.UnixMilli(st.LastPongAt)) > time.Second {
		t.Fatalf("status %+v, %v", st, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns != 1 {
		t.Fatalf("%d connections; pongs should have kept the first", f.conns)
	}
}

func TestTheAgentComesBackAtOnceAfterADrop(t *testing.T) {
	f := &fakeWorker{t: t}
	f.session = func(n int, c *websocket.Conn) {
		if n == 1 {
			c.Close(websocket.StatusGoingAway, "deploy")
			return
		}
		answerPings(context.Background(), c, nil)
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a, pub, waits := newAgent(t, srv.URL)
	f.key = pub
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	select {
	case d := <-waits:
		if d >= 250*time.Millisecond {
			t.Fatalf("waited %v after a drop", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never reconnected")
	}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.conns == 2 })
}

func TestTheAgentReplacesAConnectionThatStopsAnsweringPings(t *testing.T) {
	f := &fakeWorker{t: t}
	f.session = func(n int, c *websocket.Conn) {
		if n == 1 {
			// Half-open: reads, but nothing comes back.
			for {
				if _, _, err := c.Read(context.Background()); err != nil {
					return
				}
			}
		}
		answerPings(context.Background(), c, nil)
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a, pub, _ := newAgent(t, srv.URL)
	f.key = pub
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.conns == 2 })
}

func TestTheAgentWaitsAMinuteWhenRefusedOrRevoked(t *testing.T) {
	f := &fakeWorker{t: t, refuse: 401}
	srv := httptest.NewServer(f)
	defer srv.Close()
	a, pub, waits := newAgent(t, srv.URL)
	f.key = pub
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	if d := <-waits; d < RefusedWait {
		t.Fatalf("waited %v after a 401", d)
	}
	cancel()
	st, _ := a.State.ReadStatus()
	if st == nil || st.Connected || !strings.Contains(st.LastError, "unauthorized: no") {
		t.Fatalf("status %+v", st)
	}

	f2 := &fakeWorker{t: t}
	f2.session = func(n int, c *websocket.Conn) { c.Close(websocket.StatusCode(CloseRevoked), "this node was removed") }
	srv2 := httptest.NewServer(f2)
	defer srv2.Close()
	a2, pub2, waits2 := newAgent(t, srv2.URL)
	f2.key = pub2
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go a2.Run(ctx2)
	if d := <-waits2; d != RefusedWait {
		t.Fatalf("waited %v after a revoke", d)
	}
}

func TestJoinIsSignedWithTheKeyItBrings(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var j Join
		json.Unmarshal(body, &j)
		f := &fakeWorker{key: pub}
		if r.URL.Path != "/v1/node/join" || j.PublicKey != PublicKey(key) || !f.verify(r, "") {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized: that join token is unknown, used or expired"}`))
			return
		}
		if j.Token == "cxj_used" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized: that join token is unknown, used or expired"}`))
			return
		}
		w.Write([]byte(`{"node":{"id":"nd-test0001","name":"box"}}`))
	}))
	defer srv.Close()
	facts := Facts{Arch: "amd64", Runtime: "docker", Isolation: Isolation{Firewall: "unchecked"}}
	j, err := JoinNode(context.Background(), nil, srv.URL, "cxj_good", "test", key, facts)
	if err != nil || j.Node.ID != "nd-test0001" || j.Node.Name != "box" {
		t.Fatalf("%+v, %v", j, err)
	}
	_, err = JoinNode(context.Background(), nil, srv.URL, "cxj_used", "test", key, facts)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Status != 401 || !strings.Contains(err.Error(), "used or expired") {
		t.Fatalf("a used token: %v", err)
	}
}

func TestTheKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	s := State{Dir: t.TempDir() + "/state"}
	k1, err := s.Key()
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := s.Key()
	if !k1.Equal(k2) {
		t.Fatal("a second read made another key")
	}
	fi, _ := statFile(s.path("node.key"))
	if fi&0o077 != 0 {
		t.Fatalf("node.key mode %o", fi)
	}
	if id, err := s.Identity(); id != nil || err != nil {
		t.Fatalf("identity before joining: %v, %v", id, err)
	}
	s.SaveIdentity(Identity{ID: "nd-test0001", Name: "box", APIURL: "https://x"})
	if id, _ := s.Identity(); id == nil || id.ID != "nd-test0001" {
		t.Fatalf("identity %+v", id)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statFile(p string) (os.FileMode, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Mode().Perm(), nil
}
