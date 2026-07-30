# gVisor Runtime Isolation Tier v0（IMPLEMENTED——本地验证，发布门禁未放行）

状态：2026-07-30 七项决策已确认，契约、选择器、Generation 冻结、Kubernetes RuntimeClass、
Node attestor、canary、live Pod 复验、API/UI 投影、Docker 兼容模式和验收入口已在独立分支实现。
无 KVM 的本地 Kind/systrap runtime 与 attestor substrate 已通过；详见
[`gVisor systrap local runtime acceptance`](../reports/gvisor-runtime-systrap-local-acceptance-20260730.md)。
独立 x86_64 Linux host 的 standalone systrap/KVM canary、稳定性采样和集群非影响核对也已通过；详见
[`gVisor external host standalone acceptance`](../reports/gvisor-runtime-external-host-acceptance-20260730.md)。
逐项实现与验证边界见
[`gVisor runtime isolation implementation report`](../reports/gvisor-runtime-isolation-implementation-20260730.md)。
Codex/Claude 完整兼容矩阵、生产多节点与性能预算尚未执行，因此本文不构成平台共享 release gate
已放行或生产 GA 声明。现行发布边界仍同时受
[`Execution Target v1`](../contracts/execution-target-v1.md) 和
[`Stage 5 completion boundary`](../reports/stage-5-completion-boundary-20260729.md) 约束。

相关设计：

- [`Kubernetes Allocation Backend v1`](../contracts/kubernetes-allocation-backend-v1.md)
- [`Fast-Provision Runtime v0`](fast-provision-runtime-proposal-v0.md)
- [`Stage 5 Provider sandbox/runtime isolation`](stage-5-provider-sandbox-runtime-isolation.md)

## 1. 决策摘要

Synara 拟增加 `gvisor-sandboxed-v1` 隔离档位，在不具备 KVM 的普通 Linux 云 VM 上，以 gVisor
`runsc` 提供强于原生 `runc` 容器的用户态 application-kernel 边界。拟议的三层产品定位是：

| 层级      | 运行时                                    | 产品定位                                                 |
| --------- | ----------------------------------------- | -------------------------------------------------------- |
| trusted   | `runc`                                    | 个人开发、BYO 或可信单租户                               |
| sandboxed | gVisor `runsc`（无 KVM 时使用 `systrap`） | 普通云 VM 上的大部分不可信工作负载                       |
| microVM   | Firecracker/Cocoon                        | 独立 guest kernel、snapshot/restore 和高风险企业工作负载 |

Execution Target Kind 与运行时隔离是两条独立轴，不能用 `docker`、`kubernetes` 或 runtime 名称单独
推出安全等级：

| Target/runtime path             | 拟议 effective profile     | 初始产品边界                                           |
| ------------------------------- | -------------------------- | ------------------------------------------------------ |
| Managed Docker + `runc`         | `single-tenant-trusted-v1` | 保持个人/开发或可信单租户                              |
| Managed Docker + `runsc`        | `single-tenant-trusted-v1` | 只做兼容性试点；补齐 Docker 其他门禁前不升级多租户声明 |
| Kubernetes + native runtime     | `kubernetes-restricted-v1` | 保持当前受限多租户基线                                 |
| Kubernetes + attested `runsc`   | `gvisor-sandboxed-v1`      | gVisor 正式首发路径                                    |
| Kubernetes + Cocoon/Firecracker | `microvm-isolated-v1`      | 最高隔离层                                             |

首个实现只把 **Kubernetes + gVisor RuntimeClass** 作为可发布的
`gvisor-sandboxed-v1`。Docker `runsc` 可以随后接入，但仅仅设置 Docker `HostConfig.Runtime=runsc`
不能补足其共享命名卷、出网、资源、PID、身份与 live-runtime attestation 缺口。

## 2. 目标与非目标

### 2.1 目标

