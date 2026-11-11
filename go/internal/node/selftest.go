package node

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The isolation self-test (v0.2.2 M4). The agent can't read the host's
// nftables (it has no NET_ADMIN there), so it tests what they're for, from
// inside machines built exactly as real ones are (MachineSpec):
//
//   - probe A, a plain machine on a network of its own, tries the host on its
//     own gateway and on every address the installer found (the host's own,
//     its LAN gateway and DNS, a tailnet's), the agent's other addresses,
//     another machine's network, 169.254.169.254 and 100.100.100.100, all of
//     which must not answer; and the internet, DNS and its own agent, which
//     must. It also reports what root inside can do (capabilities, /proc
//     and /sys, devices, raw sockets, the Docker socket).
//   - probe N, on the same network with NET_ADMIN (which root has under
//     Sysbox), routes through the agent to the other network, the agent's
//     other addresses and metadata, forges a source address from the other
//     network, and sends everything through the agent as its default route.
//   - B is the other machine, on a second network.
//
// While it runs, the agent listens on all its addresses and applies the rule
// that will tell machines apart (CallerNetwork): a connection is from a
// machine only if it arrived on that machine's network's agent address and
// came from inside that network's /28 (M0 Q4). Whatever probe N reaches must
// be refused by it, and its forged source must not finish a handshake.
//
// The result is the node's isolation.firewall: pass only when every check
// passes. Without pass, the node refuses new machines.

//go:embed probe.py
var probeScript string

// SelfTestEvery is how often the self-test runs again.
const SelfTestEvery = time.Hour

// Check is one probe and what it should have found.
type Check struct {
	Name string `json:"name"`
	Want string `json:"want"`
	Got  string `json:"got"`
	OK   bool   `json:"ok"`
}

// SelfTestReport is a self-test's whole result.
type SelfTestReport struct {
	At      int64           `json:"at"`
	Pass    bool            `json:"pass"`
	Runtime string          `json:"runtime"`
	TookMs  int64           `json:"tookMs"`
	Checks  []Check         `json:"checks"`
	Inside  json.RawMessage `json:"inside,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Failed names the checks that failed.
func (r *SelfTestReport) Failed() []string {
	var out []string
	if r.Error != "" {
		out = append(out, "error: "+r.Error)
	}
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, fmt.Sprintf("%s: %s", c.Name, c.Got))
		}
	}
	return out
}

// CallerNetwork says which machine network a connection to the agent comes
// from: the one whose agent address took it, and only if the peer's address
// is inside that network (and isn't the agent's own or the host's). nets are
// the agent's machine networks.
func CallerNetwork(local, peer netip.Addr, nets []MachineNet) (MachineNet, bool) {
	local, peer = local.Unmap(), peer.Unmap()
	for _, n := range nets {
		if n.Agent == local {
			if n.Subnet.Contains(peer) && peer != n.Agent && peer != n.Gateway && peer != n.Subnet.Addr() && peer != lastAddr(n.Subnet) {
				return n, true
			}
			return MachineNet{}, false
		}
	}
	return MachineNet{}, false
}

func lastAddr(p netip.Prefix) netip.Addr {
	return addN(p.Addr(), uint32(1)<<(32-p.Bits())-1)
}

func probeID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return "cx-t" + hex.EncodeToString(b)
}

// identityListener is the agent's listener during a self-test.
type identityListener struct {
	ln   net.Listener
	port int
	nets []MachineNet
	mu   sync.Mutex
	seen []string // took>peer=verdict
}

func (l *identityListener) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			la, _ := netip.ParseAddrPort(c.LocalAddr().String())
			pa, _ := netip.ParseAddrPort(c.RemoteAddr().String())
			n, ok := CallerNetwork(la.Addr(), pa.Addr(), l.nets)
			verdict := "refused"
			if ok {
				verdict = "machine " + n.Name
			}
			l.mu.Lock()
			l.seen = append(l.seen, fmt.Sprintf("%s<%s=%s", la.Addr(), pa.Addr(), verdict))
			l.mu.Unlock()
			c.SetWriteDeadline(time.Now().Add(time.Second))
			c.Write([]byte(verdict + "\n"))
		}()
	}
}

// selfAddrs are the agent's addresses outside the given networks, with the
// gateway (the host) of each.
func (r *Runtime) selfAddrs(ctx context.Context, skip map[string]bool) (addrs, gateways []string, err error) {
	var raw struct {
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
				Gateway   string `json:"Gateway"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := r.Docker.do(ctx, "GET", "/containers/"+url.PathEscape(r.Self)+"/json", nil, &raw); err != nil {
		return nil, nil, err
	}
	for name, n := range raw.NetworkSettings.Networks {
		if skip[name] || strings.HasPrefix(name, "cloudex-cx-") {
			continue
		}
		if n.IPAddress != "" {
			addrs = append(addrs, n.IPAddress)
		}
		if n.Gateway != "" {
			gateways = append(gateways, n.Gateway)
		}
	}
	return addrs, gateways, nil
}

