package cli

// The direct path (v0.2.3): cloudex proxy, the ssh ProxyCommand, races a
// node's own direct port (go/internal/direct) against the Worker path and
// takes the first connection that proves it's the node. What it knows from
// earlier connections is in <config dir>/direct.json:
//
//   - sandboxes: id → "cloudflare" or the node's id. Ids never change
//     provider, so a sandbox known to be on Cloudflare is dialed at once,
//     with no extra call: ssh to Cloudflare is as fast as before.
//   - nodes: node id → its key and candidates, fresh for 10 minutes, so that
//     direct attempts start at once instead of after the API's answer.
//   - failed: a fingerprint of this computer's networks → until when the
//     Worker path starts at once there, after it won over direct attempts.
//
// The race, for a sandbox on a node: GET /v1/sandboxes/{ref}/direct at t=0;
// attempts at the candidates (at t=0 when cached, otherwise on the answer),
// on-link LAN first, then tailnet, then routed, 50 ms apart, each with 300 ms
// (on-link) or 1 s for TCP and TLS with the node's key pinned; the Worker
// path 300 ms after the first attempt, or at once when there is nothing to
// try. The first verified connection wins, and only it gets the ticket.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/direct"
)

const (
	directNodeFresh  = 10 * time.Minute // a node's cached candidates
	directFailedFor  = 10 * time.Minute // a network where the Worker won
	directOlderFor   = time.Hour        // a deployment without /direct
	directSandboxMax = 2000             // placements kept
)

// raceTimes are the race's timings; tests shorten nothing but use them.
type raceTimes struct {
	headStart time.Duration // the Worker path's start after the first attempt
	stagger   time.Duration // between attempts
	onLink    time.Duration // TCP and TLS to an on-link address
	other     time.Duration // to a tailnet or routed one
}

var defaultRaceTimes = raceTimes{headStart: 300 * time.Millisecond, stagger: 50 * time.Millisecond, onLink: 300 * time.Millisecond, other: time.Second}

// directMode is CLOUDEX_DIRECT: auto (the default), off (the Worker path
// only, as before v0.2.3) or only (direct or fail, for checks).
func directMode() (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(os.Getenv("CLOUDEX_DIRECT"))); m {
	case "", "auto", "on", "1":
		return "auto", nil
	case "off", "0", "no":
		return "off", nil
	case "only":
		return "only", nil
	default:
		return "", fmt.Errorf("CLOUDEX_DIRECT is %q: use auto, off or only", m)
	}
}

func debugOn() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CLOUDEX_DEBUG")))
	return v != "" && v != "0" && v != "false" && v != "no"
}

// ---------------------------------------------------------------- the cache

type directCache struct {
	API       string                `json:"api"`
	Sandboxes map[string]string     `json:"sandboxes"`
	Nodes     map[string]cachedNode `json:"nodes"`
	Failed    map[string]int64      `json:"failed"`          // fingerprint → until, ms
	Older     int64                 `json:"older,omitempty"` // until, ms: the deployment has no /direct
}

type cachedNode struct {
	PublicKey string   `json:"publicKey"`
	Port      int      `json:"port"`
	Addrs     []string `json:"addrs"`
	At        int64    `json:"at"` // ms
}

func directCachePath(dir string) string { return filepath.Join(dir, "direct.json") }

// readDirectCache never fails: a missing, unreadable or foreign file is an
// empty cache. It belongs to one deployment; another's starts afresh.
func readDirectCache(dir, apiURL string) *directCache {
	c := &directCache{}
	if data, err := os.ReadFile(directCachePath(dir)); err == nil {
		if json.Unmarshal(data, c) != nil || c.API != apiURL {
			c = &directCache{}
		}
	}
	c.API = apiURL
	if c.Sandboxes == nil {
		c.Sandboxes = map[string]string{}
	}
	if c.Nodes == nil {
		c.Nodes = map[string]cachedNode{}
	}
	if c.Failed == nil {
		c.Failed = map[string]int64{}
	}
	return c
}

