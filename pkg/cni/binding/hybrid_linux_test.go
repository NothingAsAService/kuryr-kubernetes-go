//go:build linux

package binding

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// inScratchNetNS runs fn on a locked thread inside a new, empty network
// namespace, so the links it creates never touch the host. Needs root.
func inScratchNetNS(t *testing.T, fn func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces and links")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("reading the current netns: %v", err)
	}
	defer orig.Close()
	scratch, err := netns.New()
	if err != nil {
		t.Fatalf("creating a scratch netns: %v", err)
	}
	defer scratch.Close()
	defer func() { _ = netns.Set(orig) }()

	fn()
}

// fakeOVSVsctl puts an ovs-vsctl on PATH that only records its arguments.
func fakeOVSVsctl(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ovs-vsctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return log
}

// The plumbing os-vif built for a hybrid port, on a real kernel: the qbr
// bridge, qvb enslaved to it, qvo registered on the integration bridge, and
// all of it gone again after unplug.
func TestHybridPlugBuildsAndRemovesThePlumbing(t *testing.T) {
	calls := fakeOVSVsctl(t)
	inScratchNetNS(t, func() {
		const portID = "3f2a9c1e-0b44-4d5e-9a61-1234567890ab"
		req := Request{VIF: &kuryrv1.VIF{
			ID:      portID,
			Address: "fa:16:3e:00:00:01",
			Bridge:  kuryrv1.HybridBridgeName(portID),
			Network: kuryrv1.Network{MTU: 1450},
		}}
		ctx := context.Background()

		// Twice: a retried ADD must find everything in place and not fail.
		for i := 0; i < 2; i++ {
			if err := plugHybrid(ctx, "br-int", req); err != nil {
				t.Fatalf("plugHybrid pass %d: %v", i, err)
			}
		}

		qbr, err := netlink.LinkByName(req.VIF.Bridge)
		if err != nil {
			t.Fatalf("bridge %s missing: %v", req.VIF.Bridge, err)
		}
		if qbr.Type() != "bridge" || qbr.Attrs().Flags&1 == 0 {
			t.Errorf("%s: type %s flags %v, want an up bridge", qbr.Attrs().Name, qbr.Type(), qbr.Attrs().Flags)
		}
		qvbName, qvoName := hybridVethNames(portID)
		qvb, err := netlink.LinkByName(qvbName)
		if err != nil {
			t.Fatalf("%s missing: %v", qvbName, err)
		}
		if qvb.Attrs().MasterIndex != qbr.Attrs().Index {
			t.Errorf("%s is not enslaved to %s", qvbName, qbr.Attrs().Name)
		}
		qvo, err := netlink.LinkByName(qvoName)
		if err != nil {
			t.Fatalf("%s missing: %v", qvoName, err)
		}
		if qvo.Attrs().MTU != 1450 {
			t.Errorf("%s MTU = %d, want 1450", qvoName, qvo.Attrs().MTU)
		}

		if err := unplugHybrid(ctx, "br-int", req); err != nil {
			t.Fatalf("unplugHybrid: %v", err)
		}
		for _, name := range []string{req.VIF.Bridge, qvbName, qvoName} {
			if _, err := netlink.LinkByName(name); err == nil {
				t.Errorf("%s still exists after unplug", name)
			}
		}
		if err := unplugHybrid(ctx, "br-int", req); err != nil {
			t.Errorf("a second unplug must be a no-op: %v", err)
		}
	})

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("ovs-vsctl was never called: %v", err)
	}
	got := string(raw)
	_, qvo := hybridVethNames("3f2a9c1e-0b44-4d5e-9a61-1234567890ab")
	for _, want := range []string{
		"--may-exist add-port br-int " + qvo,
		"external_ids:iface-id=3f2a9c1e-0b44-4d5e-9a61-1234567890ab",
		"external_ids:attached-mac=fa:16:3e:00:00:01",
		"--if-exists del-port br-int " + qvo,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ovs-vsctl calls lack %q:\n%s", want, got)
		}
	}
}

// The whole hybrid ADD and DEL through the bridge driver: the pod end ends up
// in the pod namespace as eth0, the host end on qbr, and DEL leaves nothing.
func TestBridgeDriverConnectsAPodThroughTheHybridBridge(t *testing.T) {
	fakeOVSVsctl(t)
	inScratchNetNS(t, func() {
		pod, err := testutils.NewNS()
		if err != nil {
			t.Fatalf("creating the pod netns: %v", err)
		}
		defer func() { _ = testutils.UnmountNS(pod) }()

		const portID = "9b1d2c3e-4f50-4a6b-8c7d-0e1f2a3b4c5d"
		req := Request{
			VIF: &kuryrv1.VIF{
				ID:      portID,
				Type:    kuryrv1.VIFTypeBridge,
				Address: "fa:16:3e:aa:bb:cc",
				VIFName: kuryrv1.HostIfaceName(portID),
				Bridge:  kuryrv1.HybridBridgeName(portID),
				Network: kuryrv1.Network{MTU: 1400},
			},
			IfName: "eth0",
			NetNS:  pod.Path(),
		}
		d := &bridgeDriver{opts: Options{OVSBridge: "br-int"}}
		ctx := context.Background()

		if err := d.Connect(ctx, req); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		tap, err := netlink.LinkByName(req.VIF.VIFName)
		if err != nil {
			t.Fatalf("host end %s missing: %v", req.VIF.VIFName, err)
		}
		qbr, _ := netlink.LinkByName(req.VIF.Bridge)
		if qbr == nil || tap.Attrs().MasterIndex != qbr.Attrs().Index {
			t.Errorf("host end %s is not on %s", req.VIF.VIFName, req.VIF.Bridge)
		}
		err = pod.Do(func(ns.NetNS) error {
			eth0, err := netlink.LinkByName("eth0")
			if err != nil {
				return err
			}
			if got := eth0.Attrs().HardwareAddr.String(); got != req.VIF.Address {
				t.Errorf("pod eth0 MAC = %s, want %s", got, req.VIF.Address)
			}
			if eth0.Attrs().MTU != 1400 {
				t.Errorf("pod eth0 MTU = %d, want 1400", eth0.Attrs().MTU)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("pod eth0 missing: %v", err)
		}

		if err := d.Disconnect(ctx, req); err != nil {
			t.Fatalf("Disconnect: %v", err)
		}
		_, qvo := hybridVethNames(portID)
		for _, name := range []string{req.VIF.VIFName, req.VIF.Bridge, qvo} {
			if _, err := netlink.LinkByName(name); err == nil {
				t.Errorf("%s still exists after Disconnect", name)
			}
		}
	})
}