// SelfTest runs the isolation self-test once.
func (r *Runtime) SelfTest(ctx context.Context) *SelfTestReport {
	t0 := time.Now()
	rep := &SelfTestReport{At: t0.UnixMilli(), Runtime: "docker"}
	if r.Sysbox(ctx) {
		rep.Runtime = "sysbox"
	}
	err := r.selfTest(ctx, rep)
	if err != nil {
		rep.Error = err.Error()
	}
	rep.TookMs = time.Since(t0).Milliseconds()
	rep.Pass = err == nil && len(rep.Checks) > 0
	for _, c := range rep.Checks {
		rep.Pass = rep.Pass && c.OK
	}
	return rep
}

// Record keeps a self-test's result as the node's isolation.
func (r *Runtime) Record(rep *SelfTestReport) {
	r.factsMu.Lock()
	r.report = rep
	if rep.Error != "" {
		// It couldn't run (the image is still being pulled, Docker was busy):
		// no verdict, so the last one stands (unchecked at first), with the
		// reason, and it runs again within a minute.
		r.iso.Failed = []string{truncate("error: "+rep.Error, 200)}
		r.factsMu.Unlock()
		r.changed()
		return
	}
	r.iso.CheckedAt = rep.At
	r.iso.Failed = nil
	if rep.Pass {
		r.iso.Firewall = "pass"
	} else {
		r.iso.Firewall = "fail"
		r.iso.Failed = rep.Failed()
		if len(r.iso.Failed) > 16 {
			r.iso.Failed = append(r.iso.Failed[:15], fmt.Sprintf("and %d more", len(r.iso.Failed)-15))
		}
		for i, f := range r.iso.Failed {
			if len(f) > 200 {
				r.iso.Failed[i] = f[:200]
			}
		}
	}
	r.factsMu.Unlock()
	r.changed()
}

// LastReport is the most recent self-test's result.
func (r *Runtime) LastReport() *SelfTestReport {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	return r.report
}

type step map[string]any

