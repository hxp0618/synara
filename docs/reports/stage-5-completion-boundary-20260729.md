# Stage 5 Provider 沙箱与运行时隔离完成边界（2026-07-29）

状态：**DONE（当前正式支持面）**。

本记录只完成路线图口径收敛，不改写既有验收报告。运行期原始事实仍以
[`stage-5-k3s-runtime-isolation-physical-acceptance-20260729.md`](stage-5-k3s-runtime-isolation-physical-acceptance-20260729.md)
及其引用的机器可读报告为准。

## 产品边界

Stage 4 已冻结 Kubernetes 的正式支持范围为**自建 Kubernetes**，并明确不支持或宣传 EKS/GKE/AKS
专属 Workload Identity、原生 IAM/KMS/CNI/Region 集成。Stage 5 因此按同一范围验收：实现提供逐 Target、
逐 Ready/非 cordon Worker 的 fail-closed gate；operator 必须在每个目标集群接入时运行，证据只授权该
Target 当时枚举出的节点集合。

托管云专属能力仍是未来独立产品项目。没有托管云 context 不能推导出托管云已经验收，但也不再让一个
明确 deferred、当前不在产品面的能力永久阻塞 Stage 5。

## 完成证据

- 物理 Debian 12 K3s Target 的首尾 Ready/非 cordon Worker 清单一致，完整集合为
  `synara-k3s-1`。
- exact-node 资源压力：peer `12/12`，fork `120` started / `136` rejected，CPU limit finite，
  64 MiB probe `OOMKilled`。
- Codex 与 Claude Agent 构成当前准入 Provider 集合；两者在完整 Worker 集合上均通过
  `metadata-egress`、`credential-scope`、`malicious-issue-denial`，聚合 cell `2/2` PASS。
- metadata 与 credential verifier 都是 Worker 镜像内的 value-free 内置命令；终态为零 command output，
  不读取或持久化 Credential 值。
- 恶意 Issue 形态输入在 server automation product-path 中保持 `source=automation`、进入 escaped
  provenance boundary 并将 full-access 降级；真实 Provider denial case 则证明双敏感类别、显式拒绝与
  零执行副作用。
- 跨租户 scrub、Target 隔离等级、Provider outer-sandbox attestation、来源标注、结果 provenance、
  服务端权威敏感动作 classifier、Approval/audit/alert 链路均已按 Stage 5 计划完成。
- 最终验证已通过 Control Plane `go test ./... -count=1`、Acceptance Runner `269/269`、Stage 5 matrix
  `13/13`、shared `456 passed / 1 skipped`、workspace `bun fmt` / `bun lint` / `bun typecheck`、
  `git diff --check`。Provider Host 全包并行运行曾出现 5 个墙钟 timeout；同一受影响 runtime 文件隔离
  重跑 `32/32` PASS，因此记录为测试调度噪声，不隐去该事实。

## 后续上线门禁

- 每个新增自建 Kubernetes Target 必须对其完整 Ready/非 cordon Worker 集合重跑 Stage 5 matrix；
  节点集合变化、漏跑、Provider 集合缺格或 cleanup/Secret scan 失败均拒绝准入。
- 新增 Provider 必须先进入 content-trust delivery registry、outer-sandbox attestation 与真实逐节点矩阵。
- Stage 9 的真实 GitHub/GitLab webhook 尚不存在。未来 adapter 必须在上线前补齐 webhook signature、外部
  身份映射、幂等投递、automation provenance、无人值守 Provider sensitive request 与拒绝/零副作用的
  adapter-specific 端到端门禁；当前 Stage 5 结果不替它宣告通过。
- 镜像生产签名、Registry retention、Admission Policy、托管云专属集成与 microVM fast-provision 各自保留
  在既有 Stage 3/6/后续运行时项目中，不被本记录误报为已完成。
