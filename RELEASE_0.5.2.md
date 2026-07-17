# NewAPI Tools v0.5.2 发行说明

`v0.5.2` 在 v0.5.1 独立、旁路、可审计控制面的基础上，交付第一版发票证据台账与“已开票金额”统计。发票数据只写入独立 Tool Store，不修改 NewAPI schema，不进入模型流量、计费或登录主链路。

本版本的定位是帮助运营与财务人员回答“已经录入了哪些发票证据、当前有效已开票金额是多少、谁在何时进行了什么操作”。它**不是税控系统、电子发票平台或会计总账**，不会代替真实开票、验真、报税和法定凭证保管。

## 发行重点

### 独立发票证据台账

- Tool Store schema 升级到 v8，新增 `invoice_documents` 与只追加的 `invoice_events`。
- 发票创建、作废事件与 `operation_audit` 在同一个 Tool Store 事务中提交，任一步失败都不会留下半完成记录。
- 除只读 CSV preview 外，所有写接口要求 `Idempotency-Key`，同一个逻辑请求可安全重试；把既有键用于不同负载会被拒绝。
- 单张创建、CSV 确认导入和作废分别以原始幂等键写入 `cp:<raw-idempotency-key>:intent/outcome` 审计链；只有完整链才能证明终态。
- 作废通过追加事件保留历史，不删除原始发票证据。
- 发票数据不会写入 NewAPI 主库、日志库或缓存，也不会调用 NewAPI Admin API。

### 金额与币种口径

- 单张发票金额使用正 `int64` 最小货币单位保存和传输，包括 `amount_minor` 与 `tax_amount_minor`；税额不得大于含税金额。
- 每条记录显式携带三字母 `currency` 和范围为 `0..9` 的 `minor_unit_scale`，不使用二进制浮点保存发票金额。
- `document_kind` 区分蓝票与红票；红票金额仍保存为正数，并通过 `related_invoice_number` 关联原蓝票，避免同一含义出现正负两种编码。
- 发票状态只允许 `issued -> voided`；文档不可删除，事件只追加。
- 汇总严格按 `currency + minor_unit_scale` 分组，不把 CNY、USD 等不同币种直接相加。
- 第一版统计有效蓝票金额、有效红票金额、作废金额、净已开票金额和对应票数；净已开票金额等于有效蓝票金额减去有效红票金额，已作废蓝票或红票不进入净额。
- 汇总字段包括 `blue_issued_minor`、`red_issued_minor`、`voided_blue_minor`、`voided_red_minor`、`voided_minor`、`net_issued_minor`、`effective_count` 与 `voided_count`。
- 服务端使用 Go `math/big` 对分组金额做任意精度汇总，累计值可以超过 `int64`；所有 summary、审计与事件金额均使用十进制字符串，禁止回落到浮点数或受限整数累计。
- “已提交但尚未完成”“可开票收入”“剩余可开票金额”和“开票率”不属于 v0.5.2 口径。

### 手工录入与 CSV 导入

- 支持单张发票证据录入。
- CSV 导入分为 preview 与 confirm 两步；预览阶段逐行返回校验错误，确认前不会写入。
- CSV 只接受 UTF-8，单次最多 1 MiB/500 行；preview 返回安全 DTO，不回显购方姓名或税号，并逐行拒绝晚于服务端时钟的未来开票时间。
- CSV 文本字段拒绝公式前缀，避免导出或复核时触发电子表格公式注入。
- 导入确认同样受 RBAC、幂等和操作审计保护。

### 查询、权限与审计

以下端点位于受认证的 `/api` 路由组：

| 方法与路径 | 最小角色 | 用途 |
|---|---|---|
| `GET /api/invoices/summary` | `viewer` | 按币种和最小单位精度查看已开票、作废和净额汇总；支持币种和 RFC3339 开票时间半开区间 |
| `GET /api/invoices` | `viewer` | 按状态、币种、RFC3339 开票时间半开区间查询脱敏列表，使用游标分页 |
| `GET /api/invoices/:id` | `operator` | 查看单张发票完整详情及事件 |
| `POST /api/invoices` | `operator` | 创建单张发票证据 |
| `POST /api/invoices/import/preview` | `operator` | 校验 CSV，不写入 |
| `POST /api/invoices/import/confirm` | `operator` | 确认导入并写入证据 |
| `POST /api/invoices/:id/void` | `admin` | 请求体只含 `reason`；作废时间由服务端 Tool Store clock 生成并追加事件 |

