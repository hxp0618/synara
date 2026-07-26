# Cloud Agent 竞争格局评估：现方案优劣势（2026-07-26）

本文回答"当前方案相对市场处于什么位置"，与两份姊妹文档构成完整评估：
[`cloud-agent-docs-audit-20260726`](../reports/cloud-agent-docs-audit-20260726.md) 回答"文档与实现是否一致"，
[`cloud-agent-direction-assessment-20260726`](cloud-agent-direction-assessment-20260726.md) 回答"实现方向是否对准产品
北极星"。本文的优势/劣势判断全部对照 2026 年 7 月市场现状给出，不重复姊妹篇已有的内部诊断细节。

**证据口径。** 外部事实来自 2026-07-26 的三路并行调研（产品线约 28 个一手来源、基础设施线含
kubernetes-sigs/agent-sandbox 仓库级核对）加一次独立模型评审；关键来源见文末。延迟/规模数字多为厂商宣称
（vendor claim），未经独立复测的一律照此标注；社区口碑类结论标注为非受控样本。内部事实以当日
`codex/saas-tenancy-user` 工作树为准。本文为持续修订文档：第一至五节是市场评估（格局、事实、
优劣势、方向确认），第六节是对齐手册（可执行清单与设计建议），服务"把本项目 cloud agent 能力做到
与成熟产品同级的易用与稳定"这一目标；修订历史见文末。

## 一、市场三层格局与本方案位置

**产品层**（第一方托管 cloud agent）：Cursor Cloud Agents、OpenAI Codex Cloud、Claude Code on the
web 为第一梯队，GitHub Copilot coding agent、Devin、Google Jules、Factory 为第二梯队。共同特征：单一
vendor（模型与运行时不可换）、调度与放置完全黑盒、不可自托管。例外苗头：Cursor 的 BYO compute
pool（API beta）、Devin Outposts（2026-07，客户自有硬件）、Factory 的 Bring-Your-Own-Machine——产品层
玩家正在向"跑在你自己的机器上"下探。

**基础设施层**（sandbox 原语）：E2B、Daytona、Modal、Morph、Cloudflare、Fly、Vercel、Runloop 等，加
kubernetes-sigs/agent-sandbox 作为 K8s 官方标准化尝试。两个结构性事实：

1. **该层正在快速商品化。** 过去约 9 个月内四大云全部推出原生 agent sandbox：AWS Bedrock AgentCore
   Code Interpreter（2025-10 GA）、GKE Agent Sandbox（2026-05-21 GA，底层即 agent-sandbox）、Azure
   Container Apps Sandboxes（Build 2026 preview）、GCP Cloud Run sandboxes（2026-07 preview）。
   Anthropic 自家 Managed Agents 的自托管参考架构就跑在 GKE Agent Sandbox / gVisor 上
   （agent-sandbox PR #950）。
2. **该层全体不做产品问题。** 调研确认：编排/fleet 管理、git/PR 工作流、agent 运行时进程管理、
   多区域路由、面向最终用户的计费/租户，没有任何一家 sandbox 厂商提供——自建平台无论选哪家后端，
   这些都得自己建。

**本方案位置**：横在两层之间——上接 9 个 Provider 的产品语义（单一 Worker Protocol / Provider Host
协议），下接 Local/SSH/Docker/Kubernetes 四种执行通道，中间是完整 SaaS 控制面（tenancy、routing、
DR、billing、audit）。这个"多 Provider + 可自托管 + 企业正确性"的组合位当前没有正面竞争者；代价见
劣势节。

## 二、竞品关键事实速览

产品层（官方文档口径，均为 2026-07 现状）：

| 维度           | Cursor Cloud Agents                         | Codex Cloud                                     | Claude Code on the web                     |
| -------------- | ------------------------------------------- | ----------------------------------------------- | ------------------------------------------ |
| 隔离单元       | Firecracker microVM，独立 AWS 账户          | "隔离容器"（运行时未披露）                      | 整 VM（hypervisor 未披露）                 |
| 资源规格       | 未公布                                      | 未公布（社区长期抱怨）                          | 4 vCPU / 16GB / 30GB，无自定义镜像         |
| 网络默认       | 开（三档，企业可锁定）                      | agent 阶段默认关；域名白名单 + 仅 GET/HEAD 可选 | Trusted 白名单默认开；双代理               |
| Secrets        | 运行期在场（Runtime Secret 可脱敏转写）     | setup 阶段后移除，agent 阶段不存在              | 无独立 secrets store，环境变量明文可见     |
| 环境缓存       | VM snapshot / Dockerfile                    | 容器缓存 12h（团队共享失效）                    | 文件系统快照约 7 天                        |
| 并行           | 不限量并行不同任务；多 repo ≤20             | worktree 并行 + `--attempts` 1–4 best-of-N      | 并行不同会话；单 repo（头号用户抱怨）      |
| 触发面         | Web/iOS/Slack/Linear/GitHub/API，触发器最全 | Web/ChatGPT 移动端/GitHub/Slack/Linear/自动化   | Web/移动端/GitHub/GitLab CI/Slack/Routines |
| 已披露安全事件 | SSH 远程 subagent 静默回退本地（部分修复）  | 云容器分支名命令注入 CVE-2025-59532             | SOCKS5 白名单绕过横跨约 130 版本，静默修补 |

