# NewAPI Tools v0.5.0 发行说明

`v0.5.0` 将项目从“NewAPI 功能扩展后台”收敛为**独立、旁路、可审计的 API 中转站经营与可靠性控制台**。

本版本建立了独立 Tool Store、版本感知的 NewAPI Admin API 适配器、操作审计闭环、RBAC、依赖健康检查、Prometheus 指标、统一搜索/时间线和渠道质量视图。它同时主动关闭了一批无法证明安全性的自动化写入与破坏性旧功能。

## 发行重点

### 独立 Tool Store

- 默认使用 `DATA_DIR/control-plane.db`，也可通过 `TOOL_STORE_PATH` 指定。
- 与 NewAPI 主库、日志库完全分离，不向 NewAPI 创建 sidecar 表。
- 保存不可变操作审计、风险案件与事件、客服备注、价格快照和对账运行记录。
- 使用 SQLite 单连接、WAL、`synchronous=FULL`、内置迁移与启动健康检查；同步级别降级时 readiness 失败关闭。

### NewAPI 版本感知控制面

- 已验证契约基线为 NewAPI `v1.0.0-rc.21`。
- `rc.21` 支持已验证的用户管理和兑换码管理，但硬删除保持禁用。
- 仅 `v1.0.0-rc.22` 或稳定版 `v1.0.0+` 被视为具备安全硬删除能力。
- 未知版本格式和未来未知主版本默认只读。
- Admin API 客户端禁止重定向、限制响应体、限制公共明文 HTTP，并传播 `X-Request-ID`。

### 可审计写操作

- 用户启用、停用、删除及兑换码生成、删除统一经过 NewAPI Admin API。
- 写入必须通过版本能力探测、RBAC 和凭据检查。
- 调用方必须提供非空理由和 `Idempotency-Key`；该键只可重试同一认证主体、动作、目标和请求负载。
- 写前 intent 与写后 outcome 都进入 Tool Store。
- 兑换码生成只允许 `admin`；单码与单次总 quota 还受服务端财务上限约束，默认约为 US$100/码和 US$1000/次，并在 intent 前校验。
- 兑换码明文只在当前成功响应或一次性救援响应中交付，不进入操作审计库；历史仅保留指纹，关闭结果弹窗后前端清空明文。`do_not_retry=true` 时必须先对账。
- 兑换码创建数量和批量删除 ID 均限制为 `1..100`，用于约束部分成功的影响范围；安全重试来自 `Idempotency-Key` 和 intent/outcome 审计，不来自批次大小。

### RBAC

- `viewer`：只读。
- `operator`：常规运营写操作。
- `admin`：永久删除、价格快照等高风险操作。
- 管理密码登录当前签发 admin JWT。
- API Key 角色由 `API_KEY_ROLE` 配置，默认 `operator`。

### 健康、指标和请求追踪

- `GET /livez`
- `GET /readyz`
- `GET /api/health`
- `GET /api/health/db`
- `GET /api/health/dependencies`
- `GET /api/control-plane/newapi/capabilities`
- `GET /metrics`

`/metrics` 使用独立的 `OBSERVABILITY_TOKEN` Bearer 认证；未配置 token 时返回 404。指标包含 HTTP 请求与延迟、依赖状态、日志新鲜度、控制面操作结果和构建身份。

所有请求统一生成或接收 `X-Request-ID`，并把它传播到日志、Tool Store 审计和 NewAPI 上游请求。

### 搜索、统一时间线和渠道质量

- `GET /api/control-plane/search?q=&limit=`
- `GET /api/control-plane/users/:id/timeline?before=&limit=`
- `GET /api/control-plane/channel-quality?window=1h|24h|7d`

搜索覆盖 NewAPI 主库、日志库与 Tool Store。用户时间线统一五类来源，使用稳定游标，并在可选表缺失时按来源降级。

渠道质量最多采样 50,000 条 NewAPI 日志，按 channel 汇总 `type=2/5`、成功率、quota、最后请求、`use_time` 平均值/p95 和置信度。该能力不执行自动封禁、切流或利润推断。

### 发行与部署完整性

