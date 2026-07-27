# NewAPI Tools v0.6.0 发行说明

`v0.6.0` 将模型监测从单一 NewAPI 日志统计升级为“真实流量证据 + 预算受控主动探测”的模型可靠性控制台，并重构认证后台的模型监控交互。它继续保持旁路控制面边界：不代理真实用户模型流量、不修改 NewAPI schema，主动探测默认关闭，所有探测遥测只写入独立 Tool Store。

## 主要变化

### 模型可靠性控制台

- 新增异常优先首屏、紧凑表格、移动端独立布局和右侧诊断抽屉；
- 真实流量与主动探测分栏展示，不再混成一个不可解释分数；
- 只有两个新鲜数据源都健康时才显示“双源健康”；
- 无流量、未探测、能力未适配或数据过期均明确展示，不会冒充绿色；
- 支持模型名称、综合状态、NewAPI token group 和时间窗口组合筛选；
- 诊断抽屉展示真实流量时间槽、主动探测结果、错误分类、首字延迟、总延迟和最近 20 条历史；
- 公开模型状态嵌入页保持原组件与接口兼容。

### 模型目录与被动流量状态

- 模型目录合并 `logs` 与 `abilities`，无近期真实流量的目录模型仍可进入监控范围；
- 被动状态新增：
  - `traffic_health`；
  - `source_state`；
  - `last_traffic_at`；
- NewAPI `type=2` 继续作为成功事实，`type=5` 作为失败事实；Embedding、Rerank 等零 completion token 不会被误判为失败。

### 主动探测

- 支持 Chat Completions 流式探测；
- 支持 Responses、Embeddings 和 Rerank 能力适配；
- 图片、音频、TTS、转写等高成本或未适配能力默认安全跳过；
- 探测使用固定健康检查输入，不提供任意提示词或请求体执行器；
- 拒绝重定向，响应最多读取 64 KiB；
- 只保存协议/语义结果、HTTP 状态、延迟、有限错误分类和响应 SHA-256；
- 不保存 API Key、Authorization、原始响应或完整上游错误正文。

### 成本、并发与权限保护

- `MODEL_PROBE_ENABLED=false` 为默认值；
- 必须配置专用 `MODEL_PROBE_API_KEY` 与精确模型白名单；
- 每日请求预算、单次模型上限、并发、超时、最大输出 Token 和保留期均有服务端硬限制；
- 计划任务和手动任务使用进程内单任务租约，不能重叠；
- 服务启动时会恢复上一个进程遗留的 `running` 任务，按已保存明细重算计数并标记为 `cancelled/process_restarted`；
- 手动探测至少要求 operator；viewer 只能读取状态、汇总和历史；
- 手动探测要求 `Idempotency-Key`，相同请求重放不会产生重复探测费用。

### Tool Store v9

新增：

- `model_probe_runs`：任务、主体、状态与计数；
- `model_probe_attempts`：每模型脱敏探测明细；
- `model_probe_rollups`：小时级汇总。

attempt 与 rollup 在同一 SQLite 事务内写入。Dashboard 使用小时聚合与索引最新记录，不会把 30 天全部历史明细读入 Go 内存。过期探测遥测按 `MODEL_PROBE_RETENTION_DAYS` 清理，不影响发票、风险、审计、价格或其他 Tool Store 数据。

### 兼容性修复

- AutoGroup 审计恢复链路改为全程使用 `int64` 日志 ID，避免在 32 位 Go 目标（例如 `windows/386`）上把高位审计 ID 截断后错误报告“日志记录不存在”。

## 配置

升级后主动探测保持关闭。建议先使用最小白名单和低预算验证：

```dotenv
MODEL_PROBE_ENABLED=false
MODEL_PROBE_API_KEY=
MODEL_PROBE_MODELS=gpt-test,text-embedding-3-small
MODEL_PROBE_CAPABILITY_MAP=text-embedding-3-small=embeddings
MODEL_PROBE_INTERVAL_SECONDS=300
MODEL_PROBE_TIMEOUT_SECONDS=20
MODEL_PROBE_STALE_SECONDS=900
MODEL_PROBE_MAX_CONCURRENCY=2
MODEL_PROBE_MAX_MODELS_PER_RUN=20
MODEL_PROBE_DAILY_REQUEST_BUDGET=100
MODEL_PROBE_MAX_OUTPUT_TOKENS=4
MODEL_PROBE_RETENTION_DAYS=30
```

安全要求：

- `MODEL_PROBE_API_KEY` 必须是专用低权限模型调用 Token；
- 禁止复用 `NEWAPI_ADMIN_ACCESS_TOKEN`、NewAPI Tools `API_KEY` 或真实用户 Token；
- 白名单必须使用精确模型名；
- 只支持 Responses 的模型应显式配置 `model=responses`；
- 图片、音频等模型在有明确成本、配额和语义断言前保持 unsupported。

## API

```http
GET  /api/model-status/probes/status
POST /api/model-status/probes/summary
POST /api/model-status/probes/run
GET  /api/model-status/probes/history?model=<name>&limit=20
```

手动探测示例：

