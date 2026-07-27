# Fast Cloud Agents 实现计划（worktree: fast-cloud-agents）

目标：在现有 Stage 4 逻辑之上，把"拉起一个远程 agent"的交互路径压到秒级。参照物：E2B（秒级
sandbox 分配）与 kubernetes-sigs/agent-sandbox（Sandbox/SandboxTemplate/SandboxClaim/
SandboxWarmPool CRD + 控制器模式）。设计蓝本：
[`fast-provision-runtime-proposal-v0`](fast-provision-runtime-proposal-v0.md)。

基线：分支 `fast-cloud-agents`，基于 `codex/saas-tenancy-user` @ `4e30436c`。

## 0. 概念对齐（agent-sandbox ↔ Synara 现有抽象）

| agent-sandbox       | Synara 现有物                                          | 差距                     |
| ------------------- | ------------------------------------------------------ | ------------------------ |
| Sandbox（稳定身份） | Worker incarnation + 逻辑 Worker + Session Workspace   | 无——已有更强的 fencing   |
| SandboxTemplate     | Worker Release Revision + immutable Manifest           | 无                       |
| SandboxWarmPool     | `worker_pools.mode=warm` + Migration `000069` 容量权威 | **软偏好 → 硬保证**      |
| SandboxClaim        | Execution Claim（fairqueue + release-aware 匹配）      | **claim 后仍有长尾启动** |
| hibernation/resume  | suspend/resume（checkpoint + receipt）                 | 恢复介质慢（对象存储）   |
| gVisor/Kata 隔离    | Pod SecurityContext + 受保护 cgroup supervisor         | 后续可选运行时层         |

结论：不需要新领域模型；实现 = 把 warm 从"软偏好"升级为"硬保证"，并把 claim 后启动链路中
可以前置的部分全部前置。

## 1. 增量顺序（每个增量独立可合并、可验证）

### 增量 A：秒级 SLO 门禁（已完成，比原计划更便宜）

- 勘察结论：`provision_tier` 不需要新迁移——Migration `000059`/`000081` 已把
  `warm_pool_mode` + `warm_pool_result`（`pending | not-requested | hit | fallback`，常量在
  `internal/executions/generation_facts.go:18-21`）固化进 Generation fact，且
  `synara_execution_cold_start_duration_seconds_30d` 分位数 gauge 已携带这两个标签
  （`internal/observability/generation_fact_metrics.go:193-195`）。tier 语义即
  `hit`=warm 命中、`fallback`=请求 warm 但冷启动、`not-requested`=cold。
- 交付（零迁移、零 Go 代码）：`deploy/kubernetes/monitoring/prometheus-rules.yaml` 新增
  `synara-control-plane.fast-provision-slo` 规则组：
  - `SynaraWarmHitColdStartSLOBreach`：warm 命中的 dispatch→Provider-ready P95 > 2.5s 持续
    15 分钟即告警——交互式秒级 SLO 的第一道硬门禁；
  - `SynaraWarmPoolFallbackSurge`：30 天窗口内 warm 请求的 fallback 占比 > 20% 持续 30 分钟
    告警——暖池容量不足信号。
- 后续把该 SLO 写入 Stage 4 完成条件属主分支文档改动，合并时一并提。

### 增量 B：`guaranteed-warm`——min-idle 硬补齐

- Pool 配置新增 `minIdleUnits`（0 = 现状软语义，>0 = 硬保证）；Kubernetes Reconciler 每轮把
  ready-idle 缺口视为必须立即补齐的目标，补齐失败/持续缺口产生 bounded 告警指标
  `synara_worker_pool_warm_deficit{capacity_class}`。
- Placement 对 interactive 请求：warm miss 时不再静默冷启动，而是（可配置的）短暂等待补齐窗口
  （bounded，默认 0 保持兼容）。
- 交付：Pool schema/配置扩展 + Reconciler 补齐逻辑 + deficit 指标 + Prometheus 告警规则。

### 增量 C：Provider Host 预启动（warm worker 在 claim 前完成 Describe）

