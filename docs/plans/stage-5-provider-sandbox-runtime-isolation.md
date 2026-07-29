# Stage 5：Provider 沙箱与运行时隔离加固

状态：DONE。启动审计日期：2026-07-28；完成审计日期：2026-07-29。路线图摘要与阶段完成状态仍以
[`TODO.md`](../../TODO.md) 为权威；本文承载启动后的实现切片、依赖、验收方法与证据。
完成口径见
[`stage-5-completion-boundary-20260729.md`](../reports/stage-5-completion-boundary-20260729.md)。

## 1. 安全目标与边界

Stage 5 同时处理两类不同的不可信输入：

- **不可信代码**：Provider 会在 workspace 中执行任意程序。执行边界必须限制进程、CPU、内存、
  临时存储、文件系统、凭证与网络影响面。
- **不可信内容**：repo 文件、Issue/PR、工具输出、网页和外部 MCP 返回值都可能携带 prompt
  injection。模型服从恶意内容时，服务端能力边界仍必须阻止不可逆或不可见的敏感动作。

Provider CLI 内建沙箱在受管运行时中保持关闭；因此只有具备经自动化守卫证明的外层隔离边界的
Target 才能进入多租户产品面。弱隔离 Target 必须显式降级为个人/开发用途，不能依赖 operator
记住隐含前提。

## 2. 2026-07-28 启动审计

已具备：

- Linux protected cgroup supervisor v3 对 Provider 子树强制 `pids.max`、`memory.max` 与
  `cpu.max`；这不是文件系统、syscall、设备或网络沙箱。
- Kubernetes Pod 已配置 restricted security context；NetworkPolicy 已拒绝/扣除云元数据 CIDR，
  DNS 已收窄到 `kube-system` 的 `kube-dns` UDP/TCP 53。启动后的严格实测又发现 kubelet
  `podPidsLimit=-1` 可使 Pod 的 PID 数不受单 Pod 约束，因此 PID 现在是独立的节点级门禁。
- Worker pool `tenantIsolation: pinned | shared` 已由 migration `000088` 冻结并在数据库层
  fail closed；尚缺 release 后的 workspace、git cache、`/tmp` 与 ownership scrub。

启动时确认的首个代码缺口：

- Kubernetes `cpuLimit`、`memoryLimit`、`ephemeralStorageLimit` 仅在非空时检查格式，空值会生成
  无对应 limit 的 execution/warm Pod。2026-07-28 已改为配置归一化时必填，并在任何 Kubernetes
  API mutation 前返回 `kubernetes_resource_limits_required`。

## 3. 交付切片

### S5-A：资源与网络下限

- [x] Kubernetes CPU、Memory、EphemeralStorage limits 与 Target `pidsLimit` 必填；execution 与 warm
      Pod 共用该 fail-closed 配置入口。Reconciler 在任何写入前通过 kubelet `configz` 检查 Target
      `nodeSelector` 下的全部节点，Worker 注册时复验实际 `spec.nodeName`；`-1`、`0`、超出 Target
      上限、无匹配节点或无读取权限均拒绝运行。
- [x] 对单 Execution 实际触发 fork、内存与 CPU 压力，证明不会影响同宿主其他 Execution/Worker。
      当前一次性 Kind（`podPidsLimit=128`）已实测 256 次 fork 中 120 成功、136 被拒绝，64Mi 内存
      OOMKilled、CPU 有限且邻居 12/12 响应；OrbStack 的 `podPidsLimit=-1` 被同一严格验收抓出并会被新
      preflight 拒绝。验收器现将 peer、fork/CPU 与 OOM Pod 强制共置到同一 Worker、回读三个实际
      nodeName，支持显式单节点钉住；all-node 模式要求 Target label selector，只枚举 Ready/非 cordon
      匹配节点并逐节点串行运行，同时支持 registry image pull policy。自建物理 K3s 的完整 Ready Worker
      集合已运行；其他自建 Target 接入时必须独立复验。
- [x] 从真实 Provider 进程发起云元数据请求，逐 Kubernetes Worker 证明不可达。当前 Worker 镜像内
      内置 value-free isolation verifier 对 AWS/阿里云/IPv6 metadata 地址执行有限拨号，不生成响应 body/header；
      `network-boundary-init` 要求连续两轮阻断后才允许 registration init 与 agentd 启动。物理 K3s 单节点上
      Codex/Claude × `synara-k3s-1` 的真实产品链路矩阵已通过，两个 Provider 都保留 fresh `network-egress`
      Approval 与精确零输出 exit-0 终态。`stage5_provider_isolation_matrix.py` 会从生产 Target label selector
      枚举首尾 Ready/非 cordon Worker 清单，逐 cell 验证 exact node、cleanup 与 Secret scan；节点集合变化即失败。
      本次自建物理 K3s Target 的 Codex/Claude × 完整 Ready Worker 集合已逐节点执行。EKS/GKE/AKS
      专属 CNI/IAM 不在 Stage 4 冻结的当前正式支持面，不作为本阶段完成前提；未来纳入产品面时须另行验收。
- [x] 逐 Provider、逐 Kubernetes Worker 证明真实 Provider 进程不存在 ambient cloud、Git/SSH、package、
      Docker/Kubernetes 或 ServiceAccount Credential。`real-provider-smoke --real-provider-case credential-scope`
      已接入 exact-node 产品链路：内置 verifier 检查环境名/路径是否存在，不持久化环境值、不读取 Credential
      文件内容；唯一内容读取是对非 symlink `.git/config` 做 64 KiB 上限的 HTTPS userinfo 检查，且不输出内容。
      受控的 execution-lifetime Provider broker task token 按设计排除。物理 K3s 的 Codex/Claude 单节点矩阵已以
      fresh `credential-access` Approval、精确零输出 exit 0 通过；未来新增 Provider 或 Target 时必须重跑完整
      Provider × Ready Worker 笛卡尔积。