// updateDirectCache reads the file again, applies the changes and writes it
// through a temporary file, so proxies running at once lose at most each
// other's latest change: it's only a cache. Errors are let go.
func updateDirectCache(dir, apiURL string, now time.Time, changes []func(*directCache)) {
	if len(changes) == 0 || dir == "" {
		return
	}
	c := readDirectCache(dir, apiURL)
	before, _ := json.Marshal(c)
	for _, f := range changes {
		f(c)
	}
	ms := now.UnixMilli()
	for k, until := range c.Failed {
		if until <= ms {
			delete(c.Failed, k)
		}
	}
	for k, n := range c.Nodes {
		if ms-n.At > directNodeFresh.Milliseconds() {
			delete(c.Nodes, k)
		}
	}
	for k := range c.Sandboxes {
		if len(c.Sandboxes) <= directSandboxMax {
			break
		}
		delete(c.Sandboxes, k)
	}
	after, err := json.MarshalIndent(c, "", "  ")
	if err != nil || string(before) == string(after) {
		return
	}
	writeFileAtomic(directCachePath(dir), append(after, '\n'), 0o600)
}

// notePlacement records where sandboxes run from records the CLI fetched
// anyway (cloudex ssh, list, info), so that proxy needs no call for them.
func notePlacement(apiURL string, ss ...api.Sandbox) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	var changes []func(*directCache)
	for _, s := range ss {
		where := "cloudflare"
		if n := s.OnNode(); n != nil {
			if n.ID == "" {
				continue
			}
			where = n.ID
		}
		id := s.ID
		changes = append(changes, func(c *directCache) { c.Sandboxes[id] = where })
	}
	updateDirectCache(dir, apiURL, time.Now(), changes)
}

// ---------------------------------------------------------------- this computer's networks

// localNets are this computer's own IPv4 networks on interfaces that are up,
// loopback aside.
type localNets struct {
	prefixes []netip.Prefix
	tailnet  bool // an address in 100.64/10 on a utun or tailscale interface
}

func readLocalNets() localNets {
	var l localNets
	ifs, err := net.Interfaces()
	if err != nil {
		return l
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok || !ip.Unmap().Is4() {
				continue
			}
			ones, _ := n.Mask.Size()
			p := netip.PrefixFrom(ip.Unmap(), ones)
			l.prefixes = append(l.prefixes, p)
			name := strings.ToLower(ifc.Name)
			if direct.IsTailscale(p.Addr()) && (strings.HasPrefix(name, "utun") || strings.Contains(name, "tailscale")) {
				l.tailnet = true
			}
		}
	}
	return l
}

// fingerprint names the networks this computer is on: it changes when the
// laptop moves or a VPN comes up.
func (l localNets) fingerprint() string {
	var s []string
	for _, p := range l.prefixes {
		s = append(s, p.Masked().String())
	}
	slices.Sort(s)
	s = slices.Compact(s)
	sum := sha256.Sum256([]byte(strings.Join(s, ",")))
	return hex.EncodeToString(sum[:8])
}

func (l localNets) onLink(a netip.Addr) bool {
	for _, p := range l.prefixes {
		if p.Bits() < 32 && p.Contains(a) && p.Addr() != a {
			return true
		}
	}
	return false
}

type candidate struct {
	addr  string // host:port
	class string // lan-onlink, tailnet or lan-routed
}

