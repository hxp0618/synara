# Cloud Agent 竞争格局评估：现方案优劣势（2026-07-26）

本文回答"当前方案相对市场处于什么位置"，与两份姊妹文档构成完整评估：
[`cloud-agent-docs-audit-20260726`](../reports/cloud-agent-docs-audit-20260726.md) 回答"文档与实现是否一致"，
[`cloud-agent-direction-assessment-20260726`](cloud-agent-direction-assessment-20260726.md) 回答"实现方向是否对准产品
北极星"。本文的优势/劣势判断全部对照 2026 年 7 月市场现状给出，不重复姊妹篇已有的内部诊断细节。

**证据口径。** 外部事实来自 2026-07-26 的三路并行调研（产品线约 28 个一手来源、基础设施线含
kubernetes-sigs/agent-sandbox 仓库级核对）加一次独立模型评审；关键来源见文末。延迟/规模数字多为厂商宣称
（vendor claim），未经独立复测的一律照此标注；社区口碑类结论标注为非受控样本。内部事实以当日
`codex/saas-tenancy-user` 工作树为准。本文为持续修订文档，服务"把本项目 cloud agent 能力做到与成熟
产品同级的易用与稳定"这一目标；修订历史见文末。

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
6. **产品表面差距大，但受制于劣势 1。** 2026 年桌面筹码：移动 App、Slack/Linear/GitHub 触发、定时
   /事件自动化（Automations/Routines）、best-of-N、CI 自动修复、PR 审查产品（Bugbot / `@codex
review` / Claude Code Review）。本方案域模型有 Automation 概念而产品表面基本只有 Web 会话。供给
   慢时做不出好的自动化产品，故排序在延迟之后。
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

## 六、易用性与稳定性对标清单

把前文事实转成可执行的对标项，服务"与成熟产品同级的易用、稳定"目标。状态：✅ 已有并领先或齐平；
🟡 协议/域模型已有、产品面未露出；❌ 缺失。优先级与第四节劣势排序一致（P0 = 产品成立门槛）。

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
