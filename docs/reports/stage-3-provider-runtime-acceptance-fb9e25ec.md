# Stage 3 Real Provider Local Product-Path Acceptance

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `fb9e25ec3489713c557072ffe02e0cedacff6d62`
- Result: **PARTIAL — REAL LOCAL TWO-TURN SMOKE PASS, RELEASE GATE OPEN**

## 1. 结论

Commit `fb9e25ec` 上的真实 Codex App Server 与 Claude Agent SDK 均通过同一套
`real-provider-smoke`：

```text
Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

两个 Provider 都完成第一 Turn、Control Plane restart 和第二 Turn；第二 Turn 明确使用
`native-cursor / cursor_usable`，并精确复现第一 Turn 的 marker。两份报告都记录
`worktreeDirty=false`、连续 Session Sequence、临时状态删除和零 Secret finding。

该结果关闭了“真实 Codex/Claude 尚未经过 Local Supervisor/agentd”的最小两轮 smoke 缺口，但不等于
完整 Local Release Suite，更不等于 SSH、Docker、Kubernetes 四 Target Release Gate。

## 2. Clean-commit evidence

| Provider     | Runtime                                  | Cases | Session Sequence | Result |
| ------------ | ---------------------------------------- | ----: | ---------------- | ------ |
| Codex        | Codex CLI `0.144.4`                      | 12/12 | `1..42`          | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | 12/12 | `1..41`          | pass   |

共同断言：

- Source Git SHA 为 `fb9e25ec3489713c557072ffe02e0cedacff6d62`。
- `worktreeDirty=false`。
- Worker Manifest 的 Worker Protocol 与 Runtime Event version range 都包含 `2`。
- Provider Runtime 为 `available=true`、`compatible=true`，Release Policy 为 `enabled=true`。
- 第一 Turn 使用 `requested=native-cursor`、`selected=authoritative-history`、`reason=cursor_absent`。
- 第一 Turn 的 canonical `content.delta / assistant_text` 拼接结果与 terminal output 都精确匹配 marker。
- Control Plane restart 后，第二 Turn 使用 `selected=native-cursor`、`reason=cursor_usable`。
- 第二 Turn 不重新提供 marker，只要求重复上一轮答案；输出仍精确匹配，证明 Provider 会话连续性。
- 每个 Execution 只有一个终态，Session Sequence 无空洞或重复。
- `environment.cleanup` 记录 `stateRemoved=true`。
- `security.output-secret-scan` findings 为空。

本机原始报告：

| Provider     | JSON report                                                            | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| ------------ | ---------------------------------------------------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Codex        | `/tmp/synara-stage3-fb9e25ec-local-real-codex/acceptance-report.json`  | `2c8348b4038f9ef5475dd445946fdaf9346b863d64180ed8b0fe3e6dbfa8f788` | `e23aa8515fe77d2d5e541ee7250a75372b1324780684687f24683b779d192f2e` |
| Claude Agent | `/tmp/synara-stage3-fb9e25ec-local-real-claude/acceptance-report.json` | `9d412610403d79654fcf75fc5b73a59836a729283ee5d63225054edc00431a2a` | `dff4204a43bf21505ce93e1c7d4ead76293696e2cb06fef17dbdcc7ad1411467` |

## 3. Claude ambient OAuth defect and fix

首次真实 Claude 产品路径运行暴露了一个实际缺陷：Provider Host 对所有 Claude Execution 都把
`CLAUDE_CONFIG_DIR` 设置为 agentd 的 Runtime Output Root。默认用户环境中的 `claude auth status` 为
OAuth 已登录，但相同环境一旦指向空的 Execution Output Root 就变成未登录，导致 Turn 失败。

修复后的边界：

- 有受控 Provider Credential 时，继续把 `CLAUDE_CONFIG_DIR` 指向 Execution Output Root，保留配置和
  运行输出隔离。
- 无 Provider Credential、明确使用本地 ambient OAuth 时，不覆盖用户的 Claude 配置查找路径。
- 单测分别锁定 controlled Credential 与 ambient OAuth 两条路径；修复后真实 Claude clean-commit smoke
  通过。

这不是 acceptance-only 特判；它修复的是 Provider Host 的实际认证环境构造。

## 4. Runner boundary

`scripts/stage3-provider-acceptance/acceptance_runner.py` 新增 `--suite real-provider-smoke`：

- 只支持真实 Codex 与 Claude Agent。
- 强制显式传入 `--runner-command-json`，避免误把 deterministic fixture 当作真实 Host。
- 禁止组合 fixture failure/canary matrix。
- 不创建 acceptance Credential；本次 Local 运行使用现有 ChatGPT/OAuth 登录。
- 生成每 Session 唯一 marker，校验 Runtime Event v2 assistant delta、terminal output、Worker fence、
  Provider Resume decision、Control Plane restart、Sequence continuity、cleanup 和 Secret scan。
- Markdown/JSON 报告显式标记为 narrow smoke，并声明不是完整 Release Gate。

## 5. DDL boundary

本次实现没有新增或修改数据库 DDL。Checked-in Stage 3 migration boundary 仍为 `000032`–`000040`；
本次只消费既有 Session Event、Execution Lease、Provider Cursor 和 Worker Manifest 数据路径。

## 6. 仍未关闭

1. 真实 Codex/Claude 的 Approval、Plan Mode User Input、Interrupt/Steer、Compact/Review 和故障恢复完整
   Local Release Suite。
2. 真实 Provider generated file、大 Diff、大 Terminal、Artifact/Checkpoint 与 auth/rate-limit/crash matrix。
3. 真实 Codex/Claude 的 SSH、Docker、Kubernetes Target Release Acceptance。
4. Registry-pushed immutable multi-arch Worker Image、签名、SBOM、reproducibility 和 rollout/rollback。
5. 多节点生产 Kubernetes、PDB、CNI enforcement、真实 Drain/Eviction 与升级压力。
6. 长 Session、多 Provider 并发、重复 Compact/Checkpoint/Resume、Retention/Cleanup 和 soak。

## 7. 发布决定

当前决定：**不批准将 Stage 3 标记为 Release Gate complete**。

可以确认的新增范围仅为：真实 Codex 与 Claude Agent 已在 clean commit 上通过 Local 产品路径的两轮
restart/native-Cursor smoke，且 Claude ambient OAuth 路径已修复并有测试保护。
