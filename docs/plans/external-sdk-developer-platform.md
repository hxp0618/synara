# Stage 7：对外 SDK 与开发者平台

状态：IN PROGRESS（设计已冻结 2026-07-26；源码 public beta 已完成，外部激活待办）

2026-08-04 快照：OpenAPI、双语言 SDK、开发者文档、示例、Service Account 机器鉴权、Webhook、
BYO Execution Target 与 durable SSH provisioning 的仓库内实现已经收敛。当前公开面为 41 条
`public-beta` / `codegen-ready` operation，0 条 `route-only`，且 TypeScript、Python、真实
HTTP/SQLite conformance、SDK release artifact verifier 和全量 Go 测试均通过。一次性 OrbStack
Ubuntu VM 上的真实 SSH 验收也已通过，Service Account 通过公共 beta API 创建 Target、以稳定
幂等键提交并重放 provisioning operation，最终 Worker online；后续 Worker replacement、Control
Plane restart continuity、cleanup 与输出 secret scan 同样通过。详见
[Stage 7 Developer API SSH 验收报告](../reports/stage-7-developer-api-ssh-acceptance-20260804.md)。

尚未完成的工作均属于仓库外激活：开发者站点部署、受控环境的外网 Webhook egress、npm/PyPI
项目 ownership 与 OIDC Trusted Publisher 登记，以及真实 Provider release gate。因此这里的
“源码 public beta 已完成”不等于已经发布、生产 GA 或完成外部 Registry 所有权审批。

当前实施进度：M1 的路由公开面分级与 OpenAPI 3.1 路由面 SSOT 已经开始落地。控制面所有注册路由
必须通过分类 mux 显式选择 `internal`、`public-beta` 或 `public-ga`，并与
`docs/api/openapi.yaml` 的完整 `x-synara-route-surfaces` 清单双向一致；核心 Project / Session /
Execution interaction / Artifact 开发者面先进入 `public-beta`，Worker、平台治理、dev-login、SCIM
与 Artifact 内容通道保持 `internal`。防暴露守卫会阻止绕过分类 mux 的新增路由，并在 Stage 5/6
与 Stage 7 GA 门禁通过前拒绝出现 `public-ga` 路由。公开 operation 默认标记为 `route-only`，
表示路由身份、operationId、鉴权方案和统一错误 envelope 已进入契约，但完整请求/成功响应 schema
尚未冻结，因此不得用于 SDK codegen 或外部发布。首条纵向流程的 `createSession`、`createTurn`、
`streamSessionEvents` 与 `resolveExecutionApproval` 已补齐请求、成功响应、幂等和限流 header schema，
提升为 `codegen-ready`，并通过固定版本的 OpenAPI 3.1 语义校验。用于连接限额回退的
`listSessionEvents` 也已冻结为 `codegen-ready`：公开分页默认 50、最大 200，保留
`afterSequence` 领域游标。Service Account 资源角色与机器 Principal 已接入
`public-beta` 路由：`api.access`、tenant/organization 固定 RBAC、即时撤销、机器 actor 的
Idempotency/Event/Audit/SSE 归因和用户级 Provider Credential 隔离已有端到端门禁；原先无界的
Project 与 Project Session 列表均已改为默认 50、最大 200 的 scope-bound opaque cursor 分页，并保持
机器 Principal 不可见 private Project/Session。per-key 通用准入限制现由 Service Account 的 `rateLimitPerMinute` 驱动（默认 600、
范围 1–60000），使用数据库 UTC 分钟窗口原子计数，跨实例共享额度；响应携带
`RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset`，超限返回 429 + `Retry-After`。
每次已准入请求和 429 都按 Tenant / Organization / Service Account / 路由模板持久化，成功、4xx、
5xx 与耗时分别累计，并提供受 `service_accounts.read` RBAC 保护的控制台汇总接口。其余分页一致性、
完整 schema 与 Stage 6 计量投影仍是 M1 后续工作；当前状态不代表 M1 完成或公开 API 已可用。

