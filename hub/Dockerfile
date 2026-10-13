# syntax=docker/dockerfile:1

# Build natively on the builder's platform and cross-compile (pure Go, no cgo).
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cercle-hub ./cmd/cercle-hub \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cercle-hub /usr/local/bin/cercle-hub
# Pre-create /data owned by the nonroot user so fresh named volumes are writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV CERCLE_LISTEN=:8080 \
    CERCLE_DB=/data/cercle.db
USER nonroot:nonroot
EXPOSE 8080
VOLUME /data
# distroless has no shell/curl: the binary checks its own /health.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/cercle-hub", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/cercle-hub"]
