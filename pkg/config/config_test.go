package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The shipped ConfigMap is the first thing an operator edits, and a key that
// no longer exists, or a handler pair that the validator rejects, would only
// show up as a crash loop on a real cluster. Parsing it here is cheap.
func TestShippedConfigMapLoadsAndValidates(t *testing.T) {
	path := filepath.Join("..", "..", "config", "manager", "kuryr_config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	conf, ok := cm.Data["kuryr.conf"]
	if !ok {
		t.Fatalf("%s has no kuryr.conf entry", path)
	}

	file := filepath.Join(t.TempDir(), "kuryr.conf")
	if err := os.WriteFile(file, []byte(conf), 0o644); err != nil {
		t.Fatalf("writing the extracted config: %v", err)
	}

	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("the shipped configuration does not load: %v", err)
	}

	// Spot-check that the values really arrived, rather than the defaults
	// quietly standing in for keys the parser ignored.
	if cfg.Kubernetes.PodSubnetsDriver != "namespace" {
		t.Errorf("pod_subnets_driver = %q, want namespace", cfg.Kubernetes.PodSubnetsDriver)
	}
	if cfg.Kubernetes.PodSecurityGroupsDriver != "policy" {
		t.Errorf("pod_security_groups_driver = %q, want policy",
			cfg.Kubernetes.PodSecurityGroupsDriver)
	}
	if cfg.Kubernetes.VIFPoolDriver != "neutron" {
		t.Errorf("vif_pool_driver = %q, want neutron", cfg.Kubernetes.VIFPoolDriver)
	}
	if !cfg.Kubernetes.ControllerHA {
		t.Error("controller_ha did not come through as true")
	}
	if cfg.NeutronDefaults.ServiceSubnet == "" {
		t.Error("service_subnet did not come through")
	}
	if len(cfg.NeutronDefaults.PodSecurityGroups) == 0 {
		t.Error("pod_security_groups did not come through")
	}
	if cfg.NamespaceSubnet.PodRouter == "" {
		t.Error("pod_router did not come through")
	}
	if !cfg.CNIDaemon.DockerMode || cfg.CNIDaemon.NetnsProcDir == "" {
		t.Errorf("cni_daemon docker_mode/netns_proc_dir = %v/%q, want true and a path",
			cfg.CNIDaemon.DockerMode, cfg.CNIDaemon.NetnsProcDir)
	}
	if cfg.Prometheus.ControllerPort == 0 || cfg.Prometheus.CNIPort == 0 {
		t.Error("the exporter ports did not come through")
	}
	if cfg.Auth.AuthURL == "" || cfg.Auth.Username == "" {
		t.Error("the Keystone credentials did not come through")
	}
}

// Every handler the sample enables must be one the binary actually knows
// about, or it silently does nothing.
func TestShippedConfigMapEnablesOnlyRealHandlers(t *testing.T) {
	known := map[string]bool{
		"vif": true, "kuryrport": true,
		"namespace": true, "kuryrnetwork": true,
		"policy": true, "kuryrnetworkpolicy": true,
		"service": true, "kuryrloadbalancer": true,
	}

	cfg := loadShipped(t)
	for _, h := range cfg.Kubernetes.EnabledHandlers {
		h = strings.TrimSpace(h)
		if !known[h] {
			t.Errorf("enabled_handlers names %q, which no controller implements", h)
		}
	}
}

func TestValidateRejectsHalfAHandlerPair(t *testing.T) {
	cfg := loadShipped(t)

	// Removing one half of a pair has to be caught: the intent would be
	// written and nothing would act on it.
	cfg.Kubernetes.EnabledHandlers = []string{
		"vif", "kuryrport", "namespace", "kuryrnetwork",
		"policy", "kuryrnetworkpolicy", "service",
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for service without kuryrloadbalancer")
	}
}

