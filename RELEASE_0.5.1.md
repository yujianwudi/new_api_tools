# NewAPI Tools v0.5.1 发行说明

`v0.5.1` 是 v0.5.0 独立旁路控制面路线的可靠性与安全审计修复版本。它不进入模型流量主链路，不修改 NewAPI schema，也不扩大未经验证的自动写入能力。

本版本重点解决两类风险：一是上游超时、网络中断或审计尾部失败后，运营人员无法证明操作到底有没有生效；二是安装、升级、日志库切换或卸载过程中，配置和服务状态可能被并发操作或中途崩溃破坏。

## 发行重点

### 可证明的写操作终态

- 新增 `GET /api/control-plane/operations/:idempotency_key` 对账端点。
- 对账严格绑定当前 actor、认证方式、动作、目标和幂等键；不同主体或认证方式不能互相读取操作结果。
- 先验证完整 intent/outcome 链，再返回 `pending`、`succeeded`、`failed`、`denied` 或 `cancelled`。
- 孤立 outcome、身份篡改、负载不一致或损坏链返回 `503`，不伪装成 `404`，也不泄漏审计载荷。
- 用户与兑换码写操作使用总 deadline，并为 outcome 审计保留尾部预算；超时或不确定结果必须先对账，不能盲目重试。

### 前端失败关闭与幂等所有权

- 用户启停、用户删除和相关高风险操作在 `sessionStorage` 中保存无敏感负载的 pending marker。
- failed/denied 的二次解除候选绑定 `pending key + action + target`；旧 K1 不能解锁同目标的新 K2。
- 登出仅清理可复用凭据，不会丢弃无法证明终态的 pending marker。
- 兑换码明文仍只在当前成功或一次性救援响应中交付，不写入 Tool Store，关闭结果后不可恢复。

### 经营数据、导出和 Tool Store 正确性

- `average_response_time` 继续保持 v0.5.0 已发布的毫秒兼容契约；`average_response_time_ms` 同样使用毫秒，避免外部看板静默缩小 1000 倍。
- 用户排序改为固定 SQL 字面量映射，移除动态排序表达式。
- 充值月份、异常上限、自动分组批次和预分配均增加显式边界，避免超大输入、溢出和内存放大。
- 充值 CSV 导出先记录 intent；若 outcome 审计写入失败，会持久化可重放的 reconciliation record，而不是丢失审计尾部。
- Tool Store 列表使用稳定 tuple cursor；客服备注更新和删除保留不可变历史证据。
- CSV 导出继续防止公式注入，HTML 边界测试不再依赖脆弱的标签过滤正则。

### 健康检查与读取韧性

- 健康探针使用 singleflight 合并并发回源，限制依赖故障时的探测放大。
- 主库、日志库、Redis、Tool Store 与 NewAPI 诊断继续独立表达健康、新鲜度和降级状态。
- 前端分析与模型状态请求增加批处理边界，避免大页面同时制造无界并发。

### 原子安装、升级和卸载

- `deploy.sh`、`install.sh` 与 `setup-log-db.sh` 使用同一个项目级排他 `flock`，覆盖安装、升级、重配、日志库切换、卸载、purge 和重装入口。
- 锁文件位于项目目录同级，要求当前 uid 所有、权限 `0600`，拒绝 symlink、目录、foreign-owner 和身份替换。
- install → deploy 通过继承的锁 fd 交接，不释放重抢；进程退出或 `SIGKILL` 后由内核自动释放。
- `.env` 和回滚快照使用 `0600 + file sync + atomic rename + parent sync`，拒绝不安全的既有文件和竞态替换。
- 升级会在停旧服务前预拉取候选及依赖，把旧 mutable tag 固定到唯一同仓库 digest，并保存完整旧 Compose overlay、network、project identity 和配置快照。
- 候选必须通过 OCI revision、`docker compose up -d --wait` 和语义 readiness；失败会恢复旧配置和旧健康服务。
- 卸载和 purge 只有在服务清理成功后才删除恢复材料，避免下一次安装复活旧秘密或半完成事务。

### 安全日志库接入

- `setup-log-db.sh` 要求 Docker Compose v2.24.0+。
- 支持 PostgreSQL URL/keyword DSN、MySQL URL/Go DSN、IPv6 和保留查询参数的安全 host/port 改写。
- 拒绝 PostgreSQL `hostaddr`、query host/port、percent-encoded 路由键等覆盖路径。
- `.env` 与 Compose override 两阶段原子提交；并发用户修改、symlink 或非普通文件会失败关闭。
- 候选日志库配置必须通过 `up --wait`；失败时恢复旧配置并重新验证旧服务。

