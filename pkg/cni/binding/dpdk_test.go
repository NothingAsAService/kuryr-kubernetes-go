package binding

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// PCI addresses really look like 0000:00:04.0, but Windows refuses to create
// a directory containing a colon. The driver never parses the address, it
// only reads it as a directory name, so an underscored stand-in exercises
// exactly the same code and keeps these tests runnable everywhere.

// fakeSysfs builds the part of /sys the DPDK driver reads.
//
// The layout mirrors a real machine: an interface points at its device, a
// virtio device points at its PCI parent, and a PCI device points at the
// driver bound to it. Reproducing it with directories and symlinks is what
// lets the sysfs walking be tested without a DPDK-capable host.
type fakeSysfs struct {
	root string
}

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	return &fakeSysfs{root: t.TempDir()}
}

func (s *fakeSysfs) mkdir(t *testing.T, parts ...string) {
	t.Helper()
	p := filepath.Join(append([]string{s.root}, parts...)...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("creating %s: %v", p, err)
	}
}

func (s *fakeSysfs) symlink(t *testing.T, target, linkParts string) {
	t.Helper()
	link := filepath.Join(s.root, linkParts)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("creating the parent of %s: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		// Windows refuses symlinks without developer mode or elevation.
		// The code under test is the same either way, so skipping here is
		// honest: this assertion has not run.
		t.Skipf("cannot create symlinks on this machine: %v", err)
	}
}

// writeFile creates an empty sysfs attribute and returns its path. The
// content is always empty: these are the files the code under test writes to,
// and the assertions read back what it wrote.
func (s *fakeSysfs) writeFile(t *testing.T, parts string) string {
	t.Helper()
	p := filepath.Join(s.root, parts)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating the parent of %s: %v", p, err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	return p
}

// virtioDevice wires up an interface backed by a virtio device on a PCI
// address, bound to a kernel driver.
func (s *fakeSysfs) virtioDevice(t *testing.T, ifname, virtio, pci, driver string) {
	t.Helper()
	s.mkdir(t, "devices/pci0000_00", pci, virtio)
	s.symlink(t, "../../../devices/pci0000_00/"+pci+"/"+virtio, "class/net/"+ifname+"/device")
	s.symlink(t, "../../../devices/pci0000_00/"+pci+"/"+virtio, "bus/virtio/devices/"+virtio)
	s.mkdir(t, "bus/pci/devices", pci)
	if driver != "" {
		s.mkdir(t, "bus/pci/drivers", driver)
		s.symlink(t, "../../../bus/pci/drivers/"+driver, "bus/pci/devices/"+pci+"/driver")
	}
}

func newDPDKDriver(t *testing.T, sys *fakeSysfs) (*dpdkDriver, string) {
	t.Helper()
	pciMount := t.TempDir()
	return &dpdkDriver{
		opts: Options{
			DPDKDriver:        "uio_pci_generic",
			DPDKPCIMountPoint: pciMount,
		},
		sysRoot: sys.root,
	}, pciMount
}

func TestDPDKFindsThePCIAddressOfAVirtioDevice(t *testing.T) {
	sys := newFakeSysfs(t)
	sys.virtioDevice(t, "eth1", "virtio1", "0000_00_04.0", "virtio-pci")
	d, _ := newDPDKDriver(t, sys)

	pci, driver, err := d.deviceInfo("eth1")
	if err != nil {
		t.Fatalf("deviceInfo: %v", err)
	}
	if pci != "0000_00_04.0" {
		t.Errorf("PCI address = %q, want 0000_00_04.0", pci)
	}
	if driver != "virtio-pci" {
		t.Errorf("driver = %q, want virtio-pci", driver)
	}
}

func TestDPDKFindsThePCIAddressOfAPlainPCIDevice(t *testing.T) {
	// Not every device is virtio; a passed-through NIC points straight at
	// its PCI directory.
	sys := newFakeSysfs(t)
	sys.mkdir(t, "devices/pci0000_00/0000_00_05.0")
	sys.symlink(t, "../../../devices/pci0000_00/0000_00_05.0", "class/net/eth2/device")
	sys.mkdir(t, "bus/pci/devices/0000_00_05.0")
	sys.mkdir(t, "bus/pci/drivers/ixgbe")
	sys.symlink(t, "../../../bus/pci/drivers/ixgbe", "bus/pci/devices/0000_00_05.0/driver")

	d, _ := newDPDKDriver(t, sys)
	pci, driver, err := d.deviceInfo("eth2")
	if err != nil {
		t.Fatalf("deviceInfo: %v", err)
	}
	if pci != "0000_00_05.0" || driver != "ixgbe" {
		t.Errorf("deviceInfo = %q, %q; want 0000_00_05.0, ixgbe", pci, driver)
	}
}

func TestDPDKReportsADeviceWithNoDriverBound(t *testing.T) {
	// A device between drivers is a valid state, not a failure: rebinding
	// simply skips the unbind.
	sys := newFakeSysfs(t)
	sys.virtioDevice(t, "eth1", "virtio1", "0000_00_04.0", "")
	d, _ := newDPDKDriver(t, sys)

	pci, driver, err := d.deviceInfo("eth1")
	if err != nil {
		t.Fatalf("deviceInfo: %v", err)
	}
	if pci != "0000_00_04.0" {
		t.Errorf("PCI address = %q, want 0000_00_04.0", pci)
	}
	if driver != "" {
		t.Errorf("driver = %q, want empty for an unbound device", driver)
	}
}

