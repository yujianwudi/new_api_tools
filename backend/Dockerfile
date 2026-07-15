# NewAPI Middleware Tool - Go Backend Dockerfile
# Multi-stage build for minimal production image
#
# 构建缓存: 使用 BuildKit 缓存挂载加速二次构建
#   DOCKER_BUILDKIT=1 docker build -t backend-go .

# syntax=docker/dockerfile:1

# Stage 1: Build Go binary
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine3.23 AS builder
ARG TARGETARCH

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Copy module files first for layer caching
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code and build with cache
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags="-s -w" \
    -o /build/server \
    ./cmd/server

# Stage 2: Minimal production image
FROM alpine:3.23.5

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata curl

# Create non-root user
RUN addgroup -S -g 10001 appgroup && \
    adduser -S -D -H -u 10001 -G appgroup -s /sbin/nologin appuser

# Copy binary from builder
COPY --from=builder --chown=appuser:appgroup /build/server /app/server

# Create data directory
RUN mkdir -p /app/data && chown -R appuser:appgroup /app

USER appuser

# Standalone backend images must listen on the container interface so a
# separately deployed frontend/reverse proxy can reach port 8000.
ENV SERVER_HOST=0.0.0.0

EXPOSE 8000

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl --fail --silent --show-error http://localhost:8000/api/health || exit 1

# Start server
CMD ["/app/server"]