- 现状假设（待勘察确认）：agentd 在 claim 后才启动 Provider Host 进程并 Describe。
- 目标：warm-pool 模式的 agentd 在注册后、claim 前就启动 Host 进程到 `Describe` 完成并缓存
  capability 描述；claim 后仅注入 Grant 凭证（FD 3 路径不变）+ StartSession。
- 安全边界不变：凭证仍在 claim 后 resolve；预启动的 Host 无凭证、无 Workspace、无 Session 状态。
- 交付：agentd warm 模式改造 + Host 生命周期管理（claim 超时/release 轮换时回收预启动进程）。

### 增量 D：Workspace cache-first（Grant 放行 + freshness 标注）

- Claim 放行条件从"网络 fetch 成功"改为"Grant resolve 成功"；网络 fetch 转后台，Session Event
  携带 `workspaceFreshness`。按 proposal §3。

### 增量 E：snapshot-restore 层（spike 调研已完成，实施为独立后续项目）

选型 spike 结论（2026-07-26，基于 agent-sandbox `docs/api.md` 实证核对）：

- **agent-sandbox：不采用（监测即可）。** 其 API 无内存快照——`operatingMode: Suspended` 是暂停/
  缩零 + PVC 保留（"Underlying resources (Pods, Services) are always deleted on expiry"），无延迟
  承诺；`SandboxWarmPool`（`replicas` + `Recreate|OnReplenish`）≈ 增量 B 已原生实现的保证池，
  引入只会在我们的 Worker/fencing/Claim 权威之下再叠一层重复的控制器与 CRD 依赖，且拿不到
  E2B 式内存 fork 收益。其 CRD 面可作为未来对外集成表面观察。
- **Kata on K8s：不满足。** VM 级隔离可得，但 Pod 内存快照恢复非生产能力（VM template/factory
  只加速冷启动）；K8s 自身的 kubelet checkpoint API 是 forensic 单向，restore 未接线。
- **结论：真正的亚秒 resume 走 Firecracker 直管**——E2B 的实际架构。落法：新增 `firecracker`
  Execution Target kind，跑在专用 Linux 宿主机上，**复用 SSH-managed Target 的安装/升级/撤销
  机制与受保护 supervisor 的安全谱系**；每 Session 一个 microVM，suspend = Firecracker snapshot
  （本地 + 异步对象存储兜底），resume = snapshot restore（目标 P95 ≤ 500ms）。Worker Protocol/
  fencing/Grant/Checkpoint 语义全部不变（提案 §2 的 `microvm-snapshot-v1` completion mode）。
- **定位**：K8s warm 池（A-D，已完成）承载通用车队的秒级启动；Firecracker target（E）作为
  premium 瞬时层。E 是项目级工作量（新 target driver + 快照加密/驻留合规评审），独立立项。

## 2. 代码锚点（勘察完成）

> 迁移链尾为 `000085_agent_executions_claim_indexes.sql`；新迁移从 **`000086`** 起。

- **Pool 模型**：`migrations/000058_worker_pools_placement.sql:6-12`（mode/capacity/desired_idle/
  max_active）；`internal/persistence/placement_models.go:19`；warm 容量权威表
  `migrations/000069_worker_pool_warm_capacity.sql`，其 `:49-98` scope 触发器**断言 authority 行与
  pool 行的 desired/max 一致**——新增列必须同步扩展。
- **Reconciler warm 路径**：计划函数 `kubernetes_warm_pool.go:316-353`（`desiredTotal =
DesiredIdleUnits + claimed`，clamp Max）；创建循环 `kubernetes_reconciler.go:900-923` **位于冷
  执行 Pod 循环（`:826-899`）之后**且受 `MaxActivePods` 截断——软语义根因。warm 槽驱逐
  `:748-807`（`warm-pool-demand-fallback`）。授权发布仅在全 pass 成功后（`:308-328`）。
