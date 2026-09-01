# Configuration

Both binaries read the same `kuryr.conf`. The controller ignores `[cni_daemon]`
and the daemon ignores most of `[kubernetes]`; nothing in the file is
per-binary, and there is no second file.

The format is the INI that the Python implementation used, deliberately
unchanged: operators already have these files and their tooling already
templates them. What changed is that the result is a typed struct validated at
load time, instead of a global registry read lazily from anywhere in the
process.

- [etc/kuryr.conf](../etc/kuryr.conf) is an annotated sample to copy and edit.
- [config/manager/kuryr_config.yaml](../config/manager/kuryr_config.yaml) is the
  ConfigMap a deployed controller and daemon actually read. Editing the sample
  does not change a running cluster.

## How values are resolved

1. Start from the built-in defaults (`pkg/config.Defaults()`).
2. Apply anything set in the file. A key left out, or left empty, keeps the
   default — so commenting a key out is the same as deleting it.
3. Fill empty Keystone credentials from `OS_*` environment variables.
4. Validate, and refuse to start if the result cannot work.

Durations are plain integers in **seconds**. Lists are comma-separated. Maps use
the `oslo.config` form `key1:value1,key2:value2`.

Point a binary at the file with `--config-file`; both default to
`/etc/kuryr/kuryr.conf`.

A key the parser does not know is ignored, as is a whole unknown section. That
is what makes an existing `kuryr.conf` safe to keep: settings that this
implementation dropped are read straight past rather than rejected.

## Validation

These are checked at startup, so a misconfiguration is a process that refuses
to start rather than a pod stuck in `ContainerCreating`.

| Rule | Why |
| --- | --- |
| `ports_pool_batch` ≥ 1 | A batch of zero never fills a pool |
| `ports_pool_max` is 0, or ≥ `ports_pool_min` | A ceiling below the floor cannot be satisfied |
| `docker_mode` requires `netns_proc_dir` | Host namespace paths need translating to where `/proc` is mounted |
| Handler pairs enabled together | Half a pair is a pipeline with no other end |
| `service` handler requires `service_subnet` (with the `default` service subnet driver) | Octavia needs somewhere to hold the VIP |
| `pod_subnets_driver = default` requires `pod_subnet` | Pods need a subnet to get addresses from |
| `pod_subnets_driver = namespace` requires the `kuryrnetwork` handler and `pod_router` | Nothing would create the per-namespace subnets |
| `pod_security_groups_driver = policy` requires the `policy` handler, and vice versa | The driver applies the groups; the handler creates them. Either alone silently enforces nothing |
| `pod_security_groups_driver = policy` requires `service_security_groups_driver = policy` | Otherwise a load balancer would not inherit the policy of the pods behind it |

## `[kubernetes]`