基础设施层（数字为厂商宣称，除注明外未独立复测）：

| 产品                | 隔离                                  | 冷启动宣称        | 快照模型                                             | 层位               |
| ------------------- | ------------------------------------- | ----------------- | ---------------------------------------------------- | ------------------ |
| E2B                 | Firecracker                           | ~150–200ms        | 内存+文件系统快照，fork ×100                         | 原始沙箱原语       |
| agent-sandbox (K8s) | RuntimeClass 可插（runc/gVisor/Kata） | GKE 层 P90 ~200ms | suspend 仅磁盘（Beta）；live hibernate 仅 GKE+gVisor | 标准/API，非产品   |
| Morph Cloud         | 整 VM                                 | 未公布            | Infinibranch：含进程态 live fork，宣称 <250ms        | 原始 VM 原语       |
| Modal Sandboxes     | gVisor                                | 未公布            | 仅文件系统快照；24h 生命周期上限                     | 平台内执行特性     |
| Cloudflare Sandbox  | Firecracker                           | 未公布            | 磁盘 backup/restore（2026-04 GA）                    | 原语 + 轻编排      |
| Fly Machines        | Firecracker                           | <300ms            | 整机 suspend/resume ≤4GB，明确不保证可恢复           | 通用 VM 计算       |
| Daytona             | 容器默认（Kata 可选）                 | ~90ms             | 磁盘/环境快照；核心开发 2026-06 转私有               | 原语 + Git/LSP API |

## 三、优势（结构性，按防御性排序）

1. **Provider 中立且协议级强制，全场唯一。** 产品层三巨头单一 vendor 是产品定义使然，改不了；市场
   已出现用户在 Codex 与 Claude 之间做"质量 vs 额度"套利的公开讨论（非受控样本）。本方案一个控制面
   同时驱动 codex app-server、claude-agent-sdk 等 8–9 个运行时，能力协商显式失败、绝不静默降级
   （[`provider-host-v2`](../contracts/provider-host-v2.md)）。企业买家规避模型商锁定的需求只有这一层
   能承接。
2. **一个领域模型贯穿 Personal → Enterprise、Local → Kubernetes。**
   [`ADR 0002`](../adr/0002-deployment-profile-execution-target-v1.md) 的
   DeploymentProfile × ExecutionTarget 正交拆分让 SQLite 单机与多副本企业控制面共享同一
   Tenant/Session/Execution/Generation 模型。托管产品做不了本地，sandbox 厂商做不了产品层，
   agent-sandbox 出不了 K8s；"桌面工具 → 自托管企业服务"的连续升级路径当前独占。
3. **执行语义正确性深度远超全场。** Generation fencing + 物理 incarnation 绑定 + 内容哈希幂等
   receipt + 不可变 Recovery Bundle + 完整候选轨迹调度决策图
   （[`execution-scheduling-decision-v1`](../contracts/execution-scheduling-decision-v1.md)）。对照：
   Codex 连资源上限都不公布；全部托管产品调度黑盒；Fly suspend 明确"不保证可恢复"；Copilot 有 59
   分钟硬顶。可解释、可审计、可重放的调度与恢复证据链是受监管租户与事故重建场景的独占能力。
4. **凭证模型比三巨头都精细。** Generation-scoped 不透明 Grant + 语义活动驱动的短期 access lease +
   撤销 fail-closed（[`provider-credential-v2`](../contracts/provider-credential-v2.md)）。对照：Claude
   Code web 无 secrets store；Cursor secrets 运行期在场；Codex 最强但 agent 阶段因此无凭证可用。
   对手履历（Codex CVE-2025-59532、Claude 约 130 版本的白名单绕过）说明该层即使巨头也难做对。
5. **真实的 DR/路由模型 + 成本守恒地基。** 产品层无一家暴露 DR 语义；本方案有 watermark 门禁跨域
   failover 与 lineage successor（[`global-target-routing-dr-v1`](../contracts/global-target-routing-dr-v1.md)）。
   计费侧调研确认全市场 cloud agent 均为"共享订阅池、零成本透明度"，本方案的真实云账单导入 + 共享
   成本守恒分摊对平台型买家（chargeback/showback）是潜在差异化——同时也是劣势 4 的过度建设项。
