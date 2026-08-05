# NewAPI Tools v0.6.1 发行说明

`v0.6.1` 是一次面向生产安全的修复发行版，重点解决用户条件筛选、邀请关系与充值明细、模型状态监测体验，以及 v0.6.0 审计中发现的安装回滚、配置耐久性、财务证据和供应链身份问题。

## 发行重点

### 用户管理与邀请分析

- 修复 7/30/31 天和从未请求等活动条件筛选，所有分页、总数、活动等级与最后计费请求时间使用同一查询语义。
- 日志库独立部署时不再静默回退为未筛选全集；日志证据失败返回明确的 unavailable/partial 状态。
- OAuth 来源能力探测使用短 TTL，并在 schema 变化或查询失败时立即失效。
- 邀请用户详情修复父子请求竞态、分页统计和不存在邀请人的错误语义；跨页请求绑定首屏 `as_of` 与全量内容 SHA-256 指纹，内容漂移返回 409。
- 邀请充值父列表与详情统一 success/completed/1、完成时间、时区和半开日期区间。
- 每个父行携带详情 SHA-256 证据哈希；详情请求同时绑定 query fingerprint、as_of、总数和内容哈希。即使记录数不变但金额、用户、状态或时间发生替换，也会返回 409，不展示旧证据。
- 证据哈希默认最多完整读取 20,000 条成功充值记录，并为 list/summary/detail 统一设置 5 秒 deadline 和每实例 2 个重查询并发槽；超过上限返回 `422 INVITE_TOPUP_EVIDENCE_SCALE_EXCEEDED`，超时或排队取消返回 `503 INVITE_TOPUP_ANALYSIS_UNAVAILABLE`，绝不对截断数据生成哈希。
- 充值来源没有可靠币种、精度和历史邀请关系时保持 `unreconciled`，金额为 `null`，不把上游原始和或未知值宣传成返利。

### 模型状态监测

- Console 与公开 Embed 使用统一的 fresh/stale/empty/unavailable 真值表；失败或过期来源不能沿用旧绿色状态。
- 认证 `status/all` 最多返回 1000 个模型，并显式返回 `limit`、`returned`、`total_models`、`truncated` 和目录 `source_state`。
- 1000 个冷缓存模型由最多两条分块聚合 SQL 完成，不再逐模型执行 1000 条查询。
- 模型监控范围、主题、排序、阈值与自定义分组写入版本化 Tool Store 配置；耐久提交成功后才发布缓存失效消息并更新 L1。
- 配置写记录 actor、request id、reason、intent/outcome 和版本号；Redis 必需但不可用时快速返回 503，耐久版本不前移。
- 主动探测预算在发网前持久预留，区分 reserved/sent/skipped/settled/uncertain；重启恢复不会重复发送可能计费的请求。
- 改进桌面和窄屏详情、范围选择与对话框焦点恢复；支持键盘操作和 reduced motion。

### Tool Store、发票与回滚

- Tool Store schema 追加到 v12；v1-v9 历史迁移未重写，v10-v12 使用冻结 checksum。
- 重装和升级先解析真实数据路径，再执行 SQLite Online Backup、完整性/schema/权限/关键表计数校验；备份或恢复失败时保留旧服务和证据。
- 候选失败时恢复旧 `.env`、Compose 身份、镜像 digest/revision 与升级前数据库，不手工篡改 `schema_migrations`。
- 隔离候选容器名称绑定 Compose project 与事务 request id，并把容器 ID、项目路径、用途和不可变镜像写入事务证据；恢复前按 ID 停止，绝不删除同名替代容器。
- 红票必须关联存在、有效、同销售方、同币种和同精度的原蓝票；并发红冲不能超过原票金额。历史不匹配记录保留为 unreconciled；两级 health、未对账数和异常数未全部证明为正常时，净额为 `null`。

### 安全与供应链

- 管理密码、API key、JWT secret、NewAPI 管理 token、探测 key 和观测 token 禁止复用。
- 发布镜像继续生成多架构 SBOM，并新增签名的 SLSA provenance 消费策略。
- 安装器和部署器同时校验 OIDC issuer、仓库、workflow、tag ref、commit、manifest digest 与 amd64/arm64 平台 digest；任一身份字段伪造都会失败关闭。
- `main` 只允许通过 PR 和必需检查合并；release tag 的创建受授权发布者限制，已创建 tag 禁止更新和删除。

## schema 与升级前备份

本版本会把 Tool Store 从旧 schema 向前迁移到 v12。应用镜像回滚不能自动把 v12 数据库降回旧 schema，因此升级前必须备份实际 `TOOL_STORE_PATH` 指向的数据库。

升级前至少保存并复核：

1. 当前完整镜像 digest 与 40 位 OCI revision；
2. 当前 `.env` 和实际 Compose 文件集合；
3. 通过 SQLite Online Backup 生成的 Tool Store 备份；
4. `PRAGMA integrity_check`、schema version、迁移 checksum、文件权限和关键表计数；
5. 备份文件的 SHA-256 与受控存储位置。

不要在服务运行时直接复制 SQLite 主文件，也不要只备份容器镜像。

## 安装模板（替换全部占位符后才可执行）

下面是发行身份模板，不是可直接执行的安装命令。安装器 commit 与脚本 SHA-256 已固定到审计后的不可变对象；manifest digest 和 release commit 的 `REPLACE_WITH_*` 故意不是合法值，当前安装器会对未替换值失败关闭。