`viewer` 可以读取汇总和脱敏列表；`operator` 可以查看完整详情、录入和导入；`admin` 才能作废。列表会脱敏购方姓名和税号，系统不会把结构化 `buyer_name` 或 `buyer_tax_id` 自动复制到通用 `operation_audit` 与 `invoice_events`。写操作仍保留 actor、认证方式、请求 ID、幂等键、动作、目标和结果；登记、导入和作废理由是可审计自由文本，禁止填写购方姓名、税号等 PII。

所有金额响应均为十进制字符串；创建/导入请求接受十进制字符串或安全整数，但拒绝小数和指数形式。每个响应都返回 `capabilities={view_summary,view_list,view_detail,create,import,void}`，调用方不能只根据 HTTP 方法猜测权限。

### 控制台页面与失败语义

- 新增一级导航与路由 `/invoices`。
- 页面按币种展示有效蓝票、有效红票冲减、作废、净已开票、有效张数和异常；后端未提供异常指标时明确显示“未提供”。
- 日期筛选以 Asia/Shanghai 自然日生成 `[issued_from, issued_to)` 半开区间，避免结束日重复或遗漏。
- 前端使用 `BigInt` 精确格式化金额，不用 JavaScript `number` 汇总财务数据。
- capabilities 未知时详情和所有写操作默认隐藏；`401` 退出登录，`403/409/412/422/503` 显式展示失败原因。
- 刷新失败会保留上次成功数据并标记陈旧，`data_as_of` 来自响应 `generated_at`，不会把失败状态显示成金额 0。
- 浏览器只保存不含购方信息、金额或 CSV 内容的 pending marker。遇到断网、超时或未知结果时，必须先通过 `GET /api/control-plane/operations/:idempotency_key` 对账；旧操作终态可证明前，不能修改负载或换新键绕过锁重新提交。

## 安全与架构边界

- 不修改 NewAPI 表结构，不在 NewAPI 数据库创建发票 sidecar 表、触发器或索引。
- 不把 NewAPI 的 `quota`、充值余额或用户余额当作现金发票金额。
- 不从 `top_ups` 或 `subscription_orders` 自动推导开票资格，也不把订单创建时间当作支付完成时间。
- 不跨币种合计；无法识别的币种、精度、金额或日期输入必须返回明确错误，不能静默扩大为全量查询或把未知显示成零。
- Tool Store 及其备份包含购方名称和可选税号快照，必须按财务 PII 保护：文件权限至少 `0600`，使用加密磁盘或等价静态加密、受控备份、最小权限访问和明确留存/销毁策略。
- 结构化购方字段不会自动进入通用 `operation_audit`、`invoice_events`、访问日志或浏览器 pending marker；自由文本理由仍可能进入审计和事件，操作员不得在其中填写 PII。生产环境仍必须使用 HTTPS，不应把控制台直接暴露到公网。
- NewAPI Tools 故障、升级或 Tool Store 不可用时，不影响 NewAPI 数据面；发票写入会失败关闭，不会降级为未审计写入。

## 从 v0.5.1 升级

v0.5.2 首次启动会向前迁移 Tool Store。升级前必须备份 `.env` 和实际 `TOOL_STORE_PATH` 指向的 SQLite 数据库：

1. 记录当前镜像的完整 OCI digest 与 40 位 revision。
2. 按 README 的一致性备份流程解析实际 `TOOL_STORE_PATH`。
3. 停止 `newapi-tools` 后复制数据库，或者使用 SQLite Online Backup API；不得在服务运行时直接复制数据库文件。
4. 确认备份可读，以 `0600`、加密存储和受控访问保护，并按财务 PII 留存策略保留到 v0.5.2 完成业务验收之后。
5. 使用固定 manifest digest 和发行 commit 升级。
6. 核验 `/livez`、`/readyz`、Tool Store 健康以及发票汇总、录入、导入预览和权限拒绝路径。

Tool Store 迁移不承诺自动向下兼容。若必须回滚到 v0.5.1，应同时恢复升级前的 `.env`、旧镜像 digest/revision 和升级前 Tool Store 备份；不能只回滚应用镜像并继续使用已迁移数据库。

