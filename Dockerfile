FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG VERSION
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o d2k ./cmd/d2k.go

FROM scratch

ARG VERSION
LABEL org.opencontainers.image.source="https://github.com/Proponent-8247/portainer-d2k" \
      org.opencontainers.image.revision="$VERSION" \
      org.opencontainers.image.title="portainer-d2k" \
      org.opencontainers.image.description="Docker/Swarm-to-Kubernetes translator with Docker-network-equivalent isolation"

COPY --from=builder /build/d2k /d2k
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENTRYPOINT ["/d2k"]