- 在没有 `/dev/kvm` 的普通 Linux VM 上提供可验真的 gVisor 隔离档位。
- 配置表达 operator/产品策略的意图与最低安全要求，环境探测只证明实际能力。
- 显式配置优先；无配置的新 Target 可以按环境与产品策略自动选择。
- requested、detected 与 effective 三种事实分开持久化并审计。
- runtime 不可用、被 admission 改写、节点漂移或 attestation 过期时 fail closed。
- 保持 Worker Protocol、Execution/Generation fencing、Release、Grant、scrub 和恢复权威不变。

### 2.2 非目标

- 不用 gVisor 替代 Firecracker/Cocoon 的独立 guest-kernel 和 microVM snapshot tier。
- 不因为宿主安装了 `runsc` 就自动把现有 Target 或正在运行的 Generation 升级。
- 不把 RuntimeClass 名称、Node 静态 label、容器内 `dmesg` 或 operator 自报 capability 当作安全证明。
- 不在首个实现中把 Managed Docker + `runsc` 开放给平台共享多租户。
- 不承诺所有 Provider、编译器、文件系统、网络、Docker-in-Docker 或设备工作负载天然兼容 gVisor。

## 3. 隔离声明

新增预留声明：

```text
gvisor-sandboxed-v1
```

用于选择和最低门槛比较的有序产品档位为：

```text
single-tenant-trusted-v1
  < kubernetes-restricted-v1
  < gvisor-sandboxed-v1
  < microvm-isolated-v1
```

该顺序是 Synara 的产品准入顺序，不是对所有威胁模型的绝对安全排序。只有完整满足对应 profile
契约才能声明该档位；某个 runtime 的存在不能自行升级 profile。

`gvisor-sandboxed-v1` 首发契约至少包含当前 `kubernetes-restricted-v1` 的全部要求，并额外要求：

- Pod 明确设置已批准的 gVisor RuntimeClass；
- RuntimeClass 的 handler 是批准的 `runsc` handler；
- 所有可调度节点都通过短期有效的 gVisor runtime readiness/compatibility attestation；
- live Pod 的 `runtimeClassName`、节点、镜像、security context、资源、卷和网络边界复验通过；
- Provider Host 只能在以上事实成立后收到 `gvisor-sandboxed-v1` 外层沙箱声明；
- runtime 不存在、不可启动、被改写或 attestation 过期时拒绝注册，不回落 native runtime。

## 4. 配置契约

`runtimeIsolation` 存在于 Execution Target 的加密配置中，不进入明文日志、审计 metadata 或 outbox。

### 4.1 显式模式

```json
{
  "runtimeIsolation": {
    "mode": "explicit",
    "runtime": "gvisor",
    "minimumProfile": "gvisor-sandboxed-v1",
    "fallbackPolicy": "fail-closed",
    "gvisorCompatibleProviders": ["codex", "claudeAgent"]
  }
}
```

规则：

- `mode=explicit` 必须提供 `runtime`；
- `runtime` 仅允许 `runc | gvisor | firecracker`；
- 配置指定的是 requested runtime，不是已生效事实；
- 环境不能满足 requested runtime 或 `minimumProfile` 时失败；
- `firecracker` 只映射到现有 `sandbox-operator-cocoon` 路径；
- Kubernetes 的 `gvisor` 只映射到 Synara 批准的 RuntimeClass，不允许任意原始 handler。

### 4.2 自动模式

```json
{
  "runtimeIsolation": {
    "mode": "auto",
    "preferred": ["gvisor", "runc"],
    "minimumProfile": "gvisor-sandboxed-v1",
    "fallbackPolicy": "fail-closed",
    "gvisorCompatibleProviders": ["codex", "claudeAgent"]
  }
}
```

规则：

- `mode=auto` 可以省略 `preferred`，由 Target Kind、Deployment Profile 和 lane 的服务端默认策略补齐；
- `preferred` 不得重复，且只能包含该 Target Kind 支持的 runtime；
- 选择顺序按 `preferred`，不是看到更强 runtime 就无条件升级；
- 候选必须同时满足环境能力、Release/Provider 兼容性和 `minimumProfile`；
- 没有满足条件的候选时失败。

### 4.3 降级策略

```json
{
  "fallbackPolicy": "allow-lower"
}
```

