# kuryr-kubernetes (Go)

A Go rewrite of [kuryr-kubernetes][upstream], the CNI plugin that gives
Kubernetes pods real Neutron ports and backs Kubernetes Services with Octavia
load balancers, instead of running an overlay network on top of the one
OpenStack already provides.

The CRD group (`openstack.org/v1`) and the `kuryr.conf` format are unchanged
from the Python implementation, so existing RBAC, tooling and configuration
files keep working. What changed is underneath: typed Go structs in place of
serialized `oslo.versionedobjects`, controller-runtime in place of a
hand-written watch/retry/dispatch pipeline, and one process with goroutines in
place of several under `cotyledon`.

[upstream]: https://opendev.org/openstack/kuryr-kubernetes

## Documentation

| Document | What is in it |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | How it works: the components, the CRDs, and what happens when a pod starts, a policy changes or a Service is created |
| [docs/configuration.md](docs/configuration.md) | Every `kuryr.conf` section and key, what it does, and which combinations are rejected at startup |
| [etc/kuryr.conf](etc/kuryr.conf) | Annotated sample configuration to copy and edit |

## What gets deployed

Three binaries, from two container images:

| Binary | Runs as | Job |
| --- | --- | --- |
| `kuryr-controller` | a Deployment, 2 replicas, one active | Watches Kubernetes, creates and deletes Neutron and Octavia resources |
| `kuryr-daemon` | a DaemonSet, one per node | Plugs interfaces into pod network namespaces |
| `kuryr-cni` | a binary the daemon installs on the host | The CNI plugin the container runtime calls; forwards to `kuryr-daemon` over localhost |

The controller never touches a node, and the daemon never talks to OpenStack.
Everything between them travels through four CRDs, which is also what makes the
state visible to `kubectl` rather than only to whoever is reading logs:

| Kind | Short name | Holds |
| --- | --- | --- |
| `KuryrPort` | `kp` | The Neutron ports behind one pod |
| `KuryrNetwork` | `kns` | The Neutron network and subnet behind one namespace |
| `KuryrNetworkPolicy` | `knp` | A NetworkPolicy translated into security group rules |
| `KuryrLoadBalancer` | `klb` | The Octavia topology behind one Service |

## Getting started

### What you need first

- An OpenStack cloud with Neutron, and Octavia if you want Services of type
  `LoadBalancer`.
- Keystone credentials for a project Kuryr can create ports in. An application
  credential is the better choice for a long-running service: it can be scoped
  and revoked without touching anyone's password.
- The Neutron subnets the cluster will use — one for pods, one the cluster's
  service CIDR is carved from.
- A Kubernetes cluster whose container runtime loads CNI plugins from
  `/etc/cni/net.d` and `/opt/cni/bin` — the default for containerd and CRI-O —
  and whose nodes can reach both the Kubernetes API and the OpenStack APIs.
- No other CNI plugin installed. Kuryr writes `10-kuryr.conflist`, and the
  runtime picks whichever config sorts first.

### Configure

Copy the sample and fill in the placeholders marked `REQUIRED`:

```sh
cp etc/kuryr.conf /etc/kuryr/kuryr.conf
$EDITOR /etc/kuryr/kuryr.conf
```

Every key is documented in [docs/configuration.md](docs/configuration.md).
Combinations that cannot work — half of a handler pair, a driver whose required
setting is missing — are rejected when the process starts, not when the first
pod is created.

### Deploy with kustomize

```sh
make docker-build docker-push CONTROLLER_IMG=<registry>/controller:<tag> CNI_IMG=<registry>/cni:<tag>
make deploy         CONTROLLER_IMG=<registry>/controller:<tag> CNI_IMG=<registry>/cni:<tag>
```

`make deploy` applies `config/default`, which puts everything in `kube-system`:
Kuryr is the cluster's CNI plugin, so it has to come up alongside the rest of
the control plane rather than in a namespace nothing can reach until Kuryr is
already working.

Edit the configuration in `config/manager/kuryr_config.yaml` before deploying —
that ConfigMap, not `etc/kuryr.conf`, is what a deployed controller reads.

### Deploy with Helm

```sh
make helm           CONTROLLER_IMG=<registry>/controller:<tag> CNI_IMG=<registry>/cni:<tag>
helm install kuryr ./dist/kuryr --namespace kube-system
```

`make helm` renders the same kustomize output into a chart under `dist/kuryr`.
The chart is a build artifact: regenerate it rather than editing it, and see
[docs/architecture.md](docs/architecture.md#helm-chart-generation) for the one
thing the generator cannot express on its own.

On a cluster that enforces Pod Security Admission on `kube-system`, label the
namespace before installing — the node agent is privileged by necessity:

```sh
kubectl label ns kube-system pod-security.kubernetes.io/enforce=privileged
```

### Check it came up

```sh
kubectl -n kube-system get deploy,ds -l app.kubernetes.io/part-of=kuryr
kubectl get kuryrports -A          # one per pod that has been given ports
kubectl get kuryrloadbalancers -A  # one per Service
```

Every CRD carries conditions, so a pod stuck in `ContainerCreating` has an
explanation on the object rather than only in the logs:

```sh
kubectl describe kuryrport -n <namespace> <pod-name>
```

## Development

```sh
make test        # unit tests, after regenerating manifests and deepcopy
make lint        # golangci-lint, the standing quality gate
make build       # all three binaries into bin/
make run         # the controller against ~/.kube/config, using etc/kuryr.conf
```

`make vet` also vets for `GOOS=linux`. That matters: everything that touches
netlink is behind a Linux build tag, so a developer on another platform never
compiles half the CNI daemon otherwise. `make lint` should be run the same way
before trusting a clean run:

```sh
GOOS=linux golangci-lint run
```

### Layout

```
api/v1/              The four CRDs and the VIF types they carry
cmd/                 The three binaries
internal/controller/ One reconciler per watched kind
pkg/cni/binding/     Node-side interface plugging (Linux only)
pkg/cni/registry/    The daemon's view of the pods on its node
pkg/cni/server/      The HTTP server the CNI plugin calls
pkg/cni/watcher/     Keeps the registry in step with the API
pkg/config/          kuryr.conf parsing and validation
pkg/drivers/         The pluggable decisions: project, subnet, security groups, VIF
pkg/openstack/       The Neutron and Octavia client
pkg/policy/          NetworkPolicy to security group rule translation
pkg/pool/            Pre-created port pools
pkg/vif/             Neutron ports to VIF objects
```

## Status

This is a rewrite in progress. The CRDs, the controllers and the CNI daemon are
implemented.

`kuryr.conf` is a smaller file than the Python implementation read: settings
that nothing in this codebase acts on have been removed rather than accepted
and quietly ignored, so every key in
[docs/configuration.md](docs/configuration.md) does something. An existing
configuration file still loads unchanged — an unknown key is read straight
past — so the dropped settings can be left in place or cleaned up at leisure.

## License

Apache 2.0. See [LICENSE](LICENSE).