### S5-B：跨租户残留

- [x] 冻结 scrub-on-release 的状态机与故障原则（2026-07-28）：
  - shared general-pool 的 Execution 终态不能直接把 Worker 变回 `idle`；控制面先进入
    `scrub-required`，并保留刚完成的 Tenant/Execution/Generation fence。
  - agentd 停止 Provider 子树并确认 containment 后，清理该 Tenant 在 workspace v2/v3、legacy
    workspace、git cache v1 的 Target-scoped 子树；临时目录必须由 Execution-scoped 私有根承载，
    不能靠扫描并删除宿主全局 `/tmp`。
  - protected provider identity 写入的路径先以不跟随 symlink 的方式恢复 agentd owner，再删除；
    任一 ownership、持久化或删除步骤失败都不得确认 scrub。
  - scrub receipt 必须绑定 Worker incarnation/instance UID、Execution、Generation、Tenant 与递增
    scrub generation；重放只允许返回同一结果，不能确认新的 fence。
  - 控制面收到 receipt 后才把 Worker 置回 `idle`。agentd/控制面在 scrub 前崩溃时，重注册只能恢复
    同一 scrub，不得 claim 下一 Tenant；重试耗尽则 drain/revoke Worker。
- [x] 实现幂等 scrub 与调度 fence，scrub 完成前不得把 Worker 交给其他 Tenant。Migration `000094`
      持久化物理 Worker/Incarnation/Instance UID、Tenant、scope Generation 与单调 scrub Generation；
      acknowledgement 后才恢复 `idle`，失败 drain，重注册只允许同一物理 UID 恢复同一 pending scrub。
      agentd 清除 Target-scoped workspace v2/v3、legacy workspace、Tenant git-cache、quarantine 与容器私有
      `/tmp`，不跟随 symlink，并在 durable absence 验证后确认 receipt。
- [x] 在同一 Worker 上执行 Tenant A → Tenant B 负向实测；B 无法枚举或读取 A 的残留。证据分两层：
      OrbStack + PostgreSQL 的真实控制面状态机已证明同一物理 Pod/Worker 的 A → scrub receipt → B fence；
      随后的同一 Worker 组合实测由真实 agentd 依次运行两个 Provider Host Protocol v2 fixture 进程，Tenant A
      在 workspace v2/v3、legacy、git cache、quarantine 与 Worker-private `/tmp` 写入 6 处 marker，agentd
      物理 scrub 并确认 receipt 后，Tenant B Provider 对同一存储根递归扫描 27 个路径，残留路径与可读 marker
      均为 0。证据见
      [`stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md`](../reports/stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md)。

### S5-C：Target 隔离等级与 Provider 沙箱守卫

- [x] 将 Kubernetes、Docker、SSH/local protected、SSH/local fallback、macOS 的隔离能力矩阵并入
      [`execution-target-v1.md`](../contracts/execution-target-v1.md)。
- [x] Docker 定位为个人/开发用途、非多租户 Target，除非实现并验收到 Kubernetes 平价。
- [x] 明确 SSH/local fallback 与 macOS 的弱隔离等级并从多租户产品面排除。
- [x] 对 `danger-full-access` / `bypassPermissions` 建立服务端权威的自动化前提守卫：Provider Host
      拒绝缺失/未知 profile；Kubernetes profile 只在 Pod-bound TokenReview 和 live Pod restricted-spec
      验真后成立，弱化 Pod 不会退回 trusted profile。
- [x] 合并延迟与内核隔离两个维度，决策 microVM 的运行位置与可信边界：Kubernetes 保留控制面，
      `sandbox-operator` 在专用 Linux/KVM 节点管理 Firecracker；agentd/broker 在 guest 外，Provider/tool
      在 guest 内并通过 fenced vsock 通信。实现与实测仍属于 fast-provision 后续交付，不能冒充已落地。

### S5-D：不可信内容与敏感动作

- [x] 冻结 prompt injection 威胁模型与通道级缓解基线：见
      [`Untrusted Content and Sensitive Actions v1`](../contracts/untrusted-content-sensitive-actions-v1.md)。
