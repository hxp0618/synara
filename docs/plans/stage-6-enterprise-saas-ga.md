# Stage 6：企业 Self-hosted GA、运营、安全与成本治理

状态：IN PROGRESS

## 1. 范围与原则

本阶段把已经存在的多租户、身份、审计、配额、保留、KMS、成本归集和可观测性能力收口成可供企业内部
正式使用、可自助运营、可审计支持的平台。当前产品边界不向外部客户售卖，不接支付、Checkout、税务或
外部订阅结算；“商业化”在本阶段专指内部成本中心/部门分摊、Provider 使用授权和成本治理。Stage 5 负责修复沙箱与运行时安全缺口；Stage 6 只验收
其结果，不建立旁路。Stage 7 负责公开 API/SDK 与机器凭证 scope；Stage 8 负责协作内容模型；Stage 9
负责事件触发自动化。

所有新增权威状态必须满足：

- PostgreSQL 与 SQLite 行为一致；涉及并发的写路径必须有真实 PostgreSQL 双连接验证。
- 以服务端状态机和数据库约束为权威，Web 管理界面不能成为唯一门禁。
- 所有管理动作记录 actor、reason、request、前后状态和稳定审计 action；Secret 与原始敏感内容不得进入审计。
- 删除、恢复、impersonation、Entitlement 和计费写入必须显式幂等或使用乐观并发版本。
- 已有能力先做现状核查和产品化，不以重建模块制造“完成”证据。

前端、桌面与 Platform authority 的仓库边界由
`docs/adr/0004-stage-6-enterprise-frontend-boundaries.md` 冻结：Electron 只保留薄壳；Tenant UI 经共享
enterprise feature 同时服务浏览器和 Desktop；Platform 跨 Tenant 操作只能进入独立 `apps/admin`；后续再按
阶段抽取共享边界。Phase B 已把 typed HTTP/SSE runtime 抽到 `@synara/control-plane-client`；Phase C 已把
Tenant destination renderer、登录/上下文呈现、capability 解析和治理组件抽到 `@synara/enterprise-ui`，Web 仅通过
显式 runtime、design-system 与 Overview extension slots 接入。Phase D 已完成独立 `apps/admin`；Phase E 的短时、
单次、device-bound Desktop Enrollment 源码、迁移、真实 PostgreSQL 并发验证、macOS arm64 本地安装态与 x64
Rosetta 本地验收也已完成。Apple Silicon 日常安装已固定为原生 arm64；无保存连接的启动不会提前访问 Keychain。
这些安装态仅使用 Apple Development 签名；Developer ID 签名/公证的 macOS arm64/x64、原生 Intel 主机以及
Windows/Linux 安装包真实协议唤起和 OS Credential Store 验收仍是候选发布门禁。统一机器合同已落在
`docs/contracts/desktop-installed-acceptance-v1.md`，`validate_desktop_native_acceptance.py` 会直接解析四份 release
provenance 并固定原生 host、签名/公证或 CI attestation、协议、凭据、Enrollment、Secret scan 和三方审批；
当前本地报告不满足该合同的 review-eligible 条件。

## 2. 交付顺序

### S6.1 Tenant 生命周期

冻结数据库兼容状态 `trialing | active | suspended | closed | deleting` 与合法转换；公开 API/UI 只暴露
`evaluation | active | suspended | closed | deleting`，而不是对外试用；Tenant 创建可选择 evaluation 或直接
启用，状态转换携带 reason 与 expected version。`closed` 是可恢复、不可运行的业务终态；`deleting`
会停止 Execution 并触发 Workspace 清理，仅在清理完成后允许恢复到 `closed`，防止恢复与清理竞态。
Web 管理面提供 owner-only 状态迁移、关闭后删除请求和独立的待删除恢复清单，确保 soft-delete 后即使
普通 Tenant 列表已隐藏记录，Owner 仍不依赖数据库或 CLI 完成撤销。
Tenant 开通分成两条不相混用的 authority：已登录用户的自助入口固定为 active Standard entitlement profile；配置好的 Platform
Operator Tenant 中 Owner/Admin 才能为已有 active internal user 选择 Enterprise/evaluation，且开通者不会获得
工作负载 Tenant Membership。两条路径都原子创建 root Organization、保留兼容表中的 entitlement assignment、Owner Membership 与 Audit。

验收：非法/过期版本转换 fail closed；非 Owner 不能转换；每次转换有前后状态审计；非 `active` Tenant
不能领取新 Execution；SQLite 与 PostgreSQL 约束一致。

### S6.2 企业身份闭环

实现 Domain Verification 与 SSO Enforcement；IdP/SCIM 停用必须原子或可恢复地撤销 Login Session、
Tenant/Organization Membership、Credential Grant，并留下关联审计。冻结固定 RBAC v1 或给出自定义 Role
决策。人工成员调岗/停用与 SCIM 必须共用同一 offboarding 原语，不能形成更弱的后台旁路；Provider Credential
显式绑定优先，自动选择固定为 user → organization → tenant → platform，Platform fallback 需要部署、Plan 与
Tenant policy 多重显式授权。

