# Build stage
FROM golang:1.26-alpine AS builder

ARG VERSION=dev

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/banschikovde/fluxview/internal/cli.version=${VERSION}" \
    -o /fluxview ./cmd/fluxview/

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache git

COPY --from=builder /fluxview /usr/local/bin/fluxview

ENTRYPOINT ["fluxview"]
CMD ["--help"]