6. **供应链纪律超前于产品阶段。** Digest-pinned 可重复构建、SBOM、SLSA provenance、Cosign/Rekor、
   release/canary/rollback 身份（[`worker-image`](../worker-image.md)）已达企业采购问卷硬门槛。

## 四、劣势（按严重性排序）

1. **交互延迟与市场差 1–2 个数量级——产品成立门槛，非优化项。** 市场基线：E2B ~150–200ms + 内存
   fork；GKE 宣称 P90 200ms；Devin 首消息 ~10s、Outposts 热启动 ~90ms；三巨头全部用环境快照缓存
   （Codex 12h、Claude ~7 天）把冷启动藏掉。本方案 warm 为软偏好、每 Turn 强制网络 fetch、正确性
   机器叠满热路径，实际秒到分钟级——内部诊断见
   [方向评估](cloud-agent-direction-assessment-20260726.md)，解法见
   [`fast-provision-runtime-proposal-v0`](fast-provision-runtime-proposal-v0.md)（未实现）。市场数据
   只加重紧迫性，不改变诊断。
2. **隔离深度低于全场基线，堵死"跑不可信代码的多租户 SaaS"。** 现状为加固容器（非 root、只读
   rootfs、seccomp、无 capabilities）+ Linux 目标的 cgroup-v2 supervisor——进程级围栏。市场基线是
   microVM（Cursor/E2B/Cloudflare/Fly/Vercel）或至少 gVisor（Modal/GKE），Claude web 为整 VM。全部
   现行契约中不存在 gVisor/Kata/microVM 概念。可行解已现成：K8s RuntimeClass 插 gVisor/Kata
   （agent-sandbox 仓库有全套示例）——应采纳而非自建。
3. **网络出口控制是最刺眼的企业功能缺口。** 市场现状：Copilot 防火墙默认开；Codex agent 阶段默认
   断网 + 域名白名单 + HTTP 方法级限制；Claude 默认 Trusted 白名单 + GitHub 凭证根本不进 VM 的双
   代理；Cursor 三档 + 企业可锁定。本方案 NetworkPolicy 收 CIDR、集成 fixtures 实际 `0.0.0.0/0`，
   无域名白名单/代理层（TODO 未开工）。prompt-injection → 外传是全行业已演示攻击路径，该项已从加
   分项变为门槛项，且应与劣势 1 同优先级。
4. **复杂度预算：小团队背着企业平台部门的代码量。** 控制面约 10.9 万行非测试 Go、约 9.6 万行测试、
   85 个 migration、50+ 子系统，且 TypeScript 服务器与 Go 控制面两套编排栈并存。近期投入（migration
   `000077`–`000083` 全部为计费守恒）与产品瓶颈（延迟）错位：能对账 AWS CUR2 发票，但付费租户尚不能
   自助注册（Stage 5 未启动）。最大存量风险不是单项技术错误，而是维护面吞噬迭代速度。
5. **证据天花板 E3（本地），合规履历为零。** 全部验收为 OrbStack/Kind 本地 lane；托管云多可用区、
   Workload Identity、真实跨域复制、生产 soak 均开放。对手持 SOC 2 Type II（Cursor、Devin）/
   SOC 2 + ISO 27001（Anthropic）销售。附注：截至本文时间点，主检出工作树另有一份未跟踪的
   session-authority failover 演练报告状态为 failed，Stage 4"控制面故障不丢权威状态"完成条件保持
   未勾选是正确的。
6. **产品表面差距大，但受制于劣势 1。** 2026 年桌面筹码：移动 App、Slack/Linear/GitHub 触发、
   定时/事件自动化（Automations/Routines）、best-of-N、CI 自动修复、PR 审查产品（Bugbot、
   `@codex review`、Claude Code Review）。本方案域模型有 Automation 概念而产品表面基本只有 Web
   会话。供给慢时做不出好的自动化产品，故排序在延迟之后。
7. **快照/fork 原语缺失 + 自托管窗口非永久。** live 内存快照/fork 正成为分层点（E2B fork、Morph
   Infinibranch、CodeSandbox hibernate），本方案 checkpoint-to-object-storage 为 15 秒级而非 500 毫秒
   级，best-of-N 类并行探索在此成本结构下不可行；同时产品层玩家（Cursor BYO pool、Devin
   Outposts、Factory BYOM）正在攻入"跑在你自己机器上"的差异化位。

## 五、对方向评估的确认与补充

