# NewAPI Middleware Tool - Go Backend Dockerfile
# Multi-stage build for minimal production image
#
# 构建缓存: 使用 BuildKit 缓存挂载加速二次构建
#   DOCKER_BUILDKIT=1 docker build -t backend-go .

# Stage 1: Build Go binary
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS builder
ARG TARGETARCH
ARG APP_VERSION=0.5.2
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

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
    -ldflags="-s -w -X github.com/new-api-tools/backend/internal/buildinfo.Version=${APP_VERSION} -X github.com/new-api-tools/backend/internal/buildinfo.Commit=${VCS_REF} -X github.com/new-api-tools/backend/internal/buildinfo.BuildDate=${BUILD_DATE}" \
    -o /build/server \
    ./cmd/server

# Stage 2: Minimal production image
FROM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata curl

ARG GEOIP_SOURCE_COMMIT=a83d44508ee6831c2770b2c4be91f9850ec429d7
ARG GEOIP_SHA256=168b01d10d0742129be1bee92bba85affaaefcf2e86b4187bcf1924ea50068bf
RUN mkdir -p /usr/share/GeoIP && \
    (curl --fail --silent --show-error --location --retry 3 --retry-all-errors --connect-timeout 30 --max-time 120 \
       -o /usr/share/GeoIP/GeoLite2-City.mmdb \
       "https://raw.githubusercontent.com/adysec/IP_database/${GEOIP_SOURCE_COMMIT}/geolite/GeoLite2-City.mmdb" || \
     curl --fail --silent --show-error --location --retry 3 --retry-all-errors --connect-timeout 30 --max-time 120 \
       -o /usr/share/GeoIP/GeoLite2-City.mmdb \
       "https://cdn.jsdelivr.net/gh/adysec/IP_database@${GEOIP_SOURCE_COMMIT}/geolite/GeoLite2-City.mmdb") && \
    echo "${GEOIP_SHA256}  /usr/share/GeoIP/GeoLite2-City.mmdb" | sha256sum -c -

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
    CMD curl --fail --silent --show-error http://localhost:8000/readyz || exit 1

# Start server
CMD ["/app/server"]