M2 的 TypeScript SDK beta 也已建立第一段可运行骨架：新增私有（防误发布）
`@polaris-agents/sdk@0.1.0-beta.1` workspace 包；底层类型由 OpenAPI 生成并有 drift gate，手写领域层
提供 `Polaris`、Session handle、自动 `Idempotency-Key`、429/5xx/网络重试、稳定错误类型、限流与
重放 metadata，以及按 durable sequence 解码/续传的 SSE async iterator。SSE 消费者提前退出时
SDK 会主动取消底层 HTTP 请求，避免泄漏连接 lease；当 user/Tenant SSE 连接池满时，会自动切换到
同样执行 replay 抑制和 sequence-gap fail-closed 的 bounded Event 分页轮询。该包当前暴露上述五个
Session/Approval operation、bounded Project 创建/列表/读取及幂等更新/归档、Session 列表/读取/Usage/模型 CAS/挂起/恢复/幂等归档、Project/Session
安全 Provider capability 投影、带 watermark 的 pending
Interaction 与安全历史投影、幂等注册/安全读取 BYO Execution Target，以及创建/查询 durable provisioning operation，
并支持幂等 structured user-input resolve、bounded Artifact metadata、短时 download grant，以及响应丢失安全的
Artifact create/upload/complete/delete、安全 Execution cancel、active Turn interrupt/steer、双入口 checkpoint resume，
以及 sequence-guarded Compact/Review/Fork/Rollback，共四十一个
`codegen-ready` operation，测试和双格式构建已通过；仓库内发布
制品/流水线已经落地，但 npm 组织占位、OIDC
trusted publisher 的仓库外登记和其余 operation 仍未完成，因此不是已发布 beta。

M2 的开发者入口已开始形成可构建闭环：新增 Astro 静态文档站，覆盖 quickstart、机器鉴权、
幂等与重试、SSE 序列语义、Approval 和 typed error，并在同一构建产物中用 Redocly 生成 API
Reference。公开 Reference 不是直接渲染完整路由 SSOT，而是由仓库内过滤器只选择
`codegen-ready` operation；当前 41 条 `public-beta` operation 已全部冻结为可生成契约，不再保留
`route-only` 灰区。
新增 CI 修复 bot、PR review bot、批量迁移三个完整 TypeScript 示例；批量示例包含 durable
sequence checkpoint，CI/PR 示例从外部任务身份派生稳定幂等键。统一的
`stage7:developer:check` 已进入主 CI，覆盖 OpenAPI lint、生成漂移、SDK 类型/测试/双格式构建、
示例 typecheck、文档内容测试与静态站/API Reference 构建。该证据仍是源码与本地浏览器门禁；
尚未完成真实外部控制面 quickstart ≤ 5 分钟计时验收、部署发布或 npm ownership，因此不能把
M2 标为完成或发布 beta。

SDK conformance 已从 mock 扩展到真实 HTTP/SQLite Control Plane：Go 测试启动完整 handler，创建
真实 Service Account API Key 与 pending Approval，再分别由独立 Bun/Python 进程加载官方 SDK，验证 Session
创建与幂等重放、Turn 创建、Event 分页、原生 SSE 连续回放、SSE pool 饱和后的轮询回退、Approval
resolve 与幂等重放，以及 provisioning create + poll terminal state；Go 侧再核对机器 Audit actor。
Control Plane CI 显式安装 Bun 和 Python 3.11，
因此双语言用例不会因缺少 runtime 而静默跳过。该夹具仍不是部署环境、真实 Provider 或外部网络验收。

