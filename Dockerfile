# NewAPI Middleware Tool - All-in-One Dockerfile (Go Backend)
# 前端 + Go 后端合并到单个镜像
#
# 构建缓存说明:
#   - npm 依赖缓存: /root/.npm
#   - Go 模块缓存: /go/pkg/mod
#   - Go 编译缓存: /root/.cache/go-build
#   使用 docker buildx build 或 DOCKER_BUILDKIT=1 启用缓存挂载

# Stage 1: 构建前端
FROM --platform=$BUILDPLATFORM node:22-alpine3.23@sha256:8516dce0483394d5708d4b2ee6cacb79fb1d617ea4e2787c2120bcca92ce372e AS frontend-builder
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: 构建 Go 后端
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine3.23@sha256:622e56dbc11a8cfe87cafa2331e9a201877271cbff918af53d3be315f3da88cc AS backend-builder
ARG TARGETARCH
ARG APP_VERSION=0.5.1
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
WORKDIR /build
RUN apk add --no-cache git ca-certificates tzdata

# 先复制依赖文件，利用层缓存
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 复制源码并编译，挂载 Go 编译缓存
COPY backend/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags="-s -w -X github.com/new-api-tools/backend/internal/buildinfo.Version=${APP_VERSION} -X github.com/new-api-tools/backend/internal/buildinfo.Commit=${VCS_REF} -X github.com/new-api-tools/backend/internal/buildinfo.BuildDate=${BUILD_DATE}" \
    -o /build/server \
    ./cmd/server

# Stage 3: 最终镜像 (Nginx + Go binary)
FROM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40
WORKDIR /app

# 安装 Nginx 和运行时依赖
RUN apk add --no-cache \
    nginx \
    supervisor \
    su-exec \
    curl \
    ca-certificates \
    tzdata

# 复制 Go 二进制
COPY --from=backend-builder /build/server /app/server

# 创建低权限运行用户和数据目录。Supervisor 仅负责启动，Nginx/Go 进程均降权运行。
RUN addgroup -S -g 10001 appgroup && \
    adduser -S -D -H -u 10001 -G appgroup -s /sbin/nologin appuser && \
    mkdir -p /app/data/geoip /run/nginx && \
    chown -R appuser:appgroup /app && \
    chown -R nginx:nginx /run/nginx /var/lib/nginx /var/log/nginx && \
    chmod 755 /app/data

# Bundle one immutable, checksum-verified GeoIP snapshot outside /app/data so
# the persistent data volume cannot hide it. Runtime network updates are off by
# default and require explicit GEOIP_AUTO_DOWNLOAD/GEOIP_AUTO_UPDATE opt-in.
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

RUN chown -R appuser:appgroup /app/data

# 复制前端构建产物
COPY --from=frontend-builder /app/dist /usr/share/nginx/html

# 复制 Nginx 配置
COPY frontend/nginx.conf /etc/nginx/http.d/default.conf

# 修改 Nginx 配置，代理到本地 Go 后端
RUN sed -i 's|http://backend:8000|http://127.0.0.1:8000|g' /etc/nginx/http.d/default.conf

# Supervisor 配置 - 同时运行 Nginx 和 Go 后端
RUN mkdir -p /etc/supervisor.d && \
    echo -e '[supervisord]\nnodaemon=true\nuser=root\n\n\
[program:nginx]\ncommand=/usr/sbin/nginx -g "daemon off;"\nautostart=true\nautorestart=true\n\
user=nginx\n\
stdout_logfile=/dev/stdout\nstdout_logfile_maxbytes=0\n\
stderr_logfile=/dev/stderr\nstderr_logfile_maxbytes=0\n\n\
[program:backend]\ncommand=/bin/sh -c "chown -R appuser:appgroup /app/data && exec su-exec appuser /app/server"\ndirectory=/app\nautostart=true\nautorestart=true\n\
stdout_logfile=/dev/stdout\nstdout_logfile_maxbytes=0\n\
stderr_logfile=/dev/stderr\nstderr_logfile_maxbytes=0\n' > /etc/supervisord.conf

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD ready="$(curl --fail --silent --show-error http://localhost:8080/readyz 2>/dev/null)" && \
      printf '%s' "$ready" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"ready"' && \
      printf '%s' "$ready" | grep -Eq '"main_database"[[:space:]]*:[[:space:]]*"ok"' && \
      printf '%s' "$ready" | grep -Eq '"tool_store"[[:space:]]*:[[:space:]]*"ok"'

CMD ["/usr/bin/supervisord", "-c", "/etc/supervisord.conf"]