`allow-lower` 只允许在不低于 `minimumProfile` 的候选之间降级。它不是绕过最低安全要求的开关。

- 平台共享多租户不得降到 `single-tenant-trusted-v1`；
- 高风险 lane 请求 `microvm-isolated-v1` 时，只有策略明确把 minimum 设置为
  `gvisor-sandboxed-v1`，才允许 Firecracker 不可用后选择 gVisor；
- 一次已持久化的 Generation 不原地换 runtime。允许的 fallback 必须形成新的、可审计的调度决定；
- runtime 启动后发生漂移时终止/恢复，不在同一 Generation 内静默切换。

### 4.4 无配置行为与兼容迁移

“无配置”分为两种情况：

1. **功能上线前已存在的 Target**：按 `legacy-native` 解释，保持当前 runtime/profile，不因宿主后来
   安装 `runsc` 而改变行为。operator 保存明确配置后才退出 legacy 模式。
2. **功能上线后新建的 Target**：API 写入规范化后的 `auto` 策略；不能长期依赖一个会随服务端版本
   漂移的隐式默认。

既有 tenant-owned Kubernetes/Docker Target 通过
`PUT /v1/tenants/{tenantID}/execution-targets/{targetID}/runtime-isolation-policy`
保存规范化策略。该操作要求所有非终态 Execution 已 drain/terminal，随后将 Target 置为 `offline`、
清除旧环境观测并失效已有 Worker Manifest；只有新一轮探测、canary 和注册复验通过后才恢复 active。
共享 Target 的策略不能由租户修改。

如果 Target 已启用 Worker Release，operator 必须先注册并 promote 带相同
`gvisorCompatibleProviders` 的 immutable revision，再保存要求 gVisor 的 fail-closed 策略。控制面会在
Target 变为 offline 之前完成该预检，避免产生无法再注册兼容 Release 的恢复死锁。

拟议的初始服务端默认：

| 场景                          | 默认 preferred                                                         | 默认 minimum                 | 结果                                           |
| ----------------------------- | ---------------------------------------------------------------------- | ---------------------------- | ---------------------------------------------- |
| Personal/local 或可信 Docker  | `runc`                                                                 | `single-tenant-trusted-v1`   | 优先兼容性                                     |
| Tenant-owned Docker auto      | `gvisor, runc`                                                         | `single-tenant-trusted-v1`   | 可审计地选择可用 runtime，仍不升级平台共享边界 |
| Kubernetes restricted lane    | `gvisor, runc`                                                         | `kubernetes-restricted-v1`   | 过渡期保留当前基线                             |
| 明确要求 gVisor 的多租户 lane | `gvisor`                                                               | `gvisor-sandboxed-v1`        | gVisor 不可用即拒绝                            |
| 高风险企业 lane               | Cocoon Target 固定 `firecracker`；显式降级策略可改选独立 gVisor Target | 默认为 `microvm-isolated-v1` | 默认不降级、不在同一 Target 内切 backend       |

gVisor 验收与节点覆盖完成后，是否把全部新平台共享 Kubernetes Target 的默认 minimum 从
`kubernetes-restricted-v1` 提升到 `gvisor-sandboxed-v1`，属于单独的发布决策，不由本设计自动完成。

过渡期新 Kubernetes Target 的规范化 auto 默认同时固化
`fallbackPolicy=allow-lower`，这样只有显式列出的 `gvisor → runc` 路径可降至仍满足
`kubernetes-restricted-v1` 的候选。显式 gVisor Target 仍使用 `fail-closed`。Docker 的两个候选当前
都只能声明 trusted profile；选择 `runsc` 不会提升产品边界。

## 5. requested、detected 与 effective

控制面必须区分：

```text
requestedRuntime / requestedProfile
detectedRuntimes / detectedProfiles
effectiveRuntime / effectiveProfile
policySource
decision
decisionReason
attestationDigest / attestedAt / expiresAt
```

示例：最低要求无法满足。

