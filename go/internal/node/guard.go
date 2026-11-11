package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"syscall"
	"time"
)

// What the agent guards on the host while machines run (the 2026-11-12
// security review):
//
//   - The disk (H2). Docker's data root is often the host's root filesystem,
//     and a machine's writes have no limit there unless the storage driver
//     can cap them (StorageOpt, which overlayfs on ext4 can't). Every few
//     seconds the agent reads the free space; below the node's floor
//     (KeepFreeGB, 5 GB unless the installer says otherwise) it stops the
//     running machine whose writable layer is largest, and its sandbox's
//     notice says why. It stops another only while the space keeps falling,
//     so a disk that's merely full doesn't cost every machine.
//   - The firewall (H3). The table can vanish while the agent runs: any
//     `flush ruleset` (systemctl restart nftables on Debian and Ubuntu) takes
//     it. Systemd loads it again (the unit is PartOf nftables.service, and a
//     timer checks it), but in between machines would reach the host and its
//     LAN. So every few seconds the agent knocks on the host from its address
//     on a machine network, at CanaryPort, which the table rejects with an
//     ICMP admin-prohibited. A refusal or an answer means the table is gone:
//     the node's verdict becomes fail, its running machines stop, and the
//     self-test runs again; once the table is back and the self-test passes,
//     they may be resumed. A timeout says nothing (some other firewall drops
//     it), so the agent doesn't act on one.

// KeepFreeGB is the default floor for Docker's free space, below which the
// watchdog stops a machine; new machines need KeepFree + 5 GB (at least
// MinFreeGB), and starts need KeepFree.
const KeepFreeGB = 5.0

// GuardEvery is how often the watchdog and the canary run.
var GuardEvery = 5 * time.Second

// keepFree is the node's floor.
func (r *Runtime) keepFree() float64 {
	if r.KeepFreeGB > 0 {
		return r.KeepFreeGB
	}
	return KeepFreeGB
}

// minFree is what a new machine needs free.
func (r *Runtime) minFree() float64 {
	return max(MinFreeGB, r.keepFree()+5)
}

// stopFor stops a running machine for a reason the agent decided on, which
// goes with its state message.
func (r *Runtime) stopFor(ctx context.Context, sandbox, id, note string) error {
	r.factsMu.Lock()
	if r.notes == nil {
		r.notes = map[string]string{}
	}
	r.notes[sandbox] = note
	r.factsMu.Unlock()
	r.logf("%s: stopping its machine: %s", sandbox, note)
	if err := r.Docker.ContainerStop(ctx, id, 2); err != nil {
		r.factsMu.Lock()
		delete(r.notes, sandbox)
		r.factsMu.Unlock()
		return err
	}
	return nil
}

// note is the reason the agent stopped a machine. It stays until the machine
// starts again, so that a hello carries it too (Machines): the state message
// that told it is lost when the link is down then, as it is after a flush of
// the host's rules took Docker's with it.
func (r *Runtime) note(sandbox string) string {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	return r.notes[sandbox]
}

// forgetNote drops a machine's note once it runs again.
func (r *Runtime) forgetNote(sandbox string) {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	delete(r.notes, sandbox)
}

// StopAll stops every running machine of this node, saying why.
func (r *Runtime) StopAll(ctx context.Context, note string) int {
	cs, err := r.Docker.Containers(ctx, r.labels(LabelRole+"=machine")...)
	if err != nil {
		r.logf("stopping every machine: %v", err)
		return 0
	}
	n := 0
	for _, c := range cs {
		id := c.Labels[LabelSandbox]
		if c.State != "running" || !sandboxID.MatchString(id) {
			continue
		}
		if err := r.stopFor(ctx, id, c.ID, note); err != nil {
			r.logf("%s: stopping its machine: %v", id, err)
			continue
		}
		n++
	}
	return n
}

// diskWatch is one look at the free space.
type diskWatch struct {
	lastStop time.Time
	lastFree float64
}

