FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o /bankingsync .

FROM alpine:3

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /bankingsync /app/bankingsync

VOLUME ["/data"]

ENTRYPOINT ["/app/bankingsync"]