M3 的 Webhook、Python SDK 与 BYO target/provisioning 纵向切片已经开始实现。Python 3.11+
`polaris-agents@0.1.0b1` 使用标准库传输、OpenAPI 生成的 TypedDict/operation 清单和手写领域层，
已具备自动幂等键、429/5xx/网络重试、Event list/SSE、连接池饱和轮询回退、replay 抑制和 sequence
gap fail-closed；与 TypeScript SDK 共享上述真实控制面 conformance，但尚未完成 PyPI ownership、
trusted publisher 的仓库外配置和部署环境验收。Tenant-scoped owner/admin API Key 现可用两个新增
`codegen-ready` operation 幂等注册 SSH/Docker/Kubernetes Execution Target，并读取不含加密
configuration 的安全投影；Service Account 不能创建 local target。SSH install/upgrade/revoke、Worker
release/pool/placement 与 isolation policy mutation 仍是 internal，直到其跨进程 idempotency 和运维
契约实现，因此当前不能把注册成功解释为 compute ready。外部 provisioning v1 已按持久化异步
operation resource 实现为两个 `public-beta` operation（见
`docs/contracts/execution-target-provisioning-v1.md`），具备 durable receipt、
单 target 单活动操作、leader claim/lease/takeover、operation + target 双重 fencing、稳定 Worker
identity、安全结果投影、过期 claim takeover，以及“远端命令成功后进程退出”的同 operation 恢复；
现有同步 SSH 路由仍不公开。一次性本地部署的真实 SSH 演练已经通过，但尚未替代外部生产环境和
真实 Provider release gate。租户管理员可在
控制台创建、禁用、启用、轮换密钥和吊销 HTTPS Webhook，并查看投递状态；签名密钥通过现有 KMS
envelope 加密且仅创建/轮换时显示。选定 Session Event 在业务事务内按 endpoint 独立投影为 Outbox
消息，外发 body 只含版本、delivery/event/sequence 和资源 ID，不含 prompt、credential 或原始
Event payload；投递使用 `timestamp.body` 的 HMAC-SHA256、稳定幂等键、无 redirect/proxy 的安全
HTTP client，并拒绝 literal/DNS 解析后的 private、loopback、link-local 与 reserved 目标。已有本地
夹具验证成功签名投递、独立重试、死信和响应 body 不落库；这些仍不是部署环境的网络出口演练，不能
把 M3 标为完成。

SDK 发布工程已经建立默认不发布的受控 beta 流水线：源码 workspace 的 npm 包继续保持
`private: true`，发布任务只把 allowlist 中的构建产物与清理后的 manifest 复制到一次性 staging；
npm tarball、Python wheel 和 sdist 在发布前验证精确内容、包名、跨生态版本映射与 SHA-256 manifest，
并生成 GitHub artifact attestation。流水线先运行双 SDK 源码门禁和真实 Control Plane conformance，
只有从精确 `polaris-sdk-v<version>` tag 手动选择 publish、仓库变量确认 registry 已就绪、且通过
`polaris-npm` / `polaris-pypi` protected environment 后，才分别使用 npm/PyPI OIDC trusted
publishing；仓库不保存 registry token。npm organization/package ownership、PyPI project/publisher
登记和受保护环境审批都属于仓库外待办，因此当前实现只是可审计发布路径，不代表已经发布 beta。

Stage 1–6 把 Synara 从单机 GUI 变成了带租户体系、分布式执行平台和企业运营能力的 SaaS。但当前
`/v1` 的消费者全部是第一方：Web UI（浏览器 → Node 代理 → Go Control Plane，cookie 鉴权）、
Worker（Worker Protocol）和 IdP 的 SCIM 客户端。没有任何外部第三方可以在不读仓库源码的前提下
调用这个平台。

Stage 7 的目标是把同一个控制面开放成**外部开发者可编程的产品**：官方 SDK、机器鉴权、机器可读
契约、文档与自助接入，让第三方把 Synara cloud agents 嵌入自己的产品和工作流（CI 自动修复、
PR review 机器人、批量迁移、内部工具）。这直接服务"希望更多人使用平台"的增长目标，并与
[方向评估](cloud-agent-direction-assessment-20260726.md) 的北极星互补：E2B 式产品的增长引擎是
SDK-first——fast-provision 解决"体验快"，本 Stage 解决"接得进"。