[方向评估](cloud-agent-direction-assessment-20260726.md) 的北极星（E2B 式秒级）与
[fast-provision 提案](fast-provision-runtime-proposal-v0.md) 的三层供给方向，经市场对照后完全成立。
市场调研补充三点它未覆盖的判断：

1. **Egress 控制应与冷启动同优先级。** 三巨头 + Copilot 已全部把默认限网/白名单做成出厂配置，
   企业评审把它当门槛项；fast-provision 落地顺序中应给它留位，而非归入 Stage 5。
2. **隔离深度靠采纳解决，不自建。** microVM/gVisor 层已被四大云 + agent-sandbox 商品化；
   ExecutionTarget 恰好是正确的适配缝——以 RuntimeClass（gVisor 先行、Kata 强隔离）或托管 sandbox
   服务作为新 capacity class 接入，与 fast-provision 提案的 `snapshot-restore` 层级评估合并做 spike。
3. **冻结计费/DR 类扩张直到交互 lane 达标。** 本文与独立模型评审各自得出同一结论：护城河是执行
   语义（fencing/Bundle/决策图/Grant），不是数据库 HA 与发票对账。已建成的计费/DR 正确性不回退，
   但新增投入应让位于劣势 1–3。

保留资产清单（不因任何重构动摇）：Worker Protocol 与 Provider Host 协议、Generation/incarnation
fencing、幂等 receipt、Recovery Bundle、调度决策证据图、Grant 凭证模型、供应链纪律。这些是延迟问题
修复后真正构成销售差异的部分。

## 六、易用性与稳定性对齐手册

把前文事实转成可执行的对标项与设计建议。本节由对标清单（U/S 编号）与五份建议组成：对齐路线
（A/B/C 三阶段）、SLO 草案、环境模型、失败呈现与恢复 UX、diff 审查与 PR 流。状态：✅ 已有并领先
或齐平；🟡 协议/域模型已有、产品面未露出；❌ 缺失。优先级与第四节劣势排序一致（P0 = 产品成立
门槛）。

易用性：

| #   | 对标项            | 成熟产品基线                                                                       | Synara 现状                                            | 差距动作                                     |
| --- | ----------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------ | -------------------------------------------- |
| U1  | 秒级可开始        | E2B ~150–200ms；GKE 宣称 P90 200ms；三巨头以环境缓存隐藏冷启动                     | ❌ 秒到分钟级（劣势 1）                                | P0：fast-provision 三层供给 + SLO 门禁       |
| U2  | 环境快照/缓存     | Codex 容器缓存 12h；Claude 快照约 7 天；Cursor VM snapshot；Jules Run-and-Snapshot | ❌ 每 Turn 物化 + 网络 fetch                           | P0：cache-first + volume snapshot（提案 §3） |
| U3  | 中途 steering     | 三巨头全支持（Web/移动/Slack 追问）                                                | ✅ Steer/Interrupt 已统一为 durable Control Command    | 保持；移动/异步面见 U5                       |
| U4  | 触发面/自动化     | cron/webhook/GitHub/Slack/Linear；Cursor 触发器最全；Claude Routines               | ❌ 域模型有 Automation 概念，无产品化触发器            | P1（依赖 P0——供给慢做不出好自动化）          |
| U5  | 移动端与完成通知  | Cursor iOS、ChatGPT 移动端、Claude App 均可监控/追问                               | ❌ 仅 Web                                              | P2                                           |
| U6  | diff 审查与 PR 流 | 三家 diff→PR 一键；独立 PR 审查产品（Bugbot/`@codex review`/Claude Code Review）   | 🟡 git worktree/branch/push/PR 生命周期已有（Stage 3） | P1：Web diff 审查 UX + PR 审查产品化         |
| U7  | best-of-N 并行    | Codex `--attempts` 1–4；市场整体稀缺                                               | ❌ Fork 语义已有，成本结构不支持并行探索               | P2，依赖快照层（劣势 7）                     |
| U8  | 多 repo 任务      | Cursor ≤20 repo；Claude 单 repo 为头号用户抱怨                                     | ❌ 单 Session 单 Workspace                             | P2：市场三方分裂处，潜在差异化位             |
| U9  | 资源规格透明      | 仅 Claude 公布（4 vCPU/16GB/30GB）；Codex/Cursor 不公布且被公开抱怨                | 🟡 capacity class + 全量资源事实已持久化，未向用户露出 | P1：把内部指标产品化，低成本差异化           |

稳定性：

