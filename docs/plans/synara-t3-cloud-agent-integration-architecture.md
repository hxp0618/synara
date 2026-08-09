# Synara × T3 Cloud Agent 插件化集成设计

- 设计状态：TARGET FROZEN（目标架构、authority 和主路径已确认；变更需新增 ADR）
- 实施状态：IN PROGRESS（Phase 0–3 source implementation in progress；M1 验收门禁尚未完成）
- 发布状态：NOT PUBLISHED / NOT DEPLOYED
- 日期：2026-08-08
- 确认日期：2026-08-08
- 实施更新：2026-08-09
- 目标宿主：Synara、[T3 Code](https://github.com/pingdotgg/t3code)
- Synara 基线：`codex/saas-tenancy-user` @ `fc9f63ac74eeb04cf201506972878ac15307a0e4`
- T3 Code 初次调研基线：upstream `main` @ [`a20923ce463335e89e92f5983d98a180536e8e7d`](https://github.com/pingdotgg/t3code/tree/a20923ce463335e89e92f5983d98a180536e8e7d)
- T3 Code 实施基线：upstream `main` @ [`a6c9b41f902fba2a4137806c09e829935e91baac`](https://github.com/pingdotgg/t3code/tree/a6c9b41f902fba2a4137806c09e829935e91baac)
- T3 Code 本地跟踪目录：`/Users/huang/devel/project/huang/business/t3code`
- Synara 实施 worktree：`/Users/huang/devel/project/huang/business/synara-t3-cloud-agent`
- T3 Code 实施 worktree：`/Users/huang/devel/project/huang/business/t3code-cloud-agent`

> 本文同时记录目标设计和当前隔离 worktree 的 source implementation 与 local validation evidence。
> `TARGET FROZEN` 表示不再改变公共
> Runtime、双宿主 authority 和 `T3EnvironmentLease` 的方向；`IN PROGRESS` 只表示源码骨架、定向测试和
> 本地构建达到附录 A 记录的边界，不表示 Phase 0–3/M1 已验收，也不表示已提交、发布、部署、完成真实
> Provider 付费 Turn，或达到 public beta/GA。每次推进 T3 Code commit 仍必须复核 Provider SPI。

## 0. 结论先行

实仓复核后，建议把方案收敛为**一个可移植 Runtime、两种宿主编排方式、一个完整云环境产品**，
而不是把“插件包”理解为一个可以直接 import 到任何 Agent GUI 的 T3 `ProviderDriver`。

1. 把现有 `@synara/provider-host` 提炼成无宿主依赖的 `@synara/cloud-agent-runtime`；
2. 把 Provider Host Protocol v2.2 基线、additive v2.3 `GenerateText`、Runtime Event v2、能力目录和
   错误语义提炼成稳定的 `@synara/cloud-agent-protocol`；
3. 把 Codex/Claude 提取成基于 `@synara/cloud-agent-provider-api` 的通用 Provider 包，不放在
   Runtime 的宿主私有子路径；
4. 公共 ABI 只暴露普通 JS/stdio 协议，不暴露 T3 或 Synara 的 Effect 类型；
5. T3 第一版由 **T3 仓内薄桥接 Driver** 调用 Runtime。该桥接属于 T3 构建，不把 T3 当前内部
   `ProviderDriver` 冒充成稳定的第三方插件 SDK；
6. T3 使用 Synara 云算力的正式形态，不是“本地 T3 + 远程 Provider”，而是 Synara 创建一个
   `T3EnvironmentLease`，让 T3 server、Provider Runtime、Workspace、Git、Terminal 和 checkpoint
   位于同一 sandbox；
7. T3 Client 通过现有 pairing/connection 模型连接该 T3 server，managed 模式使用 DPoP-bound
   exchange；pairing token 只作为一次性短期 bootstrap credential，不能直接访问业务 API；第一版不新增 `SynaraConnectionTarget`，
   不改 Web/Desktop/Mobile 的连接模型；
8. Synara-native 模式继续由 Control Plane 拥有 Session/Turn/Execution；T3-on-Synara 模式则由
   T3 server 拥有 Thread/Turn/Checkpoint。两种模式复用 Runtime，不共享同一份编排权威；
9. `delegated-control-only` 只保留为后续实验/观察入口，不进入第一批正式交付，因为 T3 当前没有
   足够的 Workspace/Terminal/VCS capability 开关来安全降级。

目标交付拓扑如下；这是 M1/M2 目标，不是当前 source implementation 的物理依赖图：

```text
发布包
├── @synara/cloud-agent-protocol     # 稳定、app-neutral
├── @synara/cloud-agent-provider-api # 通用 Provider Plugin ABI；无 Effect/宿主类型
├── @synara/cloud-agent-runtime      # 通用装载/会话内核；out-of-process 优先
├── @synara/cloud-agent-provider-codex
├── @synara/cloud-agent-provider-claude
├── @synara/cloud-agent-testkit      # 双宿主 conformance
└── @synara/cloud-agent-distribution # managed release 必需：manifest、bin、校验和；不导出 T3 内部 SPI

Synara 构建
├── @synara/provider-host            # 兼容壳
├── agentd adapter
└── T3EnvironmentLease controller    # 后续新增的云环境产品面

T3 构建/薄 fork
├── SynaraCloudAgentDriver.ts        # T3-owned Effect bridge
├── SynaraCloudAgentTextGeneration.ts
├── contributedDrivers.ts            # 显式 composition seam
└── @synara/cloud-agent-distribution # 固定 Runtime + Provider 包版本和 digest
```

### 0.1 收拢后的唯一目标与主路径

**唯一目标**：先把 Codex/Claude 的单一实现物理拆入七个可发布、app-neutral 包，并让 Synara 与 T3
Embedded 通过同一套进程级 conformance；随后只把同一 T3 bridge 连同完整 T3 server 部署进
generation-fenced `T3EnvironmentLease`，以 `create → ready → terminate` 作为首个云 MVP。

```text
M0 基线与兼容证据
  ↓
M1 Portable Runtime RC + T3 Embedded 验收（当前唯一近程里程碑）
  ├── M1 Release Gate → RC → 可选 G-EXPOSURE
  └── M2 T3 Environment Lease MVP（完整 T3 server 同 sandbox）
        └── M2 Release Gate → RC → 可选 G-EXPOSURE
```

| 轨道                               | 目标与退出条件                                                                 | 当前状态                                  |
| ---------------------------------- | ------------------------------------------------------------------------------ | ----------------------------------------- |
| M0 基线                            | golden frames、真实 Codex/Claude happy/failure path、前后行为可比较            | 未完成真实 Provider 路径                  |
| M1：Phase 1–3                      | 七个包 + T3 thin bridge + Synara 兼容壳 + 双宿主进程级 conformance + 不可变 RC | source implementation in progress，未验收 |
| M2：Phase 4                        | Lease、同 Workspace、pairing/DPoP、generation/broker、RBAC、计量、审计         | 未开始                                    |
| Release M1 / M2                    | 各自 exact pin、外部 install/rollback、digest/provenance/SBOM                  | M1 open；M2 not started                   |
| G-EXPOSURE                         | 每个 RC 独立批准用户范围、支持等级、channel、回滚与事故响应                    | not started                               |
| Deferred D1：Suspend/Resume        | quiesce、SQLite/Workspace 一致快照、新 generation resume                       | 不阻塞 M2 MVP                             |
| Deferred D2：Generic UX / upstream | composition seam、server-advertised descriptor、generic UX、小型 upstream PR   | 不阻塞 M1/M2                              |
| Deferred D3：Polaris delegated     | control-only 产品需求与全套 Workspace capability 降级                          | 默认关闭                                  |
| Deferred D4：生态扩展              | 其他 Provider、动态目录/市场、公共 T3 Provider SDK                             | 不进入首批范围                            |

M1 未通过前可以做 M2 原型设计，但不能把 Environment Lease 标为受支持产品；M2 不依赖 D1/D2/D3/D4。M1
必须生成可验证的内部 Distribution candidate，M2 只消费其不可变 digest；公开 Registry 发布与上游接受是
独立 exposure 决策，不能反向阻塞架构验证。

本文中，**Distribution candidate** 是带固定内容与待签 release manifest 的内部不可变制品；**RC** 是已
关闭对应工程 Gate、可提交发布评审的 candidate；**exposure** 是经产品/运维批准后向指定用户范围提供支持，
公开 npm/Registry 只是 exposure channel 之一。工程里程碑完成、RC 形成和公开 exposure 不互相冒充。

文档发生冲突时按以下顺序解释：本节与第 22 节的确认决策 → 第 4/17/19 节的目标和门禁 → 附录 A 的
时点证据。附录 A 只能证明当前进度，不能修改目标或提升发布状态。

### 0.2 本次 T3 Code 实仓复核带来的修正

| 实际源码事实                                                                                                                                                    | 对原设计的修正                                                                                     |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `ProviderDriver` 虽是 record，但它的 config decoder、`create()`、`ProviderInstance`、adapter 和 text generation 都直接使用 T3 内部 Effect/contract/service 类型 | 公共包不能直接稳定导出 T3 Driver；必须有 T3-owned bridge                                           |
| `BUILT_IN_DRIVERS`、`BuiltInDriversEnv` 和 Registry Hydration 静态绑定                                                                                          | “安装包 + import + 数组一项”低估了类型环境、Layer 注入和 hydration 改动                            |
| `ProviderDriverKind` 与 `providerInstances.config` 是开放 envelope，未知 driver 可以 round-trip                                                                 | Server settings contract 无需为 Synara driver 改 schema，这是可利用的稳定点                        |
| 设置页的 provider metadata、添加向导和 settings schema 仍由前端静态列表定义                                                                                     | 薄 fork 第一版必须补静态 metadata/config schema；后续改为 server-advertised driver descriptor      |
| `turn.diff.updated` 只创建 missing placeholder；`CheckpointReactor` 随后读取本地 cwd、捕获本地 Git ref 并生成真正 diff                                          | 远端 Provider 事件不能成为当前 T3 的权威 diff；单加 `CheckpointAuthority` 不足以完成远端 Workspace |
| T3 的远程架构明确规定一个 server 同时拥有 provider、orchestration、terminal、git 和 filesystem                                                                  | 正式云集成应移动完整 T3 server，而不是拆开 T3 runtime                                              |
| 现有 `BearerConnectionTarget` 能配对任意可达 T3 endpoint                                                                                                        | Synara 只需返回标准 pairing URL，不需要新增连接类型和四端适配                                      |

复核证据：

- [`ProviderDriver.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/provider/ProviderDriver.ts)；
- [`builtInDrivers.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/provider/builtInDrivers.ts) 与
  [`ProviderInstanceRegistryHydration.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/provider/Layers/ProviderInstanceRegistryHydration.ts)；
- [`providerInstance.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/packages/contracts/src/providerInstance.ts)；
- [`ProviderRuntimeIngestion.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/orchestration/Layers/ProviderRuntimeIngestion.ts) 与
  [`CheckpointReactor.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/orchestration/Layers/CheckpointReactor.ts)；
- [`remote.md`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/docs/internals/remote.md) 与
  [`connection/model.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/packages/client-runtime/src/connection/model.ts)；
- [`providerDriverMeta.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/web/src/components/settings/providerDriverMeta.ts) 与
  [`ProviderInstanceCard.tsx`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/web/src/components/settings/ProviderInstanceCard.tsx)。

因此，“插件化”的稳定边界应当放在 **Cloud Agent Runtime 进程协议**，而不是当前 T3 内部的
TypeScript SPI；“云端化”的稳定边界应当放在 **T3 Execution Environment 租约**，而不是远程
Provider Adapter。

## 1. 为什么不能直接复制一份代码

### 1.1 两个项目看起来相近，但权威边界不同

T3 Code 当前是一个本地或远程部署的单体执行环境：一个 T3 server 持有 Provider 进程、项目目录、
Git、Terminal、Checkpoint 和线程状态，客户端通过 Effect RPC WebSocket 连接。它目前已经具备：

- 开放字符串形态的 `ProviderDriverKind`；
- `ProviderDriver` → `ProviderInstance` → `ProviderAdapter` 的驱动模型；
- 运行时实例注册、配置解码和 Scope 生命周期；
- Provider Runtime Event → event-sourced orchestration 的投影；
- Web、Desktop、Mobile 共享同一服务端执行边界。

相关当前源码：

- [T3 Code Architecture](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/docs/internals/overview.md)
- [Provider architecture](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/docs/internals/providers.md)
- [`ProviderDriver.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/provider/ProviderDriver.ts)
- [`ProviderAdapter.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/provider/Services/ProviderAdapter.ts)
- [`providerInstance.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/packages/contracts/src/providerInstance.ts)

Synara 的 Cloud Agent 则是分布式权威模型：

- Go Control Plane 持有 Tenant、Project、Session、Turn、Execution 和幂等权威；
- Target、Pool、Worker、Generation 决定执行位置与 fencing；
- `agentd` 持有 Worker 身份、Workspace 物化、凭证 Grant、Artifact 上传和恢复；
- Provider Host 只负责 Provider 会话、Turn 和标准化事件；
- gVisor/Cocoon、Credential Grant、KMS、RBAC、Audit、Billing、DR 等属于平台权威。

因此，可移植层只能覆盖两边的交集：**Provider Runtime + Session/Turn 控制 + 标准事件**。分布式
调度、租户隔离和平台治理不能假装是一个本地 npm 插件能够复制的能力。

### 1.2 “本地 T3 + 远程 Provider”有 Workspace 双权威问题

如果只在 T3 Code 里新增一个调用 Polaris API 的 Provider Adapter：

```text
T3 server（本机）                 Synara Worker（远端）
├── 本地 Project/Workspace A     ├── 远端 Workspace B
├── 本地 Git Checkpoint          ├── agentd Checkpoint
├── 本地 Terminal                ├── Provider Host 修改 B
└── 远程 Provider Adapter ──────►└── Session / Turn
```

聊天流可以工作，但会产生以下错误体验：

- Agent 改了远端 B，T3 文件树仍显示本地 A；
- T3 的 Turn checkpoint 捕获 A，远端 diff 来自 B；
- T3 Terminal 操作 A，Agent 命令运行在 B；
- Revert、Fork、Rollback 可能同时由两边执行；
- 用户看到“完成”，但当前可视目录没有那些文件。

因此本文把正式路径明确分为 `t3-embedded` 和 `t3-environment-lease`；另把
`delegated-control-only` 隔离为默认关闭的实验路径，不允许用同一个模糊的 `remote=true` 掩盖差异。

### 1.3 Effect 不能成为插件 ABI

当前 Synara 使用自有固定提交的 Effect 4 beta，T3 Code 基线使用 `effect@4.0.0-beta.103`。即便
TypeScript 接口形状相似，跨包直接暴露 `Effect.Effect`、`Layer`、`Context.Service` 或品牌类型，
也会造成：

- 双份 Effect runtime；
- 类型参数和 Service Tag 不兼容；
- 宿主升级被插件锁死；
- 一个项目的内部依赖进入另一个项目的公共 ABI。

所以公共插件 ABI 必须是普通 TypeScript/JavaScript：`Promise`、`AsyncIterable`、`AbortSignal`、
纯 JSON 值和结构化错误。两个宿主在适配层内各自转换到自己的 Effect 版本。

## 2. 目标与非目标

### 2.1 目标

1. Cloud Agent Runtime 的同一份实现能够被 Synara `agentd` 和 T3 Code Provider Driver 调用。
2. 保留 Provider Host Protocol v2.2 既有命令与 Runtime Event v2 语义；首批以 additive minor 2.3
   增加 `GenerateText`，不得破坏 2.2 golden frame 的读取与既有命令语义。
3. T3 Code 集成只复制宿主桥接，不复制 Provider 实现；薄 fork 的补丁面有明确文件预算和跟踪基线。
4. Synara 集成保持 `apps/provider-host`、Worker 镜像和 `agentd` 现有行为兼容。
5. Runtime 不知道 Tenant、Kubernetes、gVisor、Cocoon、T3 RPC 或任何 UI 状态。
6. 每种运行模式只有一个编排权威和一个 Workspace/VCS/Checkpoint 权威；环境平台与 Agent 编排的
   所有权分别建模。
7. Provider 能力必须运行时协商，不能因宿主 UI 有按钮就假设 Provider 支持。
8. 重启、断流、重复命令和乱序事件必须有确定行为。

### 2.2 非目标

- 不把 Synara Control Plane 改写成 TypeScript。
- 不把 Tenant/RBAC/Billing/Audit/Worker Pool 塞进插件。
- 不在第一阶段设计插件市场或自动下载第三方可执行文件。
- 不让插件直接获得宿主数据库连接。
- 不把 MCP 当成 Agent Session 生命周期协议；MCP 仍用于工具发现与调用。
- 不承诺 T3 Code 上游会接受该插件；先做可维护的薄集成，再准备上游化。
- 不把“在 T3 Code 能聊天”描述为完整的 Cloud Workspace 集成。

## 3. 可移植 Cloud Agent 的领域边界

### 3.1 一个 Cloud Agent Runtime 应该负责什么

```text
CloudAgentRuntime
├── Describe / capability negotiation
├── Start / resume / stop Provider session
├── Send / steer / interrupt / suspend Turn
├── Approval / structured user input
├── Compact / review / provider-history rollback / fork
├── Provider-native cursor and authoritative-history reconstruction
├── Canonical Runtime Event v2 projection
├── Provider process lifecycle and bounded backpressure
└── Stable, redacted error classification
```

### 3.2 Runtime 明确不负责什么

```text
HostAuthority
├── Workspace create/materialize/mount/cleanup
├── Git checkpoint and workspace rollback
├── Artifact physical storage and signed grants
├── Credential authorization and secret lifecycle
├── Tenant/Organization/Project authorization
├── Worker placement, warm pool and scheduling
├── Generation/lease/workload identity fencing
├── container/gVisor/microVM isolation
├── cost, quota, audit, retention and DR
└── UI, RPC, database and client connection
```

### 3.3 “Cloud Agent”不是单一进程

产品概念应拆为三个同心层：

```mermaid
flowchart TB
  subgraph P["Portable Runtime"]
    Proto["Protocol and schemas"]
    Runtime["Provider runtime kernel"]
    Events["Canonical event normalization"]
  end

  subgraph H["Host Adapter"]
    SynaraAdapter["Synara agentd adapter"]
    T3Adapter["T3 ProviderDriver adapter"]
  end

  subgraph A["Host Authority"]
    SynaraAuthority["Synara Control Plane, Worker, Workspace, Security"]
    T3Authority["T3 orchestration, Git, terminal, filesystem"]
  end

  Proto --> Runtime
  Runtime --> Events
  Events --> SynaraAdapter
  Events --> T3Adapter
  SynaraAdapter --> SynaraAuthority
  T3Adapter --> T3Authority
```

Polaris `delegated-control-only` 属于 Deferred D3 的替代 backend，不是第三种主路径 Host Adapter，也不进入
核心拓扑。

## 4. 目标架构

### 4.1 总体拓扑

```mermaid
flowchart TB
  subgraph Core["Published Cloud Agent packages"]
    Contracts["cloud-agent-protocol"]
    Kernel["cloud-agent-runtime"]
    Testkit["cloud-agent-testkit"]
  end

  subgraph Native["Synara-native orchestration"]
    Agentd["agentd"]
    CP["Go Control Plane"]
    Compat["provider-host compatibility wrapper"]
    NativeWorkspace["Synara Workspace, Checkpoint and Artifact"]
  end

  subgraph T3Local["T3 embedded"]
    Driver["T3-owned Cloud Agent bridge"]
    Orchestrator["T3 orchestration"]
    T3Workspace["T3 filesystem, VCS, terminal and checkpoint"]
  end

  subgraph Cloud["T3 on Synara Environment Lease"]
    Lease["Synara lease, placement, isolation, ingress and cost"]
    Sandbox["One generation-fenced sandbox"]
    CloudT3["T3 server: orchestration authority"]
    CloudBridge["T3-owned Cloud Agent bridge"]
    CloudWorkspace["One shared filesystem, VCS, terminal and checkpoint"]
    Broker["Synara short-lived credential broker"]
  end

  Contracts --> Kernel
  Testkit --> Kernel
  Kernel --> Compat
  Compat <--> Agentd
  Agentd <--> CP
  Agentd --> NativeWorkspace
  Kernel --> Driver
  Driver <--> Orchestrator
  Orchestrator --> T3Workspace
  Lease --> Sandbox
  Sandbox --> CloudT3
  Sandbox --> CloudBridge
  Sandbox --> CloudWorkspace
  CloudT3 <--> CloudBridge
  CloudT3 --> CloudWorkspace
  CloudBridge --> Kernel
  CloudBridge --> Broker
```

云环境中的 Synara 和 T3 各自只拥有自己那一层：

- Synara：Tenant/RBAC、环境租约、调度、隔离、generation、入口、凭证授权、用量和回收；
- T3 server：Project、Thread、Turn、Provider session、文件、Git、Terminal 和 checkpoint；
- Cloud Agent Runtime：Provider 进程、Provider session、Turn 命令和标准事件；
- T3 Client：只连接一个普通的 T3 `ExecutionEnvironment`，不知道底层是否由 Synara 创建。

这意味着 T3-on-Synara 不复用 Synara-native 的 `Session/Turn/Execution` 编排表作为第二权威。平台可
记录 lease 级审计与计量，也可以记录 opaque T3 activity telemetry，但不能同时对同一个 Turn 执行
Control Plane 状态机。

### 4.2 两种 Host Adapter 与两种 T3 部署 Profile

Host Adapter 只有两种，分别对应两套互不重叠的 Agent 编排权威：

| Host Adapter    | Agent 编排权威       | Workspace/VCS/Checkpoint 权威   | 决策                  |
| --------------- | -------------------- | ------------------------------- | --------------------- |
| Synara adapter  | Synara Control Plane | agentd / Synara Worker          | 保留现状              |
| T3-owned bridge | T3 server            | 同一个 T3 execution environment | M1/M2 复用同一 bridge |

T3-owned bridge 再按部署位置分成两个 profile；它们不是两套 Provider 实现：

| T3 Deployment Profile  | Runtime/Workspace 位置                            | 使用场景                  | 里程碑 |
| ---------------------- | ------------------------------------------------- | ------------------------- | ------ |
| `t3-embedded`          | Runtime 与 T3 server/Workspace 同机               | 本地 T3 或用户自管远程 T3 | M1     |
| `t3-environment-lease` | T3 server、Runtime、Workspace 同一 Synara sandbox | T3 使用 Synara 云算力     | M2     |

第一阶段先完成 `t3-embedded`，证明 Runtime 可以脱离 Synara 控制面；M2 只改变部署和环境供给，不新增第二套
T3 bridge。`delegated-control-only` 使用 Synara 编排权威和远端 Workspace，是 Deferred D3 的不同 backend，
不属于上述 profile，也不是任何主路径里程碑的前置阶段。

### 4.3 为什么 Environment Lease 不是另一种 Synara Session

如果把 T3 Thread 映射成 Synara Session，再让 T3 继续运行自己的 orchestration，会出现两套 turn
receipt、两套 checkpoint、两套 rollback 和两套结算终态。`T3EnvironmentLease` 应是计算/环境资源，
其最小模型更接近：

```ts
type T3EnvironmentLease = {
  readonly leaseId: string;
  readonly tenantId: string;
  readonly projectId: string;
  readonly generation: number;
  readonly state:
    | "provisioning"
    | "ready"
    | "quiescing"
    | "snapshotting"
    | "suspended"
    | "resuming"
    | "terminating"
    | "terminated"
    | "failed";
  readonly environmentId: string;
  readonly workspaceVolumeId: string;
  readonly t3StateVolumeId: string;
  readonly cloudAgentDistributionReleaseDigest: string;
  readonly expiresAt: string;
};
```

`environmentId` 与 T3 `stateDir/environment-id` 一致并在同一 lease 的 suspend/resume 间持久化；
`generation` 每次执行实例替换递增，用于入口、broker 和旧进程 fencing。T3 的数据库、server settings、
Provider binding 和 Workspace 要放在受控持久卷，不能依赖一次性容器层。Phase 4 MVP 只开放
`provisioning → ready → terminating → terminated/failed`；`quiescing` 到 `resuming` 是 Deferred D1
通过一致性门禁后才能开放的扩展状态。

双方都可以做最小、协调的修改，但不能形成双权威。固定职责如下：

| 修改位置            | 允许调整的职责                                                                | 禁止成为的第二权威                                     |
| ------------------- | ----------------------------------------------------------------------------- | ------------------------------------------------------ |
| Synara              | Environment Lease/controller、调度、generation、ingress、broker、审计和计量   | T3 Thread/Turn、Provider Session、Workspace checkpoint |
| T3 Code 薄集成层    | contributed-driver seam、Bridge、managed auth/revoke、quiesce hook 和设置入口 | Synara Tenant/RBAC、Lease、Generation、Broker          |
| Cloud Agent Runtime | Provider 进程/会话、Turn 命令、标准事件和 receipt                             | Workspace/VCS/Checkpoint、平台资源生命周期             |

任何后续“复用”都先抽 helper/contract，再由单一 owner 落库和推进状态；不得让 Synara 和 T3 同时写同一个
Turn、Workspace 或 Checkpoint 的终态。

### 4.4 Synara 现有能力的复用与不能直接复用的部分

实仓复核表明，Environment Lease 可以复用大量**机制**，但不能伪装成当前 `agent_executions` 的一行：

| Synara 当前能力                                                                                  | 可复用部分                                                             | 必须新增/调整                                                                                                                          |
| ------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `execution_targets`、Worker Pool、placement/capacity、Kubernetes allocation、SandboxClaim/Cocoon | Target 选择、容量、runtime isolation、release pin、Worker/Sandbox 健康 | 新增长生命周期 environment workload/controller；不能要求每个 lease 绑定一个 Agent Turn                                                 |
| `agent_executions` + `worker_leases`                                                             | generation、lease token hash、heartbeat、fencing 的事务模式            | 当前 Execution 强制 FK 到 Session/Turn，不能创建假 Turn；新增 environment lease/generation 表                                          |
| `remote_workspaces` + `workspace_materializations` + agentd materializer                         | 安全路径、incarnation、manifest、cleanup、cache、snapshot 代码         | 当前 Workspace/Materialization 强制绑定 Session 和 Execution；新增 lease-owned materialization，不能填假 Session ID                    |
| `execution_provider_credential_grants`                                                           | 不可变 credential version snapshot、revoke/rotation/fencing 原则       | 当前 grant 一对一绑定 Execution generation；Lease 需要按 Provider Instance/credential profile 的多 grant 模型                          |
| agentd `provider_credential_broker`                                                              | 上游 allowlist、header 注入、受控 proxy、task token、secret 不下发原则 | 当前实现是单 RunnerCredential 的 loopback TCP reverse proxy；Lease 需要多实例、lease/generation-aware、Unix socket/vsock 的长期 broker |
| Worker release/manifest/attestation                                                              | 固定 T3、Runtime、Provider CLI digest 与供应链证据                     | 增加 `runtimeKind=t3-code` release unit 和 T3 server health/version attestation                                                        |

推荐新增的最小持久化聚合：

```text
t3_environment_leases
├── tenant / organization / project / target
├── state / current_generation / environment_id
├── workspace_volume / t3_state_volume / runtime_state_volume
├── lifecycle policy snapshot
└── release manifest reference

t3_environment_lease_generations
├── lease_id / generation
├── worker or sandbox incarnation
├── lease token hash / heartbeat / expiry
├── allocation / ingress route / ready attestation
└── started / suspended / terminated facts

t3_environment_credential_grants
├── lease_id / generation / provider_instance_id
├── credential id / immutable version / allowed provider
└── expiry / revoke state / broker policy digest

t3_environment_endpoints
├── lease_id / generation / route id
├── HTTPS/WSS base / state / auth policy digest
└── advertised / revoked / drained timestamps
```

长期可以把表名提升为通用 `agent_environment_*`，用 `runtime_kind=t3-code` 区分；MVP 不应先做
无实际第二宿主的泛化迁移。Service 层应先抽公共 fencing/materialization/broker helper，数据库外键仍
保持两种 authority 分离。

## 5. 包拆分

### 5.1 `@synara/cloud-agent-protocol`

职责：公共 schema、协议版本、命令/消息 envelope、事件、能力和错误。

约束：

- 不依赖 Synara `@synara/contracts`；
- 不依赖 T3 `@t3tools/contracts`；
- 不导出 Effect 类型；
- 浏览器安全的部分不得 import `node:*`；
- Provider kind 使用开放 slug，不使用当前 Synara 的闭集 `ProviderKind`；
- JSON Schema 作为跨语言和 wire conformance 的来源；
- TypeScript 类型从同一个 schema 生成或被一致性测试约束。

建议导出：

```ts
export type CloudAgentProviderKind = string & { readonly __brand: "CloudAgentProviderKind" };

export type CloudAgentProtocolVersion = {
  readonly major: number;
  readonly minor: number;
};

export type CloudAgentCommand =
  | DescribeCommand
  | StartSessionCommand
  | ResumeSessionCommand
  | SendTurnCommand
  | SteerTurnCommand
  | InterruptTurnCommand
  | SuspendTurnCommand
  | ResolveApprovalCommand
  | ResolveUserInputCommand
  | CompactSessionCommand
  | RollbackSessionCommand
  | ForkSessionCommand
  | StartReviewCommand
  | GenerateTextCommand
  | StopSessionCommand;

export type CloudAgentMessage =
  | RuntimeEventMessage
  | InteractionRequestMessage
  | ArtifactCandidateMessage
  | CheckpointMessage
  | ProgressMessage
  | ResultMessage
  | ErrorMessage;
```

现有来源：

- [`provider-host-v2.md`](../contracts/provider-host-v2.md)
- [`runtime-event-v2.md`](../contracts/runtime-event-v2.md)
- `packages/contracts/src/providerHost.ts`
- `packages/contracts/src/providerRuntime.ts`
- `packages/cloud-agent-protocol/provider-capability-catalog.json`（唯一可编辑来源）

迁移时先复制 schema 到新包，再让 `@synara/contracts` 从新包 re-export；不能同时保留两份可编辑
定义。

Provider Host v2.2 没有覆盖 T3 `ProviderInstance.textGeneration` 要求的 thread title、branch、commit 和
PR 内容生成。目标是保持 v2.2 既有命令语义，并以 additive minor `2.3` 新增 `GenerateText`；当前 handler
仍接受 major 2 command，但 2.2 golden compatibility gate 尚未完成，不能从 focused test 外推。宿主仍须
通过 descriptor/capability 协商后使用：

```ts
type CloudAgentTextGenerationRequest =
  | { task: "thread-title"; message: string; previousTitle?: string; model?: string }
  | { task: "branch-name"; message: string; model?: string }
  | {
      task: "commit-message";
      branch: string | null;
      stagedSummary: string;
      stagedPatch: string;
      includeBranch: boolean;
      model?: string;
    }
  | {
      task: "pr-content";
      baseBranch: string;
      headBranch: string;
      commitSummary: string;
      diffSummary: string;
      diffPatch: string;
      changeRequestTemplate?: string;
      model?: string;
    };

type CloudAgentTextGenerationResult =
  | { task: "thread-title"; title: string }
  | { task: "branch-name"; branch: string }
  | { task: "commit-message"; subject: string; body: string; branch?: string }
  | { task: "pr-content"; title: string; body: string };
```

所有文本字段必须有单项与总 payload 上限，patch 超限时由 T3 先按现有 policy 截断；Runtime 返回值再
做 task-specific schema 校验。Runtime 未声明相应 task 时，bridge 返回稳定
`TextGenerationError`，不能创建空 service 或偷偷切到另一个内置 Provider。

### 5.2 `@synara/cloud-agent-runtime`

职责：当前 `apps/provider-host` 的可复用实现、显式 Provider registry 和会话内核。Runtime 包保留 stdio
兼容子入口，但 `cloud-agent-runtime` executable 只由 Distribution 发布。

建议入口：

```text
@synara/cloud-agent-runtime
@synara/cloud-agent-runtime/node
@synara/cloud-agent-runtime/stdio
```

Runtime 只负责装载、会话生命周期、命令路由和事件校验，不以内置子路径绑定 Codex/Claude。Provider
实现通过 `@synara/cloud-agent-provider-api` 显式注册；这使同一份 Codex/Claude 插件可以被 Synara 和
T3 Code 使用，而不是复制为两个宿主私有实现。

> 当前过渡实现尚未达到这一目标边界：默认 JS registry 已是 app-neutral，但历史 Provider Host、Codex
> App Server、Claude SDK 实现及其依赖仍位于 Runtime 包；Distribution 的 stdio bin 也仍进入 legacy
> Protocol handler。必须完成附录 A.5 的物理迁移与真实 registry 启动后，才能声明“Runtime 不内置
> Provider”。

对外 JavaScript ABI：

```ts
export interface CloudAgentRuntimeV1 {
  readonly abiVersion: 1;
  readonly providerKinds: ReadonlyArray<string>;
  describe(providerKind: string, signal?: AbortSignal): Promise<CloudAgentProviderDescriptor>;
  createSession(
    providerKind: string,
    input: {
      readonly hostInstanceId: string;
      readonly hostThreadId: string;
      readonly configuration: Readonly<Record<string, unknown>>;
    },
    host: CloudAgentHostServices,
    signal?: AbortSignal,
  ): Promise<CloudAgentProviderSession>;
}

export interface CloudAgentProviderSession extends AsyncDisposable {
  readonly sessionId: string;
  readonly events: AsyncIterable<CloudAgentMessageEnvelope>;
  execute(
    command: CloudAgentCommandEnvelope,
    signal?: AbortSignal,
  ): Promise<CloudAgentMessageEnvelope>;
  close(reason?: string): Promise<void>;
}

export interface CloudAgentHostServices {
  readonly workspace: {
    readonly authority: "host" | "external-readonly";
    readonly root: string | null;
    readonly runtimeOutputRoot?: string;
    readonly providerStateRoot?: string;
    readonly generation: number;
    readonly readOnly: boolean;
  };
  readonly credential: CloudAgentCredentialSource;
  readonly log: CloudAgentLogger;
  acceptArtifact?(candidate: CloudAgentArtifactCandidate, signal?: AbortSignal): Promise<void>;
}
```

这里的 `CloudAgentCredentialSource` 只允许一次性读取或匿名 FD，不接受“把 key 填进命令参数”的
实现。`acceptArtifact` 只接收候选和受控流，最终存储权仍在宿主。上述签名是已确认 ABI；legacy
Start/Resume command payload 仍携带 `RunnerInput` 属于 M1 待移除的内部迁移债务，不得提升为公共 ABI。

### 5.3 通用 Provider Plugin 包

公共 Provider 层拆成一个宿主无关 ABI 和独立实现包：

```text
@synara/cloud-agent-provider-api
@synara/cloud-agent-provider-codex
@synara/cloud-agent-provider-claude
```

`@synara/cloud-agent-provider-api` 只导出普通 TypeScript/JavaScript 结构、`Promise`、`AsyncIterable`、
`AbortSignal`、JSON Schema 和结构化错误，不导出 Synara/T3 类型或 Effect。Provider 包实现该 ABI，
声明自己的 upstream CLI/SDK probe、兼容范围、能力和配置 schema：

```ts
export interface CloudAgentProviderPluginV1 {
  readonly abiVersion: 1;
  readonly providerKind: string;
  describe(signal?: AbortSignal): Promise<CloudAgentProviderDescriptor>;
  createSession(
    input: {
      readonly hostInstanceId: string;
      readonly hostThreadId: string;
      readonly configuration: Readonly<Record<string, unknown>>;
    },
    host: CloudAgentHostServices,
    signal?: AbortSignal,
  ): Promise<CloudAgentProviderSession>;
}

const runtime = createCloudAgentRuntime({
  providers: [createCodexProvider(), createClaudeProvider()],
});
```

装载策略固定为编译期显式注册和 allowlist：不扫描 `node_modules`，不执行用户目录中的 JS，也不允许
Provider 包绕过 Runtime 直接写宿主数据库。第一批可执行 Provider 仅为 Codex 与 Claude；Cursor、
Antigravity、Grok、Kilo、OpenCode、Pi 先只保留扩展节奏和 capability mapping，待各自具备远程 Runtime
路径与 conformance 后逐个发布，不能因为已经进入 Host catalog 就标记为可移植 Provider。

当前 `createCodexProvider()` / `createClaudeProvider()` 只是把 legacy Provider Host 包装成 Plugin ABI 的
兼容 facade，不等于 Provider 实现已经独立。它们允许两个宿主先验证 ABI 和装载路径，但发布前仍须把
Codex/Claude 执行实现、probe 和 upstream 依赖分别迁入各自包，并证明安装 Codex 包不会间接拉入 Claude
SDK。

### 5.4 `@synara/cloud-agent-distribution`

这是本地库消费者可选、受控 Synara/T3 managed release 必需的安装/分发包，不是 T3 Provider SDK。
职责仅包含：

- 依赖并固定 Protocol、Runtime、Provider API 和选定 Provider 包的精确兼容版本；
- 暴露 Runtime bin、manifest、JSON Schema、digest 和版本探针；
- 提供 T3 fork 可消费的普通 JS helper 与 stdio client；
- 提供安装后人工注册说明；
- 不 import `@t3tools/*`、T3 内部源码或 Synara Control Plane 客户端；
- 不通过 `postinstall` 修改 T3 仓库。

当前包仍使用 `workspace:*` 依赖，且 Distribution 自身尚未导出 schemas 子路径；因此“精确 pin + schema
exposure”仍是 M1 制品门禁。`npm pack --dry-run` 只能证明文件可打包，不能证明依赖已改写、外部项目可
安装，或 bin/manifest/schema 能被真实消费者解析。

原先计划的 `@synara/cloud-agent-plugin-t3code` 名称可以保留为营销/分发别名，但在 T3 发布公共
Provider SDK 前，它不能承诺直接导出一个跨 T3 版本稳定的 `ProviderDriver`。

### 5.5 T3-owned Cloud Agent Bridge（非公共稳定 ABI）

桥接代码跟随 T3 源码构建，职责包括：

- 在 T3 自己的 Effect runtime 中实现 `ProviderDriver<Config, R>`；
- 构造 T3 要求的 `snapshot`、`adapter` 和 `textGeneration` 三组 per-instance closure；
- 把 `ProviderSessionStartInput`、Turn、approval、user-input、interrupt、rollback 投影成普通 Runtime
  命令；
- 把 Runtime Event v2 穷尽映射成 T3 `ProviderRuntimeEvent`；
- 每个 Provider Instance 持有独立的 child scope、process manager、binding store 和 event pump；
- 对四个 T3 text-generation 操作提供显式实现或稳定的 unsupported error，不能用空对象占位；
- 只通过 `@synara/cloud-agent-distribution` 暴露的普通 JS/stdio ABI 调用 Runtime，不直接绑定
  Provider 实现内部模块。

当前桥接文件固定在 T3 仓内：

```text
apps/server/src/provider/Drivers/SynaraCloudAgentDriver.ts
apps/server/src/provider/cloudAgent/CloudAgentProcess.ts
apps/server/src/provider/cloudAgent/CloudAgentProtocol.ts
apps/server/src/provider/cloudAgent/CloudAgentRuntimeProbe.ts
apps/server/src/provider/cloudAgent/SynaraCloudAgentAdapter.ts
apps/server/src/provider/cloudAgent/CloudAgentProcess.test.ts
apps/server/src/provider/cloudAgent/SynaraCloudAgentAdapter.test.ts
apps/server/src/provider/cloudAgent/SynaraCloudAgentAdapter.integration.test.ts
apps/server/src/textGeneration/SynaraCloudAgentTextGeneration.ts
apps/server/src/textGeneration/SynaraCloudAgentTextGeneration.test.ts
apps/server/src/provider/contributedDrivers.ts
apps/server/src/provider/Drivers/SynaraCloudAgentDriver.test.ts
apps/server/src/provider/contributedDrivers.test.ts
```

这层可以很薄，但不是零代码，也不是仅加一个数组项。它必须随每个固定的 T3 commit 运行 focused
conformance。等 T3 提取真正的 `@t3tools/provider-sdk` 后，才把它迁回 Synara 发布包。

### 5.6 `@synara/cloud-agent-backend-polaris`（Deferred D3）

职责：通过 `@polaris-agents/sdk` 调用 Synara Developer API，提供 delegated 模式。

它不是 Runtime 内核的替代品，而是另一个 `CloudAgentBackendV1` 实现：

```ts
export interface CloudAgentBackendV1 {
  createSession(input: CreateCloudSession, signal?: AbortSignal): Promise<CloudSessionHandle>;
  getSession(id: string, signal?: AbortSignal): Promise<CloudSessionHandle>;
}

export interface CloudSessionHandle {
  readonly id: string;
  sendTurn(input: CloudTurnInput, options: MutationOptions): Promise<CloudTurn>;
  events(options: EventStreamOptions): AsyncIterable<CloudSessionEvent>;
  interrupt(options: MutationOptions): Promise<void>;
  respondToApproval(input: ApprovalResponse, options: MutationOptions): Promise<void>;
  respondToUserInput(input: UserInputResponse, options: MutationOptions): Promise<void>;
  suspend(options: MutationOptions): Promise<void>;
  close(): Promise<void>;
}
```

现有 Polaris SDK 已覆盖 Session、Turn、SSE、Approval、user-input、interrupt、steer、compact、review、
rollback、fork、Artifact 和 usage，是这个包的传输基础。但当前 SDK 的 SSE 主要验证 sequence，插件层
仍必须对 `eventType + payload` 做完整 Runtime Event v2 校验，不能直接 cast。

### 5.7 `@synara/cloud-agent-testkit`

职责：任何 Runtime 或 Host Adapter 都必须通过的黑盒测试。

至少提供：

- golden NDJSON frames；
- Describe/版本协商；
- command idempotency；
- Start → Send → Event → Result；
- interrupt、approval、user-input；
- event gap、duplicate、late event；
- process crash 和 resume；
- bounded payload/backpressure；
- secret redaction；
- workspace path escape 和 symlink 负向测试；
- capability 与实际行为一致性。

当前 testkit 已覆盖 descriptor 值域、Protocol/Runtime Event vocabulary、完整 terminal correlation、
多路 transcript 的 late-frame/terminal 规则；尚未提供上述进程级、Workspace、安全和双宿主公共 suite，
因此只能称为 conformance foundation，不能称为双宿主黑盒验收完成。

### 5.8 `@synara/provider-host` 兼容壳

当前包名和 Worker 镜像入口先不删除：

```ts
#!/usr/bin/env node
import "@synara/cloud-agent-runtime/stdio";
```

兼容壳继续提供 `provider-host` bin，现有 `agentd`、镜像构建和运维脚本不需要同时切换。

## 6. 插件 Manifest 与加载策略

### 6.1 Manifest 草案

```json
{
  "schemaVersion": 1,
  "id": "com.synara.cloud-agent",
  "displayName": "Synara Cloud Agent",
  "distributionVersion": "0.1.0",
  "releaseState": "source",
  "protocol": "2.3",
  "runtimeProtocol": { "major": 2, "minor": 3 },
  "runtimeEventVersions": { "minimum": 2, "maximum": 2 },
  "providerPluginAbi": 1,
  "runtime": { "package": "@synara/cloud-agent-runtime", "version": "0.2.0" },
  "providers": [
    { "kind": "codex", "package": "@synara/cloud-agent-provider-codex", "version": "0.1.0" },
    { "kind": "claudeAgent", "package": "@synara/cloud-agent-provider-claude", "version": "0.1.0" }
  ],
  "entrypoints": {
    "node": "./dist/index.mjs",
    "stdio": "./dist/stdio.mjs"
  },
  "permissions": [
    "spawn-provider-process",
    "read-host-workspace",
    "write-host-workspace",
    "emit-artifact-candidates"
  ],
  "credentialDelivery": ["anonymous-fd"],
  "releaseDigest": null
}
```

源码/本地 dry-run manifest 的 `releaseState` 必须是 `source` 且 `releaseDigest` 为 `null`。只有 release
pipeline 对最终 tarball/bin/schema/SBOM 计算 SHA-256 并写入不可变发布记录后，才能生成 managed
candidate；不能在源码里预填一个看似有效但并未覆盖最终制品的 digest。

Manifest 只声明能力，不授予权限。宿主显式允许后才实例化。

### 6.2 第一阶段：编译期显式注册

T3 Code 基线的 `BUILT_IN_DRIVERS` 是静态数组，且未知 Driver 会成为 unavailable shadow。当前薄 fork
已经把 fork-owned 注册隔离到 `CONTRIBUTED_DRIVERS`，最终仍由应用 composition root 编译期 allowlist：

```ts
import { SynaraCloudAgentDriver } from "./Drivers/SynaraCloudAgentDriver.ts";

export const CONTRIBUTED_DRIVERS = [SynaraCloudAgentDriver];

export const BUILT_IN_DRIVERS = [
  CodexDriver,
  ClaudeDriver,
  CursorDriver,
  GrokDriver,
  OpenCodeDriver,
  ...CONTRIBUTED_DRIVERS,
];
```

第一版固定改动范围是：

1. T3-owned driver、transport、adapter、text-generation bridge 及 focused tests；
2. `contributedDrivers.ts` 与 `builtInDrivers.ts` 的组合、环境类型和 drift test；
3. Registry Hydration 继续只消费组合后的完整数组，不直接依赖 Synara 包；
4. 静态 `providerDriverMeta`、配置 schema、添加/编辑入口和 unavailable 回退；
5. `providerInstances` 配置 fixture，覆盖 UI 写入、round-trip 和 server restart；
6. 只通过固定的 out-of-process Distribution executable 通信；不引入跨仓 workspace dependency。

不使用 `postinstall` 修改宿主源码，不扫描全局 `node_modules`，不自动执行用户目录里的 JS。
手工编辑内部 settings JSON 只能用于测试/恢复，不作为受支持的主要产品安装路径。薄 fork 第一版必须让
用户通过现有设置页完成添加和修改；server-advertised descriptor 在后续替换静态 metadata，不阻塞首版。

更适合向上游提交的最小 seam 不是“内置 Synara”，而是把 Registry Hydration 参数化：

```ts
export const makeProviderInstanceRegistryHydrationLive = <R>(
  drivers: ReadonlyArray<AnyProviderDriver<R>>,
) => /* current hydration implementation */;
```

T3 的应用 composition root 再显式传入 `BUILT_IN_DRIVERS + CONTRIBUTED_DRIVERS`。这样第三方驱动仍是
编译期受信代码，但不会让 hydration 直接 import 一份全局数组。若上游不接受 seam，我们维护一个
小 fork，并用固定 commit + focused tests 控制漂移。

Deferred D2 可在这条最小 seam 之上增加 server-advertised `ProviderDriverDescriptor`：`driverKind`、展示名、
badge、配置 JSON Schema、secret field hints、capability 和可选 icon key，再让 generic UI 消费它。D2 不承诺
稳定公共 Provider SDK。

### 6.3 后续：公共 Provider SDK（Deferred D4）

如果准备向 T3 Code 上游提交，建议从它现有内部 SPI 提炼
`@t3tools/provider-sdk`，但这是上游 API 决策，不应该阻塞本项目第一版。公共 SDK 至少应包含：

- `ProviderDriver`/`ProviderInstance` 的稳定结构类型；
- HostContext（instance state dir、logger、process spawner、secret source）；
- ProviderAdapter 基础契约；
- conformance test helper；
- ABI/version probe。

该 SDK 以 D2 已验证的 composition seam 和 descriptor 为输入，但它本身、第三方签名策略、其他 Provider
与动态目录/市场全部归 D4。

## 7. Host Adapter 设计

### 7.1 Synara Adapter

Synara 第一阶段保持现有 wire：

```mermaid
sequenceDiagram
  participant CP as Control Plane
  participant Agentd as agentd
  participant Host as cloud-agent-runtime
  participant Provider as Provider CLI or SDK

  CP->>Agentd: Claim with Execution and Generation
  Agentd->>Agentd: Materialize Workspace and resolve Grant
  Agentd->>Host: Describe over NDJSON stdio
  Host-->>Agentd: Protocol, capabilities, runtime versions
  Agentd->>Host: StartSession or ResumeSession
  Host->>Provider: Start provider runtime
  Agentd->>Host: SendTurn
  Host-->>Agentd: Runtime Event v2
  Agentd->>CP: Generation-fenced event append
  Host-->>Agentd: ArtifactCandidate and Result
  Agentd->>CP: Artifact verification and execution terminal state
```

Synara Adapter 继续负责：

- Execution ID/Generation、lease 和 command receipt；
- credential FD 3；
- Workspace 路径和 runtime-output root；
- ArtifactCandidate 的 anchored open、Secret Guard、上传和校验；
- suspend checkpoint、resume snapshot 和 Worker migration；
- 控制面 Session Sequence。

Runtime 不 import Go 模型，也不自行调用 Control Plane。

### 7.2 T3 Embedded Adapter

每个 Provider Instance 可以配置一个上游 Provider，例如 `codex` 或 `claudeAgent`。每个 T3 thread
建立一个独立 Runtime child/session：

```mermaid
sequenceDiagram
  participant UI as T3 Client
  participant Orch as T3 Orchestration
  participant Driver as Synara T3 Driver
  participant Runtime as Cloud Agent Runtime
  participant Provider as Provider CLI or SDK

  UI->>Orch: thread.turn.start
  Orch->>Driver: startSession or sendTurn
  Driver->>Runtime: Describe once per runtime build
  Driver->>Runtime: StartSession with T3 workspace root
  Runtime->>Provider: Start or resume
  Driver->>Runtime: SendTurn
  Runtime-->>Driver: Runtime Event v2 stream
  Driver-->>Orch: T3 ProviderRuntimeEvent
  Orch-->>UI: orchestration.subscribeThread
  Orch->>Orch: T3-owned Git checkpoint and diff
```

适配规则：

- T3 `threadId` 是宿主会话键；Runtime 内部 session ID 不暴露为 T3 路由权威；
- T3 workspace root 由 T3 解析后传入，Runtime 不做 Project 查找；
- T3 保持 Git checkpoint 和 filesystem 权威；
- Provider Host v2.2 handler 只有一个 `sessionInput` 和一个 `activeOperation`，所以第一版必须每个 T3
  thread 一个 Runtime child；不能在一个 child 内无协议依据地 multiplex 多个 thread；
- `GenerateText` 使用独立短生命周期 child 或有界专用池，不复用正在执行 Turn 的 child，也不写入
  thread binding/checkpoint；
- Portable Runtime 的 rollback 边界只允许恢复 Provider conversation，Workspace rollback 仍由 T3
  执行；当前 Provider Host v2.2 虽声明 `RollbackSession`/`ForkSession` 命令，但实现刻意交给
  Control Plane emulation，T3 首版需用 authoritative history 的 stop/restart 重建来补齐，不能把
  当前命令声明误报成已原生支持；
- Driver `Scope` 关闭时，必须先 StopSession，再关闭 stdin/FD，最后 bounded kill；
- 一个 Instance 的 child/process/pubsub 不得泄漏到另一个 Instance。

### 7.3 T3 Environment Lease Adapter（推荐云形态）

这一层不是 Provider Adapter，而是 Synara 的环境供给/生命周期 Adapter。它启动完整 T3 server，
然后交还一个标准 pairing URL：

```mermaid
sequenceDiagram
  participant User as User or Synara UI
  participant Lease as Synara Lease API
  participant Supervisor as Lease Supervisor
  participant T3 as T3 server in sandbox
  participant Broker as Credential Broker
  participant Client as T3 Client

  User->>Lease: Create T3EnvironmentLease(project, source, release)
  Lease->>Supervisor: Allocate sandbox with generation
  Supervisor->>Supervisor: Mount workspace and persistent T3 state
  Supervisor->>Broker: Start generation-fenced local broker
  Supervisor->>T3: Start server with fixed stateDir and isolated admin bootstrap
  T3-->>Supervisor: Ready(environmentId, health, version)
  Supervisor->>T3: Create user pairing link(scopes, subject, proof key)
  Supervisor-->>Lease: Ready(endpoint, pairingUrl, expiresAt)
  Lease-->>User: Standard T3 pairing URL
  Client->>T3: Existing pairing and DPoP-bound exchange
  Client->>T3: Normal RPC and WebSocket traffic
  T3->>Broker: Request short-lived provider credential
  Broker-->>T3: Anonymous FD or single-use grant
```

推荐 sandbox 布局：

```text
/workspace                       # T3 唯一 Project/agent cwd
/var/lib/t3                      # 持久化 stateDir/environment-id/settings/db
/var/lib/cloud-agent             # Runtime binding/cursor state
/run/synara/credential-broker.sock
/run/synara/lease.json           # leaseId + generation；只读、无 secret
```

关键规则：

- T3 创建和回滚自己的 hidden Git refs；Synara 不对同一 Turn 再创建 checkpoint；
- Synara 只在 Deferred D1 对 volume snapshot/suspend-resume 做环境级保护，不把它暴露为 T3 Turn
  checkpoint；
- T3 server、Runtime、Terminal 和文件 API 都只能看到当前 generation 的 mount namespace；
- pairing endpoint 必须 HTTPS/WSS 可达；Synara 可提供 ingress/tunnel，但 T3 Client 仍保存普通
  `BearerConnectionTarget`；
- T3 startup administrative bootstrap 仅供 Supervisor 调用 `createPairingLink`、list/revoke link/session；
  它不返回给用户、不写入 pairing URL，不得复用成普通用户凭证；
- 每个用户 link 使用 `subject=synara:user:<userId>:membership:<membershipVersion>`，只授予
  `orchestration:read orchestration:operate terminal:operate review:write relay:read` 的必要子集，禁止
  `access:read`、`access:write` 和 `relay:write`；
- pairing token 一次性且短期；Stage 7 public beta 的 managed/relay 连接必须绑定客户端 proof key，
  通过 T3 现有 DPoP exchange 获得短期 session；普通 Bearer fallback 只允许受控 `internal-self-hosted`
  策略，不能作为 public beta 默认；
- Synara RBAC、Project membership、membership version、显式 revoke 或 lease generation 变化时，
  Supervisor 必须撤销该 subject/generation 的未消费 pairing link 和全部活动 T3 session，再关闭入口；
- 每个 lease 只能属于一个 Tenant/Project/trust-domain tuple；endpoint、administrative bootstrap、用户
  session 和 broker grant 均不得跨 tuple 复用；
- lease terminate/revoke 时先阻止新连接和新 credential grant，再 drain T3/Runtime，最后卸载 volume；
- 同一 lease suspend/resume 保持 `environmentId`；克隆 lease 则生成新的 environment identity；
- Provider 长期凭证不写入 `providerInstances.environment`、T3 settings 或 sandbox 镜像。

上述流程复用 T3 现有的 scope、subject、proof-key、pairing-link 和 session revoke 能力，见
[`environment-auth.md`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/docs/internals/environment-auth.md) 与
[`EnvironmentAuth.ts`](https://github.com/pingdotgg/t3code/blob/a20923ce463335e89e92f5983d98a180536e8e7d/apps/server/src/auth/EnvironmentAuth.ts)。
实现时需要新增的是 Synara membership/generation 到这些 T3 revoke primitive 的协调层，不是另造一套
长期 access token。

### 7.4 Delegated Control Adapter（Deferred D3，不在主路径）

如果以后确实需要本地 T3 观察 Synara-native Session，不能只加
`CheckpointAuthority = "t3" | "provider"`。当前 T3 的文件、VCS、checkpoint、Terminal、preview 和
source-control command 是一组耦合的环境能力，需要至少建模：

```ts
type WorkspaceExecutionProfile =
  | {
      readonly kind: "co-located";
      readonly filesystem: "local";
      readonly vcs: "local";
      readonly checkpoint: "t3";
      readonly terminal: "local";
      readonly preview: "local";
    }
  | {
      readonly kind: "external-control-only";
      readonly filesystem: "disabled";
      readonly vcs: "disabled";
      readonly checkpoint: "external-readonly";
      readonly terminal: "disabled";
      readonly preview: "disabled";
    };
```

`external-control-only` 必须同时做到：不解析本地 cwd、不创建 placeholder 后再捕获本地 Git、不允许
revert/source-control/Terminal、把远端 diff 标为只读 projection，并显式提示 Workspace 未挂载。
这会横跨 orchestration、checkpoint reactor、VCS、workspace、terminal、preview 和 UI，已经不是最小
Provider 插件。因此第一版不实现；若未来需要，单独做 T3 Remote Workspace RFC 和补丁预算。

## 8. 身份与持久化映射

### 8.1 通用身份

| Portable 概念      | Synara-native                              | T3 embedded                                   | T3 Environment Lease                             |
| ------------------ | ------------------------------------------ | --------------------------------------------- | ------------------------------------------------ |
| `hostInstanceId`   | Worker incarnation / Provider Host process | Provider Instance ID                          | `environmentId + ProviderInstanceId`             |
| `hostThreadId`     | Session ID                                 | Thread ID                                     | Thread ID                                        |
| `runtimeSessionId` | Provider session/cursor                    | Runtime child session ID                      | Runtime child session ID                         |
| `turnId`           | Control Plane Turn ID                      | T3 Turn ID                                    | T3 Turn ID                                       |
| `executionId`      | Execution ID                               | `environmentId + threadId + attempt` 稳定派生 | `leaseId + threadId + attempt` 稳定派生          |
| `generation`       | Control Plane Generation                   | 每次 Runtime session 重建递增                 | Lease generation；Runtime 子 generation 另设字段 |
| `commandId`        | durable control command ID                 | T3 command ID 或确定性派生值                  | T3 command ID 或确定性派生值                     |
| `eventId`          | Worker/Control Plane event ID              | Runtime event ID                              | Runtime event ID                                 |

T3 映射的 `executionId` 不冒充 Synara Execution，只满足协议相关性和 fencing。日志中必须带
`hostKind=t3code`，防止运营数据混淆。

### 8.2 Resume Cursor

T3 的 `resumeCursor` 是 unknown，可保存不含凭证的版本化值：

```json
{
  "kind": "synara-cloud-agent-resume-v1",
  "runtimeSessionId": "...",
  "providerCursor": "...",
  "lastRuntimeEventId": "...",
  "generation": 3
}
```

要求：

- 不放 API key、token、绝对 credential path；
- 反序列化先校验版本和大小；
- generation 不匹配时不能直接复用 child process；
- Provider cursor 失效时按能力声明选择 authoritative-history reconstruction；
- 未声明该能力时返回稳定 `session_resume_expired`，不静默创建空历史。

### 8.3 Host-owned Binding 与 Receipt Store

实施复核后，不再为 T3 Bridge 新增一套私有 SQLite。T3 已有 `ProviderSessionRuntime` 持久化
`resumeCursor`，`ProviderService` 负责停止/重启后的 binding 恢复，Orchestration Engine 已有 command
receipt，`ProviderRuntimeIngestion` 使用稳定 `eventId` 做幂等投影。Bridge 应复用这些宿主权威，而不是
双写另一份 session/receipt 数据库。

Cloud Agent resume cursor 使用有版本的宿主不透明对象：

```json
{
  "schemaVersion": 1,
  "providerResumeCursor": "provider-native-cursor",
  "runtimeGeneration": 4
}
```

T3 启动新 Runtime process 时将 `runtimeGeneration` 加一，再把新值随 Provider cursor 一起交回宿主
持久化；同一 live process 内命令沿用当前 generation，rollback/reconstruction 也必须创建新 generation。
Runtime 消息必须同时匹配 Protocol major/minor、`requestId`、`executionId`、`generation` 和
`commandId` 才能进入 T3 event stream。

若未来宿主没有等价的 binding/receipt 能力，`CloudAgentHostServices` 可以新增 app-neutral state adapter；
公共 Runtime/Provider 包仍不得直接连接 T3 或 Synara 主数据库。事务顺序保持“先验证 → 映射 → 宿主
dispatch 成功 → 宿主 receipt 持久化”，稳定 event ID 由 execution/generation/command/sequence 派生。

### 8.4 用量与费用权威

Environment Lease 与 Agent Turn 的计量必须分账，不复用一个含义不清的 `usage` 数字：

| 数据类别                           | 权威来源                       | 用途与约束                                                      |
| ---------------------------------- | ------------------------------ | --------------------------------------------------------------- |
| Lease vCPU/内存/存储/网络/运行时长 | Synara Lease Metering Ledger   | 平台资源用量与内部成本；按 lease/generation 幂等记账            |
| Provider token/请求/上游费用       | Broker/Provider signed receipt | Provider 报告后入账；保留 provider、模型、价格版本和 receipt ID |
| T3 本地 usage/transcript analytics | T3 Code                        | 用户分析展示；不是 Synara billing 或内部成本权威                |
| Runtime 过程指标                   | Cloud Agent Runtime telemetry  | 性能/诊断；不得直接成为结算记录                                 |

上游 Provider 未返回价格、模型无法映射价格表或 receipt 缺失时，成本状态必须是 `unknown/unreported`，
不能写成 `0`。`T3EnvironmentLease` 不能为了复用现有账单表而伪造 Synara Session、Turn 或 Agent
Execution 费用；需要独立的 lease ledger，再通过 tenant/project/cost-center 做汇总展示。任何修正都使用
append-only adjustment，不覆盖原始 receipt。

## 9. 命令映射

### 9.1 T3 → Cloud Agent Runtime

| T3 Adapter 方法      | Runtime 命令                                    | 说明                                                                 |
| -------------------- | ----------------------------------------------- | -------------------------------------------------------------------- |
| `startSession`       | `Describe` + `StartSession/ResumeSession`       | Describe 结果按 runtime build cache                                  |
| `sendTurn`           | `SendTurn`                                      | 一次只有一个 foreground Turn                                         |
| `interruptTurn`      | `InterruptTurn`                                 | command ID 必须可重放                                                |
| `respondToRequest`   | `ResolveApproval`                               | request ID 原样关联                                                  |
| `respondToUserInput` | `ResolveUserInput`                              | 先验证 answer schema                                                 |
| `stopSession`        | `StopSession`                                   | 幂等；未知 session 成功 no-op                                        |
| `rollbackThread`     | `RollbackSession` 或 authoritative-history 重建 | 当前 v2.2 Host 返回 emulated/unsupported，T3 Adapter 先补齐 fallback |
| `startReview`        | `StartReview`                                   | 能力为 native/emulated 才开放                                        |
| `steerTurn`          | `SteerTurn`                                     | 不支持时明确 capability error                                        |
| `stopAll`            | 对所有 session `StopSession`                    | bounded 并发，最后回收 child                                         |
| `textGeneration.*`   | `GenerateText`（Protocol 2.3 可选）             | 四类 task 独立协商、输入/输出限额与 schema 校验                      |

T3 当前有些能力尚未出现在它的基础 Adapter 接口中，插件只能投影共同子集；不能把 Synara 的扩展
方法强塞进 UI。能力扩展以后按 additive 方式加入。

### 9.2 Polaris delegated 命令（Deferred D3）

| 插件命令                     | Polaris API                                                        |
| ---------------------------- | ------------------------------------------------------------------ |
| Create/Get Session           | `POST /v1/projects/{projectID}/sessions` / `GET /v1/sessions/{id}` |
| Send Turn                    | `POST /v1/sessions/{id}/turns`                                     |
| Event stream                 | `GET /v1/sessions/{id}/events/stream`                              |
| Poll fallback                | `GET /v1/sessions/{id}/events`                                     |
| Interrupt/Steer              | active Turn control routes                                         |
| Approval/User input          | Execution interaction resolve routes                               |
| Compact/Review/Rollback/Fork | 对应 Session routes                                                |
| Artifact                     | Session Artifact metadata + short-lived download grant             |

当前这些只有 source implementation evidence；Registry 发布、外部部署和真实 Provider release
gate 仍是独立边界，见 [`external-sdk-developer-platform.md`](external-sdk-developer-platform.md)。

## 10. 事件映射与顺序

### 10.1 事件原则

1. Runtime Event v2 是公共插件内部的标准事件，不直接持久化 Provider 原始 payload。
2. Adapter 必须逐类型校验 payload，不能只校验 `eventType`。
3. T3 event ID 由 Runtime event ID 确定性派生，重放产生同一个 ID。
4. Synara Session Sequence 由 Control Plane 分配；T3 embedded 不伪造该 sequence。
5. Polaris delegated 必须持久化 `lastSequence`，重复 sequence 丢弃，gap 先 replay，仍有 gap 则
   fail closed。
6. `Result` 不是唯一完成信号；必须确认事件 pump 已处理完该命令之前的全部事件。

### 10.2 主要映射

| Runtime Event v2                   | T3 投影                                                                  |
| ---------------------------------- | ------------------------------------------------------------------------ |
| `session.started`                  | Provider session started/ready                                           |
| `session.state.changed`            | session status                                                           |
| `thread.started`                   | provider thread reference                                                |
| `turn.started`                     | active Turn                                                              |
| `content.delta` + `assistant_text` | assistant delta                                                          |
| `item.started/updated/completed`   | tool/activity lifecycle                                                  |
| `request.opened/resolved`          | approval activity                                                        |
| `user-input.requested/resolved`    | structured input activity                                                |
| `thread.token-usage.updated`       | usage projection                                                         |
| `turn.plan.updated`                | plan projection（宿主支持时）                                            |
| `turn.diff.updated`                | embedded/lease 中仅触发 T3 本地 checkpoint 捕获；不信任远端 diff payload |
| `runtime.warning`                  | bounded warning activity                                                 |
| `runtime.error`                    | typed error + session state                                              |
| `turn.completed/aborted`           | Turn terminal + checkpoint trigger                                       |
| `session.exited`                   | session closed/error                                                     |

未知事件处理：

- 同一 major/minor 允许的未知 additive payload 字段：保留或忽略；
- 未协商的 event type：`protocol_violation`；
- Provider 原始未知消息：Runtime 降级为 bounded `runtime.warning`，不穿透原始 JSON。

### 10.3 Subscribe-before-command

为避免快速 Turn 的首批事件早于监听器建立：

```text
1. 创建并启动 event pump
2. 等待 pumpReady barrier
3. 记录 pending command receipt
4. 发送 SendTurn
5. 消费事件
6. 收到 terminal Result
7. drain 该 command 的事件
8. 返回宿主
```

不能先调用 `SendTurn`，再异步 fork stream consumer。

## 11. Workspace、Checkpoint 与 Artifact

### 11.1 单一 Workspace 权威

每个 Runtime session 必须在创建时固定：

```ts
type WorkspaceBinding = {
  readonly authority: "host" | "external-readonly";
  readonly root: string | null;
  readonly generation: number;
  readonly readOnly: boolean;
};
```

- `synara-native`：从 Runtime 看 `authority=host`，agentd 负责 Workspace checkpoint。
- `t3-embedded`：从 Runtime 看 `authority=host`，T3 负责 Git checkpoint。
- `t3-environment-lease`：从 Runtime 看仍是 `authority=host`；host 是 sandbox 内的 T3 server，root
  来自当前 lease generation 的 mount。
- `delegated-control-only`：`authority=external-readonly` 且 `root=null`，本地文件能力整体关闭。

一个 session 生命周期内不允许从 host 切到 provider authority；切换必须创建新 generation。

### 11.2 Checkpoint 分工

Portable contract 中的 `RollbackSession`、`ForkSession` 只允许处理 Provider 会话/历史重建；当前
Provider Host v2.2 将二者刻意留给 Control Plane emulation，并未提供原生实现。宿主始终负责物理
Workspace checkpoint：

- Synara：Checkpoint/Artifact/Resume Snapshot 契约；
- T3 embedded：hidden Git refs、diff 和 revert；
- T3 Environment Lease：仍由 T3 创建 hidden Git refs、diff 和 revert；Synara 只做环境级 volume
  snapshot/suspend，不参与 Turn checkpoint；
- delegated control-only：只能显示外部只读 diff，不允许本地 revert。

这条分工必须进入 testkit，避免后续某个 Provider 实现偷偷执行 `git reset`。

### 11.3 Artifact

Runtime 只能发 `ArtifactCandidate`：

```ts
type ArtifactCandidate = {
  readonly sourceRoot: "workspace" | "runtime-output";
  readonly relativePath: string;
  readonly kind: "diff" | "generated-file" | "terminal-log" | "provider-output";
  readonly contentType: string;
  readonly reportedSize?: number;
  readonly sha256?: string;
};
```

宿主必须：

- canonicalize root；
- 禁止 absolute path、`..` 和 symlink escape；
- anchored/no-follow open；
- 限制大小与文件类型；
- 执行 Secret Guard；
- 上传、校验 hash、再生成 durable reference。

T3 embedded/Environment Lease 可选择内联小 diff；大文件放入 driver-owned artifact store 或交给 T3
后续 Artifact SPI。delegated control-only 只消费 Synara 已 Ready 的 Artifact，不相信 Provider 自报
URL。

## 12. 配置模型

### 12.1 T3 embedded 示例

```json
{
  "providerInstances": {
    "synara_codex": {
      "driver": "synaraCloudAgent",
      "displayName": "Synara Runtime · Codex",
      "enabled": true,
      "config": {
        "providerKind": "codex",
        "runtimeBinaryPath": "/opt/synara/bin/cloud-agent-runtime",
        "runtimeBinarySha256": "sha256:<64-hex>",
        "credentialProfile": ""
      }
    }
  }
}
```

首批 embedded slice 使用 Provider 本机已有认证；`credentialProfile` 非空时 fail closed，并提示等待
Phase 4 Lease credential broker，不能假装已经挂载凭证。配置 `runtimeBinarySha256` 时
`runtimeBinaryPath` 必须是绝对路径，T3 在启动/广告 ready 前读取**单个可执行文件**并校验 SHA-256；它
不是覆盖 tarball、manifest、schema、SBOM 和 provenance 的 Distribution release digest。未配置该值只
适合本地验证；managed release 仍必须另行校验受信 release manifest。

### 12.2 T3 Environment Lease 创建示例

```json
{
  "kind": "t3-code",
  "projectId": "prj_...",
  "source": {
    "kind": "git",
    "repository": "org/repo",
    "revision": "main"
  },
  "release": {
    "t3CodeCommit": "a6c9b41f902fba2a4137806c09e829935e91baac",
    "cloudAgentDistributionReleaseDigest": "sha256:..."
  },
  "providerBindings": [
    {
      "providerInstanceId": "synara_codex",
      "credentialProfileId": "pcp_..."
    }
  ],
  "resources": {
    "profile": "standard",
    "region": "auto"
  },
  "lifecycle": {
    "suspendPolicy": "disabled",
    "maximumLeaseSeconds": 28800
  }
}
```

Lease ready 后，Synara 返回：

```json
{
  "leaseId": "t3l_...",
  "generation": 1,
  "environmentId": "...",
  "state": "ready",
  "pairingUrl": "https://app.t3.codes/pair?host=https://...#token=...",
  "expiresAt": "..."
}
```

约束：

- `pairingUrl` 是短期 bootstrap secret，不进入日志、审计详情或 query parameter；token 必须位于 fragment；
- pairing token 必须一次性消费且短期失效，只能提交到 T3 现有 `/oauth/token` exchange，不能直接访问
  HTTP、RPC 或 WebSocket；exchange 失败或成功消费后都不得重新使用；
- Stage 7 public beta 的 managed/direct-ingress 与 relay 连接必须使用客户端 proof key，沿用 T3 现有
  DPoP exchange 获得短期 session；不得把普通 Bearer 作为自动降级或默认路径；
- `t3CodeCommit` 与 `cloudAgentDistributionReleaseDigest` 必须来自允许的 release manifest，不能由普通
  用户指向任意镜像；Lease 状态、审计和启动证明统一使用该字段名；
- Phase 4 MVP 的 `suspendPolicy` 固定为 `disabled`；只有 Deferred D1 一致性门禁通过后才能启用 idle
  suspend；
- Lease API 不接受 Provider 长期凭证，只按 Provider Instance 接受 `credentialProfileId` 引用；该 ID
  不是 secret，且必须经过 Tenant/Project RBAC 与 Provider kind 绑定校验；
- Lease controller 根据 `providerBindings` 生成受管 T3 Provider Instance 配置，沿用 12.1 的 shape，但把
  `credentialProfile` 写成对应的 `pcp_...`；普通用户不能通过编辑配置改变已绑定的 profile；
- 只有 Supervisor 注入了可验证的 managed-lease context、lease ID 和当前 generation 时，T3 bridge 才把
  非空 `credentialProfile` 映射为 HostServices 的一次性 credential source，并由 broker 通过匿名 FD/单次
  grant 交付秘密；`{ "kind": "synara-broker", ... }` 不是 `providerInstances.config` 的公共字段；
- `t3-embedded` 没有上述受管上下文，继续对任何非空 `credentialProfile` fail closed；
- 未安装 bridge 时配置必须 round-trip 并显示 unavailable，不能导致 T3 server 启动失败。

### 12.3 Synara-native 配置

第一阶段继续使用现有 Worker release、Provider Host enablement、Provider credential Grant 和受控 proxy
变量。Runtime 包不新增另一套 `.env`。实验 Provider allowlist、proxy、package mirror 等都由
agentd 生成受控环境。

## 13. 凭证与安全边界

### 13.1 统一的客户端认证状态机

T3 Environment Lease 不新增 Synara 专用 access token，也不按 direct ingress、relay 分裂认证协议。
所有非本地连接统一为同一个状态机：

```text
short-lived single-use pairing token
  + client proof key
        │
        ▼
T3 /oauth/token DPoP exchange
        │
        ▼
short-lived, scope-limited, proof-bound environment session
        │
        ▼
single-purpose WebSocket ticket / authenticated HTTP
```

统一的是 **bootstrap、exchange、session、revoke 的语义**，不是把所有 transport 合并为一种连接目标：

- pairing token 只建立初始信任，成功兑换即原子消费，并受短 TTL、lease、generation、subject 和
  scope 上限约束；
- public beta 的 managed direct ingress 与 relay 都必须走 T3 已有 DPoP exchange；access token 与
  客户端 JWK thumbprint 绑定，session 使用短 TTL，WebSocket 继续只暴露短期单用途 ticket；
- direct 与 relay 只负责 endpoint/transport 选择，不改变环境 session 的 issuer、audience、scope、
  proof-key 和 revoke 语义；relay credential 与 environment session 仍是两个 trust boundary；
- 普通 Bearer exchange 仅能由显式 `internal-self-hosted` access profile 开启，用于受控、可审计且风险
  已被部署方接受的环境；客户端不得在 DPoP 失败、密钥不可用或服务端 challenge 后自动 fallback；
- public beta access profile 的服务端 descriptor 不广告 `bearer-access-token`，客户端若无法生成或
  安全保存 proof key，应 fail closed 并给出不可连接状态；
- browser cookie 可继续作为同一 scoped session 模型的浏览器 transport adapter，但不能被用来绕过
  public beta managed/relay 的 proof-key 要求；是否允许它由 access profile 明确声明，而非客户端猜测。

建议只引入一个部署策略枚举，而不是增加新协议或新连接类型：

```ts
type LeaseAccessProfile = "public-beta-proof-bound" | "internal-self-hosted";
```

`public-beta-proof-bound` 是所有 Synara managed Lease 的固定默认值；`internal-self-hosted` 必须由
部署级策略显式启用，不能由普通 Tenant、Project、pairing link 或客户端请求切换。Synara 负责把 profile、
lease/generation/subject/scope 上限注入 Lease Supervisor；T3 继续负责现有 descriptor、token exchange、
session store、DPoP 校验和 WebSocket ticket。这样 Synara 只增加策略与 claims/fencing 接缝，T3 只需让
客户端按 descriptor 对 direct/relay 共用现有 `authorizeDpop` 路径。

### 13.2 Embedded 凭证

- 宿主读取凭证；Runtime 通过匿名 FD 或一次性 broker 获取；
- secret 不进入 argv、普通环境变量、manifest、SQLite 和 Runtime Event；
- Runtime 子进程环境从 allowlist 构建，不继承整个 T3/Synara server 环境；
- 收集的 secret 只用于当前 run 的流式 redaction，完成后释放；
- `Describe` 必须能在无凭证状态运行。

### 13.3 Environment Lease Credential Broker

正式多用户产品不能把长期 Service Account 或 Provider key 写进 Cloud Workspace。Lease Supervisor
应在 sandbox 外或受控 sidecar 中提供本地 broker，T3 bridge 只持有当前 lease/generation 的短时
工作负载身份：

```text
claims = tenant + organization + project + lease + generation + provider + operations + expiry + nonce
```

Broker 规则：

- 只监听 Unix socket/vsock，不暴露公网；
- 校验调用进程、lease、generation、provider kind 和允许操作；
- 返回匿名 FD、pipe 或单次 exec grant，不返回可长期复制的明文 token；
- lease revoke、generation 变化和用户撤权立即拒绝新 grant；
- 记录 grant metadata 和结果，不记录 secret；
- T3 server settings 只保存 credential profile reference，不保存 secret value。

### 13.4 Delegated Control 凭证（Deferred D3）

实验性的 Polaris control-only Adapter 可以使用最小 scope 的短时 `Host Integration Grant`。现有
`syna_sa_` Service Account token 只适合受控内部验证，不应成为安装文档默认路径，也不得进入
Web/Mobile 客户端、T3 provider config、resume cursor 或日志。

### 13.5 插件代码信任

Node 插件与 T3/Synara server 同进程时拥有宿主进程权限，manifest 不是安全沙箱。因此：

- 默认使用 out-of-process stdio Runtime；
- 插件包必须固定 version/digest；
- 发布生成 provenance、SBOM 和最小文件 allowlist；
- 不自动加载任意目录中的第三方包；
- 生产 Worker 继续由 release manifest 固定 Runtime 和 Provider CLI 版本；
- 高风险 Provider 仍必须在外层 gVisor/Cocoon 等宿主隔离中运行。

## 14. 失败、恢复与幂等

### 14.1 稳定错误分类

沿用 Provider Host v2 的稳定错误：

```text
provider_not_installed
provider_version_incompatible
capability_unsupported
credential_missing
credential_invalid
authentication_required
session_resume_invalid
session_resume_expired
provider_rate_limited
provider_unavailable
workspace_invalid
protocol_violation
cancelled
interrupted
internal_error
```

每个错误保留：`retryable`、`requiresNewExecution`、`requiresUserAction`、
`canReconstructFromHistory`、`canMoveWorker`。T3 可以忽略它不理解的字段，但不能反转含义。

### 14.2 Runtime crash

```text
1. Driver 检测 child exit
2. 停止接收新命令
3. 当前 Turn 标记 recoverable/non-recoverable
4. 增加 generation
5. 重新 spawn + Describe
6. 优先 native cursor resume
7. cursor 无效且支持 authoritative-history 时重建
8. replay 未确认 command
9. event receipt 去重
10. 恢复 ready 或返回稳定错误
```

不能在无法证明历史恢复时创建空 Session 并继续。

### 14.3 SSE 断线

Polaris delegated 使用 durable sequence：

- 从已提交 `lastSequence` 重连；
- 429/SSE pool 饱和时切换 bounded polling；
- duplicate sequence 忽略；
- sequence gap 先补页；
- gap 无法补齐时停止该 session 的新 command；
- 消费者取消时必须 abort HTTP body，释放 connection lease。

### 14.4 Stop 语义

`stopSession` 只释放 live Provider Runtime，不默认删除 Workspace、归档 Project 或销毁 Synara
Execution Target。宿主分别决定：

- T3：关闭当前 live session，保留 thread/checkpoint；
- Synara：根据 lifecycle policy suspend/release execution；
- 显式 archive/delete 必须是另一条用户可见命令。

## 15. 版本与兼容策略

### 15.1 七个独立版本轴

| 版本轴                         | 当前/初始值                            | 作用                                                    |
| ------------------------------ | -------------------------------------- | ------------------------------------------------------- |
| Provider Plugin ABI            | `1`                                    | Runtime 与通用 Provider 包的 JavaScript/stdio 接口      |
| Provider Host Protocol         | `2.2` 基线；当前 additive `2.3`        | host/runtime command wire 与可选 `GenerateText`         |
| Runtime Event                  | `2`                                    | canonical event vocabulary                              |
| Runtime package semver         | `0.x` 起                               | 通用装载/会话内核实现                                   |
| Provider package semver        | Codex/Claude 各自 `0.x` 起             | Provider 实现独立发布、回滚和安全修复                   |
| Upstream SDK/CLI compatibility | 每个 Provider 的 probe + version range | 实际 Codex/Claude 可执行文件或 SDK 的兼容范围           |
| Distribution release digest    | `sha256:<immutable>`                   | 固定上述包、T3 commit、bin、schema、SBOM 的完整发布单元 |

不能因 package minor 升级就默认为 wire 兼容，也不能因 Protocol minor 相同就忽略 Event/Provider ABI。
Distribution digest 是受控环境的部署身份；npm semver 不能替代 digest。

### 15.2 兼容规则

- Protocol major 不同：拒绝启动；
- Provider Plugin ABI major 不同：拒绝装载该 Provider；
- Runtime Event range 不重叠：拒绝启动；
- Protocol minor：通过 Describe 协商，以 capability 为准；
- 新能力默认 `unsupported`，不得因字段缺失视为 `native`；
- 新可选 payload 字段 additive；
- 删除命令、改变字段含义、扩大 secret 暴露均需 major；
- Provider 的 `Describe`/version probe 不满足 upstream SDK/CLI 兼容范围：Provider unavailable，不能静默
  降级到宿主内置实现；
- 受控 Environment Lease 必须使用 allowlist 中的 Distribution digest；
- 未知 Provider kind 可以持久化和显示 unavailable，不让配置解析崩溃。

### 15.3 双宿主兼容矩阵门禁

每次生成 Distribution candidate 至少测试以下目标；具体一次性命令、版本和测试数量只记录在附录 A，
不能写入长期兼容承诺：

| 目标                         | 固定身份                                  | M1 当前判定                                  |
| ---------------------------- | ----------------------------------------- | -------------------------------------------- |
| Synara Provider Host wrapper | Synara commit + wrapper version           | 本地定向验证；镜像/真实 Provider 待补        |
| Provider Host Protocol       | Protocol 2.2 reader + additive 2.3        | handler 接受 major 2；2.2 golden gate 未完成 |
| T3 Code bridge               | T3 commit + bridge patch digest           | 本地定向验证；restart/drain E2E 待补         |
| Distribution candidate       | manifest + tarball/bin/schema/SBOM digest | 尚未生成不可变 candidate                     |
| 外部消费环境                 | Node/Bun/pnpm 与宿主 commit               | 尚未完成临时项目 install/import/bin smoke    |

### 15.4 两个宿主独立升级策略

两个项目的精确依赖与 Provider 工具版本是时点证据，统一记录在附录 A.4；正文只固定兼容政策。T3
内置 Provider driver 的 SDK/CLI 与 `synaraCloudAgent` out-of-process Distribution 是两条版本轴，
不会跨同一个 Plugin ABI 混用对象或类型。

这些宿主版本可以独立升级，因为公共边界不传递 Effect 对象，且通过普通 JS/stdio、JSON Schema 和版本协商
隔离。独立版本不应演变成维护两份 Codex/Claude 实现；目标态中两个 Provider 包分别是唯一发布源。当前
尚未达到目标态：两个包仍代理 Runtime 的 legacy facade，真实 Codex/Claude 实现和 Claude SDK 依赖仍在
Runtime 中，因此独立 Provider 发布/回滚尚未得到证明。

受控 Synara-native 与 `T3EnvironmentLease` 默认使用同一 Distribution release；若本地/自管 T3 暂时固定
另一个 Provider/Runtime minor，必须满足兼容矩阵、记录精确 semver 和 digest，并只在承诺的一个 minor
兼容窗口内并存。任何宿主升级 Effect、SDK/CLI 或 bridge 后，都要重新运行对应双宿主 conformance；不能
只凭 lockfile 安装成功判断兼容。

## 16. 性能与资源约束

插件化不能明显拖慢 fast cloud agent 路径。建议预算：

| 指标                           | 预算/门禁                                                |
| ------------------------------ | -------------------------------------------------------- |
| Adapter 本地 dispatch 附加 P95 | ≤ 50 ms                                                  |
| Event 收到到宿主 dispatch P95  | ≤ 100 ms                                                 |
| stdio/T3 transport 消息队列    | 默认 2048，满时背压                                      |
| JS Plugin session 事件队列     | 默认 2048；优先保 terminal，淘汰最旧 non-terminal 并告警 |
| Describe cache                 | 按 executable digest + config revision                   |
| NDJSON 单 command              | 沿用 2 MiB 上限                                          |
| NDJSON 单 message              | 沿用 1 MiB 上限                                          |
| durable Session Event payload  | 沿用 64 KiB；大内容 Artifact 化                          |
| Runtime Stop/quiesce deadline  | 默认 5 s                                                 |
| T3 `StopSession` 等待          | 默认 2 s；之后仍关闭 child scope                         |
| child force-kill deadline      | 默认 5 s，定向回收已跟踪 PID                             |
| T3 Phase 3 child 模型          | 每个 active thread 一个；尚无 idle pool                  |
| T3 后续 idle child 预算        | 每 Instance 初始 4、TTL 15 min；soak 后才能启用          |

这些是插件自身开销，不代替 Synara dispatch→provider-ready P95、warm hit 和 snapshot resume SLO。

Child reaper 不能只看 foreground Turn。存在后台 subagent、workflow、waiting approval/user-input、未 drain
event 或 text-generation operation 时都视为 live；只有 Runtime 明确 quiesced、binding/receipt/cursor 已持久化
后才能 LRU 回收。T3 基线本身仍在修复 background subagent reaper 语义，因此该 surface 必须列为 P1 漂移。

## 17. 代码迁移与实施阶段

阶段编号描述工程依赖，不代表已经完成；当前只是 M1 source implementation in progress。发布工程是每个
里程碑自己的退出门禁，上游化和 suspend/resume 不在主路径中。

| 工程阶段    | 所属里程碑/轨道    | 当前判定 | 进入下一步前仍缺少的决定性证据                                      |
| ----------- | ------------------ | -------- | ------------------------------------------------------------------- |
| Phase 0     | M0                 | 未完成   | 真实 Codex/Claude happy/failure characterization                    |
| Phase 1     | M1                 | 部分完成 | schema → TS/runtime decoder 单一来源与完整 golden compatibility     |
| Phase 2     | M1                 | 部分完成 | Provider 物理拆包、Distribution registry bin、Synara 镜像兼容       |
| Phase 3     | M1                 | 部分完成 | 双宿主进程级 suite、event drain barrier、restart/rollback E2E       |
| Phase 4     | M2                 | 未开始   | M1 exit gate 与不可变内部 Distribution candidate                    |
| Release M1  | M1 gate            | 未关闭   | exact semver、schema、digest/provenance/SBOM、外部 install/rollback |
| Release M2  | M2 gate            | 未开始   | Lease 制品 manifest、外部环境 install/rollback、固定 commit matrix  |
| Deferred D1 | Suspend/Resume     | 延后     | quiesce、原子快照、new-generation resume                            |
| Deferred D2 | Generic/upstream   | 延后     | composition seam、descriptor、generic UX；不阻塞 M1/M2              |
| Deferred D3 | Polaris delegated  | 延后     | control-only 产品需求与完整 capability 降级；不阻塞 M1/M2           |
| Deferred D4 | Provider ecosystem | 延后     | 其他 Provider、动态目录/市场和公共 SDK；不阻塞 M1/M2                |

### Phase 0 / M0：基线和 characterization（不改行为）

交付：

- 固定 Synara、T3 Code commit；
- 在基线清单记录 T3 clone path、origin、commit、dirty state 和复核文件；
- 保存 Provider Host v2 golden frames；
- 列出当前 Provider capability matrix；
- 给现有 `apps/provider-host` 补充黑盒 CLI 测试；
- 记录 Codex/Claude 两条真实 happy path 和失败路径。

完成条件：重构前后可以用同一组 frames 和行为测试比较。

### Phase 1 / M1：抽取 Protocol 包

建议改动：

```text
packages/cloud-agent-protocol/
  package.json
  src/index.ts
  src/protocol.ts
  src/runtime-event.ts
  src/capabilities.ts
  src/errors.ts
  schemas/*.json
  fixtures/*.jsonl
```

Synara 兼容改动：

- `packages/contracts/src/providerHost.ts` 改为 re-export/host projection；
- `packages/contracts/src/providerRuntime.ts` 保留 UI/宿主扩展，公共事件从新包导入；
- capability catalog 只保留一个可编辑来源；
- Protocol v2.2 既有命令 golden frame 保持兼容；v2.3 只增加可选 `GenerateText` 与 descriptor 字段。

完成条件：无迁移、无 API 路由变化、无破坏性 Worker wire 变化；Protocol v2.2 reader/golden 兼容门禁通过。

### Phase 2 / M1：抽取 Runtime 与 Provider 包

按实现职责从 `apps/provider-host` 拆出：

- `cloud-agent-provider-api`：Provider Plugin ABI、descriptor、version probe 和配置 schema；
- `cloud-agent-runtime`：protocol loop、显式 Provider registry、event validation、interaction request ID、
  diff/terminal/generated-file candidate、host service/redaction 和 resume orchestration；
- `cloud-agent-provider-codex`：Codex App Server lifecycle、event normalization、cursor 和兼容探针；
- `cloud-agent-provider-claude`：Claude SDK lifecycle、event normalization、cursor 和兼容探针。
- `cloud-agent-testkit`：先建立 protocol transcript、descriptor 和 process conformance foundation；
- `cloud-agent-distribution`：先建立显式 registry、manifest、stdio/bin 与内部 candidate 组装路径。

`apps/provider-host` 只保留 CLI 兼容壳和 Synara build metadata。

完成条件：现有 `agentd` focused test、Provider Host test、Worker image build 全绿；生成二进制的
Describe、命令和事件与旧版兼容。

### Phase 3 / M1：T3 Embedded Bridge

实现：

- T3 仓内 driver/config schema 和显式 registration；
- 静态 `providerDriverMeta`、设置页添加/编辑入口和 unavailable 回退；
- per-thread Runtime process manager；
- T3 ↔ Runtime command mapper；
- Runtime Event v2 → T3 event mapper；
- 复用 T3 `ProviderSessionRuntime`、command receipt 与 Runtime ingestion，resume cursor 携带 Runtime
  generation，不新增第二套 Bridge 数据库；
- process scope/stop/recovery；
- snapshot/status/model projection；
- Protocol 2.3 可选 `GenerateText` 与 branch/title/commit/PR text-generation bridge；
- Synara wrapper 与 T3 bridge 共同消费 testkit 的进程级 suite，并通过 Distribution stdio 执行；
- Codex 和 Claude 两个实例。

T3 核心改动保持显式、薄且 upstream-friendly。第一阶段复用现有设置页并加入最小静态 metadata/schema，
让用户可以正常添加和修改 Provider Instance；手写内部配置仅作为 fixture/恢复手段。随后再用
server-advertised descriptor 替换静态条目。

完成条件：同一 T3 workspace 中完成 Start、Turn、approval、interrupt、diff、checkpoint、rollback、
server restart resume，并对 branch/title/commit/PR 四类 text generation 逐类执行；其他内置 Provider
行为不变。T3 focused tests 固定在明确 commit 上通过，且公共 Synara 包不 import `@t3tools/*`。

### Phase 4 / M2：T3 Environment Lease MVP

实现：

- `T3EnvironmentLease` API、RBAC、状态机和 idempotency；
- 固定 T3 commit + Distribution digest 的 release manifest；
- workspace volume、T3 state volume 和 Runtime state volume；
- generation-fenced Lease Supervisor 与 credential broker；
- T3 health/readiness、标准 pairing URL、HTTPS/WSS ingress；
- `public-beta-proof-bound` access profile、一次性 pairing 消费和 T3 现有 DPoP exchange；
- terminate/revoke、审计和独立 Lease Metering Ledger；
- 同一 lease 生命周期保持 `environmentId`，克隆 lease 生成新 identity；
- T3 server 内运行 Phase 3 bridge，继续由 T3 负责 Turn checkpoint。

完成条件：T3 看到的文件、Terminal、diff、source control 与 Provider 修改的是同一 Workspace；
Web/Desktop/Mobile 可用现有 pairing 流程和 managed DPoP session 进入；direct ingress 与 relay 共用
proof-bound environment session 语义且不会自动降级为 Bearer；过期 generation 无法连接或获取凭证；没有长期
Service Account/Provider key 进入 sandbox；Synara 不创建第二套 Turn 状态；MVP 生命周期只承诺
`create → ready → terminate`，不承诺 suspend/resume。

Lease Credential Broker/Grant 作为 Stage 7 public beta 扩展发布前，必须通过：Tenant/Project RBAC、
per-user subject 与最小 scope、managed DPoP、link/session/generation revoke、secret redaction、broker
fencing、独立计量 ledger 和审计负向测试。任一门禁缺失时最多只能标记 internal beta。

### Release Gate（每个里程碑独立执行）

- packed exact semver 与外部 install/import/bin smoke；
- immutable manifest/digest 与 schema exposure；
- provenance/SBOM/secret scan；
- 兼容矩阵和升级策略；
- 独立示例仓库/fixture；
- 插件禁用/回滚路径。

可安装制品、digest/provenance/SBOM 和兼容矩阵是 M1/M2 各自形成 RC 的工程 Gate。公开 npm/Registry 的
时点、用户范围和支持等级由独立 G-EXPOSURE 控制；只有选定公开 npm channel 时才要求 OIDC trusted
publishing。M1 可独立生成和批准 Distribution candidate，不必等待 M2。最小 composition seam/upstream PR
归 Deferred D2。稳定公共 Provider SDK 归 Deferred D4；二者都不阻塞薄 fork、M1 或 M2。

### Deferred D1：一致性 Suspend/Resume

该阶段不与 Phase 4 MVP 同时承诺。必须新增并验证：

1. `ready → quiescing` 先关闭新 Turn、terminal mutation、background job 和新连接 admission；
2. 等待 active Turn、subagent/workflow、approval/user-input、terminal、text generation 与 event queue
   全部进入可证明的 quiescent 状态；超时则失败并恢复 admission；
3. 停止 T3 server/Runtime，确认 SQLite 无 writer；
4. 进入 `snapshotting`，一致性保存 T3 `state.sqlite`、WAL、SHM、Workspace volume、Runtime binding/
   receipt/cursor 与 lease generation；
5. 快照全部 durable 后才进入 `suspended`；任一步失败都回滚临时快照、恢复原 generation 并回到
   `ready`，不得留下半挂起环境；
6. resume 使用新 generation fencing，恢复卷后启动 T3/Runtime，完成 health/version/identity 校验再
   重新开放 admission；旧 endpoint、session 和 broker grant 始终保持 revoked。

T3 现有 server update 流程是在停止旧 server 后才快照 SQLite/WAL/SHM；Lease 实现必须维持同等级别的
静止边界，不能只依据“当前没有前台 Turn”直接快照。

完成条件：故障注入覆盖 quiesce timeout、待审批、活动 Terminal、event 未 drain、停止中断、三类 SQLite
文件不一致、Workspace 快照失败和 resume health 失败；同一 lease 保持 `environmentId`，但所有旧
generation 能力均被 fencing。

### Deferred D2：Generic Driver UX 与上游 seam

实现：

- 参数化 Provider Registry Hydration 的小型 upstream PR；
- server-advertised `ProviderDriverDescriptor` RFC；
- generic config form、unknown driver 只读回退和 capability-driven UI；
- T3 commit compatibility matrix 和 bridge patch drift gate；
- 安装、升级、禁用和回滚操作文档。

完成条件：前端不需要为每个外部 driver 硬编码完整表单；上游未接受 seam 时，薄 fork 仍能由
自动 drift report 维护，且不影响 Phase 3 已有的受支持设置入口。

### Deferred D3：Polaris Delegated Control

只有存在明确的“本地 T3 只观察/控制 Synara-native Session”需求时才启动。先完成
`WorkspaceExecutionProfile` RFC 和跨 filesystem/VCS/checkpoint/terminal/preview 的禁用测试，再实现
`cloud-agent-backend-polaris`、SSE receipt 和 capability 降级。它不是 Environment Lease 的前置条件。

### Deferred D4：Provider 生态扩展

Codex/Claude 之外的 Provider、动态目录/市场、第三方签名策略和公共 Provider SDK 只有在 M1/M2 主路径
稳定后才单独立项；它们不得改变现有 owner、放宽编译期 allowlist，或成为任何当前 Gate 的替代证据。

## 18. 验证矩阵

本节严格按 Gate 拆分测试维度；一个小节只关闭标题中的 Gate，不会把其他里程碑条件反向带入。Gate 的
owner、状态和退出证据以第 19 节的唯一表格为准。

### 18.1 G-BASELINE：重构前 characterization

- 固定 Synara/T3 commit、Provider 工具版本和 capability matrix；
- 保存 Provider Host Protocol v2.2 golden frames；
- 对真实 Codex/Claude 分别记录 happy path、认证失败、限流/不可用和 resume failure；
- 重构后使用同一输入和断言复跑，不用新实现结果倒推旧基线。

### 18.2 G-SCHEMA：协议与生成一致性

- major/minor/event range 不兼容 fail closed；
- Protocol v2.2 handler golden 与 additive v2.3 reader 兼容；
- schema、TypeScript vocabulary 和 runtime decoder discriminator/required fields 一致；
- command/message size 按 UTF-8 实际字节限制。

### 18.3 G-REGISTRY：Distribution 启动与 allowlist

- `Describe` 无凭证成功；
- Distribution stdio 从显式默认 Plugin registry 启动，不进入 legacy handler；
- manifest、实际 registry、Describe 与禁用行为使用同一 allowlist；
- stdout 只有协议帧，诊断只进 stderr；未知/禁用 Provider fail closed。

### 18.4 G-PKG：Provider 物理拆包

Codex/Claude 两包分别覆盖：

- install/import/version probe；
- 只拉入自己的 upstream CLI/SDK 依赖；
- 可独立发布、升级、回滚和安全修复；
- Provider 实现不再通过 Runtime legacy facade 间接执行。

### 18.5 G-CONFORMANCE：双宿主进程级公共行为

- command duplicate 返回同一 terminal result，terminal 后输出被拒绝，event/command 相关 ID 一致；
- bounded queue/backpressure、取消、late frame、process crash 和 resume；
- Start/Resume/Send/Interrupt、approval/user-input；
- model switch、plan、review、compact、rollback、fork 按能力矩阵验证；
- CLI/SDK crash、credential invalid/expired、native cursor invalid → history reconstruction；
- diff、terminal log、generated file ArtifactCandidate；
- Provider raw payload/secret redaction、Artifact path escape 和 symlink 负向测试；
- Synara wrapper 与 T3 bridge 共同消费同一 testkit process suite。

### 18.6 G-T3-DRAIN：T3 终态与重启

- terminal/event pump 有显式 ACK/drain barrier；
- assistant buffered/streaming 全部投影后才允许 `turn.completed`；
- T3 server/Runtime restart 后恢复 durable history，不重复消息、不丢终态；
- browser reconnect 不改变 command receipt、event ID 或 generation；
- interrupt/stop 与 fatal transport 并发时全部 pending 都得到稳定终态。

### 18.7 G-E2E：M1 双宿主验收

- Web、Desktop、Mobile 共同 contract，以及本地、direct remote、relay/tunnel connection；
- 多 Provider Instance、driver config 热重载、scope teardown 和 unavailable config round-trip；
- Bridge 构造真实 snapshot/adapter/text-generation service，Registry Hydration 与构建 composition 一致；
- CheckpointReactor 不丢 turn；Runtime `turn.diff.updated` 只触发本地 checkpoint，不成为远端权威 diff；
- T3 embedded + Codex 完成编辑文件、diff、rollback；
- T3 embedded + Claude 完成 approval、user input、interrupt；
- 插件 N→N+1 升级后恢复 Session，回滚后未知 capability/config 保留并显示 unavailable；
- T3 内置五个 Provider 回归；
- Synara local/SSH/Docker/Kubernetes、warm prestart、Worker replacement/Generation fencing、gVisor/Cocoon、
  credential FD/Grant revoke、Artifact Secret Guard、Control Plane restart/event replay 全回归；
- Synara-native 既有 suspend/resume/migration 回归；现有 Provider Host v2 行为和 capability projection
  与 manifest 一致。

### 18.8 G-LEASE：M2 Environment Lease

- `create → ready → terminate`，T3 server/Runtime/Terminal/文件 API 使用同一 Workspace；
- 一次性 pairing → DPoP exchange → 短期 session → WebSocket ticket，覆盖 direct ingress 与 relay；
- public beta proof-key 强制、Bearer downgrade 拒绝和显式 `internal-self-hosted` 策略；
- pairing/client credential/ingress/broker grant 按 tenant/project/subject/lease/generation revoke；
- T3 state/workspace volume、`environmentId` 和 generation fencing 规则；
- credential broker 的进程/lease/provider scope，且审计与计量不记录 secret/pairing token；
- Synara 不创建第二套 Thread/Turn/Checkpoint 权威。

### 18.9 Deferred 验证（不计入当前 Gate closure）

1. Deferred D1 Lease suspend/resume：相同 `environmentId`、连接恢复、旧 generation 被 fencing；
2. Deferred D3 control-only：所有 Workspace/VCS/Terminal/preview 操作明确 disabled，不触及本地同名目录。

这些场景只有对应 Deferred 轨道获批启动后才成为其自身退出条件，不阻塞 M1/M2。

## 19. 验收标准

下表是 M1/M2 的**唯一权威完成口径**。`open` 表示已有部分源码或本地验证证据但退出条件尚未全部满足；
`not started` 表示对应产品面尚未实施。focused test、build、pack dry-run 或一次握手都不能单独改变状态。

| Gate          | Owner                       | 阻塞里程碑 | 当前状态    | 必须同时满足的退出证据                                                                                                                                                                     |
| ------------- | --------------------------- | ---------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| G-BASELINE    | Synara + T3                 | M0、M1     | open        | 固定 commit 上保存 v2.2 golden frames，并完成真实 Codex/Claude happy/failure characterization，使重构前后可用同一行为集比较                                                                |
| G-ARCH        | Synara + T3                 | M1         | open        | 公共 ABI/依赖保持 app-neutral；Runtime、Host Adapter 与 Workspace/Checkpoint authority 单一；Provider Host v2 无破坏；Instance 状态隔离                                                    |
| G-PKG         | Synara Runtime/Provider     | M1         | open        | Codex/Claude 实现、probe 与 upstream 依赖物理迁入各自 Provider 包；两包可独立安装、发布和回滚，且不会互相拉入另一 Provider 的 SDK                                                          |
| G-REGISTRY    | Synara Runtime/Distribution | M1         | open        | Distribution stdio 从显式默认 Plugin registry 启动；实际 bin 的 allowlist、Describe 与禁用行为由同一 registry 控制，不再进入 legacy handler                                                |
| G-SCHEMA      | Synara Protocol             | M1         | open        | Protocol schema、TypeScript 与 runtime decoder 由一个可审计来源生成/校验；真实 v2.2 handler golden 与 additive v2.3 兼容门禁通过                                                           |
| G-CONFORMANCE | Synara + T3                 | M1         | open        | 两个宿主共同消费 testkit 的进程级 suite，覆盖 correlation、取消、背压、crash/resume、secret、path/symlink 和 capability 行为一致性                                                         |
| G-T3-DRAIN    | T3                          | M1         | open        | terminal/event pump 有显式 ACK/drain barrier；server/process restart 后 durable history 可恢复，`turn.completed` 不早于全部事件投影                                                        |
| G-RELEASE-M1  | Synara Release              | M1         | open        | packed dependency 已从 `workspace:*` 改写为精确 semver；Distribution 暴露 manifest/schema；外部临时项目 install/import/bin smoke、不可变 digest、provenance、SBOM、upgrade/rollback 全通过 |
| G-E2E         | Synara + T3                 | M1         | open        | 固定 commit matrix 上完成真实 Codex/Claude upstream Turn、文件修改/checkpoint/rollback、双 Provider 并发 soak、Synara 兼容回归与 T3 crash/reconnect E2E                                    |
| G-LEASE       | Synara + T3                 | M2         | not started | M1 immutable Distribution candidate 已固定；Lease create/ready/terminate、pairing/DPoP、broker/generation、RBAC、计量、审计和 revoke/fencing 负向测试通过                                  |
| G-RELEASE-M2  | Synara Release              | M2         | not started | 基于已关闭 G-LEASE 的制品生成 M2 manifest/digest/provenance/SBOM，并完成外部环境 install、upgrade、rollback 与固定 commit matrix                                                           |
| G-EXPOSURE    | Product + Release           | 对外发布   | not started | 批准目标用户范围、支持等级、Registry/channel、升级/回滚与事故响应；没有该批准不得标 public beta/GA，也不得公开发布                                                                         |

G-BASELINE 关闭是进入 M1 验收的前置条件；G-ARCH、G-PKG、G-REGISTRY、G-SCHEMA、G-CONFORMANCE、
G-T3-DRAIN、G-RELEASE-M1 与 G-E2E 也全部关闭后，M1 工程里程碑才完成并形成 RC。G-LEASE 与
G-RELEASE-M2 全部关闭后，M2 工程里程碑才完成并形成 RC。任何 RC 的对外 exposure 还必须单独关闭
G-EXPOSURE；公开 Registry 不属于工程完成的自动结果。Deferred D1 suspend/resume 不阻塞 M2 的
`create → ready → terminate`，也不能在 G-LEASE 关闭前被用来提升 M2 状态。

以下每个 bullet 都以唯一 Gate ID 开头，只阻塞该 ID；一条同时影响两个 Gate 时会显式写两个 ID，不按
章节标题推导交叉依赖。

### 19.1 架构验收维度

- `G-ARCH`：公共包不 import `apps/server`、Go Control Plane 或任何 UI 文件；
- `G-ARCH`：公共 ABI 不出现 Effect、T3 branded ID、Synara `ProviderKind` 闭集；
- `G-ARCH`：Workspace/Checkpoint authority 在创建 Runtime Session 时唯一且可观察；
- `G-ARCH`、`G-SCHEMA`：Provider Host v2 wire 第一阶段零破坏；
- `G-ARCH`、`G-CONFORMANCE`：一个 Provider Instance 的状态不泄漏到另一个 Instance；
- `G-LEASE`：Environment Lease 与 Agent orchestration authority 分离，Synara 与 T3 不写同一 Turn 或
  Checkpoint 终态。

### 19.2 功能验收维度

- `G-CONFORMANCE`：Synara 原有 Runtime 路径行为等价，Start/Resume/Send/Interrupt/Approval/UserInput/
  Stop 全部可重放；
- `G-E2E`：T3 embedded 同时支持 Codex、Claude，且 T3 内置 Provider 行为不变；
- `G-T3-DRAIN`：server/runtime/browser restart 后不重复消息、不丢终态；
- `G-E2E`：rollback 由 Runtime 恢复 Provider history、由宿主恢复 Workspace checkpoint，bridge 只协调
  两个 owner；任一侧失败时整体 fail closed，不能把单侧成功报告为已回滚；
- `G-LEASE`：T3 Environment Lease 使用标准 pairing、managed DPoP 且完成 create/ready/terminate；
- `G-LEASE`：managed direct-ingress 与 relay 不广告或自动降级到普通 Bearer；只有部署级显式
  `internal-self-hosted` profile 可以启用 Bearer。

### 19.3 安全验收维度

- `G-CONFORMANCE`：Provider secret 不出现在 argv、普通 env、日志、事件或 resume cursor；
- `G-CONFORMANCE`：Artifact path escape/symlink 测试全绿；
- `G-ARCH`、`G-CONFORMANCE`：未授权 Provider/能力 fail closed；
- `G-E2E`：Synara Tenant/Generation/Grant 边界不因抽包放松，T3 插件禁用后不再启动 Runtime child；
- `G-RELEASE-M1`：M1 插件包固定 digest，并生成 provenance/SBOM；
- `G-LEASE`：pairing token、broker grant 和 Provider secret 不进入日志/持久化配置/SQLite，旧 generation
  无法继续连接或取密；
- `G-LEASE`：Supervisor administrative bootstrap 不暴露给用户；membership/RBAC/generation 变化撤销
  对应 pairing link 和活动 session；
- `G-LEASE`：pairing token 兑换后原子失效；DPoP token 重放、proof key 不匹配、Bearer downgrade、旧
  generation 和已撤权 subject 均 fail closed；
- `G-RELEASE-M2`：M2 制品重新生成并验证包含 Lease surface 的 digest/provenance/SBOM 和 secret scan。

### 19.4 Release 与 Exposure 验收维度

- `G-RELEASE-M1`：双宿主兼容矩阵固定实测 commit，npm tarball 内容符合 allowlist；
- `G-RELEASE-M1`：install、upgrade、rollback、uninstall 文档完整，示例不包含真实凭证；
- `G-RELEASE-M2`：Lease 制品的外部环境 install/upgrade/rollback、固定 commit matrix 与运行清单通过；
- `G-RELEASE-M2`：M2 示例、manifest 和审计 evidence 不包含真实 credential/pairing token；
- `G-EXPOSURE`：产品/运维批准用户范围、支持等级、channel、回滚和事故响应后才能对外发布；
- `G-EXPOSURE`：所选 channel 为公开 npm 时才要求 OIDC trusted publishing；内部 RC 不以此为前置条件；
- `G-EXPOSURE`：Registry 发布前最多标 internal beta；G-LEASE/G-RELEASE-M2 未关闭时，不标 Lease public
  beta；未经独立 GA 批准不标 GA。

## 20. 风险与缓解

| 风险                          | 影响                                | 缓解                                                                   |
| ----------------------------- | ----------------------------------- | ---------------------------------------------------------------------- |
| T3 SPI 持续变化               | Bridge 频繁破坏                     | app-neutral ABI + T3-owned bridge + commit matrix + focused drift gate |
| 低估 Driver 构造要求          | title/commit/status 等功能半残      | conformance 强制 snapshot、adapter、四类 text generation 全部有实现    |
| Effect 版本冲突               | 类型/运行时异常                     | 公共 ABI 禁止 Effect；宿主内转换                                       |
| 两套 Runtime Event 漂移       | 丢事件/错误投影                     | 单一 schema 来源 + golden frames + mapper exhaustiveness               |
| 本地/远端 Workspace 混淆      | 数据错误、误回滚                    | 正式云形态移动完整 T3 server；control-only 默认关闭                    |
| 双 Turn/Checkpoint 权威       | revert/fork/终态不一致              | T3 Lease 不复用 Synara Session 状态机；T3 独占 Turn checkpoint         |
| T3 stateDir 未持久化          | environment identity 与 thread 丢失 | 独立 state volume；suspend/resume identity 测试                        |
| 未 quiesce 即做环境快照       | SQLite/Workspace/receipt 不一致     | Phase 4 不开放 suspend；Deferred D1 admission fence + stop + 原子快照  |
| 长期 API key 进入 Sandbox     | 横向访问                            | generation-fenced local broker + 匿名 FD/单次 grant                    |
| Pairing endpoint 生命周期漂移 | 已回收环境仍可访问                  | 先 revoke ingress/auth，再 drain，再卸载和回收                         |
| 动态插件任意代码执行          | 宿主失陷                            | 第一阶段只编译期 allowlist，Runtime 默认进程隔离                       |
| Provider 能力被 UI 过度承诺   | 用户遇到假按钮                      | Describe + capability map，缺失默认 unsupported                        |
| 事件重放产生重复消息          | timeline 污染                       | 稳定 event ID + receipt + sequence gap gate                            |
| 包抽取拖慢冷启动              | 破坏秒级体验                        | Describe cache、预启动、性能预算和 before/after SLO                    |

## 21. 备选方案与否决理由

### 21.1 直接复制 Synara Provider Adapter 到 T3 Code

否决。会形成两份 Provider 生命周期、事件规范、安全修复和能力目录，短期快、长期无法维护。

### 21.2 只实现一个 Polaris Provider Adapter

可作为 delegated control-only，但不能作为完整方案。它没有解决 Workspace/Git/Terminal/Checkpoint
权威，默认启用会制造错误产品语义。

### 21.3 用 MCP 作为插件协议

否决为主协议。MCP 适合工具调用，不提供完整 Session/Turn、stream、interrupt、approval、resume、
checkpoint、generation 和 terminal result 语义。MCP 可以继续作为 Runtime 内的工具入口。

### 21.4 只实现 ACP

ACP 可作为兼容 entrypoint，特别适合让已有 ACP 宿主快速启动 Runtime；但它不能替代 Synara 已有
Provider Host v2 的 Credential/Artifact/Checkpoint/Generation 能力。若后续提供 ACP，应该是
`cloud-agent-runtime/acp` 适配器，不是新的核心模型。

### 21.5 把 Synara Control Plane 全部移入 npm 包

否决。Go 控制面、数据库事务、Worker Protocol、Kubernetes reconciler、KMS 和企业治理不是
Provider Runtime，可移植化会破坏现有权威和验证资产。

### 21.6 第一版就做动态插件目录/市场

否决。它增加签名、更新、权限、任意代码执行和兼容治理，不能帮助验证 Runtime 是否真的可复用。

## 22. 已确认决策记录

以下决策已于 2026-08-08 确认，是后续实施基线，不再作为开放问题：

1. Developer API 继续使用 `Polaris` 品牌；
2. 首个 T3 交付只做 `t3-embedded`，不捆绑 `delegated-control-only`；
3. 新增独立 `T3EnvironmentLease` 产品面，不复用 Synara Session/Execution。允许 Synara 与 T3 Code
   各做最小协调修改以降低长期 patch，但必须遵守 4.3 的 owner 表：Synara 独占 Lease/调度/RBAC/
   generation/broker，T3 独占 Thread/Turn/Workspace/VCS/Checkpoint，Runtime 只拥有 Provider 生命周期；
4. 同时设计其他 Provider 的扩展节奏，但第一批通用可执行包只发布 Codex 与 Claude。当前 Host catalog
   其余 Cursor、Antigravity、Grok、Kilo、OpenCode、Pi 属于 `local-only` 或尚无可移植 Runtime 路径，
   不能把“进入 catalog”误报成“已支持远端执行”；
5. T3 Code 使用 fork `git@github.com:hxp0618/t3code.git`：`main` 只与
   `https://github.com/pingdotgg/t3code.git` fast-forward 同步，集成工作进入独立
   `codex/cloud-agent-runtime-integration` 分支或对应 worktree；
6. `@synara/provider-host` 兼容包保留一个 minor release，并在移除前提供迁移告警和双宿主矩阵；
7. Lease Credential Broker/Grant 属于 Stage 7 public beta 扩展，但必须先完成 Phase 4 所列 RBAC、
   managed DPoP、revoke/fencing、secret containment、独立计量和审计门禁；未通过时只允许
   internal beta。
8. Lease 客户端认证统一沿用 T3 的 bootstrap/token-exchange/session/WebSocket-ticket 模型：pairing
   token 一次性且短期，Stage 7 public beta 的 managed direct ingress 与 relay 强制绑定客户端 proof
   key；普通 Bearer 只允许部署级显式 `internal-self-hosted` 策略，不能作为 public beta 默认或失败回退。

这些选择不改变核心拆分：Protocol、Provider Plugin、Runtime、Host Adapter、Host Authority 五层必须
保持独立。若要改变任一 owner、公开阶段或 first-provider 范围，必须新增 ADR/变更记录，而不是在实现中
隐式偏离。

## 23. M1 当前唯一近程交付

M1 的近程交付固定为**七个发布包加一个 T3 integration slice**：

1. `@synara/cloud-agent-protocol`：从现有 contract 抽取，保留 v2.2 行为并以 additive 2.3 增加
   `GenerateText`；
2. `@synara/cloud-agent-provider-api`：固定通用 Provider Plugin ABI；
3. `@synara/cloud-agent-provider-codex`：物理承载 Codex 实现、probe 与依赖，成为唯一通用 Codex 发布源；
4. `@synara/cloud-agent-provider-claude`：物理承载 Claude 实现、probe 与依赖，成为唯一通用 Claude 发布源；
5. `@synara/cloud-agent-runtime`：从 `apps/provider-host` 抽取通用装载/会话内核；Runtime 不发布同名
   executable，旧 `provider-host` bin 由兼容壳保留；
6. `@synara/cloud-agent-testkit`：把现有测试升级为双宿主进程级黑盒 conformance；
7. `@synara/cloud-agent-distribution`：固定上述包的精确版本，暴露 bin/manifest/schema，并生成不可变
   release digest、provenance 与 SBOM；
8. T3 Code `embedded` integration slice：通过同一个 Bridge 注册 Codex/Claude，一个 Provider Instance、一个
   Workspace，覆盖 Start/Resume/Send/Interrupt/Approval/UserInput/Diff/Checkpoint/Rollback/Restart、四类
   `GenerateText` 与设置页安装路径；完整退出条件以第 19 节 M1 Gate 为准。

`@synara/provider-host` 兼容壳与 Synara adapter 是 M1 的兼容工作面，但不增加第八个发布包。上述第 1–7
项是七个发布包，第 8 项是 T3 仓内 integration slice；二者必须在同一个 M1 gate 中验收。

完成这批后进入 `T3EnvironmentLease` 的 API/状态机/安全威胁模型和 Phase 4 实施门禁；设计方向已经
确认，不再回退为复用 Synara Session/Execution。Polaris delegated 只在出现明确 control-only 产品需求
后评估。这样先证明“Cloud Agent Runtime 确实成为可插拔内核”，再增加环境供给，不同时引入远端
Workspace 投影。

截至 2026-08-09，上述范围已有 **source implementation in progress**，但不能整体标记“已实现”：Protocol、
Provider API、Runtime registry、T3 Bridge 和 Distribution 已有首批源码；Codex/Claude 仍是 legacy
facade，testkit 仍缺完整双宿主黑盒 suite。npm 发布、真实 Codex/Claude 付费 Turn、T3 完整 server
process crash/restart E2E 或 Phase 4 同样未完成；准确边界见附录 A，不得由 focused test 外推。

## 24. 与现有 Synara 方向的关系

本设计不替代以下现有方向：

- [`fast-cloud-agents-implementation.md`](fast-cloud-agents-implementation.md) 继续负责秒级供给、
  guaranteed warm、Provider Host prestart 和 Workspace cache-first；
- [`fast-provision-runtime-proposal-v0.md`](fast-provision-runtime-proposal-v0.md) 继续负责
  Kubernetes warm 与 microVM/snapshot 供给层；
- [`external-sdk-developer-platform.md`](external-sdk-developer-platform.md) 继续负责公共 API、SDK、
  Service Account、SSE 和 BYO Target；
- [`worker-protocol-v2.md`](../contracts/worker-protocol-v2.md) 继续负责 Worker/Generation/Artifact/
  Workspace Grant；
- [`provider-host-v2.md`](../contracts/provider-host-v2.md) 是本插件 Runtime wire 的现有基础。

它新增的是一条明确的复用轴：**同一个 Provider Runtime 内核，可以被多个 Agent GUI/控制面宿主，
以不同的 Workspace 和平台权威方式使用。**

## 25. T3 Code 本地基线与持续跟踪机制

### 25.1 当前基线

| 字段                   | 当前值                                                         |
| ---------------------- | -------------------------------------------------------------- |
| 本地目录               | `/Users/huang/devel/project/huang/business/t3code`             |
| `origin`               | `git@github.com:hxp0618/t3code.git`                            |
| 官方 upstream          | `https://github.com/pingdotgg/t3code.git`                      |
| 本地 remote 状态       | 已配置 `origin` 与官方 `upstream`                              |
| 主 clone 分支          | 本地 `main` tracking `origin/main`                             |
| 初次调研 commit        | upstream `a20923ce463335e89e92f5983d98a180536e8e7d`            |
| 实施固定 commit        | upstream `a6c9b41f902fba2a4137806c09e829935e91baac`            |
| 本地/fork `main`       | `8101cd044911c7dc2a2adf7c7a9ba7962abf57b6`                     |
| 实施 worktree describe | `v0.0.33-nightly.20260808.1033-10-ga6c9b41f9`                  |
| fork/upstream          | fork `main` 保持旧点；实施 worktree 直接固定 upstream commit   |
| 基线复核时间           | 2026-08-09 CST                                                 |
| 主 clone 状态          | 完整 clone、非 shallow、clean                                  |
| 集成 worktree          | `/Users/huang/devel/project/huang/business/t3code-cloud-agent` |
| 集成分支               | `codex/cloud-agent-runtime-integration`（已创建，未提交）      |

从初次调研点 `a20923...` 到实施固定点 `a6c9b41...`，watched surface 出现三组上游变更：Claude resume
握手修复、Codex queued follow-up stop 修复、ProviderService 的图片附件支持。它们修改了
`ProviderRuntimeIngestion`、Claude/Codex session runtime 与 `ProviderService`，属于 P1 复核而非 P0
重设计：完整 T3 server/Workspace 权威、ProviderDriver SPI、connection/auth 与 checkpoint owner 没有
改变。Bridge 因此显式拒绝当前 Protocol 2.3 尚不能投影的附件，防止静默丢文件；其余命令映射继续适用。

主 clone 继续保持只读跟踪，不和 Synara dirty checkout 混写；实现只在独立 worktree。官方 remote 已
配置，以下命令只用于新 clone 的一次性初始化：

```bash
git -C /Users/huang/devel/project/huang/business/t3code remote add upstream https://github.com/pingdotgg/t3code.git
```

仅在确认 `upstream` 尚不存在时执行。主 clone 的 `main` 不自动 merge/rebase；所有集成改动进入
`codex/cloud-agent-runtime-integration` 独立 worktree，不直接提交到 `main`。何时 fast-forward 并推送
fork `origin/main` 属于单独发布动作，不由本地 source implementation 自动触发。

### 25.2 必须跟踪的 T3 surface

```text
Provider SPI
├── apps/server/src/provider/ProviderDriver.ts
├── apps/server/src/provider/builtInDrivers.ts
├── apps/server/src/provider/Layers/ProviderInstanceRegistryHydration.ts
├── apps/server/src/provider/Layers/ProviderInstanceRegistryLive.ts
├── apps/server/src/provider/Layers/ProviderSessionReaper.ts
└── apps/server/src/provider/Services/ProviderAdapter.ts

Contracts and settings
├── packages/contracts/src/providerInstance.ts
├── packages/contracts/src/settings.ts
├── apps/server/src/serverSettings.ts
├── apps/web/src/components/settings/providerDriverMeta.ts
├── apps/web/src/components/settings/AddProviderInstanceDialog.tsx
└── apps/web/src/components/settings/ProviderInstanceCard.tsx

Workspace authority
├── apps/server/src/orchestration/Layers/ProviderRuntimeIngestion.ts
├── apps/server/src/orchestration/Layers/CheckpointReactor.ts
├── apps/server/src/checkpointing/
├── apps/server/src/vcs/
├── apps/server/src/workspace/
└── apps/server/src/terminal/

Remote environment
├── docs/internals/remote.md
├── packages/client-runtime/src/connection/
├── packages/contracts/src/remoteAccess.ts
├── apps/server/src/environment/
└── apps/server/src/auth/
```

### 25.3 每次同步的只读流程

```bash
git -C /Users/huang/devel/project/huang/business/t3code status --short --branch
git -C /Users/huang/devel/project/huang/business/t3code fetch origin main
git -C /Users/huang/devel/project/huang/business/t3code fetch upstream main
git -C /Users/huang/devel/project/huang/business/t3code rev-parse origin/main upstream/main
git -C /Users/huang/devel/project/huang/business/t3code log --date=short --pretty=format:'%h %ad %s' a6c9b41f902fba2a4137806c09e829935e91baac..upstream/main -- apps/server/src/provider packages/contracts/src/providerInstance.ts apps/server/src/orchestration/Layers/CheckpointReactor.ts packages/client-runtime/src/connection
git -C /Users/huang/devel/project/huang/business/t3code diff --name-status a6c9b41f902fba2a4137806c09e829935e91baac..upstream/main -- apps/server/src/provider packages/contracts/src/providerInstance.ts apps/server/src/orchestration packages/client-runtime/src/connection docs/internals/remote.md
```

`fetch` 后不自动 merge/rebase。先输出影响报告，再由实现分支选择新的固定 commit。每份报告至少记录：

```yaml
previousCommit: a6c9b41f902fba2a4137806c09e829935e91baac
candidateCommit: <upstream/main sha>
providerSpiImpact: none | compatible | bridge-change | redesign
checkpointImpact: none | compatible | behavior-change | redesign
connectionImpact: none | compatible | behavior-change | redesign
uiMetadataImpact: none | compatible | bridge-change
focusedTestsRequired: []
decision: stay | advance | block
```

### 25.4 漂移分级与响应

| 等级            | 触发条件                                                                                    | 响应                                                 |
| --------------- | ------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| P0 重新设计     | T3 改变“一 server 一完整环境”边界、checkpoint 权威或 Provider event 终态语义                | 暂停升级，更新本文和 threat model                    |
| P1 Bridge 更新  | `ProviderDriver`、`ProviderInstance`、Effect env、Runtime Event 或 text generation 形状变化 | 更新 T3-owned bridge 和 focused tests，再推进 commit |
| P2 配置/UI 更新 | driver descriptor、设置 schema、unknown-driver UX 变化                                      | 更新配置/UX adapter，不阻塞 Runtime 协议             |
| P3 无影响       | 文档、样式或不相关 Provider 实现变化                                                        | 记录后推进基线                                       |

检查时点：开始 Phase 3 前、每次升级 T3 commit 前、发布 candidate 前，以及 T3 上游 Provider/remote
architecture 相关 PR 合入后。这里只定义跟踪机制；除非另行授权，不创建自动拉取、自动合并或远端
通知任务。

## 附录 A：2026-08-09 Source Implementation Evidence

### A.1 实施位置与变更边界

本批只写入两个隔离 worktree：

| 宿主   | worktree                                                          | 分支                                      | 固定基线     |
| ------ | ----------------------------------------------------------------- | ----------------------------------------- | ------------ |
| Synara | `/Users/huang/devel/project/huang/business/synara-t3-cloud-agent` | `codex/synara-t3-cloud-agent-integration` | `fc9f63a...` |
| T3     | `/Users/huang/devel/project/huang/business/t3code-cloud-agent`    | `codex/cloud-agent-runtime-integration`   | `a6c9b41...` |

原 Synara dirty checkout 与 T3 主 clone 在本附录首次取证时没有接收运行时代码改动，两个集成分支当时也
尚未提交或推送。后续 source-control merge 状态必须以当前 Git refs 为准；代码进入远端分支不改变本附录
的验证边界，也不等同于 npm 发布、部署、public beta 或 GA。

### A.2 Synara 已实现

- 新增 `cloud-agent-protocol`、`cloud-agent-provider-api`、`cloud-agent-runtime`、通用 Codex/Claude
  Provider、`cloud-agent-testkit` 与 `cloud-agent-distribution` 七个包；
- Provider Plugin ABI v1 与默认 Runtime 根入口只使用 Promise、AsyncIterable、AbortSignal 和普通 JSON
  类型，不导出 Effect、T3 ID 或 Synara ProviderKind 闭集；旧 Provider Host 类型只在明确标记的
  `legacy-provider-host` 迁移子入口，但 package 内部仍有 Synara legacy 依赖；
- Protocol 当前为 additive 2.3，保留既有 v2.2 命令语义，加入四类隔离 `GenerateText`；Runtime Event
  保持 v2；能力目录只有一个可编辑来源；JSON Schema 现在按 discriminator 验证 Result/Error/Event 和
  关键 command payload，并以 Ajv 门禁 vocabulary 一致性；
- `createCodexProvider()` 与 `createClaudeProvider()` 是普通 Plugin ABI factory，宿主/Distribution 显式
  注册，不使用 `@synara/cloud-agent-runtime/providers/*` 私有路径或目录扫描；当前 factory 仍代理 legacy
  实现，尚不能视为独立 Provider implementation；
- `@synara/provider-host` 缩为只依赖 Runtime 的兼容 bin wrapper；只有 Distribution 发布
  `cloud-agent-runtime` bin，Runtime 包不再争用同名 bin。Distribution manifest 与各 package version
  一致；`CLOUD_AGENT_DISTRIBUTION_MANIFEST` JavaScript export 已深冻结，但直接导入的 `manifest.json`
  不具备运行时冻结语义。源码 `releaseDigest` 故意为空，最终 digest 必须由 release pipeline 生成；
- stdio client 实现 command/message 上限、写入背压、并发 command correlation、AbortSignal、匿名 fd 3
  自动注入和 scoped process teardown；单命令取消使用 terminal tombstone，不再杀死共享 Runtime；
- stdio server 通过最多 64 帧的串行 NDJSON writer 执行真实 UTF-8 message 上限与 `drain` backpressure，
  返回前 flush；handler 不再为长 Turn 累积全部中间事件；
- 当前 legacy stdio handler 的 StopSession 使用 session epoch fencing，先阻断旧事件/状态提交，再等待活动
  operation quiesce；另一个 JS Plugin facade path 的 close 共享 promise，释放 credential 前等待 command/
  control/artifact task，并验证 Host generation、Workspace、model 与 Artifact 相对路径/digest；
- 上述 Plugin HostServices authority、credential、Artifact 与 close guard 目前只在 JS facade path 生效；
  Distribution 的 active stdio bin 仍进入 legacy handler，跨仓握手尚未获得这些 Plugin path 保证；
- testkit 已提供 descriptor 值域和 transcript 级 correlation/event/late-frame 断言，但完整进程级黑盒矩阵
  仍列为未完成。

### A.3 T3 Code 已实现

- `contributedDrivers.ts` 提供编译期显式 composition seam；新增 `synaraCloudAgent` driver、settings schema、
  设置页 metadata/select 控件和 unavailable round-trip，不扫描插件目录；
- Driver 在广告 ready 前执行真实 `Describe`，验证 Provider kind、Protocol、核心 capability、Runtime Event
  v2、四类 text-generation task、Provider 可用性/兼容范围和显式 enablement；配置
  `runtimeBinarySha256` 时要求绝对 binary path 并校验该文件。Codex/Claude 各提供一个默认模型；
- 每个 T3 thread 使用独立、scoped Runtime process。消息必须匹配 Protocol 2.3、request、execution、
  generation 与 command；stdout 在 JSON.parse 前按原始字节分帧并限制大小，decode/correlation/unknown
  command/exit/write 任一 fatal 都原子关闭、失败全部 pending，并拒绝后续 execute；
- 映射 Start/Resume/Send/Steer/Interrupt/Approval/UserInput/Stop、Runtime Event v2、resume cursor 与
  rollback history reconstruction。Approval/UserInput 使用 Runtime 要求的 `resolution` envelope；新进程
  generation 递增并随 T3 已持久化 resume cursor 保存；
- Adapter 的父 Scope 关闭会 stopAll 并 shutdown event queue；start/send/stop/rollback 用短临界区和 CAS，
  不持锁等待 Runtime 长操作；StopSession 有 2 秒 deadline，失败/中断仍清理 active state、event fiber、
  child scope 和 session map；
- `plan`、per-turn model mismatch、`auto`/`auto-accept-edits`、`acceptForSession`/`cancel` 等当前 wire 无法
  精确表达的语义会明确返回 validation error，不再静默降级；Artifact/Checkpoint 未接入 T3 authority 时
  产生 bounded warning，不伪装为已消费；
- 四类 T3 text-generation 操作都调用 Protocol 2.3 `GenerateText`，每次使用隔离 Runtime session，失败
  不回退到 T3 内置 Provider，并用 finalizer 保证失败路径也发送 StopSession；
- 当前 wire 没有附件投影，因此图片/文件输入返回稳定 validation error，不静默丢弃。`turn.diff.updated`
  继续只触发 T3 本地 CheckpointReactor，Provider diff 不成为 Workspace 权威；
- `credentialProfile` 在 embedded source slice 中只保留未来配置槽；非空时 fail closed。当前 embedded
  模式使用 Provider 本机认证，Phase 4 才接 generation-fenced credential broker。

### A.4 已观察的本地证据

本设计基线的依赖快照如下；它只描述当前来源，不构成未来兼容承诺：

| 依赖面                | Synara                             | T3 Code                                                    |
| --------------------- | ---------------------------------- | ---------------------------------------------------------- |
| `packageManager` 声明 | Bun `1.3.12`                       | pnpm `11.10.0`                                             |
| 本机最终复核执行器    | Bun `1.3.14`                       | Node `26.7.0` + pnpm `8.11.0`，有 engine mismatch warning  |
| Effect                | `effect-smol` Git commit `8881a9b` | `4.0.0-beta.103`，仓内 patch                               |
| Claude Agent SDK      | manifest 精确 `0.3.207`            | manifest `^0.3.170`，lock 为 `0.3.170`                     |
| Codex CLI             | 受控 Worker release `0.145.0`      | 使用环境中的 `codex` binary，由 health/version probe 识别  |
| Claude Code CLI       | 受控 Worker release `2.1.197`      | 使用环境中的 `claude` binary，由 health/version probe 识别 |

T3 列中的 Claude SDK/Codex CLI 属于 T3 **内置 Provider driver**。新增的 `synaraCloudAgent` driver 不链接
或复用这些实现，而是通过 out-of-process Distribution 使用当前 Runtime 携带的 Claude SDK `0.3.207` 与
Codex compatibility policy；两组版本可以独立升级，但每次都必须重跑对应 compatibility matrix。

- Synara 新包 build/typecheck/focused tests、Provider Host wrapper build，以及 npm pack dry-run；
- T3 contracts、settings UI、Driver probe/digest、process correlation、adapter、approval、structured user
  input、interrupt、resume generation、rollback focused tests；text-generation 代码实现四类任务，但当前
  focused test 只执行 thread-title 路径；
- 使用实际 `@synara/cloud-agent-distribution` stdio entrypoint，从 T3 完成 Claude
  `Describe → ready → StartSession → StopSession` 跨仓握手；
- 公共默认 Runtime declaration 不引用 Effect、`@t3tools/*` 或 Synara host contract；T3 bridge 代码留在
  T3 仓内。

最终本地结果：

- Synara：七个新包合计 21 个 test files / 215 tests 通过，contracts 18 files / 209 tests 通过；
  Provider Host wrapper 和七个新包 build/typecheck 通过；`bun fmt` 检查 2944 个文件，`bun lint` exit 0
  （仓内既有 warnings 仍存在），`bun typecheck` 20/20 packages 通过；
- T3：contracts 33 tests、Web settings 11 tests、server focused suite 5 files / 29 tests 通过；无 Runtime
  环境变量时 cross-repository test 另有 1 file / 1 test 明确 skipped。contracts/Web/server 三个定向
  typecheck、21 个范围内文件 format check 与 lint 均通过且无输出告警；
- 跨仓：最终 Distribution stdio entrypoint 的 Claude Driver
  `Describe → ready → StartSession → StopSession` 通过；
- 发布预检：七个包 `npm pack --dry-run` 全部成功，tarball allowlist 中 test source 数量为 0；源码
  manifest 保持 `releaseDigest: null`，没有伪造 managed release evidence。

这些都只是 local validation evidence。

### A.5 明确未完成/不能外推

- 未发布 npm 包、未生成最终 release digest/provenance/SBOM，未安装到生产 Synara/T3；
- Runtime 仍直接承载 Codex App Server、Claude SDK 和 Synara legacy contract；两个 Provider 包仍是兼容
  facade，Distribution stdio 仍走 legacy handler，尚未由 default Provider registry 启动。Provider 真拆包、
  独立依赖/发布/回滚、allowlist 对实际 bin 的约束，以及让 active stdio path 获得 Plugin HostServices
  authority/credential/Artifact/close guard，都是 M1 硬门禁；
- 公共 TypeScript command 仍以通用 payload 表达，legacy Start/Resume wire 内仍携带 RunnerInput；虽然 JSON
  Schema 已加强且 Host authority 在兼容层 fail closed，schema → TS/runtime decoder 的单一代码生成仍未完成；
- testkit 尚未成为两个宿主共同消费的进程级黑盒 suite；缺少 crash/resume、approval/user-input、
  backpressure、secret redaction、path escape/symlink 等完整公共矩阵。T3 的跨仓测试在无 Runtime 环境变量
  时会显式 skip，只有专门 compatibility job 的实跑才构成证据；
- T3 的 terminal 与 event pump 尚无显式 ACK/drain barrier；transport 保证 publish 顺序和 late-frame
  fail closed，但“所有 Runtime Event 已投影后再产生 `turn.completed`”仍需进程级测试和 barrier；
- 当前 `runtimeBinarySha256` 只验证单文件，不是 Distribution release digest；Distribution 的五个内部
  依赖仍是 `workspace:*`，自身也没有 schema export。`npm pack --dry-run` 不能代替 packed `package.json`
  精确 semver 检查、manifest/schema exposure 或外部临时项目 install/import/bin smoke；
- 本机最终 T3 focused test 虽通过，但 Node `26.7.0` + pnpm `8.11.0` 不满足仓库声明的 Node `^24.13.1` +
  pnpm `11.10.0`，命令明确产生 engine mismatch warning；G-RELEASE-M1 必须在固定受支持 toolchain 重跑；
- 未执行真实 Codex/Claude Provider Turn。当前机器 Codex 为 `0.147.0`，超出已声明的
  `>=0.145.0 <0.146.0` 范围，正确结果应是 unavailable；Claude 只验证了 SDK-backed session handshake，
  没有产生上游调用费用；
- 尚未完成完整 T3 server process crash/restart E2E、浏览器重连、真实文件修改/checkpoint/rollback E2E
  和两个 Provider 实例并发 soak；rollback history 仍只覆盖当前 adapter 进程内记录，恢复后旧 Turn 的
  authoritative history 接入尚未完成；目前只有 Bridge resume/generation、T3 既有持久化链路与 focused
  test；
- Phase 4 `T3EnvironmentLease`、pairing/DPoP、Lease Supervisor、credential broker、ingress、RBAC、计量与
  审计均未实施；Deferred D1 suspend/resume 更未开始；
- 因此本批只能标记 **source implementation in progress**，不能标记 Phase 0–3 complete、deployed、
  public beta 或 GA。