NewAPI 主库没有迁移，也无需为本版本执行任何上游 schema 变更。

## 安装

下列安装器仍固定到已经审计的不可变提交。执行前必须从 v0.5.2 发行页复制并替换真实的 `<MANIFEST_DIGEST>` 与 `<RELEASE_COMMIT_SHA>`：

```bash
INSTALLER_COMMIT_SHA=2d8a8f87c57f51b4dc49ce380f09d678e4a48b6b
INSTALL_SCRIPT_SHA256=773d4ce81a0cbb7e5b230bae5f6002745498876963cd3267a3cc54d214fdc419
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/$INSTALLER_COMMIT_SHA/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1
NEWAPI_TOOLS_REF=v0.5.2 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST> \
NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA> \
bash "$install_script"
```

任一占位符未替换时不要执行。安装或升级后至少核验：

```bash
docker compose ps
curl -fsS http://127.0.0.1:1145/livez
curl -fsS http://127.0.0.1:1145/readyz
docker compose logs --tail=200 newapi-tools
```

## 发行验证门禁

候选提交在创建 tag 前必须通过：

- `go test ./...`、`go test -race ./...` 与 `go vet ./...`；
- `govulncheck ./...`；
- 发票 Tool Store 迁移、事务回滚、幂等冲突、RBAC、作废事件和多币种汇总测试；
- CSV preview/confirm、公式注入、逐行校验与超限输入测试；
- 前端 lint、TypeScript 检查、生产构建和发票页面状态测试；
- `npm audit --omit=dev --audit-level=high`；
- 部署、回滚、供应链固定和 Compose 校验；
- 独立终审确认 Critical 0、Major 0。

发行 tag 必须是严格无前导零的 annotated `v0.5.2`，tag commit 必须已经属于 `origin/main`，且以下身份完全一致：

- `backend/internal/buildinfo/buildinfo.go` 中的版本；
- `frontend/package.json` 与 `frontend/package-lock.json` 中的版本；
- README 标题和版本徽章；
- 本发行文档标题与 `NEWAPI_TOOLS_REF=v0.5.2`；
- Git tag、OCI version label、镜像 manifest 和 GitHub Release tag。

Tag 构建完成后还必须核验：

- manifest 同时包含 `linux/amd64` 与 `linux/arm64`；
- 两个平台的 `org.opencontainers.image.revision` 都等于发行 merge commit；
- 两个平台均有 SPDX SBOM 与 SLSA provenance；
- 发行页记录的 manifest digest、tag 和 commit 完全一致。

## 已知限制

- v0.5.2 只记录发票证据和作废事实，不连接税务平台，不生成、签名、发送或验真电子发票。
- 不校验购方税号的法定有效性，不代替财务人员复核票面和税务合规。
- 红票只是人工录入或 CSV 导入的证据，不会调用税务平台自动开红票或验证其法律状态。
- 不支持退款、拒付、发票申请审批、附件归档或自动状态同步。
- 不关联 `top_ups`、`subscription_orders` 或线下收款，因此不能证明某张发票对应哪一笔收入。
- 不计算可开票收入、待开票金额、剩余可开票金额或开票率；在来源分配、退款/拒付台账和对账模型完成前，页面不得展示这些指标。
- v0.5.2 不提供发票导出；在具备脱敏、权限和导出审计闭环前，不通过前端临时拼接 CSV 绕过该边界。
- 手工和 CSV 录入的业务真实性取决于授权操作员。不可变事件与审计能够证明“谁录入了什么”，不能证明外部票据本身真实有效。
- 同一币种但错误的 `minor_unit_scale` 会形成独立汇总组，系统不会猜测并合并。

## 回滚

恢复升级前核验过的 OCI digest、revision、`.env` 和 Tool Store 备份：

```dotenv
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<previous-digest>
NEWAPI_TOOLS_EXPECTED_REVISION=<previous-release-commit>
```

```bash
docker compose pull --include-deps newapi-tools
docker compose up -d --force-recreate --wait --wait-timeout 180 newapi-tools
```

回滚后重新核验 `/livez`、`/readyz`、依赖健康和 Tool Store 版本。若恢复的是升级前数据库，v0.5.2 期间新录入的发票证据不会自动出现在 v0.5.1 中，应按组织的审计保留策略单独封存。
