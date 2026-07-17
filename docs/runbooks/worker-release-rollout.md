# Worker Release Rollout 运维 Runbook

本 Runbook 描述当前 Worker Release API、Credential Binding、Canary/Promote/Rollback 和安全撤销边界。
它不是“rollout 已通过生产发布门禁”的声明。真实 Codex/Claude 四 Target gate、registry-pushed
multi-arch 镜像、多节点生产 Kubernetes 和 soak 尚未完成前，只能在明确授权的验收环境使用。

发布门禁见 `docs/release-checklists/stage-3-provider-runtime-remote-worker.md`；deterministic managed Docker
rollout 证据见 `docs/reports/stage-3-worker-release-rollout-d3af9380.md`，总体 Provider runtime 证据见
`docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`。

## 1. 核心不变量

- Worker Release Revision 是 immutable、Execution Target scoped，并绑定一个 immutable Worker Manifest。
- 每个 Target 只有一个 CAS Policy：一个 promoted Revision，最多一个 canary Revision。
- Policy Version 每次只增加 `1`；任何 stale `expectedPolicyVersion` 必须返回冲突。
- Worker 和尚未租用的 Execution 可以被 release-pinned；已租用 Execution 不得被静默改绑到另一 Revision。
- 持有 active Lease 的 Busy Worker 在替换期间必须保留原容器和 Generation；它按当前 release class 占用
  desired capacity，但不得错误占用另一 canary/promoted slot。只有 Execution 到达安全终态并释放 Lease 后
  才允许替换。
- managed Local、SSH、Docker 和 Kubernetes agentd 的 Lease renewal interval 从 Control Plane Worker Lease
  TTL 派生，约为 TTL 的三分之一；不得依赖可能长于权威 TTL 的固定默认值。
- Canary、Promote、Rollback 都写 immutable Transition、Audit 和 Outbox 记录。
- 每个 Target 最多一个 active `worker_image_pull` Binding；`000039` 在历史歧义未清理时拒绝升级。
- `000040` 要求最新 immutable Transition 与当前 Policy 的 Tenant、Target、Version、promoted、canary
  和百分比完全一致。
- 普通升级使用 Release Policy 和 graceful Drain；operator revoke 是不可逆安全动作，不是 rollout 快捷键。
- Registry Credential 只供 Control Plane Target provisioner 拉取 Worker Image，不能进入 agentd Workload、
  Provider Host、Event、Audit Metadata 或日志。
- 禁止直接修改 `worker_release_*`、`worker_instances`、`agent_executions` 或 Credential Binding 表绕过服务层。

## 2. 术语

| 术语             | 含义                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------ |
| Worker Manifest  | Worker Build、Git SHA、Image Digest、Protocol、Provider Runtime 和 Capability 的不可变证据 |
| Release Revision | Target scoped、单调递增的发布编号，绑定一个 Worker Manifest                                |
| Policy           | 当前 promoted Revision、可选 canary Revision、canary 百分比和 CAS Version                  |
| Transition       | immutable promote/canary/rollback 历史                                                     |
| Abort canary     | 调用 rollback API 指向当前 promoted Revision，仅移除 canary，不伪造新 promoted 镜像        |
| Operator revoke  | 不可逆撤销 Worker incarnation，并写 logical identity tombstone                             |

## 3. 预检

1. 完成 PostgreSQL 备份，并确认 `/ready=200`。
2. 确认当前镜像 embedded migration 与数据库 Checksum 一致；本实现边界为 `000041`。
3. 记录 Tenant、Execution Target、Worker Manifest、Image Digest、Commit SHA 和当前 Policy Version。
4. 从同一 clean SHA 运行 Registry supply-chain gate，确认双平台 manifest 可重复、`HIGH/CRITICAL=0`、
   Secret=0、非 EOSL、漏洞数据库未过期，并人工评审所有 `UNKNOWN` finding。
5. 只使用 Registry 返回的 immutable Digest；不要把可变 tag 当 Revision 身份。
6. 确认操作人具有 `worker.manage`；Registry Binding 还需要 `credentials.manage`。
7. 准备受保护的登录 Cookie Jar。不要把 Cookie、Token、Credential Payload 或完整 Registry auth
   粘贴到命令行历史、工单或聊天。
