package cli

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/direct"
)

// The fakes: a node agent's direct port, other servers that answer at a
// candidate's address, the Worker path and the API.

func listen(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// fakeAgent serves the direct port with key: TLS, the ticket line, then
// "ok" and a banner, or "err <refuse>".
func fakeAgent(t *testing.T, key ed25519.PrivateKey, refuse string, tickets *atomic.Int32) string {
	cert, err := direct.NodeCert(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := direct.ServerTLS(cert)
	return listen(t, func(raw net.Conn) {
		defer raw.Close()
		c := tls.Server(raw, cfg)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if c.Handshake() != nil {
			return
		}
		line, err := direct.ReadLine(c, direct.LineMax)
		if err != nil || !strings.HasPrefix(line, "CXD1 ") {
			return
		}
		if tickets != nil {
			tickets.Add(1)
		}
		if refuse != "" {
			io.WriteString(c, "err "+refuse+"\n")
			return
		}
		io.WriteString(c, "ok\nagent\n")
		io.Copy(io.Discard, c)
	})
}

func ecdsaServer(t *testing.T) string {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}, NextProtos: []string{direct.ALPN}}
	return listen(t, func(raw net.Conn) {
		defer raw.Close()
		c := tls.Server(raw, cfg)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Handshake()
		io.Copy(io.Discard, c)
	})
}

func captivePortal(t *testing.T) string {
	return listen(t, func(c net.Conn) {
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 4096))
		io.WriteString(c, "HTTP/1.1 302 Found\r\nLocation: http://login.example/\r\nContent-Length: 0\r\n\r\n")
		io.Copy(io.Discard, c)
	})
}

// stall accepts and never answers: the hairpin of N8.
func stall(t *testing.T) string {
	return listen(t, func(c net.Conn) {
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		io.Copy(io.Discard, c)
	})
}

func closedPort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

const blackhole = "blackhole"

type fakeNet struct {
	mu    sync.Mutex
	at    map[string]string // candidate → real address, or blackhole
	dials []string
	first time.Time // of the first dial
}

func (f *fakeNet) dial(ctx context.Context, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dials = append(f.dials, addr)
	if f.first.IsZero() {
		f.first = time.Now()
	}
	real, ok := f.at[addr]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("no such fake address " + addr)
	}
	if real == blackhole {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", real)
}

type fakeEnds struct {
	t          *testing.T
	ans        *api.Direct
	apiErr     error
	apiDelay   time.Duration
	apiCalls   atomic.Int32
	apiAt      atomic.Int64 // ns after t0 the API answered
	workerAt   atomic.Int64 // ns after t0 the Worker path started, 0 for never
	workerRefs []string
	workerErr  error
	mu         sync.Mutex
	t0         time.Time
}

