// Package config loads kuryr.conf.
//
// The Python implementation used oslo.config. Operators already have
// kuryr.conf files, deployment tooling already templates them, and the section
// layout is documented, so the Go rewrite keeps the same INI file rather than
// inventing a new format. What changed is that the result is a typed struct
// validated at load time instead of a global registry read lazily from
// anywhere in the process.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/ini.v1"
)

const (
	// driverDefault selects the stock implementation of a driver slot. It is
	// what most of kuryr.conf ships with.
	driverDefault = "default"

	// policyName is shared by the NetworkPolicy security group driver and the
	// handler that creates those groups. They have to be enabled together,
	// which is what several of the checks in Validate are about.
	policyName = "policy"

	// driverNamespace gives each Kubernetes namespace its own Neutron subnet.
	driverNamespace = "namespace"

	// driverAnnotation reads the choice from an annotation on the object, or
	// on its namespace, rather than from kuryr.conf.
	driverAnnotation = "annotation"

	// driverNoop is the "do nothing" driver: no port pool, no extra
	// interfaces. It is the default for both slots that accept it.
	driverNoop = "noop"
)

// Config is the whole of kuryr.conf.
type Config struct {
	Kubernetes       Kubernetes
	NeutronDefaults  NeutronDefaults
	OctaviaDefaults  OctaviaDefaults
	CNIDaemon        CNIDaemon
	PodVIFNested     PodVIFNested
	NestedDPDK       NestedDPDK
	VIFPool          VIFPool
	NamespaceSubnet  NamespaceSubnet
	AnnotationSubnet AnnotationSubnet
	Binding          Binding
	VHostUser        VHostUser
	Prometheus       Prometheus
	Auth             Auth
}

// Kubernetes is the [kubernetes] section: which handlers run and which
// drivers they use.
type Kubernetes struct {
	APIRoot string

	// Driver selection. These are the names registered with the driver
	// registries in pkg/drivers; the Python version resolved them through
	// stevedore entry points.
	PodProjectDriver            string
	ServiceProjectDriver        string
	NamespaceProjectDriver      string
	NetworkPolicyProjectDriver  string
	PodSubnetsDriver            string
	ServiceSubnetsDriver        string
	PodSecurityGroupsDriver     string
	ServiceSecurityGroupsDriver string
	PodVIFDriver                string
	VIFPoolDriver               string
	// EndpointsOctaviaProvider names the Octavia provider driver to build
	// with. "default" leaves the choice to the cloud.
	EndpointsOctaviaProvider string
	MultiVIFDrivers          []string

	EnabledHandlers        []string
	AdditionalIfnamePrefix string
	PortDebug              bool
	ControllerHA           bool

	ResyncPeriod            time.Duration
	MaxConcurrentReconciles int
}

// NeutronDefaults is the [neutron_defaults] section.
type NeutronDefaults struct {
	Project           string
	PodSubnet         string
	ServiceSubnet     string
	PodSecurityGroups []string
	OVSBridge         string
	ExternalSvcNet    string
	ExternalSvcSubnet string
	SubnetMapping     map[string]string
	ResourceTags      []string
}

// OctaviaDefaults is the [octavia_defaults] section.
type OctaviaDefaults struct {
	MemberMode        string
	EnforceSGRules    bool
	LBAlgorithm       string
	TimeoutClientData int32
	TimeoutMemberData int32
}

// CNIDaemon is the [cni_daemon] section.
type CNIDaemon struct {
	BindAddress          string
	VIFAnnotationTimeout time.Duration
	DockerMode           bool
	NetnsProcDir         string
	CNIFailuresCount     int
}

// PodVIFNested is the [pod_vif_nested] section.
type PodVIFNested struct {
	// WorkerNodesSubnets are the Neutron subnets the Kubernetes nodes live
	// on. NetworkPolicy security groups always allow ingress from them, so
	// that kubelet health checks keep working on an otherwise isolated pod.
	WorkerNodesSubnets []string
}

// NestedDPDK is the [nested_dpdk] section.
type NestedDPDK struct {
	// Driver is the userspace driver a device is rebound to after being
	// unbound from its kernel driver.
	Driver string
	// PCIMountPoint is the directory where the PCI address of each device is
	// written, for the DPDK application in the pod to read.
	PCIMountPoint string
}

