package binding

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
)

// vhostUserFixture is a driver with real directories behind it.
type vhostUserFixture struct {
	driver     *vhostUserDriver
	mountPoint string
	ovsPath    string
	// added and deleted record the OVS port calls, in place of ovs-vsctl.
	added, deleted []string
}

func newVHostUserFixture(t *testing.T) *vhostUserFixture {
	t.Helper()
	mountPoint := t.TempDir()
	ovsPath := t.TempDir()

	f := &vhostUserFixture{
		driver: &vhostUserDriver{opts: Options{
			VHostUserMountPoint: mountPoint,
			OVSVhuPath:          ovsPath,
			OVSBridge:           "br-int",
		}},
		mountPoint: mountPoint,
		ovsPath:    ovsPath,
	}
	f.driver.addPort = func(_ context.Context, bridge string, req Request) error {
		f.added = append(f.added, bridge+"/"+req.VIF.ID)
		return nil
	}
	f.driver.delPort = func(_ context.Context, bridge string, req Request) error {
		f.deleted = append(f.deleted, bridge+"/"+req.VIF.ID)
		return nil
	}
	return f
}

func (f *vhostUserFixture) request(mode string) Request {
	// The socket path is what the controller computed: a file inside the
	// mount the pod shares.
	socket := f.mountPoint + "/vhu0123456789"
	return Request{
		VIF: &kuryrv1.VIF{
			ID:      "port-uuid",
			Type:    kuryrv1.VIFTypeVHostUser,
			Address: "fa:16:3e:11:22:33",
			VHostUser: &kuryrv1.VHostUserConfig{
				Path: socket,
				Mode: mode,
			},
		},
		IfName:      "eth0",
		ContainerID: "container-1",
	}
}

// listenSocket creates a real Unix socket, which is what the driver checks
// for before moving anything.
func listenSocket(t *testing.T, p string) {
	t.Helper()
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	t.Cleanup(func() {
		// Closing the listener may remove the file; both outcomes are fine.
		if err := l.Close(); err != nil {
			t.Logf("closing the test socket: %v", err)
		}
	})
}

func TestVHostUserClientModeMovesTheSocket(t *testing.T) {
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeClient)

	source := filepath.Join(f.ovsPath, "vhu0123456789")
	listenSocket(t, source)

	if err := f.driver.Connect(context.Background(), req); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The socket has to end up where the pod can see it, because Open vSwitch
	// cannot be told to create it there.
	if _, err := os.Stat(req.VIF.VHostUser.Path); err != nil {
		t.Errorf("the socket was not moved into the shared mount: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Errorf("the socket is still in the Open vSwitch directory: %v", err)
	}

	// The config file is the contract with the application in the pod.
	configPath := filepath.Join(f.mountPoint, "container-1-eth0")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config file: %v", err)
	}
	var conf vhostUserConfig
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatalf("decoding the config file: %v", err)
	}
	if conf.VHostName != "vhu0123456789" {
		t.Errorf("vhostname = %q, want the port name", conf.VHostName)
	}
	if conf.VHostMAC != "fa:16:3e:11:22:33" {
		t.Errorf("vhostmac = %q, want the VIF address", conf.VHostMAC)
	}
	if conf.Mode != vhostUserModeClient {
		t.Errorf("mode = %q, want client", conf.Mode)
	}
}

func TestVHostUserServerModeMovesNothing(t *testing.T) {
	// In server mode the pod creates the socket and Open vSwitch connects to
	// it, so there is nothing on this side to move.
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeServer)

	if err := f.driver.Connect(context.Background(), req); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := os.Stat(filepath.Join(f.mountPoint, "container-1-eth0")); err != nil {
		t.Errorf("the config file was not written: %v", err)
	}
	if _, err := os.Stat(req.VIF.VHostUser.Path); !os.IsNotExist(err) {
		t.Error("a socket was created in server mode, where the pod owns it")
	}
}

