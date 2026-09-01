//go:build linux

package binding

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// unixEEXIST is the error netlink returns when a VLAN id is already taken on
// an uplink.
var unixEEXIST = syscall.EEXIST

// kubeletPort is the port kubelet serves its read/write API on. The address
// it binds is, on almost every node, the uplink Kuryr needs.
const kubeletPort = 10250

// detectUplink finds the node interface a nested VIF should hang off.
//
// The Python implementation tried the configured name, then the interface
// holding kubelet's listen address, then the first non-loopback interface.
// The last of those is a coin flip on a node with several NICs, so a default
// route check is inserted ahead of it here: the interface that reaches the
// rest of the world is the one carrying the trunk.
func detectUplink(configured string) (netlink.Link, error) {
	if configured != "" {
		link, err := netlink.LinkByName(configured)
		if err != nil {
			return nil, fmt.Errorf("[binding]link_iface is %q but that interface does not exist: %w",
				configured, err)
		}
		return link, nil
	}

	if link := uplinkByKubelet(); link != nil {
		return link, nil
	}
	if link := uplinkByDefaultRoute(); link != nil {
		return link, nil
	}
	if link := firstUsableLink(); link != nil {
		return link, nil
	}

	return nil, fmt.Errorf(
		"cannot determine the node uplink interface; set [binding]link_iface")
}

// uplinkByKubelet finds the interface holding the address kubelet listens on.
func uplinkByKubelet() netlink.Link {
	addrs := listeningAddresses(kubeletPort)
	if len(addrs) == 0 {
		return nil
	}

	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	for _, link := range links {
		if link.Attrs().Flags&net.FlagLoopback != 0 {
			continue
		}
		linkAddrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		for _, la := range linkAddrs {
			for _, want := range addrs {
				if la.IP.Equal(want) {
					return link
				}
			}
		}
	}
	return nil
}

// listeningAddresses parses /proc/net/tcp and /proc/net/tcp6 for sockets
// listening on a port, returning the local addresses.
//
// Reading proc directly avoids shelling out to ss or lsof, neither of which
// is guaranteed to be present in the daemon image.
func listeningAddresses(port int) []net.IP {
	const tcpStateListen = "0A"

	var out []net.IP
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Scan() // header
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 4 || fields[3] != tcpStateListen {
				continue
			}
			ip, p, ok := parseProcAddr(fields[1])
			if !ok || p != port {
				continue
			}
			if ip.IsUnspecified() {
				// Listening on all interfaces tells us nothing about which
				// one is the uplink.
				continue
			}
			out = append(out, ip)
		}
		_ = f.Close()
	}
	return out
}

// parseProcAddr decodes the HEXADDR:HEXPORT form used in /proc/net/tcp.
func parseProcAddr(s string) (net.IP, int, bool) {
	host, portStr, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port, err := strconv.ParseInt(portStr, 16, 32)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return nil, 0, false
	}

	// Addresses are stored as little-endian 32-bit words.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+3] = raw[i+3], raw[i]
		raw[i+1], raw[i+2] = raw[i+2], raw[i+1]
	}
	switch len(raw) {
	case 4, 16:
		return net.IP(raw), int(port), true
	default:
		return nil, 0, false
	}
}

// uplinkByDefaultRoute returns the interface carrying the default route.
func uplinkByDefaultRoute() netlink.Link {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil
	}
	for _, r := range routes {
		if r.Dst != nil {
			continue // not a default route
		}
		if r.LinkIndex <= 0 {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			continue
		}
		if link.Attrs().Flags&net.FlagLoopback != 0 {
			continue
		}
		return link
	}
	return nil
}

// firstUsableLink is the last resort: the first non-loopback interface that
// is up and has an address.
func firstUsableLink() netlink.Link {
	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	for _, link := range links {
		attrs := link.Attrs()
		if attrs.Flags&net.FlagLoopback != 0 || attrs.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil || len(addrs) == 0 {
			continue
		}
		return link
	}
	return nil
}

// cleanupConflictingVLAN finds and removes an interface that has taken the
// VLAN id we need.
//
// This happens when the runtime starts a new pod before reaping the network
// namespace of the one it replaced. The offending interface is in some other
// namespace, so every namespace on the node has to be searched.
func cleanupConflictingVLAN(opts Options, uplinkIndex, vlanID int) error {
	paths, err := netnsPaths(opts)
	if err != nil {
		return err
	}

	seen := map[[2]uint64]bool{}
	for _, path := range paths {
		// Namespaces are identified by (device, inode): several /proc entries
		// point at the same namespace, and entering it repeatedly is wasted
		// work at best.
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		id := [2]uint64{st.Dev, st.Ino}
		if seen[id] {
			continue
		}
		seen[id] = true

		removed := false
		err = ns.WithNetNSPath(path, func(ns.NetNS) error {
			links, err := netlink.LinkList()
			if err != nil {
				return err
			}
			for _, link := range links {
				vlan, ok := link.(*netlink.Vlan)
				if !ok {
					continue
				}
				if vlan.VlanId == vlanID && vlan.ParentIndex == uplinkIndex {
					if err := netlink.LinkDel(link); err != nil {
						return err
					}
					removed = true
					return nil
				}
			}
			return nil
		})
		if err != nil {
			// A namespace can vanish mid-scan, which is expected.
			continue
		}
		if removed {
			return nil
		}
	}

	return fmt.Errorf("no interface found holding vlan id %d on uplink index %d",
		vlanID, uplinkIndex)
}

// netnsPaths lists every network namespace on the node, from both the
// per-process view and the named namespaces cri-o and ip-netns maintain.
func netnsPaths(opts Options) ([]string, error) {
	var paths []string

	procDir := "/proc"
	if opts.NetNSProcDir != "" {
		procDir = opts.NetNSProcDir
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", procDir, err)
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid
		}
		paths = append(paths, filepath.Join(procDir, e.Name(), "ns", "net"))
	}

	for _, dir := range []string{"/var/run/netns", "/run/netns"} {
		named, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range named {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}

	return paths, nil
}
