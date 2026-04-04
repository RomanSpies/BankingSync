FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o /bankingsync .

FROM alpine:3 AS runtime

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 11011 bankingsync && adduser -S -u 11011 -G bankingsync bankingsync \
    && mkdir -p /data && chown bankingsync:bankingsync /data

WORKDIR /app

COPY --from=builder /bankingsync /app/bankingsync

FROM runtime AS sbom

COPY --from=anchore/syft:latest /syft /usr/local/bin/syft
RUN syft dir:/ --select-catalogers "apk,go" --exclude './usr/local/bin/**' -o cyclonedx-json=/app/sbom.cdx.json

FROM runtime

COPY --from=sbom /app/sbom.cdx.json /app/sbom.cdx.json

VOLUME ["/data"]

USER bankingsync

ENTRYPOINT ["/app/bankingsync"]
