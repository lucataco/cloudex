package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runtime runs machines under Docker (v0.2.2 M4): one container and one
// network for each sandbox, built by MachineSpec and NetFor, on the node's
// machine pool. It makes, starts, stops and removes them when the deployment
// asks (ops create, start, stop and delete), and reports what Docker says
// happened to them (state messages). Machines carry no restart policy: after
// the host reboots they stay stopped, with their disks, until they're
// started again.
type Runtime struct {
	Docker *Docker
	NodeID string
	Pool   netip.Prefix
	// Self is the agent's own container, which joins each machine network at
	// .2; empty when the agent doesn't run in one (it then can't run machines).
	Self string
	// Allowed are the image repositories machines may run; ImageOverride, when
	// set, is used instead of the deployment's image (it must be allowed too).
	Allowed       []string
	ImageOverride string
	// DockerRoot is where Docker's data root is mounted, read-only, for its
	// free space; empty when it isn't.
	DockerRoot string
	Unisolated bool
	// HostProbes are the host's own addresses and its LAN's (ip:port), which the
	// installer found and the self-test must find closed.
	HostProbes []string
	Log        *log.Logger

	// Called when the facts change, and when a machine starts or stops.
	OnFacts func()
	OnState func(Machine)

	mu      sync.Mutex // lifecycle and allocation
	factsMu sync.Mutex
	image   string // the image machines run
	img     ImageFact
	pulling bool
	iso     Isolation
	report  *SelfTestReport
	dinfo   *DockerInfo
	infoAt  time.Time
}

// MinFreeGB is the free space on Docker's disk below which new machines are refused.
const MinFreeGB = 10.0

// DefaultImageRepo is the only image repository allowed unless the installer widens the list.
const DefaultImageRepo = "ghcr.io/lucataco/cloudex-base"

