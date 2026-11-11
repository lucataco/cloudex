package node

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// A machine's container is built here and nowhere else, from a fixed set of
// flags. What the deployment may choose is MachineRequest: the sandbox, its
// size, a few environment variables and whether it holds for a restore. The
// rest (the image, the network, the addresses, the runtime) is the agent's.
// Nothing a request carries can add a mount, a device, a capability,
// privileged mode, host networking, or a namespace shared with the host:
// CreateBody has no field for most of them, and the ones it has are set here
// to their safe values (spec_test.go checks the whole body).

// MachineRequest is what the deployment asks for (op create).
type MachineRequest struct {
	Sandbox  string            `json:"sandbox"`
	Image    string            `json:"image,omitempty"`
	CPUs     float64           `json:"cpus,omitempty"`
	MemoryMB int               `json:"memoryMb,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Hold     bool              `json:"hold,omitempty"`
}

// MachineHost is what the agent decides about a machine.
type MachineHost struct {
	NodeID  string
	Image   string     // checked against the allowed list already
	Network MachineNet // its own network
	Sysbox  bool       // run it under sysbox-runc (Docker inside works)
	HostCPU int        // the host's CPUs, the most a machine may have
	HostMB  int        // the host's memory in MiB
	Role    string     // machine, probe or check
	Labels  map[string]string
	DiskGB  int // the writable layer's size cap, where the storage driver has one; 0: none
}

// The default size: standard-2's on Cloudflare is 1 vCPU and 6 GiB.
const (
	DefaultCPUs     = 1.0
	DefaultMemoryMB = 6144
	MinMemoryMB     = 512
	PidsLimit       = 4096
	StopSeconds     = 20
)

// MachineDNS are the resolvers machines use (Docker's embedded resolver
// forwards to them), so that a machine never asks the host's or the LAN's.
var MachineDNS = []string{"1.1.1.1", "8.8.8.8"}

// CapDrop are the capabilities taken from Docker's default set (N9).
var CapDrop = []string{"NET_RAW", "MKNOD", "SETFCAP"}

// A self-test's or the installer's check machine: cx-t and six hex digits,
// which no sandbox id has.
var probeIDPattern = regexp.MustCompile(`^cx-t[0-9a-f]{6}$`)

var envKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// Variables the agent sets, which a request can't.
var reservedEnv = map[string]bool{"CLOUDEX_SANDBOX": true, "CLOUDEX_DOCKER": true, "CLOUDEX_HOLD": true}

const (
	envMax      = 64
	envValueMax = 16 << 10
)

// CreateBody is POST /containers/create's body as the agent sends it. Fields
// that aren't here are Docker's defaults: no binds, mounts, devices, added
// capabilities, published ports, or shared namespaces.
type CreateBody struct {
	Image       string            `json:"Image"`
	Hostname    string            `json:"Hostname"`
	Env         []string          `json:"Env"`
	Labels      map[string]string `json:"Labels"`
	StopTimeout int               `json:"StopTimeout"`
	HostConfig  HostConfig        `json:"HostConfig"`
	Networking  NetworkingConfig  `json:"NetworkingConfig"`
}

type HostConfig struct {
	NetworkMode    string            `json:"NetworkMode"`
	Privileged     bool              `json:"Privileged"`
	CapDrop        []string          `json:"CapDrop"`
	PidMode        string            `json:"PidMode"`
	IpcMode        string            `json:"IpcMode"`
	UTSMode        string            `json:"UTSMode"`
	UsernsMode     string            `json:"UsernsMode"`
	CgroupnsMode   string            `json:"CgroupnsMode"`
	Runtime        string            `json:"Runtime"`
	Tmpfs          map[string]string `json:"Tmpfs"`
	NanoCPUs       int64             `json:"NanoCpus"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
	PidsLimit      int64             `json:"PidsLimit"`
	OomScoreAdj    int               `json:"OomScoreAdj"`
	RestartPolicy  RestartPolicy     `json:"RestartPolicy"`
	DNS            []string          `json:"Dns"`
	DNSSearch      []string          `json:"DnsSearch"`
	ExtraHosts     []string          `json:"ExtraHosts"`
	Sysctls        map[string]string `json:"Sysctls"`
	LogConfig      LogConfig         `json:"LogConfig"`
	CapAdd         []string          `json:"CapAdd,omitempty"` // probes only (ProbeSpec); never for a machine
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	// StorageOpt caps the writable layer ({"size": "50G"}) where the storage
	// driver can (btrfs, zfs, overlay2 on xfs with pquota); elsewhere the
	// agent's disk watchdog is the limit (review H2).
	StorageOpt map[string]string `json:"StorageOpt,omitempty"`
}

type RestartPolicy struct {
	Name string `json:"Name"`
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointConfig `json:"EndpointsConfig"`
}

type EndpointConfig struct {
	IPAMConfig struct {
		IPv4Address string `json:"IPv4Address"`
	} `json:"IPAMConfig"`
}

