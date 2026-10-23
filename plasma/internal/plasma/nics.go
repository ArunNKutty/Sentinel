package plasma

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Link NICs. A VM's eth0 is its management NIC on plasma-br0; every link a
// topology wires to it adds one more NIC, eth1 upward, whose host-side tap
// joins a link bridge (or, for a `host:` endpoint, stays unbridged).

// nicNamePattern is a link NIC's guest name. eth0 is the management NIC.
var nicNamePattern = regexp.MustCompile(`^eth([1-9][0-9]*)$`)

// ifacePattern keeps a host interface name usable by ip(8) and the kernel.
var ifacePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// nicIndex returns n for "ethN", or false for anything else.
func nicIndex(name string) (int, bool) {
	m := nicNamePattern.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// NicTapName is the host-side tap for a VM's link NIC. Keyed on the VM's
// address octet rather than its ID, so it is unique and always fits IFNAMSIZ:
// "pt254-eth99" is 11 bytes.
func NicTapName(octet int, nic string) string {
	return fmt.Sprintf("pt%d-%s", octet, nic)
}

// NicMAC is a link NIC's address. The 0b byte keeps it clear of management
// MACs (02:00:0a:00:00:<octet>), so no two NICs on the host ever share one.
func NicMAC(octet, index int) string {
	return fmt.Sprintf("02:00:0b:00:%02x:%02x", index, octet)
}

// LinkBridgeName names the private bridge behind a VM-to-VM link. Derived from
// both endpoints, order-insensitively, so each VM's launch arrives at the same
// name without coordinating with the other.
func LinkBridgeName(aNode, aIface, bNode, bIface string) string {
	a, b := aNode+":"+aIface, bNode+":"+bIface
	if b < a {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte(a + "|" + b))
	return LinkBridgePrefix + hex.EncodeToString(sum[:4])
}

// NodeBridgeName is the host bridge a `kind: bridge` node stands for.
func NodeBridgeName(node string) string { return NodeBridgePrefix + node }

// isLinkBridge reports whether plasma owns a bridge by this name.
func isLinkBridge(name string) bool {
	return strings.HasPrefix(name, LinkBridgePrefix) || strings.HasPrefix(name, NodeBridgePrefix)
}

// ValidateNics checks a VM's link NICs as the daemon receives them: eth1..ethN
// with no gaps, so declared names are the guest's names, and every tap bound
// for a bridge plasma owns. Topology resolution guarantees this already; the
// daemon checks again because the API is reachable without a topology file.
func ValidateNics(nics []NicSpec) error {
	for i, n := range nics {
		idx, ok := nicIndex(n.Name)
		if !ok {
			return fmt.Errorf("nic %q: want eth1, eth2, ... (eth0 is the management NIC)", n.Name)
		}
		if idx != i+1 {
			return fmt.Errorf("nic %q is at position %d; link NICs must be eth1..eth%d in order with no gaps",
				n.Name, i+1, len(nics))
		}
		switch {
		case n.Bridge != "" && n.HostName != "":
			return fmt.Errorf("nic %s: set bridge or host_name, not both", n.Name)
		case n.Bridge != "":
			if !isLinkBridge(n.Bridge) || len(n.Bridge) > maxIfaceName || !ifacePattern.MatchString(n.Bridge) {
				return fmt.Errorf("nic %s: bridge %q must be a %s* or %s* name of at most %d bytes",
					n.Name, n.Bridge, LinkBridgePrefix, NodeBridgePrefix, maxIfaceName)
			}
		case n.HostName != "":
			if err := checkHostIface(n.HostName); err != nil {
				return fmt.Errorf("nic %s: %w", n.Name, err)
			}
			if n.Address != "" {
				return fmt.Errorf("nic %s: a host tap gets no address", n.Name)
			}
		default:
			return fmt.Errorf("nic %s: needs a bridge or a host_name", n.Name)
		}
		if n.Address != "" {
			if p, err := netip.ParsePrefix(n.Address); err != nil || !p.Addr().Is4() {
				return fmt.Errorf("nic %s: address %q must be IPv4 with a prefix length, e.g. 10.77.0.1/24", n.Name, n.Address)
			}
		}
	}
	return nil
}

// checkHostIface vets the name a `host:` endpoint gives its tap. Plasma's own
// prefixes are refused so a host tap cannot be mistaken for, or collide
// with, an interface plasma generates and later deletes.
func checkHostIface(name string) error {
	if len(name) > maxIfaceName || !ifacePattern.MatchString(name) {
		return fmt.Errorf("host interface %q must be at most %d bytes of letters, digits, '_', '.' or '-'",
			name, maxIfaceName)
	}
	for _, p := range []string{"tap-", LinkBridgePrefix, NodeBridgePrefix, Bridge} {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("host interface %q uses plasma's reserved prefix %q", name, p)
		}
	}
	if nicTapPattern.MatchString(name) {
		return fmt.Errorf("host interface %q has the shape of a generated link tap (pt<octet>-...)", name)
	}
	return nil
}

