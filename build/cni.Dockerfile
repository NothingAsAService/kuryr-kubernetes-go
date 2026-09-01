# kuryr-cni: the node agent and the CNI plugin binary it serves.
#
# Unlike the controller this cannot be distroless. The daemon shells out to
# ovs-vsctl to attach ports to the integration bridge, and the init script
# needs a shell to install the plugin onto the host. It stays a small
# single-purpose image rather than the full OpenStack runtime the Python
# version required.
FROM golang:1.27 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY pkg/ pkg/

ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
        -o /out/kuryr-daemon ./cmd/kuryr-daemon \
    && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" \
        -o /out/kuryr-cni ./cmd/kuryr-cni

FROM quay.io/centos/centos:stream9
LABEL org.opencontainers.image.title="kuryr-cni" \
      org.opencontainers.image.description="Kuryr CNI daemon and plugin" \
      org.opencontainers.image.licenses="Apache-2.0"

# openvswitch supplies ovs-vsctl, which is how a port is attached to the
# integration bridge. iproute and pciutils are for operators debugging a node,
# and kmod-libs is needed to load the vhost/uio modules a DPDK workload uses.
#
# Open vSwitch is not in the CentOS Stream base repositories. The NFV SIG ships
# it, and only under version-numbered package names -- there is no plain
# "openvswitch" to install -- so the release package goes on first and the
# version is named explicitly rather than left to float.
RUN dnf upgrade -y \
    && dnf install -y --setopt=tsflags=nodocs centos-release-nfv-openvswitch \
    && dnf install -y --setopt=tsflags=nodocs openvswitch3.5 iproute pciutils kmod-libs \
    && dnf clean all \
    && rm -rf /var/cache/dnf

COPY --from=builder /out/kuryr-daemon /usr/bin/kuryr-daemon
COPY --from=builder /out/kuryr-cni /kuryr-cni
COPY build/cni_ds_init /usr/bin/cni_ds_init
COPY etc/cni/net.d/kuryr.conflist.template /etc/kuryr-cni/kuryr.conflist.template

RUN chmod +x /usr/bin/cni_ds_init

# The daemon plugs interfaces on the host, so it runs privileged and as root.
# That is inherent to what a CNI node agent does, not a shortcut.
ENTRYPOINT ["/usr/bin/cni_ds_init"]
