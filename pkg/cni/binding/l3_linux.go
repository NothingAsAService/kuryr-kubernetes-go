//go:build linux

package binding

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// ConfigureL3 assigns addresses and routes inside the pod namespace.
//
// The Python version did this with pyroute2 from kuryr_kubernetes/cni/binding/
// base.py. Two behaviours are preserved deliberately: IPv6 is force-enabled
// because some runtimes disable it on a fresh namespace, and an existing
// default route is left alone rather than overwritten.
func ConfigureL3(req Request) error {
	opts := Options{NetNSProcDir: netnsProcDirFromEnv()}
	return configureL3In(opts.ResolveNetNS(req.NetNS), req)
}

// ConfigureL3With is ConfigureL3 with explicit options, used by the daemon
// where the config is already loaded.
func ConfigureL3With(req Request, opts Options) error {
	return configureL3In(opts.ResolveNetNS(req.NetNS), req)
}

func configureL3In(nsPath string, req Request) error {
	needsIPv6 := false
	for _, sub := range req.VIF.Network.Subnets {
		if ip, _, err := net.ParseCIDR(sub.CIDR); err == nil && ip.To4() == nil {
			needsIPv6 = true
			break
		}
	}

	return ns.WithNetNSPath(nsPath, func(ns.NetNS) error {
		if needsIPv6 {
			if err := enableIPv6(); err != nil {
				return err
			}
		}

		link, err := netlink.LinkByName(req.IfName)
		if err != nil {
			return fmt.Errorf("looking up %s in the pod namespace: %w", req.IfName, err)
		}

		if err := addAddresses(link, req); err != nil {
			return err
		}

		// Addresses must be on the link before routes can resolve through it.
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bringing up %s: %w", req.IfName, err)
		}

		return addRoutes(link, req)
	})
}

// addAddresses assigns every address the VIF carries to the interface.
//
// It runs inside the pod namespace. An address that is already there is not an
// error: the runtime can replay a CNI ADD, and the second pass has to be a
// no-op rather than a failure.
func addAddresses(link netlink.Link, req Request) error {
	for _, sub := range req.VIF.Network.Subnets {
		_, cidr, err := net.ParseCIDR(sub.CIDR)
		if err != nil {
			return fmt.Errorf("subnet %s has an unparseable CIDR %q: %w", sub.ID, sub.CIDR, err)
		}
		ones, bits := cidr.Mask.Size()

		for _, raw := range sub.IPs {
			ip := net.ParseIP(raw)
			if ip == nil {
				return fmt.Errorf("subnet %s has an unparseable address %q", sub.ID, raw)
			}
			addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, bits)}}
			if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("adding %s to %s: %w", addr, req.IfName, err)
			}
		}
	}
	return nil
}

// addRoutes installs the per-subnet routes and, if this interface is the
// default gateway, the default route.
//
// It runs inside the pod namespace, after addAddresses and LinkSetUp: a route
// cannot resolve through an interface that is down or has no address on the
// route's network.
func addRoutes(link netlink.Link, req Request) error {
	for _, sub := range req.VIF.Network.Subnets {
		for _, r := range sub.Routes {
			_, dst, err := net.ParseCIDR(r.Destination)
			if err != nil {
				return fmt.Errorf("route destination %q is unparseable: %w", r.Destination, err)
			}
			gw := net.ParseIP(r.NextHop)
			if gw == nil {
				return fmt.Errorf("route next hop %q is unparseable", r.NextHop)
			}
			route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Gw: gw}
			if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, syscall.EEXIST) {
				return fmt.Errorf("adding route %s via %s: %w", r.Destination, r.NextHop, err)
			}
		}

		if !req.IsDefaultGateway || sub.Gateway == "" {
			continue
		}
		gw := net.ParseIP(sub.Gateway)
		if gw == nil {
			return fmt.Errorf("subnet %s has an unparseable gateway %q", sub.ID, sub.Gateway)
		}
		def := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}
		if err := netlink.RouteAdd(def); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				// Another interface already owns the default route. The
				// Python version logged and moved on, and overwriting it
				// here would break multi-interface pods.
				continue
			}
			return fmt.Errorf("adding default route via %s: %w", sub.Gateway, err)
		}
	}
	return nil
}

// enableIPv6 clears the per-namespace IPv6 disable flag.
//
// Some runtimes hand out a namespace with IPv6 switched off, and an address
// assignment then fails with a confusing EACCES rather than anything that
// names IPv6.
func enableIPv6() error {
	const path = "/proc/sys/net/ipv6/conf/all/disable_ipv6"
	if err := os.WriteFile(path, []byte("0"), 0o644); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the pod needs IPv6 but this kernel has IPv6 compiled out")
		}
		return fmt.Errorf("enabling IPv6 in the pod namespace: %w", err)
	}
	return nil
}

// netnsProcDirFromEnv lets the CNI plugin path work without a full config
// load. The daemon uses ConfigureL3With instead.
func netnsProcDirFromEnv() string { return os.Getenv("KURYR_NETNS_PROC_DIR") }