// VIFPool is the [vif_pool] section.
type VIFPool struct {
	PortsPoolMax             int
	PortsPoolMin             int
	PortsPoolBatch           int
	PortsPoolUpdateFrequency time.Duration
	// VIFPoolMapping maps a pod VIF driver name to the pool driver that
	// should manage it, for clusters mixing bare metal and nested nodes.
	VIFPoolMapping map[string]string
}

// NamespaceSubnet is the [namespace_subnet] section.
type NamespaceSubnet struct {
	PodRouter     string
	PodSubnetPool string
}

// AnnotationSubnet is the [annotation_subnet] section: the settings of the
// annotation pod subnet driver, which lets a workload name the Neutron subnet
// its ports come from.
type AnnotationSubnet struct {
	// FallbackDriver serves pods that carry no subnet annotation. The
	// annotation is an override, so there still has to be a default underneath
	// it -- normally "default" or "namespace".
	FallbackDriver string
	// EnforceProject refuses a subnet owned by a project other than the pod's.
	// It defaults to on: without it, anyone able to set a pod annotation can
	// put a port on any subnet the controller credentials can reach, which in
	// a multi-tenant cluster means any tenant's.
	EnforceProject bool
}

// Binding is the [binding] section.
type Binding struct {
	// LinkIface is the node uplink a nested VIF hangs off. When empty the
	// binding driver auto-detects it.
	LinkIface string
}

// VHostUser is the [vhostuser] section.
type VHostUser struct {
	MountPoint string
	OVSVhuPath string
}

// Prometheus is the [prometheus_exporter] section.
type Prometheus struct {
	ControllerPort int
	CNIPort        int
}

// Auth carries Keystone credentials. The Python version got these through
// kuryr-lib into a [neutron] section; the same section is read here, and any
// field left empty falls back to the standard OS_* environment variables so a
// container can be fed an application credential the usual way.
type Auth struct {
	AuthURL                     string
	Username                    string
	UserID                      string
	Password                    string
	ProjectName                 string
	ProjectID                   string
	UserDomainName              string
	ProjectDomainName           string
	ApplicationCredentialID     string
	ApplicationCredentialSecret string
	Region                      string
	CACertFile                  string
	Insecure                    bool
}

// Defaults returns a Config populated with the same defaults the Python
// implementation used, so an operator moving over gets identical behaviour
// from an identical file.
func Defaults() *Config {
	return &Config{
		Kubernetes: Kubernetes{
			APIRoot:                     "",
			PodProjectDriver:            driverDefault,
			ServiceProjectDriver:        driverDefault,
			NamespaceProjectDriver:      driverDefault,
			NetworkPolicyProjectDriver:  driverDefault,
			PodSubnetsDriver:            driverDefault,
			ServiceSubnetsDriver:        driverDefault,
			PodSecurityGroupsDriver:     driverDefault,
			ServiceSecurityGroupsDriver: driverDefault,
			PodVIFDriver:                "neutron-vif",
			VIFPoolDriver:               driverNoop,
			EndpointsOctaviaProvider:    driverDefault,
			MultiVIFDrivers:             []string{driverNoop},
			EnabledHandlers: []string{
				"vif", "kuryrport", "service", "kuryrloadbalancer",
			},
			AdditionalIfnamePrefix:  "eth",
			ResyncPeriod:            10 * time.Hour,
			MaxConcurrentReconciles: 10,
		},
		NeutronDefaults: NeutronDefaults{
			OVSBridge:     "br-int",
			SubnetMapping: map[string]string{},
		},
		OctaviaDefaults: OctaviaDefaults{
			MemberMode:     "L3",
			EnforceSGRules: true,
			LBAlgorithm:    "ROUND_ROBIN",
		},
		CNIDaemon: CNIDaemon{
			BindAddress:          "127.0.0.1:5036",
			VIFAnnotationTimeout: 60 * time.Second,
			CNIFailuresCount:     3,
		},
		NestedDPDK: NestedDPDK{
			Driver:        "uio_pci_generic",
			PCIMountPoint: "/var/pci_address",
		},
		VIFPool: VIFPool{
			PortsPoolMax:             0,
			PortsPoolMin:             5,
			PortsPoolBatch:           10,
			PortsPoolUpdateFrequency: 20 * time.Second,
			VIFPoolMapping:           map[string]string{},
		},
		AnnotationSubnet: AnnotationSubnet{
			FallbackDriver: driverDefault,
			EnforceProject: true,
		},
		VHostUser: VHostUser{
			MountPoint: "/var/cni/vhostuser",
			OVSVhuPath: "/var/run/openvswitch/",
		},
		Prometheus: Prometheus{ControllerPort: 9654, CNIPort: 9655},
	}
}