- [x] 在 Provider 上下文入口为外部内容附加结构化来源与信任级别。External/Synara MCP 与 automation
      已接线。恶意 Issue 形态的 automation 回归曾抓出 Reactor 虽收到 `message.source=automation` 却在
      bootstrap boundary 重新使用原始 `input.messageText`、实际绕过 provenance wrapper；现已统一改为
      `provenanceWrappedMessageText`，并对 external-mcp/synara-mcp 同时冻结。所有三类不可信来源即使内部命令
      请求 `full-access` 也由 decider 降为 `approval-required`。所有本地 Provider 现有 exhaustive delivery
      registry，并收到同一 host content-trust policy；补齐的 Antigravity 路径因没有 system/MCP transport，
      每个 CLI Turn 都携带 identity-only host block，失败的首个进程不会永久消费一次性 policy。该 policy 已将
      repo、tool output、web fetch 与 MCP result 声明为不可信，但它只是纵深防御。Claude SDK 已在 managed
      Provider Host 与本地 adapter 两条路径用 `PostToolUse.updatedToolOutput` 把成功结果嵌入宿主控制的
      `synara.provider-untrusted-content.v1` envelope，原值只能位于 `content`；失败结果因 SDK 只允许
      `additionalContext`，以不复制 error 正文的相邻 provenance 标记补强。实时 `AskUserQuestion` 用户答案不降级。
      Codex 0.145 的 `PostToolUse` 不支持原位改写 result，而且只在 handler success 后运行，无法覆盖 MCP
      `isError`、patch/Approval 拒绝或 handler failure；managed 与 local 两条路径因此都在隔离
      `CODEX_HOME` 上把唯一 Host-owned 命令安装为单个 session-flags `PreToolUse` hook。managed
      命令复用只读 Worker 镜像内的 Provider Host 入口；local 命令把固定的有界 hook 程序嵌入已证明的启动参数，
      只调用 Synara 当前绝对进程路径，不依赖
      workspace 中的可变 helper。两者都在开 Thread 前通过 `hooks/list` 拒绝任一缺失或额外启用的 non-managed
      hook，并在 start/resume/fork 请求上强制 `config.bypass_hook_trust=true`，弥补 0.145 app-server 未把 CLI flag
      传播到 Thread Hook Engine 的缺口。Pre hook 在执行前为所有受支持且非实时用户答案的工具写入 bounded Host
      developer context，模型随后消费成功或失败 native result 时都能看到来源；它不复制 input/output/error 正文，
      同时会在不能发出 fresh Approval 的 permission mode 下执行前拒绝敏感调用。`request_user_input` 实时答案保留
      可信作者身份。0.145 的 `write_stdin` 明确跳过 Pre hook，故 Synara 还固定关闭 `features.unified_exec`，只暴露
      每条命令都重新分类的一次性 `shell_command`。默认不经 ToolRegistry 的 hosted cached web search 现由
      `web_search="disabled"` 关闭；code mode、browser use 与 computer use 同样关闭。local/managed 启动均在开
      Thread 前用 `config/read` 复验有效 search mode、全部受限 feature 与精确 MCP 配置，参数被忽略或覆盖即
      fail closed。local `synara` 还要求完整字段集、同一 scoped lease 的 numeric-loopback `/mcp` URL 与
      `bearer_token_env_var`，并证明 shell policy 排除 gateway token 及所有保留的 model-provider
      `env_key` / `env_http_headers` credential mapping，且不能经 `set` / `include_only` / 非默认继承重引入；
      discovery 无 lease 时 MCP 固定为空集。真实 0.145.0 Responses 请求捕获未出现 `web_search`、code-mode `exec` 或 `wait`；当前
      `tool_search` 只枚举 Host-owned Synara MCP metadata，不是第三方 runtime result。Pi direct SDK
      路径现禁止 project extension trust，并把 hidden in-memory Host `tool_result` extension 固定在最后，为
      成功/失败结果追加不复制正文的相邻 context，
      原始 text/image block 保持顺序与模态。ACP 的 `session/update` 是 Agent 已把结果送回模型后的单向投影；
      OpenCode/Kilo 虽有模型消费前 `tool.execute.after`，但只存在于同进程 external plugin chain，因此不冒充
      Host 边界；二者的 Synara 子进程现同时关闭 repository project config 与全部 user/global external plugin，
      后者通过强制 CLI/env pure mode 实现。不可认证同一启动 profile 的外部 server 不承接 untrusted dispatch。
      ACP、OpenCode/Kilo 与 Antigravity 仍如实标为 `policy-only`；Antigravity 2.0 官方契约也确认 `PostToolUse`
      只能返回 `{}`，而同 UID external `PreInvocation` plugin 不能冒充 Host 边界。结果 provenance registry 现已成为
      三类 server-authored untrusted source 的硬准入：必须同时具备 fresh Approval、对应 runtime 的 repository
      startup isolation，以及成功/失败结果在模型消费前的 Host provenance。Agent/Synara MCP schema、External MCP
      capability、Automation create/update/run、durable decider 与 Reactor backstop 因此只允许 Codex/Claude；普通
      人工 Turn 不受影响。未来 Provider 或 Codex specialized tool surface 改变时必须先升级并复验 registry，不能
      仅靠 policy text 扩大准入。证据见
      [`stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md`](../reports/stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md)。