- Go 后端携带 version、commit 和 build date 构建身份。
- tag 构建从 GitHub ref 生成语义版本。
- 发布 `linux/amd64` 与 `linux/arm64` 多架构镜像。
- Docker 基础镜像与 Redis 固定到多架构 manifest digest。
- GeoIP 数据固定到提交 `a83d44508ee6831c2770b2c4be91f9850ec429d7`，并校验 SHA-256 `168b01d10d0742129be1bee92bba85affaaefcf2e86b4187bcf1924ea50068bf`。
- GeoIP 自动下载和运行时更新默认关闭，只有显式同时开启相关开关才允许更新。
- 固定 GeoIP 快照下载或校验失败时，镜像构建失败关闭，不生成缺少该制品的发行镜像。
- 安装/部署脚本支持明确的 `repo@sha256:digest`。
- tag 或 commit 派生镜像会解析为 OCI digest，并校验 `org.opencontainers.image.revision`。
- 手工 Compose 部署通过独立策略门禁拒绝 tag；所有 Compose 路径统一要求 Docker Compose v2.24.0+，不再回退旧版 `docker-compose` v1。
- 安装与升级会在停止旧服务前通过 `pull --include-deps newapi-tools` 递归预拉取应用、策略门禁和固定 digest 的 Redis，任一拉取失败都保持旧服务运行。
- 拉取候选前会把旧 mutable tag 从本地镜像解析为唯一同仓库 RepoDigest 并写成回滚锚点；无法解析时在 pull/down 前失败关闭。未验证候选值不会覆盖该锚点。
- 候选必须通过 revision 校验、`docker compose up -d --wait --wait-timeout 180` 和包含 Tool Store 的语义 readiness 后才提交配置；拉取、启动、健康或提交失败会恢复旧 Compose overlay、network、project identity 与旧 digest。
- 发布镜像生成 SBOM 和 provenance attestations。

## 破坏性安全变化

以下旧功能在 v0.5.0 中被禁用、降级为预览，或明确返回 `501 NOT_IMPLEMENTED`：

| 旧能力 | v0.5.0 行为 |
|---|---|
| Abuse Broadcast 路由和后台写入器 | 不挂载；Tool Store 风险案件取代 sidecar 写库 |
| AI 自动评估、自动扫描、连接测试 | 501 |
| 自动分组执行、批量移动、回滚 | 仅预览或 501 |
| 批量删除不活跃用户 | 仅预览 |
| 批量永久清理 | 仅预览或禁用 |
| Token 批量变更 | 501 |
| 批量开启 IP 记录 | 501 |
| Storage 通用配置入口（GET/POST/DELETE） | 全部 501；不再提供通用配置读取或写入 |
| 自动创建数据库索引 | 禁用，只返回建议 |
| 缓存 warmup 状态 | 501；改用健康端点 |
| 解封后批量恢复 Token | 不再执行 |

这些变化是有意的失败关闭策略。不要通过恢复旧直写数据库路径来绕过。

## 升级前检查

1. 记录当前 `newapi-tools` 镜像 tag 和 OCI digest。
2. 备份 `.env`。
3. 解析实际 `TOOL_STORE_PATH` 并备份 Tool Store；旧版本没有该文件时可以忽略。
4. 确认 NewAPI 版本；`rc.21` 的硬删除会继续被拒绝。
5. 为控制台创建独立 NewAPI 管理凭据，不要复用模型调用 Token 或本工具 `API_KEY`。
6. 配置 `NEWAPI_ADMIN_ACCESS_TOKEN` 和 `NEWAPI_ADMIN_USER_ID`。
7. 根据调用方最小权限配置 `API_KEY_ROLE`。
8. 为 `/metrics` 配置独立 `OBSERVABILITY_TOKEN`，或接受它返回 404。
9. 确认 Tool Store 实际路径有持久化卷映射并可备份。
10. 生产环境保持 `FRONTEND_BIND=127.0.0.1`，通过 HTTPS 反向代理访问。
11. 保持 `GEOIP_AUTO_DOWNLOAD=false` 和 `GEOIP_AUTO_UPDATE=false`，除非已明确批准运行时外网下载；如需更新，必须同时开启两个开关，且仍只接受发行版固定提交与校验和。
12. 确认宿主机已安装 Docker Compose v2.24.0 或更高版本；旧版 `docker-compose` v1 不受支持。

建议使用下面的 Compose 离线备份流程。它从运行中容器解析实际的 `TOOL_STORE_PATH`（未显式配置时按 `DATA_DIR/control-plane.db` 解析），保留 `.env`，并在复制 SQLite 前停止服务：

```bash
backup_dir="backups/v0.5.0-$(date +%Y%m%d%H%M%S)"
mkdir -p "$backup_dir"
cp -- .env "$backup_dir/.env"

tool_store_path="$(
  docker compose exec -T newapi-tools \
    sh -c 'readlink -f "${TOOL_STORE_PATH:-${DATA_DIR:-./data}/control-plane.db}"'
)"
docker compose exec -T newapi-tools test -f "$tool_store_path" || {
  echo "Tool Store 不存在或不可读: $tool_store_path" >&2
  exit 1
}
(
  set -e
  trap 'docker compose start newapi-tools >/dev/null' EXIT
  docker compose stop newapi-tools
  docker compose cp "newapi-tools:${tool_store_path}" "$backup_dir/control-plane.db"
  docker compose start newapi-tools
  trap - EXIT
)
```