8. 对本次发布创建唯一 Idempotency Key，并为每个不同操作使用不同值。

在生产操作前，可从同一 clean commit 运行 deterministic managed Docker mechanics gate：

```bash
python3 scripts/stage3-provider-acceptance/docker_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --output-dir /tmp/synara-docker-worker-release-rollout \
  --timeout 3600
```

该命令使用 loopback-only disposable Registry、两个不同 Registry Digest、两 Worker 主 Target 和一个
candidate observer，验证 Revision、CAS Policy、canary/promote/rollback、活动 Execution 阻断、Audit、
Busy baseline Worker 容器/Generation 保持、终态后替换、Outbox、Event Sequence 与精确 cleanup。它不使用
生产 Registry Credential/TLS，也不执行真实 Provider、
Kubernetes 多节点、load 或 soak，因此不能替代下面的生产预检与观察窗口。

### 3.1 生产签名策略

`deploy/worker/signing-policy.json` 必须与发布 Commit 一起评审和提交。`ephemeral-key` 只用于 disposable
mechanics gate，不能作为生产批准。生产发布选择以下一种模式：

- `keyless`：配置短期 OIDC token 的环境变量名，以及获批的 certificate identity/regexp 和 OIDC
  issuer/regexp。Regexp 必须首尾锚定并兼容 RE2；token 值不得进入命令行、报告或聊天。
- `kms-key`：配置获批的 AWS/GCP/Azure/Vault KMS URI，以及最小 credential 环境变量名集合；值只注入
  gate 进程，不写入 Docker 参数或报告。

两种生产模式都要求 TLS Registry、transparency-log upload/verification 和 Registry auth 外部安全配置；
传入 `--insecure-registry` 必须失败。保存报告中的 signing-policy SHA、非 Secret identity/KMS reference、
tlog 结论和 Secret-state cleanup，不保存 token、KMS Credential 或完整 Registry auth。

示例环境变量只保存非 Secret 标识：

```bash
export SYNARA_ORIGIN='https://synara.example.com'
export SYNARA_TENANT_ID='replace-with-tenant-id'
export SYNARA_TARGET_ID='replace-with-execution-target-id'
export SYNARA_COOKIE_JAR='/secure/path/synara-cookies.txt'
```

读取当前状态：

```bash
curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/workers"
```

保存响应时只保留 Manifest/Revision/Policy/Worker ID、Version、Channel、Status 和时间；不要抓取数据库、
Credential Payload、Kubernetes Secret 或 Worker Environment 做“证据”。

## 4. 私有 Registry Credential

### 4.1 创建或轮换

通过受控 Settings UI 或安全 API 客户端创建 `purpose=registry` 的 Credential：

- `provider=oci`。
- `credentialType=basic`，Payload 只包含 `host`、`username`、`password`；Docker 和 Kubernetes 均可用。
- `credentialType=bearer_token`，Payload 只包含 `host`、`token`；当前仅 Docker 可用，Kubernetes
  明确返回 Unsupported，不能把 Token 伪装成 Basic。
- Scope 只能是 `tenant` 或与 Target 相同的 `organization`。

当前 Registry Credential `host` contract 只接受无端口的公共 hostname。镜像引用若使用自定义端口，
authority 解析会保留端口，但无法与现有 Credential selector 匹配，因此 provision/reconcile 必须安全
失败。不要通过手工 Kubernetes Secret、Docker auth 文件或数据库 selector 改写绕过该限制。

不要在 Runbook 命令中内联 Registry 密码或 Token。正常 Rotation 保持 Credential ID 和 Binding
identity，Credential Version 只增加一次；下一次 provision/reconcile 必须使用新版本。

### 4.2 建立 Target Binding

`worker_image_pull` 只能绑定 Execution Target，服务端从加密 Payload 派生 exact Registry Host selector：

```bash
export SYNARA_REGISTRY_CREDENTIAL_ID='replace-with-registry-credential-id'

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --data "{\"executionTargetId\":\"${SYNARA_TARGET_ID}\",\"credentialId\":\"${SYNARA_REGISTRY_CREDENTIAL_ID}\",\"bindingKind\":\"worker_image_pull\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/credential-bindings"
```