- [x] 建立服务端权威的敏感动作清单并强制 Approval：受保护分支 push、CI、依赖、Credential
      路径与新增出网目标。classifier、Claude 双路径、Codex native approval、Cursor/Grok/Droid ACP 以及
      OpenCode/Kilo permission 路径的 fresh-approval fence 已完成。准入现额外要求 repository 可执行启动配置
      在审批回调生效前已隔离：local Codex 使用独立的 Synara-owned `CODEX_HOME`，仅共享 `auth.json`，
      session store 独立且只按明确 resume/fork ID copy-on-write 导入单个 rollout；0600 config 只保留非可执行
      model-provider transport 子集，把明文 provider bearer token 转为不下传工具子进程的环境变量；保留的
      base URL 必须是无 userinfo/query/fragment 的 HTTP(S)，静态 query 只允许 dedicated table 中日期形态的
      `api-version`，其余形态 fail closed，避免长期 Credential 重新落入同 UID 工具可读的 config。并丢弃
      command-backed/AWS provider auth、user/project MCP、hooks、plugins、rules、skills、profiles 与 project trust。
      Codex 最低版本升至 0.145.0，并由 `--strict-config` 与 Host CLI flags 关闭 executable extensions、external
      memory import、shell snapshot、hosted web、code/browser/computer use 与会跳过 continuation Pre hook 的
      unified exec，MCP 只允许空集或唯一的 Host-owned Synara gateway；`config/read` 在开 Thread 前复验同一有效
      tool surface、精确 transport 和全部 Provider credential env 的 shell exclusion。local/managed Claude 使用
      `settingSources: []` + `strictMcpConfig`，OpenCode/Kilo 的所有 Synara-started server、discovery 与辅助 CLI
      命令同时使用 provider-specific project-config kill switch、`--pure` 与 `OPENCODE_PURE=1` / `KILO_PURE=1`；
      workspace 打开前还要求版本不低于已审计 floor（OpenCode 1.15.11、Kilo 7.4.16）；不支持 pure interface、
      版本过低或版本不可解析的 binary 启动即失败，已配置的外部 server 因无法认证启动 profile 而对三类
      server-authored untrusted source fail closed；managed Codex 使用隔离 HOME 与 hooks attestation。Cursor 会
      自动读取 project MCP，Droid 虽已通过每会话
      0600 runtime settings 关闭所有 hooks/继承 autonomy/IDE auto-connect/cloud sync，仍可能加载 project
      MCP/plugin，Grok 也没有已证明的完整 kill switch；故三者与缺少宿主 permission callback 的
      Antigravity/Pi 一并从 External/Synara MCP 与 automation 的创建、更新、运行和最终编排入口 fail closed 排除。
      Cloud Control Plane 现严格校验敏感评估的类别/布尔不变式，并将同一 assessment 从 `request.opened`
      复制到 `request.resolved`；真实 Claude metadata case 会在 `full-access` 下验证整条链。Codex full-access
      的受支持敏感 tool call 现由 Host PreToolUse 在执行前拒绝，必须切换到 `approval-required`；任意可执行程序
      内部未暴露的 syscall 仍依赖外层沙箱/最小凭证/egress。`apply_patch` 的 patch header 与 native
      `fileChange.changes[].path` 都进入同一 classifier；0.145 随后的 pathless file-change Approval 必须按
      `itemId` 命中前置 `item/started` assessment，否则 local/managed Host 都直接拒绝。metadata-egress、
      credential-scope 两个 Codex case
      现在都要求 `approval-required` 把各自精确 assessment 保留到 pending / opened / resolved，且仅允许一次授权。
      新增 `malicious-issue-denial` exact-node case 则让真实 Claude/Codex
      发出带 `credential-access` + `protected-branch-publish` 的安全熔断请求，由 Runner 显式 `decline`，并要求
      fenced bounded-declined Provider lifecycle、零 command output 与 Artifact、无 exit/signal 且
      `commandExecuted=false`；Codex/Claude 的物理 K3s exact-node 运行均已通过。聚合矩阵编排器已把该 case
      与前两项一起列为
      每个 Provider × Node cell 的硬门禁，并校验 exact-node 子报告、cleanup 与 Secret scan。
      共享 classifier 现进一步封住命令形态与内容形态绕过：带 Git 全局参数的 fetch/push、带 package-manager
      全局参数的依赖变更、扩展的 CI/manifest/lockfile/Credential 路径，以及 `Write`/`Edit` 新内容中的 URL 或
      Credential 引用都进入同一服务端评估；`old_string` 删除内容不会被当作新增 authority。当前 Codex
      attested mutation surface 只有 `apply_patch`，其 patch command/header 由 3909-character inline guard
      保守扫描并保持 4096 上限；未来 content-key 写工具必须先扩展 guard 与工具面 attestation。共享、全部
      Provider 入口与 exact-node Runner 的本地回归证据见
      [`stage-5-sensitive-action-classifier-local-acceptance-20260729.md`](../reports/stage-5-sensitive-action-classifier-local-acceptance-20260729.md)。
- [x] 收敛 git、云端和 Provider Credential Grant 的任务级权限范围：Provider 长期 Key 由 agentd
      execution-lifetime loopback broker 保管，子进程只拿 task token；git fetch 凭证在 Provider 前清除，
      publish 型 Grant 不进入普通 Workload；ambient git/cloud secret 不进入 Provider 环境。Kubernetes
      Pod-bound registration token 只投影到受限 init，主容器一次性消费后删除。真实 Provider
      `credential-scope` exact-node 验收已冻结这些 absence 断言，并在物理 K3s 的 Codex/Claude 单节点矩阵通过；
      托管 Provider × Node 运行证据仍待补齐。
- [x] 外部 MCP 返回值按不可信内容处理，MCP 自身网络访问不能绕开 egress allowlist：当前 managed
      Provider Host 默认不允许第三方 MCP（Claude 禁用 user/project/local settings，Codex 使用 agentd-owned
      clean `CODEX_HOME` 并以 `mcp_servers={}` 启动）；未来接入必须同时增加 result provenance 且运行在
      Target egress 内。
- [x] 将触发输入、敏感动作、Approval 与结果串成可审计链路并提供疑似注入告警：External MCP audit
      migration `088` 持久化 source/trust/digest/indicator IDs 和 `prompt_injection_suspected`，并通过
      integration/request/project/created Thread → Message source → Turn/request/item/sensitive categories 关联；
      不存第二份 prompt 明文。本地 Runtime Event 现也在 `request.opened` / `request.resolved` 顶层保留同一
      canonical assessment，并投影到 Approval activity。外发通知属于 Stage 6，Stage 9 恶意 Issue 实测仍保留。