```json
{
  "requestedRuntime": "firecracker",
  "requestedProfile": "microvm-isolated-v1",
  "detectedProfiles": ["kubernetes-restricted-v1", "gvisor-sandboxed-v1"],
  "effectiveRuntime": null,
  "effectiveProfile": null,
  "policySource": "target-explicit",
  "decision": "rejected",
  "decisionReason": "minimum isolation profile unavailable"
}
```

示例：高风险 lane 通过显式策略改选已经验收的独立 gVisor Target。

```json
{
  "requestedRuntime": "gvisor",
  "requestedProfile": "gvisor-sandboxed-v1",
  "effectiveRuntime": "gvisor",
  "effectiveProfile": "gvisor-sandboxed-v1",
  "policySource": "lane-policy",
  "decision": "selected"
}
```

“从 microVM lane 改选 gVisor Target”的降级原因属于上层 scheduling decision；Target 内的 runtime
decision 只记录其自身实际 requested/effective 事实，不伪造一个跨 allocation backend 的 fallback。

effective 决定必须在创建 Pod、SandboxClaim 或其他外部资源前绑定 Execution + Generation。Target
之后改变配置、RuntimeClass 或节点能力不能重写历史事实。

Managed Docker 的 Worker 容器先于具体 Execution 存在，因此它在 Worker claim 事务中、持久化
`execution.leased` 之前，把最近一次未过期且晚于 Target/Worker Release 更新的 Target runtime 选择复制为
`allocation_backend=docker-engine` 的 immutable Generation decision。缺失或过期观测会拒绝 claim；Docker
`runsc` 的 effective profile 仍是 `single-tenant-trusted-v1`，不会伪造 Kubernetes attestation。

## 6. 环境探测与 attestation

探测范围是 Target 的实际 eligible placement set，而不是控制面所在机器或集群中任意一个健康节点。

### 6.1 Native/runc

- Docker Engine/containerd 暴露批准的 native runtime；
- 当前 Target 所需 namespace、cgroup、seccomp 和资源能力可用；
- Docker 路径继续遵守 `single-tenant-trusted-v1`；Kubernetes native 路径继续执行现有 Pod/node
  restricted gates。

### 6.2 gVisor

Docker 兼容性试点：

- Engine runtime inventory 中存在批准名称及路径的 `runsc`；
- 创建请求显式固定 `HostConfig.Runtime`；
- 由宿主侧 verifier 确认 container runtime identity；容器内输出只作为诊断，不能作为授权证据。

Kubernetes 正式路径：

- 读取 cluster-scoped RuntimeClass，要求精确名称与批准的 `runsc` handler；
- RuntimeClass scheduling、Target nodeSelector、taint/toleration 求交后的每个 eligible node 都必须通过；
- Node 上的 attestor 回报固定版本的 runsc/containerd shim、健康状态和短期心跳；
- 在启用 Target 前运行 disposable canary Pod，验证真实启动、进程、文件、网络和终止；
- 每个正式 Pod 固定 `runtimeClassName`，注册时读取 live Pod 并再次验证；
- attestation 必须包含 instance identity、runtime/version/config digest、node identity、observed/expiry 时间；
- 静态 Node label、RuntimeClass 对象存在或容器内 `dmesg` 均不足以单独授权。

### 6.3 Firecracker/Cocoon

沿用现有 `microvm-isolated-v1` 门禁：KVM、jailer/guest、Cocoon supervisor、fenced vsock、credential
broker、guest identity、资源/网络和短期心跳必须同时成立。`/dev/kvm` 存在本身不是授权。

### 6.4 缓存与失效

- attestation 使用短 TTL，过期后停止新调度并重新探测；
- runtime binary/config、RuntimeClass、Node、shim 或 attestor instance 改变会使 digest 失效；
- 已运行 Generation 按 lease/heartbeat 进入 drain、terminal 或 recovery，不能把旧 attestation 延长到新实例；
- 控制面重启后从持久化事实恢复，但必须在新调度前刷新环境观测。

## 7. 选择算法

每次准备新的 Execution Generation 时：

