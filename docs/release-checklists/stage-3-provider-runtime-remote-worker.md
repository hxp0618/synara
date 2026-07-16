# Stage 3 Provider Runtime / Remote Worker 发布检查单

每次发布复制本检查单，并记录 Commit、不可变镜像 Digest、数据库 Migration、执行人、时间和证据链接。
未满足项必须保持未勾选，不能用 deterministic fixture、单一 Target 或静态代码检查替代真实发布证据。

当前实现期证据汇总见
`docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md`。该报告明确保持
`PARTIAL / RELEASE GATE OPEN`，不能直接作为目标环境发布批准。

## 1. 发布身份与证据边界

- [ ] 发布分支、Commit SHA 和 Git 工作区状态已记录。
- [ ] Control Plane、Worker 和 Provider Host 使用同一已提交源码构建。
- [ ] Worker 镜像使用 Registry 返回的不可变 Digest，不使用 tag 作为唯一发布身份。
- [ ] Worker Manifest ID、Release Revision ID、Execution Target ID 和目标环境已记录。
- [ ] Provider Host Protocol 固定为 `2.1`，Worker Protocol 固定为 `2`，Runtime Event 固定为 `2`。
- [ ] 报告明确区分真实 Provider、deterministic fixture、Target 类型和是否经过 Control Plane/agentd。
- [ ] 当前已知限制、外部依赖和未执行项已由发布负责人接受。
- [ ] 没有把 Local Provider Host smoke 描述成 Local Supervisor、SSH、Docker 或 Kubernetes Release Gate。

## 2. 数据库与 DDL

当前工作树的 checked-in forward migration boundary 是 `000040`：

| Migration | 发布不变量                                                                              |
| --------- | --------------------------------------------------------------------------------------- |
| `000032`  | Compact、Review、Rollback、Fork Turn 形状、逻辑历史祖先和 Primary Control Command       |
| `000033`  | Provider Credential User/Organization/Tenant/Platform Scope、选择器和自动选择策略       |
| `000034`  | Worker operator revocation、logical identity tombstone 和 Token/Lease/Claim fencing     |
| `000035`  | Project/Target Credential Binding 与 immutable per-Generation Execution Grant           |
| `000036`  | `projects.git_credential_id` 兼容回填后退役为不可写 authority                           |
| `000037`  | immutable Worker Release Revision、CAS Policy、Transition History 和 release pinning    |
| `000038`  | disabled Binding 历史也可用的复合外键 lookup indexes                                    |
| `000039`  | 每个 Execution Target 最多一个 active `worker_image_pull` Binding，歧义升级 fail closed |
| `000040`  | Worker Release Transition 必须与当前 Policy 的版本和 promoted/canary 状态完全一致       |

- [ ] PostgreSQL 备份完成，并在隔离环境验证可恢复。
- [ ] `/ready.checks.schema.expectedVersion` 与当前镜像内 migration boundary 一致。
- [ ] `control_plane_schema_migrations` 中版本连续、Checksum 匹配，没有手工补写记录。
- [ ] PostgreSQL 真实 forward migration integration 全部通过。
- [ ] SQLite safety mirror tests 全部通过。
- [ ] `000037` 的 Revision、Policy、Transition、Worker/Execution release shape 和多 Revision Target 已验证。
- [ ] `000038` 的四个 Credential Binding 外键索引在 PostgreSQL 和 SQLite 均存在。
- [ ] `000039` 在重复 active Target Binding 上拒绝升级，修复歧义后可重试且新唯一索引生效。
- [ ] `000040` 在 Policy/最新 Transition 不一致时拒绝升级，并阻止写入不匹配的 Transition。
- [ ] PostgreSQL 不依赖 Runtime `AutoMigrate`；历史 migration 文件没有被修改。
- [ ] 回滚方案确认旧镜像可以读取已应用的新 schema，或已有经过评审的 forward fix；不得仅回滚 Deployment。

只读核对：

```sql
SELECT version, name, checksum, applied_at
FROM control_plane_schema_migrations
ORDER BY version;
```

## 3. Worker 构建与供应链