| #   | 对标项              | 成熟产品基线                                              | Synara 现状                                               | 差距动作                                    |
| --- | ------------------- | --------------------------------------------------------- | --------------------------------------------------------- | ------------------------------------------- |
| S1  | 会话跨断线/重启持久 | Claude 关浏览器任务继续；三家均为异步任务模型             | ✅ 控制面权威 Session + SSE backlog + Recovery Bundle     | 已领先，保持                                |
| S2  | 长任务与生命周期    | Copilot 59 分钟硬顶；Cursor 宣称 25–52h；上限普遍不公布   | ✅ suspend/resume + `absoluteExpiresAt` 三层策略化        | 已领先，保持                                |
| S3  | Worker/Pod 故障连续 | 全部黑盒，无公开语义                                      | ✅ Generation fencing + 跨 Pod 恢复（Stage 3 验收）       | 已领先；缺生产 soak（劣势 5）               |
| S4  | 失败可解释性        | Codex "job was killed" 无解释是公开抱怨                   | 🟡 Pod failure 分类、冷启动分位已持久化，未露出给最终用户 | P1：错误与延迟归因的用户可见化              |
| S5  | 缓存失效语义        | Codex 团队共享缓存失效（secret 轮换全队冷重建）是公开痛点 | —（尚无缓存层）                                           | P0 设计输入：缓存键按环境隔离、失效显式可见 |
| S6  | 默认限网 + 白名单   | 四家全有；Copilot 默认开启                                | ❌ fixtures 实际 `0.0.0.0/0`（劣势 3）                    | P0：域名白名单/代理层                       |
| S7  | 隔离深度            | microVM（Cursor/E2B 等）或 gVisor（Modal/GKE）或整 VM     | ❌ 加固容器 + cgroup supervisor（劣势 2）                 | P0：RuntimeClass 采纳 gVisor/Kata           |
| S8  | 安全响应履历        | 两家有已披露 CVE 与修复时间线；SOC 2 为售卖门槛           | 🟡 设计强于对手履历，但无外部审计/披露流程                | P2：Stage 5 渗透测试 + 安全响应制度         |

读法：✅ 集中在"执行语义/恢复"（第三节优势的产品化投影），❌ 集中在"供给速度/安全边界/产品表面"
（第四节劣势的投影）。P0 三项（U1/U2、S6、S7）构成"产品成立"最小集合；P1 四项（U4、U6、U9、S4）
是"易用性齐平"集合，全部可复用既有域模型与指标，不需要新领域概念。

### 对齐路线建议（与 fast-provision 提案 §6 合并排序）

阶段 A——产品成立（P0，前三步即 [提案](fast-provision-runtime-proposal-v0.md) 落地顺序 1–3）：

1. SLO 门禁 + `provision_tier` 度量标签（纯度量/文档，对标 U1 的验收面）。
2. `guaranteed-warm`：min-idle 硬补齐 + Provider Host 预启动（U1 主体，无新基础设施）。
3. Workspace cache-first + 显式 freshness（U2；S5 的设计输入在此步落地——缓存键按环境隔离、
   失效显式可见，避免复刻 Codex 团队共享缓存失效的公开痛点）。
4. Egress 白名单/代理层（S6）。独立子系统，不占 1–3 关键路径，可并行；市场参照取 Codex 模型
   （默认断网 + 域名白名单 + HTTP 方法级限制）为最完整形态，Claude 的"凭证代理不进 VM"为第二层
   目标。
5. RuntimeClass gVisor 单 Target 原型（S7），与提案 §7 的 microVM spike 合并出数据后再定路线。

阶段 B——易用性齐平（P1，全部为既有事实/域模型的产品化露出，不新增领域概念）：

6. 失败与延迟归因用户可见化（S4 + U9）：把 Pod failure 分类、冷启动分位、capacity class 直接
   投影到会话 UI；反面教材即 Codex "job was killed" 无解释。
7. Web diff 审查 UX 与 PR 流打磨（U6），随后才是独立 PR 审查产品。
8. 触发面第一步（U4）：GitHub webhook + cron 两种触发器复用 Automation 域模型起步；触发器矩阵
   的完整参照是 Cursor（PR 事件/CI 完成/label/review + Slack/Linear/Sentry/PagerDuty）。

阶段 C——差异化（P2，允许新原语）：移动端与通知（U5）、快照层与 best-of-N（U7，跟随提案
`snapshot-restore` 层）、多 repo 任务（U8，市场三方分裂处）、安全响应制度与外部审计（S8）。

排序原则：A 不新增领域概念且决定产品成立；B 只做露出，收益/成本比最高；C 才引入新原语。任何
阶段不回退第三节已领先项（S1–S3）的语义。

### 可度量定义：SLO 草案 v0