- [x] 在 Stage 9 前加入恶意 Issue 输入的无人值守负向测试。
      当前已有 orchestration product-path 回归证明恶意 closing-tag/`git push`/Credential 指令以
      `source=automation` 进入 escaped JSON-string boundary 且 full-access 被降级；补充 ingestion 链路已证明同一
      Issue 形态消息触发的敏感请求保留精确 assessment、保持 pending，显式 decline 后无任何 tool lifecycle。
      真实 Provider 的 `malicious-issue-denial` Runner 门禁也已就绪：native user Turn 回放 Issue 形态文本，命令以
      `false &&` 安全熔断，Runner 必须看到双类别原生 Approval、显式拒绝及 bounded non-executed lifecycle。
      该 case 已在物理 K3s 的 Codex/Claude exact-node 矩阵通过。真实 Stage 9 Issue webhook adapter 尚不存在；
      webhook signature、外部身份映射、幂等投递与真实无人值守 Provider 拒绝的 adapter-specific 端到端测试
      保留为 Stage 9 自身上线门禁，任何 adapter 在该门禁通过前不得上线。

## 4. 验证与证据规则

- 单元/集成测试只能证明仓库内 fail-closed 行为，不能替代真实内核、容器网络或跨租户残留实测。
- Kubernetes YAML/对象检查不能代替从运行中 Provider 进程发起的元数据负向请求。
- 自建 Target 的证据只授权该次首尾清单中完整的 Ready/非 cordon Worker 集合，不自动授权其他集群；
  EKS/GKE/AKS 专属 IAM/CNI/Region 能力仍须在未来进入正式产品面时独立验收。
- 每个实测报告记录当前 commit/tree、Target Kind、集群/运行时版本、失败注入、原始断言与残余风险。
- 阶段最终验证执行一次 `bun fmt`、`bun lint`、`bun typecheck`，并运行受影响 Go/TypeScript
  聚焦测试；不使用 `bun test`。

## 5. 当前实现与验证证据

2026-07-28：

- 受影响的 11 个 Go 包完整测试：PASS。
- Provider Host 3 个文件 82 tests；Server 9 个文件 291 passed / 2 skipped；shared 3 tests；Web 10
  tests：全部 PASS。
- Kubernetes rollout gate Python 单测：16/16 PASS。
- 一次性 Kind v1.33.1、`podPidsLimit=128`：fork 120/136 成功/拒绝、CPU finite、memory OOMKilled、
  metadata 无响应、peer 12/12；一次性令牌 projected → init → staged → consumed/deleted：PASS。
- OrbStack v1.34.8+orb1 明确观测 `podPidsLimit=-1`，严格 PID 验收 FAIL；这是新门禁应拒绝的负向证据，
  不是可忽略的本地差异。跨 Tenant A → B 控制面状态机与真实 agentd + Provider Host 组合 probe：PASS。
- Provider 敏感动作增量：共享 classifier 6/6；ACP/OpenCode 组合 131/131；Agent/External MCP 创建入口
  89/89；Automation 完整 package 121/121。Pi/Antigravity 的 server-authored untrusted dispatch 在任何
  worktree 或 Provider mutation 前拒绝，持久化旧 Automation 也在 run-path backstop 失败。
- 真实 Provider metadata 验收代码：新增 Kubernetes exact-node pin、AWS/阿里云/IPv6 bounded probe、
  Claude full-access fresh-approval 与 request/resolution assessment 断言；Runner 251/251、四 Target release
  gate 88/88、Kubernetes real rollout gate 18/18 聚焦单测通过。尚无托管集群/真实 Credential 运行证据。
- 恶意 Issue/不可信入口回归：修复 ProviderCommandReactor 在普通/sidechat/bootstrap/retry 路径丢弃已包装
  provenance 文本的问题；automation、external-mcp、synara-mcp 的 runtime backstop 与 Provider 输入测试
  122/122 PASS。该证据不包含真实 Issue webhook 或真实模型敏感工具调用。
- 真实 agentd + Provider A → B：A 写入 6 类 marker，agentd scrub/receipt 后 B Provider 扫描 27 个路径，
  `residualPathCount=0`、`residualMarkerReadable=false`；fixture 单测 21/21 PASS。独立证据见
  [`stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md`](../reports/stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md)。
- 详细证据、镜像哈希、失败发现与残余边界见
  [`stage-5-runtime-isolation-local-acceptance-20260728.md`](../reports/stage-5-runtime-isolation-local-acceptance-20260728.md)。
- 按仓库指令，当前对话未获显式授权，未执行 workspace 级 `bun fmt`、`bun lint`、`bun typecheck`；
  因而 Stage 5 仍不得标记完成。

2026-07-29 增量：

- 全 Provider content-trust delivery registry 与 Antigravity every-Turn identity-only host block：相关 Server
  84/84 PASS。Claude 成功/失败 native result provenance 已接入 managed/local 两条路径；shared 12/12、
  Provider Host 28/28、Server Claude+harness 137/137 PASS。其后已补入 managed Codex successful result 的
  adjacent host-context hook 与启动自检；Pi 也已用 hidden in-memory `tool_result` extension 接入成功/失败
  adjacent context，并拒绝 project extension trust。OpenCode/Kilo 子进程强制关闭 project config/plugin；
  ACP 与 OpenCode/Kilo 的审计分别确认只有模型消费后的 Client notification、或不可 attestation 的同进程
  external plugin chain，继续诚实标为 `policy-only`。当前增量定向回归：Provider Host 4 files 87/87、
  Server Pi/OpenCode/ACP/registry 6 files 129/129、shared policy 13/13 PASS。
- 结果 provenance registry 已从报告性矩阵升级为 untrusted dispatch 的权威准入。policy-only Provider 不再出现
  在 Agent/External MCP target schema/capability 中，Automation 的 create/update/run 与 durable decider 在任何
  Provider 启动、Thread 创建或 worktree 物化前 fail closed；Reactor 保留最后一道复验。初轮聚焦回归修正 7 条
  旧产品预期后，8 files 351/351 PASS；完整证据见
  [`stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md`](../reports/stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md)。