func (f *fakeEnds) api(ctx context.Context) (*api.Direct, error) {
	f.apiCalls.Add(1)
	select {
	case <-time.After(f.apiDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.apiAt.Store(int64(time.Since(f.t0)))
	if f.apiErr != nil {
		return nil, f.apiErr
	}
	a := *f.ans
	return &a, nil
}

func (f *fakeEnds) worker(ctx context.Context, ref string) (net.Conn, error) {
	f.workerAt.Store(int64(time.Since(f.t0)) + 1)
	f.mu.Lock()
	f.workerRefs = append(f.workerRefs, ref)
	f.mu.Unlock()
	if f.workerErr != nil {
		return nil, f.workerErr
	}
	a, b := net.Pipe()
	go func() {
		io.WriteString(b, "worker\n")
		io.Copy(io.Discard, b)
	}()
	return a, nil
}

var testNets = localNets{
	prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24"), netip.MustParsePrefix("100.80.14.76/32")},
	tailnet:  true,
}

type raceSetup struct {
	key     ed25519.PrivateKey
	pubB64  string
	net     *fakeNet
	ends    *fakeEnds
	race    *directRace
	tickets atomic.Int32
	logs    []string
	logMu   sync.Mutex
	late    chan func(*directCache)
}

func newRace(t *testing.T, addrs ...string) *raceSetup {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	s := &raceSetup{key: key, pubB64: base64.StdEncoding.EncodeToString(pub), net: &fakeNet{at: map[string]string{}}}
	ticket := "eyJ0ZXN0Ijp0cnVlfQ.c2ln"
	exp := time.Now().Add(time.Minute).UnixMilli()
	ans := &api.Direct{Sandbox: "cx-node01", Provider: "node", PublicKey: s.pubB64, Ticket: &ticket, ExpiresAt: &exp}
	ans.Node = &struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{"nd-aaaa1111", "box"}
	ans.Direct = &struct {
		Port  int      `json:"port"`
		Addrs []string `json:"addrs"`
	}{7022, addrs}
	s.ends = &fakeEnds{t: t, ans: ans, apiDelay: 20 * time.Millisecond}
	s.late = make(chan func(*directCache), 4)
	s.race = &directRace{
		ref: "cx-node01", id: "cx-node01", port: 22, mode: "auto",
		api: s.ends.api, worker: s.ends.worker, dial: s.net.dial,
		nets:  testNets,
		cache: readDirectCache(t.TempDir(), "https://api.example"),
		times: defaultRaceTimes,
		late:  func(f func(*directCache)) { s.late <- f },
		logf: func(format string, a ...any) {
			s.logMu.Lock()
			s.logs = append(s.logs, format)
			s.logMu.Unlock()
		},
	}
	return s
}

// run races, reads the first line from the winner and checks it's want.
func (s *raceSetup) run(t *testing.T, want string) (raceOutcome, time.Duration) {
	t.Helper()
	s.ends.t0 = time.Now()
	out, err := s.race.run(context.Background())
	took := time.Since(s.ends.t0)
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	defer out.conn.Close()
	out.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(out.conn).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("read %q (%v) via %s, want %q", line, err, out.via, want)
	}
	return out, took
}

func (s *raceSetup) applyChanges(c *directCache) {
	for _, f := range s.race.changes {
		f(c)
	}
}

// failedMark says whether the race marked the network failed, at once or
// once the attempts still going when the Worker path won had ended.
func (s *raceSetup) failedMark(t *testing.T) bool {
	c := readDirectCache(t.TempDir(), "x")
	s.applyChanges(c)
	if c.Failed[testNets.fingerprint()] != 0 {
		return true
	}
	select {
	case f := <-s.late:
		f(c)
	case <-time.After(1500 * time.Millisecond):
	}
	return c.Failed[testNets.fingerprint()] != 0
}

// N8's failure table: each bad candidate falls back to the Worker path
// within 350 ms of when the Worker path starts with no candidates at all
// (the API's answer).
func TestRaceFallsBackFromEachBadCandidate(t *testing.T) {
	impostor, _, _ := ed25519.GenerateKey(rand.Reader)
	_, impostorKey, _ := ed25519.GenerateKey(rand.Reader)
	_ = impostor
	cases := []struct {
		name  string
		serve func(t *testing.T) string
	}{
		{"blackhole", func(*testing.T) string { return blackhole }},
		{"closed port", closedPort},
		{"ECDSA TLS server", ecdsaServer},
		{"impostor cloudex-node key", func(t *testing.T) string { return fakeAgent(t, impostorKey, "", nil) }},
		{"HTTP 302 captive portal", captivePortal},
		{"accept then stall", stall},
	}
	for _, class := range []string{"192.168.1.10:7022", "100.101.102.103:7022", "10.9.8.7:7022"} {
		for _, c := range cases {
			t.Run(c.name+" at "+class, func(t *testing.T) {
				t.Parallel()
				s := newRace(t, class)
				s.net.at[class] = c.serve(t)
				out, _ := s.run(t, "worker")
				if out.via != "worker" {
					t.Fatalf("via %s", out.via)
				}
				late := time.Duration(s.ends.workerAt.Load() - s.ends.apiAt.Load())
				if late > 350*time.Millisecond {
					t.Fatalf("the Worker path started %v after the answer", late)
				}
				if s.tickets.Load() != 0 {
					t.Fatal("a ticket was sent")
				}
				if !s.failedMark(t) {
					t.Fatal("the network wasn't marked failed")
				}
			})
		}
	}
}

func TestRaceFallsBackWhenTheTicketIsRefused(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "expired", &s.tickets)
	out, _ := s.run(t, "worker")
	if out.via != "worker" || s.tickets.Load() != 1 {
		t.Fatalf("via %s, %d tickets", out.via, s.tickets.Load())
	}
	if late := time.Duration(s.ends.workerAt.Load() - s.ends.apiAt.Load()); late > 350*time.Millisecond {
		t.Fatalf("the Worker path started %v after the answer", late)
	}
	if s.failedMark(t) {
		t.Fatal("a refused ticket isn't the network's fault")
	}
}

