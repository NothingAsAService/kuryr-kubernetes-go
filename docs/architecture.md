# How it works

Kuryr gives a pod a real Neutron port instead of an address on an overlay
network. That means every pod is a first-class citizen of the OpenStack
network: security groups apply to it, Neutron routes to it, and an Octavia load
balancer can reach it directly rather than through a node proxy.

This document describes how that is arranged. For what each configuration key
does, see [configuration.md](configuration.md).

## The shape of the system

```
        Kubernetes API                              OpenStack
             |                                     Neutron / Octavia
             |                                            ^
   +---------+---------+                                  |
   |                   |                                  |
   v                   v                                  |
Pod, Service,      KuryrPort, KuryrNetwork,               |
Namespace,         KuryrNetworkPolicy,     ---------------+
NetworkPolicy      KuryrLoadBalancer        kuryr-controller
   (intent)           (accepted work)         (one active replica)
                           |
                           | watched, filtered to one node
                           v
                      kuryr-daemon  ->  plugs veth / VLAN / macvlan
                      (one per node)     into the pod's netns
                           ^
                           | HTTP on 127.0.0.1:5036
                      kuryr-cni  <- called by the container runtime
```

Two properties are worth stating up front, because most of the design follows
from them.

**The controller never touches a node, and the daemon never touches
OpenStack.** The controller has cloud credentials and no host access; the
daemon runs privileged on every node and has read-only Kubernetes access and no
cloud credentials. Compromising the thing that runs on every node does not hand
over the ability to create ports.

**Everything between them is a CRD.** Not an annotation, not a message queue.
The intermediate state is a first-class API object, so it can be inspected with
`kubectl`, validated by the API server, and carries conditions explaining why
something has not finished.

## Handlers come in pairs

`[kubernetes]enabled_handlers` names the reconcilers that run. They come in
pairs: one half watches a Kubernetes object and writes down what should exist,
the other half makes OpenStack match. Enabling one half of a pair is rejected
at startup rather than silently doing nothing.

| Kubernetes-facing half | CRD | OpenStack-facing half |
| --- | --- | --- |
| `vif` — watches Pods | `KuryrPort` | `kuryrport` — creates Neutron ports |
| `namespace` — watches Namespaces | `KuryrNetwork` | `kuryrnetwork` — creates networks and subnets |
| `policy` — watches NetworkPolicies | `KuryrNetworkPolicy` | `kuryrnetworkpolicy` — creates security groups and rules |
| `service` — watches Services and EndpointSlices | `KuryrLoadBalancer` | `kuryrloadbalancer` — builds the Octavia topology |

The split is not decoration. The Kubernetes-facing half talks only to the
Kubernetes API, so it is fast and cannot fail because of the cloud. When
Neutron is unavailable, the retries pile up against `KuryrPort` objects, not
against `Pod` objects that every other controller in the cluster is also
watching.

It also means the translated form is visible. A NetworkPolicy that is not doing
what someone expected can be diffed against the `KuryrNetworkPolicy` it
produced, which is a much shorter conversation than reading selector logic.

## Starting a pod, end to end

This is the path that matters most, because it is on the critical path of every
pod start.

1. **A pod is scheduled.** `PodReconciler` adds the finalizer
   `openstack.org/pod-finalizer` to the pod, then creates a `KuryrPort` with
   the same name and namespace. The finalizer goes on *before* the KuryrPort is
   created, so there is no window in which a port exists but its owner can
   vanish.

   The `KuryrPort` records `podUid` and `podNodeName`. The UID matters: a
   StatefulSet pod keeps its name across restarts but gets a fresh UID, so name
   alone cannot tell a live pod from a dead one. It also carries the label
   `kuryr.openstack.org/nodeName`, which is what lets each node's daemon watch
   only its own pods.