不要在服务运行时直接 `cp` SQLite 文件。不能停机时，必须针对解析后的宿主机数据库路径使用 SQLite Online Backup API（例如 `sqlite3` 的 `.backup`）。自定义 `TOOL_STORE_PATH` 还必须有对应的持久化卷映射。

## 升级

推荐重新运行固定到发行 tag 的安装器：

```bash
INSTALLER_COMMIT_SHA=dc66439fcd2f1830bb955127101ebcb38fc40e72
INSTALL_SCRIPT_SHA256=26780fb76c281b09c4327588435848ffd35f184ce3cd790caedd6303914a9d21
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c -
NEWAPI_TOOLS_REF=v0.5.0 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST> \
NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA> \
bash "$install_script"
```

安装器 commit 与 SHA-256 已固定。必须从本发行页复制并替换 `<MANIFEST_DIGEST>` 和 `<RELEASE_COMMIT_SHA>`；不要执行仍含占位符的命令。

已有 checkout 应通过 `deploy.sh` 同时校验不可变 manifest digest 与发行 commit：

```bash
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST> \
NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA> \
bash ./deploy.sh
```

## 升级后验证

基础验证：

```bash
docker compose ps
curl -fsS http://127.0.0.1:1145/livez
curl -fsS http://127.0.0.1:1145/readyz
curl -fsS http://127.0.0.1:1145/api/health
docker compose logs --tail=200 newapi-tools
```

使用 JWT 验证依赖和能力矩阵：

```bash
curl -fsS \
  -H "Authorization: Bearer <JWT>" \
  http://127.0.0.1:1145/api/health/dependencies

curl -fsS \
  -H "Authorization: Bearer <JWT>" \
  http://127.0.0.1:1145/api/control-plane/newapi/capabilities
```

API Key 调用方应改用 `X-API-Key: <API_KEY>` 请求头。

使用独立观测 token 验证指标：

```bash
curl -fsS \
  -H "Authorization: Bearer <OBSERVABILITY_TOKEN>" \
  http://127.0.0.1:1145/metrics
```

首个生产写操作前，建议先用非破坏性对象验证：

- 请求带唯一 `Idempotency-Key`；
- 请求体带明确理由；
- Tool Store 中同时出现 intent 和 outcome；
- 审计记录包含同一个 request ID；
- NewAPI 拒绝或超时时，控制台不把结果标记为成功。

## 回滚

优先恢复升级前记录的 OCI digest：

```dotenv
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<previous-digest>
```

```bash
docker compose pull --include-deps newapi-tools
docker compose up -d --force-recreate --wait --wait-timeout 180 newapi-tools
```

注意：

- Tool Store 迁移不承诺自动向下兼容。
- 回滚镜像前保留 v0.5.0 的 `.env` 和按实际 `TOOL_STORE_PATH` 创建的数据库备份。
- 不要声称镜像回滚会自动降级 Tool Store 数据。
- v0.5.0 不修改 NewAPI schema，因此 NewAPI 表结构无需回滚。

## 已知限制

- 控制台不是代理网关，不提供请求级切流或熔断执行器。
- 渠道质量是旁路统计，不是自动路由决策。
- 价格快照和对账运行记录提供证据框架，但 v0.5.0 仍不是财务会计的唯一事实源。
- NewAPI fork 可能改变 Admin API 或日志语义；未知版本会按设计只读。
- 无状态 JWT 在到期或轮换 secret 前无法服务端单独撤销。
- Tool Store 为单实例 SQLite；部署多个写实例前需要设计明确的单写者或外部持久化方案。

## 质量门禁

发行工作流覆盖：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `govulncheck`
- 前端 ESLint、TypeScript/Vite build、`npm audit --omit=dev`
- Shell、Compose 和部署测试
- CodeQL
- 多架构镜像构建与 manifest 检查

## 镜像

```text
ghcr.io/yujianwudi/new_api_tools:0.5.0
ghcr.io/yujianwudi/new_api_tools:0.5
ghcr.io/yujianwudi/new_api_tools:<发行提交前7位SHA>
```

`latest` 跟随可变的 `main`。生产环境应使用 `0.5.0` 或 OCI digest。