RBAC v1 决策：本阶段不引入自定义 Role。Tenant 固定为
`owner | admin | security_admin | cost_admin | auditor | member`，Organization 固定为
`owner | admin | agent_operator | member | viewer`。Stage 7 API Key 使用独立资源 scope；未来如需自定义 Role，
必须设计 versioned policy language、迁移与 deny-first 兼容语义，不能扩散自由字符串判断。

### S6.3 Entitlement、用量与成本

建立 versioned internal entitlement profile/Feature Flag，不复制现有 Quota。补齐 Token 与 Network 计量，建立内部核算周期、
软性配额预警和 per-Session/per-Turn 成本解释。Entitlement 与 Quota admission 在同一权威事务边界内判定。

当前 Stage 6 候选固定使用 `internal-self-hosted` 模式：Provider 成本、共享 Target 估算分摊和云账单实际分摊
必须形成可解释、不可重复的内部成本投影；支付页面、Stripe live evidence 和 Finance settlement 不属于当前 GA
门禁。当前运行配置拒绝启用 Stripe adapter，Tenant UI 不挂载支付面板，发布证据也不得要求支付材料。

Migration `000150` 又把 Provider 成本可用性从隐含的 `0` 提升为显式单调事实：只有 Runtime 明确报告价格时
`provider_cost_reported=true`，包括明确报告的零成本；历史正成本安全回填，历史零值保持 unavailable。Session/
Turn 与 Tenant 周期聚合会排除未知成本并显示缺失报告数量，不再把未定价执行冒充 `$0.00`。
历史支付实现（Migration `000109`、`000131`、`000132` 及对应 adapter/validator）不属于当前产品路径；保留仅为
迁移兼容和后续清理，不构成能力承诺。Candidate/Release v4 已通过 Migration `000152`/`000153` 切换为
`internal-self-hosted` 的用量/成本证据；旧 Stripe receipt 仅供 v2/v3 离线审计，不能进入当前候选或产品 UI。

### S6.4 运营与支持

交付 Tenant/Organization 与 Platform Admin 管理界面，以及 Worker、Execution、Queue、Artifact、Credential、
Identity Connection 运维视图。支持态 impersonation 必须只读、写明理由、限时、可由 Tenant 禁用、Tenant
管理员可见并全程审计。