2. **`KuryrPortReconciler` creates the ports.** It resolves, through the
   configured drivers, which OpenStack project owns the pod, which subnets it
   gets addresses from, and which security groups apply. Then it asks the pod
   VIF driver — or the pool, if pooling is on — for a VIF, and writes the
   result to `status.vifs` keyed by interface name.

   Multi-VIF drivers run here too: if the pod carries the
   `k8s.v1.cni.cncf.io/networks` annotation and the `npwg` driver is enabled,
   the extra interfaces are created alongside the default one.

   The reconciler then polls Neutron until the port is `ACTIVE`, and sets the
   `Provisioned` and `Active` conditions as it goes. A port that stays `DOWN`
   longer than the timeout gets that fact written onto the `KuryrPort`; the
   retries do not stop, but the problem becomes visible to `kubectl` instead of
   only to whoever is reading logs.

3. **The runtime calls the CNI plugin.** `kuryr-cni` does almost nothing: it
   parses its stdin config and POSTs the request to `kuryr-daemon` at
   `127.0.0.1:5036`. Keeping the plugin thin is deliberate — it is executed
   once per pod by the runtime, so it must not need a cloud connection, a cache
   or a warm-up.

4. **The daemon binds the interfaces.** `kuryr-daemon` runs a controller-runtime
   cache filtered to its own node, so it only ever receives the `KuryrPort`
   objects it will be asked about. Its `registry` is the in-memory view of
   those pods.

   On ADD the daemon:
   - takes a per-pod lock, so a concurrent DEL for the same pod is serialised
     while different pods proceed in parallel;
   - waits for the pod's VIFs to appear in the registry, checking the pod UID
     so a stale `KuryrPort` from an earlier incarnation is rejected rather than
     used;
   - plugs each interface with the binding driver for its VIF type;
   - records which container ID owns the interfaces, so a DEL arriving during
     the wait can tell whether it belongs to this call;
   - **waits for the port to be ACTIVE before returning.** Returning early
     would let the pod start on an interface whose flows are not yet
     programmed, which presents as random packet loss rather than as a failure.

   Waiters block on a broadcast channel and wake the instant the watcher writes,
   rather than polling.

5. **Deletion runs the same path backwards.** Deleting the pod triggers its
   finalizer, which deletes the `KuryrPort`; the `KuryrPort` finalizer releases
   the Neutron ports — or returns them to the pool — and only then is the pod's
   own finalizer removed. A DEL that finds no registry data waits only briefly:
   if the VIF information is gone there is nothing to reconstruct it from, and
   blocking would leave the pod stuck `Terminating`.

## Binding drivers

Binding is the node-side act of putting an interface into the pod's network
namespace. The driver is chosen from the VIF type recorded on the `KuryrPort`,
not from configuration — the controller already decided, and the node carries
it out.

| VIF type | What happens on the node |
| --- | --- |
| `ovs` | A veth pair; one end into the pod, the other into the integration bridge with `ovs-vsctl` |
| `bridge` | A veth pair; the host end enslaved to a Linux bridge, which is what Neutron asks for when it wants iptables rules on the port |
| `nested-vlan` | An 802.1Q subinterface of the node VM uplink, moved into the pod namespace |
| `nested-macvlan` | A macvlan child of the node VM uplink — no trunk, no VLAN id limit, and no Neutron filtering |
| `nested-dpdk` | The device is unbound from its kernel driver and rebound to a userspace one; the pod drives it itself. The port behind it is attached to the node VM through Nova, not Neutron, so it cannot be pooled |
| `vhostuser` | A vhost-user socket, plus a file describing it, placed where the pod can reach them |

Everything that touches netlink is behind a `linux` build tag. The interface
and the registry are portable, so the rest of the daemon and all of the tests
build anywhere — which is why `make vet` also vets for `GOOS=linux`.

## Bare metal and nested deployments

Where the Kubernetes nodes run decides how a pod port reaches the network.

- **Bare metal** (`pod_vif_driver = neutron-vif`). The node is the compute host
  running the Neutron OVS agent. A pod port is bound directly to the node.
