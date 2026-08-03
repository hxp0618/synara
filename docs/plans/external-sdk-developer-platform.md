# Stage 7：对外 SDK 与开发者平台

状态：TODO（设计已冻结 2026-07-26，待实施）

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