var imageRef = regexp.MustCompile(`^([a-z0-9][a-z0-9.-]*(:[0-9]{1,5})?/)?[a-z0-9][a-z0-9._/-]{0,200}(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

// ImageRepo is an image reference without its tag and digest.
func ImageRepo(ref string) string {
	repo, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

// ImageAllowed says whether machines may run ref.
func ImageAllowed(ref string, allowed []string) error {
	if !imageRef.MatchString(ref) {
		return fmt.Errorf("%q isn't an image reference", ref)
	}
	if len(allowed) == 0 {
		allowed = []string{DefaultImageRepo}
	}
	repo := ImageRepo(ref)
	for _, a := range allowed {
		if repo == a {
			return nil
		}
	}
	return fmt.Errorf("machines on this node may run only %s, not %s (the installer's --allow-image widens the list)", strings.Join(allowed, ", "), repo)
}

func (r *Runtime) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log.Printf(format, args...)
	}
}

func (r *Runtime) changed() {
	if r.OnFacts != nil {
		r.OnFacts()
	}
}

// ---------------------------------------------------------------- facts

// DockerFacts are what Docker says about the host.
func (r *Runtime) dockerInfo(ctx context.Context) (*DockerInfo, error) {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	if r.dinfo != nil && time.Since(r.infoAt) < time.Minute {
		return r.dinfo, nil
	}
	i, err := r.Docker.Info(ctx)
	if err != nil {
		return r.dinfo, err
	}
	r.dinfo, r.infoAt = i, time.Now()
	return i, nil
}

// Sysbox says whether Docker lists sysbox-runc.
func (r *Runtime) Sysbox(ctx context.Context) bool {
	i, err := r.dockerInfo(ctx)
	if err != nil || i == nil {
		return false
	}
	_, ok := i.Runtimes["sysbox-runc"]
	return ok
}

// FreeGB is the free space on Docker's disk; -1 when it can't tell.
func (r *Runtime) FreeGB() float64 {
	if r.DockerRoot == "" {
		return -1
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.DockerRoot, &st); err != nil {
		return -1
	}
	return float64(st.Bavail) * float64(st.Bsize) / 1e9
}

// Facts adds Docker's view, the disk, the self-test and the image to f.
func (r *Runtime) Facts(f Facts) Facts {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if i, err := r.dockerInfo(ctx); err == nil && i != nil {
		if i.NCPU > 0 {
			n := i.NCPU
			f.CPUs = &n
		}
		if i.MemTotal > 0 {
			mb := int(i.MemTotal >> 20)
			f.MemoryMB = &mb
		}
		if _, ok := i.Runtimes["sysbox-runc"]; ok {
			f.Runtime = "sysbox"
		}
	}
	if gb := r.FreeGB(); gb >= 0 {
		gb = float64(int(gb*10)) / 10
		f.DiskFreeGB = &gb
	}
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	iso := r.iso
	if iso.Firewall == "" {
		iso.Firewall = "unchecked"
	}
	iso.Unisolated = r.Unisolated
	f.Isolation = iso
	if r.img.Ref != "" {
		img := r.img
		f.Image = &img
	}
	return f
}

// ---------------------------------------------------------------- the image

// SetImage is the image the deployment says machines run (its welcome
// message), unless the installer gave one. It's pulled at once.
func (r *Runtime) SetImage(ref string) {
	if r.ImageOverride != "" {
		ref = r.ImageOverride
	}
	r.factsMu.Lock()
	same := r.image == ref
	r.image = ref
	if !same {
		r.img = ImageFact{Ref: ref, State: "missing"}
	}
	r.factsMu.Unlock()
	if err := ImageAllowed(ref, r.Allowed); err != nil {
		r.factsMu.Lock()
		r.img = ImageFact{Ref: ref, State: "refused"}
		r.factsMu.Unlock()
		r.logf("the deployment's image is refused: %v", err)
		r.changed()
		return
	}
	go r.EnsureImage(context.Background())
}

// Image is the image machines run now.
func (r *Runtime) Image() string {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	if r.ImageOverride != "" {
		return r.ImageOverride
	}
	return r.image
}

// EnsureImage pulls the image when Docker doesn't have it, once at a time.
func (r *Runtime) EnsureImage(ctx context.Context) error {
	ref := r.Image()
	if ref == "" {
		return errors.New("the deployment hasn't said which image machines run yet")
	}
	if err := ImageAllowed(ref, r.Allowed); err != nil {
		return err
	}
	have, err := r.Docker.HasImage(ctx, ref)
	if err != nil {
		return err
	}
	set := func(state string, p *float64) {
		r.factsMu.Lock()
		if r.image == ref || r.ImageOverride == ref {
			r.img = ImageFact{Ref: ref, State: state, Progress: p}
		}
		r.factsMu.Unlock()
	}
	if have {
		set("present", nil)
		r.changed()
		return nil
	}
	r.factsMu.Lock()
	if r.pulling {
		r.factsMu.Unlock()
		return fmt.Errorf("pulling %s; try again when node info says it's present", ref)
	}
	r.pulling = true
	r.factsMu.Unlock()
	defer func() {
		r.factsMu.Lock()
		r.pulling = false
		r.factsMu.Unlock()
	}()
	r.logf("pulling %s", ref)
	t0, last := time.Now(), time.Time{}
	zero := 0.0
	set("pulling", &zero)
	r.changed()
	err = r.Docker.Pull(ctx, ref, func(p float64) {
		p = float64(int(p*1000)) / 1000
		set("pulling", &p)
		if time.Since(last) > 5*time.Second {
			last = time.Now()
			r.changed()
		}
	})
	if err != nil {
		set("missing", nil)
		r.changed()
		return err
	}
	r.logf("pulled %s in %v", ref, time.Since(t0).Round(100*time.Millisecond))
	set("present", nil)
	r.changed()
	return nil
}

// ---------------------------------------------------------------- machines

// MachineInfo is a machine's answer to create and start.
type MachineInfo struct {
	Address   string `json:"address"`
	Container string `json:"container"`
	State     string `json:"state"`
	// Created is set when create made the machine, not when it was there
	// already, so the Worker can tell a sandbox whose machine was removed
	// outside Cloudex that it started over.
	Created bool `json:"created,omitempty"`
	// CA is the node CA's certificate (M6), in start's answer, which the
	// machine trusts for the provider hosts; empty before the deployment has
	// named them.
	CA string `json:"ca,omitempty"`
}

func (r *Runtime) labels(extra ...string) []string {
	return append([]string{LabelNode + "=" + r.NodeID}, extra...)
}

// container finds a sandbox's machine.
func (r *Runtime) container(ctx context.Context, sandbox string) (*ContainerDetail, error) {
	cs, err := r.Docker.Containers(ctx, r.labels(LabelSandbox+"="+sandbox, LabelRole+"=machine")...)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	return r.Docker.ContainerInspect(ctx, cs[0].ID)
}

// VerdictWait is how long create and start wait for the agent's first
// self-test, which runs when it starts and takes about 20 s, so that a
// sandbox resumed just after the agent restarted isn't refused (v0.2.2 M5).
var VerdictWait = 60 * time.Second

// awaitVerdict waits, up to VerdictWait, until the self-test has a verdict.
func (r *Runtime) awaitVerdict(ctx context.Context) {
	deadline := time.Now().Add(VerdictWait)
	for time.Now().Before(deadline) {
		r.factsMu.Lock()
		checked := r.Unisolated || r.iso.Firewall == "pass" || r.iso.Firewall == "fail"
		r.factsMu.Unlock()
		if checked || sleepCtx(ctx, 250*time.Millisecond) != nil {
			return
		}
	}
}

// isolated refuses machines while the self-test doesn't pass, unless the
// node joined with --unisolated (for testing only).
func (r *Runtime) isolated() error {
	if r.Unisolated {
		return nil
	}
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	switch r.iso.Firewall {
	case "pass":
		return nil
	case "fail":
		return fmt.Errorf("this node's isolation self-test fails (%s), so it runs no machines; check its firewall (cloudex-node-firewall.service), or join with --unisolated, for testing only", strings.Join(r.iso.Failed, "; "))
	default:
		return errors.New("this node hasn't finished its isolation self-test yet; try again in a minute")
	}
}

func (r *Runtime) ready() error {
	if r.Self == "" {
		return errors.New("this agent doesn't run in its container (cloudex-node), so it can't run machines")
	}
	if r.NodeID == "" {
		return errors.New("this node hasn't joined")
	}
	return nil
}

// Create makes a sandbox's machine and its network, without starting it.
// Asking again for one that exists answers with it.
func (r *Runtime) Create(ctx context.Context, req MachineRequest) (*MachineInfo, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !sandboxID.MatchString(req.Sandbox) {
		return nil, errors.New("not a sandbox id")
	}
	r.awaitVerdict(ctx)
	if err := r.isolated(); err != nil {
		return nil, err
	}
	image := r.Image()
	if req.Image != "" && r.ImageOverride == "" {
		image = req.Image
	}
	if err := ImageAllowed(image, r.Allowed); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, err := r.container(ctx, req.Sandbox); err != nil {
		return nil, err
	} else if c != nil {
		return r.describe(c, req.Sandbox), nil
	}
	if gb := r.FreeGB(); gb >= 0 && gb < MinFreeGB {
		return nil, fmt.Errorf("Docker's disk on this node has %.1f GB free; new machines need %.0f", gb, MinFreeGB)
	}
	if have, err := r.Docker.HasImage(ctx, image); err != nil {
		return nil, err
	} else if !have {
		if image == r.Image() {
			go r.EnsureImage(context.Background())
		}
		return nil, fmt.Errorf("the node is still pulling %s; try again when node info says it's present", image)
	}
	info, _ := r.dockerInfo(ctx)
	h := MachineHost{NodeID: r.NodeID, Image: image, Sysbox: r.Sysbox(ctx)}
	if info != nil {
		h.HostCPU, h.HostMB = info.NCPU, int(info.MemTotal>>20)
	}
	n, err := r.newNetwork(ctx, req.Sandbox, "machine")
	if err != nil {
		return nil, err
	}
	h.Network = n
	body, err := MachineSpec(req, h)
	if err == nil {
		_, err = r.Docker.ContainerCreate(ctx, n.Name, body)
	}
	if err != nil {
		r.removeNetwork(context.Background(), n)
		return nil, err
	}
	r.logf("%s: made its machine on %s (%s)", req.Sandbox, n.Subnet, n.Bridge)
	return &MachineInfo{Address: n.Machine.String(), Container: n.Name, State: "stopped", Created: true}, nil
}

func (r *Runtime) describe(c *ContainerDetail, sandbox string) *MachineInfo {
	addr := ""
	for name, n := range c.NetworkSettings.Networks {
		if name == "cloudex-"+sandbox || addr == "" {
			addr = n.IPAddress
		}
	}
	state := "stopped"
	if c.State.Running {
		state = "running"
	}
	return &MachineInfo{Address: addr, Container: strings.TrimPrefix(c.Name, "/"), State: state}
}

// newNetwork makes a network for a sandbox (or a probe) on the next free
// /28, and attaches the agent at .2.
func (r *Runtime) newNetwork(ctx context.Context, id, role string) (MachineNet, error) {
	all, err := r.Docker.Networks(ctx)
	if err != nil {
		return MachineNet{}, err
	}
	var used []netip.Prefix
	for _, nw := range all {
		for _, c := range nw.IPAM.Config {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil {
				used = append(used, p)
			}
		}
	}
	for tries := 0; tries < 4; tries++ {
		sub, err := NextSubnet(r.Pool, used)
		if err != nil {
			return MachineNet{}, err
		}
		n, err := NetFor(id, sub)
		if err != nil {
			return MachineNet{}, err
		}
		_, err = r.Docker.NetworkCreate(ctx, NetworkCreate{
			Name:   n.Name,
			Driver: "bridge",
			IPAM:   IPAM{Driver: "default", Config: []IPAMConfig{{Subnet: n.Subnet.String(), Gateway: n.Gateway.String()}}},
			Options: map[string]string{
				"com.docker.network.bridge.name":                 n.Bridge,
				"com.docker.network.bridge.enable_icc":           "true", // the machine and the agent
				"com.docker.network.bridge.enable_ip_masquerade": "true",
			},
			Labels: map[string]string{LabelNode: r.NodeID, LabelSandbox: id, LabelRole: role},
		})
		var de *DockerError
		if errors.As(err, &de) && (strings.Contains(de.Message, "overlap") || de.Status == 409) {
			used = append(used, sub) // taken meanwhile, or by something that isn't a network
			continue
		}
		if err != nil {
			return MachineNet{}, err
		}
		if err := r.Docker.NetworkConnect(ctx, n.Name, r.Self, n.Agent.String(), -1000); err != nil {
			r.Docker.NetworkRemove(context.Background(), n.Name)
			return MachineNet{}, fmt.Errorf("attaching the agent to %s: %w", n.Name, err)
		}
		return n, nil
	}
	return MachineNet{}, errors.New("couldn't find a free network for the machine")
}

func (r *Runtime) removeNetwork(ctx context.Context, n MachineNet) {
	if r.Self != "" {
		r.Docker.NetworkDisconnect(ctx, n.Name, r.Self)
	}
	if err := r.Docker.NetworkRemove(ctx, n.Name); err != nil {
		r.logf("removing the network %s: %v", n.Name, err)
	}
}

// ReadyCheck exits 0 once the entrypoint is ready in this start of the
// machine, and 3 while it holds for a restore, as sandbox.ts's BOOT_CHECK.
const ReadyCheck = `b=$(cat /run/cloudex-boot 2>/dev/null) || b=$(cat /proc/sys/kernel/random/boot_id); ` +
	`[ "$(cat /run/cloudex-ready 2>/dev/null)" = "$b" ] && exit 0; [ "$(cat /run/cloudex-held 2>/dev/null)" = "$b" ] && exit 3; exit 1`

// Start starts a sandbox's machine and waits until its entrypoint is ready,
// or, with hold, holds for a restore (a machine made with hold to start from
// a named save). A machine made with hold that starts again without it, a
// resume, carries CLOUDEX_HOLD=1 for life (Docker keeps a container's
// environment), so it's let go at once.
func (r *Runtime) Start(ctx context.Context, sandbox string, hold bool) (*MachineInfo, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	r.awaitVerdict(ctx)
	if err := r.isolated(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	c, err := r.container(ctx, sandbox)
	if err == nil && c == nil {
		err = fmt.Errorf("no machine here runs %s", sandbox)
	}
	if err == nil {
		// After a reboot the agent's address on the network is kept, but check.
		if err = r.attachSelf(ctx, sandbox); err == nil {
			err = r.Docker.ContainerStart(ctx, c.ID)
		}
	}
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	if !hold && slices.Contains(c.Config.Env, "CLOUDEX_HOLD=1") {
		// /run is a tmpfs, so the entrypoint's hold finds this at once.
		if _, code, err := r.Docker.Run(ctx, c.ID, "0", []string{"touch", "/run/cloudex-go"}); err != nil || code != 0 {
			r.logf("%s: letting a held machine go: exit %d %v", sandbox, code, err)
		}
	}
	if err := r.waitReady(ctx, c.ID, 90*time.Second); err != nil {
		return nil, err
	}
	r.logf("%s: started, ready in %v", sandbox, time.Since(t0).Round(time.Millisecond))
	c, err = r.Docker.ContainerInspect(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	return r.describe(c, sandbox), nil
}

// attachSelf makes sure the agent is on the sandbox's network at .2.
func (r *Runtime) attachSelf(ctx context.Context, sandbox string) error {
	self, err := r.Docker.ContainerInspect(ctx, r.Self)
	if err != nil {
		return fmt.Errorf("the agent's own container: %w", err)
	}
	name := "cloudex-" + sandbox
	if _, ok := self.NetworkSettings.Networks[name]; ok {
		return nil
	}
	ns, err := r.Docker.Networks(ctx, r.labels(LabelSandbox+"="+sandbox)...)
	if err != nil || len(ns) == 0 || len(ns[0].IPAM.Config) == 0 {
		return fmt.Errorf("the machine's network is missing: %v", err)
	}
	sub, err := netip.ParsePrefix(ns[0].IPAM.Config[0].Subnet)
	if err != nil {
		return err
	}
	n, err := NetFor(sandbox, sub)
	if err != nil {
		return err
	}
	return r.Docker.NetworkConnect(ctx, n.Name, r.Self, n.Agent.String(), -1000)
}

func (r *Runtime) waitReady(ctx context.Context, id string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, code, err := r.Docker.Run(cctx, id, "0", []string{"sh", "-c", ReadyCheck})
		cancel()
		if err == nil && (code == 0 || code == 3) {
			return nil
		}
		if c, ierr := r.Docker.ContainerInspect(ctx, id); ierr == nil && !c.State.Running {
			return fmt.Errorf("the machine exited while starting (exit %d)", c.State.ExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the machine wasn't ready within %v", limit)
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// Stop stops a sandbox's machine; its disk stays.
func (r *Runtime) Stop(ctx context.Context, sandbox string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.container(ctx, sandbox)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no machine here runs %s", sandbox)
	}
	return r.Docker.ContainerStop(ctx, c.ID, StopSeconds)
}

// Delete removes a sandbox's machine, its anonymous volumes and its network.
func (r *Runtime) Delete(ctx context.Context, sandbox string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs, err := r.Docker.Containers(ctx, r.labels(LabelSandbox+"="+sandbox)...)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := r.Docker.ContainerRemove(ctx, c.ID); err != nil {
			return err
		}
	}
	ns, err := r.Docker.Networks(ctx, r.labels(LabelSandbox+"="+sandbox)...)
	if err != nil {
		return err
	}
	for _, n := range ns {
		if r.Self != "" {
			r.Docker.NetworkDisconnect(ctx, n.ID, r.Self)
		}
		if err := r.Docker.NetworkRemove(ctx, n.ID); err != nil {
			return err
		}
	}
	r.logf("%s: removed its machine", sandbox)
	return nil
}

// Machines lists this node's machines and their states, for hello.
func (r *Runtime) Machines(ctx context.Context) ([]Machine, error) {
	if r.NodeID == "" {
		return []Machine{}, nil
	}
	cs, err := r.Docker.Containers(ctx, r.labels(LabelRole+"=machine")...)
	if err != nil {
		return nil, err
	}
	out := []Machine{}
	for _, c := range cs {
		id := c.Labels[LabelSandbox]
		if !sandboxID.MatchString(id) {
			continue
		}
		state := "stopped"
		if c.State == "running" {
			state = "running"
		}
		out = append(out, Machine{ID: id, State: state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Target is where streams reach a running machine.
func (r *Runtime) Target(ctx context.Context, sandbox string) (Target, bool, error) {
	c, err := r.container(ctx, sandbox)
	if err != nil || c == nil {
		return Target{}, false, err
	}
	i := r.describe(c, sandbox)
	return Target{Container: c.ID, Address: i.Address}, c.State.Running, nil
}

// Watch follows Docker's events for this node's machines, calling OnState
// for each start and stop, until ctx ends. A broken stream is followed again.
func (r *Runtime) Watch(ctx context.Context) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		t0 := time.Now()
		err := r.Docker.Events(ctx, func(e Event) {
			id := e.Actor.Attributes[LabelSandbox]
			if e.Actor.Attributes[LabelRole] != "machine" || !sandboxID.MatchString(id) || r.OnState == nil {
				return
			}
			switch e.Action {
			case "start":
				r.OnState(Machine{ID: id, State: "running"})
			case "die":
				r.OnState(Machine{ID: id, State: "stopped"})
			case "oom":
				r.logf("%s: its machine ran out of memory", id)
			}
		}, r.labels(LabelRole+"=machine")...)
		if ctx.Err() != nil {
			return
		}
		if time.Since(t0) > time.Minute {
			attempt = 0
		}
		d := Backoff(attempt, nil)
		r.logf("Docker's event stream ended (%v); following it again in %v", err, d.Round(time.Millisecond))
		sleepCtx(ctx, d)
	}
}

// Sweep removes what an earlier self-test or check left behind (the agent
// died during one).
func (r *Runtime) Sweep(ctx context.Context) {
	for _, role := range []string{"probe", "check"} {
		cs, _ := r.Docker.Containers(ctx, r.labels(LabelRole+"="+role)...)
		for _, c := range cs {
			r.Docker.ContainerRemove(ctx, c.ID)
		}
		ns, _ := r.Docker.Networks(ctx, r.labels(LabelRole+"="+role)...)
		for _, n := range ns {
			if r.Self != "" {
				r.Docker.NetworkDisconnect(ctx, n.ID, r.Self)
			}
			r.Docker.NetworkRemove(ctx, n.ID)
		}
	}
}
