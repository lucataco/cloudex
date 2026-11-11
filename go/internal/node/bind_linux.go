//go:build linux

package node

import (
	"context"
	"net"
	"net/netip"
	"syscall"
)

// interfaceOf names the interface that has addr; "" when none does.
func interfaceOf(addr netip.Addr) string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifs {
		as, _ := i.Addrs()
		for _, a := range as {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr() == addr {
				return i.Name
			}
		}
	}
	return ""
}

// listenOn listens on addr, bound to the interface ifname when it's given.
func listenOn(addr, ifname string) (net.Listener, error) {
	lc := net.ListenConfig{}
	if ifname != "" {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) { serr = syscall.BindToDevice(int(fd), ifname) }); err != nil {
				return err
			}
			return serr
		}
	}
	return lc.Listen(context.Background(), "tcp4", addr)
}
