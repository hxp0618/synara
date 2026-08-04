# Stage 7 Developer API SSH 验收

- 日期：2026-08-04
- 分支：`codex/stage-7-external-sdk-platform`
- Worktree：`synara-stage-7`
- 最终 Run：`stage3-provider-acceptance-e443e93a-d9a7-4b06-bfd9-e9cd754115a8`
- 结果：**PASS**（16/16 case，150420 ms）

## 验收范围

本次使用受验收器完整托管、最终自动删除的 OrbStack Ubuntu 24.04 arm64 VM，运行真实 Control
Plane、SSH、systemd、agentd 和 Worker Protocol。Provider 行为使用确定性 fixture，因此本报告证明
Developer API 与执行基础设施链路，不是 Codex App Server 或 Claude Agent SDK 的真实 Provider
release gate，也不代表生产 GA。

执行命令：

```bash
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target ssh \
  --suite fixture \
  --ssh-machine-name synara-stage7-sdk-live5 \
  --ssh-developer-api-provisioning \
  --output-dir .tmp/stage7-live-ssh-developer-api-complete-20260804 \
  --timeout 900
```

## Stage 7 关键证据

- 临时 Service Account 使用 tenant-scoped `owner` 与 `api.access`，一次性 `syna_sa_` token 未写入报告。
- Service Account 通过 `public-beta` Developer API 创建 SSH Execution Target。
- durable provisioning operation 首次 attempt 为 1，稳定 Idempotency-Key 重放得到同一 operation。
- operation 终态为 `succeeded`，Target 从 `offline` 进入 `active`，Worker online 且 post-registration
  heartbeat 已建立。
- 公共 operation 结果不投影 SSH configuration、私钥或内部 systemd service name；报告 secret scan 通过。
- 错误 Host Key 的负例被 `ssh_connection_failed` 拒绝，未在远端留下目标文件。
- 后续 fixture 覆盖 text/tool/usage/Artifact、Approval、large terminal、user input、Provider error、
  Worker replacement、替换后的 Workspace continuity、Control Plane restart 和第二 Turn continuity。
- cleanup case 通过，最终 `orbctl list --format json` 返回空数组。

## 同轮门禁

- `go test ./...`：通过。
- `bun run stage7:developer:check`：通过；生成 41 条公开 operation。
- Runner 单测：277/277 通过。
- TypeScript SDK：17/17 通过；Python SDK：11/11 通过。
- Developer Docs：Astro 类型检查、2 个内容测试、12 个静态页面与 Redocly API Reference 构建通过。

## 未覆盖边界

- 真实 Codex/Claude Provider Credential 与 Provider release gate。
- 外部生产控制面、生产证书、生产 DNS 和外网 Webhook egress policy。
- npm/PyPI ownership、OIDC Trusted Publisher 仓库外登记和实际发布。
- 人工产品、安全、法务和 GA 审批。
