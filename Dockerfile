FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG VERSION=dev
ARG REVISION=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X github.com/portainer/d2k/internal/types.Version=${VERSION}" -o d2k ./cmd/d2k.go

FROM scratch

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/Proponent-8247/portainer-d2k" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.title="portainer-d2k" \
      org.opencontainers.image.description="Docker/Swarm-to-Kubernetes translator with Docker-network-equivalent isolation"

COPY --from=builder /build/d2k /d2k
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# d2k only needs the Kubernetes API and its listen sockets (>1024). Run the
# immutable scratch image as an unprivileged numeric identity so restricted
# Pod Security does not depend on a deployment-time override.
USER 65532:65532

ENTRYPOINT ["/d2k"]
