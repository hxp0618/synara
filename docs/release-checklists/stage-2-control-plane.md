# Stage 2 Control Plane 发布检查单

每次发布复制本检查单并记录镜像 Digest、数据库版本、执行人、时间和证据链接。未满足项不得用
“代码看起来正确”代替运行证据。

## 1. 范围和配置

- [ ] 发布 Commit、不可变镜像 Digest 和目标环境已记录。
- [ ] `DeploymentProfile` 与 Metadata、Artifact、Queue、Replica 配置匹配。
- [ ] Enterprise 禁止 Dev Bootstrap，Public URL 使用 HTTPS。
- [ ] Cookie Secure/SameSite/Path、Trusted Proxy CIDR 和 Ingress Timeout 已复核。
- [ ] PostgreSQL 备份完成，并验证可恢复。
- [ ] Migration `000001` 至 `000016` 完整、Checksum 未变化。
- [ ] Provider Cursor Key、Credential KMS、Worker Registration Token 来自 Secret Manager。
- [ ] MinIO/S3 Bucket、Region、CORS、Lifecycle 和 Workload Identity 已验证。
- [ ] 真实 AWS S3 部署已使用明确授权的测试 Bucket 运行 Live Store；若不适用，记录原因和审批人。

## 2. 自动化验证

```bash
cd services/control-plane
go test ./...
go test -race ./...
SYNARA_TEST_DATABASE_URL='postgres://...' go test -count=1 ./...
```

- [ ] Go 全量测试通过。
- [ ] Go Race Test 通过。
- [ ] 真实 PostgreSQL Integration Test 通过。
- [ ] MinIO/S3-compatible Live Store 测试通过。
- [ ] 若目标使用 AWS S3，真实 AWS S3 Live Store 通过。

Web/Proxy：

```bash
cd apps/server
bun run test src/controlPlaneProxy.test.ts

cd ../web
bun run test src/lib/controlPlaneClient.test.ts \
  src/controlPlaneStoreProjection.test.ts \
  src/components/ChatView.logic.test.ts
bun run build
```

- [ ] Proxy Cookie、多 `Set-Cookie`、SSE 和流式下载测试通过。
- [ ] Tenant Context、Projection Authority、SSE 重连和本地主路径测试通过。
- [ ] Web Build 通过。
- [ ] 按仓库约束，仅在操作人明确要求时运行 `bun fmt`、`bun lint`、`bun typecheck`。

## 3. 运行验收

- [ ] Single-node 完整 Acceptance：`deploy/saas/acceptance.sh`。
- [ ] 双副本 Compose Acceptance：`deploy/saas/multi-replica-acceptance.sh`。
- [ ] Worker/MinIO/PostgreSQL 故障演练：`deploy/saas/failure-acceptance.sh`。
- [ ] Kind 双副本 Acceptance：`KIND_BIN=/path/to/kind deploy/kubernetes/kind-acceptance.sh`。
- [ ] 删除一个 Control Plane Pod 时 Service 连续 Ready。
- [ ] 数据库不可用时 Readiness 降级，恢复后已有 Session/Worker 状态保留。
- [ ] MinIO/S3 不可用时 Readiness 降级，恢复后 Pending Artifact 可继续完成。
- [ ] Worker 失联后 Execution 进入 Recovering，新 Generation 接管，旧 Generation 被拒绝。
- [ ] 两副本 Migration 只产生 16 条唯一版本记录。
- [ ] Reconciler ServiceAccount 的 Pod/Secret 权限符合清单，不向 Worker 注入控制面 Token。

## 4. 浏览器主流程

- [ ] SaaS 登录成功，显示 User、Tenant 和 Organization Context。
- [ ] 主界面创建 Control Plane Project、Session 和 Turn。
- [ ] Worker Claim/Start/Event/Complete 后 SSE 输出可见。
- [ ] 页面刷新后从 PostgreSQL/Session Event 恢复。
- [ ] SSE 断开只显示 Reconnecting，不把 Execution 误判为完成。
- [ ] Control Plane 不可用时不创建前端孤儿 Session。
- [ ] 未配置 Control Plane 的隔离实例无登录门，可创建本地项目并从 SQLite Snapshot 刷新恢复。
- [ ] 浏览器 Console 无相关 Warning/Error、无框架错误 Overlay。

## 5. 可观测性和安全

- [ ] HTTP、DB Pool、Login、Execution、Worker、Lease、SSE、Artifact、Outbox、Background 指标存在。
- [ ] Prometheus Rules 已由 Operator 加载，并验证关键告警表达式。
- [ ] 随机 Sentinel 日志审计未发现 Credential、Token、Prompt 或 Presigned Query。
- [ ] Ingress、日志 Sidecar 和集中采集平台也完成同类审计。
- [ ] Outbox Pending/Oldest/Retry/Dead Letter 均为可接受水平。
- [ ] 没有 Stale Worker、Expired Lease 或 Expired SSE Lease 异常堆积。

## 6. 发布和回滚

- [ ] 先发布一个副本并观察 Readiness、5xx、SSE Catch-up 和 Outbox。
- [ ] 第一副本稳定后再继续滚动第二副本。
- [ ] 发布后执行登录、Session Replay、Worker Heartbeat 和 Artifact Smoke。
- [ ] 回滚镜像与数据库兼容性已确认。
- [ ] 故障时停止 rollout；不手工修改 Event Sequence、Lease Generation、Outbox Published 状态。
- [ ] 验收报告和已知限制已附到 Release/PR。