```bash
# TEMPLATE ONLY - DO NOT RUN UNTIL EVERY REMAINING REPLACE_WITH_* VALUE IS REPLACED
INSTALLER_COMMIT_SHA=2e46e6352f926c048fa996de51c53f2f40d9fbd0
INSTALL_SCRIPT_SHA256=c23bae15c239a4ae2b11bcd1013d53d76eb20d4ca40ecdd163a381052d103462
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1
NEWAPI_TOOLS_REF=v0.6.1 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:REPLACE_WITH_64_HEX_MANIFEST_DIGEST \
NEWAPI_TOOLS_EXPECTED_REVISION=REPLACE_WITH_40_HEX_RELEASE_COMMIT \
bash "$install_script"
```

真实 release commit 与 manifest digest 只在 tag 构建、签名和 provenance 全部验证后写入 GitHub Release。固定安装器来自提交 `2e46e6352f926c048fa996de51c53f2f40d9fbd0`，其 Git blob 内容 SHA-256 为 `c23bae15c239a4ae2b11bcd1013d53d76eb20d4ca40ecdd163a381052d103462`。

## 回滚

候选服务不健康时，安装/部署事务会自动恢复旧配置、旧镜像和升级前 Tool Store 备份。手工灾难恢复时也必须把这四者作为同一组恢复：

- 升级前 `.env`；
- 升级前 Compose 项目身份与 overlay 集合；
- 升级前不可变镜像 digest/revision；
- 升级前已校验 Tool Store 备份。

恢复后验证 `/livez`、`/readyz`、Tool Store schema/关键表、用户筛选、邀请详情、模型 Console/Embed 和发票汇总。不要把迁移后的 v12 数据库直接交给不支持该 schema 的旧镜像。

## 性能验收证据

候选代码的可重复门禁使用真实文件型 SQLite fixture、EXPLAIN、3 次预热和 20 次采样的 nearest-rank p95；阈值违约会使测试非零退出。

| 路径 | fixture | Linux/amd64 p95 | 门槛 |
|---|---:|---:|---:|
| 默认用户列表 | 100,000 users / 100,000 billable logs / 30 日 | 4.637 ms | 800 ms |
| 7 日 active 筛选 | 同上 | 271.811 ms | 800 ms |
| 邀请用户详情 | 99,999 invitees | 162.725 ms | 500 ms |
| 邀请充值列表 | 100,000 users / 99,000 top-ups / 30 日 | 592.337 ms | 1.5 s |
| 邀请充值 summary | 同上 | 192.136 ms | 1.5 s |
| 邀请充值详情 | 同上 | 56.535 ms | 500 ms |
| 20,000 行高偏斜邀请充值列表 | 单邀请人 20,000 条完整证据 | 345.017 ms | 1.5 s |
| 20,000 行高偏斜邀请充值详情 | 同上 | 128.241 ms | 500 ms |
| 认证模型 `status/all` | 1,000 models，冷应用缓存 | 115.936 ms | 2 s |
| 模型配置耐久提交 | 文件型 Tool Store | 0.477 ms | 500 ms |
| Redis 故障快速失败 | 文件型 Tool Store + Redis failure | 0.174 ms | 500 ms |

以上数字来自提交 `2e46e6352f926c048fa996de51c53f2f40d9fbd0` 的推送前 Linux/amd64 候选运行，最终 PR/main SHA 的性能 workflow 必须重新确认。它们不替代 PostgreSQL/MySQL 和实际存储硬件上的容量测试。已跟踪的 CI 矩阵覆盖真实 SQLite、MySQL 8.4.6、PostgreSQL 16.4、特殊字符搜索、NULL、精确十进制 canonical hash 与 PostgreSQL 高精度 `NUMERIC` 漂移；本地 SQLite 单元与性能路径已通过，完整三引擎结果等待最终 PR/main SHA 的 GitHub CI services。生产环境仍应持续观察同名 SLO，并在数据量或数据库方言变化时复测。

## 发行门禁

发布 commit 必须通过：

- Go 全量、Linux race、vet、govulncheck；
- 前端 67 项 Vitest/RTL/MSW、ESLint、TypeScript、生产构建和两级 npm audit；
- 安装/升级/回滚、Tool Store 故障注入、日志库恢复和供应链 Shell 测试；
- 10 万用户、30 天日志、邀请充值和 1000 模型性能工作流；
- Actionlint、CodeQL、amd64/arm64 构建；
- 二次只读审计无 P0/P1；
- annotated `v0.6.1` 指向已审核且属于 `origin/main` 的 commit；
- manifest、平台镜像 revision、SBOM、Cosign 签名、SLSA provenance、安装器 commit/hash 和 Release commit 完全一致。

## 已知限制

- 邀请充值分析基于当前 `users.inviter_id`，没有事件时点邀请关系；因此只能作为运营证据，不能用于返利结算。
- NewAPI 原始 top-up 数据没有足够的币种和精度快照时，金额保持 `null/unreconciled`。
- v0.6.1 不建设完整用户计费账户、利润中心、自动开票余额或税务平台集成。
- 主动探测会产生真实上游请求；默认关闭，启用时必须使用低权限专用 key、白名单和预算。
- SQLite 性能门禁不能代替生产 PostgreSQL/MySQL 容量测试。
- Tool Store v12 不承诺由旧镜像直接读取；向下回滚必须恢复升级前数据库备份。
- 390px、桌面宽度、深色模式、对比度、完整键盘路径与读屏的真实浏览器验收尚未执行；现有组件测试不能替代人工视觉与辅助技术验收，计划在取得 Playwright 许可或 CI 浏览器环境后于 v0.6.2 补齐。
