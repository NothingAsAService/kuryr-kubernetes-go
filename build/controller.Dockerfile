# kuryr-controller.
#
# The Python image needed a CentOS base, a virtualenv, pip, a compiler and the
# whole OpenStack SDK dependency tree. A static Go binary needs none of that,
# so the runtime layer is distroless: nothing to patch, no shell to exploit,
# and an image measured in tens of megabytes rather than hundreds.
FROM golang:1.27 AS builder

WORKDIR /src

# Dependencies first, so a source-only change reuses the cached layer.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/

# CGO is off so the result runs on a base with no libc at all.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/kuryr-controller ./cmd/kuryr-controller

FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.title="kuryr-controller" \
      org.opencontainers.image.description="Kubernetes to Neutron controller" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/kuryr-controller /kuryr-controller

# The distroless nonroot user. The controller talks to two APIs and writes
# nothing to disk, so it never needs more.
USER 65532:65532

ENTRYPOINT ["/kuryr-controller"]
CMD ["--config-file", "/etc/kuryr/kuryr.conf"]
