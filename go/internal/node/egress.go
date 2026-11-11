package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// What a machine reaches at its agent (v0.2.2 M6). On Cloudflare a sandbox's
// outbound traffic to http://cloudex.internal and to the AI providers' hosts
// is caught by the Worker's Egress entrypoint (apps/api/src/egress.ts). On a
// node, the agent has two listeners on its own address in each machine
// network (.2, which the machine knows as cloudex.internal):
//
//   - :80 is cloudex.internal. /save-on-term is answered here ({"saved":
//     false}: a node's machine keeps its disk when it stops, and docker stop
//     mustn't wait on the deployment); /activity is sent on at most once
//     every 30 s per machine; everything else goes to the deployment.
//   - :443 is the provider hosts that KEYS_SCRIPT points at the agent. TLS
//     ends here, with a certificate from the node CA (ca.go) for the name the
//     machine asked for, which must be one of the CA's hosts.
//
// Either way the request goes on, as it came (method, path, headers, body,
// streamed both ways), to the deployment's signed route POST|GET|...
// /v1/node/egress?target=<url>, which binds it to this node and the sandbox
// and runs Egress there; the real provider key is added in the Worker, so it
// never reaches the node or the machine.
//
// Who is calling: a connection counts only when it arrived on the agent's
// address of that machine's network AND its source is inside that
// network's /28 (CallerNetwork, M0 Q4). The listeners bind per address,
// never 0.0.0.0, so the agent's control address and its addresses on other
// machines' networks have nothing listening; a machine with NET_ADMIN that
// routes to another network's agent address arrives from its own /28 and is
// refused. Nothing but the deployment's route is ever dialled: the agent is
// no proxy to anywhere else.

// EgressBodyMax is the largest request body the agent sends on: the
// deployment's edge refuses bodies over 100 MB with a 413 before the Worker
// sees them (M0 Q1). Saves and backups send 64 MiB parts.
const EgressBodyMax = 100_000_000

// EgressInflightMax is how many requests one machine may have in flight.
const EgressInflightMax = 64

// ActivityEvery is how often a machine's /activity reaches the deployment.
const ActivityEvery = 30 * time.Second

// EgressRoute is the deployment's route for a node's guest traffic.
const EgressRoute = "/v1/node/egress"

// Egress runs the agent's listeners on its machine networks.
type Egress struct {
	API  string
	Node string
	Key  ed25519.PrivateKey
	HTTP *http.Client
	Log     *log.Logger
	Debug   bool
	Version string
	// CA is the node CA now; nil until the deployment has named its provider hosts.
	CA func() *CA
	// Ports replaces 80 and 443 (tests, which can't bind them; 0 is any).
	Ports []int

	mu       sync.Mutex
	nets     map[string]*egressNet // by sandbox
	activity map[string]time.Time
	refused  atomic.Int64
}

type egressNet struct {
	sandbox  string
	net      MachineNet
	servers  []*http.Server
	addrs    []string // where the two listeners are
	inflight atomic.Int32
}

func (e *Egress) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

// Listening is the sandboxes whose networks have listeners now.
func (e *Egress) Listening() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.nets))
	for id := range e.nets {
		out = append(out, id)
	}
	return out
}

// Sync opens listeners on the networks it hasn't yet, and closes the ones of
// networks that are gone. A network whose agent address isn't up yet (the
// agent isn't attached) is tried again at the next sync.
func (e *Egress) Sync(nets map[string]MachineNet) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nets == nil {
		e.nets = map[string]*egressNet{}
	}
	for id, en := range e.nets {
		if n, ok := nets[id]; !ok || n != en.net {
			en.close()
			delete(e.nets, id)
		}
	}
	for id, n := range nets {
		if e.nets[id] != nil {
			continue
		}
		en, err := e.listen(id, n)
		if err != nil {
			if e.Debug {
				e.logf("%s: no listeners on %s yet: %v", id, n.Agent, err)
			}
			continue
		}
		e.nets[id] = en
	}
}

// Close closes every listener.
func (e *Egress) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, en := range e.nets {
		en.close()
		delete(e.nets, id)
	}
}

func (en *egressNet) close() {
	for _, s := range en.servers {
		s.Close()
	}
}

// machineListener accepts only connections from inside its network.
type machineListener struct {
	net.Listener
	n MachineNet
	e *Egress
}