func (r *Runtime) selfTest(ctx context.Context, rep *SelfTestReport) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := r.EnsureImage(ctx); err != nil {
		return fmt.Errorf("the machine image: %w", err)
	}
	add := func(name, want, got string, ok bool) {
		rep.Checks = append(rep.Checks, Check{Name: name, Want: want, Got: got, OK: ok})
	}
	// The agent's own namespace: forwarding off, so no machine routes through it.
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
		v := strings.TrimSpace(string(b))
		add("agent ip_forward", "0", v, v == "0")
	} else {
		add("agent ip_forward", "0", "unreadable: "+err.Error(), false)
	}

	r.mu.Lock()
	idA, idB := probeID(), probeID()
	netA, errA := r.newNetwork(ctx, idA, "probe")
	var netB MachineNet
	errB := errA
	if errA == nil {
		netB, errB = r.newNetwork(ctx, idB, "probe")
	}
	r.mu.Unlock()
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, id := range []string{idA, idB} {
			cs, _ := r.Docker.Containers(cctx, r.labels(LabelSandbox+"="+id)...)
			for _, c := range cs {
				r.Docker.ContainerRemove(cctx, c.ID)
			}
		}
		for _, n := range []MachineNet{netA, netB} {
			if n.Name != "" {
				r.removeNetwork(cctx, n)
			}
		}
	}
	defer cleanup()
	if errA != nil {
		return errA
	}
	if errB != nil {
		return errB
	}

	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	il := &identityListener{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, nets: []MachineNet{netA, netB}}
	go il.serve()
	port := strconv.Itoa(il.port)

	h := MachineHost{NodeID: r.NodeID, Image: r.Image(), Sysbox: r.Sysbox(ctx), Role: "probe"}
	mk := func(id string, n MachineNet, netAdmin bool, addr netip.Addr) (string, error) {
		h.Network = n
		var b *CreateBody
		var err error
		if netAdmin {
			b, err = ProbeSpec(id, h)
		} else {
			b, err = MachineSpec(MachineRequest{Sandbox: id, CPUs: 0.25, MemoryMB: MinMemoryMB}, h)
		}
		if err != nil {
			return "", err
		}
		b.Networking.EndpointsConfig[n.Name] = endpoint(addr)
		name := n.Name
		if netAdmin {
			name += "-n"
		}
		cid, err := r.Docker.ContainerCreate(ctx, name, b)
		if err != nil {
			return "", err
		}
		return cid, r.Docker.ContainerStart(ctx, cid)
	}
	pa, err := mk(idA, netA, false, netA.Machine)
	if err != nil {
		return fmt.Errorf("probe A: %w", err)
	}
	pn, err := mk(idA, netA, true, netA.Probe)
	if err != nil {
		return fmt.Errorf("probe N: %w", err)
	}
	pb, err := mk(idB, netB, false, netB.Machine)
	if err != nil {
		return fmt.Errorf("machine B: %w", err)
	}
	for _, id := range []string{pa, pn, pb} {
		if err := r.waitReady(ctx, id, 90*time.Second); err != nil {
			return fmt.Errorf("a probe machine didn't start: %w", err)
		}
	}

	selfAddrs, selfGWs, err := r.selfAddrs(ctx, map[string]bool{netA.Name: true, netB.Name: true})
	if err != nil {
		return err
	}

	// Probe A: a plain machine.
	type want struct{ name, kind, expect string }
	var plain []step
	wants := map[string]want{}
	t := func(name, host string, p any, expect string) {
		id := fmt.Sprintf("s%d", len(wants))
		plain = append(plain, step{"id": id, "kind": "tcp", "host": host, "port": p})
		wants[id] = want{name, "tcp", expect}
	}
	u := func(name, host, expect string) {
		id := fmt.Sprintf("s%d", len(wants))
		plain = append(plain, step{"id": id, "kind": "udp53", "host": host})
		wants[id] = want{name, "udp53", expect}
	}
	t("internet 1.1.1.1:443", "1.1.1.1", 443, "open")
	id := fmt.Sprintf("s%d", len(wants))
	plain = append(plain, step{"id": id, "kind": "dns", "name": "example.com"})
	wants[id] = want{"DNS example.com", "dns", "resolved"}
	u("DNS 1.1.1.1 (udp)", "1.1.1.1", "answered")
	t("its own agent "+netA.Agent.String(), netA.Agent.String(), port, "own")
	for _, p := range []int{22, 80, 443} {
		t(fmt.Sprintf("the host on the machine's gateway %s:%d", netA.Gateway, p), netA.Gateway.String(), p, "closed")
	}
	for _, g := range selfGWs {
		t("the host on the agent's network "+g+":22", g, 22, "closed")
	}
	for _, a := range selfAddrs {
		t("the agent's control address "+a, a, port, "closed")
	}
	t("the agent on another machine's network "+netB.Agent.String(), netB.Agent.String(), port, "closed")
	t("another machine "+netB.Machine.String()+":22", netB.Machine.String(), 22, "closed")
	t("metadata 169.254.169.254:80", "169.254.169.254", 80, "closed")
	t("tailnet 100.100.100.100:53", "100.100.100.100", 53, "closed")
	u("tailnet DNS 100.100.100.100 (udp)", "100.100.100.100", "timeout")
	for _, hp := range r.HostProbes {
		if host, ok := strings.CutPrefix(hp, "udp53="); ok {
			u("the host's network "+host+" (DNS, udp)", host, "timeout")
			continue
		}
		host, p, err := net.SplitHostPort(hp)
		if err == nil {
			t("the host's network "+hp, host, p, "closed")
		}
	}
	insideID := "inside"
	plainPhases := [][]step{plain, {{"id": insideID, "kind": "inside"}}}
	outA, err := r.probe(ctx, pa, plainPhases)
	if err != nil {
		return fmt.Errorf("running probe A: %w", err)
	}
	for id, w := range wants {
		got := outA[id]
		add("plain: "+w.name, w.expect, got, judge(w.expect, got, netA))
	}
	r.judgeInside(rep, outA[insideID])

	// Probe N: NET_ADMIN, routing through the agent.
	a := netA.Agent.String()
	nIn := func(args ...string) step {
		return step{"id": "ip" + strings.Join(args, "_"), "kind": "ip", "args": args}
	}
	nt := func(id, host string, p any, src string) step {
		s := step{"id": id, "kind": "tcp", "host": host, "port": p}
		if src != "" {
			s["src"] = src
		}
		return s
	}
	forged := addN(netB.Subnet.Addr(), 5).String()
	phases := [][]step{
		{nIn("route", "add", netB.Subnet.String(), "via", a)},
		{nt("routedMachine", netB.Machine.String(), 22, ""), nt("routedAgent", netB.Agent.String(), port, "")},
		{nIn("addr", "add", forged+"/32", "dev", "eth0")},
		{nt("forged", netB.Agent.String(), port, forged)},
		{nIn("addr", "del", forged+"/32", "dev", "eth0"), nIn("route", "add", "169.254.169.254/32", "via", a)},
		{nt("routedMetadata", "169.254.169.254", 80, "")},
	}
	for i, s := range selfAddrs {
		phases = append(phases, []step{nIn("route", "add", s+"/32", "via", a)}, []step{nt(fmt.Sprintf("routedControl%d", i), s, port, "")})
	}
	phases = append(phases,
		[]step{nIn("route", "replace", "default", "via", a)},
		[]step{nt("throughAgent", "1.1.1.1", 443, "")})
	outN, err := r.probe(ctx, pn, phases)
	if err != nil {
		return fmt.Errorf("running probe N: %w", err)
	}
	if got := outN["iproute_add_"+netB.Subnet.String()+"_via_"+a]; got != "ok" {
		add("NET_ADMIN: change its routes", "ok", got, false)
	}
	add("NET_ADMIN: another machine, routed through the agent", "closed", outN["routedMachine"], judge("closed", outN["routedMachine"], netA))
	add("NET_ADMIN: the agent's address on another network, routed to it", "closed or refused as a machine", outN["routedAgent"], judge("refused", outN["routedAgent"], netA))
	add("NET_ADMIN: a source address forged from another network", "no handshake", outN["forged"], judge("closed", outN["forged"], netA))
	add("NET_ADMIN: metadata, routed through the agent", "closed", outN["routedMetadata"], judge("closed", outN["routedMetadata"], netA))
	for i, s := range selfAddrs {
		k := fmt.Sprintf("routedControl%d", i)
		add("NET_ADMIN: the agent's control address "+s+", routed to it", "closed or refused as a machine", outN[k], judge("refused", outN[k], netA))
	}
	add("NET_ADMIN: the internet with the agent as its gateway", "closed (the agent doesn't forward)", outN["throughAgent"], judge("closed", outN["throughAgent"], netA))

	// What the agent's listener saw: nothing from probe N, or forged, may
	// have passed as a machine of another network.
	il.mu.Lock()
	seen := append([]string(nil), il.seen...)
	il.mu.Unlock()
	bad := []string{}
	for _, s := range seen {
		if strings.Contains(s, "=machine") && !strings.HasPrefix(s, netA.Agent.String()+"<"+netA.Machine.String()+"=") {
			bad = append(bad, s)
		}
	}
	add("the agent tells machines apart (network and source)", "only probe A, on its own network", strings.Join(seen, ", "), len(bad) == 0)
	return nil
}