先分两类以免混淆：**不变式**是契约已保证、必须 100% 的性质（违反即 bug，验收即回归）；**SLO**
是分位目标，按[方向评估](cloud-agent-direction-assessment-20260726.md)建议 1 升级为 Stage 4 完成
条件与 release checklist 门禁。市场参照：成熟产品一律不发布延迟 SLO（以异步任务框架 + 环境缓存
掩盖体感）；公开的供给延迟承诺仅 GKE"90% 分配 ≤200ms"一例。发布明确 SLO 本身即 U9 透明差异化的
一部分。

不变式（现行契约已保证）：

- 事件零丢失：SSE 重连后按权威 Event Sequence 补齐 backlog，无空洞（S1）。
- 无第二终态：旧 Generation/Pod/Lease 不能产生第二终态或重复副作用（S3）。
- 失败必有 bounded class：非用户取消的终态失败 100% 携带低基数失败分类（S4 的数据前提，
  Migration `000078` 已保证）。

SLO 草案（目标值待评审，度量来源全部已存在）：

| SLI                                  | 目标（草案）                            | 度量来源                                           | 对标  |
| ------------------------------------ | --------------------------------------- | -------------------------------------------------- | ----- |
| 交互供给延迟 dispatch→provider ready | warm 命中 P95 ≤2.5s、P99 ≤5s            | trailing-30d Generation facts（`000059`/`000081`） | U1    |
| interactive lane warm 命中率         | ≥99%（首阶段 ≥95%）                     | warm hit/fallback durable facts                    | U1    |
| 交互排队时长                         | P95 ≤1s                                 | `synara_execution_queue_*`（`000084`）             | U1    |
| suspended→可继续输入                 | checkpoint 层 P95 ≤15s；快照层 ≤1s      | claim-time resume decision facts                   | S1/S2 |
| Worker/Pod 丢失后恢复成功率          | ≥99.5%，outcome-unknown ≤0.1%           | recovery outcome trailing-30d                      | S3    |
| 控制面就绪可用性                     | enterprise 双副本 99.9%（Stage 5 定稿） | `/ready` 与 readiness 探针序列                     | S1    |

注：延迟目标沿用 fast-provision 提案 §0，补充排队与命中率拆解；outcome-unknown 预算把 fail-closed
的代价显式化——它是安全边界的成本，不得为清零而放宽不可重放约束。

### 环境模型建议（U2/U4 的具体化，市场模式蒸馏）

三家产品的环境配置 UX 已收敛到同一形态：**环境 = 基础镜像 + setup script + 环境变量与 secrets +
网络等级**，与 repo 关联、可快照缓存、团队可共享。差异只在细节：Codex 是 per-repo 环境 + setup/
maintenance 双脚本 + 12h 容器缓存（setup/env/secret 任一变更即失效）；Claude 是命名环境（网络等级、
env vars、约 5 分钟预算的 setup script）+ 成功后文件系统快照约 7 天 + org 共享环境；Cursor 是
`.cursor/environment.json`（repo→个人→团队优先级）+ snapshot/Dockerfile 双模式 + "agent 代配环境后
存快照"；Jules 用"Run and Snapshot"做环境验证动作。三家共同的坑与缺口：Codex 团队共享缓存因 secret
轮换全队冷重建；setup 与 agent 阶段环境分离导致 `export` 不持久；Claude 不支持自定义基础镜像被抱怨；
devcontainer 摄取三家都没有。

映射到本方案：所需底座几乎全部已有——Worker Manifest/Release 冻结镜像与 build 身份，Workspace v1
管 git 物化与 checkpoint，三层 lifecycle policy + Session 冻结 snapshot 恰好就是"org 共享环境"的
既有形状。缺的只是用户侧 **Environment** 实体。建议形状（与既有模式同构，不新增领域概念类型）：

- Project 级实体，版本化 append-only revision；Session 创建时冻结 environment revision 进
  Recovery Bundle——与现有"冻结 effective policy"完全同构。
- 字段：引用现有 Worker Release 作基础（首版不开放任意 Dockerfile，保住供应链纪律；Claude 同样
  不支持自定义镜像）、幂等 setupScript（显式超时预算，Cursor 的幂等要求 + Claude 的预算制）、非密
  env vars + secret 引用（走既有 credential/secret 域，不落明文——对齐 S6/凭证优势，避开 Claude
  明文可见的缺陷）、环境级网络等级（None/Trusted/Custom，即 S6 的落地面）。
- 快照缓存：setup 成功后的文件系统/volume 快照即 fast-provision cache-first（阶段 A 第 3 步）的
  自然载体；缓存键 =（environment revision, Worker Release, 仓库基点），失效显式可见且按环境隔离
  （S5 设计输入，直接规避 Codex 团队级失效痛点）。
- 显式不做：任意 Dockerfile（后续按需评估）、devcontainer 摄取（市场空位，列为 U8 之后的机会项）。

