FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags timetzdata -ldflags="-s -w -X main.Version=${VERSION}" -o /bankingsync .

FROM alpine:3 AS base

# Everything the runtime needs besides the binary is prepared here, on the build
# platform, and only copied into the target image. A RUN in a target-platform
# stage would need QEMU on the runner to build for another architecture.
FROM --platform=$BUILDPLATFORM alpine:3 AS rootfs

COPY --from=base /etc/passwd /etc/group /out/etc/
RUN apk add --no-cache ca-certificates \
    && mkdir -p /out/etc/ssl/certs /out/data \
    && cp /etc/ssl/certs/ca-certificates.crt /out/etc/ssl/certs/ \
    && echo 'bankingsync:x:11011:' >> /out/etc/group \
    && echo 'bankingsync:x:11011:11011::/data:/sbin/nologin' >> /out/etc/passwd

FROM base AS runtime

COPY --from=rootfs /out/etc/passwd /out/etc/group /etc/
COPY --from=rootfs /out/etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=rootfs --chown=11011:11011 /out/data /data

WORKDIR /app

COPY --from=builder /bankingsync /app/bankingsync

FROM --platform=$BUILDPLATFORM anchore/syft:latest AS syft

FROM --platform=$BUILDPLATFORM alpine:3 AS sbom

COPY --from=syft /syft /usr/local/bin/syft
COPY --from=runtime / /target-rootfs
RUN mkdir -p /app \
    && syft dir:/target-rootfs --select-catalogers "apk,go" -o cyclonedx-json=/app/sbom.cdx.json

FROM runtime

COPY --from=sbom /app/sbom.cdx.json /app/sbom.cdx.json

VOLUME ["/data"]

USER bankingsync

ENTRYPOINT ["/app/bankingsync"]
