# NewAPI Tools v0.6.2 发行说明

`v0.6.2` 是 `v0.6.1` 运行时整改的供应链闭环发行版。它保留用户条件筛选、邀请充值证据、模型状态真值、Tool Store 可恢复升级与财务失败关闭等修复，并解决 Cosign v3.1.2 attestation 参数、CUE policy、恢复工作流 OIDC 身份和双平台 digest 消费校验问题。

## 为什么跳过 v0.6.1 Release

`v0.6.1` annotated tag 已创建且受保护，但 tag workflow 在生成自定义 SLSA provenance 时向 Cosign v3.1.2 `attest` 传入了不支持的 `-a` 参数。镜像 manifest 与普通 Cosign 签名已经存在，自定义 attestation 未完成，因此从未创建 GitHub Release。

标签内的安装/部署脚本只信任 tag 身份的 recovery；修复后的 recovery 必须从受保护 `main` 运行并获得 `refs/heads/main` OIDC 身份，两者无法在不移动旧标签的情况下同时成立。项目因此保留 `v0.6.1` 的 tag、tag object、commit 和历史失败记录，不更新、不删除、不发布，并由 `v0.6.2` 接续发行。

## 供应链修复

- `cosign sign` / `cosign verify` 继续用 annotations 绑定目标 tag 与 commit；`attest` / `verify-attestation` 不再传入 Cosign v3 不支持的 annotation 参数。
- 所有 Cosign CUE policy 临时文件使用明确的 `.cue` 后缀，并按 Cosign v3.1.2 实际生成的 in-toto `Statement/v0.1` 消费 SLSA v1 predicate；CI 真实执行离线签名、bundle 验签、DSSE 解码及正反 CUE policy。
- 正常 tag workflow 的 SLSA predicate 和 policy 精确绑定 repository、tag ref、revision、manifest digest、amd64/arm64 digest、builder identity 与 resolved dependency。
- 安装器和部署器从目标不可变 manifest 读取真实 amd64/arm64 子 digest，并把精确值写入闭合 CUE 结构；重复、缺失、额外平台或合法格式的伪造 digest 都失败关闭。
- recovery 只能从受保护 `main` 调度，只接受 `v0.6.2+` annotated tag，并把实际 workflow ref/SHA 与目标 tag object/commit 分开记录和验证。
- recovery 必须接收已记录的 `expected_manifest_digest`；exact image 缺失或 digest 不同即停止，不重建镜像、不覆盖 exact tag、不移动 minor alias。
- 每次 Sigstore 写入前后重新验证远端 tag object/peeled commit 与 registry manifest digest。

## 功能基线

- 用户管理筛选统一 list/count/stats/active/last-billed 语义，并对日志证据不可用返回明确 partial/unavailable。
- 邀请用户与充值明细绑定 `as_of`、query fingerprint、总数与内容 SHA-256；跨页内容漂移返回 409。
- 模型 Console/Embed 使用 fresh/stale/empty/unavailable 真值表，主动探测预留预算后才发网，重启不会重放 uncertain 请求。
- Tool Store v12 使用冻结 migration checksum；升级前执行 SQLite Online Backup 与校验，失败恢复旧配置、镜像和数据库。
- 红票关联与并发冲销失败关闭；不完整对账返回 `null` 与 `unreconciled`，不伪造可信 0。

## 安装前置条件

- Docker Engine，Docker Compose v2.24.0 或更高版本，以及 Docker Buildx；
- Linux 主机上的 `git`、`flock`、`sha256sum`、`realpath`、`stat`、`sync`，以及支持 `-k` 的 GNU/BusyBox `timeout`；
- 已备份并验证实际 `TOOL_STORE_PATH`、`.env`、Compose overlay/project identity 和当前镜像 digest/revision。

## 安装模板（替换全部占位符后才能执行）

以下模板故意保留 manifest digest 与 release commit 占位符。真实值只会在 `v0.6.2` tag workflow 的双架构构建、SBOM、Cosign 和 SLSA 全部通过后写入 GitHub Release；仓库内模板不可直接执行。

```bash
# TEMPLATE ONLY - DO NOT RUN UNTIL EVERY REMAINING REPLACE_WITH_* VALUE IS REPLACED
INSTALLER_COMMIT_SHA=5230df4175a47d272a003317a56347cae2d9997f
INSTALL_SCRIPT_SHA256=0797d2457c6a79e213968470ce1fafa4ac7474aabff5104ea7ebfcee382ac3a1
install_script="$(mktemp)"
trap 'rm -f "$install_script"' EXIT
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location \
  "https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh" \
  --output "$install_script"
printf '%s  %s\n' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1
NEWAPI_TOOLS_REF=v0.6.2 \
NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:REPLACE_WITH_64_HEX_MANIFEST_DIGEST \
NEWAPI_TOOLS_EXPECTED_REVISION=REPLACE_WITH_40_HEX_RELEASE_COMMIT \
bash "$install_script"
```

## 升级、备份与回滚

升级前必须保存并复核：

1. 当前完整镜像 digest 与 40 位 OCI revision；
2. 当前 `.env`、Compose project identity 与实际 overlay 集合；
3. 对真实 `TOOL_STORE_PATH` 执行 SQLite Online Backup 产生的数据库备份；
4. `PRAGMA integrity_check`、schema version、migration checksum、关键表计数与文件权限；
5. 备份文件 SHA-256 和受控存储位置。

候选服务不健康时，事务会恢复旧 dotenv、Compose 身份、不可变镜像和升级前 Tool Store 备份。应用镜像回滚不能自动把 v12 schema 降级；手工灾难恢复必须把配置、Compose、镜像和匹配的数据库备份作为同一组恢复。

## 发行验收门禁

- Go 全量、Linux race、vet、govulncheck；
- SQLite、MySQL 8.4.6、PostgreSQL 16.4 证据一致性；
- 前端 67 项测试、lint、typecheck、生产构建和两级 npm audit；
- 安装/部署/回滚、Tool Store 故障注入、性能、Cosign v3 CLI 与供应链结构测试；
- actionlint、CodeQL、amd64/arm64 构建；
- annotated `v0.6.2` 指向 exact `main` merge commit；
- exact manifest、两个平台 revision、SBOM、Cosign signature annotations、SLSA v1 predicate/CUE policy全部验证；
- 发布后 recovery 演练保持 tag object、peeled commit、exact digest 和 minor alias 不变。

任何占位符、失败或跳过的门禁都禁止创建 GitHub Release。

## 已知限制

- 邀请返利证据仍基于当前 `users.inviter_id`，没有事件时点关系，不能直接作为返利结算账本。
- 上游充值缺少可靠币种、精度或历史关系时，金额保持 `null/unreconciled`。
- 主动探测会产生真实上游请求，默认关闭；启用时必须使用低权限专用 key、白名单和预算。
- SQLite 性能门禁不替代生产 PostgreSQL/MySQL 容量测试。
- 390px、桌面宽度、深色模式、对比度、完整键盘路径和读屏仍需真实浏览器人工验收；组件测试不能替代该验收。