- 启动期可执行配置准入拆为“宿主可观测 fresh Approval”与“repository startup isolation”两项独立能力：
  local Codex/Claude/OpenCode/Kilo 与 managed Host Codex/Claude 同时满足；Cursor/Grok/Droid 只满足前者并
  fail closed。Droid 每会话私有 runtime settings 已关闭 hooks、继承 autonomy、IDE auto-connect 与 cloud
  sync，但因 project MCP/plugin 尚未隔离不升级准入。local Codex 已改用上述最小隔离 home，而不是依赖
  `mcp_servers` map 的深合并覆盖；真实 0.145.0 probe 证明恶意 project MCP 与用户 MCP 均未加载、`mcp list`
  只含 `synara`，严格 app-server `initialize` 及 OpenAI/custom-provider rollout resume 均成功。最终定向回归
  计数见下方最新验证记录。
- 真实 Provider `malicious-issue-denial` exact-node case：安全熔断命令、双类别 fresh Approval、显式
  `decline`、零 command/Terminal/output/Artifact 生命周期及 exact marker 已落地；Acceptance Runner
  263/263、shared classifier 8/8 PASS，尚无托管 Provider × Node 实跑证据。
- 修复 Stage 3 release gate 误把显式 Stage 5 case 纳入 `--real-provider-matrix` 的集合污染；Local、Docker、
  Kubernetes、SSH 与 Kubernetes real rollout 五组测试合计 107/107 PASS。
- 新增 `stage5_provider_isolation_matrix.py`：clean source + immutable image 前提、首尾节点清单、完整
  Codex/Claude × Node cell、逐 case 安全证据、子报告/cleanup/Secret scan 聚合与 inventory-change
  fail-closed；11/11 PASS。
- 最终允许范围内回归：Provider Host 3 files 82/82、Acceptance Runner 263/263、五类 release gate +
  Stage 5 matrix 118/118、`git diff --check` PASS。Python 3.14 Runner 测试仍输出既有未关闭 SQLite
  `ResourceWarning`，但测试结果为 PASS。
- 当前 `kubectl` 仅配置 `docker-desktop`、`orbstack`；没有可代表生产 IAM/CNI/Ready Worker 集合的托管
  context，故未运行聚合矩阵，也不将本地集群证据冒充生产验收。
- Codex 启动隔离修正后的最终定向回归：Server 15 files 713 passed / 2 skipped、shared 3 files 14/14、
  Provider Host 4 files 87/87；Server 与 Provider Host build、`git diff --check` 均 PASS。真实 Codex 0.145.0
  probe 另行证明 strict app-server `initialize`、custom-provider rollout resume 以及 project/user MCP 排除；
  `mcp list --json` 只有 `synara`。当前对话仍未获授权运行 workspace 级 `bun fmt`、`bun lint`、
  `bun typecheck`，Stage 5 继续保持 IN PROGRESS。
- local Codex successful-result provenance 已与 managed 路径共用同一来源规则和 exact hook attestation。
  该轮真实 Codex 0.145.0 app-server probe 通过 `initialize` 和 `hooks/list`，看到当时配置的 enabled
  session-flags `PreToolUse` / `PostToolUse` 两个 hook；本地命令为内联固定程序且不复制 `tool_response`。
  本轮最终定向回归：
  Server 15 files 714 passed / 2 skipped、Provider Host 4 files 88/88、shared 3 files 15/15；Server 与
  Provider Host build、`git diff --check` 均 PASS。该证据随后被下面的 failure-path 审计与单 hook 设计取代。
- Codex tool-policy 实跑发现并修复了一个仅看 `hooks/list` 无法发现的 0.145 缺口：CLI
  `--dangerously-bypass-hook-trust` 没有进入 app-server Thread Hook Engine，hook 虽显示 enabled/untrusted 却不执行。
  local/managed start、resume、fork 现都携带 request-level bypass。真实 full-access 探针中安全熔断的
  `false && git push` 产生 blocked PreToolUse 且无 command item；同一命令在 approval-required 下产生 completed
  PreToolUse、恰好一次 native Approval，显式 decline 后无进程启动。Codex metadata-egress 验收也已改为先
  fresh Approval、再实际探测 CNI/NetworkPolicy。补充真实 `apply_patch` 探针证明 full-access 修改
  `package.json` 会在执行前 blocked，文件未创建；approval-required 则产生一次 native file-change Approval，
  显式 decline 后 item 以 `declined` 完成且仍无文件副作用。Host 已利用其前置 path-bearing `item/started`
  保存精确 `dependency-change` assessment，缺失该关联时 fail closed。
- 本轮最终允许范围回归：Server 15 files 814 passed / 2 skipped、Provider Host 4 files 92/92、shared 3 files
  19/19、Acceptance Runner 263/263、Stage 5 matrix 12/12；Server 与 Provider Host build 均 PASS。Python 3.14
  Runner 仍输出既有未关闭 SQLite `ResourceWarning`，但测试结果为 PASS。当前对话仍未获授权运行 workspace
  级 `bun fmt`、`bun lint`、`bun typecheck`，且仅有 `docker-desktop` / `orbstack` context，没有托管
  Provider × Node 运行证据；Stage 5 继续保持 IN PROGRESS。
