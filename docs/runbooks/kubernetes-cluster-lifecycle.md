# Kubernetes Cluster 生命周期运维 Runbook

适用范围：Stage 4 enterprise / single-node Control Plane 管理的 Kubernetes Execution Target，包括接入、
计划升级、路由下线、Credential 轮换和事故隔离。Worker Release 发布细节见
[`worker-release-rollout.md`](worker-release-rollout.md)，Control Plane 与数据库处置见
[`control-plane-operations.md`](control-plane-operations.md)。

本文只描述当前产品实际具备的权威操作。正式支持边界是 operator 管理的自建 Kubernetes；OrbStack/Kind 提供
checked-in 机制与回归证据，目标硬件上的长时 soak 由 operator 使用同一 runner 完成。EKS/GKE/AKS 专属
Workload Identity、AWS/GCP/Azure 原生账单导出、托管云多可用区及跨云 E4/E5 当前明确 deferred，不是接入前置项。
代码里的 “managed Kubernetes Target” 表示由 Synara Reconciler 管理的 Target，并不表示云厂商托管 Kubernetes。

## 1. 当前能力边界

| 操作 | 当前权威入口 | 边界 |
| --- | --- | --- |
| 创建 Target / Group / Member | Tenant API | 支持；Target 加密配置不会回显 |
| 停止 Group Member 的新调度 | Member `PATCH status=draining` | 支持；不打断已有 Execution |
| 放弃计划维护 | Member `PATCH status=active` | 只允许 `draining -> active` |
| 永久停止一个 Member 的路由 | Member `PATCH status=disabled` | 支持；存在非终态 Group Execution 时拒绝，`disabled` 不可恢复 |
| 触发 Region/Cluster evacuation | Location Outage `PUT` | 支持；TTL authority，只迁移满足 lease-free/DR 门禁的 Execution |
| SSH Target 物理撤销 | SSH `revoke` API | 支持，先提交本地 fence 再清远端 |
| 托管 Kubernetes Target 终态 disable | Target `POST .../kubernetes/disable` | 支持；与 Reconciler 同锁，全部 durable/health 门禁通过后才提交 |
| Kubernetes Target 历史行 delete/reactivate | 无 | **不支持且不需要**；`disabled` 行保留历史 FK 与审计，不得回写 |
| 已有固定 Target Session 改绑 Group/Target | 无通用迁移 API | **不支持**；存在此类 Session 时 disable 会拒绝，需归档或保持 blocked |
| 外部 Kubernetes inline API Credential 原地更新 | 无 Target configuration update API | **不支持**；使用新 Target/新 Namespace 的蓝绿接入 |

所以本文可以完整完成“接入、计划维护、路由下线、恢复、Provider/Registry/Publisher Credential 轮换和事故
隔离”。托管 Kubernetes Target 在无固定 Session 等残留时可以完成受审计终态 disable，并在其后清理精确
Target-owned Kubernetes 资源；需要保留固定 Session 或原地轮换 encrypted configuration 时仍必须走蓝绿/归档，
不能绕过门禁。

## 2. 不变量

- PostgreSQL 中的 Target、Group Member version、Execution placement、Lease、Recovery Bundle、Audit 和 Outbox
  是权威；Kubernetes label、Pod 名称、浏览器状态和工单文本都不是。
- 所有 `kubectl` 必须显式指定 `--context` 和 `--namespace`。禁止依赖当前 context，也禁止对不确定对象使用 glob。
- 不在命令行、日志、工单或报告中放 Cookie、kubeconfig、ServiceAccount token、Registry auth、KMS payload、
  Provider secret 或 Presigned URL。
- Group Member 的 `draining` 只阻止新 placement。它不终止、迁移或重放已有执行，也不能证明数据已经复制。
- Location Outage 是有 TTL 的 evacuation authority，不是持久下线状态。发布方必须在维护窗口持续刷新，停止
  刷新后等待报告中的 `expiresAt` 越过服务端时间。
