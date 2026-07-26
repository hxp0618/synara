# Cloud Agent 文档审计报告（2026-07-26）

范围：stage-4 计划、`docs/contracts/` 全部 22 份活跃契约、3 份 runbook、`docs/worker-image.md`、
3 份 ADR、2 份 release-checklist、stage-2/3 历史计划与 drift-audit。目的：为后续项目重构提供一份
文档与实现一致性的基线，以及重构时必须处理的系统性文档设计问题清单。

所有修改都落在 `cloud-agents-optimization` worktree。交叉核对以当前实现为权威：HTTP 路由
（`internal/httpapi/server.go`）、权限（`internal/authorization/permissions.go`）、指标
（`internal/observability/`）、配置（`internal/config/config.go`）、迁移目录与部署脚本。

## 一、已直接修复的问题

### 自相矛盾（实现问题）

1. `docs/plans/stage-4-distributed-execution-resource-lifecycle.md`：§1 仍称 Worker incarnation 资源
   时长"尚不是最终预聚合形态"，而同文档 §8 与 TODO.md 记录 Migration `000079`/`000081` rollup 已完成。
   已修正。
2. `docs/contracts/workspace-v1.md`："SSH repositories remain blocked" 与同文档 Implemented boundary
   及 `worker-protocol-v2.md`、代码 `internal/agentd/git_ssh_agent.go` 矛盾。已改写为已实现语义。
3. `docs/contracts/execution-target-v1.md`：结尾 "Stage 3 remains partial" 与 TODO.md 的 Stage 3
   已按收窄边界验收关闭矛盾。已改写并钉住 runtime SHA `8415efa1`。

### 格式损坏导致语义错误（实现问题）

4. `docs/contracts/global-target-routing-dr-v1.md`：queue-pressure 负载公式 `(ceiling * memberWeight)`
   被格式化器拆成列表项，读作减法；已按 `routing/service.go` `effectiveLoadRank` 修复为乘法，并修复
   反引号粘连。全库扫描确认无同类损坏。

### 契约缺位（设计问题）

5. `docs/contracts/provider-credential-v2.md`：Generation-scoped opaque Grant（Migration `000049`，
   `execution_provider_credential_grants`，resolve 路由）与短期访问授权（Migration `000055`）在契约层
   零记录，只存在于 stage-4 计划叙述。已补两个小节。
6. `docs/contracts/role-permission-matrix.md`：代码 41 个权限中 `scheduling_policy.*`、`lifecycle.*`、
   `quota.*`、`retention.*`、`identity.*`、`service_accounts.*`、`artifact.*` 七组在矩阵缺行；
   Credentials 行把 tenant admin 写成 no（实际有 `credentials.use`）。已按 `permissions.go` 逐格补齐
   租户表 + 组织表，并声明矩阵是代码的文档投影。
7. `docs/contracts/control-plane-observability-v1.md`：缺已上线的
   `synara_execution_queue_depth` / `synara_execution_queue_oldest_age_seconds`。已补。
8. `docs/contracts/reconciler-leader-election-v1.md`：控制器清单缺 `synara:metric-rollup` 与
   `synara:billing-shared-allocation-scheduler`（代码共 9 个持久 lease）。已补齐并改为精确 lease 名。

### 状态/锚点过时（实现问题）

9. `docs/contracts/tenant-retention-v1.md`：只写 advisory lock，实际是持久 lease + advisory lock 双层。
   已修正并交叉引用。
10. `docs/contracts/deployment-profile-v1.md`："future OIDC/SAML/SCIM" —— 已实现。已修正并链接
    `enterprise-identity-v1`。
11. `docs/runbooks/control-plane-operations.md`："迁移已连续到 `000041`"（实际 `000081`）。已改为以
    `/ready` expectedVersion 为权威。
12. `docs/runbooks/worker-release-rollout.md`："本实现边界为 `000042`" 同类问题，同样修复。
13. `docs/contracts/workspace-v1.md`："DDL source of truth through `000027`" 锚点过时，已改为
    区间 + 持续扩展表述。
18. `docs/contracts/provider-host-v1.md`：三份冻结基线中唯一缺 legacy 横幅的文档，已补
    （详见第二节基线条目）。
19. `docs/plans/stage-2-go-control-plane-productionization.md`：状态节增加"已完成/已验收 +
    文中'当前'为收口时点"声明，"当前 Schema"改为"收口时 Schema"。
20. `docs/plans/stage-3-provider-runtime-remote-worker-productization.md`：标题下增加 COMPLETE
    横幅（2026-07-24 收口、runtime SHA `8415efa1`），声明文中 checkpoint/`partial`/migration
    boundary 均为时点快照。
