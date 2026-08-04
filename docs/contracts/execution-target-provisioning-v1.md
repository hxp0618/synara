# Execution Target Provisioning v1

状态：Stage 7 `public-beta` 源码实现（2026-08-04；部署环境验收待完成）

## 边界

现有 `ssh/install`、`ssh/upgrade`、`ssh/revoke` 是第一方同步运维接口。一次请求跨越数据库、
SSH 上传、systemd、Worker 注册和 readiness；target generation fence 能阻止旧执行覆盖新状态，
但不能在 API receipt 与远端副作用之间建立跨进程的幂等提交点。因此外部 API 使用持久化异步
operation resource，内部同步路由继续保持 `internal`。

## HTTP 契约

创建：
`POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations`

- Tenant-scoped owner/admin Service Account 或有权用户 Principal；需要 `workers.manage`，target 的
  organization ownership 仍会逐次校验。
- 必须提供 `Idempotency-Key`；body 为 `{ "action": "install" | "upgrade" | "revoke" }`。
- v1 只接受 `kind=ssh` 的 tenant-owned target；local、platform-shared target fail closed。
- 首次返回 `202`；相同 actor、key、target 和 action 返回同一 operation，并设置
  `Idempotency-Replayed: true`；同 key 不同请求返回 `409 idempotency_conflict`。

查询：
`GET /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/provisioning-operations/{operationID}`

- 需要 `workers.read`，并重复执行 tenant/organization ownership 检查。
- 返回 `id`、`targetId`、`action`、`state`、`attempt`、时间戳、`result?`、`error?`。
- `result` 只允许 target/action/status/`binarySha256?`；`error` 只允许稳定 code 和安全 message。
  SSH 配置、key、passphrase、registration token、路径和远端输出禁止持久化到投影或响应。

## 状态机与幂等

```text
accepted -> running -> succeeded
                    -> failed
```

- `accepted` 是 durable receipt，不代表已连接远端；`running` 带 claim holder、expiry 和单调递增
  attempt generation；`succeeded` / `failed` 是 terminal。
- 失败后同一 key 仍返回原 operation；显式再次尝试必须使用新 key。
- 同一 target 同时最多一个非 terminal operation；冲突返回 `409 provisioning_in_progress` 和现有
  operation ID。v1 不提供 cancel；revoke 是独立的 authority-removal operation。

## Claim、恢复与 fencing

- leader-elected reconciler claim `accepted` 或 lease 已过期的 `running` operation；claim、attempt
  generation 和 expiry 在一个事务中提交。
- target 状态写入同时匹配 operation ID 和 SSH target generation；operation terminal commit 另外匹配
  claim holder 与 attempt generation，因此旧 claim 不能提交 operation 结果。
- install/upgrade 的 Worker instance UID 从 operation ID 稳定派生。claim takeover 必须识别同一
  operation 已创建的文件和 unit；不同 operation 遇到这些路径才是 `ssh_install_conflict`。
- revoke 先在本地事务撤销 Worker authority，再执行远端清理；远端失败保持 target offline 且
  authority 已撤销。相同 operation takeover 可继续清理，不能重新授权。
- 只有 exact Worker identity、lease/fencing、manifest 和 readiness 全部通过，install/upgrade 才
  succeeded；target 注册或 SSH 命令成功本身不等于 compute ready。

## Audit、指标与公开门禁

接受、claim、成功、失败记录 actor、scope、target、operation、action、attempt 和 request ID，禁止
配置与远端输出。指标覆盖 action/state、queue depth、oldest age 和 takeover，不以资源 ID
作为 label。API 请求使用现有限流/usage attribution，后台 reconcile 不重复计费。

源码公开门禁现覆盖 PostgreSQL partial unique/check/FK、claim lease/takeover、旧 claim commit 拒绝、
install 在远端命令成功后进程取消的同 operation 恢复、既有 revoke authority-first、Service Account
RBAC/Audit、安全响应投影，以及双 SDK create+poll 的真实 HTTP/SQLite conformance。因此两个 route
进入 `public-beta` / `codegen-ready`，但不等于已部署或 GA。部署发布仍要求真实双副本 takeover、真实
SSH target 演练、queue/oldest-age/takeover 指标告警、运行时长 SLO 和 rollback 证据。