- 只有 lease-free 且 Recovery Bundle、Interaction/Control outcome、Checkpoint/Artifact/Memory readiness 全部满足
  契约时才允许 failover。不得通过删除 Pod 强迫跨域恢复。
- Pod/Namespace 删除必须先读 immutable UID，并在 API DeleteOptions 中使用该 UID precondition。`DELETE 202`、
  `NotFound` 或名称消失都不能替代精确终态/UID 证明。
- 不直接更新 `execution_targets`、`execution_target_group_members`、`agent_executions`、`worker_leases`、
  `reconciler_leases` 或 Credential 表。

## 3. 操作输入与证据目录

每次变更先记录以下非秘密输入：

- 变更单和操作人；Git commit、Control Plane/Worker immutable image digest、Migration expectedVersion；
- Tenant、Organization、Target、Target Group、Member ID 和当前 Member version；
- Kubernetes provider/Region/Cluster、context、Namespace 名和观察到的 Namespace UID；
- routing publisher identity/key ID 摘要、health/DR readiness version 与 `expiresAt`；
- Worker Release policy version、promoted/canary revision、Worker/Execution/Lease 的聚合数量；
- 计划的 RTO/RPO、outage TTL、最长 drain 等待和明确回滚点。

Cookie 只放在权限为 `0600` 的 jar 文件中：

```bash
export SYNARA_ORIGIN='https://synara.example.com'
export SYNARA_TENANT_ID='replace-with-tenant-id'
export SYNARA_TARGET_ID='replace-with-target-id'
export SYNARA_TARGET_GROUP_ID='replace-with-target-group-id'
export SYNARA_TARGET_MEMBER_ID='replace-with-target-group-member-id'
export SYNARA_K8S_CONTEXT='replace-with-explicit-context'
export SYNARA_WORKER_NAMESPACE='replace-with-worker-namespace'
export SYNARA_COOKIE_JAR='/secure/path/synara-cookies.txt'

test "$(stat -f '%Lp' "$SYNARA_COOKIE_JAR")" = '600'
```

Linux 上使用 `stat -c '%a'`。不要把 Cookie 内容复制进环境变量。

## 4. 通用预检

### 4.1 Control Plane 与路由 authority

```bash
curl --fail-with-body --cookie "$SYNARA_COOKIE_JAR" \
  "$SYNARA_ORIGIN/v1/tenants/$SYNARA_TENANT_ID/execution-targets/$SYNARA_TARGET_ID"

curl --fail-with-body --cookie "$SYNARA_COOKIE_JAR" \
  "$SYNARA_ORIGIN/v1/tenants/$SYNARA_TENANT_ID/execution-target-groups"

curl --fail-with-body --cookie "$SYNARA_COOKIE_JAR" \
  "$SYNARA_ORIGIN/v1/tenants/$SYNARA_TENANT_ID/workers"

curl --fail-with-body "$SYNARA_ORIGIN/ready"
curl --fail-with-body "$SYNARA_ORIGIN/metrics" > /secure/evidence/synara-metrics.prom
```

至少核对：

- Target、Group、Member 均处于预期状态，Member version 与本次 CAS 输入一致；
- `synara_reconciler_leadership{controller="kubernetes-execution-reconciler",state="active"}=1`；
- `synara_execution_target_health` 为 fresh healthy/degraded，容量没有 saturated；
- `synara_execution_queue_depth`、oldest age、Worker administrative status 和非终态 Execution 数量；
- 跨域操作所需的每个 source DR domain 都有 fresh readiness 和足够的 `replicatedThroughAt`。

### 4.2 目标 Kubernetes

