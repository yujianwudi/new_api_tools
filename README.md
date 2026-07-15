<p align="center">
  <img src="./docs/images/banner.svg" alt="newapi-tool banner" width="100%" />
</p>

<p align="center">
  <img alt="Docs" src="https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87-E05243?style=for-the-badge" />
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26.5-00ADD8?style=for-the-badge&logo=go&logoColor=white" />
  <img alt="Gin" src="https://img.shields.io/badge/Gin-1.11-008ECF?style=for-the-badge" />
  <img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?style=for-the-badge&logo=react&logoColor=111827" />
  <img alt="Vite" src="https://img.shields.io/badge/Vite-8-646CFF?style=for-the-badge&logo=vite&logoColor=white" />
  <img alt="Database" src="https://img.shields.io/badge/DB-PostgreSQL%20%7C%20MySQL-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" />
  <img alt="Redis" src="https://img.shields.io/badge/Cache-Redis%20%7C%20Memory-DC382D?style=for-the-badge&logo=redis&logoColor=white" />
  <img alt="Docker" src="https://img.shields.io/badge/Docker-Ready-2496ED?style=for-the-badge&logo=docker&logoColor=white" />
  <img alt="Port" src="https://img.shields.io/badge/Port-1145-0EA5E9?style=for-the-badge" />
</p>

# NewAPI-Tool | NewAPI 增强管理中间件

**NewAPI-Tool** 是面向 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 的增强管理中间件。它以旁路方式连接 NewAPI 数据库和缓存服务，把仪表盘、充值审计、兑换码管理、风控分析、模型监控和运维配置集中到一个独立管理后台中。

它的核心原则是**零侵入运行**：不修改 NewAPI 源码，不改变 NewAPI 原有表结构，不接管 NewAPI 主服务流量；只在管理员需要审计、分析、批量处理或扩展运营能力时提供额外工作台。

## 项目信息