// Load reads kuryr.conf from path, applying defaults for anything absent. An
// empty path yields the defaults merged with the environment.
func Load(path string) (*Config, error) {
	c := Defaults()

	if path != "" {
		f, err := ini.LoadSources(ini.LoadOptions{
			AllowBooleanKeys:         true,
			SpaceBeforeInlineComment: true,
		}, path)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", path, err)
		}
		c.applyINI(f)
	}

	c.applyEnv()

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyINI(f *ini.File) {
	k := f.Section("kubernetes")
	str(k, "api_root", &c.Kubernetes.APIRoot)
	str(k, "pod_project_driver", &c.Kubernetes.PodProjectDriver)
	str(k, "service_project_driver", &c.Kubernetes.ServiceProjectDriver)
	str(k, "namespace_project_driver", &c.Kubernetes.NamespaceProjectDriver)
	str(k, "network_policy_project_driver", &c.Kubernetes.NetworkPolicyProjectDriver)
	str(k, "pod_subnets_driver", &c.Kubernetes.PodSubnetsDriver)
	str(k, "service_subnets_driver", &c.Kubernetes.ServiceSubnetsDriver)
	str(k, "pod_security_groups_driver", &c.Kubernetes.PodSecurityGroupsDriver)
	str(k, "service_security_groups_driver", &c.Kubernetes.ServiceSecurityGroupsDriver)
	str(k, "pod_vif_driver", &c.Kubernetes.PodVIFDriver)
	str(k, "vif_pool_driver", &c.Kubernetes.VIFPoolDriver)
	str(k, "endpoints_driver_octavia_provider", &c.Kubernetes.EndpointsOctaviaProvider)
	list(k, "multi_vif_drivers", &c.Kubernetes.MultiVIFDrivers)
	list(k, "enabled_handlers", &c.Kubernetes.EnabledHandlers)
	str(k, "additional_ifname_prefix", &c.Kubernetes.AdditionalIfnamePrefix)
	boolean(k, "port_debug", &c.Kubernetes.PortDebug)
	boolean(k, "controller_ha", &c.Kubernetes.ControllerHA)
	integer(k, "max_concurrent_reconciles", &c.Kubernetes.MaxConcurrentReconciles)

	n := f.Section("neutron_defaults")
	str(n, "project", &c.NeutronDefaults.Project)
	str(n, "pod_subnet", &c.NeutronDefaults.PodSubnet)
	str(n, "service_subnet", &c.NeutronDefaults.ServiceSubnet)
	list(n, "pod_security_groups", &c.NeutronDefaults.PodSecurityGroups)
	str(n, "ovs_bridge", &c.NeutronDefaults.OVSBridge)
	str(n, "external_svc_net", &c.NeutronDefaults.ExternalSvcNet)
	str(n, "external_svc_subnet", &c.NeutronDefaults.ExternalSvcSubnet)
	list(n, "resource_tags", &c.NeutronDefaults.ResourceTags)
	dict(n, "subnet_mapping", c.NeutronDefaults.SubnetMapping)

	o := f.Section("octavia_defaults")
	str(o, "member_mode", &c.OctaviaDefaults.MemberMode)
	boolean(o, "enforce_sg_rules", &c.OctaviaDefaults.EnforceSGRules)
	str(o, "lb_algorithm", &c.OctaviaDefaults.LBAlgorithm)
	integer32(o, "timeout_client_data", &c.OctaviaDefaults.TimeoutClientData)
	integer32(o, "timeout_member_data", &c.OctaviaDefaults.TimeoutMemberData)

	d := f.Section("cni_daemon")
	str(d, "bind_address", &c.CNIDaemon.BindAddress)
	seconds(d, "vif_annotation_timeout", &c.CNIDaemon.VIFAnnotationTimeout)
	boolean(d, "docker_mode", &c.CNIDaemon.DockerMode)
	str(d, "netns_proc_dir", &c.CNIDaemon.NetnsProcDir)
	integer(d, "cni_failures_count", &c.CNIDaemon.CNIFailuresCount)

	p := f.Section("vif_pool")
	integer(p, "ports_pool_max", &c.VIFPool.PortsPoolMax)
	integer(p, "ports_pool_min", &c.VIFPool.PortsPoolMin)
	integer(p, "ports_pool_batch", &c.VIFPool.PortsPoolBatch)
	seconds(p, "ports_pool_update_frequency", &c.VIFPool.PortsPoolUpdateFrequency)
	dict(p, "vif_pool_mapping", c.VIFPool.VIFPoolMapping)

	ns := f.Section("namespace_subnet")
	str(ns, "pod_router", &c.NamespaceSubnet.PodRouter)
	str(ns, "pod_subnet_pool", &c.NamespaceSubnet.PodSubnetPool)

	as := f.Section("annotation_subnet")
	str(as, "fallback_driver", &c.AnnotationSubnet.FallbackDriver)
	boolean(as, "enforce_project", &c.AnnotationSubnet.EnforceProject)

	nest := f.Section("pod_vif_nested")
	list(nest, "worker_nodes_subnets", &c.PodVIFNested.WorkerNodesSubnets)

	dpdk := f.Section("nested_dpdk")
	str(dpdk, "dpdk_driver", &c.NestedDPDK.Driver)
	str(dpdk, "pci_mount_point", &c.NestedDPDK.PCIMountPoint)

	b := f.Section("binding")
	str(b, "link_iface", &c.Binding.LinkIface)

	v := f.Section("vhostuser")
	str(v, "mount_point", &c.VHostUser.MountPoint)
	str(v, "ovs_vhu_path", &c.VHostUser.OVSVhuPath)

	pe := f.Section("prometheus_exporter")
	integer(pe, "controller_exporter_port", &c.Prometheus.ControllerPort)
	integer(pe, "cni_exporter_port", &c.Prometheus.CNIPort)

	a := f.Section("neutron")
	str(a, "auth_url", &c.Auth.AuthURL)
	str(a, "username", &c.Auth.Username)
	str(a, "user_id", &c.Auth.UserID)
	str(a, "password", &c.Auth.Password)
	str(a, "project_name", &c.Auth.ProjectName)
	str(a, "project_id", &c.Auth.ProjectID)
	str(a, "user_domain_name", &c.Auth.UserDomainName)
	str(a, "project_domain_name", &c.Auth.ProjectDomainName)
	str(a, "application_credential_id", &c.Auth.ApplicationCredentialID)
	str(a, "application_credential_secret", &c.Auth.ApplicationCredentialSecret)
	str(a, "region_name", &c.Auth.Region)
	str(a, "cafile", &c.Auth.CACertFile)
	boolean(a, "insecure", &c.Auth.Insecure)
}