```bash
kubectl --context "$SYNARA_K8S_CONTEXT" cluster-info
kubectl --context "$SYNARA_K8S_CONTEXT" get namespace "$SYNARA_WORKER_NAMESPACE" -o json
kubectl --context "$SYNARA_K8S_CONTEXT" get priorityclass synara-worker-nonpreempting-v1 -o json
kubectl --context "$SYNARA_K8S_CONTEXT" -n "$SYNARA_WORKER_NAMESPACE" get \
  serviceaccount,resourcequota,networkpolicy,pods -o wide
kubectl --context "$SYNARA_K8S_CONTEXT" -n "$SYNARA_WORKER_NAMESPACE" get events \
  --sort-by=.lastTimestamp
```

记录 Namespace UID、Pod UID、Node、镜像 digest 和 ServiceAccount subject。输出先经过 Secret scan，再进入证据库。

## 5. 新 Cluster 接入

### 5.1 先建立集群侧最小权限

按部署模式验证 Control Plane 身份只拥有当前实现需要的权限：

- `tokenreviews.authentication.k8s.io`: `create`；
- Namespace：仅在 `manageNamespace=true` 时 `get/create/patch`；
- Pod：`get/list/create/patch/delete`；
- ServiceAccount、Secret、ResourceQuota：`get/create/patch`；
- NetworkPolicy：`get/create/patch`；
- PriorityClass：cluster-scoped `get` only；不得授予 Target Credential `create/patch/delete`；
- 不授予 Namespace delete、Node mutation、cluster-admin 或 Worker Pod 使用的 Control Plane token。

对每个 verb 用目标身份执行 `kubectl auth can-i`。把结果保存为布尔矩阵，不保存 bearer token。
由 Cluster Operator 预先应用 `deploy/kubernetes/worker-priority-class.yaml`。自定义 Pool PriorityClass 也必须预先
创建且真实 `preemptionPolicy=Never`；不能依赖 Pod 覆盖可抢占类，因为 Kubernetes Admission 会直接拒绝这种不一致。

### 5.2 创建 Target

通过 `POST /v1/tenants/{tenantId}/execution-targets` 创建；请求体放在受保护文件中，并用
`curl --data-binary @file` 发送。响应只能包含 safe Target fields。若响应、Audit、Outbox 或日志出现
`configuration`、kubeconfig、client key/token，立即停止并按 Secret incident 处理。

必须使用 immutable Worker image digest、显式 requests/limits、quota、egress CIDR、Pod Security、独立 Worker
Namespace 和正确的 `controlPlaneUrl`。外部 Cluster Credential 当前不能原地轮换，因此第一次接入就要记录它的
到期时间和蓝绿替换提前量。

### 5.3 加入 Group

先创建或读取 Target Group，再调用：

```text
POST /v1/tenants/{tenantId}/execution-target-groups/{targetGroupId}/members
```

冻结正确的 `region`、`clusterId`、priority 和 weight。Region/Cluster 是 DR domain 身份，接入后不能通过浏览器标签
“修正”；错误身份应新建正确 Target/Member，不能改历史 Execution placement。

### 5.4 发布健康与 DR readiness

- tenant-owned managed Kubernetes health 由 Reconciler fresh conclusion 发布；不要再用 Tenant API 双写。
- external/platform-shared Target 使用配置过精确 scope 的 Ed25519 publisher。
- DR readiness 只能在 Artifact/Checkpoint/Memory 实际越过故障域后发布，并绑定 source domain、destination
  domain、watermark 和 TTL。手工复制一份 fixture 不算 readiness。

### 5.5 接入验收

在放入生产流量前至少完成：

1. Namespace/RBAC/NetworkPolicy/ResourceQuota/Pod Security 正负例，以及默认/自定义 PriorityClass 的
   `preemptionPolicy=Never` 与可抢占类 fail-closed 负例；
2. bound Worker ServiceAccount TokenReview + Pod GET 成功，unbound identity 拒绝；
3. 一个真实执行达到 Provider ready 并完成 Artifact/Checkpoint；
4. 一个 `waiting-for-approval` 执行完成 Suspend、旧 Pod UID 消失、新 Generation 恢复；
5. 如果宣称跨域可恢复，执行 destination Worker 真正消费恢复状态的演练；
6. 在目标自建集群执行 bounded node disruption/soak，并保存 create-only final、journal 和 partial evidence；若没有
   宣称跨故障域恢复，不要求云厂商 identity、原生账单或多可用区证据。