```bash
curl -fsS -X POST http://127.0.0.1:1145/api/model-status/probes/run \
  -H "Authorization: Bearer <ADMIN_JWT>" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: probe-$(date +%s)" \
  --data '{"models":["gpt-test"]}'
```

## 升级前检查

`v0.6.0` 首次启动会把 Tool Store 从 v8 前向迁移到 v9。升级前必须：

1. 记录当前运行镜像的不可变 digest 和 OCI revision；
2. 备份 `.env`；
3. 解析实际 `TOOL_STORE_PATH`；
4. 对 SQLite 使用 Online Backup API 创建一致性备份，不使用运行中普通文件复制；
5. 检查备份可读、权限为 0600 或受同等访问控制；
6. 确认 `MODEL_PROBE_ENABLED=false`；
7. 确认 Docker Compose v2.24.0 或更高版本。

示例：

```bash
backup_dir="backups/v0.6.0-$(date +%Y%m%d%H%M%S)"
mkdir -p "$backup_dir"
cp .env "$backup_dir/.env"
sqlite3 "${TOOL_STORE_PATH:-./data/control-plane.db}" ".backup '$backup_dir/control-plane.db'"
chmod 600 "$backup_dir/.env" "$backup_dir/control-plane.db"
sqlite3 "$backup_dir/control-plane.db" "PRAGMA integrity_check;"
```

## 安装和升级

发行安装必须同时固定 installer commit、installer SHA-256、镜像 manifest digest 和发行 commit。以下占位符必须从本发行页替换为真实值：

```bash
INSTALLER_COMMIT_SHA=<INSTALLER_COMMIT_SHA>
INSTALL_SCRIPT_SHA256=<INSTALL_SCRIPT_SHA256>
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1
NEWAPI_TOOLS_REF=v0.6.0 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST> \
NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA> \
bash "$install_script"
```

任何占位符未替换时不要执行。

升级后验证：

```bash
docker compose ps
curl -fsS http://127.0.0.1:1145/livez
curl -fsS http://127.0.0.1:1145/readyz
docker compose logs --tail=200 newapi-tools
```

登录后还应检查：

1. 模型可靠性页面能够列出目录模型；
2. 主动探测显示“已关闭”或“待配置”，没有意外请求；
3. Tool Store schema version 为 9；
4. 真实流量状态和数据新鲜度合理；
5. 配置专用 Token 后，仅对白名单中的 1–2 个低成本模型执行一次手动探测；
6. NewAPI 日志、探测历史和实际计费请求数一致；
7. 再逐步提高白名单、预算或开启周期调度。

## 回滚

快速停止主动探测：

```dotenv
MODEL_PROBE_ENABLED=false
```

然后重建 `newapi-tools` 服务。保留 v9 数据不会继续产生模型请求。

应用回滚必须恢复升级前记录的旧镜像 digest：

```dotenv
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<PREVIOUS_DIGEST>
NEWAPI_TOOLS_EXPECTED_REVISION=<PREVIOUS_COMMIT_SHA>
```

```bash
docker compose pull --include-deps newapi-tools
docker compose up -d --force-recreate --wait --wait-timeout 180 newapi-tools
```

注意：v0.5.2 不认识 Tool Store v9，镜像回滚不等于数据库自动降级。需要完整回滚时：

1. 停止 v0.6.0；
2. 封存当前 v9 Tool Store；
3. 恢复升级前 Online Backup；
4. 启动旧镜像；
5. 验证 livez、readyz、数据库和旧功能；
6. 不手工修改 `schema_migrations` 或删除迁移对象伪造降级。

## 验证状态

本地已通过：

```bash
cd backend
go test ./...
go vet ./...
govulncheck ./...

cd ../frontend
npm run lint
npx tsc --noEmit
npm run build
npm audit --omit=dev --audit-level=high --registry=https://registry.npmjs.org
```

本地还已通过 Bash 语法、部署 DSN、安全备份和事务部署/回滚测试；模型可靠性控制台完成桌面、诊断抽屉和 390px 移动端真实渲染 QA，控制台无 error/warn，移动端无横向溢出。

以下门禁以 GitHub Linux CI 为最终证据：`go test -race ./...`、Node 22 干净 `npm ci`、Compose 校验、供应链固定测试、多架构构建、SBOM、provenance 和 OCI revision 校验。本机缺少 GCC 与 Docker，且 Windows CRLF 会影响本地 WSL 的供应链正则校验，因此不能把这些本机限制误报为产品通过或失败。

## 已知限制

- v0.6.0 主动探测 NewAPI 的模型入口，不按具体上游 channel 探测；
- 任务租约为单进程内租约，多实例同时启用主动探测前应确保只有一个调度实例；
- 图片、音频、TTS 和转写默认 unsupported；
- 24h 聚合提供可用率和平均延迟，不提供延迟直方图或历史 p95；
- 探测预算以请求次数控制，不换算货币成本；
- 公开嵌入页没有暴露内部错误详情、预算或管理操作。

完整任务与验收标准见 [`docs/V0.6_MODEL_MONITORING_TASK_BOOK.md`](./docs/V0.6_MODEL_MONITORING_TASK_BOOK.md)。