// judge reads a probe result against what it should be: open, own (its own
// agent, which names its network), resolved, answered, timeout, closed (no
// answer of any kind) or refused (closed, or reached the agent and refused
// as a machine).
func judge(expect, got string, own MachineNet) bool {
	switch expect {
	case "open":
		return strings.HasPrefix(got, "open")
	case "own":
		return got == "open:machine "+own.Name
	case "resolved", "answered":
		return strings.HasPrefix(got, expect)
	case "timeout":
		return got == "timeout"
	case "closed":
		return got == "timeout" || got == "unreachable"
	case "refused":
		return got == "timeout" || got == "unreachable" || got == "open:refused"
	}
	return false
}

// DockerCaps is the capability set root has in a plain-Docker machine:
// Docker's default set without NET_RAW, MKNOD and SETFCAP.
const DockerCaps = "00000000200405fb"

var standardDevices = map[string]bool{"core": true, "fd": true, "full": true, "mqueue": true, "null": true, "ptmx": true, "pts": true,
	"random": true, "shm": true, "stderr": true, "stdin": true, "stdout": true, "tty": true, "urandom": true, "zero": true}

func (r *Runtime) judgeInside(rep *SelfTestReport, raw string) {
	var in struct {
		CapEff        string   `json:"capEff"`
		UIDMap        string   `json:"uidMap"`
		DockerSocket  bool     `json:"dockerSocket"`
		ProcSysWrite  string   `json:"procSysWrite"`
		SysrqWrite    string   `json:"sysrqWrite"`
		SysWrite      string   `json:"sysWrite"`
		KcoreRead     string   `json:"kcoreRead"`
		RawSocket     string   `json:"rawSocket"`
		PacketSocket  string   `json:"packetSocket"`
		Mknod         string   `json:"mknod"`
		Mount         string   `json:"mount"`
		RouteChange   string   `json:"routeChange"`
		Devices       []string `json:"devices"`
		IPv6          bool     `json:"ipv6"`
		IPv6Addresses []string `json:"ipv6Addresses"`
	}
	add := func(name, want, got string, ok bool) {
		rep.Checks = append(rep.Checks, Check{Name: name, Want: want, Got: got, OK: ok})
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		add("inside: what root can do", "a report", raw, false)
		return
	}
	rep.Inside = json.RawMessage(raw)
	add("inside: a Docker or containerd socket mounted from the host", "none", fmt.Sprint(in.DockerSocket), !in.DockerSocket)
	add("inside: IPv6", "off (lo keeps ::1)", fmt.Sprintf("eth0 %v %v", in.IPv6, in.IPv6Addresses), !in.IPv6 && len(in.IPv6Addresses) == 0)
	odd := []string{}
	for _, d := range in.Devices {
		if !standardDevices[d] && d != "console" {
			odd = append(odd, d)
		}
	}
	if rep.Runtime == "sysbox" {
		// Root is an unprivileged user on the host; inside its own namespaces
		// it has capabilities and may mount, which Docker inside needs.
		add("inside: root is not root on the host", "uid_map not 0 0", in.UIDMap, !strings.HasPrefix(in.UIDMap, "0 0 "))
		add("inside: /proc/sysrq-trigger", "denied", in.SysrqWrite, strings.HasPrefix(in.SysrqWrite, "denied"))
		add("inside: /proc/kcore", "denied", in.KcoreRead, strings.HasPrefix(in.KcoreRead, "denied"))
		return
	}
	add("inside: root's capabilities", DockerCaps, in.CapEff, in.CapEff == DockerCaps)
	for _, c := range []struct{ name, got string }{
		{"/proc/sys", in.ProcSysWrite}, {"/proc/sysrq-trigger", in.SysrqWrite}, {"/sys", in.SysWrite}, {"/proc/kcore", in.KcoreRead},
		{"raw sockets", in.RawSocket}, {"packet sockets", in.PacketSocket}, {"mknod", in.Mknod}, {"mount", in.Mount}, {"route changes", in.RouteChange},
	} {
		add("inside: "+c.name, "denied", c.got, strings.HasPrefix(c.got, "denied"))
	}
	add("inside: devices", "Docker's standard set", strings.Join(in.Devices, " "), len(odd) == 0)
}