`docs/release-matrices/stage-6-operations-ui-v1.json` 固定日常操作的 UI → typed client → authenticated route
链路。ADR 0004 接受后，矩阵已升级为 surface-aware v2：20 项 Tenant Web 操作绑定到 enterprise Settings
注册/host seam，29 项 Platform Tenant/Support/Release/Compliance/Provider/Governance Authority/Incident/Incident Exercise/SLO/Recovery/Penetration/Capacity 操作绑定到独立 `apps/admin`；49 项源码 surface 均为
`reachable`。收据固定为 `source-ui-routes-validated-all-surfaces-reachable-not-operations-passed`，真实部署的
角色分离浏览器演练与 Audit 证据仍是外部 GA Gate。
候选部署证据由 `scripts/stage6-operations/validate_operations_exercise_evidence.py` 固定：精确绑定
Control Plane/Web/Admin digest、两个独立 HTTPS origin、十一个分离角色账号、49 项正向与固定负向角色浏览器证据、
全局唯一 request ID、Support 四眼/只读/撤销/Audit 闭环及 Operations/Security 双审批。失败或借助 CLI、数据库、
浏览器开发者工具的行会保留为 ineligible receipt；即使全部满足，收据仍为
`evidence-validated-not-operations-passed`，不能替代真实候选执行和 GA 签署。
Phase A 已建立 `apps/web/src/features/enterprise` 注册边界与七个 capability-filtered Organization Settings
目的地，保留旧 `?section=tenancy` 深链，并从客户组合移除 Platform 操作。Phase B 已新增
`packages/control-plane-client`，迁移 typed models、cookie transport、稳定错误、Artifact helper 与可续传 SSE；
Web 的 62 个消费文件改用公开包 API，Desktop 自定义协议只通过一个 host resolver 接入，HTTP(S) 始终保持
同源代理。Phase C 已将完整 Tenant renderer closure、身份/凭证/生命周期/合规等叶子组件、capability 解析和
共享查询辅助移入 `packages/enterprise-ui`；包不引用 Web route、Electron 或应用 singleton，Web adapter 只注入
Control Plane runtime、现有设计系统以及 Execution Targets/Project Sessions 两个 Overview 扩展槽。企业 UI 包
22 个测试文件/59 个测试及 ESM/CJS/d.ts 构建通过，完整 Web Vitest 为 301 个文件/3502 个测试；Chromium 4 条
focused browser tests 覆盖 narrow width、compact/spacious density、键盘顺序与真实 reduced-motion 模拟。
Phase D 已新增独立 React/Vite `apps/admin`、独立 SSO/Operator Tenant 门禁、Owner/Admin/Security 权限面、
Platform Tenant triage、provisioning、versioned internal entitlement profile 与四眼 Support Access；Compose/Kustomize 和兼容
矩阵已登记独立构建/服务。Migration `000111` 又增加产品内 Release Governance：精确候选 identity、创建者分离、
四类必需 append-only 审批、versioned rollout/observation、终态决定与结构化残余风险全部进入 Platform Audit，
Security 可执行角色审查但不能创建或推进候选。隔离浏览器用真实 Owner、第二 Admin、Security 与 support_readonly 会话验证允许/拒绝
路径，并关联 entitlement assignment、Support 与写阻断 Audit request ID。该结果仍不代表打包 Desktop 一致性、生产部署
49 项全矩阵或 GA 通过。
Migration `000112` 又增加 Platform Compliance Governance：不可变 SOC 2/ISO scope、七类 Control Owner、
append-only evidence digest/reference/retention、独立 review 和 Security/Operations/Legal-Privacy/Executive 四人
start-gate decision；最强状态固定为 `record-complete-not-audit-active`，不冒充外部审计或认证。
Migration `000114` 再把 Release、Compliance 与 Provider commercial 的职能角色从请求体标签提升为 Operator
Tenant Owner 授予的限时权威。Platform Admin → Governance roles 提供无 CLI/数据库依赖的授权/撤销；服务层与
PostgreSQL/SQLite 插入触发器均重验 exact key、active Membership/User/Tenant 和 expiry，offboarding 或撤销立即
阻断新决策。该记录仍不冒充公司职务、律师或审计机构的外部 authority。
Migration `000115` 继续把 Release 候选的影响域固定为不可变枚举，并由服务与 PostgreSQL/SQLite 共同推导
Privacy/Legal 是否为第五个必需审批。商业条款、个人数据、保留/Legal Hold、Residency、受监管客户和安全事件
不再只依赖 runbook 文字或前端勾选。
Migration `000134` 进一步关闭候选只登记旧 receipt 的旁路：Platform Admin 必须上传完整 bounded v3 receipt，
Control Plane 对精确字节重算 SHA-256、验证十类证据均可进入人工门并核对 candidate/commit/lockfile/environment/
Desktop artifact-set；原始字节和有界摘要不可变保存，PostgreSQL/SQLite 均拒绝跨候选复用或直接篡改。
Migration `000135` 再把 Final Review v1 接入 Platform Release Governance：候选进入 `observing` 后由 Owner/Admin
上传精确收据，Control Plane 重新校验候选身份、32 项 Control、分离的最终审批、Audit request ID、决定摘要、残余
风险和外部权威边界并做 append-only 保存；未绑定或决定/风险不一致时，服务与 PostgreSQL/SQLite 都拒绝进入
`released`。该产品门禁只证明内部记录一致，不替代真实 GA authority。
Migration `000117` 新增 Platform Incident Governance：事件 identity、公开组件/Region、Commander/Communications/
Security-Privacy 分权、独立 Status Board origin、外部公开更新顺序与节奏、关闭审批均为数据库约束；Admin 提供对应
产品路径并记录 Audit。SQLite 与临时 PostgreSQL 17 已验证并发公开证据串行化和历史不可变，但该能力只记录外部
发布证据，不能冒充真实 Status Board、值班链路、employee notification delivery 或 production-like 演练。
Migration `000118` 又把离线 SLO 收据接入 Platform SLO Window Governance：精确 30 天收据字节/digest 与 Release
candidate identity 不可变绑定，服务端重算四项 SLO 和 error-budget policy，失败窗口也必须保留；只有 receipt
eligible 且 Engineering/Operations/Security/Product 四个不同职能 authority 全部批准时才能形成内部 approved
记录。SQLite 与临时 PostgreSQL 17 已验证并发审批和历史防篡改，但内部审批仍不等于生产 SLO claim。
Migration `000119` 继续关闭 Release Governance 对 v2 candidate receipt 顶层布尔值的过度信任：服务与数据库现在
逐项验证九类 projected receipt 的精确 schema/assessment/path/digest/time、候选 Artifact/Region/origin/Migration、
manifest/release-evidence reference、Desktop artifact-set 以及 Recovery/Residency 的外部权威边界。空对象、路径复用、
字段漂移和弱化验证边界在进入人工审批前即 fail closed；该门禁仍不声称验证了外部签名或真实控制执行。
Migration `000120` 再把已产品化的 SLO Window Governance 接入 Release Governance 最终批准：候选只有在存在同
candidate、同 Operator Tenant、eligible 且四职能已经内部批准的不可变 SLO window 时才能进入 `approved`。服务层、
SQLite 与 PostgreSQL 均阻断缺失或跨候选窗口；它组合内部治理权威，但仍不把本地/production-like 收据冒充生产 SLO。
Migration `000121` 又将 Recovery v2 从离线文件投影提升为 Platform Recovery Governance：完整收据、候选与恢复后
release identity、四组件 RPO/RTO、五份来源决策和单一 subject digest 均由服务重验并不可变保存；Database/KMS/
Operations/Security/Storage 五个不同的 `recovery.*` 职能 authority 才能形成内部 approved。Admin 新增 Recovery drills
导入、组件投影和决策页面。该状态仍明确不验证真实 backup authority、外部审批身份或密码学签名。
Migration `000122` 再将 Recovery Governance 接入 Release Governance 最终批准：候选只有在存在同 candidate、同
Operator Tenant、eligible、四组件目标与 canary 通过、五份来源决策通过、外部权威边界保持显式且五职能已内部
批准的不可变 Recovery drill 时才能进入 `approved`。服务层、SQLite 与 PostgreSQL 均阻断缺失、跨候选或弱化
边界的 Recovery 门禁；它组合内部治理权威，但仍不把本地或 production-like 收据冒充真实生产恢复证明。
Migration `000123` 将第三方渗透收据提升为 Platform Penetration Governance：导入字节必须与 candidate bundle
记录的 SHA-256、Commit、环境和 Web/Control Plane/Worker/Provider Host 四类 Artifact 精确一致，并重验 Stage 5
依赖、独立性声明、六类攻击面、必需方法论及 High/Critical 状态。Engineering/Product/Security 三个不同的
`penetration.*` 职能 authority 才能形成内部 approved；Admin 提供导入、投影和决策页面。该状态不验证第三方
身份、签署报告、密码学签名或真实执行。Migration `000124` 再要求 Release Governance 在进入 approved 前消费
同 candidate、同 Operator Tenant 的上述 approved 记录；服务、SQLite 与 PostgreSQL 都拒绝缺失、跨候选和
数据库直写绕过，但不会把产品内审批表述为第三方渗透测试已经通过。
Migration `000125` 将 Capacity/soak 收据提升为 Platform Capacity Governance：validator receipt 现保留完整起止时间、
forecast/load、五阶段与五类扰动投影；导入必须与 candidate bundle 中的 exact digest、Commit 和环境一致，服务端
重算 24/72 小时连续窗口、20%+ headroom、三 Region 探针覆盖、SLO/饱和度/Tenant fairness 与七类证据引用。
只有不同 Engineering/Operations `capacity.*` 职能 authority 才能形成内部 approved；Admin 提供导入、投影与决策。
Migration `000126` 再要求 Release Governance 在进入 approved 前消费同 candidate、同 Operator Tenant 的上述记录；
服务、SQLite 与 PostgreSQL 都拒绝缺失、跨候选和数据库直写绕过，但不验证真实环境、遥测、签名、执行或外部审批权威。
Migration `000127` 把 exact Incident exercise receipt 提升为 Platform Incident Exercise Governance：导入字节必须与
candidate bundle 中的 SHA-256、Commit 和环境一致，服务端重算独立 HTTPS Status Board origin、六角色分权、SEV
paging/ack/escalation、六个公开组件、公开时间线节奏、订阅者投递、恢复观察、复盘标志及 manifest/七份 evidence
引用。只有不同 Operations/Communications `incident_exercise.*` 职能 authority 才能形成内部 approved；Admin 提供
导入、投影与决策页面。Migration `000128` 再要求 Release Governance 在进入 approved 前消费同 candidate、同
Operator Tenant 的上述记录；服务、SQLite 与 PostgreSQL 都拒绝缺失、跨候选和数据库直写绕过，但不验证真实
Status Board、paging、订阅者投递、签名、执行或外部审批权威。
Support Grant 现由 Migration `000110` 固定其 Platform Operator Tenant authority；每次客户读取、Tenant 列表与
Session 认证都重验 requester 的 active Platform role 和 Operator Tenant 生命周期，离职/降权立即撤权，历史未绑定
Grant fail closed。该边界已通过 SQLite、HTTP 及临时 PostgreSQL 17 的 authority 不可变和 offboarding 回归。