// orderCandidates puts on-link LAN addresses first, then tailnet ones (only
// when this computer is on a tailnet), then the rest. Entries are IPv4
// addresses, or address:port. It returns what it skipped, and why.
func orderCandidates(addrs []string, port int, l localNets) (cands []candidate, skipped []string) {
	var onlink, tail, rest []candidate
	seen := map[string]bool{}
	for _, raw := range addrs {
		host, p := raw, strconv.Itoa(port)
		if h, pp, err := net.SplitHostPort(raw); err == nil {
			host, p = h, pp
		}
		a, err := netip.ParseAddr(host)
		if n, perr := strconv.Atoi(p); err != nil || !a.Unmap().Is4() || perr != nil || n < 1 || n > 65535 {
			skipped = append(skipped, raw+": not an IPv4 address and port")
			continue
		}
		a = a.Unmap()
		c := candidate{addr: net.JoinHostPort(a.String(), p)}
		if seen[c.addr] {
			continue
		}
		seen[c.addr] = true
		switch {
		case direct.IsTailscale(a):
			c.class = "tailnet"
			if !l.tailnet {
				skipped = append(skipped, c.addr+": this computer isn't on a tailnet")
				continue
			}
			tail = append(tail, c)
		case l.onLink(a):
			c.class = "lan-onlink"
			onlink = append(onlink, c)
		default:
			c.class = "lan-routed"
			rest = append(rest, c)
		}
	}
	return append(append(onlink, tail...), rest...), skipped
}

func (r *directRace) timeoutFor(c candidate) time.Duration {
	if c.class == "lan-onlink" {
		return r.times.onLink
	}
	return r.times.other
}

// dialPinned opens TCP to addr and runs TLS with pub pinned, all within
// limit: a stall anywhere (a hairpin, a captive portal) fails in time.
func dialPinned(ctx context.Context, dial func(context.Context, string) (net.Conn, error), addr string, pub ed25519.PublicKey, limit time.Duration) (net.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	raw, err := dial(actx, addr)
	if err != nil {
		return nil, err
	}
	c, err := direct.HandshakeTLS(actx, raw, pub)
	if err != nil {
		return nil, err // not a nil *tls.Conn in an interface
	}
	return c, nil
}

func plainDial(ctx context.Context, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

// why says in a few words why an attempt failed.
func why(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, direct.ErrPin):
		return "wrong key: not this node"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "no route to host"
	case errors.Is(err, syscall.EHOSTDOWN):
		return "host is down"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
		return "closed before TLS finished"
	}
	var rh tls.RecordHeaderError
	if errors.As(err, &rh) {
		return "not the node's TLS"
	}
	return err.Error()
}

func decodeKey(s string) (ed25519.PublicKey, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, false
	}
	return ed25519.PublicKey(b), true
}

// ---------------------------------------------------------------- the race

type directRace struct {
	ref  string // as given: an id or a name
	id   string // the id when known before the answer (an id, or a name's last id)
	port int
	mode string // auto or only

	api    func(ctx context.Context) (*api.Direct, error)
	worker func(ctx context.Context, ref string) (net.Conn, error)
	dial   func(ctx context.Context, addr string) (net.Conn, error)
	// get resolves a name when /direct can't (an older deployment).
	get func(ctx context.Context) (*api.Sandbox, error)

	nets     localNets
	cache    *directCache
	override []string // CLOUDEX_DIRECT_CANDIDATES
	times    raceTimes
	now      func() time.Time
	logf     func(format string, args ...any) // nil: quiet

	changes []func(*directCache)
	t0      time.Time
	// answered is the sandbox id the API gave, even when no path connected
	// after: cloudex proxy records a name's id before ssh reads known_hosts.
	answered string
	// late, when set, saves a change decided after run returned: whether
	// direct attempts still going when the Worker path won all failed.
	late func(func(*directCache))
}

// raceOutcome is the connection to splice, how it was made, and the id the
// API gave (for cloudex proxy's name records), "" when it didn't answer.
type raceOutcome struct {
	conn net.Conn
	via  string
	id   string
}

func (r *directRace) log(format string, args ...any) {
	if r.logf == nil {
		return
	}
	r.logf("[%5.1fms] "+format, append([]any{float64(r.now().Sub(r.t0).Microseconds()) / 1000}, args...)...)
}