`docs/contracts/saas-api-conventions.md` 早已预告本阶段："External API clients use bearer
sessions in a later phase."——Stage 7 就是那个 later phase。

## 1. 现状盘点（2026-07-26，`codex/saas-tenancy-user`）

### 1.1 已有地基（复用，不重建）

- **资源面完整且单点注册。** 全部租户路由集中在
  `services/control-plane/internal/httpapi/server.go`：Tenant/Organization/Membership/Invitation、
  Project、Session（turns、steer、interrupt、model-switch、compact、reviews、rollback、fork、
  suspend、resume、archive）、Execution（cancel、resume、approvals、user-input）、Artifact（含
  grant-token 上传/下载通道）、Credential 与 Binding、Execution Target/Group/Worker
  Pool/Worker Release（含 SSH install/upgrade/revoke 与 canary/promote/rollback）、
  Quota/Retention/Scheduling/Lifecycle Policy、Billing、Audit、SCIM。单点注册意味着 allowlist
  分级和防暴露守卫有唯一的落点。
- **API 约定成熟。** `X-Request-ID`/`X-Trace-ID`/`Traceparent` 全响应回传；`Idempotency-Key`
  与业务/Event/Outbox/Audit 同事务持久化，重放返回 `Idempotency-Replayed: true`，冲突返回
  `409 idempotency_conflict`；稳定 error code envelope；state-changing 请求强制 JSON content
  type。这些是 SDK 重试与错误模型能直接建立在其上的语义。
- **事件流已可续传。** `GET /v1/sessions/{id}/events/stream`（SSE）支持 `afterSequence` 游标、
  严格 sequence 连续性、durable backlog 分页回放、按 user/tenant 的连接限额（429 +
  `Retry-After`）；另有轮询回退 `GET /v1/sessions/{id}/events`。断线重连语义是现成的。
- **Service Account 的凭证形状是对的。** `internal/serviceaccounts`：`syna_sa_` 前缀、数据库仅存
  SHA-256、tenant + 可选 organization scope、rotate/revoke、`last_used_at`。缺的只是资源级
  scope 词表（现在仅 `scim.read/write`、`identity.read/manage`）和进入租户路由的 principal 通路。
- **RBAC 已冻结。** `internal/authorization` 的固定角色 × ~45 permission 常量矩阵
  （`docs/contracts/role-permission-matrix.md`）。API Key 的权限语言不需要新发明。
- **机器可读事件契约已存在。** `docs/contracts/runtime-event-v2.schema.json` 可直接生成 SDK 的
  事件类型。
- **de-facto 客户端契约。** `packages/control-plane-client/src/index.ts` 的手写 typed wrapper
  是目前请求/响应形状最完整的单一来源，可作为 OpenAPI 初稿的对照物；它已从 Web 私有路径抽出，仍不等于
  机器生成或公开兼容承诺。

### 1.2 三个真实缺口

1. **租户资源没有机器鉴权路径。** `requireAuth` 只接受 login-session cookie；Go 服务无 CORS
   处理，部署假设 same-origin Node 代理。Service Account 的 bearer 只接在 `/scim/v2/*`，其
   principal 类型进不了基于 `identity.Principal` 的租户 handler。对外 SDK 在鉴权/传输轴上是
   greenfield。
2. **没有 OpenAPI / route manifest。** 契约散落在 `server.go`、`packages/control-plane-client` 和 prose
   合同里，双源漂移风险已经存在。SDK 面必须先有单一机器可读来源。
3. **第一方视角遗留的粗糙处。** 分页不一致（audit 用 cursor、events 用 `afterSequence` 且默认
   limit 100 与约定的 50 不一致、`GET /v1/projects/{id}/sessions` 无分页无上界）；只有 session
   一条流，无 execution 级流。第一方客户端可以容忍这些，公开 API 不能。