// applyEnv fills empty auth fields from the standard OS_* variables.
func (c *Config) applyEnv() {
	envInto(&c.Auth.AuthURL, "OS_AUTH_URL")
	envInto(&c.Auth.Username, "OS_USERNAME")
	envInto(&c.Auth.UserID, "OS_USER_ID")
	envInto(&c.Auth.Password, "OS_PASSWORD")
	envInto(&c.Auth.ProjectName, "OS_PROJECT_NAME")
	envInto(&c.Auth.ProjectID, "OS_PROJECT_ID")
	envInto(&c.Auth.UserDomainName, "OS_USER_DOMAIN_NAME")
	envInto(&c.Auth.ProjectDomainName, "OS_PROJECT_DOMAIN_NAME")
	envInto(&c.Auth.ApplicationCredentialID, "OS_APPLICATION_CREDENTIAL_ID")
	envInto(&c.Auth.ApplicationCredentialSecret, "OS_APPLICATION_CREDENTIAL_SECRET")
	envInto(&c.Auth.Region, "OS_REGION_NAME")
	envInto(&c.Auth.CACertFile, "OS_CACERT")

	// A containerised controller has no api_root; derive it the way the
	// Python client did, from the injected service environment.
	if c.Kubernetes.APIRoot == "" {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if host != "" && port != "" {
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			c.Kubernetes.APIRoot = fmt.Sprintf("https://%s:%s", host, port)
		}
	}
}

// Validate rejects combinations that cannot work, as early as possible.
//
// The Python implementation discovered most of these at the moment a pod was
// created, which turned a typo in a config file into a pod stuck in
// ContainerCreating and a stack trace in the controller log.
func (c *Config) Validate() error {
	if err := c.validateLimits(); err != nil {
		return err
	}

	handlers := map[string]bool{}
	for _, h := range c.Kubernetes.EnabledHandlers {
		handlers[strings.TrimSpace(h)] = true
	}
	if err := c.validateHandlerPairs(handlers); err != nil {
		return err
	}
	return c.validateDrivers(handlers)
}

