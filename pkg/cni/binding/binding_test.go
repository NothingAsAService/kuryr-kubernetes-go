package binding

import (
	"testing"

	kuryrv1 "opendev.org/openstack/kuryr-kubernetes/api/v1"
	"opendev.org/openstack/kuryr-kubernetes/pkg/config"
)

func TestResolveNetNSPassesThroughWithoutDockerMode(t *testing.T) {
	opts := Options{}
	for _, path := range []string{"/proc/1234/ns/net", "/var/run/netns/cni-abc"} {
		if got := opts.ResolveNetNS(path); got != path {
			t.Errorf("ResolveNetNS(%q) = %q, want it unchanged", path, got)
		}
	}
}

func TestResolveNetNSRewritesProcPaths(t *testing.T) {
	// A containerised daemon sees its own /proc. Without the rewrite it would
	// look up pid 1234 in the wrong namespace, or find nothing.
	opts := Options{NetNSProcDir: "/host_proc"}

	got := opts.ResolveNetNS("/proc/1234/ns/net")
	want := "/host_proc/1234/ns/net"
	if got != want {
		t.Errorf("ResolveNetNS = %q, want %q", got, want)
	}
}

func TestResolveNetNSLeavesNamedNamespacesAlone(t *testing.T) {
	// cri-o keeps namespaces as bind mounts under /var/run/netns, which the
	// daemon shares directly. Rewriting those would break it.
	opts := Options{NetNSProcDir: "/host_proc"}

	path := "/var/run/netns/cni-9f3a"
	if got := opts.ResolveNetNS(path); got != path {
		t.Errorf("ResolveNetNS(%q) = %q, want it unchanged", path, got)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Binding.LinkIface = "ens3"
	cfg.NeutronDefaults.OVSBridge = "br-int"
	cfg.CNIDaemon.DockerMode = true
	cfg.CNIDaemon.NetnsProcDir = "/host_proc"

	opts := OptionsFromConfig(cfg)

	if opts.LinkIface != "ens3" {
		t.Errorf("linkIface = %q, want ens3", opts.LinkIface)
	}
	if opts.NetNSProcDir != "/host_proc" {
		t.Errorf("netnsProcDir = %q, want /host_proc", opts.NetNSProcDir)
	}
	if opts.OVSBridge != "br-int" {
		t.Errorf("ovsBridge = %q, want br-int", opts.OVSBridge)
	}
}

func TestOptionsFromConfigIgnoresProcDirWithoutDockerMode(t *testing.T) {
	// netns_proc_dir only means anything in docker_mode. Honouring it
	// otherwise would send a host-namespace daemon looking in the wrong place.
	cfg := config.Defaults()
	cfg.CNIDaemon.DockerMode = false
	cfg.CNIDaemon.NetnsProcDir = "/host_proc"

	if got := OptionsFromConfig(cfg).NetNSProcDir; got != "" {
		t.Errorf("netnsProcDir = %q, want empty when docker_mode is off", got)
	}
}

func TestForRejectsUnknownVIFType(t *testing.T) {
	_, err := For(&kuryrv1.VIF{Type: "not-a-real-type"}, Options{})
	if err == nil {
		t.Fatal("expected an error for an unregistered VIF type")
	}
}

func TestHybridVethNamesMatchTheNeutronAgent(t *testing.T) {
	qvb, qvo := hybridVethNames("3f2a9c1e-0b44-4d5e-9a61-1234567890ab")
	if qvb != "qvb3f2a9c1e-0b" || qvo != "qvo3f2a9c1e-0b" {
		t.Errorf("names = %s, %s; want qvb3f2a9c1e-0b, qvo3f2a9c1e-0b", qvb, qvo)
	}
}