### 发行与供应链门禁

- GitHub Actions tag 门禁要求严格无前导零的 `vMAJOR.MINOR.PATCH`、annotated tag、tag commit 属于 `origin/main`，且 buildinfo、README、发行文档和安装 ref 一致。
- Docker、Go、Node、Redis 和 GitHub Actions 依赖继续固定到受审计版本或 digest。
- 发布 `linux/amd64` 与 `linux/arm64` 镜像，并生成 SPDX SBOM 与 SLSA provenance。
- 生产安装必须使用发行页核验过的 OCI manifest digest 和 40 位发行 commit，不能依赖可变 tag。

## 兼容性与边界

- 已验证 NewAPI 基线仍是 `v1.0.0-rc.21`；该版本的永久删除继续禁用。
- 不创建、不迁移、不修改 NewAPI 表结构；新增持久状态只写入独立 Tool Store。
- 不代理模型请求，不参与 NewAPI 请求、计费或登录主链路。
- 所有 Compose 路径要求 Docker Compose v2.24.0 或更高版本；旧版 `docker-compose` v1 被拒绝。
- 项目级 `flock` 是宿主机 advisory lock：项目脚本之间会互斥，但具有文件权限的 root 或编辑器仍可绕过脚本直接改文件。
- 前端 pending marker 当前是单标签页 `sessionStorage` 范围；跨标签页目标级租约留待后续版本。

## 安装

从本发行页复制真实的 `<MANIFEST_DIGEST>` 与 `<RELEASE_COMMIT_SHA>`。下列安装器固定到最终审计修复提交，不依赖可变分支：

```bash
INSTALLER_COMMIT_SHA=80199a9182c9c2e4c8771b4c90fac2952ee0f331
INSTALL_SCRIPT_SHA256=300eebfd65fda0d13914f4222ad212a3277660d2f5bdf01ef0f57761e2bd4185
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1
NEWAPI_TOOLS_REF=v0.5.1 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST> \
NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA> \
bash "$install_script"
```

任一占位符未替换时不要执行。安装后至少核验：

```bash
docker compose ps
curl -fsS http://127.0.0.1:1145/livez
curl -fsS http://127.0.0.1:1145/readyz
docker compose logs --tail=200 newapi-tools
```

## 从 v0.5.0 升级

1. 记录当前应用镜像的完整 OCI digest。
2. 备份 `.env`。
3. 解析实际 `TOOL_STORE_PATH`，停止 `newapi-tools` 后复制数据库，或使用 SQLite Online Backup API 创建一致性备份。
4. 确认 Docker Compose 版本不低于 v2.24.0。
5. 使用上面的固定安装器和本发行页的真实 digest/revision 执行升级。
6. 核验 `/livez`、`/readyz`、受保护依赖健康、Tool Store 可读性和 NewAPI 能力矩阵。

升级脚本会在候选失败时恢复旧 digest、旧配置和旧服务；不要在升级过程中并行运行另一个安装、部署或卸载命令。

## 验证门禁

本发行候选在提交前通过：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `govulncheck ./...`：可达漏洞 0、导入包漏洞 0
- 前端 expiry、secret-storage、control-plane reliability 回归
- ESLint、TypeScript、Vite production build
- `npm audit --omit=dev --audit-level=high`：0 vulnerabilities
- 部署、回滚、崩溃恢复、锁与 DSN 故障注入测试
- setup-log-db 安全测试与供应链固定门禁
- 独立终审：Critical 0、Major 0

Tag 构建完成后还必须核验：

- manifest 同时包含 `linux/amd64` 与 `linux/arm64`；
- 两个平台的 `org.opencontainers.image.revision` 都等于发行 merge commit；
- 两个平台均存在 SPDX SBOM 与 SLSA provenance；
- 发行页记录的 manifest digest、tag 和 commit 完全一致。

## 回滚

恢复升级前核验过的 OCI digest，并使用升级前的 `.env` 与 Tool Store 备份：

```dotenv
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<previous-digest>
NEWAPI_TOOLS_EXPECTED_REVISION=<previous-release-commit>
```

```bash
docker compose pull --include-deps newapi-tools
docker compose up -d --force-recreate --wait --wait-timeout 180 newapi-tools
```

Tool Store 迁移不承诺自动向下兼容。回滚应用镜像前必须保留并核验数据库备份，不能把镜像回滚等同于数据自动降级。
