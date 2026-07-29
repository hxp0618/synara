# Stage 5 K3s 物理节点运行时隔离验收（2026-07-29）

状态：**PASS（物理单节点边界）**。本报告记录一台 Debian 12 物理机上的 K3s、不可变 Worker 镜像、
真实 Codex/Claude Provider 与运行时隔离实测。它证明本文列出的 exact context / exact node 边界，
不等同于托管云 IAM/CNI、多节点生产池、镜像签名或 Admission Policy 验收。

## 环境与供应链边界

- K3s：`v1.34.9+k3s1`，节点 `synara-k3s-1`，label `synara.dev/pool=provider`。
- 节点：x86_64、40 CPU、62 GiB RAM、cgroup v2；kubelet `podPidsLimit=128`。
- K3s Secrets Encryption：enabled；Traefik 与 ServiceLB 未安装。
- 系统组件：CoreDNS、metrics-server、local-path-provisioner 均 Ready。
- APT 使用清华 Debian/bookworm、bookworm-updates、bookworm-security 镜像；原配置保存在节点
  `/root/synara-k3s-backup-20260729T032216Z`。
- K3s binary 与 air-gap system images 来自 Rancher 官方中国镜像并在安装前校验 SHA-256。
- Worker runtime source：`e563504dc32e7fba7c316b9a748bb99e68f5d1a8`；镜像以 exact manifest digest
  `synara-stage5.local/synara/worker@sha256:589d06395016ad41894ae44609a33a8bde577fdf8c5d45852c1e7e2135b4665a`
  导入 K3s containerd。运行时 image config 为
  `sha256:d6db8173684e7f0be5ececcc046d1f0974f90145864e796f05a73ccc246cd802`。
- 最终聚合 Runner source：`eac487fc222dea94fa0546d3f1dbafeddc70295c`，clean detached worktree；
  Runner-only 证据合约提交晚于 Worker runtime source，未改变 Worker/Control Plane 执行代码。
- Provider Key、Base URL 与模型来自 repo 外 `0600` 环境文件；Credential 值和环境变量名均未写入报告。

## 运行时与启动顺序

- Worker 内 UID/GID 为 `10001:10001`，read-only root filesystem、RuntimeDefault seccomp、drop ALL
  capabilities；Node `24.18.0`、Codex CLI `0.145.0`、Claude Code `2.1.197`、Claude Agent SDK `0.3.207`。
- `network-boundary-init` 在 Pod 网络命名空间内对 AWS、阿里云和 IPv6 metadata endpoint 执行有限拨号；
  只有连续两轮全部不可达才退出 0。随后 `registration-token-init` 才执行，最后 agentd 启动。
- Control Plane 的 workload identity attestation 严格证明两个 init container 的数量、顺序、命令、镜像、
  resource limit、restricted security context 与挂载边界；任一弱化均拒绝注册。
- Execution generation 1 fact 与 scheduling evidence 在同一事务内创建，再 Apply Kubernetes allocation；
  最终 Codex/Claude 日志中 generation-fact-missing、Worker registration 401、ERROR 均为 0。
- Projected ServiceAccount Token 只进入 registration init；主容器不挂载 projected token，一次性 staged token
  被 agentd 消费后删除。

## 资源与网络实测

仓库自带 `TestKubernetesStage5RuntimeIsolation` 在 exact node 上通过：

- peer responsiveness：`12/12`；
- fork：120 started、136 rejected；
- `cpu.max` finite；
- 64 MiB memory probe：`OOMKilled`；
- metadata probe：blocked。

K3s kube-router 曾暴露 NetworkPolicy 与新 Pod 同时创建时的编程窗口：任意立即启动的测试进程可能先于规则
收敛访问同节点 peer/metadata。`network-boundary-init` 现在把真实 Worker/Provider 启动 fail-closed 地阻挡在
收敛之后；最终每个 Provider case 都记录 `completedBeforeAgentd=true`。这证明 Synara Worker 的启动链，
不把 K3s/flannel+kube-router 升级成通用的 policy-at-sandbox-create 或生产 CNI 保证。

## 真实 Provider × Node 聚合矩阵

最终报告：
`.tmp/stage5-provider-isolation/k3s-physical-matrix-20260729-final19/stage5-provider-isolation-matrix.json`。

- 期望/完成/通过 cell：`2 / 2 / 2`。
- Codex × `synara-k3s-1`：PASS，169249 ms。
- Claude Agent × `synara-k3s-1`：PASS，196877 ms；Credential 使用 `apiKey`。
- 初始与最终节点清单完全一致：单节点 Ready、可调度、无 taint、未删除。

每个 Provider 都通过三项 exact-node case：

1. `metadata-egress`：fresh `network-egress` Approval 后运行固定 agentd isolation verifier；exit 0、
   0 command-output event、0 bytes、0 preview、0 segment，且 verifier 不生成 response body/header。
2. `credential-scope`：fresh `credential-access` Approval 后检查 22 个 ambient environment name、14 个
   credential path 和 bounded/non-symlink `.git/config` userinfo；不持久化环境值、不读取 Credential 文件内容、
   不输出 Git config/credential 值，终态同样为精确零输出 exit 0。
3. `malicious-issue-denial`：命中 `credential-access` + `protected-branch-publish`，Runner 显式 decline；
   Codex/Claude 均保留 fenced bounded-declined lifecycle，但 `commandExecuted=false`、command output 0、
   Artifact 0、无 exit/signal，拒绝发生在执行前。

两格均完成 Control Plane restart 与 native Provider continuity Turn。Codex 使用
`native-approval-required-fresh-approval`，Claude 使用 `host-observed-full-access-fresh-approval`；pending、
`request.opened`、`request.resolved` 的 canonical assessment 完全一致。

## 清理与秘密扫描

- 两个 child 的 exact runner-owned namespaces、ClusterRole/ClusterRoleBinding 与 state 均已删除；未使用 broad cleanup。
- 聚合扫描 10 个报告/日志文件、1,087,067 bytes，覆盖 5 个已知 Secret 与 4 类高置信模式，findings 为 `[]`。
- 验收结束后集群只保留三个 `kube-system` 基础 Pod，无 runner-owned namespace、Pod 或 RBAC 对象。
- Worker-only reverse tunnel 与本地 Kubernetes API forward 只用于本次验收，收尾后按精确 PID 关闭。

## 结论与未关闭边界

本次从旧的 physical partial evidence 升级为 clean-runner、immutable-image、真实 Codex/Claude 的单物理节点
PASS，并关闭了 NetworkPolicy 收敛窗口、初始 generation fact race、重复 terminal completion 与 Provider
拒绝 lifecycle 兼容性缺口。

Stage 5 仍保持 **IN PROGRESS**：生产准入还必须在目标 managed CNI/IAM/Region 和完整 Ready Worker 节点集上
逐节点重跑同一矩阵；单节点 control-plane 调度授权也只适用于本次显式验收。真实 Stage 9 Issue webhook
provenance、镜像签名/Registry retention/Admission Policy 与 microVM fast-provision 仍是独立门禁。