21. `docs/plans/stage-3-drift-audit.md`："Current baseline" 改为 "Audit baseline" 并加历史记录
    横幅，覆盖文中多处 "`partial`"与 "boundary 仍为 `000041`" 的时点性。

22. `docs/plans/saas-tenancy-organization-user-plan.md` §5.3：Permission 清单停留在 Phase 0-3 的
    29 个初始值（代码现有 48 个）。按单一来源原则未补第三份全量副本，改为标注时点性并指向
    `permissions.go` 与 `role-permission-matrix.md`。§5.1/5.2 的 6 租户角色、5 组织角色与代码一致；
    文档头部 Phase 0-6 状态声明与 TODO.md 一致。

迁移号全库清扫结论：runbook/checklist 的现行断言已全部修复（第 11、12 项及 stage-2 检查单
自带的正确写法）；剩余硬编码迁移号全部位于上述已加历史横幅的关闭阶段文档内，属合法时点
快照。stage-4 计划中的 "schema 73/73" 等数字是 final 报告的验收观测值，同样合法。

### 结构与可读性（可优化问题）

14. stage-4 计划 §1 的 100 行追加式状态流水账重构为 §1.1 主题分组摘要（挂起恢复 / 安全凭证 /
    调度路由 / 成本指标 / 仍开放项）+ §1.2 本地验收证据，内容零丢失。
15. stage-4 计划 §2 状态图由误导性线性箭头链改为显式转换关系。
16. `docs/contracts/cloud-cost-accounting-v1.md`：intro "three durable domains" 计数错误且遗漏
    shared-Target 分摊三表。已修正。
17. `docs/contracts/execution-target-v1.md`：Kinds 表与 Managed Kubernetes 节补 Warm/general pool
    交叉引用（`worker-pool-placement-v1`）。

## 二、核对通过、明确不改的文档

- `managed-cloud-billing-acceptance-v1`、`worker-protocol-v2`、`provider-host-v2`、
  `session-execution-state-machine`、`execution-scheduling-policy/decision-v1`、
  `worker-pool-placement-v1`、`agentd-protected-cgroup-supervisor-v2`、`agentd-runner-v1`、
  `outbox-delivery-v1`、`artifact-v1`、`audit-log-v1`、`enterprise-identity-v1`、
  `production-authentication-policy`、`saas-api-conventions`、`runtime-event-v2`、
  `vault-kms-operations`、`worker-image`：抽查的路由、权限、错误码、指标、环境变量、常量、
  文件引用（约 120 项）全部与实现一致。
- 3 份 ADR：按 ADR 惯例是不可变决策记录，其中的 "Phase 5-6 remain future work" 等表述是决策时点
  快照，不应改写。
- `stage-3-provider-runtime-remote-worker.md` 检查单：closure banner 明确声明旧 checkpoint 文字为审计
  历史（含 "Provider Host Protocol 固定为 2.1"——那是 Stage 3 关闭时点的事实，2.2 属 Stage 4）；
  历史发布证据不改写。
- `deploy/saas/README.md` / `deploy/kubernetes/README.md`：与 enterprise-identity-v1（SAML metadata
  端点）、kubernetes-resilience-acceptance-v1（hook 环境变量、managed-hook 控制器、证据 sidecar）
  完全同步。
- `deploy/kubernetes/monitoring/prometheus-rules.yaml`：14 条告警覆盖 observability 契约声称的全部
  8 类（另含错误率/延迟/后台失败/SSE lease 四条契约未逐一列名的告警，契约用词是列举而非穷尽，
  不构成失真）。
- `canary.md`、`server-architecture-migration.md`：本地产品/TS 服务端重构文档，不属云平台契约；
  后者恰是未来重构的入口清单，保持原样。
- 冻结基线 `worker-protocol-v1`、`provider-credential-v1`：自带 legacy 横幅指向 v2，全文与各自 v2
  delta 的 "remain unchanged unless this delta says otherwise" 声明一致，保持不动。
  `provider-host-v1` 原本**缺少**版本横幅（读者会误以为 `codex exec --json` 一次性 runner 是现行
  契约），已补上与另两份同风格的 legacy 横幅并注明 `SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v1`
  开关边界——计入修复第 18 项。

## 三、重构时必须处理的系统性问题（未在本轮修改）

1. **三份互相漂移的状态副本**：TODO.md Stage 4、stage-4 计划 §1、计划 §8 记录几乎相同的
   migration/证据信息，本轮修掉的多处自相矛盾均源于此。建议重构后 TODO.md 每项一行状态 + 链接，
   细节单一来源于计划文档。
2. **契约把 Migration 编号当规范锚点**：`cloud-cost-accounting-v1` 等契约以 `000075/000077/000080`
   作为语义标识。重构若重排迁移，所有契约同时失真。建议契约只描述不变式，把迁移编号收进
   "实现附录"小节或独立索引。