// validateLimits checks the numeric knobs, which stand on their own: nothing
// here depends on which handlers or drivers are enabled.
func (c *Config) validateLimits() error {
	if c.VIFPool.PortsPoolMax != 0 && c.VIFPool.PortsPoolMax < c.VIFPool.PortsPoolMin {
		return fmt.Errorf(
			"[vif_pool]ports_pool_max (%d) must be 0 or at least ports_pool_min (%d)",
			c.VIFPool.PortsPoolMax, c.VIFPool.PortsPoolMin)
	}
	if c.VIFPool.PortsPoolBatch < 1 {
		return fmt.Errorf("[vif_pool]ports_pool_batch must be at least 1, got %d",
			c.VIFPool.PortsPoolBatch)
	}
	if c.CNIDaemon.DockerMode && c.CNIDaemon.NetnsProcDir == "" {
		return fmt.Errorf("[cni_daemon]netns_proc_dir is required when docker_mode is true")
	}
	return nil
}

// validateHandlerPairs checks the handlers that only work in pairs.
//
// Each pair is one half that writes a CRD and one half that acts on it, so
// either alone is a pipeline with no other end.
func (c *Config) validateHandlerPairs(handlers map[string]bool) error {
	// The pod handler only creates KuryrPorts; without the kuryrport handler
	// nothing ever calls Neutron and every pod hangs.
	if handlers["vif"] != handlers["kuryrport"] {
		return fmt.Errorf(
			"[kubernetes]enabled_handlers: vif and kuryrport must be enabled together")
	}
	if handlers[policyName] != handlers["kuryrnetworkpolicy"] {
		return fmt.Errorf(
			"[kubernetes]enabled_handlers: policy and kuryrnetworkpolicy must be enabled together")
	}
	if handlers["namespace"] != handlers["kuryrnetwork"] {
		return fmt.Errorf(
			"[kubernetes]enabled_handlers: namespace and kuryrnetwork must be enabled together")
	}
	// The service handler writes KuryrLoadBalancer objects and the
	// kuryrloadbalancer handler turns them into Octavia resources. One
	// without the other means either intent nobody acts on, or objects
	// nobody creates.
	if handlers["service"] != handlers["kuryrloadbalancer"] {
		return fmt.Errorf(
			"[kubernetes]enabled_handlers: service and kuryrloadbalancer must be enabled together")
	}
	return nil
}

// validateDrivers checks the settings a driver needs from the rest of the
// file, and the handlers a driver cannot work without.
func (c *Config) validateDrivers(handlers map[string]bool) error {
	// nested-dpdk attaches each interface to a specific node VM through Nova,
	// so there is no such thing as an interchangeable unbound port to pool.
	if c.Kubernetes.PodVIFDriver == "nested-dpdk" && c.Kubernetes.VIFPoolDriver != driverNoop {
		return fmt.Errorf(
			"[kubernetes]pod_vif_driver=nested-dpdk cannot be pooled; set vif_pool_driver=noop, not %q",
			c.Kubernetes.VIFPoolDriver)
	}
	if handlers["service"] && c.NeutronDefaults.ServiceSubnet == "" &&
		c.Kubernetes.ServiceSubnetsDriver == driverDefault {
		return fmt.Errorf(
			"[neutron_defaults]service_subnet is required by the service handler; it must be " +
				"the Neutron subnet the cluster's service CIDR is carved from")
	}
	if handlers["service"] && c.Kubernetes.PodSecurityGroupsDriver == policyName &&
		c.Kubernetes.ServiceSecurityGroupsDriver != policyName {
		return fmt.Errorf(
			"[kubernetes]pod_security_groups_driver is policy, so service_security_groups_driver "+
				"must be too, not %q: otherwise a load balancer would not inherit the policy "+
				"of the pods behind it", c.Kubernetes.ServiceSecurityGroupsDriver)
	}
	if err := c.validatePodSubnetDrivers(handlers); err != nil {
		return err
	}
	// The two halves of NetworkPolicy support. The driver is what puts a
	// policy's security group on a pod, and the handlers are what create that
	// group. Either alone silently enforces nothing.
	if c.Kubernetes.PodSecurityGroupsDriver == policyName && !handlers[policyName] {
		return fmt.Errorf(
			"[kubernetes]pod_security_groups_driver=policy requires the policy and " +
				"kuryrnetworkpolicy handlers")
	}
	if handlers[policyName] && c.Kubernetes.PodSecurityGroupsDriver != policyName {
		return fmt.Errorf(
			"[kubernetes]enabled_handlers lists policy, so pod_security_groups_driver "+
				"must be policy, not %q: otherwise the security groups are created but "+
				"never applied to any pod", c.Kubernetes.PodSecurityGroupsDriver)
	}
	return nil
}