结论：环境模型是 B 阶段体感收益最大的单件——它同时落地 U2（缓存）、U4 前置（自动化需要稳定环境
引用）、S5（失效语义）与 S6 入口（网络等级），而实现上只是既有 policy/Bundle 模式的一次复用。

### 失败呈现与恢复 UX 建议（S4 的具体化）

市场正反例。反面：Codex"the job was killed during the step (likely due to resource limits)"——不说
哪种资源、无提额路径，是公开抱怨最集中的稳定性 UX 缺陷；Jules 失败尝试照扣每日额度同为公开痛点。
正面：Claude 把"内存不足会失败"写成文档化边界并给出 escape hatch（转本地 Remote Control）；Cursor
用运行 artifacts（截图/录像/日志）让失败可检视；Devin 给子会话结构化输出 schema 与每会话成本上限。
蒸馏成四条原则：失败必须归因（阶段 × 类别 × 责任方）；失败必须给下一步；恢复必须说明保留了什么；
失败不应白白计费。

本方案的底层数据已全部存在，缺的只是投影：

- **失败卡片**：阶段（排队/供给/物化/Provider 启动/运行中）× bounded class（Migration `000078` 的
  Unschedulable/ImagePullBackOff/OOMKilled/Evicted 等分类）× 归因（租户环境/平台/Provider）×
  建议动作。对照 Codex 反例：OOMKilled 应呈现为"内存超出 capacity class X，建议改用 Y 或调整
  构建"，而非"job was killed"。
- **"为什么在等"**：排队时把 `synara_execution_queue_*` 与调度决策的 bounded rejection code
  （capacity-saturated/health-expired/dr-readiness-\* 等）投影为一句人话；这是调度证据图（第三节
  优势 3）的第一个用户可见收益。
- **恢复透明卡**：新 Generation 恢复后显式列出"已保留（权威历史/Workspace checkpoint/待审批
  interaction）/ 已重建（新 Pod）/ 不确定（outcome-unknown 及其含义）"。outcome-unknown 是市场
  无对等物的诚实原语——对手在同类场景只能静默重试或静默丢失。
- **失败不计费政策**：供给阶段失败（Pod 创建/镜像拉取/注册失败）不计入租户用量——release ledger
  与 requested-resource-seconds 的精确性使这条可以做成硬承诺，直接差异化 Jules 的额度痛点。
- **边界显式化**：会话创建即显示 capacity class 规格与生命周期边界（Web 已有 lifecycle bounds
  预览，补资源规格即可），对齐 Claude 的透明做法、避开 Codex/Cursor 的不公布抱怨。

结论：S4 不需要任何新数据或新契约，是纯投影工作；其中"恢复透明卡 + 失败不计费"两项直接把第三节
的正确性优势转译成用户可感知的稳定性，是"内部深度 → 外部体感"转化率最高的两件。

### diff 审查与 PR 流建议（U6 的具体化）

市场收敛形态与蒸馏原则：任务的"结果页"是**摘要 + diff**（Codex 任务列表带 diff-stat 徽章与
running/reviewable/merged 状态；Cursor 以截图/录像 artifacts 辅助审查）；**行内评论即 steering**
（Claude 的 diff 行内评论直接变成下一轮指令，是三家中最好的交互）；**PR 创建是显式动作、agent 永
不自 merge**（Copilot 强制人审 + 只能推 `copilot/*` 分支，已是行业共识 guardrail）；**审查产品与
执行 agent 分离**（Bugbot / `@codex review` / Claude Code Review 均为独立触发面与独立计费，且都
支持 repo 内规则文件——`BUGBOT.md` / AGENTS.md `## Code Review Rules` / `REVIEW.md`）。

本方案映射（git/PR 生命周期与 Artifact/Event 统一投影 Stage 3 已完成，以下为产品面组织）：

- **会话结果页**：摘要 + diff + Artifact 面板（终端/长日志/生成文件已是 Artifact 引用，聚合即可），
  取代"从聊天记录里翻结果"。
- **行内评论 → Steer**：diff 行内评论直接生成 durable Steer Control Command——Synara 的 Steer 本就
  是持久化命令，只差 UI 绑定；采纳 Claude 模式。
- **agent 分支命名空间 + 显式 PR**：agent 只推 `synara/*` 前缀分支（`gitpolicy` 域已存在，加前缀
  约束即可），PR 创建永远是用户显式动作，agent 不 merge。
- **规则文件惯例**：审查/编码规则读 repo 内 AGENTS.md 系文件，对齐三家已建立的用户习惯，不发明
  新约定。