## 2. 目标用户与产品分层

| 用户               | 需求                                             | Stage 7 范围                                                 |
| ------------------ | ------------------------------------------------ | ------------------------------------------------------------ |
| 应用开发者（主力） | 用 REST/SDK 创建会话、发 turn、流式消费、审批    | 核心；M1–M2                                                  |
| 自带算力团队       | 把自己的 SSH/Docker/K8s 机器接入 SaaS 控制面     | 收敛为 SDK/CLI 对既有 execution-targets API 的封装；M3 起    |
| 深度伙伴           | 自定义 Provider/Runner（Provider Host Protocol） | 非目标；Stage 7 之后单独立项，避免把内部协议过早变成公共承诺 |

## 3. 设计决策

### D1 公开面显式 allowlist，三级分级

- 每条 `/v1` 路由归入 `public-ga` / `public-beta` / `internal` 三级；默认 `internal`，公开必须
  显式声明。
- `internal` 永久项：`/v1/workers/*`（Worker Protocol）、platform routing-authority publisher、
  dev-login、SCIM（保持 Service Account bearer，但不算开发者产品面）。
- 分级落在 OpenAPI spec 的扩展字段上，并配一个守卫测试：枚举 mux 的全部注册路由，未声明分级
  即失败——防止新路由被静默公开。

### D2 OpenAPI 3.1 单一契约源 + CI 一致性门禁

- 在 `docs/api/openapi.yaml`（或 `services/control-plane/api/`）编写 OpenAPI 3.1，作为公开面的
  SSOT；请求/响应形状以 `controlPlaneClient.ts` 与 handler 现状为初稿对照。
- CI conformance：对真实控制面（SQLite profile 起进程）跑 spec 驱动的一致性测试——路由存在性、
  分级 allowlist、error envelope、分页语义、`Idempotency-Replayed` 行为。实现与 spec 漂移即红。
- 冻结 spec 前先收口 1.2-3 的不一致：统一 `limit`/`cursor` 语义、给无界列表补分页；events 的
  `afterSequence` 是有意的领域游标，保留但在 spec 中显式建模。
- 错误模型维持现有 envelope（`error.code` 为稳定契约），不迁移 RFC 7807——additive 原则优先。
- 长期方向：`controlPlaneClient.ts` 也从同一 spec 生成，消除双源（维护性优先，允许分阶段做）。

### D3 机器鉴权：扩展 Service Account，不新造凭证体系

- **凭证载体复用** `service_accounts` / `service_account_tokens`（哈希存储、轮换、吊销、
  `last_used_at` 全部现成）。对外文档语言称 "API Key"，实体上就是带资源 scope 的 Service
  Account token。
- **与 SCIM 凭证同一实体**：资源 API Key 与 SCIM/identity 的 Service Account 是同一模型，
  scope 词表分组（`scim.*`/`identity.*` 组与资源组），共用创建/轮换/吊销/审计生命周期与
  Console 视图，不引入第二种凭证实体。
- **权限语言复用固定 RBAC**：key 绑定固定角色（如 `member`、`agent_operator`）派生权限集，
  不发明第二套 scope DSL——与 Stage 6 "若不需要自定义 Role 则冻结固定 RBAC v1" 保持一致。
- **principal 统一**：授权层接受 machine principal 进入租户路由；Audit actor 记 Service
  Account 身份；`Idempotency-Key` 的 "tenant + actor" scope 语义天然覆盖 machine actor。
- **传输**：`Authorization: Bearer syna_sa_…`。tenant 从 key 的 scope 解析；路由中的 tenant
  path param 与 key scope 不一致直接 403。维持"client-supplied tenant header 永不授权"。
- **边界不放松**：不开 CORS，公共 API 面向服务器侧调用；浏览器场景只走官方前端。cookie 安全
  模型不因 Stage 7 改变。artifact 内容通道继续用短时 grant token，SDK 直接复用。