// Case 8: a stale candidate first, the node second: direct wins.
func TestRaceWinsDirectPastAStaleCandidate(t *testing.T) {
	s := newRace(t, "192.168.1.99:7022", "100.101.102.103:7022")
	s.net.at["192.168.1.99:7022"] = blackhole
	s.net.at["100.101.102.103:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	out, _ := s.run(t, "agent")
	if out.via != "direct 100.101.102.103:7022 (tailnet)" || out.id != "cx-node01" {
		t.Fatalf("via %q id %q", out.via, out.id)
	}
	if s.ends.workerAt.Load() != 0 {
		t.Fatal("the Worker path was dialed")
	}
	if s.tickets.Load() != 1 {
		t.Fatalf("%d tickets", s.tickets.Load())
	}
}

func TestRaceSendsTheTicketToTheWinnerOnly(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022", "192.168.1.11:7022", "10.0.0.2:7022")
	var other atomic.Int32
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.net.at["192.168.1.11:7022"] = fakeAgent(t, s.key, "", &other)
	s.net.at["10.0.0.2:7022"] = fakeAgent(t, s.key, "", &other)
	out, _ := s.run(t, "agent")
	if out.via != "direct 192.168.1.10:7022 (lan-onlink)" {
		t.Fatalf("via %q", out.via)
	}
	time.Sleep(100 * time.Millisecond)
	if s.tickets.Load() != 1 || other.Load() != 0 {
		t.Fatalf("tickets: winner %d, others %d", s.tickets.Load(), other.Load())
	}
}

func TestOrderCandidates(t *testing.T) {
	got, skipped := orderCandidates([]string{
		"10.0.0.2", "100.64.1.2", "192.168.1.20", "fd7a:115c:a1e0::1", "192.168.1.21:7100", "192.168.1.20", "nonsense", "172.16.0.9",
	}, 7022, testNets)
	want := []candidate{
		{"192.168.1.20:7022", "lan-onlink"}, {"192.168.1.21:7100", "lan-onlink"},
		{"100.64.1.2:7022", "tailnet"},
		{"10.0.0.2:7022", "lan-routed"}, {"172.16.0.9:7022", "lan-routed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v", got)
	}
	if len(skipped) != 2 || !strings.HasPrefix(skipped[0], "fd7a:") {
		t.Fatalf("skipped %v: the tailnet's v6 /48 is neither on-link nor tried", skipped)
	}
	// Off the tailnet, 100.64/10 addresses aren't tried: the range alone
	// doesn't make a tailnet (a carrier's CGNAT uses it too).
	off := localNets{prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24"), netip.MustParsePrefix("100.70.0.9/10")}}
	got, skipped = orderCandidates([]string{"100.64.1.2", "192.168.1.20"}, 7022, off)
	if len(got) != 1 || got[0].class != "lan-onlink" || len(skipped) != 1 {
		t.Fatalf("off the tailnet: %v, skipped %v", got, skipped)
	}
}

func TestFingerprintFollowsTheNetworks(t *testing.T) {
	a := localNets{prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24"), netip.MustParsePrefix("10.1.2.3/8")}}
	b := localNets{prefixes: []netip.Prefix{netip.MustParsePrefix("10.200.0.1/8"), netip.MustParsePrefix("192.168.1.77/24")}}
	c := localNets{prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.5/24"), netip.MustParsePrefix("100.80.14.76/32")}}
	if a.fingerprint() != b.fingerprint() {
		t.Fatal("the same networks, other addresses: the fingerprint changed")
	}
	if a.fingerprint() == c.fingerprint() {
		t.Fatal("a VPN came up: the fingerprint didn't change")
	}
}

func TestRaceCachedCloudflareAsksNothing(t *testing.T) {
	s := newRace(t)
	s.race.cache.Sandboxes["cx-node01"] = "cloudflare"
	out, took := s.run(t, "worker")
	if out.via != "worker" || s.ends.apiCalls.Load() != 0 {
		t.Fatalf("via %s, %d API calls", out.via, s.ends.apiCalls.Load())
	}
	if took > 20*time.Millisecond {
		t.Fatalf("took %v", took)
	}
}

func TestRaceCloudflareAnswerIsCachedForGood(t *testing.T) {
	s := newRace(t)
	s.ends.ans = &api.Direct{Sandbox: "cx-node01", Provider: "cloudflare"}
	out, _ := s.run(t, "worker")
	if out.via != "worker" {
		t.Fatal(out.via)
	}
	if late := time.Duration(s.ends.workerAt.Load() - s.ends.apiAt.Load()); late > 5*time.Millisecond {
		t.Fatalf("the Worker path started %v after the answer", late)
	}
	dir := t.TempDir()
	updateDirectCache(dir, "u", time.Now().Add(1000*time.Hour), s.race.changes)
	if c := readDirectCache(dir, "u"); c.Sandboxes["cx-node01"] != "cloudflare" {
		t.Fatalf("cache %v", c.Sandboxes)
	}
}

func TestRaceCachedCandidatesStartBeforeTheAnswer(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.ends.apiDelay = 200 * time.Millisecond
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.race.cache.Sandboxes["cx-node01"] = "nd-aaaa1111"
	s.race.cache.Nodes["nd-aaaa1111"] = cachedNode{PublicKey: s.pubB64, Port: 7022, Addrs: []string{"192.168.1.10"}, At: time.Now().UnixMilli()}
	out, took := s.run(t, "agent")
	if !strings.HasPrefix(out.via, "direct ") {
		t.Fatal(out.via)
	}
	if first := s.net.first.Sub(s.ends.t0); first > 20*time.Millisecond {
		t.Fatalf("the first attempt began %v in", first)
	}
	// The ticket waits for the answer, and nothing else does.
	if took < 200*time.Millisecond || took > 260*time.Millisecond {
		t.Fatalf("took %v", took)
	}
}

func TestRaceIgnoresAStaleNodeCache(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.ends.apiDelay = 100 * time.Millisecond
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.race.cache.Sandboxes["cx-node01"] = "nd-aaaa1111"
	s.race.cache.Nodes["nd-aaaa1111"] = cachedNode{PublicKey: s.pubB64, Port: 7022, Addrs: []string{"192.168.1.10"}, At: time.Now().Add(-11 * time.Minute).UnixMilli()}
	s.run(t, "agent")
	if first := s.net.first.Sub(s.ends.t0); first < 100*time.Millisecond {
		t.Fatalf("a cache 11 minutes old was used (first attempt at %v)", first)
	}
}

// D14: a cached key that isn't the API's never gets the ticket.
func TestRaceDropsACachedKeyTheAPIDisowns(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.ends.apiDelay = 100 * time.Millisecond
	_, old, _ := ed25519.GenerateKey(rand.Reader)
	var oldTickets atomic.Int32
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.net.at["192.168.1.50:7022"] = fakeAgent(t, old, "", &oldTickets)
	s.race.cache.Sandboxes["cx-node01"] = "nd-aaaa1111"
	s.race.cache.Nodes["nd-aaaa1111"] = cachedNode{PublicKey: base64.StdEncoding.EncodeToString(old.Public().(ed25519.PublicKey)), Port: 7022, Addrs: []string{"192.168.1.50"}, At: time.Now().UnixMilli()}
	out, _ := s.run(t, "agent")
	if out.via != "direct 192.168.1.10:7022 (lan-onlink)" || oldTickets.Load() != 0 {
		t.Fatalf("via %s, the old key got %d tickets", out.via, oldTickets.Load())
	}
	c := readDirectCache(t.TempDir(), "x")
	s.applyChanges(c)
	if c.Nodes["nd-aaaa1111"].PublicKey != s.pubB64 {
		t.Fatal("the cache kept the old key")
	}
}

func TestRaceOnAFailedNetworkStartsTheWorkerAtOnce(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.net.at["192.168.1.10:7022"] = blackhole
	s.race.cache.Sandboxes["cx-node01"] = "nd-aaaa1111"
	s.race.cache.Failed[testNets.fingerprint()] = time.Now().Add(5 * time.Minute).UnixMilli()
	out, took := s.run(t, "worker")
	if out.via != "worker" || s.ends.workerAt.Load() > int64(10*time.Millisecond) {
		t.Fatalf("via %s, the Worker path started at %v", out.via, time.Duration(s.ends.workerAt.Load()))
	}
	if took > 60*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	// It still races direct, and a direct win clears the mark.
	s2 := newRace(t, "192.168.1.10:7022")
	s2.net.at["192.168.1.10:7022"] = fakeAgent(t, s2.key, "", &s2.tickets)
	s2.ends.workerErr = errors.New("slow worker")
	s2.race.cache.Failed[testNets.fingerprint()] = time.Now().Add(5 * time.Minute).UnixMilli()
	s2.run(t, "agent")
	c := readDirectCache(t.TempDir(), "x")
	c.Failed[testNets.fingerprint()] = 1
	s2.applyChanges(c)
	if _, ok := c.Failed[testNets.fingerprint()]; ok {
		t.Fatal("a direct win left the network marked failed")
	}
}

func TestRaceWithNoDirectPathGoesStraightToTheWorker(t *testing.T) {
	s := newRace(t)
	s.ends.ans.Direct, s.ends.ans.Ticket, s.ends.ans.ExpiresAt = nil, nil, nil
	out, _ := s.run(t, "worker")
	if out.via != "worker" || len(s.net.dials) != 0 {
		t.Fatalf("via %s, dials %v", out.via, s.net.dials)
	}
	if late := time.Duration(s.ends.workerAt.Load() - s.ends.apiAt.Load()); late > 5*time.Millisecond {
		t.Fatalf("the Worker path started %v after the answer", late)
	}
	c := readDirectCache(t.TempDir(), "x")
	s.applyChanges(c)
	if c.Failed[testNets.fingerprint()] != 0 {
		t.Fatal("no attempt was made; the network isn't to blame")
	}
}

func TestRaceFinalAnswersAreErrors(t *testing.T) {
	for _, status := range []int{404, 409, 410} {
		s := newRace(t)
		s.ends.apiErr = &api.Error{Status: status, Message: "no"}
		s.ends.t0 = time.Now()
		if _, err := s.race.run(context.Background()); err == nil || s.ends.workerAt.Load() != 0 {
			t.Fatalf("%d: err %v, worker %v", status, err, s.ends.workerAt.Load())
		}
	}
	// Anything else: the Worker path, which says what's wrong if anything is.
	s := newRace(t)
	s.ends.apiErr = &api.Error{Status: 503, Message: "busy"}
	if out, _ := s.run(t, "worker"); out.via != "worker" {
		t.Fatal(out.via)
	}
}

func TestRaceOnlyModeNeverUsesTheWorker(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.race.mode = "only"
	s.net.at["192.168.1.10:7022"] = closedPort(t)
	s.ends.t0 = time.Now()
	_, err := s.race.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CLOUDEX_DIRECT=only") || s.ends.workerAt.Load() != 0 {
		t.Fatalf("err %v, worker %v", err, s.ends.workerAt.Load())
	}
	s = newRace(t)
	s.race.mode = "only"
	s.race.cache.Sandboxes["cx-node01"] = "cloudflare"
	s.ends.ans = &api.Direct{Sandbox: "cx-node01", Provider: "cloudflare"}
	if _, err := s.race.run(context.Background()); err == nil || !strings.Contains(err.Error(), "Cloudflare") {
		t.Fatalf("err %v", err)
	}
	s = newRace(t, "192.168.1.10:7022")
	s.race.mode = "only"
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	if out, _ := s.run(t, "agent"); !strings.HasPrefix(out.via, "direct") {
		t.Fatal(out.via)
	}
}

func TestRaceOverrideReplacesTheCandidates(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.net.at["192.168.1.66:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	s.race.override = []string{"192.168.1.66"}
	if out, _ := s.run(t, "agent"); out.via != "direct 192.168.1.66:7022 (lan-onlink)" {
		t.Fatal(out.via)
	}
}

// A name whose /direct fails (but not finally) still connects, and its id
// comes from a lookup, as before v0.2.3.
func TestRaceANameTheAPIDidntAnswer(t *testing.T) {
	s := newRace(t)
	s.race.ref, s.race.id = "web", ""
	s.ends.apiErr = &api.Error{Status: 404, Message: "this deployment is older than the CLI", Older: true}
	s.race.get = func(context.Context) (*api.Sandbox, error) { return &api.Sandbox{ID: "cx-web123"}, nil }
	out, _ := s.run(t, "worker")
	if out.id != "cx-web123" || s.race.answered != "cx-web123" || !reflect.DeepEqual(s.ends.workerRefs, []string{"cx-web123"}) {
		t.Fatalf("id %q, worker refs %v", out.id, s.ends.workerRefs)
	}
	c := readDirectCache(t.TempDir(), "x")
	s.applyChanges(c)
	if c.Older == 0 {
		t.Fatal("an older deployment wasn't remembered")
	}
}

func TestDirectCacheBelongsToOneDeployment(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	updateDirectCache(dir, "https://a", now, []func(*directCache){func(c *directCache) {
		c.Sandboxes["cx-aaaaaa"] = "cloudflare"
		c.Nodes["nd-1"] = cachedNode{At: now.Add(-11 * time.Minute).UnixMilli()}
		c.Nodes["nd-2"] = cachedNode{At: now.UnixMilli()}
		c.Failed["old"] = now.Add(-time.Second).UnixMilli()
		c.Failed["new"] = now.Add(time.Minute).UnixMilli()
	}})
	c := readDirectCache(dir, "https://a")
	if c.Sandboxes["cx-aaaaaa"] != "cloudflare" || len(c.Nodes) != 1 || len(c.Failed) != 1 {
		t.Fatalf("cache %+v", c)
	}
	if info, err := os.Stat(directCachePath(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("direct.json: %v %v", info, err)
	}
	if c := readDirectCache(dir, "https://b"); len(c.Sandboxes) != 0 {
		t.Fatal("another deployment read this one's cache")
	}
	os.WriteFile(directCachePath(dir), []byte("{garbage"), 0o600)
	if c := readDirectCache(dir, "https://a"); c.Sandboxes == nil || len(c.Sandboxes) != 0 {
		t.Fatal("a corrupt cache")
	}
}

// The commands that fetch a sandbox anyway record where it runs, so the
// proxy that follows asks nothing for one on Cloudflare.
func TestListRecordsPlacementsForTheProxy(t *testing.T) {
	f := withFakeAPI(t, map[string]string{"GET /v1/sandboxes": "[" + sandboxJSON + "]"})
	if code, _, errOut := run(t, "list"); code != 0 {
		t.Fatal(errOut)
	}
	writeKnownHosts(t, "cx-abc123.cloudex")
	f.requests = nil
	run(t, "proxy", "cx-abc123", "22")
	if want := []string{"GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestProxyDirectOffIsTheWorkerPathAsBefore(t *testing.T) {
	f := proxyAPI(t, "cx-abc123")
	t.Setenv("CLOUDEX_DIRECT", "off")
	run(t, "proxy", "web.cloudex", "22")
	if want := []string{"GET /v1/sandboxes/web", "GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v", f.requests)
	}
	t.Setenv("CLOUDEX_DIRECT", "sometimes")
	if code, _, errOut := run(t, "proxy", "web.cloudex", "22"); code == 0 || !strings.Contains(errOut, "CLOUDEX_DIRECT") {
		t.Fatalf("code %d: %s", code, errOut)
	}
}

func TestProxyOnAnOlderDeploymentLooksTheNameUp(t *testing.T) {
	f := withFakeAPI(t, map[string]string{
		"GET /v1/sandboxes/web":        sandboxJSON,
		"GET /v1/sandboxes/web/direct": `!404 {"error":"not found"}`,
	})
	run(t, "proxy", "web.cloudex", "22")
	if want := []string{"GET /v1/sandboxes/web/direct", "GET /v1/sandboxes/web", "GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v", f.requests)
	}
	dir, _ := config.Dir()
	if got := readSSHNames(dir)["web"]; got != "cx-abc123" {
		t.Fatalf("recorded %q", got)
	}
	// Remembered for an hour: the next connection is the v0.2.2 one.
	f.requests = nil
	run(t, "proxy", "web.cloudex", "22")
	if want := []string{"GET /v1/sandboxes/web", "GET /v1/sandboxes/cx-abc123/tcp/22"}; !reflect.DeepEqual(f.requests, want) {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestDoctorChecksEachNodesDirectPort(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	fn := &fakeNet{at: map[string]string{
		"192.168.1.10:7022": fakeAgent(t, key, "", nil),
		"10.0.0.9:7022":     blackhole,
		"192.168.1.20:7022": fakeAgent(t, otherKey, "", nil),
	}}
	ns := []api.Node{
		{Name: "box", State: "online", PublicKey: pubB64, Direct: &api.NodeDirect{Port: 7022, Addrs: []string{"192.168.1.10", "10.0.0.9"}, State: "listening"}},
		{Name: "far", State: "online", PublicKey: pubB64, Direct: &api.NodeDirect{Port: 7022, Addrs: []string{"192.168.1.20"}, State: "listening"}},
		{Name: "bad", State: "online", PublicKey: pubB64, Direct: &api.NodeDirect{Port: 7022, State: "failed", Error: "published on a public address"}},
		{Name: "old", State: "online", Direct: &api.NodeDirect{Port: 7022, Addrs: []string{"192.168.1.10"}, State: "listening"}},
		{Name: "off", State: "offline", PublicKey: pubB64, Direct: &api.NodeDirect{Port: 7022, Addrs: []string{"192.168.1.10"}, State: "listening"}},
		{Name: "none", State: "online"},
	}
	start := time.Now()
	lines := directChecks(context.Background(), ns, testNets, fn.dial)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("took %v: the checks run at once", took)
	}
	if len(lines) != 4 {
		t.Fatalf("lines %v", lines)
	}
	if !lines[0].ok || !strings.Contains(lines[0].msg, "box answers directly at 192.168.1.10:7022 (lan-onlink") || !strings.Contains(lines[0].msg, "10.0.0.9:7022: timeout") {
		t.Fatalf("box: %+v", lines[0])
	}
	if lines[1].ok || !strings.Contains(lines[1].msg, "far's direct port doesn't answer from here (192.168.1.20:7022: wrong key") {
		t.Fatalf("far: %+v", lines[1])
	}
	if lines[2].ok || !strings.Contains(lines[2].msg, "failed: published on a public address") {
		t.Fatalf("bad: %+v", lines[2])
	}
	if lines[3].ok || !strings.Contains(lines[3].msg, "doesn't give its key") {
		t.Fatalf("old: %+v", lines[3])
	}
}

// A direct attempt that verifies after the Worker path won (a slow line,
// a slow answer) doesn't mark the network failed.
func TestRaceALateDirectSuccessKeepsTheNetwork(t *testing.T) {
	s := newRace(t, "100.101.102.103:7022")
	s.ends.apiDelay = 280 * time.Millisecond
	slow := fakeAgent(t, s.key, "", &s.tickets)
	s.net.at["100.101.102.103:7022"] = listen(t, func(c net.Conn) {
		defer c.Close()
		time.Sleep(250 * time.Millisecond) // a far line
		up, err := net.Dial("tcp", slow)
		if err != nil {
			return
		}
		defer up.Close()
		go io.Copy(up, c)
		io.Copy(c, up)
	})
	out, _ := s.run(t, "worker")
	if out.via != "worker" {
		t.Fatal(out.via)
	}
	if s.failedMark(t) {
		t.Fatal("marked failed though the node answered")
	}
}

// After a slow answer, attempts still get a head start on the Worker path.
func TestRaceASlowAnswerStillGivesDirectAHeadStart(t *testing.T) {
	s := newRace(t, "192.168.1.10:7022")
	s.ends.apiDelay = 290 * time.Millisecond
	s.net.at["192.168.1.10:7022"] = fakeAgent(t, s.key, "", &s.tickets)
	if out, _ := s.run(t, "agent"); !strings.HasPrefix(out.via, "direct") {
		t.Fatal(out.via)
	}
	if s.ends.workerAt.Load() != 0 {
		t.Fatal("the Worker path started")
	}
}