- Codex 0.145 pinned source audit 证明 `PostToolUse` 仅在 `success_for_logging=true` 时运行；MCP `isError`、
  patch/Approval 拒绝与 handler failure 都会跳过。local/managed 因此收敛为恰好一个 attested `PreToolUse`：
  除实时用户答案外，它在执行前写入同一 bounded provenance developer context；敏感动作在不可询问模式仍先
  fail closed。`write_stdin` 又被确认明确跳过 Pre，故隔离参数关闭 unified exec，回退到一次性
  `shell_command`。最终真实 app-server 双 probe 只看到一个 enabled `preToolUse`；成功/失败各有且仅有一个
  `completed/context` 和 raw rollout developer provenance，模型都能复述仅该 context 提供的精确 policyVersion
  与 toolName。成功路径只出现一次 completed `shell_command`，零 `exec_command` / `write_stdin`；失败路径的
  file-change 为 `declined` 且 `package.json` 不存在。内联命令 3857 bytes，低于 4096-byte gate。
  `thread/read` 不投影 developer-only context 是 0.145 的既有展示行为，不是模型消费缺失。托管集群矩阵与
  ToolRegistry 外路径仍保持未完成。
- 本轮最终允许范围回归：Server 24 files 1003 passed / 2 skipped、Provider Host 4 files 94/94、shared 3 files
  20/20、Acceptance Runner 263/263、Stage 5 matrix 12/12；Server 与 Provider Host build、`git diff --check`
  以及相关 untracked source whitespace scan 均 PASS。Acceptance Runner 从脚本子目录误跑时曾因 repo-relative
  fixture 路径产生 1 个 `FileNotFoundError`，从仓库根按契约重跑后 263/263 PASS；Python 3.14 仍输出既有未关闭
  SQLite `ResourceWarning`。当前对话仍未获授权运行 workspace 级 `bun fmt`、`bun lint`、`bun typecheck`，且
  仅有 `docker-desktop` / `orbstack` context，没有托管 Provider × Node 运行证据；Stage 5 继续保持 IN PROGRESS。
- OpenCode/Kilo 启动隔离继续收紧：此前仅关闭 repository project config，仍会载入 user/global external
  same-process plugins。当前所有 Synara-started server、model/auth discovery 与辅助命令均强制 `--pure` 及对应
  `*_PURE=1`，同时保留 `*_DISABLE_PROJECT_CONFIG=1`；版本门禁固定为 OpenCode 1.15.11 / Kilo 7.4.16，旧 binary
  不认识 `--pure`、版本过低或无法解析时均 fail closed。已运行的外部
  OpenCode/Kilo server 无法证明同一 profile，现由 durable decider、Automation create/update/run，以及读取当前
  server settings 的 Reactor last-moment backstop 拒绝接收 external-mcp/synara-mcp/automation 内容，连 provider
  start 与 subagent steer 都不会发生。其 public `tool.execute.after` 仍只来自 pure mode 会移除的 external plugin，
  所以 result provenance 继续诚实标为 `policy-only`。本轮聚焦回归：runtime/security/decider 3 files 44/44、
  ProviderCommandReactor 114/114、AutomationService 122/122、ProviderHealth 92/92 PASS；官方 release source、
  SHA-256 与隔离 XDG 二进制 probe 见
  [`stage-5-opencode-kilo-pure-mode-local-acceptance-20260729.md`](../reports/stage-5-opencode-kilo-pure-mode-local-acceptance-20260729.md)。
  11 个直接受影响文件合并回归 478/478 PASS；第一次完整 Server suite 暴露并修正一条与当前 Codex token
  exclusion 实现不一致的旧测试断言，最终机器可读全量复跑为 3106 passed / 7 skipped / 0 failed，Server build
  PASS。
- Codex 0.145 当前有效工具面继续收口：启动参数固定 `web_search="disabled"`，同时关闭 code mode、browser use
  与 computer use；local/managed 都在开 Thread 前用 `config/read` 对有效 feature、hosted search mode 与精确
  MCP server 集合 fail-closed attestation。官方 0.145.0 arm64 binary 的真实配置读取与本地 Responses request
  capture 均证明无 `web_search`、code-mode `exec` 或 `wait`。Shared 全包 453 passed / 1 skipped、Provider Host
  155/155、Server 全量 3107 passed / 7 skipped / 0 failed；Server 与 Provider Host build PASS。证据见
  [`stage-5-codex-attested-tool-surface-local-acceptance-20260729.md`](../reports/stage-5-codex-attested-tool-surface-local-acceptance-20260729.md)。
- Codex MCP attestation 从“名称集合”提升为精确 transport + credential containment：session/fork 的 URL 与
  token 现在来自同一 scoped lease；有效 `synara` 配置必须是 numeric-loopback `/mcp`、精确字段集和指定
  `bearer_token_env_var`，shell policy 必须排除 gateway token 及 model-provider `env_key` /
  `env_http_headers` credential mapping，且不能经 `set` / `include_only` / 非默认继承重引入；无 lease 的
  discovery MCP 固定为空集。真实 0.145.0 `initialize` + `hooks/list` + `config/read` 探针同时证明 hook/config
  attestation 与 `discoveryMcpServers=[]`。Shared 453 passed / 1 skipped、Provider Host 155/155、Server 3109
  passed / 7 skipped / 0 failed；两项 build 与 `git diff --check` PASS。证据见
  [`stage-5-codex-mcp-transport-attestation-local-acceptance-20260729.md`](../reports/stage-5-codex-mcp-transport-attestation-local-acceptance-20260729.md)。
