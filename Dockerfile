FROM golang:1.27-alpine AS builder

ARG VERSION=dev

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/banschikovde/fluxview/internal/cli.version=${VERSION}" \
    -o /fluxview ./cmd/fluxview/

FROM alpine:3.21

RUN apk add --no-cache git

COPY --from=builder /fluxview /usr/local/bin/fluxview

# Pre-create the cache dir so a named volume mounted there inherits this ownership (Docker would create it as root).
RUN addgroup -g 65532 fluxview && \
    adduser -D -u 65532 -G fluxview -h /home/fluxview -s /sbin/nologin fluxview && \
    mkdir -p /home/fluxview/.cache/fluxview && \
    chown -R 65532:65532 /home/fluxview/.cache

USER 65532:65532

ENTRYPOINT ["fluxview"]
CMD ["--help"]