Phase E 已冻结 `docs/contracts/desktop-enrollment-v1.md` 并实现 Migration `000108`、单次 Enrollment 原子兑换、
Ed25519 device proof、Desktop audience session、refresh family replay 撤销、Admin 自助签发/撤销、严格 allowlist
Deep Link、Electron main 进程代理、系统凭据加密存储、完整 hydration 后提交以及失败回滚/断开保留本地状态。
该迁移在已进入 HEAD 的 runtime-isolation Migration `000107` 之后完成冲突改号；兼容检查现会拒绝数字版本重复。
SQLite 聚焦矩阵与真实 PostgreSQL 双连接兑换/rotation 竞争均已通过。macOS arm64 最终本地包还完成了真实
LaunchServices `synara://connect` 唤起、allowlist 拒绝、Ed25519 proof、共享 Enterprise UI hydration、Keychain-backed
密文落盘、重启 rotation、UI disconnect、后端就绪后双向自动重载和本地 SQLite 保留；安装态暴露的 staged runtime
依赖、可信 CORS 与旧查询缓存问题均已修复。后续 x64/Rosetta 安装态又验证了同一连接/rotation/disconnect 路径，
并修复了无连接启动提前打开 `safeStorage`、x64 与 Mach-O `x86_64` 名称映射以及跨架构 staging 沿用构建机原生
依赖的问题；原生 arm64 build 10 已安装到 `~/Applications/Synara.app`，在完全隔离 HOME 与实际用户目录两条路径
均无钥匙串提示或 Intel 兼容警告。不可变边界报告见
`docs/reports/stage-6-desktop-macos-arm64-local-acceptance-20260731.md`、
`docs/reports/stage-6-desktop-macos-x64-rosetta-local-acceptance-20260731.md` 与
`docs/reports/stage-6-desktop-macos-arm64-local-install-fix-20260731.md`。这些包只用 Apple Development 签名，
Gatekeeper 拒绝且无 stapled ticket；Rosetta 也不等于原生 Intel 主机。因此它们不替代 Developer ID 签名/公证的
macOS arm64/x64、Windows 与 Linux 安装验收、候选部署端到端验收，也不代表 production-like 49 项全矩阵或
GA 通过。四平台候选证据现在统一由 `docs/contracts/desktop-installed-acceptance-v1.md`、
`docs/runbooks/desktop-native-release-acceptance.md` 与 `scripts/stage6-desktop/validate_desktop_native_acceptance.py`
约束；Rosetta、Apple Development、未签名 Windows 例外或错误 Linux Secret Service 只能得到 ineligible receipt。
Desktop 启动模式现另有非 Secret 的原子持久化权威：全新 profile 在后端启动前必须由用户选择 local 或 Cloud
Panel；后续启动只按既有 mode 恢复。没有 mode 时，受保护 Cloud 连接、legacy local SQLite 或显式 Enrollment
Link 都不得替用户推断模式；local 收到 Enrollment Link 还必须确认后才切到 Cloud。切到 local 会停止 Cloud
URL/凭据代理注入但保留 OS Credential Store 中的连接，切回 Cloud 才重新 hydration/rotation；只有显式
disconnect 才撤销并删除设备凭据。四平台候选验收已把首次选择、双向切换、两种模式重启恢复、local 零 Cloud
流量与凭据保留列为必需字段。