- **跨 Provider 交叉审查（差异化落点）**：同一 diff 交给第二个 Provider 审查——单 vendor 产品
  结构上做不了这件事，而本方案的 Provider 中立（第三节优势 1）使其成为零新概念的组合功能；独立
  计费有 Claude Code Review（约 $15–25/review）先例，可接既有 billing 地基。

结论：U6 的底座（git 流、Artifact 权威、Steer 命令、gitpolicy）全部已有，工作量集中在 Web 结果页
与评论绑定；"跨 Provider 交叉审查"是全场唯一没人能抄的审查功能，应作为审查产品化的首发卖点。

## 七、主要外部来源

产品层（一手）：Cursor docs（cloud-agent/security、security-network、setup、automations、api）与
pricing/enterprise/changelog；OpenAI `learn.chatgpt.com/docs`（cloud、environments/cloud-environment、
cloud/internet-access、enterprise/admin-setup）与 GHSA-w5fx-fh39-j5rw（CVE-2025-59532）；Anthropic
`code.claude.com/docs`（claude-code-on-the-web、sandbox-environments、code-review、
server-managed-settings）、anthropic.com/engineering/how-we-contain-claude、SecurityWeek 与研究者
oddguan.com 对 SOCKS5 绕过的披露；GitHub docs/blog（coding agent、cloud/local sandboxes、usage-based
billing）；docs.devin.ai 与 Cognition 官方博客（Outposts、Devin-manages-Devins）；jules.google/docs；
docs.factory.ai 与 factory.ai/news（Droid Computers）。

基础设施层（一手）：e2b.dev docs/pricing/blog 与 `e2b-dev/infra`（Apache-2.0）；
`kubernetes-sigs/agent-sandbox` README/api/keps/releases（v0.5.3、KEP-539.2、PR #762、PR #950）与
Google Cloud blog（GKE Agent Sandbox GA、Agent Substrate）；modal.com、cloud.morph.so、
developers.cloudflare.com/sandbox、fly.io/docs（suspend-resume）、daytona.io、vercel.com/sandbox、
runloop.ai、beam.cloud、blaxel.ai、AWS Bedrock AgentCore devguide、Azure Build 2026 公告。

社区/二手（已按非受控样本处理）：openai/codex discussion #2251（限额）、OpenAI 社区论坛资源上限
投诉、forum.cursor.com（outage、SSH 回退、snapshot drift）、anthropics/claude-code issues
#23627/#35362/#44656/#27934（单 repo）、Reddit 质量对比聚合、Answer.AI 对 Devin 的独立评测。

## 修订记录

- 2026-07-26 r1：初版——三层格局、竞品速览、优势/劣势、对方向评估的确认与补充、来源。
- 2026-07-26 r2：新增第六节"易用性与稳定性对标清单"（U1–U9 / S1–S8，含 P0/P1/P2 分级）；原
  来源节改为第七节；前言标注本文为持续修订文档。
- 2026-07-26 r3：第六节新增"对齐路线建议"——A（产品成立）/B（易用性齐平）/C（差异化）三阶段，
  与 fast-provision 提案 §6 落地顺序合并，并为各步标注市场参照与反面教材。
- 2026-07-27 r4：第六节新增"可度量定义：SLO 草案 v0"——区分契约不变式与分位 SLO，六项 SLI 全部
  映射到既有 trailing-30d 度量，目标值沿用 fast-provision §0 并补排队/warm 命中率拆解。
- 2026-07-27 r5：第六节新增"环境模型建议"——蒸馏三家环境配置 UX 收敛形态与公开痛点，给出 Project
  级版本化 Environment 实体形状（复用 Worker Release/policy 冻结/Recovery Bundle 既有模式），并
  接到 U2/U4/S5/S6 与 fast-provision cache-first。
- 2026-07-27 r6：第六节新增"失败呈现与恢复 UX 建议"——四条市场蒸馏原则（归因/下一步/恢复透明/
  失败不计费），五个纯投影件（失败卡片、为什么在等、恢复透明卡、失败不计费政策、边界显式化），
  全部复用既有 durable facts 与调度证据图。
- 2026-07-27 r7：第六节新增"diff 审查与 PR 流建议"——结果页/行内评论即 steering/显式 PR 与分支
  命名空间/规则文件惯例四条收敛形态，加"跨 Provider 交叉审查"差异化落点。至此 U2/U4/S4/S5/S6
  入口/U6 的具体化闭环，第六节构成完整对齐手册。
- 2026-07-27 r8：编辑收敛——第六节标题改为"对齐手册"并补小节导览；前言写明"一至五评估 + 六手册"
  两段式结构；修复劣势 6 中被硬换行拆断的代码段与缩进。无内容性变更。