- **Placement**：`internal/placement/service.go:445-520` `selectExecutionFromPolicy`；fallback 序
  `:461-478`；warm 偏好测试 `:499-514`；**静默冷回退 `:516-518`（无信号）**。注意该函数在
  launch 事务内持锁——不能睡眠等补齐。
- **Claim**：warm 匹配 `internal/executions/lifecycle.go:207-224`；fairqueue warm tiebreak
  `claim_fair_queue.go:23-26`；release 过滤 `workerreleases/scheduling.go:141-151`。
- **agentd**：Host 严格在 claim 后启动（`daemon.go:160` Claim → `:238` runExecution →
  `provider_host_v2.go:236` start）。凭证经 FD-3 **惰性写入**（`:982-989`）——预启动持 FD 不写
  即可行；无凭证 Describe 路径已存在（`:193-226`）。预 claim Host 的 cgroup 命名需占位方案
  （execution ID 未知）。
- **Generation facts**：`deriveWarmPoolResult` `generation_facts.go:80-88`；claim 时写结果 `:209`；
  写一次触发器 `migrations/000059:188-191`；rollup 复合主键含双 warm 标签
  （`metric_rollup_models.go:43-52`、`000081:16,25-52`）。
- **Config**：pool 尺寸仅 API（`placement_api.go:35,61`）；`maxActivePods` 来自 Target 加密配置
  （默认 50，`kubernetes_reconciler.go:1634`）；`SYNARA_AGENTD_WORKER_MODE`（agentd
  `config.go:264-283`）。

### 增量 B 设计决策（已定）

- **D1**：`worker_pools.min_idle_units INTEGER NOT NULL DEFAULT 0`，
  `CHECK (0 <= min_idle_units AND min_idle_units <= desired_idle_units)`——min-idle 是 desired 内的
  "保证下限"，不是独立目标；plans 算术不变，只改优先级/豁免/亏空核算。镜像进 warm 容量权威行并
  扩展 `000069` scope 触发器与 SQLite 对应约束。
- **D2**：Reconciler 重排——每个 warm pool 的**前 `min_idle_units` 个槽（slot 0..min-1）在冷执行
  Pod 循环之前创建**（保证槽），其余 warm 槽保持现状（冷循环后、best-effort）。槽名稳定，保证
  身份稳定。
- **D3**：`warm-pool-demand-fallback` 驱逐只允许命中 slot index ≥ min_idle_units 的槽。
- **D4**：保证需求超出 `MaxActivePods` 时不 fail closed：按预算截断并计入亏空（fail-visible）；
  API 层只保留 D1 的列级 CHECK（maxActivePods 在加密 Target 配置内，API 不解密）。
- **D5**：新指标 `synara_worker_pool_warm_deficit{capacity_class}` =
  Σ max(0, min_idle − ready_idle)，从权威行投影（`desired_idle_units` 已在权威行但未导出，一并
  导出 min）；Prometheus 告警：deficit > 0 持续 10 分钟 warning。
- **D6**：placement 不在事务内等待（勘察确认持锁）；interactive 等待补齐留给未来在事务外的
  重试形态，本增量不做。

## 3. 验证策略

- 每增量：Go focused test + 现有 PostgreSQL/SQLite 双栈测试模式；增量 B/C 需要 OrbStack 真实
  Kubernetes 验收 lane（复用 stage-4 已有 runner 模式）。
- SLO 证明：增量 A 的 tier 分位数在增量 B/C 落地前后对比，目标 warm 命中 P95 ≤ 2.5s。
- 遵守 TODO Roadmap-wide rules：迁移号跨分支唯一（新迁移从当前链尾 `000084` 起，合并前复核）；
  docs/reports 证据不可变；sweep 唯一权威。

## 4. 进度

