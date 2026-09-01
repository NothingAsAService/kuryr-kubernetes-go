package policy

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestSubtractCIDRsWithoutExcepts(t *testing.T) {
	got, err := SubtractCIDRs("10.0.0.0/8", nil)
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	want := []string{"10.0.0.0/8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubtractCIDRsSingleExcept(t *testing.T) {
	// 10.0.0.0/8 minus 10.128.0.0/9 is exactly the other half.
	got, err := SubtractCIDRs("10.0.0.0/8", []string{"10.128.0.0/9"})
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	want := []string{"10.0.0.0/9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubtractCIDRsFullyExcepted(t *testing.T) {
	got, err := SubtractCIDRs("10.0.0.0/8", []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing left", got)
	}
}

func TestSubtractCIDRsCoversExactly(t *testing.T) {
	// Whatever the split produces, the result must cover every address in
	// the base except those in the holes. Check that property directly
	// rather than pinning an exact decomposition.
	base := "192.168.0.0/16"
	excepts := []string{"192.168.5.0/24", "192.168.200.128/25"}

	got, err := SubtractCIDRs(base, excepts)
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}

	basePrefix := netip.MustParsePrefix(base)
	holes := make([]netip.Prefix, 0, len(excepts))
	for _, e := range excepts {
		holes = append(holes, netip.MustParsePrefix(e))
	}
	result := make([]netip.Prefix, 0, len(got))
	for _, g := range got {
		result = append(result, netip.MustParsePrefix(g))
	}

	inResult := func(addr netip.Addr) bool {
		for _, p := range result {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	inHole := func(addr netip.Addr) bool {
		for _, h := range holes {
			if h.Contains(addr) {
				return true
			}
		}
		return false
	}

	// Sample addresses across the base: excepted ones must be absent,
	// everything else present.
	for _, probe := range []string{
		"192.168.0.1", "192.168.4.255", "192.168.5.0", "192.168.5.255",
		"192.168.6.0", "192.168.200.127", "192.168.200.128", "192.168.200.255",
		"192.168.201.0", "192.168.255.255",
	} {
		addr := netip.MustParseAddr(probe)
		want := !inHole(addr)
		if got := inResult(addr); got != want {
			t.Errorf("address %s: covered = %v, want %v", probe, got, want)
		}
	}

	// Nothing produced may stray outside the base.
	for _, p := range result {
		if !basePrefix.Contains(p.Addr()) {
			t.Errorf("result %s lies outside the base %s", p, base)
		}
	}
}

func TestSubtractCIDRsIPv6(t *testing.T) {
	got, err := SubtractCIDRs("fd00::/64", []string{"fd00::/65"})
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	// RFC 5952 compresses the first longest run of zero groups, so the upper
	// half of fd00::/64 prints as fd00::8000:0:0:0 rather than
	// fd00:0:0:0:8000::.
	want := []string{"fd00::8000:0:0:0/65"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubtractCIDRsIgnoresExceptOutsideBase(t *testing.T) {
	// Kubernetes requires excepts inside the cidr. One that is not removes
	// nothing, and failing the whole policy over it would be worse.
	got, err := SubtractCIDRs("10.0.0.0/8", []string{"192.168.0.0/16"})
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	want := []string{"10.0.0.0/8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubtractCIDRsRejectsMixedFamilies(t *testing.T) {
	if _, err := SubtractCIDRs("10.0.0.0/8", []string{"fd00::/64"}); err == nil {
		t.Fatal("expected an error when the except is a different address family")
	}
}

func TestSubtractCIDRsRejectsGarbage(t *testing.T) {
	if _, err := SubtractCIDRs("not-a-cidr", nil); err == nil {
		t.Fatal("expected an error for an unparseable cidr")
	}
	if _, err := SubtractCIDRs("10.0.0.0/8", []string{"nonsense"}); err == nil {
		t.Fatal("expected an error for an unparseable except")
	}
}

func TestSubtractCIDRsHostRoute(t *testing.T) {
	// Removing a single host from a /30 leaves three hosts, which can only
	// be expressed as separate prefixes.
	got, err := SubtractCIDRs("10.0.0.0/30", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("SubtractCIDRs: %v", err)
	}
	want := []string{"10.0.0.0/32", "10.0.0.2/31"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEtherTypeOf(t *testing.T) {
	if got, err := EtherTypeOf("10.0.0.0/8"); err != nil || got != "IPv4" {
		t.Errorf("EtherTypeOf(v4) = %q, %v; want IPv4", got, err)
	}
	if got, err := EtherTypeOf("fd00::/64"); err != nil || got != "IPv6" {
		t.Errorf("EtherTypeOf(v6) = %q, %v; want IPv6", got, err)
	}
	if _, err := EtherTypeOf("rubbish"); err == nil {
		t.Error("expected an error for an unparseable cidr")
	}
}

func TestHostCIDR(t *testing.T) {
	if got, err := HostCIDR("10.0.1.42"); err != nil || got != "10.0.1.42/32" {
		t.Errorf("HostCIDR(v4) = %q, %v; want 10.0.1.42/32", got, err)
	}
	if got, err := HostCIDR("fd00::42"); err != nil || got != "fd00::42/128" {
		t.Errorf("HostCIDR(v6) = %q, %v; want fd00::42/128", got, err)
	}
	if _, err := HostCIDR("not-an-ip"); err == nil {
		t.Error("expected an error for an unparseable address")
	}
}