3. **易变事实硬编码**：本轮修复的 4 处过时迁移号/状态只是抽样；重构前建议对 docs/ 做一次
   "具体数字/版本号必要性"清扫，能引用权威端点（如 `/ready` expectedVersion）的一律改引用。
4. **`docs/repo-scan-2026-04-16/` 存档**：30 个死链（另一台机器绝对路径）。已处理（修复第 27 项）：
   在目录内新增 ARCHIVED README，声明时点性、坏链事实与可安全删除性（已验证无目录外引用）；
   是否物理删除留给操作人决定。
5. **分支代码问题（非文档）**：`bun typecheck` 在 `@synara/provider-host` 失败——
   `providerHost.ts:745-761` memoryDocuments 字段推断为 `unknown`、`protocol.ts:611` payload 无
   `error` 属性、测试引用超出联合类型的 `"resume-with-recovery-metadata"`。该漂移存在于已提交代码，
   与文档无关，重构前必须先修。
6. **角色矩阵同步纪律**：`role-permission-matrix.md` 已声明为 `permissions.go` 的投影；重构时可考虑
   由测试生成/校验该矩阵，杜绝再次漂移。

## 三点五、hxp0618 提交文档的扩展审核

操作人要求对所有 `hxp0618` 提交过的文档（148 个 .md：60 非报告 + 88 验收报告）补充审核。
非报告集与既有审计几乎重合，新增覆盖：

- `deploy/billing/README.md`：计费 acceptance lane 描述与 cloud-cost-accounting 契约一致；三个必跑
  测试函数与三个强制负例子测试均在 `internal/billing` 中存在，脚本存在。
- `deploy/kubernetes/monitoring|security|security/registry|security/vault/README.md`、
  `deploy/personal/README.md`：与 observability 契约、vault-kms-operations runbook、worker-image
  签名章节、deployment-profile 逐项一致；vault README 明确声明具体地址是 `kind-synara-stage3-prod`
  专用、生产必须重渲染——与 runbook 同一口径。
- 已删除的 `agentd-protected-cgroup-supervisor-v1.md` 与旧位置 `docs/saas-tenancy-organization-user-plan.md`
  确认为被取代后的正确清理。
- **88 份验收报告敏感信息扫描通过**：AKIA/私钥块/Bearer/presigned 签名/带密码连接串等模式仅两处
  命中，均为误报（`postgres://synara:***@`——密码已脱敏；prose 斜杠被误判 base64）。runbook 的
  sentinel 扫描纪律有效。
- `REMOTE.md`（Phase 1 单机远程 runbook）：审核通过，自洽、限制诚实、引用齐全。
- `scripts/stage3-provider-acceptance/README.md`（1326 行）：审核通过。生产值与
  vault-kms-operations/worker-image 完全一致（`kind-synara-stage3-prod`、Registry/Vault 镜像钉扎）、
  Provider 版本与 lockfile 一致、"8 Provider × 28 Capability" 目录与 CLAUDE.md 的 9 Provider 减去有意
  排除的 droid 一致；引用的 5 份 stage-3 报告、fixture/测试文件、7 个 gate 脚本共 13 项全部存在。
  各 suite 的证据边界声明（"deterministic ... not real Provider/production"）是全文档集中最严谨的。
- 根 `README.md`：**修复第 24-26 项**——(24) "schema chain currently ends at `000041`" 过时锚点
  （硬编码反模式第 5 例）改为指向迁移目录 + `/ready` 权威；(25) "Productionization and the Web
  main-flow authority cutover remain in progress" 与 Stage 2/3 已验收矛盾，改为 Stage 2/3 closed、
  Stage 4 in progress；(26) Provider 列表补上缺失的 Factory Droid（9 个 Provider 只列了 8 个，
  contracts 中 droid 存在）。引用文件（CONTRIBUTING/截图/external-mcp）齐全。
- hxp0618 提交文档范围（148 个 .md）至此全部审核完毕。

## 三点六、需要操作人裁决的上游分歧

主检出的未提交 TODO.md 在某轮修改中**删除了 Roadmap-wide rules 末尾的 4 条工程规则**（迁移号
跨分支唯一、`docs/reports` 证据不可变 + `.oxfmtrc` ignore 维护、sweep 唯一不节流权威、根
`package.json` overrides 优先级陷阱），且未移动到任何其他文档。这些规则事实核查为真
（`.oxfmtrc.json:11` 确实 ignore `docs/reports`；sweep 规则与 state-machine 契约的 throttle 段落
删除相互印证——规则是"原则"，契约删的是"过时实现描述"）。**worktree 保留这 4 条**，不采纳
删除；若主检出的删除是有意的，请操作人在合并时裁决。

