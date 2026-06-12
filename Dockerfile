# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /flux-diff ./cmd/flux-diff/

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache git

COPY --from=builder /flux-diff /usr/local/bin/flux-diff

ENTRYPOINT ["flux-diff"]
CMD ["--help"]