func TestVHostUserConnectFailsWithoutTheSocket(t *testing.T) {
	// Reporting this is what turns "the pod silently has no network" into a
	// CNI error the runtime retries.
	f := newVHostUserFixture(t)

	err := f.driver.Connect(context.Background(), f.request(vhostUserModeClient))
	if err == nil {
		t.Fatal("expected an error when Open vSwitch has not created the socket")
	}
}

func TestVHostUserConnectRejectsARegularFile(t *testing.T) {
	f := newVHostUserFixture(t)
	source := filepath.Join(f.ovsPath, "vhu0123456789")
	if err := os.WriteFile(source, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("writing the decoy file: %v", err)
	}

	if err := f.driver.Connect(context.Background(), f.request(vhostUserModeClient)); err == nil {
		t.Fatal("expected an error when the path is not a socket")
	}
}

func TestVHostUserDisconnectRemovesBothFiles(t *testing.T) {
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeClient)

	source := filepath.Join(f.ovsPath, "vhu0123456789")
	listenSocket(t, source)
	if err := f.driver.Connect(context.Background(), req); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err := f.driver.Disconnect(context.Background(), req); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if _, err := os.Stat(req.VIF.VHostUser.Path); !os.IsNotExist(err) {
		t.Error("the socket survived Disconnect; Open vSwitch cannot remove it itself")
	}
	if _, err := os.Stat(filepath.Join(f.mountPoint, "container-1-eth0")); !os.IsNotExist(err) {
		t.Error("the config file survived Disconnect")
	}
}

func TestVHostUserDisconnectIsIdempotent(t *testing.T) {
	// The runtime retries DEL freely, so a second call must succeed.
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeClient)

	if err := f.driver.Disconnect(context.Background(), req); err != nil {
		t.Errorf("Disconnect with nothing to remove: %v", err)
	}
	if err := f.driver.Disconnect(context.Background(), req); err != nil {
		t.Errorf("second Disconnect: %v", err)
	}
}

func TestVHostUserDriverNeedsAMountPoint(t *testing.T) {
	// Without it the socket would be written somewhere the pod cannot reach,
	// which fails in a way that looks like a network fault.
	if _, err := For(&kuryrv1.VIF{Type: kuryrv1.VIFTypeVHostUser}, Options{}); err == nil {
		t.Fatal("expected an error when [vhostuser]mount_point is unset")
	}
}

func TestVHostUserConnectRejectsAVIFWithoutConfig(t *testing.T) {
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeClient)
	req.VIF.VHostUser = nil

	if err := f.driver.Connect(context.Background(), req); err == nil {
		t.Fatal("expected an error for a vhostuser VIF with no vhost-user configuration")
	}
}

// os-vif created the OVS port before Python's driver ran; without it there is
// no socket to move. Both connect and disconnect must manage it.
func TestVHostUserManagesTheOVSPort(t *testing.T) {
	f := newVHostUserFixture(t)
	req := f.request(vhostUserModeServer)

	if err := f.driver.Connect(context.Background(), req); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := f.driver.Disconnect(context.Background(), req); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if len(f.added) != 1 || f.added[0] != "br-int/port-uuid" {
		t.Errorf("added ports = %v, want [br-int/port-uuid]", f.added)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "br-int/port-uuid" {
		t.Errorf("deleted ports = %v, want [br-int/port-uuid]", f.deleted)
	}
}

func TestVHostUserPortColumnsFollowThePodMode(t *testing.T) {
	server := vhostUserPortColumns(&kuryrv1.VIF{VHostUser: &kuryrv1.VHostUserConfig{
		Mode: vhostUserModeServer, Path: "/var/cni/vhostuser/vhu1"}})
	if len(server) != 2 || server[0] != "type=dpdkvhostuserclient" ||
		server[1] != "options:vhost-server-path=/var/cni/vhostuser/vhu1" {
		t.Errorf("pod-as-server columns = %v, want an OVS client pointed at the pod socket", server)
	}
	client := vhostUserPortColumns(&kuryrv1.VIF{VHostUser: &kuryrv1.VHostUserConfig{Mode: vhostUserModeClient}})
	if len(client) != 1 || client[0] != "type=dpdkvhostuser" {
		t.Errorf("pod-as-client columns = %v, want an OVS-created socket", client)
	}
}