func (r *directRace) change(f func(*directCache)) { r.changes = append(r.changes, f) }

// finalStatus is an answer the Worker path would give as well.
func finalStatus(err error) bool {
	var e *api.Error
	if !errors.As(err, &e) || e.Older {
		return false
	}
	switch e.Status {
	case 401, 403, 404, 409, 410:
		return true
	}
	return false
}

type attemptDone struct {
	gen  int
	cand candidate
	conn net.Conn
	err  error
	took time.Duration
}

type workerDone struct {
	conn net.Conn
	err  error
}

type apiDone struct {
	ans *api.Direct
	err error
}

type ticketDone struct {
	conn net.Conn
	via  string
	err  error
}

// run races the paths and returns the winner's connection.
func (r *directRace) run(ctx context.Context) (raceOutcome, error) {
	if r.now == nil {
		r.now = time.Now
	}
	if r.dial == nil {
		r.dial = plainDial
	}
	r.t0 = r.now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fp := r.nets.fingerprint()
	failedNet := r.cache.Failed[fp] > r.t0.UnixMilli()
	placement := ""
	if r.id != "" {
		placement = r.cache.Sandboxes[r.id]
	}
	byID := r.id != "" && r.id == r.ref

	// A sandbox known to be on Cloudflare, by id, goes to the Worker at
	// once with no other call: as fast as before v0.2.3.
	if placement == "cloudflare" && byID && r.mode != "only" {
		r.log("%s is on Cloudflare (cached): the Worker path", r.id)
		c, err := r.worker(ctx, r.id)
		if err != nil {
			return raceOutcome{}, err
		}
		return raceOutcome{conn: c, via: "worker", id: r.id}, nil
	}

	attempts := make(chan attemptDone, 64)
	workers := make(chan workerDone, 1)
	apis := make(chan apiDone, 1)
	tickets := make(chan ticketDone, 1)

	var (
		gen         int
		genCtx                         = ctx
		genCancel   context.CancelFunc = func() {}
		pinB64      string
		pin         ed25519.PublicKey
		tried       = map[string]bool{}
		pending     int  // attempts not yet back
		made        int  // attempts launched
		verified    bool // an attempt passed TLS with the pin
		firstLaunch time.Time
		timer       *time.Timer
		timerC      <-chan time.Time

		workerStarted bool
		workerPending bool
		workerCancel  context.CancelFunc = func() {}
		workerErr     error
		wconn         net.Conn // the Worker path, connected

		ans        *api.Direct
		apiPending = true
		apiErr     error

		waiting     *attemptDone // verified, waiting for the API's ticket
		presenting  bool
		ticketErr   error
		attemptErrs []string
		done        bool
		markLate    bool // the Worker won with attempts still going: judge the network when they end
	)
	closeConn := func(c net.Conn) {
		if c != nil {
			c.Close()
		}
	}
	dropWaiting := func() {
		if waiting != nil {
			closeConn(waiting.conn)
			waiting = nil
		}
	}
	defer func() {
		if !markLate {
			genCancel()
		}
		if timer != nil {
			timer.Stop()
		}
		if !done {
			workerCancel()
			closeConn(wconn)
			dropWaiting()
		}
		// Results still on their way are closed as they come.
		go func(p int, w, t, mark bool, g int) {
			any := false
			for ; p > 0; p-- {
				a := <-attempts
				if a.gen == g && a.err == nil {
					any = true
				}
				closeConn(a.conn)
			}
			if mark && !any && r.late != nil {
				until := r.now().Add(directFailedFor).UnixMilli()
				r.late(func(c *directCache) { c.Failed[fp] = until })
			}
			if w {
				closeConn((<-workers).conn)
			}
			if t {
				closeConn((<-tickets).conn)
			}
		}(pending, workerPending, presenting, markLate, gen)
	}()

	startWorker := func(reason string) {
		if workerStarted || r.mode == "only" {
			return
		}
		workerStarted, workerPending = true, true
		if timer != nil {
			timer.Stop()
			timerC = nil
		}
		ref := r.ref
		if r.answered != "" {
			ref = r.answered
		}
		r.log("worker path starting: %s", reason)
		wctx, wc := context.WithCancel(ctx)
		workerCancel = wc
		go func() {
			c, err := r.worker(wctx, ref)
			workers <- workerDone{c, err}
		}()
	}

	launch := func(addrs []string, port int, key ed25519.PublicKey, keyB64, from string) {
		if r.override != nil {
			addrs = r.override
			from += ", replaced by CLOUDEX_DIRECT_CANDIDATES"
		}
		if keyB64 != pinB64 {
			// Another key: whatever was verified with the old one can't take
			// this ticket.
			genCancel()
			dropWaiting()
			gen++
			tried = map[string]bool{}
			pin, pinB64 = key, keyB64
			// Not ended with run: attempts still going when the Worker path
			// wins finish (within their limits) to say if the network failed.
			genCtx, genCancel = context.WithCancel(context.WithoutCancel(ctx))
		}
		cands, skipped := orderCandidates(addrs, port, r.nets)
		for _, s := range skipped {
			r.log("skip %s", s)
		}
		var fresh []candidate
		for _, c := range cands {
			if !tried[c.addr] {
				tried[c.addr] = true
				fresh = append(fresh, c)
			}
		}
		if len(fresh) == 0 {
			return
		}
		if firstLaunch.IsZero() {
			firstLaunch = r.now()
			if failedNet {
				startWorker("the Worker path won on this network lately")
			} else if timerC != nil {
				// Attempts that start late (after a slow answer) still get a
				// head start of two staggers on the Worker path.
				if left := r.times.headStart - firstLaunch.Sub(r.t0); left < 2*r.times.stagger {
					timer.Reset(2 * r.times.stagger)
				}
			}
		}
		r.log("trying %d candidate(s) from %s", len(fresh), from)
		g, gctx, key := gen, genCtx, pin
		for i, c := range fresh {
			pending++
			made++
			delay := time.Duration(i) * r.times.stagger
			limit := r.timeoutFor(c)
			go func() {
				if delay > 0 {
					t := time.NewTimer(delay)
					defer t.Stop()
					select {
					case <-t.C:
					case <-gctx.Done():
						attempts <- attemptDone{gen: g, cand: c, err: gctx.Err()}
						return
					}
				}
				start := time.Now()
				conn, err := dialPinned(gctx, r.dial, c.addr, key, limit)
				attempts <- attemptDone{gen: g, cand: c, conn: conn, err: err, took: time.Since(start)}
			}()
		}
	}

	ticketReady := func() bool { return ans != nil && ans.Ticket != nil && ans.PublicKey == pinB64 }
	presentOn := func(w *attemptDone) {
		presenting = true
		ticket, via, conn := *ans.Ticket, fmt.Sprintf("direct %s (%s)", w.cand.addr, w.cand.class), w.conn
		go func() {
			tctx, tc := context.WithTimeout(ctx, r.times.other)
			defer tc()
			err := direct.PresentTicket(tctx, conn, ticket)
			if err != nil {
				conn.Close()
			}
			tickets <- ticketDone{conn, via, err}
		}()
	}
	// finishWorker returns the Worker path, with the id the API gave, or
	// the one a lookup gives for a name it didn't answer about.
	finishWorker := func() (raceOutcome, error) {
		id := r.answered
		if id == "" && byID {
			id = r.id
		}
		done = true
		switch {
		case made > 0 && !verified && pending == 0:
			r.log("chose the Worker path; on this network it starts at once for the next %v", directFailedFor)
			until := r.now().Add(directFailedFor).UnixMilli()
			r.change(func(c *directCache) { c.Failed[fp] = until })
		case made > 0 && !verified:
			r.log("chose the Worker path; %d direct attempt(s) still going", pending)
			markLate = true
		default:
			r.log("chose the Worker path")
		}
		return raceOutcome{conn: wconn, via: "worker", id: id}, nil
	}

	actx, apiCancel := context.WithCancel(ctx)
	defer apiCancel()
	go func() {
		a, err := r.api(actx)
		apis <- apiDone{a, err}
	}()
	if failedNet {
		r.log("asking the API; the Worker path won on this network lately")
	} else {
		r.log("asking the API")
	}
	if r.mode != "only" {
		timer = time.NewTimer(r.times.headStart)
		timerC = timer.C
	}

	switch {
	case placement == "cloudflare":
		// By name: the API says which sandbox the name is now, and the
		// Worker path follows by id, on the connection the answer came on
		// (dialing by name at once was slower: 2026-11-14, M3 Results).
	case placement != "":
		if n, ok := r.cache.Nodes[placement]; ok && r.t0.UnixMilli()-n.At <= directNodeFresh.Milliseconds() {
			if key, ok := decodeKey(n.PublicKey); ok {
				launch(n.Addrs, n.Port, key, n.PublicKey, "the cache")
			}
		}
		if failedNet {
			startWorker("the Worker path won on this network lately")
		}
	}

	for {
		// The Worker path connected with no verified direct connection
		// ahead of it: it won, once the name's id is known.
		if wconn != nil && waiting == nil && !presenting && (byID || r.answered != "" || !apiPending) {
			return finishWorker()
		}
		directLive := pending > 0 || waiting != nil || presenting || apiPending
		// Cached candidates that all failed: the Worker path now, not after
		// the answer, which would most likely name the same ones.
		if !workerStarted && r.mode != "only" && pending == 0 && waiting == nil && !presenting && (!apiPending || made > 0) {
			reason := "nothing to try directly"
			switch {
			case ans != nil && ans.Provider != "node":
				reason = "on Cloudflare"
			case ticketErr != nil:
				reason = "the node refused the ticket"
			case made > 0:
				reason = "every direct attempt failed"
			}
			startWorker(reason)
		}
		if !directLive && !workerPending && wconn == nil {
			return raceOutcome{}, r.failure(ans, apiErr, workerErr, ticketErr, attemptErrs)
		}

		select {
		case <-ctx.Done():
			return raceOutcome{}, ctx.Err()

		case <-timerC:
			timerC = nil
			if waiting == nil && !presenting {
				startWorker("the direct attempts' head start is over")
			}

		case a := <-apis:
			apiPending = false
			if a.err != nil {
				apiErr = a.err
				var e *api.Error
				if errors.As(a.err, &e) && e.Older {
					r.log("the deployment has no direct path: it's older than this CLI")
					until := r.now().Add(directOlderFor).UnixMilli()
					r.change(func(c *directCache) { c.Older = until })
				} else {
					r.log("API: %v", a.err)
				}
				if finalStatus(a.err) {
					return raceOutcome{}, a.err
				}
				genCancel()
				dropWaiting()
				if !byID && r.get != nil {
					// Which sandbox the name is, as before v0.2.3.
					sb, err := r.get(ctx)
					if err != nil {
						return raceOutcome{}, err
					}
					r.answered = sb.ID
				}
				continue
			}
			ans = a.ans
			id := ans.Sandbox
			r.answered = id
			switch {
			case ans.Provider != "node":
				r.log("%s is on Cloudflare", id)
				r.change(func(c *directCache) { c.Sandboxes[id] = "cloudflare" })
				genCancel()
				dropWaiting()
			case ans.Node == nil || ans.Direct == nil || ans.Ticket == nil:
				node := ""
				if ans.Node != nil {
					node = ans.Node.ID
				}
				r.log("%s is on node %s, which has no direct path", id, node)
				if node != "" {
					r.change(func(c *directCache) { c.Sandboxes[id] = node; delete(c.Nodes, node) })
				}
				genCancel()
				dropWaiting()
			default:
				node, d, pk := ans.Node.ID, *ans.Direct, ans.PublicKey
				key, ok := decodeKey(pk)
				if !ok {
					r.log("the API's key for node %s isn't an Ed25519 key", node)
					ans.Ticket = nil
					genCancel()
					dropWaiting()
					break
				}
				r.log("%s is on node %s (%s), direct %v port %d", id, ans.Node.Name, node, d.Addrs, d.Port)
				r.change(func(c *directCache) {
					c.Sandboxes[id] = node
					c.Nodes[node] = cachedNode{PublicKey: pk, Port: d.Port, Addrs: d.Addrs, At: r.now().UnixMilli()}
				})
				if pinB64 != "" && pinB64 != pk {
					r.log("the node's key isn't the cached one: the cached attempts are dropped")
				}
				launch(d.Addrs, d.Port, key, pk, "the API")
				if waiting != nil && ticketReady() {
					w := waiting
					waiting = nil
					presentOn(w)
				}
			}

		case a := <-attempts:
			pending--
			if a.gen != gen {
				closeConn(a.conn)
				continue
			}
			if a.err != nil {
				if errors.Is(a.err, context.Canceled) {
					continue
				}
				r.log("  %-21s %-10s %6.1fms  %s", a.cand.addr, a.cand.class, ms(a.took), why(a.err))
				attemptErrs = append(attemptErrs, a.cand.addr+": "+why(a.err))
				if errors.Is(a.err, direct.ErrPin) {
					node := placement
					if ans != nil && ans.Node != nil {
						node = ans.Node.ID
					}
					if node != "" && node != "cloudflare" {
						r.change(func(c *directCache) { delete(c.Nodes, node) })
					}
				}
				if pending == 0 && runtime.GOOS == "darwin" && localNetworkBlocked(attemptErrs) {
					r.log("every attempt failed with no route to host: if the node is up on this network, macOS may be blocking this app's Local Network access (System Settings, Privacy & Security, Local Network)")
				}
				continue
			}
			verified = true
			r.log("  %-21s %-10s %6.1fms  verified", a.cand.addr, a.cand.class, ms(a.took))
			if waiting != nil || presenting || wconn != nil {
				closeConn(a.conn)
				continue
			}
			// The first verified connection wins: the other attempts stop.
			genCancel()
			if timer != nil {
				timer.Stop()
				timerC = nil
			}
			w := a
			if ticketReady() {
				presentOn(&w)
			} else {
				waiting = &w
				r.log("waiting for the API's ticket")
			}

		case t := <-tickets:
			presenting = false
			if t.err == nil {
				r.log("chose %s", t.via)
				done = true
				workerCancel()
				closeConn(wconn)
				wconn = nil
				r.change(func(c *directCache) { delete(c.Failed, fp) })
				return raceOutcome{conn: t.conn, via: t.via, id: ans.Sandbox}, nil
			}
			ticketErr = t.err
			r.log("%s: %v", t.via, t.err)

		case w := <-workers:
			workerPending = false
			if w.err != nil {
				workerErr = w.err
				r.log("worker path: %v", w.err)
				continue
			}
			r.log("worker path open")
			wconn = w.conn
		}
	}
}

