//go:build !linux

package node

import (
	"net"
	"net/netip"
)

func interfaceOf(netip.Addr) string { return "" }

func listenOn(addr, _ string) (net.Listener, error) { return net.Listen("tcp4", addr) }
