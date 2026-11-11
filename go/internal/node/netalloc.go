package node

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Each machine has a Docker network of its own: a /28 from the node's pool,
// a bridge whose name starts with "cx" (which the host firewall keys on), no
// IPv6, the host's bridge address at .1, the agent at .2 and the machine at
// .3. Docker keeps bridge networks apart, and the firewall (firewall.go)
// keeps them apart from the host, its networks and each other.

// MachineNet is one machine's network.
type MachineNet struct {
	Name    string       // the Docker network: cloudex-<sandbox>
	Bridge  string       // the host's bridge: cx<the sandbox id's 6 characters>
	Subnet  netip.Prefix // a /28
	Gateway netip.Addr   // .1, the host
	Agent   netip.Addr   // .2
	Machine netip.Addr   // .3
	Probe   netip.Addr   // .4, the self-test's second probe
}

// SubnetBits is a machine network's size: 16 addresses.
const SubnetBits = 28

// NetFor lays out the network for a sandbox (or a self-test probe) in a /28.
func NetFor(sandbox string, subnet netip.Prefix) (MachineNet, error) {
	if subnet.Bits() != SubnetBits || !subnet.Addr().Is4() || subnet.Masked() != subnet {
		return MachineNet{}, fmt.Errorf("%s isn't a /%d", subnet, SubnetBits)
	}
	bridge, err := BridgeName(sandbox)
	if err != nil {
		return MachineNet{}, err
	}
	base := subnet.Addr()
	return MachineNet{
		Name:    "cloudex-" + sandbox,
		Bridge:  bridge,
		Subnet:  subnet,
		Gateway: addN(base, 1),
		Agent:   addN(base, 2),
		Machine: addN(base, 3),
		Probe:   addN(base, 4),
	}, nil
}

// BridgeName is a sandbox's bridge: cx and its id's six characters
// (cx-abc123 has cxabc123). A self-test's probe network has cx-t and six hex
// digits, which no sandbox can have.
func BridgeName(sandbox string) (string, error) {
	if sandboxID.MatchString(sandbox) {
		return "cx" + sandbox[3:], nil
	}
	if probeIDPattern.MatchString(sandbox) {
		return sandbox, nil
	}
	return "", fmt.Errorf("%q isn't a sandbox id", sandbox)
}

func addN(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:]) + n
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// CheckPool says whether a prefix can be a node's machine pool: IPv4, from
// 10.0.0.0/8 or 172.16.0.0/12 (which the firewall blocks for machines
// anyway), and from /12 to /24.
func CheckPool(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || p.Masked() != p {
		return fmt.Errorf("%s isn't an IPv4 network such as 10.250.0.0/16", p)
	}
	if p.Bits() < 12 || p.Bits() > 24 {
		return fmt.Errorf("the machine pool %s must be from a /12 to a /24", p)
	}
	ok := false
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12"} {
		if q := netip.MustParsePrefix(r); q.Contains(p.Addr()) && q.Bits() <= p.Bits() {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("the machine pool %s must be inside 10.0.0.0/8 or 172.16.0.0/12", p)
	}
	return nil
}

// ErrPoolFull is when every /28 in the pool is taken.
var ErrPoolFull = errors.New("the node's machine pool has no free network left")

// NextSubnet is the first /28 of pool that overlaps nothing in used.
func NextSubnet(pool netip.Prefix, used []netip.Prefix) (netip.Prefix, error) {
	return NextSubnetAfter(pool, used, netip.Prefix{})
}

// NextSubnetAfter is the first free /28 of the pool after last, going round:
// subnets are handed out in turn, so a deleted machine's isn't given to the
// next one while its old connections may still be tracked (review H8).
func NextSubnetAfter(pool netip.Prefix, used []netip.Prefix, last netip.Prefix) (netip.Prefix, error) {
	if err := CheckPool(pool); err != nil {
		return netip.Prefix{}, err
	}
	n := uint32(1) << (SubnetBits - pool.Bits())
	start := uint32(0)
	if last.IsValid() && last.Bits() == SubnetBits && pool.Contains(last.Addr()) {
		start = (ipU32(last.Addr())-ipU32(pool.Addr()))>>(32-SubnetBits) + 1
	}
	for k := uint32(0); k < n; k++ {
		i := (start + k) % n
		cand := netip.PrefixFrom(addN(pool.Addr(), i<<(32-SubnetBits)), SubnetBits)
		free := true
		for _, u := range used {
			if u.Overlaps(cand) {
				free = false
				break
			}
		}
		if free {
			return cand, nil
		}
	}
	return netip.Prefix{}, ErrPoolFull
}

// PoolCandidates are the /16s the installer tries, in order.
var PoolCandidates = []string{"10.250.0.0/16", "10.251.0.0/16", "10.252.0.0/16", "10.253.0.0/16", "10.254.0.0/16",
	"172.30.0.0/16", "172.29.0.0/16", "172.28.0.0/16", "10.199.0.0/16", "10.99.0.0/16"}

// PickPool is the first candidate that overlaps none of the prefixes in use
// on the host (its routes and Docker's networks).
func PickPool(used []netip.Prefix) (netip.Prefix, error) {
	for _, c := range PoolCandidates {
		p := netip.MustParsePrefix(c)
		free := true
		for _, u := range used {
			if u.Overlaps(p) {
				free = false
				break
			}
		}
		if free {
			return p, nil
		}
	}
	return netip.Prefix{}, errors.New("every machine pool the installer knows collides with a route or a Docker network on this host; give one with --pool")
}

// HostRoutes reads the IPv4 routes of the network namespace it runs in from
// /proc/net/route (the default route is skipped). The installer runs it in
// the host's namespace.
func HostRoutes(path string) ([]netip.Prefix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []netip.Prefix
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 8 {
			continue
		}
		dst, err1 := hex.DecodeString(fl[1])
		mask, err2 := hex.DecodeString(fl[7])
		if err1 != nil || err2 != nil || len(dst) != 4 || len(mask) != 4 {
			continue
		}
		// /proc/net/route is little-endian.
		d := netip.AddrFrom4([4]byte{dst[3], dst[2], dst[1], dst[0]})
		m := binary.LittleEndian.Uint32(mask)
		bits := 0
		for m != 0 {
			bits += int(m & 1)
			m >>= 1
		}
		if bits == 0 {
			continue
		}
		out = append(out, netip.PrefixFrom(d, bits).Masked())
	}
	return out, sc.Err()
}

func ipU32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
