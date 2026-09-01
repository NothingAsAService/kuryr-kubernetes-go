// Package policy translates Kubernetes NetworkPolicy objects into Neutron
// security group rules.
//
// This replaces network_policy.py and network_policy_security_groups.py,
// about 1200 lines between them. The translation here is a pure function over
// an interface that resolves selectors, so the hard part - deciding which
// rules a policy means - can be tested without a cluster or a cloud.
package policy

import (
	"fmt"
	"net/netip"
	"slices"
)

// SubtractCIDRs returns the address ranges in base that are not in excepts.
//
// A NetworkPolicy ipBlock can carve holes out of a range with `except`, but a
// security group rule has no such concept: it matches one prefix or nothing.
// The only faithful translation is to express the remainder as a set of
// prefixes, which is what this does by splitting base in half and discarding
// halves that are fully excepted.
//
// Ignoring `except` and emitting the base prefix, which is the easy way out,
// would allow traffic the policy explicitly forbids. That is a security bug,
// so the extra work is worth it.
func SubtractCIDRs(base string, excepts []string) ([]string, error) {
	basePrefix, err := parsePrefix(base)
	if err != nil {
		return nil, fmt.Errorf("ipBlock cidr %q: %w", base, err)
	}

	var holes []netip.Prefix
	for _, e := range excepts {
		hole, err := parsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("ipBlock except %q: %w", e, err)
		}
		if hole.Addr().Is4() != basePrefix.Addr().Is4() {
			return nil, fmt.Errorf("ipBlock except %q is not the same address family as cidr %q", e, base)
		}
		if !basePrefix.Overlaps(hole) {
			// Kubernetes requires excepts to be inside the cidr. One that is
			// not simply removes nothing, so skip it rather than fail: a
			// policy should not stop working over a harmless mistake.
			continue
		}
		holes = append(holes, hole)
	}

	if len(holes) == 0 {
		return []string{basePrefix.String()}, nil
	}

	var out []netip.Prefix
	remainder(basePrefix, holes, &out)

	result := make([]string, 0, len(out))
	for _, p := range out {
		result = append(result, p.String())
	}
	slices.Sort(result)
	return result, nil
}

// remainder appends the parts of p not covered by holes.
func remainder(p netip.Prefix, holes []netip.Prefix, out *[]netip.Prefix) {
	// Fully excluded: nothing of p survives.
	for _, hole := range holes {
		if coversOrEquals(hole, p) {
			return
		}
	}

	// Untouched: keep p whole.
	overlapping := holes[:0:0]
	for _, hole := range holes {
		if p.Overlaps(hole) {
			overlapping = append(overlapping, hole)
		}
	}
	if len(overlapping) == 0 {
		*out = append(*out, p)
		return
	}

	// Partly covered: split and recurse. The split always terminates because
	// a host prefix cannot be split, and a host prefix that overlaps a hole
	// is covered by it.
	left, right, ok := split(p)
	if !ok {
		return
	}
	remainder(left, overlapping, out)
	remainder(right, overlapping, out)
}

// coversOrEquals reports whether outer contains all of inner.
func coversOrEquals(outer, inner netip.Prefix) bool {
	return outer.Bits() <= inner.Bits() && outer.Contains(inner.Addr())
}

// split halves a prefix. It fails only for a single-address prefix, which
// cannot be divided.
func split(p netip.Prefix) (netip.Prefix, netip.Prefix, bool) {
	maxBits := p.Addr().BitLen()
	if p.Bits() >= maxBits {
		return netip.Prefix{}, netip.Prefix{}, false
	}

	childBits := p.Bits() + 1
	left := netip.PrefixFrom(p.Addr(), childBits)

	// The right half starts one bit further along, at the address with the
	// new bit set.
	rightAddr := setBit(p.Addr(), p.Bits())
	right := netip.PrefixFrom(rightAddr, childBits)

	return left.Masked(), right.Masked(), true
}

// setBit returns addr with bit index n set, counting from the most
// significant bit.
func setBit(addr netip.Addr, n int) netip.Addr {
	bytes := addr.AsSlice()
	bytes[n/8] |= 1 << (7 - uint(n%8))

	out, ok := netip.AddrFromSlice(bytes)
	if !ok {
		// AsSlice always returns a valid length, so this cannot happen;
		// return the original rather than panic if it somehow does.
		return addr
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return p.Masked(), nil
}

// EtherTypeOf reports the address family of a CIDR, in the form Neutron
// expects on a security group rule.
func EtherTypeOf(cidr string) (string, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("cidr %q: %w", cidr, err)
	}
	if p.Addr().Is4() {
		return "IPv4", nil
	}
	return "IPv6", nil
}

// HostCIDR turns a single address into a prefix covering just it, which is
// how a pod IP becomes a security group rule.
func HostCIDR(ip string) (string, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("address %q: %w", ip, err)
	}
	return netip.PrefixFrom(addr, addr.BitLen()).String(), nil
}