- **每 key 治理**：rate limit（含 SSE 连接配额与现有 `eventstream` 限额分池）、用量归因
  （Tenant/Organization/key 三级，进 Stage 6 计量）、过期时间、吊销即时生效、审计可查。

### D4 SDK = 生成的传输层 + 手写的领域层

语言矩阵：**TypeScript 首发**（自家栈、生态最大）、**Python 紧随**（AI 开发者主力）、Go 随
BYO-target 客户需求。全部从同一 OpenAPI 生成传输层（HTTP、类型、错误映射、分页迭代器）。
codegen 采用**自建管线**（openapi-generator/oapi-codegen 类开源工具 + 仓库内模板），不引入
Fern/Stainless 类托管服务：契约源、生成模板与发布节奏保持完全自控，无外部供应商依赖；模板
维护成本由双语言共享同一套 conformance 用例兜底。

领域层手写，承担 agent 领域无法 codegen 的体验：

- **Session handle**：`sessions.create` → `session.sendTurn/steer/interrupt/fork/…`；
  `session.events({ afterSequence })` 返回 async iterator，内建 SSE 断线重连、严格 sequence
  校验、超限时轮询回退。
- **Interaction 封装**：approval / structured user-input 以回调或 promise 暴露；
  `snapshotSequence` 与事件流的 reconcile 语义（contract 中的 watermark 规则）由 SDK 消化，
  调用方不需要理解竞态细节。
- **可靠性内建**：所有 mutation 自动生成 `Idempotency-Key`（UUID）；仅在带 key 时对网络错误
  /5xx 指数退避重试；`Idempotency-Replayed` 暴露给调用方；429 尊重 `Retry-After`。
- **typed errors**：稳定 `error.code` → 语言侧类型化异常/判别 union；`message` 不参与分支。
- **Artifact 流封装**：create → grant-token 上传 → complete；download → grant-token 读取。
- **redacted 事件显式建模**：`session.event.redacted` 是公共类型，降级展示由调用方决定。

事件类型从 `runtime-event-v2.schema.json` 生成；未知事件保持 Stage 3 的前向兼容策略（保留原始
payload，不抛弃、不失败）。

**内部 `@synara/contracts` 不是公共契约**：它是 GUI ↔ Node 的 WS 协议包，不对外导出，防止内部
协议泄漏成公共承诺。

**公共品牌定名 Polaris**（客户端类名 `Polaris`）。npm 裸名 `synara` 已被同类产品（AI coding
assistant，2026-06 仍活跃）占用，是不用 synara 做公共包名的直接动因；裸名 `polaris` 在
npm/PyPI 也均被占（2026-07-26 核查），PyPI `polaris-agents` 可用。首选坐标：npm
`@polaris-agents/sdk`、PyPI `polaris-agents`、Go `github.com/synara-ai/polaris-go`。npm org
归属无法匿名确认，注册占位是 M1 前置动作；发布走 GitHub Actions OIDC trusted publishing +
npm provenance、强制 2FA，与仓库既有签名/SBOM 供应链姿态一致。

目标 quickstart 形状（验收线：注册 → API Key → 下面代码跑通 ≤ 5 分钟）：

```ts
import { Polaris } from "@polaris-agents/sdk";

const polaris = new Polaris({ apiKey: process.env.POLARIS_API_KEY });

const session = await polaris.sessions.create({
  projectId: "…",
  provider: "codex",
  model: "gpt-5.6-sol",
});

await session.sendTurn({ inputText: "修复 CI 上失败的测试并提交 PR" });

for await (const event of session.events()) {
  if (event.type === "runtime.output.delta") process.stdout.write(event.payload.text);
  if (event.type === "interaction.approval.requested") await event.approve();
}
```

### D5 事件：SSE 为主、thin-payload Webhook 为辅