func TestDPDKReportsAnUnknownInterface(t *testing.T) {
	sys := newFakeSysfs(t)
	d, _ := newDPDKDriver(t, sys)

	if _, _, err := d.deviceInfo("nonexistent"); err == nil {
		t.Fatal("expected an error for an interface that has no device")
	}
}

func TestDPDKRebindFollowsTheDevbindSequence(t *testing.T) {
	// The order matters: unbind, clear the override with a NUL, set the new
	// driver, then bind. Setting the override without clearing it first is
	// silently ignored by some kernels.
	// rebind only writes to sysfs attributes, so no device symlinks are
	// needed here.
	sys := newFakeSysfs(t)
	unbind := sys.writeFile(t, "bus/pci/drivers/virtio-pci/unbind")
	override := sys.writeFile(t, "bus/pci/devices/0000_00_04.0/driver_override")
	sys.mkdir(t, "bus/pci/drivers/uio_pci_generic")
	bind := sys.writeFile(t, "bus/pci/drivers/uio_pci_generic/bind")

	d, _ := newDPDKDriver(t, sys)
	if err := d.rebind("0000_00_04.0", "virtio-pci", "uio_pci_generic"); err != nil {
		t.Fatalf("rebind: %v", err)
	}

	assertFile(t, unbind, "0000_00_04.0")
	assertFile(t, override, "uio_pci_generic")
	assertFile(t, bind, "0000_00_04.0")
}

func TestDPDKRebindSkipsTheUnbindWhenNothingIsBound(t *testing.T) {
	sys := newFakeSysfs(t)
	override := sys.writeFile(t, "bus/pci/devices/0000_00_04.0/driver_override")
	sys.mkdir(t, "bus/pci/drivers/uio_pci_generic")
	bind := sys.writeFile(t, "bus/pci/drivers/uio_pci_generic/bind")

	d, _ := newDPDKDriver(t, sys)
	if err := d.rebind("0000_00_04.0", "", "uio_pci_generic"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	assertFile(t, override, "uio_pci_generic")
	assertFile(t, bind, "0000_00_04.0")
}

func TestDPDKRebindReportsAMissingDriver(t *testing.T) {
	// Writing to a driver that does not exist has to be an error, or the pod
	// would start with a device bound to nothing.
	sys := newFakeSysfs(t)
	sys.writeFile(t, "bus/pci/drivers/virtio-pci/unbind")
	sys.writeFile(t, "bus/pci/devices/0000_00_04.0/driver_override")

	d, _ := newDPDKDriver(t, sys)
	if err := d.rebind("0000_00_04.0", "virtio-pci", "uio_pci_generic"); err == nil {
		t.Fatal("expected an error when the target driver is not present")
	}
}

func TestDPDKWritesAndRemovesThePCIFile(t *testing.T) {
	// The PCI file is the whole contract with the pod: the DPDK application
	// reads it to claim the device.
	sys := newFakeSysfs(t)
	d, pciMount := newDPDKDriver(t, sys)
	req := Request{
		VIF:         &kuryrv1.VIF{ID: "port-uuid", Type: kuryrv1.VIFTypeNestedDPDK},
		IfName:      "eth0",
		ContainerID: "container-1",
	}

	if err := d.writePCIFile(req, "0000_00_04.0"); err != nil {
		t.Fatalf("writePCIFile: %v", err)
	}
	assertFile(t, filepath.Join(pciMount, "container-1-eth0"), "0000_00_04.0")

	if err := d.Disconnect(context.Background(), req); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pciMount, "container-1-eth0")); !os.IsNotExist(err) {
		t.Errorf("the PCI file survived Disconnect: %v", err)
	}
}

func TestDPDKDisconnectIsIdempotent(t *testing.T) {
	sys := newFakeSysfs(t)
	d, _ := newDPDKDriver(t, sys)
	req := Request{
		VIF:         &kuryrv1.VIF{ID: "port-uuid", Type: kuryrv1.VIFTypeNestedDPDK},
		IfName:      "eth0",
		ContainerID: "container-1",
	}

	if err := d.Disconnect(context.Background(), req); err != nil {
		t.Errorf("Disconnect with nothing to remove: %v", err)
	}
	if err := d.Disconnect(context.Background(), req); err != nil {
		t.Errorf("second Disconnect: %v", err)
	}
}

func TestDPDKDriverNeedsConfiguration(t *testing.T) {
	vifDPDK := &kuryrv1.VIF{Type: kuryrv1.VIFTypeNestedDPDK}

	if _, err := For(vifDPDK, Options{DPDKPCIMountPoint: "/var/pci_address"}); err == nil {
		t.Error("expected an error when [nested_dpdk]dpdk_driver is unset")
	}
	if _, err := For(vifDPDK, Options{DPDKDriver: "uio_pci_generic"}); err == nil {
		t.Error("expected an error when [nested_dpdk]pci_mount_point is unset")
	}
	if _, err := For(vifDPDK, Options{
		DPDKDriver:        "uio_pci_generic",
		DPDKPCIMountPoint: "/var/pci_address",
	}); err != nil {
		t.Errorf("a fully configured DPDK driver was refused: %v", err)
	}
}

func TestInterfaceNameByMACRejectsGarbage(t *testing.T) {
	if _, err := interfaceNameByMAC("not-a-mac"); err == nil {
		t.Error("expected an error for an unparseable MAC address")
	}
	// A well-formed address that no interface carries has to be reported
	// clearly: it usually means the port is not attached to the node yet.
	if _, err := interfaceNameByMAC("fa:16:3e:00:00:01"); err == nil {
		t.Error("expected an error for a MAC address no interface has")
	}
}

func assertFile(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	if string(got) != want {
		t.Errorf("%s contains %q, want %q", p, got, want)
	}
}