ADR 前旧聚合页的隔离结果只保留为历史实现证据。当前 Phase A-C 在新的隔离 Personal Control Plane 上已重跑：
本地模式不注册 SaaS 导航，未登录态进入统一门禁，Owner 登录后七个 Organization 目的地、旧深链、SCIM 搜索
和页面错误检查通过，客户 Settings 不出现 Platform 管理面；窄屏 Data 页在 390px、reduced-motion 下无横向
溢出。该轮同时修复 Session Tenant 投影遗漏 lifecycle version/时间戳导致 Owner 状态迁移控件被禁用的问题。
该 Tenant Web 单账号结果本身仍不证明多角色允许/拒绝或打包 Desktop；Phase D 的 Admin 多角色隔离验收是
另一条本地实现证据，两套最终 host 仍必须在 production-like 候选部署完成 49 项全矩阵才可进入 GA 证据。

### S6.5 数据治理与合规

在现有 Audit/Retention 上增加 Legal Hold；实现带不可变摘要收据的 Tenant 数据导出、用户数据导出、单用户
删除与 DSAR 工单状态机；冻结数据驻留产品承诺、Provider 许可与隐私边界，并把 SOC 2 Type II / ISO 27001
证据采集接入 Audit 与发布流程。

### S6.6 可靠性与 GA

产品内发布通道复用 Web 现有 post-upgrade notice 与 Settings → Release history，并以同一 release entry 驱动；
管理员动作、破坏性变更和紧急安全例外使用结构化 notice，普通功能文案不能替代行动、截止时间与迁移路径。
用户、Tenant 管理员、部署运维和支持排障入口统一收敛到 `docs/enterprise/`；源码矩阵只证明必需文档、章节与
本地链接存在，每个候选版本仍须按实际角色、镜像配置及签署承诺复核。

接通 Control Plane → Worker → Provider 分布式 Tracing，完成 Tenant 安全边界审计；冻结 SLO、错误预算、
On-call、事故响应、公开 Status Board、RPO/RTO 与真实恢复演练。建立 Migration × Protocol × Worker Image ×
Web 兼容矩阵、SBOM/签名/扫描和发布审批，最后执行第三方渗透、容量与长稳验收。