- 拉端保持 SSE（语义已完备）；execution 级进度继续投影进 session 流，**不做独立 execution
  流（已决策）**。客户端过滤有契约保证：`executionId` 是 `runtime-event-v2.schema.json` 的
  required 字段；SDK 提供 `turn.events()` 一类按 executionId 过滤的 helper。不做的理由：独立
  流要复制整套授权投影（redacted interaction 可见性）与 SSE 连接限额面，而高扇出监控的正确
  答案是 Webhook。URL 空间 `/v1/executions/{id}/events*` 保留为 internal 永不他用；重评触发
  条件：出现"单 session 并发多 execution 且消费方只关心其一"的真实外部需求。
- 新增租户级 **Webhook**（server-to-server）：从 Outbox 投影选定事件（turn 终态、approval 挂起、
  execution failed、session suspended 等），HMAC-SHA256 签名 + 时间戳防重放、指数退避重试、
  死信可查。
- **thin payload 决策**：webhook 只携带事件 ID/类型/sequence/资源 ID，消费方回读 API 取详情。
  理由：签名 URL 外发的内容最小化，天然绕开 interaction redaction 的投影复杂度，也符合
  "Credential、Prompt 不出现在非预期通道" 的既有安全基调。

### D6 版本与弃用政策

- `/v1` additive-only；破坏性变更进 `/v2`。`public-beta` 端点显式标注可变。
- 弃用流程：`Deprecation`/`Sunset` 响应头 + changelog + ≥ 12 个月窗口。
- SDK 走 semver：major 跟随 API 破坏性变更，minor 增能力；每个 SDK 版本声明其兼容的最低
  控制面版本。
- 公开事件面版本化沿用 runtime-event schema 的版本机制。

### D7 DX 闭环与验收线

- **文档站**：API reference 由 OpenAPI 生成；quickstart、认证/幂等/流式专题、cookbook。
- **Examples 仓库**：至少三个对准目标用户的完整示例——CI 失败自动修复 bot、PR review bot、
  批量代码迁移脚本。
- **Console**：Web 后台 API Key 管理页（创建、scope、轮换、吊销、last-used、用量）。
- **发布工程**：npm/PyPI 发布流水线；SDK conformance suite 对真实控制面运行并进 CI；
  两语言共享同一套 conformance 用例定义。
- **CLI**（thin wrapper）后置为可选项，优先级低于双语言 SDK。

### D8 免费层与试用形态

平台的免费层杠杆与一般 SaaS 不同：LLM token 成本通过强制 BYOK（用户自带 Provider API Key）
完全转移给用户，平台边际成本只剩 Worker 计算，因此免费层可以给得慷慨而不烧钱。

- **免费层**：强制 BYOK、1–2 并发 session、每月约 20 execution-hours（标准：够做出可演示的
  真实项目，对齐 E2B 式 PLG）、shared pool/batch lane、7 天 session/artifact 保留、无信用卡。
- **付费 dev 层**：按 execution-seconds 计费（`requested-resource-seconds` 计量现成）；
  interactive lane（fast-provision 热池）是付费差异化点，与方向评估的双 lane 天然对齐。
- **企业层**归 Stage 6，不在此展开。
- **上限单位**：并发 session + execution-seconds；不用 turn 数或 token 数（token 是 BYOK 的
  事，turn 成本方差太大）。
- **执行面复用**：`tenant_quotas`、fair-share 调度、M1 的 per-key rate limit；不新建配额机制。
- **时序**：自助免费层依赖 Stage 6 注册；之前用 waitlist/人工开通，但配额形状按本节冻结，
  M1 的用量归因从第一天就按此模型打点。价格页与售卖流程仍归 Stage 6 商业化。

## 4. 非目标

- 不公开 Worker Protocol / Provider Host Protocol 作为 API 承诺（BYO target 用户拿到的是安装物
  与 execution-targets API，协议兼容性由平台内部管理）。
