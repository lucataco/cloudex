package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lucataco/cloudex/go/internal/direct"
)

// The direct port's relay (v0.2.3): a sidecar container, cloudex-node-direct,
// that Docker publishes the direct port on, so that a host address missing
// when the agent starts (a late tailscale0, a DHCP change) fails the sidecar
// alone, never the agent (research/v0.2.3-plan.md M0 Q2). It runs the agent's
// own image as `cloudex-node relay`, holds no socket, key or secret, and only
// passes each connection on to the agent's listener (direct.go), after one
// line in the clear that names the client: direct.RelayHeader.

// RelayName is the sidecar's container name.
const RelayName = "cloudex-node-direct"

// AgentNetwork is the agent's own Docker network, which the installer makes;
// the sidecar joins it, and the agent's address on it is its control address.
const AgentNetwork = "cloudex-node"

// RelayPort is the port inside the sidecar and on the agent's control
// address. The host port Docker publishes may differ (--direct-port); inside
// both containers it is always this one, above 1024, so neither needs a
// capability to bind it.
const RelayPort = direct.Port

// RelayConnsMax is the most connections the relay carries at once: the
// agent's session and handshake caps, with room. More are closed at accept.
const RelayConnsMax = 384

// RelayBody is the sidecar's POST /containers/create body. Fields that
// aren't here are Docker's defaults: no binds, mounts, devices, added
// capabilities or shared namespaces.
type RelayBody struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	StopTimeout  int                 `json:"StopTimeout"`
	HostConfig   RelayHostConfig     `json:"HostConfig"`
}

type RelayHostConfig struct {
	NetworkMode    string                   `json:"NetworkMode"`
	PortBindings   map[string][]PortBinding `json:"PortBindings"`
	ReadonlyRootfs bool                     `json:"ReadonlyRootfs"`
	CapDrop        []string                 `json:"CapDrop"`
	SecurityOpt    []string                 `json:"SecurityOpt"`
	Memory         int64                    `json:"Memory"`
	MemorySwap     int64                    `json:"MemorySwap"`
	PidsLimit      int64                    `json:"PidsLimit"`
	RestartPolicy  RestartPolicy            `json:"RestartPolicy"`
	LogConfig      LogConfig                `json:"LogConfig"`
}

// LabelAgent marks the agent's own objects: its container, network, volume
// and the sidecar (the installer's label).
const LabelAgent = "build.cloudex.agent"

// relayPortKey is the sidecar's port in Docker's form.
var relayPortKey = strconv.Itoa(RelayPort) + "/tcp"

// RelaySpec is the sidecar: the agent's image (by id), relaying to the
// agent's control address, published on exactly the given host addresses
// (each RFC 1918 or 100.64/10 IPv4) and port.
func RelaySpec(image string, to netip.Addr, addrs []netip.Addr, hostPort int) (*RelayBody, error) {
	if image == "" {
		return nil, errors.New("no image for the relay")
	}
	if !to.Is4() {
		return nil, errors.New("the agent's control address isn't IPv4")
	}
	if hostPort < 1 || hostPort > 65535 {
		return nil, fmt.Errorf("the direct port %d isn't a port", hostPort)
	}
	if len(addrs) == 0 || len(addrs) > DirectAddrsMax {
		return nil, fmt.Errorf("the direct port needs 1 to %d addresses", DirectAddrsMax)
	}
	var binds []PortBinding
	seen := map[netip.Addr]bool{}
	for _, a := range addrs {
		if !direct.Usable(a) {
			return nil, fmt.Errorf("%s isn't a private (RFC 1918) or tailnet (100.64/10) IPv4 address", a)
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		binds = append(binds, PortBinding{HostIP: a.String(), HostPort: strconv.Itoa(hostPort)})
	}
	return &RelayBody{
		Image:        image,
		Cmd:          RelayCmd(to),
		Labels:       map[string]string{LabelAgent: "1", LabelRole: "direct"},
		ExposedPorts: map[string]struct{}{relayPortKey: {}},
		StopTimeout:  2,
		HostConfig: RelayHostConfig{
			NetworkMode:    AgentNetwork,
			PortBindings:   map[string][]PortBinding{relayPortKey: binds},
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Memory:         64 << 20,
			MemorySwap:     64 << 20,
			PidsLimit:      64,
			RestartPolicy:  RestartPolicy{Name: "unless-stopped"},
			LogConfig:      LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "2"}},
		},
	}, nil
}

// RelayCmd is the sidecar's command.
func RelayCmd(to netip.Addr) []string {
	return []string{"relay", "--to", netip.AddrPortFrom(to, RelayPort).String()}
}

// Relay accepts connections on ln and passes each on to the agent at to,
// after the header naming the client. It returns when ctx ends.
func Relay(ctx context.Context, ln net.Listener, to string, lg *log.Logger) error {
	go func() { <-ctx.Done(); ln.Close() }()
	var open atomic.Int64
	var refusedAt atomic.Int64
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, syscall.EMFILE) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		if open.Load() >= RelayConnsMax {
			c.Close()
			if now := time.Now().Unix(); refusedAt.Swap(now) != now && lg != nil {
				lg.Printf("over %d connections at once: closing new ones", RelayConnsMax)
			}
			continue
		}
		open.Add(1)
		go func() {
			defer open.Add(-1)
			defer func() { recover() }()
			relayOne(c, to)
		}()
	}
}

func relayOne(c net.Conn, to string) {
	defer c.Close()
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	up, err := d.Dial("tcp4", to)
	if err != nil {
		return
	}
	defer up.Close()
	up.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(up, direct.RelayHeader(peer)); err != nil {
		return
	}
	up.SetWriteDeadline(time.Time{})
	// TCP to TCP: io.Copy uses splice(2) on Linux, so the relay keeps no
	// buffers of its own.
	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
				return
			}
		}
		dst.Close()
		src.Close()
	}
	go half(up, c)
	go half(c, up)
	wg.Wait()
}