Which parts of Kuryr run, and which driver fills each slot.

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled_handlers` | `vif,kuryrport,service,kuryrloadbalancer` | The reconcilers to run. See [Handlers](#handlers) |
| `api_root` | derived | Kubernetes API URL. Falls back to `KUBERNETES_SERVICE_HOST`/`_PORT_HTTPS`, then to the ambient kubeconfig |
| `pod_project_driver` | `default` | Which OpenStack project owns a pod's resources: `default` (one project from `[neutron_defaults]project`) or `annotation` (read from the namespace's `openstack.org/kuryr-project`) |
| `service_project_driver` | `default` | Same, for Services |
| `namespace_project_driver` | `default` | Same, for Namespaces |
| `pod_subnets_driver` | `default` | `default` puts every pod on `pod_subnet`; `namespace` gives each Kubernetes namespace its own Neutron subnet |
| `service_subnets_driver` | `default` | Only `default` is implemented: the subnet from `[neutron_defaults]service_subnet` |
| `pod_security_groups_driver` | `default` | `default` applies a fixed list; `policy` makes the groups follow NetworkPolicy |
| `service_security_groups_driver` | `default` | `default` applies a fixed list; `policy` gives a load balancer the groups of the pods it fronts |
| `pod_vif_driver` | `neutron-vif` | How pod ports are created: `neutron-vif`, `nested-vlan`, `nested-macvlan` |
| `vif_pool_driver` | `noop` | `noop` creates a port per pod on demand; `neutron` keeps a pool of pre-created ports |
| `multi_vif_drivers` | `noop` | Extra interfaces: `noop`, or `npwg` to read the `k8s.v1.cni.cncf.io/networks` annotation. More than one may be listed; their results are concatenated |
| `endpoints_driver_octavia_provider` | `default` | The Octavia provider to build with. `default` lets the cloud choose. Checked against the cloud at startup |
| `port_debug` | `false` | Name Neutron ports after their pod. Costs one extra write per port, and is worth it the first time you read a port list during an incident |
| `controller_ha` | `false` | Hold a Lease so exactly one replica is active. `--leader-elect` overrides it |
| `max_concurrent_reconciles` | `10` | Per-controller reconcile concurrency |
| `additional_ifname_prefix` | `eth` | Prefix for interfaces beyond the first |

### Handlers

Handlers come in pairs, one watching Kubernetes and one driving OpenStack.
Enabling half a pair is rejected at startup.

| Pair | Gives you |
| --- | --- |
| `vif` + `kuryrport` | Pod networking. Without both, pods hang |
| `namespace` + `kuryrnetwork` | A Neutron subnet per Kubernetes namespace |
| `policy` + `kuryrnetworkpolicy` | NetworkPolicy enforced as security groups |
| `service` + `kuryrloadbalancer` | Services backed by Octavia |

A minimal working set is `vif,kuryrport,service,kuryrloadbalancer`.

## `[neutron_defaults]`

| Key | Default | Meaning |
| --- | --- | --- |
| `project` | — | **Required.** The OpenStack project everything is created in |
| `pod_subnet` | — | **Required** unless `pod_subnets_driver = namespace`. The subnet pods get addresses from |
| `service_subnet` | — | **Required** by the `service` handler. The Neutron subnet the cluster's service CIDR is carved from. Nothing lives on it; it exists so Octavia can hold a VIP at each Service's clusterIP |
| `pod_security_groups` | — | **Required** unless `pod_security_groups_driver = policy`. The groups a pod gets when no NetworkPolicy selects it |
| `ovs_bridge` | `br-int` | The integration bridge the CNI agent plugs veth pairs into |
| `external_svc_net` | — | Required only for Services of type `LoadBalancer`. Without it such a Service is reported as unsupported rather than left silently without an address |
| `external_svc_subnet` | — | Pins the floating IP to a particular subnet of that network |
| `resource_tags` | — | Tags put on every resource Kuryr creates. This is how an operator tells them apart from everything else in the project |
| `subnet_mapping` | empty | Per-driver subnet overrides for the `npwg` driver, as `driver:subnet-id,...` |

## `[octavia_defaults]`

| Key | Default | Meaning |
| --- | --- | --- |
| `member_mode` | `L3` | `L3` routes to members through a router and registers them on the load balancer's own subnet. `L2` bridges instead, and needs each backend's real subnet — including `worker_nodes_subnets` for backends on the node network |
| `lb_algorithm` | `ROUND_ROBIN` | The Octavia pool algorithm |
| `enforce_sg_rules` | `true` | Derive the VIP's security rules from what the backends allow. Only meaningful with the Amphora provider, which rewrites the source address so a backend's own groups cannot see the real client |
| `timeout_client_data` | `0` | Listener client timeout, ms. 0 leaves it to Octavia. A Service can override it with the `openstack.org/kuryr-timeout-client-data` annotation |
| `timeout_member_data` | `0` | Listener member timeout, ms. Annotation: `openstack.org/kuryr-timeout-member-data` |

## `[vif_pool]`

Only used when `vif_pool_driver = neutron`. See
[architecture.md](architecture.md#port-pools) for how the pool behaves.

| Key | Default | Meaning |
| --- | --- | --- |
| `ports_pool_min` | `5` | Refill when the pool drops below this |
| `ports_pool_batch` | `10` | How many ports one bulk create makes |
| `ports_pool_max` | `0` | Ceiling above which a returned port is deleted rather than recycled. 0 means no ceiling |
| `ports_pool_update_frequency` | `20` | Seconds between pool maintenance passes |
| `vif_pool_mapping` | empty | For clusters mixing bare metal and nested nodes, as `vif-driver:pool-driver,...` |

## `[namespace_subnet]`

Only used when `pod_subnets_driver = namespace`.

| Key | Default | Meaning |
| --- | --- | --- |
| `pod_router` | — | **Required** by that driver. The router each per-namespace subnet is attached to |
| `pod_subnet_pool` | — | The subnet pool new namespace subnets are carved from |

## `[pod_vif_nested]`

| Key | Default | Meaning |
| --- | --- | --- |
| `worker_nodes_subnets` | — | The Neutron subnets the Kubernetes nodes live on. NetworkPolicy security groups always allow ingress from them, so kubelet probes keep working on an otherwise isolated pod. Also how `L2` member mode resolves a backend that sits on the node network |

## `[binding]`

| Key | Default | Meaning |
| --- | --- | --- |
| `link_iface` | auto-detected | The node uplink a nested VIF hangs off. Set it when the node has more than one candidate |

## `[cni_daemon]`

| Key | Default | Meaning |
| --- | --- | --- |
| `bind_address` | `127.0.0.1:5036` | Where the daemon listens for the `kuryr-cni` plugin. Changing it means changing `daemonAddress` in the CNI conflist too |
| `vif_annotation_timeout` | `60` | Seconds a CNI ADD waits for the port to exist and become ACTIVE. The runtime enforces its own deadline above this |
| `cni_failures_count` | `3` | Consecutive failures before the daemon reports itself unhealthy |
| `docker_mode` | `false` | Set when the daemon runs in a container, as it does in the DaemonSet |
| `netns_proc_dir` | — | **Required** when `docker_mode` is true. Where the host's `/proc` is mounted inside the container |

## `[nested_dpdk]`

Only used by the `nested-dpdk` binding driver.

| Key | Default | Meaning |
| --- | --- | --- |
| `dpdk_driver` | `uio_pci_generic` | The userspace driver a device is rebound to |
| `pci_mount_point` | `/var/pci_address` | Where each device's PCI address is written for the pod to read |

## `[vhostuser]`

Only used by the `vhostuser` binding driver.

| Key | Default | Meaning |
| --- | --- | --- |
| `mount_point` | `/var/cni/vhostuser` | Where the socket and its description are placed for the pod |
| `ovs_vhu_path` | `/var/run/openvswitch/` | Where Open vSwitch creates the socket |

## `[prometheus_exporter]`

| Key | Default | Meaning |
| --- | --- | --- |
| `controller_exporter_port` | `9654` | Controller metrics port. The Deployment passes `--metrics-bind-address`, which takes precedence and serves over TLS with authentication, so this only applies when that flag is absent |
| `cni_exporter_port` | `9655` | Node agent metrics port. The agent is hostNetwork and node-local, and serves plaintext here |

## `[neutron]` — Keystone credentials

Any value left empty falls back to the matching environment variable, so an
application credential can be injected from a Secret, or a sourced `openrc`,
instead of being written into the file.

| Key | Environment fallback |
| --- | --- |
| `auth_url` | `OS_AUTH_URL` |
| `username` | `OS_USERNAME` |
| `user_id` | `OS_USER_ID` |
| `password` | `OS_PASSWORD` |
| `project_name` | `OS_PROJECT_NAME` |
| `project_id` | `OS_PROJECT_ID` |
| `user_domain_name` | `OS_USER_DOMAIN_NAME` |
| `project_domain_name` | `OS_PROJECT_DOMAIN_NAME` |
| `application_credential_id` | `OS_APPLICATION_CREDENTIAL_ID` |
| `application_credential_secret` | `OS_APPLICATION_CREDENTIAL_SECRET` |
| `region_name` | `OS_REGION_NAME` |
| `cafile` | `OS_CACERT` |
| `insecure` | none — file only |

An application credential is the better choice for a long-running service: it
can be scoped and revoked without touching the user's password.

## Command-line flags

Flags are not in the file. Where both exist, the flag wins.

### `kuryr-controller`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--config-file` | `/etc/kuryr/kuryr.conf` | Path to `kuryr.conf` |
| `--leader-elect` | `false` | Hold a Lease. Overrides `controller_ha` |
| `--leader-election-namespace` | `kube-system` | Where the Lease lives |
| `--metrics-bind-address` | unset | Serve metrics here, over TLS with authentication |
| `--metrics-secure` | `true` | Require authentication on the metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Liveness and readiness |
| `--enable-http2` | `false` | Off by default |