- 不做自定义 Provider/Runner 伙伴 SDK（Stage 7 之后单独评估）。
- 不做浏览器直连 SDK、不开 CORS、不改 cookie 安全模型。
- 不承诺 GraphQL/gRPC 表面；Worker Protocol 未来演进（契约保留 gRPC 可能性）与本 Stage 无关。
- 不在本 Stage 建设计费商务侧（价格页、套餐售卖）——用量计量对接 Stage 6，商业化政策归 Stage 6。

## 5. 里程碑与交付顺序

- **M1 API 产品化**：allowlist 三级分级 + 守卫测试；一致性收口（分页/无界列表）；OpenAPI SSOT
  - CI conformance；Service Account 资源 scope + bearer 进租户路由；per-key rate limit 与用量
    归因。出口：外部用 curl + API Key 能走通 create session → send turn → SSE → approval 全流程。
- **M2 TypeScript SDK beta + 文档站 + examples**。出口：quickstart ≤ 5 分钟验收线达标。
- **M3 Python SDK + Webhook + Console key 管理页 + BYO target 接入流**。出口：双语言
  conformance 绿；webhook 投递/重试/死信演练通过。
- **M4 GA**：版本与弃用政策公示；self-serve onboarding 接 Stage 6 的 tenant 注册；SLO 引用
  Stage 6 的对外承诺。出口：完成条件全绿。

M1–M2 只依赖 Stage 2 级别的 API 稳定性，可与 Stage 4–6 剩余项并行；M4 的 self-serve 与计量
依赖 Stage 6 对应条目。**M4 GA 另有一条硬门槛：Stage 5 的沙箱隔离加固必须先完成**——公开
SDK 意味着任意第三方提交任意代码，见下节。

## 6. 与其他 Stage 的关系

- **Stage 6**：其 "User、Service Account、Credential 和 API Token 的生命周期管理" 条目由本
  Stage 具体化为资源级 API Key；自助注册、配额计费、文档制度是 M4 的门槛依赖；两个 Stage 的
  文档条目共建不重复。
- **Stage 5（硬依赖）**：公开 SDK 把"运行任意第三方代码"从内部假设变成产品承诺，因此沙箱
  隔离加固是 GA 的前置条件而非并行项。特别是共享 Worker 的跨租户残留、资源上限缺失与云元数据
  端点可达——在只有第一方消费者时是可控风险，在自助注册的公开平台上是不可接受的。免费层
  （D8）会放大这一点：低门槛注册意味着攻击者获取执行环境的成本接近于零。
- **Stage 4**：执行平台的可靠性与调度语义是 SDK 对外承诺的底座；本 Stage 不新增执行面需求。
- **方向评估 / fast-provision**：SDK 是"更多人进来"的通道，fast-provision 是"进来后留下"的
  体验。`sessions.create` 的参数设计为 interactive/batch lane 与 capacity class 预留前向兼容
  位，不阻碍 [fast-provision 提案](fast-provision-runtime-proposal-v0.md) 落地。
- **Roadmap-wide rules 全部适用**：跨进程命令幂等/版本化/可审计；前端不直连 Worker；公开面
  变更同样受 migration/协议兼容矩阵约束。

## 7. 已决策记录

设计冻结时的全部开放问题已于 2026-07-26 决策完毕：

- codegen 供应链 → 自建管线（并入 D4）。
- SCIM Service Account 与资源 API Key → 同一实体分 scope（并入 D3）。
- 公共包命名 → 品牌定名 **Polaris**，首选坐标 npm `@polaris-agents/sdk`、PyPI
  `polaris-agents`（并入 D4；npm 裸名 `synara` 与 `polaris` 均已被占是命名动因）。
- execution 级独立事件流 → 不做，session 流 + `executionId`（required 字段）过滤为正式方案
  （并入 D5）。
- 免费层形态 → 强制 BYOK + 平台计算配额（新增 D8）。