1. 解析并规范化 Target 配置；legacy Target 生成明确的兼容策略。
2. 合并 Deployment Profile、lane、租户/组织策略，得到 requested runtime/profile 和 minimum。
3. 读取 Target eligible placement set 的未过期 attestation；不足时触发只读探测或 canary gate。
4. 按显式 runtime 或 auto `preferred` 枚举候选。
5. 过滤不满足最低 profile、Provider/Release 兼容性、资源、region 和 Target backend 的候选。
6. 无候选则在创建外部资源前 fail closed。
7. 持久化 immutable isolation decision 及 attestation digest。
8. 创建带精确 runtime 选择的 Pod/Sandbox，并进行 live-object 与 runtime registration 复验。
9. 只有复验通过后，agentd/Provider Host 才能注入 effective outer sandbox profile。
10. materialization 或注册失败时，当前 Generation 不原地换 runtime；允许的 fallback 走新的决定与审计。

环境探测是能力输入，不能覆盖显式 minimum；operator capability、Target 自定义字段和 workload
自报信息也不能升级 effective profile。

首发不在同一个 Target/Generation 内把 `sandbox-operator-cocoon` 改成 `native-pod`。高风险 lane 的
显式 gVisor 降级必须由调度策略改选一个已经验收的独立 gVisor Target，并产生新的 Generation 决策；这仍
满足“只有明确策略才可降级”，同时保留 allocation backend 与历史事实的不可变性。

gVisor 还要求双重兼容声明：Target 加密策略中的 `gvisorCompatibleProviders` 必须覆盖 Target 启用的
Provider；如果 Target 使用 immutable Worker Release，则 promoted/canary revision 的
`gvisorCompatibleProviders` 也必须覆盖同一集合。缺少声明时，`auto` 只能选择仍满足 minimum 的后备
runtime；显式 gVisor 返回 `gvisor_provider_compatibility_unaccepted`，且不会创建 Pod。

## 8. 持久化、API 与审计形状

实现阶段应持久化 Generation-scoped、append-only 的 isolation decision，至少绑定：

```text
execution_id
generation
target_id
allocation_backend
requested_runtime
requested_profile
effective_runtime
effective_profile
policy_source
decision
decision_reason_code
attestation_digest
attested_at
created_at
```

`allocation_backend` 首发允许 `docker-engine | native-pod | sandbox-operator-standard |
sandbox-operator-cocoon`；Docker Generation 只允许 `runc | gvisor` 与
`single-tenant-trusted-v1`，且不得携带 RuntimeClass 或虚构的 attestation。

环境详情可以进入单独的 bounded attestation fact；不得存储 credential、完整环境变量或无界 probe 输出。

安全 API 可以返回 requested/effective profile、runtime、decision、reason code 和时间，不返回 runtime
socket、宿主路径、token、原始 attestation payload 或节点敏感配置。UI 必须区分：

- configured/requested；
- available/detected；
- running/effective；
- fallback/rejected；
- stale/unattested。

不能只显示一个会把“配置了 gVisor”误读成“当前正在 gVisor 中运行”的绿色 badge。

## 9. 失败语义

拟议的稳定错误码：

```text
runtime_isolation_configuration_invalid
runtime_isolation_minimum_unavailable
runtime_isolation_fallback_forbidden
gvisor_runtime_class_unavailable
gvisor_runtime_handler_unapproved
gvisor_eligible_node_unattested
gvisor_attestation_stale
gvisor_canary_failed
gvisor_live_pod_runtime_invalid
gvisor_provider_compatibility_unaccepted
```

错误正文不能包含宿主路径、runtime socket、完整 Node 配置或 probe 输出。后台 reconciliation 可以重试
短暂不可用，但用户可见状态必须保持 queued/blocked，而不是回落到更弱 runtime。

## 10. 实现切片

当前实现状态：GVISOR-A、GVISOR-B 已完成代码与定向测试；GVISOR-C 已接入 exact RuntimeClass 的
真实 Provider/负向/负载验收入口，并增加由真实 Provider 触发的 bounded gVisor 工具链兼容用例及
Provider × Node P50/P95/P99 聚合，且完成无 KVM systrap substrate 本地验证，但真实 Codex/Claude
矩阵和性能预算仍是 release blocker；GVISOR-D 只交付 trusted 兼容模式与基础 hardening，独立
Execution volume、宿主 runtime attestation 和强制 egress allowlist 未完成前不会升级产品声明。