- [ ] Worker Image 已推送到目标 Registry，并记录 registry-returned Digest。
- [ ] 至少生成目标平台所需的 `linux/amd64`、`linux/arm64` manifest list；若只发布单架构，已记录审批。
- [ ] Base Image、Node.js、Codex CLI、Claude Agent SDK 和系统包均由锁文件或 Digest 固定。
- [ ] Worker Manifest 中的 Git SHA、OS/Arch、Image Digest、Protocol、Provider Runtime 和 Capability Hash 可追溯。
- [ ] SBOM、依赖漏洞扫描和镜像签名/来源验证完成。
- [ ] Worker 使用非 Root 用户，Workspace、Git Cache 和 Runtime Output Root 权限正确。
- [ ] 没有在镜像、Layer、Build Arg、Environment 或 Manifest 中写入 Credential。
- [ ] 私有 Registry 使用 Tenant/Organization-scoped Registry Credential 和 Target-scoped
      `worker_image_pull` Binding；Binding selector 与镜像 Registry Host 精确匹配。
- [ ] Kubernetes Registry auth 仅使用 OCI Basic；Bearer-only 和带自定义端口的 Registry auth 在当前
      Credential contract 下安全失败，未通过手工 Secret 或数据库改写绕过。

## 4. Credential、SecretGuard 与安全撤销

- [ ] Provider Credential 只通过匿名 FD 3 和 Provider-specific allowlist 传递。
- [ ] Worker Registration Token、Lease Token、数据库/对象存储凭据没有进入 Provider Host 输入或子进程环境。
- [ ] Git、Registry、Package Credential 只在对应 operation stage 解析，且使用 immutable Generation Grant。
- [ ] HTTPS AskPass、SSH Agent/Host Key、Registry pull auth 和 Package auth 的临时状态在阶段结束后清理。
- [ ] SecretGuard 覆盖 Provider stdout/stderr、Runtime Event、Terminal Preview/Artifact、错误和安全 Metadata。
- [ ] 输出报告、Control Plane/agentd/Provider Host 日志和集中日志平台均通过 Secret scan。
- [ ] Rotation 后旧 Credential version 不能被新 Execution 解析；Revoke/Expiry/Binding Disable 立即阻止新解析。
- [ ] Worker operator revocation 使用当前 `expectedIncarnation` 和唯一 `Idempotency-Key`，并记录原因。
- [ ] 已确认 Worker revocation 不可逆：logical identity tombstone 禁止同身份重新注册，不能用于普通滚动升级。
- [ ] `outcomeUnknownExecutions` 或 `checkpointUnconfirmedExecutions` 非零时已升级为人工恢复事件。

## 5. 自动化质量门禁

Go：

```bash
cd services/control-plane
go test ./...
go vet ./...
go test -race ./internal/secretguard ./internal/agentd

SYNARA_TEST_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
  go test -count=1 ./...
```

- [ ] 默认 Go 全量测试通过。
- [ ] `go vet ./...` 通过。
- [ ] SecretGuard/agentd Race Test 通过。
- [ ] 真实 PostgreSQL 全量测试通过，且测试数据库在运行后清理。
- [ ] `git diff --check` 通过。

TypeScript/Web：只能使用 `bun run test`，禁止 `bun test`。

```bash
bun run --cwd packages/contracts test src/providerHost.test.ts src/providerRuntime.test.ts
bun run --cwd apps/provider-host test \
  src/protocol.test.ts \
  src/runtimeEventV2.test.ts \
  src/codexAppServerRuntime.test.ts \
  src/claudeAgentSdkRuntime.test.ts
bun run --cwd apps/web test \
  src/lib/controlPlaneClient.test.ts \
  src/lib/controlPlaneProjection.test.ts \
  src/session-logic.test.ts \
  src/components/ChatView.logic.test.ts
```

- [ ] Contracts、Provider Host、Web/Projection 和设置页 focused tests 通过。
- [ ] Web Production Build 通过。
- [ ] 操作人已明确授权，并且最终一次 `bun fmt`、`bun lint`、`bun typecheck` 全部通过。
- [ ] Final gate 后没有再修改受格式化、Lint 或 TypeScript 检查覆盖的文件。

## 6. Acceptance 证据等级

当前仓库已有的实现期证据不能替代下列发布勾选项：