读取并确认只有一个 active `worker_image_pull` Binding：

```bash
curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/credential-bindings?executionTargetId=${SYNARA_TARGET_ID}"
```

镜像 Registry Host 与 immutable Binding selector 不一致时必须停止发布。不要修改数据库 selector；
应禁用旧 Binding，再为正确 Credential 创建新 Binding。

### 4.3 失效与泄漏处置

- 正常换钥：Rotate Credential，验证新 Version 后执行一次受控 image pull/canary。
- 确认泄漏：立即 Revoke Credential，并 Disable 对应 Binding；创建全新的 Credential 和 Binding。
- Kubernetes reconciler 在后续 foundation apply 中将 Target-scoped docker config Secret 更新为当前
  Credential 或空 auth map；Docker 只在 pull 请求中生成 `X-Registry-Auth`。
- 已经拉取到节点的镜像不会因 Credential Revoke 自动删除；按目标平台的镜像缓存策略单独处置。
- 不要把 Kubernetes docker config、Docker auth header 或 KMS 解密结果复制到事故记录。

禁用 Binding：

```bash
export SYNARA_BINDING_ID='replace-with-binding-id'

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --request POST \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/credential-bindings/${SYNARA_BINDING_ID}/disable"
```

## 5. 创建 Release Revision

先从 `GET /v1/tenants/{tenantID}/worker-manifests` 选择 Target 已观测且与不可变 Image Digest 一致的
Manifest。Manifest 不可用、Digest 缺失或 Provider Runtime 不兼容时不得创建发布 Revision。

```bash
export SYNARA_WORKER_MANIFEST_ID='replace-with-worker-manifest-id'
export SYNARA_CREATE_KEY="worker-release-create-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_CREATE_KEY}" \
  --data "{\"workerManifestId\":\"${SYNARA_WORKER_MANIFEST_ID}\",\"description\":\"approved immutable worker image\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases"
```

记录返回的 `id`、单调 `revision`、`workerManifestId`、`workerBuildGitSha` 和 `imageDigest`。

## 6. 初始 Promote

Target 尚无 Policy 时，初始 promote 使用 `expectedPolicyVersion=0`：

```bash
export SYNARA_REVISION_ID='replace-with-release-revision-id'
export SYNARA_PROMOTE_KEY="worker-release-initial-promote-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_PROMOTE_KEY}" \
  --data '{"expectedPolicyVersion":0,"reason":"initial approved worker release"}' \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases/${SYNARA_REVISION_ID}/promote"
```

如果 Policy 已存在，不得重用 `0`。重新 GET overview，使用当前 Version 和正确操作。

## 7. 启动 Canary

Canary Revision 必须比 promoted Revision 更新，且不能与 promoted 相同。比例范围是 `1..100`。

```bash
export SYNARA_CANARY_REVISION_ID='replace-with-newer-release-revision-id'
export SYNARA_POLICY_VERSION='replace-with-current-policy-version'
export SYNARA_CANARY_PERCENT='10'
export SYNARA_CANARY_KEY="worker-release-canary-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_CANARY_KEY}" \
  --data "{\"expectedPolicyVersion\":${SYNARA_POLICY_VERSION},\"reason\":\"approved canary window\",\"canaryPercent\":${SYNARA_CANARY_PERCENT}}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases/${SYNARA_CANARY_REVISION_ID}/canary"
```

任何 `409 worker_release_policy_version_conflict` 都表示状态已变化。重新读取 overview，确认操作者和
Transition 后再决定下一步；不要仅替换 Version 重放旧意图。

## 8. Canary 观察

至少验证：

1. Policy 中 promoted/canary Revision、百分比和 Version 与操作一致。
2. `GET /workers` 中 Worker 的 `workerReleaseRevisionId`、`workerReleaseChannel`、
   `workerReleaseStatus` 和 `workerReleaseReason` 可解释。
3. 新 Execution 在 Lease 前冻结正确 Revision/Channel；已租用 Execution 没有被静默改绑。
4. Busy promoted Worker 在 canary 启动期间保持原容器 ID、Generation 和 release pin，且 Target 不因 deferred
   replacement 被误标为 `offline`；该 Worker 不得占用 canary slot。