### `kuryr-daemon`

| Flag | Default | Meaning |
| --- | --- | --- |
| `--config-file` | `/etc/kuryr/kuryr.conf` | Path to `kuryr.conf` |
| `--node-name` | hostname | The node this daemon serves. The DaemonSet sets it from the downward API; the hostname fallback is a guess, and a node registered under a different name would make the daemon watch nothing at all |
| `--metrics-bind-address` | unset | Metrics endpoint |
| `--health-probe-bind-address` | `:8090` | Liveness and readiness |

## Annotations and labels

| Name | On | Effect |
| --- | --- | --- |
| `openstack.org/kuryr-project` | Namespace | Pins everything in it to an OpenStack project. Read by the `annotation` project driver |
| `openstack.org/kuryr-timeout-client-data` | Service | Overrides `timeout_client_data` for that Service's listeners |
| `openstack.org/kuryr-timeout-member-data` | Service | Overrides `timeout_member_data` |
| `k8s.v1.cni.cncf.io/networks` | Pod | Extra interfaces, read by the `npwg` multi-VIF driver |
| `pod_vif` | Node | Which pod VIF driver this node uses, for clusters mixing bare metal and nested nodes |
| `kuryr.openstack.org/nodeName` | KuryrPort | Set by the controller. Lets each node's daemon watch only its own pods |