func TestValidateRejectsPolicyWithoutTheDriver(t *testing.T) {
	cfg := loadShipped(t)
	cfg.Kubernetes.PodSecurityGroupsDriver = "default"

	if err := cfg.Validate(); err == nil {
		t.Error("expected an error when the policy handlers run but no pod uses their groups")
	}
}

func loadShipped(t *testing.T) *Config {
	t.Helper()
	path := filepath.Join("..", "..", "config", "manager", "kuryr_config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	file := filepath.Join(t.TempDir(), "kuryr.conf")
	if err := os.WriteFile(file, []byte(cm.Data["kuryr.conf"]), 0o644); err != nil {
		t.Fatalf("writing the extracted config: %v", err)
	}
	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("loading the shipped configuration: %v", err)
	}
	return cfg
}

// The sample in etc/ is what `make run` loads and what an operator copies
// first. It is a plain file rather than a ConfigMap, so it can be handed
// straight to Load.
func TestSampleConfigLoadsAndValidates(t *testing.T) {
	path := filepath.Join("..", "..", "etc", "kuryr.conf")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the sample configuration does not load: %v", err)
	}

	// Spot-check across sections, so a renamed key shows up here rather than
	// as a default quietly standing in for one the parser ignored.
	if cfg.NeutronDefaults.Project == "" {
		t.Error("project did not come through")
	}
	if cfg.NeutronDefaults.PodSubnet == "" || cfg.NeutronDefaults.ServiceSubnet == "" {
		t.Error("the subnets did not come through")
	}
	if len(cfg.NeutronDefaults.PodSecurityGroups) == 0 {
		t.Error("pod_security_groups did not come through")
	}
	if len(cfg.NeutronDefaults.ResourceTags) == 0 {
		t.Error("resource_tags did not come through")
	}
	if !cfg.Kubernetes.PortDebug {
		t.Error("port_debug did not come through as true")
	}
	if cfg.OctaviaDefaults.MemberMode != "L3" {
		t.Errorf("member_mode = %q, want L3", cfg.OctaviaDefaults.MemberMode)
	}
	if cfg.CNIDaemon.BindAddress == "" || cfg.CNIDaemon.VIFAnnotationTimeout == 0 {
		t.Error("the cni_daemon settings did not come through")
	}
	if cfg.Prometheus.ControllerPort == 0 || cfg.Prometheus.CNIPort == 0 {
		t.Error("the exporter ports did not come through")
	}
	if cfg.Auth.AuthURL == "" || cfg.Auth.Username == "" {
		t.Error("the Keystone credentials did not come through")
	}
}

// A handler the sample enables but no binary implements would silently do
// nothing, which is the worst way to find out a name is wrong.
func TestSampleConfigEnablesOnlyRealHandlers(t *testing.T) {
	known := map[string]bool{
		"vif": true, "kuryrport": true,
		"namespace": true, "kuryrnetwork": true,
		"policy": true, "kuryrnetworkpolicy": true,
		"service": true, "kuryrloadbalancer": true,
	}

	cfg, err := Load(filepath.Join("..", "..", "etc", "kuryr.conf"))
	if err != nil {
		t.Fatalf("loading the sample configuration: %v", err)
	}
	for _, h := range cfg.Kubernetes.EnabledHandlers {
		h = strings.TrimSpace(h)
		if !known[h] {
			t.Errorf("enabled_handlers names %q, which no controller implements", h)
		}
	}
}