| 证据                                                | 当前结论                         | 发布边界                                                             |
| --------------------------------------------------- | -------------------------------- | -------------------------------------------------------------------- |
| 真实 Codex/Claude Local two-Turn product-path smoke | clean commit `fb9e25ec` 各 12/12 | 经过 Control Plane/LocalSupervisor/agentd，但不是完整 Local Gate     |
| 真实 Codex/Claude Generated File Checkpoint         | 当前工作区 matrix pass           | Ready Snapshot 已验；standalone Artifact 与大 Diff 仍开放            |
| 真实 Codex `0.144.x` `terminal-large`               | Explicit Unsupported             | Unified Exec 仅保留 1 MiB Head/Tail；不得牺牲 durable Approval       |
| Claude ambient OAuth `terminal-large`               | Explicit Unsupported             | 需 controlled Credential 绑定 Runtime Output Root                    |
| deterministic Local/Docker core suite               | 已通过                           | 证明共享 Control Plane/agentd/Host orchestration，不证明真实 Adapter |
| deterministic Provider fault matrix                 | malformed/oversized/crash 已通过 | 不是真实 Provider failure 分类                                       |
| deterministic Docker/Kubernetes failure matrix      | 已通过实现期运行                 | 不等于生产网络、真实 CNI 或正式 rollout                              |
| SSH fixture                                         | 2026-07-14 disposable VM 13/13   | 不是当前 Commit 的真实 Provider gate                                 |
| Kubernetes fixture                                  | clean commit `2763ebd3` 13/13    | 不是当前 Commit 的真实 Provider gate                                 |

真实 Provider × Target gate：

- [ ] Codex × Local：Discovery、Start、Send、第二 Turn、Restart、Interaction、Artifact 和错误分类。
- [ ] Claude × Local：同上。
- [ ] Codex × SSH：install/upgrade/revoke、Host Key、systemd restart、Workspace continuity。
- [ ] Claude × SSH：同上。
- [ ] Codex × Docker：replace、volume/checkpoint、network interruption、resource limits。
- [ ] Claude × Docker：同上。
- [ ] Codex × Kubernetes：Pod replacement、Drain、Eviction、Network、Image rollout。
- [ ] Claude × Kubernetes：同上。
- [ ] 所有运行均来自本次发布 Commit 和 registry-pushed immutable image。
- [ ] 多 Turn 长 Session、多 Provider 并发、长日志、Checkpoint/Resume 和 Retention soak 完成。
- [ ] 故障运行没有重复终态、双 Worker 写入、Generation 回退或 Credential 泄漏。

## 7. Web 与前后端联通

- [ ] SaaS Web 的 Project、Session、Turn、Compact、Review、Rollback、Fork 只调用 Control Plane API。
- [ ] SaaS handler 没有回退到 `readNativeApi()` 或本地 Provider discovery。
- [ ] Credential Scope、Auto-select、Project/Target Binding 和 Disable 操作可以从设置页完成。
- [ ] Worker 列表、operator revoke、Release Revision、Canary、Promote、Rollback 使用服务端权威状态。
- [ ] CAS conflict 会重新读取 `policyVersion`，不会覆盖并发运维操作。
- [ ] SSE 断开、刷新和 Server restart 后 Event Sequence、Interaction、Artifact 和 Worker 状态一致。
- [ ] 未配置 Control Plane 的本地模式没有回归，也不会写入 SaaS authority。
- [ ] 浏览器 Console 无相关 Error/Warning 或框架 Overlay。

## 8. Canary、Promote 与 Rollback

- [ ] 已按 `docs/runbooks/worker-release-rollout.md` 完成预检。
- [ ] 初始 promoted Revision 绑定已验证的 Worker Manifest 与不可变 Image Digest。
- [ ] Canary 使用更新的 Revision、`expectedPolicyVersion` 和非空原因，比例在 `1..100`。
- [ ] Canary Worker/Execution 的 Revision 与 Channel 可从 API 和运行证据追溯。
- [ ] Canary 观察窗口覆盖 Claim、Session continuation、Artifact、Interaction、Drain 和错误率。
- [ ] Promote 只针对当前 active canary Revision，并使用最新 Policy Version。
- [ ] Abort-canary 使用 rollback API 指向当前 promoted Revision；真正 rollback 只选择更旧 Revision。
- [ ] 任何 `409 worker_release_policy_version_conflict` 都先重新读取 Policy，不重放旧版本写入。
- [ ] Busy Worker 不被手工原地替换；失败时先停止 rollout，再按安全边界 Drain/Release/Recover。

## 9. 发布完成条件

- [ ] 所有 Required 项已勾选，未执行项有审批、负责人和截止时间。
- [ ] 验收报告来自最终 Commit，且报告、日志和资源清理均通过 Secret scan。
- [ ] Registry、Docker、Kubernetes、SSH 临时资源已按精确 owner/Target 标识清理。
- [ ] Release/PR 附带 DDL boundary、Runbook、Acceptance Report 和已知限制。
- [ ] 真实四 Target Provider gate、registry-pushed multi-arch 和生产 soak 未完成时，发布状态保持阻断。