企业 Control Plane 的 OTLP exporter 已增加生产 mTLS 配置门禁：HTTPS、`http/protobuf`、完整 client identity、
collector Region 与 1–90 天 retention declaration 缺一不可，凭据 URL、明文 auth Header 和相对证书路径均
fail closed；非 Local agentd 使用独立的无凭据 Worker policy，只允许 credential-free HTTPS 或带显式端口的
IP-loopback HTTP relay，并拒绝 Header、client certificate/key 与 insecure override。Region/retention 仅作为 bounded resource metadata 输出；生产 collector 的 IAM、
Network、真实落盘 Region 与删除执行仍必须由候选部署 annex 验收。Kubernetes 主生产清单已接入同一配置面：
endpoint 默认为空，protocol/Region/retention 来自 Target UUID ConfigMap，客户端身份由 agentd 文件系统之外的
relay/service mesh 持有；`validate_observability_deployment.py` 的变异测试会拒绝默认启用、缺项、client identity 与 insecure/header
认证回退。动态 native/Warm Worker Pod 与 sandbox-operator standard 模板也使用 Target Namespace 内按完整 Target UUID 命名、可选的
operator-owned ConfigMap，避免共享 Namespace 的 Target 共用 exporter policy；Pod identity 会拒绝资源改名、必需引用和凭据注入，Control Plane 不读取或
持久化凭据。Docker/SSH agentd 则从 operator-only 根目录派生 Target UUID 路径，以只读 bind/远端文件和 agentd
白名单解析装配，Target JSON 不能选择宿主机路径；Cocoon guest 仍需自己的 Target-local authority。收据永久保持
`enterprise-otel-deployment-wiring-validated-not-collector-accepted`。

Artifact 对象存储也已增加源码与本地运行门禁：远程 presign 最长 15 分钟，Enterprise endpoint 必须为无凭据
HTTPS origin，静态 access key 只有携带 session token 的临时凭据才可启动，Kubernetes base 不再注入 Artifact
静态密钥。单节点 Compose 用 pinned `minio/mc` 一次性创建 private bucket、独立非 root application user、精确
`tenants/*` policy 与 exact-origin CORS，Control Plane 等待 bootstrap 成功且永不接收 root credential。静态校验器的
8 个 mutation case 与临时 MinIO 运行已验证生产前缀 presigned lifecycle，并拒绝前缀外写入、建桶和 Admin API；
assessment 仍为 `artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted`，真实云 IAM/bucket/KMS/Region/
Network/Access Log 仍须由候选部署 annex 和负向探针证明。

Worker 供应链不复制 Stage 3 的构建/签名/扫描体系；Stage 6 新增独立证据绑定器，把同一 clean commit 的 v2
release manifest、Registry release-gate 与 Vault/KMS admission 三份 JSON 通过 exact-byte Registry report hash 和
Worker digest 串联。校验器要求 cached/no-cache 双架构可重复摘要、每平台 SPDX/SLSA attestation、production KMS +
Rekor proof、零例外且 24 小时内的 Trivy 双平台扫描，以及 signed 允许和 unsigned/wrong-key/tag-drift/四类
controller 全部拒绝。收据固定为 `evidence-validated-not-worker-supply-chain-approved`；它不证明生产 Registry、
KMS、Rekor、集群或操作者 authority。candidate bundle 已显式升级为 v3，把该收据作为第十份必需输入；Python
保留 v2 历史读取，但受保护发布、Release Governance 与 PostgreSQL/SQLite 新候选只接受 v3。Migration `000134`
要求活动 v2 候选先关闭或替换，终态 v2 只作为不可变历史保留。
最终候选还必须在 `observing` 状态绑定 Migration `000135` 约束的 Final Review v1 收据，之后才能以完全相同的
决定摘要和残余风险进入 `released`。