裁决结果（已合并）：操作人裁定**保留这 4 条规则**。`worker-control-pull` 合并进本分支时，TODO.md 是唯一冲突文件，冲突已按该裁决解决——4 条规则全部保留，同时保留本分支在文件中部新增的内容。主检出未提交的删除未被采纳。

## 三点七、方向评估（重构准备的战略输入）

操作人确认产品北极星为 E2B 式"秒级远端 agent"。对照该目标的偏差分析、有效地基清单与按杠杆
排序的重构建议见独立文档
[`cloud-agent-direction-assessment-20260726`](../plans/cloud-agent-direction-assessment-20260726.md)。
核心结论：企业正确性地基有效，但冷启动仍是观察指标而非产品门禁；建议新增 snapshot-restore
运行时层级、Warm Pool 保证化、Workspace cache-first，并把秒级 SLO 写进 Stage 4 完成条件。

## 四、增量同步记录（审计完成后的上游变更）

主检出在审计期间继续产生未提交文档/脚本更新，本 worktree 已按轮同步：

- **采纳上游删除**：`session-execution-state-machine.md` 中 "Claim 热路径恢复扫描按 2s 节流、多数
  轮询跳过" 段落被上游删除。核对代码证实删除正确：`executions/lifecycle.go:36` 在每次 Claim 无条件
  调用 `RecoverExpired`，executions 包不存在 throttle——该段描述的是从未落地（或已移除）的机制。
- **同步上游新增**：namespace/RBAC 隔离验收 baseline（`SYNARA_K8S_NAMESPACE` 派生隔离
  ClusterRole/Binding、`SYNARA_K8S_ACCEPTANCE_RBAC_NAME`、run-owner 标签精确清理），涉及
  `kubernetes-resilience-acceptance-v1.md`、`deploy/kubernetes/`（README、acceptance.sh、
  deployment.yaml、resilience-acceptance.sh、validate-resilience-assets.py）、TODO.md、
  control-plane README、stage-4 计划开放项，以及
  `stage-4-orbstack-isolated-resilience-20260726-final1/final2.md` 两份证据（schema 81 镜像、3/3 顶层
  场景、120 秒 6/6 disruption soak、原 `synara-system` 保持 2/2）。同步后链接与脚本环境变量
  核对通过。
- **同步上游新增（Migration `000083`）**：signed platform routing publisher 落地（Ed25519 公钥配置
  `SYNARA_PLATFORM_ROUTING_PUBLISHERS_JSON`、`PUT /v1/platform/routing-authority/...` 路由、
  `synara.platform-routing-authority.v1` 签名 schema、immutable nonce receipt）。同步
  `global-target-routing-dr-v1` 与 4 个 deploy 配置，重放 queue-pressure 公式修复；stage-4 计划
  §1.1/仍开放项对应收口（"生产健康发布器"开放项改为"真实外部 Target probe"）。核对通过：config、
  migration `000083`、`server.go:192` 路由、两个新指标均在代码存在。**修复第 23 项**：上游
  observability 契约漏掉这两个新指标，worktree 已补录（与 queue 指标同类缺口）。后续上游又补充了
  `cmd/routing-authority-sign` 签名 CLI 的契约字段序与 README 操作指南（openssl 生成 PKCS#8 Ed25519、
  `--public-key-only` 导出、拒绝 symlink/宽权限 key 文件），已同步并验证工具源码存在。
  注意：上游 routing-dr 的 queue-pressure 公式段仍是损坏版本，每次同步后 worktree 都会重放乘法修复；
  分支合并时该段以 worktree 版本为准。
  另：`REMOTE.md`（Phase 1 单机远程 runbook）审核通过——自洽、限制声明诚实、4 个 deploy/remote
  引用文件齐全，与控制面文档无混淆（明确声明"不是分布式控制面"）。
- **同步上游新增（Migration `000082`）**：此前标记"保持待办"的 account-level actual invoice 分摊已
  落地——`cloud-cost-accounting-v1` 新增 "Shared Target actual-invoice allocation" 节与
  `.../actual-invoices/{invoiceImportID}/allocations` 路由、`managed-cloud-billing-acceptance-v1` 新增
  对应 E4 gate 子节、observability 新增三个 `synara_billing_shared_actual_allocation_*` 指标、
  TODO/README/stage-4 计划同步收口，证据
  `stage-4-shared-actual-allocation-orbstack-pg-20260726-final1.md`。核对通过：migration 文件、
  `httpapi/server.go:292` 路由、三个指标名、`shared_actual_allocation_models.go` 均存在。worktree 侧
  重放了本审计的 intro/queue-metrics 修复，并把 intro 扩展为同时列出 estimated 与 actual 两套
  分摊图的表。