- **Nested** (`nested-vlan`). Pods run inside node VMs, which are themselves
  Neutron ports. Each pod port is attached to the node VM's *trunk* as a VLAN
  subport. The compute host sees only the trunk parent.
- **Nested, trunkless** (`nested-macvlan`). The pod port is recorded as an
  allowed address pair on the node VM uplink and surfaced as a macvlan child.
  This lifts the 4094-VLAN-per-node ceiling, at the cost of Neutron not being
  able to filter the traffic.

In a nested deployment the pool keys on the node VM's address rather than the
node name, because the trunk belongs to the VM, not to the Kubernetes object
describing it.

A cluster can mix both: label a node `pod_vif=<driver>` and set
`[vif_pool]vif_pool_mapping` so each node's pods are served by the right pool.

## Port pools

Creating a Neutron port takes long enough to be visible in pod start latency.
The pool trades quota for that latency: ports are created in bulk ahead of
time, and a pod is handed one that already exists.

- A pool is primed when a namespace network is created, so the first pod
  scheduled into that namespace is served from it rather than waiting. Priming
  is recorded on the `KuryrNetwork` before it runs: doing it twice would create
  a batch per node again, while a prime that failed part way is made good by
  the ordinary top-up on first use.
- The first request for a given pool fills it synchronously, so the first pod
  on a node does not fail and get retried.
- Every later request is served from the pool and schedules a background
  top-up. The pod does not wait for it.
- A top-up creates a whole `ports_pool_batch` whenever the pool has fallen
  below `ports_pool_min`.
- A port handed back by a deleted pod is recycled rather than deleted, unless
  the pool is already at `ports_pool_max`.
- Security groups are part of the pool key. A pooled port with the wrong groups
  can still be used — its groups are rewritten first — but an exact match is
  preferred.

