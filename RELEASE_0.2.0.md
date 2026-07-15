# NewAPI Middleware Tool v0.2.0 发行说明

`v0.2.0` 建立了面向生产环境的安全基线，并补齐部署、测试与多架构镜像发布门禁。本版本包含安全默认值和写操作行为变化，建议所有自建实例在升级前先阅读下方注意事项。

## 主要变化

### 自 v0.1.0 起的功能演进

- 后端从早期 Python 实现演进为 Go（Gin + sqlx），支持 PostgreSQL 与 MySQL 部署。
- 完善仪表盘、充值分析、日志与模型监控、兑换码、用户和 Token 管理等核心控制台能力。
- 新增联合违规广播接入、本地事件缓存和报告匹配流程。
- 改进移动端交互、长列表操作反馈、错误提示和数据为空时的降级展示。
- 修复额度换算、Token 状态、充值聚合、日志新鲜度和多项风控查询的数据正确性问题。

### 认证与接口安全

- 登录加入按客户端地址统计的失败限流和指数退避。
- JWT、API Key、管理密码及错误响应采用更严格的校验和脱敏策略。
- CORS 默认仅允许同源；跨域访问必须显式配置精确可信 Origin。
- 公开模型状态接口增加请求体、批量大小和频率限制。
- 外部 HTTP 请求统一经过 SSRF 防护，阻止环回、私网、链路本地及不安全重定向目标。
- 可信代理改为显式 CIDR 配置，避免伪造转发头绕过限流或审计。

### NewAPI 数据安全边界

- 当 NewAPI 的 Redis 状态为启用或未知时，默认阻止直接修改用户、Token、分组和 IP 设置，避免数据库与鉴权缓存不一致。
- 永久删除兼容路径默认关闭；只有明确接受风险并同时满足安全前置条件时才可启用。
- 持续强制所有用户开启 IP 记录改为显式选择加入，默认不覆盖用户的隐私设置。
- 解封用户不再批量恢复 Token，避免复活封禁前已手工停用的泄漏凭据。
- 批量操作增加数量上限、事务边界、失败回滚和更清晰的错误分类。
- 充值、兑换码、风控、模型状态及联合违规广播相关接口补充负向测试和信息脱敏。

### 部署与运行安全

- 后端默认只监听容器内回环地址，前端端口可绑定 `127.0.0.1` 后交由 HTTPS 反向代理暴露。
- 一键部署自动生成 API Key、JWT 密钥和高强度管理密码，并避免在错误日志中输出数据库 DSN。
- 日志分库辅助脚本不再打印含密码的 DSN，并在写入或迁移后将 `.env` 权限收紧为 `600`。
- 自动检测 NewAPI 的 `LOG_SQL_DSN` 日志分库配置，兼容独立日志数据库和附加 Docker 网络。
- 数据库健康检查不再向客户端泄漏底层连接错误。
- 精确 Token 查找改用 POST 请求体，内置 Nginx 访问日志也不再记录查询串或 Referer。

### 工程质量与交付

- CI 增加 Go 测试、`go vet`、`govulncheck`、前端 lint/build/npm audit、Shell/Compose 校验和部署 DSN 测试。
- Docker 镜像改为按 digest 构建并合并 `linux/amd64`、`linux/arm64` 多架构 manifest。
- 前端工具链和依赖升级，并锁定已知高风险的传递依赖修复版本。
- 新增后续产品与工程路线图：`docs/ROADMAP.md`。

## 升级注意事项

1. 升级前备份现有 `.env`，并记录当前镜像 digest 以便回滚。
2. 推荐重新运行 `install.sh`，让脚本补齐新配置并同步 NewAPI 的 Redis 安全状态。
3. 手动部署时必须对照新的 `.env.example` 检查 `ADMIN_PASSWORD`、`API_KEY`、`JWT_SECRET`、`SERVER_HOST`、`FRONTEND_BIND` 和 `TRUSTED_PROXY_CIDRS`。
4. 默认 `FRONTEND_BIND=127.0.0.1`，需要通过宿主机 HTTPS 反向代理访问；不要在缺少访问控制时改成公网绑定。
5. NewAPI 使用 `network_mode: host` 时要求 Docker Compose `v2.24+`。
6. 如果 NewAPI 使用 Redis，直接数据库写操作会按设计失败关闭；请优先改用 NewAPI 官方管理 API。
7. `ALLOW_UNSAFE_HARD_DELETE` 默认保持 `false`。不要仅为恢复旧行为而开启它。
8. `ENFORCE_IP_RECORDING` 默认保持 `false`；开启前应完成隐私与合规评估。
9. 如果 NewAPI 配置了 `LOG_SQL_DSN`，请确认工具已连接日志库所在网络；部署脚本会自动处理常见容器场景。
10. 跨域部署需要设置 `CORS_ALLOWED_ORIGINS`；留空表示只允许同源访问。

## 兼容性与已知限制

- 本版本不修改 NewAPI schema，也不会自动向 NewAPI 数据库创建索引。
- PostgreSQL / MySQL 和日志分库路径已进入自动化门禁，但仍建议先在与生产配置一致的预发布实例演练升级。
- 发行前尚未覆盖每一种 NewAPI fork、支付渠道和第三方 Abuse Hub 的真实端到端组合。

## 回滚

1. 将部署镜像恢复到升级前记录的 digest。
2. 恢复升级前备份的 `.env` 并重新创建容器。
3. 本版本没有 NewAPI schema 迁移，因此不需要数据库结构回滚。

完整变更比较：<https://github.com/yujianwudi/new_api_tools/compare/v0.1.0...v0.2.0>

## 镜像

```text
ghcr.io/yujianwudi/new_api_tools:0.2.0
ghcr.io/yujianwudi/new_api_tools:0.2
ghcr.io/yujianwudi/new_api_tools:latest
```

镜像同时支持 `linux/amd64` 与 `linux/arm64`。