func (l *machineListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		la, _ := netip.ParseAddrPort(c.LocalAddr().String())
		pa, _ := netip.ParseAddrPort(c.RemoteAddr().String())
		if _, ok := CallerNetwork(la.Addr(), pa.Addr(), []MachineNet{l.n}); ok {
			return c, nil
		}
		c.Close()
		if n := l.e.refused.Add(1); n <= 20 || n%1000 == 0 {
			l.e.logf("refused a connection to %s from %s: not from that machine's network (%d so far)", la, pa.Addr(), n)
		}
	}
}

func (e *Egress) listen(sandbox string, n MachineNet) (*egressNet, error) {
	en := &egressNet{sandbox: sandbox, net: n}
	var lns []net.Listener
	ports := []int{80, 443}
	if len(e.Ports) == 2 {
		ports = e.Ports
	}
	for _, port := range ports {
		ln, err := net.Listen("tcp4", netip.AddrPortFrom(n.Agent, uint16(port)).String())
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, err
		}
		lns = append(lns, &machineListener{Listener: ln, n: n, e: e})
		en.addrs = append(en.addrs, ln.Addr().String())
	}
	plain := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.serveInternal(en, w, r) }),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	secure := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.serveProvider(en, w, r) }),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSConfig:         e.tlsConfig(),
	}
	en.servers = []*http.Server{plain, secure}
	go plain.Serve(lns[0])
	go secure.ServeTLS(lns[1], "", "")
	return en, nil
}

// tlsConfig ends a machine's TLS with a leaf from the node CA for the name it
// asked for (SNI), which must be one of the CA's hosts: any other name, or
// none, fails the handshake before a request is read.
func (e *Egress) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			ca := e.ca()
			if ca == nil {
				return nil, errors.New("no node CA yet")
			}
			return ca.Leaf(h.ServerName)
		},
	}
}

func (e *Egress) version() string {
	if e.Version == "" {
		return "dev"
	}
	return e.Version
}

func (e *Egress) ca() *CA {
	if e.CA == nil {
		return nil
	}
	return e.CA()
}

// refuse answers in the shape Egress uses for its own refusals (error.message
// is what Anthropic's and OpenAI's clients show), with error at the top too
// for the cloudex command.
func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "cloudex_error", "message": msg}, "message": msg})
}