Tenant 安全审计的当前聚焦矩阵位于 `docs/release-matrices/stage-6-tenant-isolation-v1.json`；110 条高风险证据引用
覆盖 622 个操作组，其中 94 条无环境依赖的本地可执行证据通过，16 条 PostgreSQL 环境门槛证据在常规收据中明确
保持 source-only；2026-07-31 已另用临时 PostgreSQL 17 执行 composite-FK、跨 Tenant interaction renewal 与
Support operator authority 三条精确门禁，2026-08-01 又执行 Migration 111 发布审批、Migration 112 合规
start-gate、Migration 113 Provider 商业授权、Migration 114 职能授权/三类审批数据库门禁、Migration 115
影响域派生的 Privacy/Legal 第五审批、Migration 116 exact-byte evidence receipt 绑定、Migration 117 事件
公开证据并发/不可变门禁、Migration 118 SLO、Migration 121 Recovery、Migration 123 Penetration 以及 Migration
125 Capacity、Migration 127 Incident exercise、Migration 129 Operations exercise 与 Migration 131 Billing exercise
收据/分权审批，并分别验证 Migration 120/122/124/126/128/130/132 的 Release exact-candidate 门禁，均通过。校验器
还要求所有 27 个直接接受 `identity.Principal` 的生产服务包至少有一条分类证据，并清点 175 个同类导出
`Service` 方法：174 个已映射到直接调用它们的聚焦测试，1 个仅供已授权且持锁调用方使用的事务 hook 以源码契约
单列，未分类数为 0。该规则关闭的是导出方法清单，不代表每个分支、身份、并发交错与部署面都已穷尽。登录
中间件另对 122 条显式 Tenant 路由统一执行 pre-handler 活动 Tenant 拒绝；收据仍固定为
not-audit-passed，不能替代全资源族矩阵、部署策略和独立测试。
Worker Execution request receipt 另经 Migration `000133` 绑定 Tenant、Execution、Generation 与 Target，重放前
重新锁定并验证 Execution 及 Session 当前 Target；相同上下文保留幂等语义，重新注册、successor Generation 与 Target 迁移后的旧
请求 fail closed。该门禁不替代其他资源/部署边界与独立渗透测试。
Migration `000133` 及同一 Generation/Session Target replay 测试已在临时 PostgreSQL 17 上通过，测试 schema 与
容器均已清理；这仍是本地数据库证据，不冒充生产 Worker fleet 验收。
成员离职身份矩阵也已补齐 Desktop 边界：人工或 SCIM 停用在同一事务撤销 Web/Desktop Session、用户
Credential 与待兑换 Enrollment，并分别写入 Audit 计数；成员重新启用不会恢复旧 Enrollment、Session、
Organization Membership 或 Credential。跨 Tenant 的 user-global Device 注册不授予独立访问，只允许 Platform
Admin 或用户认证后的 Disconnect 撤销，客户 Tenant offboarding 不越权修改。新增 SQLite 产品路径与
PostgreSQL 17 连接态/待兑换链接用例均通过；本结果关闭适用身份类型的工程缺口，但不替代对象存储、部署策略
和独立渗透审计。

容量与长稳工程基线由 `docs/contracts/capacity-long-duration-acceptance-v1.md` 冻结，并由
`scripts/stage6-capacity/validate_capacity_evidence.py` 校验 forecast+headroom、持续时长、阶段、外部探针覆盖、
SLO/饱和度/公平性声明及逐文件 hash；收据不自动宣称控制通过，生产等价运行与签署仍是外部 GA Gate。
Migration `000125` 现把补全后的 exact receipt 与重算投影接入 Platform Capacity Governance，Migration `000126`
把同候选内部批准接入 Release gate；两者仍不验证真实环境、遥测、签名、执行或外部审批权威。

事件演练工程基线由 `docs/runbooks/enterprise-incident-response.md` 冻结，并由
`scripts/stage6-incident/validate_incident_exercise_evidence.py` 校验独立 Status Board、角色分离、paging/公开节奏、
订阅者投递、恢复观察与逐文件 hash；收据不自动声明 operations ready。Migration `000127` 将 exact receipt 与重算
投影接入 Platform Incident Exercise Governance，Migration `000128` 把同候选内部批准接入 Release gate；两者仍不
验证外部 Status Board/paging authority、真实投递、签名、执行或外部审批权威。

运营演练工程基线固定为 49 项内部 self-hosted 日常操作，不把“导入/审批本收据”的 Release 元治理动作加入被治理矩阵，避免
自引用死锁。Migration `000129` 将 exact Operations browser exercise receipt、三类 Artifact、双 HTTPS origin、
11 个分离账号、48 项正向/固定负向角色、全局唯一 request ID、零开发工具回退、Support 四眼闭环及原始
Operations/Security 签署提升为 Platform Operations Exercise Governance；Migration `000130` 把同候选内部批准接入
Release gate。两者仍不验证真实部署、浏览器 Session、Audit/evidence authority、签名、执行或外部审批权威。

Migration `000131`/`000132` 的 Billing Exercise Governance 与旧 Stripe 门禁仅作为历史迁移兼容面保留，
不属于 `internal-self-hosted` 产品能力，也不得进入当前候选发布证据或 UI。Migration `000152`/`000153`
已将同候选的用量、Token、Provider 成本与内部平台成本证据接入 Governance 与 Release gate。

第三方渗透验收由 `docs/contracts/third-party-penetration-acceptance-v1.md` 冻结，并由
`scripts/stage6-penetration/validate_penetration_evidence.py` 绑定 Stage 5 依赖、独立评估方、四类候选 Artifact、
六类攻击面、发现项处置与逐文件 hash；校验器保留开放高危或不完整范围为失败证据，真实第三方报告与 Security
签署仍是外部 GA Gate。

生产 SLO 证据由 `scripts/stage6-slo/validate_slo_evidence.py` 固定 30 天窗口、外部探针、四类目标/样本量/
错误预算公式、告警与 companion signal 复核；失败或 not-assessable 窗口会被保留，真实生产遥测与发布审批仍是
外部 GA Gate。

