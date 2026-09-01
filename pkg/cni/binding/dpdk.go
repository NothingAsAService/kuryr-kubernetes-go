package binding

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

func init() {
	Register(kuryrv1.VIFTypeNestedDPDK, func(o Options) (Driver, error) {
		if o.DPDKDriver == "" {
			return nil, fmt.Errorf("[nested_dpdk]dpdk_driver is required by the nested-DPDK " +
				"binding driver")
		}
		if o.DPDKPCIMountPoint == "" {
			return nil, fmt.Errorf("[nested_dpdk]pci_mount_point is required by the " +
				"nested-DPDK binding driver; it is the directory the pod reads its PCI " +
				"address from")
		}
		return &dpdkDriver{opts: o, sysRoot: "/sys"}, nil
	})
}

// dpdkDriver hands a virtio device straight to a userspace dataplane in the
// pod.
//
// Nothing is moved into the pod network namespace here, and that is the
// point: the device is unbound from the kernel driver entirely and rebound to
// a userspace one, so the kernel no longer has a network interface for it.
// What the pod gets instead is the PCI address, written to a file in a shared
// mount, which its DPDK application uses to claim the device.
//
// The device is found by MAC address, because that is the only identifier the
// controller and the node agree on: the interface name is assigned by the
// node's kernel and the PCI address is not known until the device appears.
type dpdkDriver struct {
	opts Options
	// sysRoot is where sysfs is mounted. It is a field so the driver can be
	// tested against a directory tree instead of a real machine.
	sysRoot string
}

// Name is the registered driver name, used in logs and metrics.
func (d *dpdkDriver) Name() string { return "nested-dpdk" }

// Connect unbinds the device from its kernel driver, rebinds it to a userspace
// one, and writes the PCI address where the pod can read it. There is no
// interface to move into the namespace: a DPDK workload drives the device
// itself.
func (d *dpdkDriver) Connect(ctx context.Context, req Request) error {
	logger := log.FromContext(ctx)

	ifname, err := interfaceNameByMAC(req.VIF.Address)
	if err != nil {
		return fmt.Errorf("VIF %s: %w", req.VIF.ID, err)
	}

	pciAddr, kernelDriver, err := d.deviceInfo(ifname)
	if err != nil {
		return fmt.Errorf("VIF %s on interface %s: %w", req.VIF.ID, ifname, err)
	}

	if kernelDriver != d.opts.DPDKDriver {
		if err := d.rebind(pciAddr, kernelDriver, d.opts.DPDKDriver); err != nil {
			return fmt.Errorf("VIF %s: %w", req.VIF.ID, err)
		}
		logger.Info("device rebound to the userspace driver",
			"vif", req.VIF.ID, "pci", pciAddr, "from", kernelDriver, "to", d.opts.DPDKDriver)
	}

	if err := d.writePCIFile(req, pciAddr); err != nil {
		return err
	}

	// Record what was done on the VIF. The caller persists it, which is what
	// makes the PCI address visible in `kubectl get kuryrport` rather than
	// only in a file inside the pod.
	if req.VIF.DPDK == nil {
		req.VIF.DPDK = &kuryrv1.DPDKConfig{}
	}
	req.VIF.DPDK.PCIAddress = pciAddr
	// The driver the device came from is what a later unbind has to restore,
	// so it is the interesting one to keep.
	req.VIF.DPDK.DevDriver = kernelDriver

	return nil
}

// Disconnect removes the PCI address file the pod was given.
func (d *dpdkDriver) Disconnect(ctx context.Context, req Request) error {
	// The device is not rebound to the kernel driver here. The node VM is
	// about to have the port removed by the controller, and rebinding a
	// device that is being detached races with that. The PCI file is what
	// the pod depended on, so that is what goes.
	pciFile := d.pciFilePath(req)
	if err := os.Remove(pciFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the PCI address file %s of VIF %s: %w",
			pciFile, req.VIF.ID, err)
	}
	return nil
}

// deviceInfo returns the PCI address of an interface and the driver currently
// bound to it.
//
// The path is /sys/class/net/<if>/device -> ../../../<pci addr>, and the
// driver is a further symlink from the PCI device. Reading the links rather
// than parsing anything keeps this exactly as reliable as sysfs itself.
func (d *dpdkDriver) deviceInfo(ifname string) (pciAddr, driver string, err error) {
	deviceLink := path.Join(d.sysRoot, "class/net", ifname, "device")
	target, err := os.Readlink(deviceLink)
	if err != nil {
		return "", "", fmt.Errorf("reading %s to find the device behind the interface: %w",
			deviceLink, err)
	}

	// The link points at the device's own sysfs directory. Its name is the
	// PCI address for a PCI device, and the virtio name for a virtio one, in
	// which case the PCI address is one level up.
	name := path.Base(target)
	if strings.HasPrefix(name, "virtio") {
		virtioLink := path.Join(d.sysRoot, "bus/virtio/devices", name)
		virtioTarget, err := os.Readlink(virtioLink)
		if err != nil {
			return "", "", fmt.Errorf("reading %s to find the PCI address of the virtio "+
				"device: %w", virtioLink, err)
		}
		// .../pci0000:00/0000:00:04.0/virtio1 -> the parent is the address.
		pciAddr = path.Base(path.Dir(virtioTarget))
	} else {
		pciAddr = name
	}
	if pciAddr == "" || pciAddr == "." || pciAddr == "/" {
		return "", "", fmt.Errorf("could not work out the PCI address of interface %s from %s",
			ifname, target)
	}

	driver, err = d.boundDriver(pciAddr)
	if err != nil {
		return "", "", err
	}
	return pciAddr, driver, nil
}