- Codex model-provider 静态 transport credential 继续收口：隔离 config 的 HTTP(S) base URL 禁止
  userinfo/query/fragment；inline 或任意 `query_params` 均 fail closed，仅保留 pinned 0.145 source 已验证的
  dedicated-table `api-version=YYYY-MM-DD[-preview]` 非 secret 兼容形态。真实 0.145 `initialize` +
  `hooks/list` + `config/read` probe 同时证明该 query shape、环境式 key/header 与全部 shell exclusion 可用。
  Manager 聚焦 123 passed / 2 skipped；最终全包/构建证据见
  [`stage-5-codex-model-provider-credential-containment-local-acceptance-20260729.md`](../reports/stage-5-codex-model-provider-credential-containment-local-acceptance-20260729.md)。
- Provider 出站代理凭证边界继续下沉：Kubernetes Target、agentd 与 Provider Host 现在共享/复验
  credential-free HTTP(S)/SOCKS5 authority 语义，拒绝 userinfo、路径、query/fragment、非法 host/port、
  SOCKS5 缺失端口与 `NO_PROXY=*`/超界列表。带认证的代理 URL 不再依赖终端 redaction，因为模型工具能直接
  读取自身环境；需要认证的企业代理必须终止在 Execution-local credential-hiding gateway，Provider 只拿
  无凭证端点。验证与本地证据见
  [`stage-5-provider-proxy-credential-containment-local-acceptance-20260729.md`](../reports/stage-5-provider-proxy-credential-containment-local-acceptance-20260729.md)。
- 服务端权威敏感动作 classifier 补齐 Git/package-manager 全局参数、更多 CI/依赖/Credential 路径与
  mutating-file content 扫描，修复了 flagged `git fetch`、flagged package install、写入新 URL/Credential
  引用可漏分类的可复现缺口。Codex 当前 `apply_patch` 内容继续通过 command 规则覆盖，内联 guard 为 3909
  characters，保留 4096 fail-closed 上限。共享全包 456 passed / 1 skipped、Provider Host 169/169、Server
  3111 passed / 7 skipped、Acceptance Runner 263/263、Stage 5 matrix 12/12 PASS；完整证据见
  [`stage-5-sensitive-action-classifier-local-acceptance-20260729.md`](../reports/stage-5-sensitive-action-classifier-local-acceptance-20260729.md)。
  本次对话已获明确授权并完成 workspace `bun fmt`、`bun lint` 与最终 8/8 package `bun typecheck`；格式化后
  targeted oxfmt/oxlint、受影响回归、两项 production build 与 `git diff --check` 也通过。Web browser fixture
  因本机缺少 Playwright Chromium executable 未启动任何测试，不计为通过；对应 type-only fixture 已由 Web
  typecheck 覆盖。托管 Provider × Worker 证据仍缺失，Stage 5 保持 IN PROGRESS。
- 新增单节点 Debian 12 物理 K3s `v1.34.9+k3s1` 验收环境：kubelet `podPidsLimit=128`、Secrets
  Encryption、exact-digest Worker image 与 Provider 节点标签均已实测。资源压力验收得到 peer 12/12、
  fork 120 started / 136 rejected、finite CPU、memory OOMKilled、metadata blocked；Projected Token
  init-to-main handoff 也通过。Acceptance Runner 同时补齐 operator-provided remote image 不依赖本机 Docker
  cache，以及固定 Worker-only proxy port 供受控 tunnel 使用；相关 Python 276/276 PASS。K3s 内建
  kube-router 的 policy 在收敛后能保留 DNS并阻断 peer/metadata，但新 Pod 立即执行时抓到 policy 编程窗口。
  当前 `network-boundary-init` 会在 registration/agentd 前要求连续两轮 metadata 阻断，并由 workload identity
  attestation 证明两个 init 的顺序与限制；这关闭了 Synara Provider 启动窗口，但不冒充通用 policy-at-create
  或 managed CNI 证明。clean Runner source `eac487fc`、immutable Worker digest `sha256:589d0639…` 上的最终
  Codex/Claude × `synara-k3s-1` 聚合矩阵为 2/2 PASS；metadata、credential-scope、malicious denial、restart
  continuity、exact cleanup 与聚合 Secret scan 全部通过，Control Plane 日志中 generation fact 缺失、注册 401、
  ERROR 均为 0。完整记录见
  [`stage-5-k3s-runtime-isolation-physical-acceptance-20260729.md`](../reports/stage-5-k3s-runtime-isolation-physical-acceptance-20260729.md)。
  单节点物理证据仍不能代替目标 managed CNI/IAM/Region 和完整 Ready Worker 集合，Stage 5 保持 IN PROGRESS。

2026-07-29 完成审计：

- Stage 4 已明确正式支持面仅为自建 Kubernetes，EKS/GKE/AKS 专属集成 deferred。此前以“托管
  CNI/IAM/Region”反向阻塞 Stage 5 与该权威边界冲突，现已纠正；这不是宣告托管云通过。
- 物理 K3s 的首尾 Ready/非 cordon Worker 完整集合一致，资源压力和 Codex/Claude 三项逐节点矩阵均
  通过，因此完成当前正式支持面的 Stage 5 验收。每个后续 Target 仍须独立重跑，证据不可跨集群继承。
- Stage 9 真实 webhook 尚不存在。当前恶意 Issue 形态的 automation product-path 与真实 Provider denial
  分层门禁已通过；未来 webhook adapter 的 signature、身份映射、幂等和无人值守拒绝 E2E 保留为 Stage 9
  自身上线门禁，不用不存在的功能阻塞 Stage 5，也不提前宣告该 adapter 通过。
- 完成边界与残余门禁见
  [`stage-5-completion-boundary-20260729.md`](../reports/stage-5-completion-boundary-20260729.md)。Stage 5
  状态更新为 **DONE**。