## 6. 计划维护或 Cluster 升级

### 6.1 停止新 placement

读取最新 Member version，把非秘密 JSON 写入受控文件：

```json
{"expectedVersion":1,"status":"draining"}
```

然后调用：

```bash
curl --fail-with-body --request PATCH \
  --cookie "$SYNARA_COOKIE_JAR" \
  --header 'Content-Type: application/json' \
  --data-binary @/secure/change/member-draining.json \
  "$SYNARA_ORIGIN/v1/tenants/$SYNARA_TENANT_ID/execution-target-groups/$SYNARA_TARGET_GROUP_ID/members/$SYNARA_TARGET_MEMBER_ID"
```

预期 status=`draining`、version 恰好加一、Audit action=`execution_target_group_member.drain_started`。相同请求重试
可以返回 `Idempotency-Replayed: true`；其他 stale version 必须重新读取并重新评估意图，不能只替换 version。

### 6.2 对已有执行做计划 evacuation

发布 `status=draining` 的精确 Region/Cluster Location Outage，并在维护期间按小于 TTL 的周期刷新。它会让该位置
不能作为新 destination，并让 lease-free、安全可恢复的 source 进入 `region-failover`。仍在运行、有未确认副作用、
缺 Bundle/Checkpoint/readiness 或 outcome-unknown 的 Execution 必须留在原位置并阻塞维护，不得删 Pod 越过。

等待并记录：

- 原位置不再产生新的 committed Scheduling Decision；
- 非终态 Execution 数量收敛为零，或剩余项都有明确延期/人工处置；
- successor 唯一、source placement 不变、predecessor Bundle lineage 正确；
- 旧 Pod exact UID 不存在，目标位置 Provider ready，Session authority/Event Sequence 未丢失。

### 6.3 执行升级

- Cluster/node/runtime 升级按云厂商批准流程执行；Worker 镜像升级使用 immutable Release + canary，遵循
  `worker-release-rollout.md`。
- 不删除 Busy Worker。Release transition 会在 active Lease 存在时 fail closed，这是正确的 drain 边界。
- 每次只改变一个故障域；持续观察 `/ready`、5xx、SSE catch-up、Outbox age、queue age、Pod failure class 和
  Reconciler leader。

### 6.4 恢复流量

1. 升级后重新取得 fresh health、容量、TokenReview/Pod GET 和 DR readiness 证据。
2. 停止刷新 Location Outage，并等待其 `expiresAt` 真正过去；没有“健康”状态可覆盖这条 outage。
3. 读取最新 Member version，调用相同 `PATCH` 将 `draining -> active`。
4. 验证新 Scheduling Decision 使用新 Member version，并完成 cold/warm 各一个实际 Execution。

维护取消也必须按第 2、3 步执行；只把 Member 改回 active 而 outage 尚未过期，路由仍会正确地拒绝该位置。

## 7. 路由下线

1. 按第 6 节把 Member 置为 `draining`，并发布/刷新 Location Outage。
2. 等待该 Group + Target 下 `queued/leased/running/waiting-for-approval/recovering/suspended` 全部为零。
3. 读取最新 Member version，`PATCH status=disabled`。若返回
   `target_group_member_execution_active`，保留 draining，处理返回的 bounded active count，禁止重试风暴。
4. 确认 Member version 只增加一次、Audit action=`execution_target_group_member.disabled`，且 Group 的新执行只选择
   其他 active Member。
5. `disabled` 是终态。需要重新接入时创建新的 Member，不直接改回 active。

到这里状态是 `routing-offboarded`。要推进为终态 Target disable，必须先确认：