// validatePodSubnetDrivers checks what the pod subnet driver needs from the
// rest of the file.
//
// The annotation driver only overrides the pods that ask for a subnet, so
// whatever serves the rest still has to be configured. Every check below is
// therefore made against the effective driver rather than the configured name.
func (c *Config) validatePodSubnetDrivers(handlers map[string]bool) error {
	if c.Kubernetes.PodSubnetsDriver == driverAnnotation &&
		c.AnnotationSubnet.FallbackDriver == driverAnnotation {
		return fmt.Errorf(
			"[annotation_subnet]fallback_driver cannot be annotation; it names the driver " +
				"serving pods that carry no subnet annotation")
	}

	switch c.effectivePodSubnetsDriver() {
	case driverNamespace:
		if !handlers["kuryrnetwork"] {
			return fmt.Errorf(
				"[kubernetes]pod_subnets_driver=namespace requires the namespace and " +
					"kuryrnetwork handlers")
		}
		if c.NamespaceSubnet.PodRouter == "" {
			return fmt.Errorf("[namespace_subnet]pod_router is required by the namespace subnet driver")
		}
	case driverDefault:
		if c.NeutronDefaults.PodSubnet == "" {
			return fmt.Errorf("[neutron_defaults]pod_subnet is required by the default subnet driver")
		}
	}
	return nil
}

// effectivePodSubnetsDriver is the driver that decides a pod's subnet when
// nothing annotates one. It is the configured driver, except under the
// annotation driver, which is a decorator over a fallback.
func (c *Config) effectivePodSubnetsDriver() string {
	if c.Kubernetes.PodSubnetsDriver != driverAnnotation {
		return c.Kubernetes.PodSubnetsDriver
	}
	if c.AnnotationSubnet.FallbackDriver == "" {
		return driverDefault
	}
	return c.AnnotationSubnet.FallbackDriver
}

// PodsShareNamespaceSubnet reports whether every pod in a namespace is
// guaranteed to hold an address on that namespace's own subnet.
//
// That guarantee is what lets a NetworkPolicy namespaceSelector collapse into
// a single CIDR rule instead of one rule per pod. The annotation driver breaks
// it -- a pod may be annotated onto a subnet of its tenant's choosing -- so
// enabling it gives up the collapse rather than writing a rule that would miss
// the annotated pods.
func (c *Config) PodsShareNamespaceSubnet() bool {
	return c.Kubernetes.PodSubnetsDriver == driverNamespace
}

// HandlerEnabled reports whether a named handler should run.
func (c *Config) HandlerEnabled(name string) bool {
	for _, h := range c.Kubernetes.EnabledHandlers {
		if strings.TrimSpace(h) == name {
			return true
		}
	}
	return false
}

// --- small INI helpers -------------------------------------------------

func str(s *ini.Section, key string, dst *string) {
	if k, err := s.GetKey(key); err == nil {
		if v := strings.TrimSpace(k.String()); v != "" {
			*dst = v
		}
	}
}

func boolean(s *ini.Section, key string, dst *bool) {
	if k, err := s.GetKey(key); err == nil {
		if v, err := k.Bool(); err == nil {
			*dst = v
		}
	}
}

func integer(s *ini.Section, key string, dst *int) {
	if k, err := s.GetKey(key); err == nil {
		if v, err := k.Int(); err == nil {
			*dst = v
		}
	}
}

func integer32(s *ini.Section, key string, dst *int32) {
	if k, err := s.GetKey(key); err == nil {
		if v, err := k.Int(); err == nil {
			*dst = int32(v)
		}
	}
}

func seconds(s *ini.Section, key string, dst *time.Duration) {
	if k, err := s.GetKey(key); err == nil {
		if v, err := k.Int(); err == nil {
			*dst = time.Duration(v) * time.Second
		}
	}
}

func list(s *ini.Section, key string, dst *[]string) {
	if k, err := s.GetKey(key); err == nil {
		var out []string
		for _, part := range strings.Split(k.String(), ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			*dst = out
		}
	}
}

// dict parses the oslo.config DictOpt form: "key1:value1,key2:value2".
func dict(s *ini.Section, key string, dst map[string]string) {
	k, err := s.GetKey(key)
	if err != nil {
		return
	}
	for _, part := range strings.Split(k.String(), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		dst[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
}

func envInto(dst *string, key string) {
	if *dst == "" {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
}