// probe runs the probe script in a probe machine, as root.
func (r *Runtime) probe(ctx context.Context, container string, phases [][]step) (map[string]string, error) {
	arg, _ := json.Marshal(phases)
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, code, err := r.Docker.Run(cctx, container, "0", []string{"python3", "-c", probeScript, string(arg)})
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var last string
	for sc.Scan() {
		last = sc.Text()
	}
	if err := json.Unmarshal([]byte(last), &res); err != nil {
		return nil, fmt.Errorf("the probe exited %d: %.300s", code, out)
	}
	return res, nil
}

// MachineCheck is the installer's last step: it makes a machine the way the
// deployment would (a network of its own, MachineSpec), starts it, waits for
// its entrypoint, checks from inside it that the internet answers and the
// host and metadata don't, and removes it. It says how long the start took.
func (r *Runtime) MachineCheck(ctx context.Context) (string, error) {
	if err := r.ready(); err != nil {
		return "", err
	}
	if err := r.isolated(); err != nil {
		return "", err
	}
	if err := r.EnsureImage(ctx); err != nil {
		return "", err
	}
	id := probeID()
	r.mu.Lock()
	n, err := r.newNetwork(ctx, id, "check")
	r.mu.Unlock()
	if err != nil {
		return "", err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cs, _ := r.Docker.Containers(cctx, r.labels(LabelSandbox+"="+id)...)
		for _, c := range cs {
			r.Docker.ContainerRemove(cctx, c.ID)
		}
		r.removeNetwork(cctx, n)
	}()
	info, _ := r.dockerInfo(ctx)
	h := MachineHost{NodeID: r.NodeID, Image: r.Image(), Sysbox: r.Sysbox(ctx), Network: n, Role: "check"}
	if info != nil {
		h.HostCPU, h.HostMB = info.NCPU, int(info.MemTotal>>20)
	}
	body, err := MachineSpec(MachineRequest{Sandbox: id}, h)
	if err != nil {
		return "", err
	}
	t0 := time.Now()
	cid, err := r.Docker.ContainerCreate(ctx, n.Name, body)
	if err != nil {
		return "", err
	}
	if err := r.Docker.ContainerStart(ctx, cid); err != nil {
		return "", err
	}
	if err := r.waitReady(ctx, cid, 90*time.Second); err != nil {
		return "", err
	}
	ready := time.Since(t0)
	out, err := r.probe(ctx, cid, [][]step{{
		{"id": "net", "kind": "tcp", "host": "1.1.1.1", "port": 443},
		{"id": "dns", "kind": "dns", "name": "example.com"},
		{"id": "host", "kind": "tcp", "host": n.Gateway.String(), "port": 22},
		{"id": "meta", "kind": "tcp", "host": "169.254.169.254", "port": 80},
	}})
	if err != nil {
		return "", err
	}
	if !judge("open", out["net"], n) || !judge("resolved", out["dns"], n) {
		return "", fmt.Errorf("from inside the machine: internet %s, DNS %s", out["net"], out["dns"])
	}
	if r.Unisolated {
		return fmt.Sprintf("a machine started and was ready in %v; from inside it the internet and DNS answer, and, with no firewall (--unisolated), the host %s and metadata %s", ready.Round(10*time.Millisecond), out["host"], out["meta"]), nil
	}
	if !judge("closed", out["host"], n) || !judge("closed", out["meta"], n) {
		return "", fmt.Errorf("from inside the machine: the host %s, metadata %s", out["host"], out["meta"])
	}
	return fmt.Sprintf("a machine started and was ready in %v; from inside it the internet and DNS answer, and the host and metadata don't", ready.Round(10*time.Millisecond)), nil
}

var errNoRuntime = errors.New("this agent runs no machines: it has no machine pool (cloudex-node run --pool), or no Docker")

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
