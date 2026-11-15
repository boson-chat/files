# boson files: Go binary on distroless (nonroot). The build stage runs on
# the build host's platform (no QEMU); Go cross-compiles for the target.
FROM --platform=$BUILDPLATFORM golang:1.27 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN mkdir -p /out/data && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/files ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder --chown=65532:65532 /out/files .
COPY --from=builder --chown=65532:65532 /out/data /data

ARG VERSION=dev
ENV VERSION=$VERSION ENV=production FILES_DATA_DIR=/data

# Local storage driver (the default): mount a volume here.
VOLUME /data
# 8080: public files host (behind the reverse proxy).
# 3001: internal API (X-Internal-Token), /metrics, /healthz. Never publish.
EXPOSE 8080 3001
USER 65532:65532
ENTRYPOINT ["./files"]
CMD ["serve"]