| 项目 | 说明 |
|---|---|
| 项目定位 | NewAPI 的增强管理层，用于可视化、审计、风控和后台运维 |
| 上游项目 | [QuantumNous/new-api](https://github.com/QuantumNous/new-api) |
| 本项目来源 | 基于 [james-6-23/new_api_tools](https://github.com/james-6-23/new_api_tools) 持续优化 |
| 路线图 | [P0–P4 产品与技术路线](./docs/ROADMAP.md) |
| 运行方式 | 独立容器 / 独立进程，连接 NewAPI 现有数据库 |
| 默认端口 | `1145` |
| 后端栈 | Go `1.26.5`、Gin `1.11`、sqlx、Redis、SQLite 辅助缓存 |
| 前端栈 | React `19`、Vite `8`、TypeScript、Tailwind CSS、ECharts |
| 数据库 | 生产优先 PostgreSQL / MySQL，查询字段以导出的真实 schema 为准 |
| 部署入口 | `install.sh` 一键部署，或 `docker-compose.yml` 手动部署 |
| 稳定镜像 | `ghcr.io/yujianwudi/new_api_tools:0.2.0`（`latest` 为可变的 `main` 构建） |

## 能力速览

| 模块 | 能力 |
|---|---|
| 统一仪表盘 | 汇总用户、令牌、渠道、模型、兑换码、请求趋势、活跃用户和系统规模。 |
| 充值审计 | 查询全量充值记录，按状态、渠道、时间和用户维度筛选，提供财务汇总、支付分布、漏斗和异常分析。 |
| 兑换码管理 | 批量生成兑换码，支持固定/随机额度、前缀、过期时间、高级筛选、复制和批量删除。 |
| 风控中心 | 查看高频请求、额度消耗、关联账号、同 IP 注册、Token 轮换、封禁记录和用户风险画像。 |
| 联合违规广播 | 接入独立 `newapi-tool-AbuseHub`，同步外部通报，本地匹配 email / OAuth / LinuxDo / IP 等身份线索，由管理员人工复核。 |
| IP 与日志分析 | 对大表 `logs` 做缓存化统计，提供 IP 分布、共享 IP、用户请求排行、模型使用和同步状态。 |
| 模型监控 | 配置模型状态看板、时间窗口、展示主题、刷新间隔、分组和可公开嵌入的模型状态页。 |
| 用户与令牌运维 | 用户列表、封禁/解封、软删除清理、令牌统计、分组预览和自动分组任务。 |

## 架构边界

- **零侵入**：NewAPI-Tool 只作为增强管理层运行，不要求改动 `new-api/` 源码。
- **不改 NewAPI schema**：所有涉及 NewAPI 数据表的查询和写入都遵循现有字段、类型和索引语义。
- **审计优先**：核心能力以查询、可视化、复核和运维辅助为主，批量写操作只覆盖明确的管理场景。
- **面向生产数据规模**：`logs` 等大表查询采用索引、缓存、超时和估算策略，避免无意义全表扫描。
- **双部署形态**：生产环境可用 PostgreSQL + Redis，单机或测试场景也可使用 SQLite / Memory 相关轻量缓存能力。

## 快速部署

### 方式一：一键脚本（推荐）

如果 NewAPI 已部署在 Linux 服务器上，可以使用一键脚本自动检测环境并部署：

```bash
bash <(curl -sSL https://raw.githubusercontent.com/yujianwudi/new_api_tools/v0.2.0/install.sh)

# 明确测试 main：安装器会把镜像锁到本次 checkout commit 的 7 位 SHA 标签
NEWAPI_TOOLS_REF=main bash <(curl -sSL https://raw.githubusercontent.com/yujianwudi/new_api_tools/v0.2.0/install.sh)
```

该安装器默认把仓库和镜像固定到 `v0.2.0`，不会静默跟随后续 `main`。选择 `main` 时会解析当前完整 commit，并使用 CI 为该提交发布的 7 位 SHA 镜像标签；如果镜像尚未发布，拉取会失败且旧容器不会先被停止。其他自定义 `NEWAPI_TOOLS_REF` 必须同时显式提供完整的 `NEWAPI_TOOLS_IMAGE`（tag 或 `repo@sha256:digest`），避免代码与镜像悄然错配。升级旧安装时，脚本会把旧 `NEWAPI_TOOLS_VERSION` 迁移为完整镜像引用。

脚本会自动定位 NewAPI 安装目录、读取数据库配置、生成必要密钥、设置管理员密码、配置 Docker 网络并启动服务。安全默认值 `FRONTEND_BIND=127.0.0.1` 只允许宿主机本地访问：

```text
http://127.0.0.1:1145
```

生产环境请用宿主机 Nginx / Caddy 将 HTTPS 域名反向代理到该地址。只有在已经配置防火墙、HTTPS 和访问控制且明确需要直接暴露时，才将 `.env` 中的 `FRONTEND_BIND` 改为 `0.0.0.0`；此时才能通过 `http://your-server-ip:1145` 访问。

### 方式二：Docker Compose 手动部署

适用于熟悉 Docker 的用户或非标准环境：

```bash
git clone --branch v0.2.0 --depth 1 https://github.com/yujianwudi/new_api_tools.git
cd new_api_tools
cp .env.example .env
vim .env
docker compose up -d
```

推荐使用 Docker Compose v2。若 NewAPI 使用 `network_mode: host`，叠加配置依赖 `!reset` 语法，要求 Docker Compose `v2.24+`；一键脚本会自动检查版本。

### 日志分库（LOG_SQL_DSN）自动兼容

部分 NewAPI fork 支持 `LOG_SQL_DSN`，把 `logs` 表整张分离到**独立日志数据库**。这种部署下主库的 `logs` 表会被冻结、不再更新——本工具若只连主库，则**仪表盘流量分析、使用日志、模型监控、风控 / IP 分析全部显示为 0**（其余如用户、令牌、兑换码数据正常）。

**无需任何额外操作**：上面的一键脚本 / `deploy.sh` 会自动检测 NewAPI 是否启用了 `LOG_SQL_DSN`，若启用则自动解析、做容器名 / 网络改写、写入工具 `.env` 并把工具容器接入日志库网络。NewAPI 未启用时则跳过（日志查询回落主库，行为不变）。

```bash
# 一键脚本已涵盖日志库；重新运行即可让已部署实例补上日志库连接
bash <(curl -sSL https://raw.githubusercontent.com/yujianwudi/new_api_tools/v0.2.0/install.sh)
```

> 单独修复 / 不想整体重部署时，也可只跑日志库脚本：
> ```bash
> bash <(curl -sSL https://raw.githubusercontent.com/yujianwudi/new_api_tools/v0.2.0/setup-log-db.sh)         # 检测并配置
> bash <(curl -sSL https://raw.githubusercontent.com/yujianwudi/new_api_tools/v0.2.0/setup-log-db.sh) --print # 仅预览脱敏目标，不改动
> ```
> 即使日志库一时连不上，后端也只会**降级为读主库**（日志暂时为空），不会崩溃。

### NewAPI Redis 与写操作安全边界

NewAPI 开启 Redis 后，会优先从缓存读取用户与 Token 鉴权状态。此时本工具若直接修改数据库，封禁、删除、禁用 Token、调整分组或开启 IP 记录可能不会立即生效。部署脚本会读取 NewAPI 容器的 `REDIS_CONN_STRING`：只有该变量**明确存在且为空**时才写入 `NEWAPI_REDIS_DISABLED=true`；变量缺失、读取失败或值非空时均保持 `false`，并阻止相关直接写库操作。Redis 开启时请通过 NewAPI 官方管理 API 完成这些变更。

按“不活跃”条件批量删除用户还受 `ALLOW_UNSAFE_BATCH_DELETE=false` 保护。旁路工具无法原子协调 NewAPI 的在途请求、异步 `request_count`、消费日志开关与日志保留期，因此不能仅凭一次日志查询证明用户可安全删除。默认请使用 NewAPI 官方管理 API；只有在已停止请求入口、排空全部在途请求、核验消费日志完整性并接受兼容风险后，才可临时同时设置 `ALLOW_UNSAFE_BATCH_DELETE=true` 和 `NEWAPI_REDIS_DISABLED=true`，操作完成后应立即恢复 `false`。破坏性预览快照只存于工具内置 Redis，并通过原子领取保证一次性；Redis 不可用时会失败关闭。

永久删除额外受 `ALLOW_UNSAFE_HARD_DELETE=false` 保护。NewAPI 官方删除流程还会清理二次验证、Passkey、OAuth 绑定等认证记录，本工具的兼容数据库路径无法跨版本保证完整性，因此默认禁止单用户硬删除、批量硬删除和永久清理。仅在明确接受孤儿认证记录风险、且确认 NewAPI Redis 已关闭时，才可同时设置 `ALLOW_UNSAFE_HARD_DELETE=true` 和 `NEWAPI_REDIS_DISABLED=true`。

解封用户不会自动恢复任何已禁用 Token。当前版本没有持久化“本次封禁具体关闭了哪些 Token”的可靠快照，批量恢复可能复活封禁前已因泄漏而停用的凭据；请在 NewAPI 管理端逐个复核后再启用。

自动强制 IP 记录默认关闭。`ENFORCE_IP_RECORDING=true` 会每 10 分钟把所有普通用户的 `record_ip_log` 改为 `true`，属于持续的数据采集与批量写入行为。仅在完成隐私/合规评估、已告知用户并确认业务确有需要时开启；手动“全部开启 IP 记录”操作不受该后台开关影响，仍会经过写操作安全边界。

### 管理会话与浏览器存储

管理 JWT 只保存在当前标签页的 `sessionStorage`，启动和登出都会清除旧版本遗留在 `localStorage` 的管理 token。兑换码生成结果只在即时结果弹窗中显示、复制或下载；IndexedDB 仅保存不含兑换码明文的生成摘要，首次升级会删除旧版浏览器中的明文生成历史。

当前 JWT 是无状态凭据，后端没有 denylist；客户端登出后，已经复制或泄漏的 JWT 在自身过期前仍可使用。生产环境应按运维便利与风险缩短 `JWT_EXPIRE_HOURS`（例如 `1`–`4` 小时）。怀疑 token 泄漏时应轮换 `JWT_SECRET` 并重启服务，这会使所有已签发 JWT 立即失效。

### 可信代理与登录限流

后端默认不信任客户端提供的 `X-Forwarded-For`。只有请求的直接 TCP 对端命中 `TRUSTED_PROXY_CIDRS` 时，才会从右向左剥离可信代理并确定客户端 IP。合并镜像默认只信任 loopback（`127.0.0.1/32,::1/128`），可防止公网直连时伪造 XFF 绕过登录和公开接口限流。若宿主机还有一层 Nginx/Caddy，请把**内层 Nginx 实际看到的外层代理地址**以精确 `/32` 或 `/128` 追加到列表；不要信任整个 `172.16.0.0/12` 等私网段。外层 Nginx 应继续使用 `proxy_set_header X-Forwarded-For $remote_addr;` 覆盖客户端原始头。

## 配置说明

推荐优先使用 `SQL_DSN` 配置完整数据库连接串；设置了 `SQL_DSN` 后，分离式 `DB_*` 配置会作为兼容兜底。

| 变量名 | 说明 | 示例/默认值 |
|---|---|---|
| `NEWAPI_TOOLS_IMAGE` | 完整部署镜像引用，支持稳定 tag、短 SHA tag 或 `repo@sha256:digest` | `ghcr.io/yujianwudi/new_api_tools:0.2.0` |
| `FRONTEND_PORT` | 对外访问端口 | `1145` |
| `FRONTEND_BIND` | 端口绑定网卡；默认仅本机可达，生产环境建议由 HTTPS 反代 | `127.0.0.1` |
| `ADMIN_PASSWORD` | 管理后台登录密码 | 必填 |
| `API_KEY` | 前后端内部 API Key | 部署脚本自动生成 |
| `JWT_SECRET` | JWT 签名密钥 | 部署脚本自动生成 |
| `JWT_EXPIRE_HOURS` | 无状态 JWT 过期时间（小时）；生产环境建议按风险缩短 | `24` |
| `CORS_ALLOWED_ORIGINS` | 允许跨域的精确 Origin，逗号分隔；留空仅允许同源 | 留空 |
| `CORS_ALLOW_CREDENTIALS` | 是否允许可信跨域请求携带凭据 | `false` |
| `LOGIN_MAX_ATTEMPTS` | 登录统计窗口内允许的失败次数 | `8` |
| `LOGIN_ATTEMPT_WINDOW_SECONDS` | 登录失败统计窗口（秒） | `900` |
| `LOGIN_BACKOFF_BASE_MS` | 登录失败指数退避基础延迟（毫秒） | `500` |
| `LOGIN_BACKOFF_MAX_SECONDS` | 登录失败最大退避时间（秒） | `30` |
| `PUBLIC_MODEL_MAX_BATCH` | 公开模型状态接口单次最大模型数 | `50` |
| `PUBLIC_MODEL_MAX_BODY_BYTES` | 公开模型状态接口最大请求体（字节） | `16384` |
| `PUBLIC_MODEL_REQUESTS_PER_MINUTE` | 公开模型状态接口每客户端每分钟请求上限 | `30` |
| `SQL_DSN` | 推荐的完整数据库连接串 | `host=... port=5432 user=...` |
| `LOG_SQL_DSN` | 日志专用库连接串（NewAPI 用 `LOG_SQL_DSN` 分库时才需要；留空则日志查询回落主库）。建议用 `setup-log-db.sh` 自动生成 | 可选 |
| `DB_ENGINE` | 兼容旧版分离配置的数据库类型 | `postgres` / `mysql` |
| `DB_DNS` | 数据库主机或容器服务名 | `postgres` |
| `DB_PORT` | 数据库端口 | `5432` / `3306` |
| `DB_NAME` | 数据库名称 | `new-api` |
| `DB_USER` | 数据库用户名 | `postgres` |
| `DB_PASSWORD` | 数据库密码 | 必填 |
| `DB_MAX_OPEN_CONNS` | 数据库最大打开连接数 | `50` |
| `DB_MAX_IDLE_CONNS` | 数据库最大空闲连接数 | `15` |
| `NEWAPI_NETWORK` | NewAPI 所在 Docker 网络 | `new-api_default` |
| `NEWAPI_BASEURL` | NewAPI 内部地址，用于需要回调上游的功能 | 可选 |
| `NEWAPI_REDIS_DISABLED` | NewAPI 是否已明确关闭 Redis；仅 `true` 时允许直接修改用户/Token/分组/IP 设置。部署脚本自动检测，未知状态按 `false` 处理 | `false` |
| `ALLOW_UNSAFE_BATCH_DELETE` | 是否允许旁路按日志/计数判定并批量删除不活跃用户；仅在已停流、排空请求并核验日志完整性后临时开启，还要求 `NEWAPI_REDIS_DISABLED=true` | `false` |
| `ALLOW_UNSAFE_HARD_DELETE` | 是否启用不完整的直接数据库永久删除兼容路径；还要求 `NEWAPI_REDIS_DISABLED=true`，优先使用 NewAPI 管理 API | `false` |
| `ENFORCE_IP_RECORDING` | 是否每 10 分钟强制所有普通用户开启 IP 记录；隐私敏感且还要求 Redis 安全前置条件 | `false` |
| `TRUSTED_PROXY_CIDRS` | 允许后端解析其 XFF 的精确代理 IP/CIDR；外层反代需追加实际来源地址，禁止宽泛私网段 | `127.0.0.1/32,::1/128` |
| `REDIS_PASSWORD` | 内置 Redis 密码 | 留空或自定义 |
| `TIMEZONE` | 服务时区 | `Asia/Shanghai` |
| `LOG_LEVEL` | 日志级别 | `info` |

## 联合违规广播接入

联合违规广播 Hub 独立部署在 `newapi-tool-AbuseHub/` 目录，默认使用 SQLite 和 `8888` 端口。Hub 管理员在 `/admin/` 创建命名密钥后，会得到一次性 `Secret`；密钥名称就是 NewAPI-Tool 侧的节点名称。

NewAPI-Tool 接入流程：

1. 进入前端「联合违规广播 → 接入状态」页，填写 Hub URL（推荐使用 `/v1/live` 后缀）、节点名称、密钥、拉取间隔，并勾选「启用拉取」后保存。
2. 配置变更立即生效，不需要重启后端进程。
3. 点击「连接 Hub」，Hub 收到心跳后会把该密钥激活为已连接节点。

之后 NewAPI-Tool 会定时拉取 `GET /v1/reports`，并把收到的通报写入本地 SQLite 缓存（`DATA_DIR/abuse-broadcast.db`），不修改 NewAPI 原有表结构。

## 本地开发

后端：

```bash
cd backend
go mod download
go run ./cmd/server
```

前端：

```bash
cd frontend
npm install
npm run dev
```

## API 端点

主要端点分组：

| 分组 | 端点 |
|---|---|
| 健康检查 | `GET /api/health`、`GET /api/health/db` |
| 认证 | `POST /api/auth/login`、`POST /api/auth/logout` |
| 仪表盘 | `GET /api/dashboard/*` |
| 充值 | `GET /api/top-ups`、`GET /api/top-ups/analytics/*` |
| 兑换码 | `GET /api/redemptions`、`POST /api/redemptions/generate` |
| 风控 | `GET /api/risk/*`、`GET /api/ip/*`、`POST /api/ai-ban/*` |
| 联合广播 | `GET /api/abuse-broadcast/*`、`POST /api/abuse-broadcast/*` |
| 模型状态 | `GET /api/model-status/*`、`GET /api/embed/model-status/*`、`POST /api/embed/model-status/status/multiple` |
| 用户与令牌 | `GET /api/users`、`GET /api/tokens`、`POST /api/tokens/search`（完整 Token 精确查询）、`GET /api/auto-group/*` |
| 存储与系统 | `GET /api/storage/*`、`GET /api/system/*` |

## 数据来源说明

本项目依赖 NewAPI 既有数据结构。涉及 NewAPI 数据访问、字段含义、列类型和索引时，应优先参考仓库内的真实生产库导出：

```text
pgsql_schema_export_20260505/
```

其中 `structure.txt` 用于快速确认表和列，`schema.sql` 用于查看完整建表、索引和默认值。`new-api/` 是上游 NewAPI 源码的只读参考目录，不应在本项目提交对它的改动。

## 贡献与支持

欢迎提交 Issue 和 Pull Request。改动数据库查询时，请同时确认 PostgreSQL / MySQL 的 SQL 差异，并避免对 NewAPI 原表结构做侵入式变更。

## License

[MIT License](./LICENSE)。本仓库基于 [james-6-23/new_api_tools](https://github.com/james-6-23/new_api_tools)（原作者 james-6-23 / kyx236）持续优化，并保留 yujianwudi 的后续修改署名。

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=yujianwudi/new_api_tools&type=Date)](https://star-history.com/#yujianwudi/new_api_tools&Date)