On restart the pool **recovers** rather than starting empty: it lists the ports
it left behind (or walks the node VM's trunk, on a nested deployment) and takes
them back. Ports it cannot place are deleted, so a crash does not leak quota.

## NetworkPolicy

Translation happens in two stages, for the same reason everything else is
split.

`NetworkPolicyReconciler` answers one question: *what rules does this policy
mean, right now, given the pods and namespaces in the cluster?* It touches no
cloud. Named ports are resolved against the pods the policy selects, CIDR peers
are expanded, and the result is written to a `KuryrNetworkPolicy`.

`KuryrNetworkPolicyReconciler` then makes Neutron match. The security group is
created once and kept in step **rule by rule** — never rewritten wholesale.
That is not an optimisation: a security group applied to live ports enforces
whatever it contains at that moment, so deleting every rule and recreating it
would briefly drop traffic the policy allows.

Two details are easy to miss:

- Ingress from the worker node subnets is always allowed
  (`[pod_vif_nested]worker_nodes_subnets`), so kubelet health probes keep
  working on an otherwise isolated pod.
- When `pod_security_groups_driver = policy`, the `service` half has to use it
  too. Otherwise a load balancer would not inherit the policy of the pods
  behind it, and this is checked at startup.

## Services and load balancers

`ServiceReconciler` reads a Service *and* its EndpointSlices and writes one
`KuryrLoadBalancer` spec. The Python implementation split this in two — a
Service handler wrote the frontend, an Endpoints handler wrote the backend,
both patching the same object — which meant two writers on one resource and
conflicts retried through the whole pipeline.

`KuryrLoadBalancerReconciler` then builds the Octavia topology: load balancer,
one listener per service port, one pool per listener, and one member per ready
backend. Octavia only accepts changes while the load balancer is `ACTIVE`, so
each pass does as much as it can and requeues instead of blocking on a
provisioning poll.

Everything it creates is recorded on the status before the next step starts. A
controller that dies halfway through resumes where it stopped rather than
building a second load balancer it has no record of. A load balancer deleted
outside Kuryr is noticed and rebuilt; a provider change releases it, because a
different Octavia driver cannot be applied in place.

**Member mode** decides which subnet Octavia is told about. In the default
`L3`, traffic reaches members through a router, so the load balancer's own
subnet is used. In `L2` the traffic is bridged and Octavia needs the subnet
each backend actually sits on — including
`[pod_vif_nested]worker_nodes_subnets` for backends that turn out to be on the
node network.

**VIP security rules** exist because of the Amphora provider. Amphora rewrites
the source address, so a backend's own security groups cannot see the real
client. Kuryr therefore derives the VIP's rules from what the backends allow.
The backends' groups are deliberately *not* applied to the VIP port: Octavia
owns a security group there, and overwriting it would cut the load balancer off
from its own control plane.

## Namespace networks

With `pod_subnets_driver = namespace`, each Kubernetes namespace gets its own
Neutron subnet. `NamespaceReconciler` writes a `KuryrNetwork`;
`KuryrNetworkReconciler` creates the network, the subnet from the configured
pool, and attaches it to `[namespace_subnet]pod_router`.

The point is not isolation for its own sake. It is that a NetworkPolicy
selecting a whole namespace becomes *one* security group rule against that
namespace's CIDR, instead of one rule per pod that has to be rewritten every
time a pod is created or deleted.

Each step writes its result to the status before the next starts, so an
interrupted controller resumes rather than creating a second network.

## Restart and failure behaviour

The design assumes the controller will be killed at an arbitrary moment.

- **Status before the next step.** Every reconciler records what it created
  before it moves on, so a resumed pass adopts existing resources instead of
  duplicating them.
- **Adoption by name.** Where a resource can be created but not recorded — the
  load balancer, for instance — the next pass looks it up by a name containing
  the Kubernetes object's UID and adopts it. That is why a retry does not leak
  one.
- **Finalizers, not garbage collection.** Kubernetes cannot delete a Neutron
  port, so every CRD carries a finalizer and releases its cloud resources
  before it is removed.
- **Conditions, not events.** Events are lossy and expire; conditions live on
  the object for as long as the object does.
- **Leader election.** `controller_ha` (or `--leader-elect`) makes the
  controller hold a Lease. Two replicas reconciling the same pod would both
  create ports and only one would win the status write, leaking the other.

## Metrics

Both processes export Prometheus metrics. The names keep the Python prefixes so
existing dashboards keep working.

| Metric | Where | What it tells you |
| --- | --- | --- |
| `kuryr_ports_pool_size` | controller | Free ports held per pool |
| `kuryr_ports_pool_created_total` | controller | Ports created to fill pools |
| `kuryr_ports_pool_reused_total` | controller | Pods served from a pool rather than a fresh port |
| `kuryr_ports_pool_recycled_total` | controller | Ports handed back by deleted pods |
| `kuryr_ports_pool_deleted_total` | controller | Ports deleted because the pool was at `ports_pool_max` |
| `kuryr_ports_pool_populate_failures_total` | controller | Failed refills — rising, with an empty pool, means pods are waiting on Neutron |
| `kuryr_cni_request_duration_seconds` | daemon | CNI request latency by command and outcome |
| `kuryr_cni_request_failures_total` | daemon | CNI failures by command and a bounded reason label |

The pool metrics exist because "pods are slow to start" is otherwise hard to
attribute: it could be Neutron, an empty pool, or repopulation failing. These
separate the three.

## Helm chart generation

`make helm` renders `config/default` through `helmify`. One thing that
generator cannot express is that Kuryr has **two** service accounts: helmify
models a chart as having one, and points every workload and every RoleBinding
at it.

Accepting that would give the privileged node agent — which runs on every node
and only ever needs to read — the controller's full ClusterRole.
`hack/helm-split-serviceaccounts.sh` runs after helmify and puts the split
back. It asserts what it expects to find before each edit, so a helmify upgrade
that changes the generated shape fails loudly instead of quietly producing a
chart that over-permissions the node agent.

That script is not optional, which is why it is part of the `helm` target
rather than a documented manual step.