// cloudex.internal, on :80.
func (e *Egress) serveInternal(en *egressNet, w http.ResponseWriter, r *http.Request) {
	if !e.common(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/save-on-term" && r.Method == http.MethodPost:
		io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"saved":false}`+"\n")
		return
	case r.URL.Path == "/activity" && r.Method == http.MethodPost:
		e.mu.Lock()
		if e.activity == nil {
			e.activity = map[string]time.Time{}
		}
		last := e.activity[en.sandbox]
		due := time.Since(last) >= ActivityEvery
		if due {
			e.activity[en.sandbox] = time.Now()
		}
		e.mu.Unlock()
		if !due {
			io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
			io.WriteString(w, "ok\n")
			return
		}
	}
	e.forward(en, w, r, "http://cloudex.internal"+requestPath(r))
}

// A provider host, on :443.
func (e *Egress) serveProvider(en *egressNet, w http.ResponseWriter, r *http.Request) {
	if !e.common(w, r) {
		return
	}
	sni := ""
	if r.TLS != nil {
		sni = strings.ToLower(r.TLS.ServerName)
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ca := e.ca()
	if ca == nil || sni == "" || !ca.Allows(sni) || host != sni {
		refuse(w, http.StatusMisdirectedRequest, "the node's agent passes on HTTPS calls only to the AI providers the deployment holds keys for, by their own names")
		return
	}
	e.forward(en, w, r, "https://"+sni+requestPath(r))
}

// What both listeners refuse: CONNECT (nothing is proxied), WebSockets
// (Egress passes provider calls over HTTPS only, and Codex falls back on a
// 426), and more in flight than a machine may have.
func (e *Egress) common(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodConnect {
		refuse(w, http.StatusMethodNotAllowed, "the node's agent isn't a proxy")
		return false
	}
	if r.Header.Get("Upgrade") != "" {
		w.WriteHeader(http.StatusUpgradeRequired)
		io.WriteString(w, "Cloudex passes provider calls over HTTPS only\n")
		return false
	}
	return true
}

// requestPath is the path and query of the request, whatever form its
// request line had: an absolute URL's host is never used.
func requestPath(r *http.Request) string {
	p := r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	return p
}

// hopHeaders aren't passed on in either direction.
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Host": true,
}

// HeaderGuestUA carries the machine's own User-Agent to the Worker, which
// puts it back (nodeegress.ts). The request to the deployment goes with the
// agent's: the edge's Browser Integrity Check refuses some clients' default
// ones (Python's urllib, which the cloudex command inside uses: 1010).
const HeaderGuestUA = "x-cx-guest-ua"

func copyRequestHeaders(dst, src http.Header, agentUA string) {
	for k, v := range src {
		if hopHeaders[k] || strings.HasPrefix(strings.ToLower(k), "x-cx-") || k == "User-Agent" {
			continue
		}
		dst[k] = append([]string(nil), v...)
	}
	if ua := src.Get("User-Agent"); ua != "" {
		dst.Set(HeaderGuestUA, ua)
	}
	dst.Set("User-Agent", agentUA)
}

// errBodyTooBig is a body that grew past EgressBodyMax while it was sent.
var errBodyTooBig = errors.New("request body over the limit")

type cappedBody struct {
	r io.ReadCloser
	n int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > EgressBodyMax {
		return n, errBodyTooBig
	}
	return n, err
}

func (c *cappedBody) Close() error { return c.r.Close() }

func bodyTooBig(w http.ResponseWriter) {
	refuse(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a request from a sandbox on a node can carry at most %d bytes (100 MB): the deployment's edge refuses larger bodies. Send it in parts", EgressBodyMax))
}

// EgressURL is the deployment's route for target, and the path with query
// that is signed.
func EgressURL(api, target string) (full, signed string, err error) {
	full, signed, err = Endpoint(api, EgressRoute)
	if err != nil {
		return "", "", err
	}
	q := "target=" + url.QueryEscape(target)
	return full + "?" + q, signed + "?" + q, nil
}

func (e *Egress) forward(en *egressNet, w http.ResponseWriter, r *http.Request, target string) {
	if n := en.inflight.Add(1); n > EgressInflightMax {
		en.inflight.Add(-1)
		refuse(w, http.StatusTooManyRequests, fmt.Sprintf("a sandbox on a node may have at most %d requests to cloudex.internal and the providers in flight", EgressInflightMax))
		return
	}
	defer en.inflight.Add(-1)
	if r.ContentLength > EgressBodyMax {
		bodyTooBig(w)
		return
	}
	full, signed, err := EgressURL(e.API, target)
	if err != nil {
		refuse(w, http.StatusBadGateway, err.Error())
		return
	}
	var body io.Reader
	var capped *cappedBody
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		capped = &cappedBody{r: r.Body}
		body = capped
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, full, body)
	if err != nil {
		refuse(w, http.StatusBadRequest, "a request the agent can't pass on: "+err.Error())
		return
	}
	req.ContentLength = r.ContentLength
	if body == nil {
		req.ContentLength = 0
	}
	copyRequestHeaders(req.Header, r.Header, "cloudex-node/"+e.version())
	Signer{Key: e.Key, Node: e.Node}.Sign(req.Header, r.Method, signed, en.sandbox)
	hc := e.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		if capped != nil && capped.n > EgressBodyMax {
			bodyTooBig(w)
			return
		}
		if r.Context().Err() != nil {
			return
		}
		refuse(w, http.StatusBadGateway, "the node's agent couldn't reach the deployment: "+err.Error())
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		if hopHeaders[k] {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	flush, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flush != nil {
				flush.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// MachineNets lists this node's machine networks, by sandbox, from Docker:
// the networks labelled as a machine's, which the agent joined at .2.
func (r *Runtime) MachineNets(ctx context.Context) (map[string]MachineNet, error) {
	ns, err := r.Docker.Networks(ctx, r.labels(LabelRole+"=machine")...)
	if err != nil {
		return nil, err
	}
	out := map[string]MachineNet{}
	for _, nw := range ns {
		id := nw.Labels[LabelSandbox]
		if !sandboxID.MatchString(id) || len(nw.IPAM.Config) == 0 {
			continue
		}
		sub, err := netip.ParsePrefix(nw.IPAM.Config[0].Subnet)
		if err != nil {
			continue
		}
		n, err := NetFor(id, sub)
		if err != nil {
			continue
		}
		out[id] = n
	}
	return out, nil
}

// egressLoop keeps the listeners in step with the machine networks: at
// once, when asked (a machine was made, started or removed), and every 30 s.
func (a *Agent) egressLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	defer a.egress.Close()
	for {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		nets, err := a.Runtime.MachineNets(lctx)
		cancel()
		if err == nil {
			a.egress.Sync(nets)
		} else if ctx.Err() == nil {
			a.logf("listing machine networks for the listeners: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.egressNow:
		}
	}
}

func (a *Agent) kickEgress() {
	if a.egressNow == nil {
		return
	}
	select {
	case a.egressNow <- struct{}{}:
	default:
	}
}