5. Canary 完成真实 Provider Describe、Start、Send、第二 Turn、Interaction、Artifact 和错误分类。
6. Worker replacement/Drain 没有重复终态、双 Worker 写入或 Generation 回退。
7. Control Plane 5xx、Execution failure/recovery、Worker offline、Artifact failure 和 Secret scan 无异常。
8. Docker/Kubernetes 实际运行镜像的 registry-returned Digest 与 Revision Manifest 一致。

当前 deterministic managed Docker gate 已使用两个不同 Registry Digest 验证第 3、4、6、8 项的 mechanics，
但 deterministic Provider fixture 不能满足第 5 项，也不能替代真实 Provider 与生产 rollout。

## 9. Promote Canary

只有当前 active canary Revision 可以 Promote：

```bash
export SYNARA_POLICY_VERSION='replace-with-latest-policy-version'
export SYNARA_FINAL_PROMOTE_KEY="worker-release-promote-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_FINAL_PROMOTE_KEY}" \
  --data "{\"expectedPolicyVersion\":${SYNARA_POLICY_VERSION},\"reason\":\"canary acceptance completed\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases/${SYNARA_CANARY_REVISION_ID}/promote"
```

若 Promote 返回 `409 worker_release_active_executions`，先核对 `releaseRevisionId` 与 `releaseChannel`；这可能
是仍在 promoted baseline 上运行的 Busy Execution，也可能是 active canary。保留旧容器并等待该 Execution
达到安全终态，不要重建 Interaction、强制释放 Lease 或直接改 release pin。Promote 成功后继续观察旧
Worker graceful Drain、Busy Execution 终态和新 Worker Claim，不要立即清理旧镜像。

## 10. Abort Canary 与真正 Rollback

### 10.1 Abort Canary

当 canary 失败但当前 promoted 仍安全时，调用 rollback endpoint，Revision ID 指向当前 promoted：

```bash
export SYNARA_PROMOTED_REVISION_ID='replace-with-current-promoted-revision-id'
export SYNARA_POLICY_VERSION='replace-with-latest-policy-version'
export SYNARA_ABORT_KEY="worker-release-abort-canary-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_ABORT_KEY}" \
  --data "{\"expectedPolicyVersion\":${SYNARA_POLICY_VERSION},\"reason\":\"canary failed acceptance\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases/${SYNARA_PROMOTED_REVISION_ID}/rollback"
```

数据库 `worker_release_transitions.action` 仍保存 `rollback`，因为 `000037` 的持久化 action vocabulary
固定为 `promote | canary | rollback`。`GET .../worker-releases` 的 API list 将该精确形状投影为
`abort-canary`，Audit Action 为 `worker_release.canary_aborted`，Outbox Topic 为
`worker.release.canary-aborted`。不要创建一条伪 Revision 表示“回到当前版本”，也不要让消费者只看
数据库 action 字符串猜测 abort 语义。

### 10.2 真正 Rollback

Rollback Revision 必须比当前 promoted 更旧：

```bash
export SYNARA_OLDER_REVISION_ID='replace-with-older-release-revision-id'
export SYNARA_POLICY_VERSION='replace-with-latest-policy-version'
export SYNARA_ROLLBACK_KEY="worker-release-rollback-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_ROLLBACK_KEY}" \
  --data "{\"expectedPolicyVersion\":${SYNARA_POLICY_VERSION},\"reason\":\"production regression rollback\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/execution-targets/${SYNARA_TARGET_ID}/worker-releases/${SYNARA_OLDER_REVISION_ID}/rollback"
```

服务可能拒绝会让已租用或 retired release Execution 失去安全归属的 Transition。遇到冲突时保留证据，
等待 Execution 到安全边界或执行经过评审的恢复；禁止直接改 release pin。

## 11. Worker Operator Revocation

只在 Worker Credential/Host 身份泄漏、Worker 被接管或必须永久禁止该 logical identity 时使用。

1. GET `/workers`，记录当前 `id`、`incarnation`、Target、Namespace/Pod identity 和活动 Lease。
2. 评估正在运行的 Execution 是否可能产生 outcome unknown 或 checkpoint unconfirmed。
3. 使用当前 incarnation 和唯一 Idempotency Key：