- [x] worktree 创建（`fast-cloud-agents` @ `4e30436c`）
- [x] agent-sandbox 概念对齐
- [ ] 代码勘察（进行中；A 所需锚点已自查完成）
- [x] 增量 A（SLO 告警门禁，`prometheus-rules.yaml`）
- [x] 增量 B（已验收：codex gpt-5.6-sol 实现，18 文件 +452/-69；D1-D6 逐条核对——保证槽先于冷
      Pod 消费预算、双 pass 驱逐豁免、`000086` 迁移 + 双栈 scope 断言、
      `synara_worker_pool_warm_deficit` + `SynaraWarmPoolMinIdleDeficit` 告警、契约文档同步。
      独立验证：`go build` + 六包 `go test` 全绿。报告 `.codex-report-increment-b.md`。
      PostgreSQL 集成验证已补：disposable PostgreSQL 17 上
      `TestPostgresWorkerPoolWarmCapacityRejectsScopeAndMutation` RUN+PASS（0.63s）。
      live OrbStack lane 留给合并前验收。）
- [x] 增量 C（已验收：6 文件 +262/-55 + 新增 `provider_host_prestart.go`/测试。C1-C6 逐条核对
      ——warm-pool + 无受保护 root 门控、FD-3 采纳时才写入（`deliverCredential`/
      `closeWithoutWrite` 拆除路径零泄漏）、SHA-1 合成 prestart 身份、单槽生命周期 + 3 次
      100/200ms 退避、`configureProviderHostV2ForExecution` 共享校验（采纳/回退同一套检查、
      不发第二次 Describe）、五种 bounded outcome 日志。独立验证：`go build` +
      `go test -count=1 ./internal/agentd/...` 全绿（23.5s）。报告 `.codex-report-increment-c.md`。）
- [x] 增量 D（已验收：workspace.go/workspace_cache.go/config.go/daemon.go + 三个测试文件 +
      契约同步。D1-D6 核对——`daemon.go:634` Grant resolve 先于物化且 fail-closed（专属测试证明
      新鲜缓存也被门禁）、窗口默认 0 逐字节兼容且 >1h 拒绝、marker 原子替换 + 锁内读写、
      后台刷新复用本次凭证并在 runExecution 返回前 cancel+join、`fallback-after-skip-doubt`
      存疑回退、零 wire 变更。独立验证：全套 `go test -count=1 ./internal/agentd/...` fresh 通过
      （32.2s，覆盖 codex 沙箱受限的 loopback 测试）。报告 `.codex-report-increment-d.md`。）
- [x] 增量 E spike（调研完成：agent-sandbox 无内存快照、Kata 不满足亚秒 resume；结论 =
      Firecracker 直管作为独立后续项目，详见 §1 增量 E 节）
- [x] 迁移号冲突处置（2026-07-27）：主分支合并 `cloud-agents-optimization` 后出现
      `000086_worker_reconciliation_drains.sql`，与本分支撞号。按 roadmap 规则本分支（未部署侧）
      已改号为 **`000087_worker_pool_min_idle.sql`**；代码/测试零硬编码引用，SQLite 全包 +
      disposable PostgreSQL 17 集成测试改号后复验 PASS。本分支迁移链 85→87 留洞合法
      （`readMigrations` 只查重复不查连续），合并时主分支 `000086` 自然补位。
      `.codex-report-increment-b.md` 保留历史原文（报告忠实记录当时创建的是 000086）。
- [ ] 合并前：live OrbStack Kubernetes 验收 lane + 提交/PR（待操作人指示）。
      合并风险监测（e87cc840 时点）：主分支 14 个新提交中唯一触碰本分支修改文件的是
      `f7dafe10`（reconciler foundation map 加锁，结构体字段区）——与本分支的 warm 循环改动
      不重叠，预计干净合并；agentd 包的其余主分支改动均在本分支未动的文件。
      CI 就绪监测（82a64f80 时点）：主分支 CI 已新增全模块 race、govulncheck、staticcheck
      （SA*/S1*/QF1\*）门禁。本分支预检：四个增量包 `-race -count=1` 全绿；staticcheck 仅报
      2 处 `tar.TypeRegA`——均在本分支**未修改**的基线文件且主分支已修复（TypeRegA→TypeReg），
      合并即自动继承，本分支新代码零发现。提交后先 merge 主分支再跑 CI 即绿。