func (r *Runtime) checkDisk(ctx context.Context, w *diskWatch) {
	free := r.FreeGB()
	floor := r.keepFree()
	if free < 0 || free >= floor {
		return
	}
	// Something already stopped: another only while the space still falls.
	if !w.lastStop.IsZero() && time.Since(w.lastStop) < 10*time.Minute && free > w.lastFree-0.25 {
		return
	}
	cs, err := r.Docker.ContainersSized(ctx, r.labels(LabelRole+"=machine")...)
	if err != nil {
		r.logf("the disk watchdog: listing machines: %v", err)
		return
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].SizeRw > cs[j].SizeRw })
	for _, c := range cs {
		id := c.Labels[LabelSandbox]
		if c.State != "running" || !sandboxID.MatchString(id) {
			continue
		}
		note := fmt.Sprintf("Docker's disk on this node had %.1f GB free, under its floor of %.0f GB, and this machine had written the most (%.1f GB). Free space on the host, or delete files in the sandbox after resuming it.", free, floor, float64(c.SizeRw)/1e9)
		if err := r.stopFor(ctx, id, c.ID, note); err != nil {
			r.logf("the disk watchdog: %v", err)
			return
		}
		w.lastStop, w.lastFree = time.Now(), free
		return
	}
}

// Canary results.
const (
	canaryNone    = ""        // no machine network to knock from, or no answer either way
	canaryPresent = "present" // the table rejected the knock
	canaryGone    = "gone"    // the host answered: the table is missing
)

// canary knocks on the host from the agent's address on a machine network.
func (r *Runtime) canary(ctx context.Context, nets map[string]MachineNet) string {
	ids := make([]string, 0, len(nets))
	for id := range nets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := nets[id]
		d := net.Dialer{Timeout: 1500 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: n.Agent.AsSlice()}}
		c, err := d.DialContext(ctx, "tcp4", netip.AddrPortFrom(n.Gateway, CanaryPort).String())
		switch {
		case err == nil:
			c.Close()
			return canaryGone
		case errors.Is(err, syscall.ECONNREFUSED):
			return canaryGone
		case errors.Is(err, syscall.EHOSTUNREACH):
			return canaryPresent
		case errors.Is(err, syscall.EADDRNOTAVAIL):
			continue // the agent isn't on that network (yet)
		default:
			return canaryNone // a timeout: something else drops it
		}
	}
	return canaryNone
}

// firewallGone acts on a missing table.
func (r *Runtime) firewallGone(ctx context.Context) {
	r.factsMu.Lock()
	r.iso.Firewall = "fail"
	r.iso.CheckedAt = time.Now().UnixMilli()
	r.iso.Failed = []string{"the firewall's table (inet cloudex) is gone from the host's nftables, so machines would reach the host and its network (a flush of the ruleset, such as systemctl restart nftables, removes it)"}
	r.factsMu.Unlock()
	r.changed()
	n := r.StopAll(ctx, "this node's firewall table (inet cloudex) disappeared, so its machines were stopped before they could reach the host or its network. It is loaded again by cloudex-node-firewall.service; once the node's self-test passes, resume the sandbox.")
	r.logf("the firewall's table is gone: stopped %d running machines and refuse new ones until the self-test passes", n)
}

// Guard runs the disk watchdog and the firewall canary until ctx ends.
// selfTest runs the self-test again (once the table is back).
func (r *Runtime) Guard(ctx context.Context, selfTest func()) {
	var w diskWatch
	t := time.NewTicker(GuardEvery)
	defer t.Stop()
	gone := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r.checkDisk(cctx, &w)
		if !r.Unisolated {
			if nets, err := r.MachineNets(cctx); err == nil && len(nets) > 0 {
				switch r.canary(cctx, nets) {
				case canaryGone:
					if !gone {
						gone = true
						r.firewallGone(cctx)
						selfTest()
					}
				case canaryPresent:
					if gone {
						gone = false
						r.logf("the firewall's table is back; running the self-test again")
						selfTest()
					}
				}
			}
		}
		cancel()
	}
}

// DockerTooOld says why a Docker server version can't run machines: before
// 28, Docker ignores the gateway priority the agent's networks are attached
// with, and the agent's default route could move onto a machine network
// (review H7).
func DockerTooOld(version string) error {
	if m := DockerMajor(version); m > 0 && m < 28 {
		return fmt.Errorf("Docker %s is too old for machines: nodes need Docker 28 or later (earlier versions ignore the network priority that keeps the agent's own route off machine networks); upgrade Docker, then run the installer again", strings.TrimSpace(version))
	}
	return nil
}