// MachineEnv checks the variables a request asks for.
func MachineEnv(env map[string]string) ([]string, error) {
	if len(env) > envMax {
		return nil, fmt.Errorf("at most %d environment variables", envMax)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		if !envKey.MatchString(k) {
			return nil, fmt.Errorf("%q isn't an environment variable's name", k)
		}
		if reservedEnv[k] {
			return nil, fmt.Errorf("%s is set by the node's agent", k)
		}
		if len(v) > envValueMax || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("the value of %s is too long or holds a NUL", k)
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// MachineSpec builds a machine's container from a request.
func MachineSpec(r MachineRequest, h MachineHost) (*CreateBody, error) {
	role := h.Role
	if role == "" {
		role = "machine"
	}
	switch {
	case role == "machine" && sandboxID.MatchString(r.Sandbox):
	case (role == "probe" || role == "check") && probeIDPattern.MatchString(r.Sandbox):
	default:
		return nil, errors.New("not a sandbox id")
	}
	if h.Image == "" || h.NodeID == "" || !h.Network.Subnet.IsValid() {
		return nil, errors.New("the agent has no image, node id or network for the machine")
	}
	cpus := r.CPUs
	if cpus == 0 {
		cpus = DefaultCPUs
	}
	if math.IsNaN(cpus) || cpus < 0.25 || (h.HostCPU > 0 && cpus > float64(h.HostCPU)) || cpus > 1024 {
		return nil, fmt.Errorf("cpus must be from 0.25 to the host's %d", h.HostCPU)
	}
	mem := r.MemoryMB
	if mem == 0 {
		mem = DefaultMemoryMB
		if h.HostMB > 0 && mem > h.HostMB/2 {
			mem = max(MinMemoryMB, h.HostMB/2)
		}
	}
	if mem < MinMemoryMB || (h.HostMB > 0 && mem > h.HostMB) {
		return nil, fmt.Errorf("memory must be from %d MiB to the host's %d", MinMemoryMB, h.HostMB)
	}
	env, err := MachineEnv(r.Env)
	if err != nil {
		return nil, err
	}
	docker := "0"
	runtime := "runc"
	if h.Sysbox {
		docker, runtime = "1", "sysbox-runc"
	}
	env = append(env, "CLOUDEX_SANDBOX="+r.Sandbox, "CLOUDEX_DOCKER="+docker)
	if r.Hold {
		env = append(env, "CLOUDEX_HOLD=1")
	}
	labels := map[string]string{}
	for k, v := range h.Labels {
		labels[k] = v
	}
	labels[LabelNode], labels[LabelSandbox], labels[LabelRole] = h.NodeID, r.Sandbox, role
	n := h.Network
	return &CreateBody{
		Image:       h.Image,
		Hostname:    r.Sandbox,
		Env:         env,
		Labels:      labels,
		StopTimeout: StopSeconds,
		HostConfig: HostConfig{
			NetworkMode:  n.Name,
			Privileged:   false,
			CapDrop:      append([]string(nil), CapDrop...),
			PidMode:      "",
			IpcMode:      "private",
			UTSMode:      "",
			UsernsMode:   "",
			CgroupnsMode: "private",
			Runtime:      runtime,
			// A tmpfs /run: the per-start marker can't outlive a restart (N9).
			Tmpfs:         map[string]string{"/run": "rw,nosuid,nodev,noexec,mode=755"},
			NanoCPUs:      int64(math.Round(cpus * 1e9)),
			Memory:        int64(mem) << 20,
			MemorySwap:    int64(mem) << 20, // no swap beyond the memory
			PidsLimit:     PidsLimit,
			OomScoreAdj:   500,                       // the kernel's first choice when the host runs out, before the host's own
			RestartPolicy: RestartPolicy{Name: "no"}, // the deployment decides what runs
			DNS:           append([]string(nil), MachineDNS...),
			DNSSearch:     []string{"."},
			ExtraHosts:    []string{"cloudex.internal:" + n.Agent.String()},
			// No IPv6 on the machine's interfaces (Docker leaves eth0 without it
			// on an IPv4 network; this covers any added later), while lo keeps
			// ::1, which dev servers such as Vite bind by default.
			Sysctls:    map[string]string{"net.ipv6.conf.default.disable_ipv6": "1"},
			LogConfig:  LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
			StorageOpt: storageOpt(h.DiskGB),
		},
		Networking: NetworkingConfig{EndpointsConfig: map[string]EndpointConfig{n.Name: endpoint(n.Machine)}},
	}, nil
}

func storageOpt(gb int) map[string]string {
	if gb <= 0 {
		return nil
	}
	return map[string]string{"size": fmt.Sprintf("%dG", gb)}
}

func endpoint(a netip.Addr) EndpointConfig {
	var e EndpointConfig
	e.IPAMConfig.IPv4Address = a.String()
	return e
}

// ProbeSpec is the self-test's probe that may change its routes: a machine's
// spec plus NET_ADMIN, which root has under Sysbox. Only the agent's own
// self-test makes one, with its own fixed commands, and removes it after.
func ProbeSpec(id string, h MachineHost) (*CreateBody, error) {
	h.Role = "probe"
	b, err := MachineSpec(MachineRequest{Sandbox: id, MemoryMB: MinMemoryMB, CPUs: 0.25}, h)
	if err != nil {
		return nil, err
	}
	b.HostConfig.CapAdd = []string{"NET_ADMIN"}
	return b, nil
}