// The conflist the CNI image installs onto every node has to be valid JSON
// naming the plugin binary this repo builds, or the runtime silently uses no
// CNI plugin at all.
func TestSampleCNIConflistIsUsable(t *testing.T) {
	path := filepath.Join("..", "..", "etc", "cni", "net.d", "kuryr.conflist.template")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var list struct {
		CNIVersion string `json:"cniVersion"`
		Name       string `json:"name"`
		Plugins    []struct {
			Type          string `json:"type"`
			DaemonAddress string `json:"daemonAddress"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}

	if list.CNIVersion == "" || list.Name == "" {
		t.Errorf("cniVersion/name = %q/%q, want both set", list.CNIVersion, list.Name)
	}
	if len(list.Plugins) != 1 {
		t.Fatalf("got %d plugins, want exactly one", len(list.Plugins))
	}
	if list.Plugins[0].Type != "kuryr-cni" {
		t.Errorf("plugin type = %q, want kuryr-cni (the binary cmd/kuryr-cni builds)",
			list.Plugins[0].Type)
	}

	// The plugin talks to the daemon over this address, so the two files have
	// to agree; a mismatch is a CNI ADD that times out on every pod.
	cfg, err := Load(filepath.Join("..", "..", "etc", "kuryr.conf"))
	if err != nil {
		t.Fatalf("loading the sample configuration: %v", err)
	}
	if got := list.Plugins[0].DaemonAddress; got != cfg.CNIDaemon.BindAddress {
		t.Errorf("conflist daemonAddress = %q but [cni_daemon]bind_address = %q",
			got, cfg.CNIDaemon.BindAddress)
	}
}

// nested-dpdk attaches each interface to a named node VM through Nova, so a
// pool of interchangeable unbound ports cannot exist. Catching it here beats
// failing on the first pod with a driver that cannot answer RequestVIFs.
func TestValidateRejectsPooledNestedDPDK(t *testing.T) {
	cfg := loadShipped(t)
	cfg.Kubernetes.PodVIFDriver = "nested-dpdk"
	cfg.Kubernetes.VIFPoolDriver = "neutron"

	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for nested-dpdk with a pool driver")
	}

	cfg.Kubernetes.VIFPoolDriver = "noop"
	if err := cfg.Validate(); err != nil {
		t.Errorf("nested-dpdk with vif_pool_driver=noop should be accepted: %v", err)
	}
}

func TestAnnotationSubnetDriverStillNeedsAFallback(t *testing.T) {
	// The annotation driver only overrides pods that ask for a subnet, so
	// whatever serves the rest has to be configured as usual.
	cfg := loadShipped(t)
	cfg.Kubernetes.PodSubnetsDriver = "annotation"
	cfg.AnnotationSubnet.FallbackDriver = "default"
	cfg.NeutronDefaults.PodSubnet = ""

	if err := cfg.Validate(); err == nil {
		t.Error("expected pod_subnet to still be required behind the annotation driver")
	}

	cfg.NeutronDefaults.PodSubnet = "subnet-default"
	if err := cfg.Validate(); err != nil {
		t.Errorf("annotation over the default driver should be accepted: %v", err)
	}
}

func TestAnnotationSubnetDriverCannotFallBackToItself(t *testing.T) {
	cfg := loadShipped(t)
	cfg.Kubernetes.PodSubnetsDriver = "annotation"
	cfg.AnnotationSubnet.FallbackDriver = "annotation"

	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for an annotation driver falling back to itself")
	}
}

func TestAnnotationSubnetDriverInheritsTheFallbackRequirements(t *testing.T) {
	// Falling back to the namespace driver drags in everything that driver
	// needs, which is what would otherwise be missed by checking only the
	// configured driver name.
	cfg := loadShipped(t)
	cfg.Kubernetes.PodSubnetsDriver = "annotation"
	cfg.AnnotationSubnet.FallbackDriver = "namespace"
	cfg.NamespaceSubnet.PodRouter = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected pod_router to still be required behind the annotation driver")
	}
	if !strings.Contains(err.Error(), "pod_router") {
		t.Errorf("error = %v, want it to name pod_router", err)
	}
}

func TestAnnotationSubnetEnforcesProjectByDefault(t *testing.T) {
	// The default has to be the safe one: with enforcement off, any user who
	// can annotate a pod can put a port on another tenant's subnet.
	if !Defaults().AnnotationSubnet.EnforceProject {
		t.Error("[annotation_subnet]enforce_project should default to true")
	}
}