// nicTapPattern matches what NicTapName generates.
var nicTapPattern = regexp.MustCompile(`^pt[0-9]+-`)

// linkNetworkCredential is a kernel command-line argument that tells the
// guest's systemd-networkd to leave the link NICs alone. The base image runs
// DHCP on every e* link; on a link segment there is no DHCP server, so
// network-online.target (which the app bootstrap waits for) would stall until
// wait-online times out, and the DHCP client would put its own frames on
// segments that should carry only what the VMs' applications send.
//
// systemd-network-generator turns network.network.* credentials into .network
// files under /run/systemd/network; "10-" sorts ahead of the image's
// 20-wired.network, so these links match it first. The applications configure
// link NICs themselves.
func linkNetworkCredential(nics []NicRecord) string {
	names := make([]string, len(nics))
	for i, n := range nics {
		names[i] = n.Name
	}
	unit := "[Match]\nName=" + strings.Join(names, " ") + "\n\n[Link]\nUnmanaged=yes\n"
	return "systemd.set_credential_binary=network.network.10-plasma-links:" +
		base64.StdEncoding.EncodeToString([]byte(unit))
}

// linkInfoCredential hands the guest what plasma decided about it: its VM ID
// and each link NIC's bridge (or host:<tap>) and address. systemd imports it
// as /run/credentials/@system/plasma.nics, one line each:
//
//	vm node-1
//	eth1 pbr-ctl 10.78.0.1/24
//	eth2 pbr-wire 10.77.0.1/24
//	eth3 host:a2it-cap -
//
// so an application reads its own addresses rather than having them written
// into its configuration for every node.
func linkInfoCredential(id string, nics []NicRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "vm %s\n", id)
	for _, n := range nics {
		to, addr := n.Bridge, n.Address
		if to == "" {
			to = "host:" + n.Tap
		}
		if addr == "" {
			addr = "-"
		}
		fmt.Fprintf(&b, "%s %s %s\n", n.Name, to, addr)
	}
	return "systemd.set_credential_binary=plasma.nics:" + base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// addressConflict reports another VM already holding one of nics' addresses on
// the same bridge. Topology resolution allocates without clashes within one
// file; this catches a second file, or an API caller, reusing an address.
func addressConflict(nics []NicRecord, vms []VmRecord) error {
	for _, n := range nics {
		if n.Address == "" {
			continue
		}
		mine, _ := netip.ParsePrefix(n.Address)
		for _, v := range vms {
			for _, o := range v.Nics {
				if o.Bridge != n.Bridge || o.Address == "" {
					continue
				}
				if theirs, err := netip.ParsePrefix(o.Address); err == nil && theirs.Addr() == mine.Addr() {
					return fmt.Errorf("%s: address %s is already %s's %s on %s",
						n.Name, mine.Addr(), v.ID, o.Name, n.Bridge)
				}
			}
		}
	}
	return nil
}

// ipOctet returns the last octet of a VM's management address.
func ipOctet(ip string) (int, error) {
	i := strings.LastIndex(ip, ".")
	if i < 0 {
		return 0, fmt.Errorf("bad VM address %q", ip)
	}
	return strconv.Atoi(ip[i+1:])
}