// failure is the error when no path connected.
func (r *directRace) failure(ans *api.Direct, apiErr, workerErr, ticketErr error, attemptErrs []string) error {
	if r.mode != "only" {
		if workerErr != nil {
			return workerErr
		}
		if apiErr != nil {
			return apiErr
		}
		return errors.New("no path to the sandbox")
	}
	msg := "no direct path"
	switch {
	case apiErr != nil:
		msg = apiErr.Error()
	case ans != nil && ans.Provider != "node":
		msg = "it runs on Cloudflare, which has no direct path"
	case ans != nil && (ans.Direct == nil || ans.Ticket == nil):
		msg = "its node has no direct path (an older agent, --direct off, or failed: cloudex node info says which)"
	case ticketErr != nil:
		msg = ticketErr.Error()
	}
	if len(attemptErrs) > 0 {
		msg += " (" + strings.Join(attemptErrs, "; ") + ")"
	}
	return fmt.Errorf("CLOUDEX_DIRECT=only: %s", msg)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// localNetworkBlocked: macOS answers EHOSTUNREACH at once to an app that
// Local Network privacy blocks (M0 Q8).
func localNetworkBlocked(errs []string) bool {
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		if !strings.HasSuffix(e, "no route to host") {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- cloudex doctor

type doctorLine struct {
	ok  bool
	msg string
}

// directChecks tries a TLS handshake, with the node's key pinned and no
// ticket, to each direct candidate of each online node, and says which
// answer from this computer.
func directChecks(ctx context.Context, ns []api.Node, nets localNets, dial func(context.Context, string) (net.Conn, error)) []doctorLine {
	type result struct {
		c    candidate
		took time.Duration
		err  error
	}
	lines := make([][]doctorLine, len(ns))
	done := make(chan struct{})
	running := 0
	for i, n := range ns {
		d := n.Direct
		if n.State != "online" || d == nil {
			continue
		}
		name := clean(n.Name)
		if d.State != "listening" {
			why := clean(d.State)
			if d.Error != "" {
				why += ": " + clean(d.Error)
			}
			if d.State != "off" {
				lines[i] = []doctorLine{{false, fmt.Sprintf("node %s's direct port is %s; ssh to its sandboxes takes the Worker path", name, why)}}
			}
			continue
		}
		pub, ok := decodeKey(n.PublicKey)
		if !ok {
			lines[i] = []doctorLine{{false, fmt.Sprintf("can't check node %s's direct port: the deployment doesn't give its key (older than this CLI?)", name)}}
			continue
		}
		cands, skipped := orderCandidates(d.Addrs, d.Port, nets)
		running++
		go func() {
			defer func() { done <- struct{}{} }()
			results := make([]result, len(cands))
			var wg sync.WaitGroup
			for j, c := range cands {
				wg.Add(1)
				go func() {
					defer wg.Done()
					start := time.Now()
					conn, err := dialPinned(ctx, dial, c.addr, pub, time.Second)
					if conn != nil {
						conn.Close()
					}
					results[j] = result{c, time.Since(start), err}
				}()
			}
			wg.Wait()
			var good, bad []string
			unreachable := 0
			for _, r := range results {
				if r.err == nil {
					good = append(good, fmt.Sprintf("%s (%s, %.1f ms)", r.c.addr, r.c.class, ms(r.took)))
				} else {
					bad = append(bad, fmt.Sprintf("%s: %s", r.c.addr, why(r.err)))
					if errors.Is(r.err, syscall.EHOSTUNREACH) {
						unreachable++
					}
				}
			}
			bad = append(bad, skipped...)
			var out []doctorLine
			if len(good) > 0 {
				msg := fmt.Sprintf("node %s answers directly at %s", name, strings.Join(good, ", "))
				if len(bad) > 0 {
					msg += "; not at " + strings.Join(bad, "; ")
				}
				out = append(out, doctorLine{true, msg})
			} else {
				msg := fmt.Sprintf("node %s's direct port doesn't answer from here (%s): ssh to its sandboxes takes the Worker path", name, strings.Join(bad, "; "))
				if runtime.GOOS == "darwin" && unreachable > 0 && unreachable == len(results) {
					msg += ". If this computer is on that network, macOS may be blocking this app's Local Network access (System Settings, Privacy & Security, Local Network)"
				}
				out = append(out, doctorLine{false, msg})
			}
			lines[i] = out
		}()
	}
	for ; running > 0; running-- {
		<-done
	}
	var all []doctorLine
	for _, l := range lines {
		all = append(all, l...)
	}
	return all
}