On-call 与公开事故沟通演练由 `scripts/stage6-incident/validate_incident_exercise_evidence.py` 固定独立 Status
Page、角色分权、分页/首报/更新时限、六类公开组件、订阅者投递和恢复观察；tabletop 或失败演练只保留工程
证据，真实外部配置、实名 rota 与审批仍是外部 GA Gate。

各控制收据现在由 `docs/contracts/stage-6-candidate-evidence-bundle-v4.md` 再绑定到同一 clean commit、`bun.lock`、
环境 ID、HTTPS origins、Regions、Migration tail 和九类 Artifact digest。`bun run stage6:candidate:prepare -- ...`
会从唯一证据根计算十一份输入摘要，在发布任何输出前完成 v3 语义校验，并拒绝覆盖既有候选文件、路径逃逸、
符号链接、重复输入及跨候选拼接 Billing、Desktop、Operations、Incident、Recovery、Residency、SLO、Capacity、
Penetration 与 Worker supply-chain 收据；其
`evidence-consistent-not-ga-approved` 结果只证明候选身份一致，不代表任一外部控制或最终 GA 已获批准。
受保护发布侧的 `stage6-candidate-release-binding.v2` 不再只检查顶层 eligible 与 Worker 投影：它独立重验十份
投影的 schema、固定 non-pass assessment、安全唯一路径、非零摘要、ready 和时间闭包，并校验候选 Artifact/
origin/Region/Migration、Release Evidence、Desktop/Residency 交叉字段及 Recovery/Residency 外部权威边界；真实
Python 准备器输出必须通过 TypeScript verifier 的跨语言契约测试，空投影不能再进入环境审批记录。
`stage6:environment:prepare` 又从该 verified receipt 和独立输入的 candidate/commit/run ID 生成不可覆盖、`0600`、
带 sidecar 的 Environment 配置包，五个变量、单行 base64 secret 与结构化审批 comment 不再人工重算；stdout
不输出 secret。`stage6:environment:apply` 在任何写入前要求管理员已通过 GitHub UI 禁用 bypass（官方 Environment
更新 API 不暴露该开关），要求两到六个唯一 reviewer、启用 prevent-self-review 与 protected-branch-only，并使用
GitHub API 重验 exact first-attempt active release.yml run 的仓库、commit 与 source branch protection（enforce admin、
dismiss stale review、至少一票审批、strict status checks、禁止 force-push/delete），使用 stdin 设置 secret，并回读
Environment、变量和 secret presence 后生成私有 application receipt。该收据仍不授予部署审批，且不把 GitHub
不可回读的 secret value 表述为已远端验证。受保护 release job 还会独立获取并复核同一 run/branch 响应，v2
Environment approval 与 v4 Release approval 固定 source commit/branch，不信任应用器收据本身。
其中 Recovery v2 收据固定完整候选、恢复后的 release identity、单一 recovery subject、五个分离角色审批与
完整时间窗口；源码验证仍不能证明真实备份 authority、审批 authority、密码学签名或生产恢复结果。
Desktop 的 Enterprise GA 发布还新增同一 workflow 受保护门禁：四平台签名 Artifact 上传后暂停在
`stage6-enterprise-ga` environment，外部验收把 candidate ID、commit、build run ID、bundle receipt SHA-256、
实际 v2 receipt 的单行 base64，以及由四份 provenance 计算的 Desktop artifact-set SHA-256 写入受保护环境并经独立审批后，
workflow 会在授权记录写入前和发布前两次解析 receipt 并核对同一批二进制，才发布原 Artifact 和创建 tag。重新构建
即视为新候选，不能复用旧收据。

Updater metadata 不再在最终审批后改写：独立 `finalize_release_assets` job 先完成 macOS manifest 合并和专用
channel 复制，再以 `release-final-asset-set.json` 覆盖全部最终公开字节；受保护审批记录同时绑定 raw Desktop
artifact-set 与 final asset-set，发布 job 只下载、复核和上传，任何 YAML 新增、缺失或修改都会 fail closed。

`bun run stage6:engineering:check` 已把兼容性、企业文档、运营 UI、路由鉴权、Artifact/Observability 部署、Tenant 隔离源码校验及十四组
Stage 6 Python 校验器测试接入普通 CI 与发布 preflight。它只输出
`stage6-engineering-source-gates-passed-not-ga-approved`，不能把 source consistency 升级为外部控制通过。

## 3. 证据边界

`docs/release-checklists/stage-6-enterprise-ga.md` 是 GA 门禁模板。开发期的单元测试、fixture、静态检查与本地
集群结果只能关闭对应实现项，不能替代生产恢复、第三方渗透、真实长稳、外部 Status Board 或合规认证证据。
每个真实发布必须复制模板，并固定 Commit、镜像 Digest、Migration tail、执行人与证据链接。
最终评审前还必须生成同一候选的 v2 release-evidence manifest 与 candidate-bundle consistency receipt；两者均不
替代 Legal/Privacy、Operations、Security、Product 与 Engineering 的独立签署。