// boundDriver returns the driver currently bound to a PCI device.
func (d *dpdkDriver) boundDriver(pciAddr string) (string, error) {
	link := path.Join(d.sysRoot, "bus/pci/devices", pciAddr, "driver")
	target, err := os.Readlink(link)
	if err != nil {
		if os.IsNotExist(err) {
			// A device with no driver bound at all is a valid state, and one
			// the rebind below handles by simply binding.
			return "", nil
		}
		return "", fmt.Errorf("reading %s to find the driver bound to %s: %w", link, pciAddr, err)
	}
	return path.Base(target), nil
}

// rebind moves a PCI device from one driver to another.
//
// The sequence is the one dpdk-devbind.py uses, and the order matters: the
// device is unbound first, then driver_override is cleared with a NUL and set
// to the new driver, and only then is the bind triggered. Setting the
// override without clearing it first is silently ignored by some kernels.
func (d *dpdkDriver) rebind(pciAddr, from, to string) error {
	if from != "" {
		unbind := path.Join(d.sysRoot, "bus/pci/drivers", from, "unbind")
		if err := writeSysfs(unbind, pciAddr); err != nil {
			return fmt.Errorf("unbinding %s from %s: %w", pciAddr, from, err)
		}
	}

	override := path.Join(d.sysRoot, "bus/pci/devices", pciAddr, "driver_override")
	if err := writeSysfs(override, "\x00"); err != nil {
		return fmt.Errorf("clearing the driver override of %s: %w", pciAddr, err)
	}
	if err := writeSysfs(override, to); err != nil {
		return fmt.Errorf("setting the driver override of %s to %s: %w", pciAddr, to, err)
	}

	bind := path.Join(d.sysRoot, "bus/pci/drivers", to, "bind")
	if err := writeSysfs(bind, pciAddr); err != nil {
		return fmt.Errorf("binding %s to %s: %w", pciAddr, to, err)
	}
	return nil
}

// writePCIFile tells the pod which device it was given.
func (d *dpdkDriver) writePCIFile(req Request, pciAddr string) error {
	if err := os.MkdirAll(d.opts.DPDKPCIMountPoint, 0o755); err != nil {
		return fmt.Errorf("creating the PCI address directory %s: %w",
			d.opts.DPDKPCIMountPoint, err)
	}

	pciFile := d.pciFilePath(req)
	if err := os.WriteFile(pciFile, []byte(pciAddr), 0o644); err != nil {
		return fmt.Errorf("writing the PCI address of VIF %s to %s: %w",
			req.VIF.ID, pciFile, err)
	}
	return nil
}

func (d *dpdkDriver) pciFilePath(req Request) string {
	return path.Join(d.opts.DPDKPCIMountPoint, req.ContainerID+"-"+req.IfName)
}

// writeSysfs writes one value to a sysfs attribute.
//
// sysfs attributes take a single write and reject anything appended, so the
// file is opened write-only without truncation and written once.
func writeSysfs(p, value string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", p, err)
	}
	if _, err := f.WriteString(value); err != nil {
		// Report the write error, but still close: leaking the descriptor
		// would matter more on a daemon that runs for months.
		if closeErr := f.Close(); closeErr != nil {
			return fmt.Errorf("writing %q to %s: %w (and closing it failed: %w)",
				value, p, err, closeErr)
		}
		return fmt.Errorf("writing %q to %s: %w", value, p, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s after writing %q: %w", p, value, err)
	}
	return nil
}

// interfaceNameByMAC finds the interface carrying an address.
func interfaceNameByMAC(mac string) (string, error) {
	want, err := net.ParseMAC(mac)
	if err != nil {
		return "", fmt.Errorf("unparseable MAC address %q: %w", mac, err)
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("listing the node's interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.HardwareAddr.String() == want.String() {
			return iface.Name, nil
		}
	}
	return "", fmt.Errorf("no interface on this node has the address %s; the port may not "+
		"be attached to the node VM yet", mac)
}