- 所有 Group 中指向该 Target 的 Member 都已 `disabled`；
- 没有 active/suspended 的**固定 Target** Session；若仍需保留，只能停止下线，当前没有改绑 API；
- 非终态 Execution、未完成 Workspace materialization/cleanup、非终态 Worker/Lease 都已为零；
- 每个 Worker Pool 已 `disabled`，Warm/Resident capacity 已收敛；
- managed Reconciler 最新 health 为 unexpired `healthy/available` + `exact-active-v1`，且 allocated/acknowledged
  capacity 都为零；
- Artifact/Checkpoint/Memory 已不再是 DR required-set 的唯一副本，账单和审计历史保留在 Control Plane。

调用终态 API（无请求体）：

```bash
curl --fail-with-body --request POST \
  --cookie "$SYNARA_COOKIE_JAR" \
  "$SYNARA_ORIGIN/v1/tenants/$SYNARA_TENANT_ID/execution-targets/$SYNARA_TARGET_ID/kubernetes/disable"
```

API 与完整 Kubernetes Reconciler cycle 共用 `synara:kubernetes-execution-reconciler` 锁，防止旧 reconcile 快照在
disable 提交后又创建 Pod。`kubernetes_reconciler_busy` 表示当前 cycle 尚未结束，只能 bounded backoff 后重试；
其他冲突会返回对应的 bounded count/状态，必须先消除原因。成功时 Target=`disabled`、Audit action=
`execution_target.kubernetes_disabled`；相同请求重试返回 `Idempotency-Replayed: true` 且不追加 Audit。`disabled`
为终态，Reconciler 从此不再维护该 Target。

API **不会**删除 Namespace 或 Target 历史行。成功后才可做物理清理：

1. 再次读取并核对变更前记录的 Namespace UID；若名称已被不同 UID 复用，立即停止；
2. 仅对该 Target 独占 Namespace，使用能发送 `DeleteOptions.preconditions.uid` 的批准 Kubernetes client 删除；
3. 若 Namespace 共享，只按 inventory 对每个 Target-owned object 使用精确 UID/resourceVersion 前置条件，禁止删
   Namespace；
4. 等待 exact UID absent，复核无 Pod/Worker 身份回生、Target health version 不再推进，并完成 Secret scan；
5. 只有终态 API、Audit、精确 Kubernetes 清理和数据副本证据都齐全时，记录 `target-decommissioned`。

不得删除 `execution_targets` 历史行、直接回写 status、在 disable 前删 Namespace，或把名称消失当作 UID 终态。
SSH Target 仍只能走专用 `ssh/revoke`；本 API 不适用于 Local、Docker、SSH 或 platform-shared Target。

## 8. Credential 轮换

### 8.1 Provider / Git / Registry Credential

先读取安全 metadata 和当前 version，再调用：

```text
POST /v1/tenants/{tenantId}/credentials/{credentialId}/rotate
{"expectedVersion":N,"payload":{...},"expiresAt":"..."}
```

- Payload 只从受控文件/stdin 发送；selector identity 在轮换中不可改变。
- Registry Credential 优先原 ID rotate，保持唯一 active Target Binding；用新 Pod 的真实 pull 证明新版本可用，
  现有 node image cache 不算验证。
- 已冻结 Generation Grant 不允许偷换 Credential identity/version。静态 Provider secret 不做 Generation 内热换；
  旧 Generation 会按 rotation fence/短 access lease fail closed，必要时先 drain。
- `revoke` 是不可逆动作；只有新路径已验证并且旧访问确实需要永久关闭时调用。

### 8.2 外部 Kubernetes API Credential

当前 Target encrypted configuration 没有更新 API。不得覆盖数据库密文。使用蓝绿方案：新 Credential + 新 Target +
新 Namespace，完成接入验收后把旧 Member drain/disable，再按第 7 节通过终态 Target API 和 UID-safe Kubernetes
清理退役旧 Target。若固定 Session 仍绑定旧 Target且不能归档，退役必须保持 blocked，不能用手工删除掩盖。

