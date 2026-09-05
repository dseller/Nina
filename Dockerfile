# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# go.mod requires Go 1.27; the toolchain would otherwise download itself at
# build time, which a sealed builder cannot do.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not invalidate the module layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Stamped the same way the Makefile does it. Pass --build-arg VERSION=v1.2.3
# for a real release; the default matches `make build` on a repo without tags.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/nina ./cmd/nina

# ---- runtime --------------------------------------------------------------
FROM alpine:3.22

# ca-certificates is not optional: the gateway fetches upstream OpenAPI
# documents and proxies to backends over TLS.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 65532 nina \
 && mkdir -p /etc/nina /var/cache/nina \
 && chown nina:nina /var/cache/nina

COPY --from=build /out/nina /usr/local/bin/nina

USER nina
WORKDIR /etc/nina

# Matches examples/nina.yaml: gateway, then the admin listener.
EXPOSE 18080 19090

# Spec cache survives restarts if you mount a volume here; without one the
# gateway just refetches on boot.
VOLUME ["/var/cache/nina"]

# Assumes server.admin.listen is set. Drop this if you run without the admin
# listener, or the container will be reported unhealthy.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:19090/healthz >/dev/null || exit 1

ENTRYPOINT ["nina"]
CMD ["run", "-c", "/etc/nina/nina.yaml", "--cache-dir", "/var/cache/nina"]