### GVISOR-A：契约与选择事实

- 增加 `gvisor-sandboxed-v1`、runtime policy schema 与规范化。
- 将当前仅按 Target Kind 推导 profile 的逻辑改为 requested/effective decision。
- 增加 Generation-scoped isolation decision 持久化、API 投影与审计事件。
- legacy Target 行为冻结测试；无配置不能因安装 runsc 自动改变。

### GVISOR-B：Kubernetes RuntimeClass 首发

- Kubernetes 配置增加受控 gVisor runtime 选择。
- PodSpec 固定 `runtimeClassName`。
- 增加 RuntimeClass、eligible-node、attestor、canary 和 live Pod 门禁。
- workload identity 验证同时冻结 runtimeClass、节点与 effective profile。
- native Pod、sandbox-operator-standard 与 Release/Generation fencing 回归。

### GVISOR-C：真实 Provider 验收

- Codex/Claude × Ready gVisor Node 全矩阵。
- Git、Bun/npm/pnpm、Go/Rust/Java/Node/Python、PTY、signal、文件监听、代理和网络负向用例。
- workspace scrub、Provider restart/resume、节点丢失、attestation 过期、RuntimeClass 漂移与精确 cleanup。
- 采集 dispatch/ready、Git checkout、依赖安装、编译、文件 metadata、网络和内存 P50/P95/P99。
- exact RuntimeClass 矩阵自动追加 `gvisor-compatibility`；任何工具、probe 或聚合样本缺失均 fail closed。
- 在批准性能预算与兼容矩阵前不进入平台共享 release gate。

### GVISOR-D：Managed Docker 兼容模式

- Docker spec/engine API 增加精确 `runsc` runtime。
- fresh Target observation 在 Worker claim 时冻结为 trusted-profile Generation decision；缺失/过期即拒绝。
- 增加宿主 runtime inventory 与 attestation。
- 补齐只读 rootfs、capability drop、no-new-privileges/seccomp、PID/CPU/内存、每 Execution 独立卷和出网策略。
- 在以上门禁和跨租户负向验收全部完成前，profile 保持 `single-tenant-trusted-v1`。

## 11. 发布与回滚

- 先在单独的无 KVM Linux VM/node pool 做 opt-in canary，不改变现有 Target。
- Release/Target 显式声明 gVisor compatibility；未验收 Provider/Release 不进入该 runtime。
- Worker Release 的兼容声明在 revision 创建时冻结；后续 rollout 只引用该不可变事实。
- 扩容前先证明所有 eligible nodes 的 attestation、RuntimeClass 和 canary 一致。
- 回滚先停止新 gVisor 调度，drain/terminalize 现有 Generation，再改变 Target runtime policy。
- 不允许删除 RuntimeClass 后让 Kubernetes 自动运行 native Pod；缺失必须使创建或注册失败。
- 全局提升多租户 minimum 是独立发布决策，必须有容量、告警、性能和兼容性证据。

## 12. 已确认的实现决策

以下七项已由用户确认并作为实现约束：

1. 确认 正式首发路径为 Kubernetes + RuntimeClass；Docker + runsc 后置。
2. 确认 新 profile 名称采用 `gvisor-sandboxed-v1`。
3. 确认 现有 Target 无配置时保持 legacy native；新 Target 由 API 固化 `auto` 默认。
4. 确认 过渡期不立刻废除现有 `kubernetes-restricted-v1` 多租户基线。
5. 确认 `allow-lower` 永远不能低于 `minimumProfile`，平台共享任务绝不降到 trusted。
6. 确认 高风险企业 lane 默认要求 `microvm-isolated-v1`，只有明确策略才允许降到 gVisor。
7. 确认 Docker + runsc 在完整 Docker hardening/attestation 前不升级为平台共享多租户。