### 8.3 Platform routing publisher key

配置允许一个 publisher identity 同时存在 1–8 个公钥：

1. 生成新离线 Ed25519 key，先把新 public key/key ID 加入配置并滚动 Control Plane；
2. 用新 key 发布一次 fresh、精确 scope 的 observation，验证 immutable nonce receipt 和 response digest；
3. 切换 publisher，只保留短暂双签验证窗口；
4. 等旧请求最长五分钟窗口和旧 health/readiness 最长一小时 TTL 都越过后，再移除旧 public key；
5. 验证旧 key 拒绝、新 key 成功，报告只保存 key ID/public-key digest，不保存 private key/signature/token。

云 Workload Identity 当前不在支持范围。若未来重新启用，必须作为独立项目使用 fresh Pod/session 验证 old
principal denied、new principal exact digest matched，不能把现有自建 K8s ServiceAccount 轮换证据直接升级。

## 9. 事故处置

### 9.1 单 Target/Cluster 故障

1. 保存 Target health、Member version、Location Outage、leader、Execution/Lease 聚合、Kubernetes Event 和 exact
   Pod/Node UID；不要先重启。
2. tenant-owned managed Kubernetes health 由 Reconciler 发布，不能用 Tenant API覆盖；external/shared Target 使用
   已配置的签名 publisher。没有权威 publisher 时保持 unavailable，不能手工伪造 healthy/unreachable。
3. 确认故障域后发布 `LocationStatusUnreachable`，并持续刷新 TTL。
4. 只允许自动 sweep 迁移 lease-free、安全边界完整的 Execution。对运行中或 outcome-unknown 项保留 fence并升级
   事故等级。
5. 恢复后先验证底层数据与身份，再停止 outage 刷新，等待 expiry，最后恢复 Member active。

### 9.2 Reconciler leader 异常

- `synara_reconciler_leadership` 每个 bounded controller 应只有一个 active authority；数据库不可用时不要手工选主。
- 检查 PostgreSQL 时间、lease expiry、advisory-lock 阻塞和 Control Plane readiness。不得更新 fencing token 或删除
  `reconciler_leases`。
- takeover 只在原 holder 失效后单调增加 token；旧 holder 的 mutation transaction 必须因 fence 失败。
- 使用隔离 resilience runner 演练，禁止直接在生产 namespace 删除任意 Pod来“测试”。

### 9.3 Secret 或 Credential 泄漏

1. 立即停止报告/日志扩散并限制证据访问；记录 Secret 类型和受影响 scope，不复制 Secret 值。
2. Provider/Registry Credential 走 versioned rotate/revoke；Worker token 走 Worker revoke；SSH 走 Target revoke；
   publisher key 走第 8.3 节。
3. 对旧 Worker generation、Lease、Grant、Pod UID 和 principal 做 fresh denial 验证。
4. 查询 Audit/Outbox/Session Event 的安全 metadata，确认撤权与恢复只提交一次。
5. 若无法证明旧访问已拒绝，Target/Region 保持 draining/unreachable，不恢复流量。

## 10. 完成记录

一次变更只有在以下证据齐全时才能关闭：

- before/after Target、Group、Member version/status 和 Audit action；
- before/after health/readiness/outage version、observedAt、expiresAt 和 publisher identity digest；
- Control Plane/Worker image digest、Migration expected/applied、Kubernetes server/node/runtime版本；
- Execution/Lease/Queue/Worker 聚合，source/successor lineage、旧 Pod UID absent 和新 Pod Provider-ready；
- Credential old-denied/new-success 的非秘密身份摘要；
- exact cleanup inventory，以及明确的 repository evidence、目标环境 evidence、deferred/未执行项。

任何 skipped、ambiguous、stale、cleanup failed、固定 Session 未归档/迁移、Target disable 未提交或精确 UID
清理未完成都必须保留为未完成项。Runbook 文档本身不是生产验收证据。