```bash
export SYNARA_WORKER_ID='replace-with-worker-id'
export SYNARA_WORKER_INCARNATION='replace-with-current-incarnation'
export SYNARA_REVOKE_KEY="worker-revoke-$(date +%s)"

curl --fail-with-body --silent --show-error \
  --cookie "${SYNARA_COOKIE_JAR}" \
  --header 'Content-Type: application/json' \
  --header "Idempotency-Key: ${SYNARA_REVOKE_KEY}" \
  --data "{\"expectedIncarnation\":${SYNARA_WORKER_INCARNATION},\"reason\":\"confirmed worker identity compromise\"}" \
  "${SYNARA_ORIGIN}/v1/tenants/${SYNARA_TENANT_ID}/workers/${SYNARA_WORKER_ID}/revoke"
```

响应中的以下计数必须记录并处理：

- `releasedExecutionLeases`。
- `recoveringExecutions`。
- `outcomeUnknownExecutions`。
- `checkpointUnconfirmedExecutions`。
- `requeuedWorkspaceCleanups`。

Revocation 会写 immutable Worker 状态与 logical identity tombstone，阻止旧 Token、Heartbeat、Claim、Lease
和同身份重新注册。若只是版本回退、计划内扩缩容或临时故障，使用 rollout/Drain/Recovery，不要 revoke。

## 12. 故障决策表

| 症状                           | 首选动作                                                      | 禁止动作                     |
| ------------------------------ | ------------------------------------------------------------- | ---------------------------- |
| Canary 功能回归，promoted 正常 | Abort canary                                                  | Revoke 正常 Worker           |
| Promoted 版本回归              | Rollback 到更旧 Revision                                      | 手工更新 Policy 表           |
| Stale CAS                      | 重新 GET 并评估 Transition                                    | 只替换 Version 重放旧请求    |
| Registry Credential 泄漏       | Revoke Credential、Disable Binding、创建新 Credential/Binding | 在日志中输出 docker config   |
| Worker logical identity 泄漏   | Operator revoke                                               | 删除 tombstone 后重新注册    |
| Busy Worker 升级缓慢           | 等待 Drain/安全边界                                           | 原地替换进程或强制复用 Lease |
| Checkpoint unconfirmed         | 保留 Workspace/Artifact 证据并人工恢复                        | 宣称空 Workspace 等价恢复    |

## 13. 发布后证据与清理

- 保存 Release Overview、Worker release fields、Execution pin、Audit/Outbox topic、错误码和时间线。
- 报告只引用非 Secret ID、Digest、Version、状态和计数。
- 保存 Registry-returned index/platform Digest、SBOM/SLSA identity、签名 identity/tlog policy、漏洞策略/DB
  identity、原始报告 SHA-256，以及所有非阻断 finding 的人工评审结论。
- 确认临时 Docker/Kind/SSH 资源使用精确 owner/Target 标识清理；禁止 prune 或广域 label 删除。
- 对报告、JSON、Markdown 和日志运行 Secret scan。
- 在 Release/PR 中链接本 Runbook、发布检查单、Acceptance Report 和已知限制。

## 14. 当前未关闭的发布风险

- 真实 Codex/Claude 尚未在 Local、SSH、Docker、Kubernetes 完成同 Commit 的统一 Release Gate。
- 当前 deterministic Kubernetes image-canary 不等于 registry-pushed immutable Revision rollout。
- 多节点生产 Kubernetes 的 Drain、PDB、云厂商 Eviction、CNI enforcement 和升级压力尚未证明。
- Clean commit `7659dd5f` 已证明 disposable Registry 上的 multi-arch reproducibility、默认 ephemeral
  exact-digest signing、SBOM、`HIGH/CRITICAL=0`、Secret=0、EOSL 与 DB freshness，并验证 checked-in policy
  可安全选择 keyless/KMS 路径；真实生产 signer identity、transparency-log entry/admission policy、Registry
  Credential/retention 和长时间 soak 尚未完成。证据见
  `docs/reports/stage-3-worker-registry-signing-policy-7659dd5f.md`。
- rollout/reconciler 在最终代码评审和门禁完成前不得标记为 production-approved。
