# Control Plane 生产运维 Runbook

适用范围：`single-node` 与 `enterprise` 部署。`personal` 使用同一领域模型，但默认不需要
PostgreSQL、对象存储或多副本处置。

## 1. 不变量与处置原则

- PostgreSQL 是 Tenant、Session、Execution、Lease、Event、Outbox 和登录 Session 的权威来源。
- MinIO/S3 是 Artifact Payload 的权威来源；数据库只保存受验证的 Metadata。
- `/health` 只表示进程存活，`/ready` 才表示可以接收生产流量。
- 数据库或对象存储不可用时允许 `/health=200`、`/ready=503`，不得绕过 Readiness 强行导流。
- Session Event Broker、进程内 Cache 和本地文件都不是 SaaS 权威来源。
- 不在命令行、工单、聊天或日志中粘贴 Login、Worker、Lease、Provider、KMS 或 Presigned URL 凭据。
- 先保留证据，再执行重启、扩缩容或 Replay。禁止直接修改 Event Sequence、Lease Generation
  或 Outbox Published 状态。

## 2. 五分钟初诊

```bash
kubectl -n synara-system get deploy,pods,svc
kubectl -n synara-system get events --sort-by=.lastTimestamp | tail -n 50
kubectl -n synara-system get --raw \
  /api/v1/namespaces/synara-system/services/http:synara-control-plane:3780/proxy/health
kubectl -n synara-system get --raw \
  /api/v1/namespaces/synara-system/services/http:synara-control-plane:3780/proxy/ready
kubectl -n synara-system logs deployment/synara-control-plane --since=15m
```

Compose：

```bash
docker compose --env-file deploy/saas/.env -f deploy/saas/docker-compose.yml ps
docker compose --env-file deploy/saas/.env -f deploy/saas/docker-compose.yml logs --since=15m control-plane
```

需要记录：故障开始时间、受影响 Tenant/Session/Execution ID、部署版本、Pod 名称、`requestId`、
`errorCode`、Readiness 响应和关键指标。不要记录请求 Body、Prompt 或凭据。

## 3. 数据库或 Migration 失败

症状：`/ready=503`，Schema Check 显示缺失/Checksum 不一致，或数据库写能力失败。

1. 确认 PostgreSQL 网络、证书、权限、连接数和磁盘状态。
2. 读取 `/ready.checks.schema.expectedVersion`，并与当前镜像内最高的 forward migration 对照；
   不使用历史 Stage 2 固定数量。当前仓库迁移已连续到 `000041`，后续新增迁移时以运行构建返回值为准。
3. 查询已应用版本，只读取，不手工补写：

   ```sql
   SELECT version, name, checksum, applied_at
   FROM control_plane_schema_migrations
   ORDER BY version;
   ```

4. Checksum 不一致时停止发布。恢复与该数据库匹配的镜像或执行经过评审的数据迁移；不得修改
   历史 Migration 文件后继续启动。
5. Advisory Lock 超时先确认是否有另一副本正在迁移，再检查长事务；不要删除 Lock 或并发执行 DDL。
6. 数据库恢复后等待所有副本 `/ready=200`，并验证一个已有 Login Session、Session Event Replay
   和 Worker Heartbeat。

回滚：恢复上一不可变镜像。若新 Migration 不是向后兼容，必须先执行已评审的数据库回滚方案，
不能仅回滚 Deployment。

## 4. SSE Proxy/Ingress

浏览器订阅的真实接口是
`GET /v1/sessions/{sessionID}/events/stream`。从浏览器到 Go Control Plane 的每一跳——边缘负载均衡、
Ingress、反向代理，以及可选的 TypeScript 同源 `/v1` Proxy——都必须保持流式响应。只配置最外层
代理不够；任意中间层缓冲或在 Heartbeat 之前关闭空闲连接，都会表现为无故重连或延迟批量到达。

默认参数的关系如下：

| 边界                            | 默认/建议值                         | 要求                                                                    |
| ------------------------------- | ----------------------------------- | ----------------------------------------------------------------------- |
| Control Plane Heartbeat         | `SYNARA_SSE_HEARTBEAT_INTERVAL=15s` | 代理和负载均衡的读取/空闲超时必须大于它。                               |
| Control Plane 单次写入          | `SYNARA_SSE_WRITE_TIMEOUT=10s`      | 慢客户端超过该写入期限时由服务端主动断开，不得在代理层无限挂起。        |
| Proxy/LB upstream read/idle     | 建议至少 `75s`                      | 必须覆盖多个 Heartbeat 周期；所有外层 LB 的 idle timeout 也要同步设置。 |
| Proxy upstream/downstream write | 建议至少 `75s`                      | 不得短于正常网络抖动窗口；应用的 `10s` 写期限仍是最终慢客户端保护。     |

Control Plane 已返回 `Content-Type: text/event-stream; charset=utf-8`、
`Cache-Control: no-cache, no-store` 和 `X-Accel-Buffering: no`。代理必须原样转发这些 Header，关闭
response buffering、cache 和对 Event Stream 的压缩；不要改写为 JSON，也不要等待完整响应后再发送。

### 4.1 Nginx（Single-node Compose 同源 Proxy）

下面的 upstream 名称和端口来自 `deploy/saas/docker-compose.yml`。Nginx 与该 Compose 网络相连时，
把这些指令合并到已配置证书的现有 TLS `server`，将 `/v1/` 转发给 `synara:3773`，再由仓库内
TypeScript Proxy 流式转发到 Control Plane（示例中的证书指令由部署方现有配置提供）：

```nginx
upstream synara_web {
    server synara:3773;
    keepalive 32;
}

server {
    listen 443 ssl http2;
    server_name synara.example.com;

    location /v1/ {
        proxy_pass http://synara_web;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Last-Event-ID $http_last_event_id;

        proxy_connect_timeout 5s;
        proxy_read_timeout 75s;
        proxy_send_timeout 75s;
        send_timeout 75s;

        proxy_buffering off;
        proxy_cache off;
        gzip off;
    }
}
```

生产配置还必须把 Nginx 的实际网络范围加入 `SYNARA_TRUSTED_PROXY_CIDRS`；不要用 `0.0.0.0/0`
代替明确的代理 CIDR。若 Nginx 直接代理 Go Control Plane，应使用已经存在的 `control-plane:3780`
upstream，并保持同一个 `/v1/` 路径，不要增加自定义 SSE 路径。

### 4.2 Kubernetes ingress-nginx

Kustomize base 已提供 `synara-system/synara-control-plane` Service 的 `http` 端口，但 Ingress/TLS 由
部署方管理。以下完整资源使用现有 Service 和 `/v1` Prefix；应用前替换 Host 与 TLS Secret：

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: synara-control-plane
  namespace: synara-system
  annotations:
    nginx.ingress.kubernetes.io/proxy-http-version: "1.1"
    nginx.ingress.kubernetes.io/proxy-connect-timeout: "5"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "75"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "75"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - synara.example.com
      secretName: synara-tls
  rules:
    - host: synara.example.com
      http:
        paths:
          - path: /v1
            pathType: Prefix
            backend:
              service:
                name: synara-control-plane
                port:
                  name: http
```

`proxy-read-timeout`/`proxy-send-timeout` 的值是秒。若 ingress-nginx 前还有云负载均衡，必须在该
负载均衡的真实配置中把 idle timeout 设为大于 Heartbeat（同样建议至少 `75s`）；这项配置因云厂商
和 Controller 而异，不能用上述 Ingress Annotation 冒充。Ingress 默认会转发 `Last-Event-ID`；不要用
会清除未知请求 Header 的策略覆盖它。

### 4.3 Heartbeat 与重连验证

- 每个持久化 Event 使用数据库 Sequence 作为 SSE `id`，连接建立时服务端发送 `retry: 2000`。
- 浏览器原生 `EventSource` 自动重连时会把最后成功接收的 `id` 放入 `Last-Event-ID`。客户端手工重建
  订阅时必须保存最后已应用 Sequence，并可使用 `afterSequence`；若两者同时存在，
  `afterSequence` 优先。
- 服务端先从 PostgreSQL 回放大于 Cursor 的 Event，再进入实时等待；进程内通知只负责降低延迟。
  客户端仍需按 Sequence 去重和补 Gap，SSE 断开只能显示 Reconnecting，不能推断 Execution 已完成。
- 空闲 Session 至少每个 Heartbeat 周期收到 `: keep-alive`。若 `curl` 看到内容每隔很久成批出现，
  优先检查中间层 buffering；若约在固定秒数断开，逐跳检查 read/idle timeout。

使用已有登录 Cookie Jar 验证真实路径和断点续传，不要把 Cookie 内容粘贴到命令行历史或日志：

```bash
export SYNARA_ORIGIN='https://synara.example.com'
export SYNARA_SESSION_ID='replace-with-session-id'
export SYNARA_LAST_SEQUENCE='0'

curl --fail-with-body --no-buffer \
  --cookie /secure/path/synara-cookies.txt \
  --header 'Accept: text/event-stream' \
  --header "Last-Event-ID: ${SYNARA_LAST_SEQUENCE}" \
  "${SYNARA_ORIGIN}/v1/sessions/${SYNARA_SESSION_ID}/events/stream"
```

预期首先看到 `retry: 2000`，随后看到 backlog Event 或每约 `15s` 一次的 `: keep-alive`。重连验证应
记录最后 Event 的 `id`，断开后以该值发送 `Last-Event-ID`，并确认只返回更大的 Sequence。

## 5. Outbox 堆积、重试或 Dead Letter

关注指标：

- `synara_outbox_pending`
- `synara_outbox_oldest_pending_seconds`
- `synara_outbox_retrying`
- `synara_outbox_dead_letter`

处置：

1. 确认 Dispatcher 副本仍运行、数据库可写，`claimed_at/claim_expires_at` 是否持续推进。
2. 查看管理员 Outbox 列表中的安全错误摘要；API 不返回 Payload。
3. 上游短暂失败时等待指数退避，避免批量手工 Replay 放大故障。
4. Dead Letter 必须确认消费者幂等后，通过受审计 Replay API逐条或小批恢复。
5. 不直接设置 `published_at`，不删除未处理消息。

## 6. Worker 全部离线或 Lease 恢复激增

1. 检查 Worker Deployment/Daemon、网络、时钟和 Worker Protocol Version。
2. 比较 `lastHeartbeatAt`、Lease `expiresAt` 和当前 Generation。
3. Worker 恢复后先发送 Heartbeat，再 Claim；旧 Lease Token 和旧 Generation 永久无效。
4. `running/leased` Execution 在 Lease 过期后进入 `recovering`，不得直接标记失败或手工复用旧 Token。
5. 确认 `execution.recovering`、`worker.offline` Outbox 和 Session Event 已产生，替代 Worker Claim
   后 Generation 增加。
6. 若 Provider Resume Cursor 无法解密，停止重试并转到 KMS 处置，不要清空 Cursor。

可重复演练：

```bash
deploy/saas/failure-acceptance.sh
```

## 7. MinIO/S3 不可用

预期行为：Control Plane 进程保持存活，但 Artifact Store 预检失败时 `/ready=503`；Pending Artifact
Metadata 保留，不能伪造为 Ready。

1. 检查 Endpoint、DNS、TLS、Bucket、Region、Workload Identity 和对象存储配额。
2. 禁止把 Access Key、Secret、Session Token 或完整 Presigned URL 写入日志。
3. 对象存储恢复后等待 `/ready=200`。
4. 对仍在有效期内的 Pending Artifact 重试上传和 Complete；过期记录由分布式清理任务封存。
5. 不复制临时 Object Key 到最终 Key；只有 Control Plane 在重新读取并校验 Size、SHA-256、
   Content-Type 后才能提升。

真实 AWS S3 验证必须使用明确授权的可写测试 Bucket，并通过 `SYNARA_TEST_S3_*` 变量运行共享
Live Store 测试。没有授权时只记录为未执行，不能用 MinIO 结果冒充 AWS 结果。

## 8. Kubernetes Reconciler 或 RBAC 失败

```bash
kubectl auth can-i create pods \
  --as=system:serviceaccount:synara-system:synara-control-plane
kubectl auth can-i create secrets \
  --as=system:serviceaccount:synara-system:synara-control-plane
kubectl -n synara-system describe pod -l app.kubernetes.io/name=synara-control-plane
```

- `manageNamespace=true` 需要当前 ClusterRole 的 Namespace 权限。
- Operator 预建 Namespace 时应使用 namespaced Role，并关闭 Namespace 管理。
- Worker Pod 使用独立 ServiceAccount，默认关闭 Service Account Token 自动挂载。
- 不向 Worker Pod 注入 Control Plane ServiceAccount Token。

## 9. Provider Credential KMS 失败

1. 确认 KMS Provider、Key ID、Region、Workload Identity 和 Encrypt/Decrypt 权限。
2. 本地 KEK 轮换必须执行显式重加密流程；不能直接覆盖现有 Key。
3. AWS KMS 不设置 `SYNARA_CREDENTIAL_MASTER_KEY`；Local KMS 必须提供 32-byte Key。
4. KMS 不可用时拒绝新的 Credential 读取/写入，不将密文降级为明文或写入磁盘。
5. 恢复后使用专用测试 Credential 验证 Envelope 解密，不读取真实用户 Credential 做探测。

## 10. 双副本滚动升级

1. 先备份 PostgreSQL，并确认对象存储 Versioning/Lifecycle 策略。
2. 使用不可变镜像 Digest，确认 Migration 向后兼容。
3. 保持至少一个 Ready 副本；观察 `/ready`、HTTP 5xx、SSE Catch-up、Outbox Oldest Age。
4. 删除或重启一个 Pod 后验证：
   - Service 持续 Ready。
   - 已有 Worker Token 可在替代 Pod 继续 Heartbeat。
   - SSE 从 PostgreSQL Sequence 恢复。
5. 再更新第二个副本。失败时停止 rollout，并回滚镜像，不修改权威 Event/Lease 数据。

可重复 Kind 验收：

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-acceptance.sh
```

## 11. 安全日志检查

日志允许：`requestId`、`traceId`、资源 ID、Generation、稳定 `errorCode` 和安全错误摘要。

日志禁止：

- Prompt/Input 全文。
- Login、Worker、Lease、Provider Token。
- Provider Credential 明文或 KMS Data Key。
- SAML Assertion、OIDC Token。
- Presigned URL Query。

`deploy/saas/failure-acceptance.sh` 和 `deploy/kubernetes/acceptance.sh` 都用随机 Sentinel 执行动态
日志泄漏审计。发布前仍需对真实日志采集器、Sidecar 和 Ingress Access Log 做同样检查。

## 12. 相关资料

- `docs/release-checklists/stage-2-control-plane.md`
- `docs/release-checklists/stage-3-provider-runtime-remote-worker.md`
- `docs/runbooks/worker-release-rollout.md`
- `docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`
- `docs/reports/stage-2-production-acceptance-b507b0c3.md`（当前固定 PASS 证据）
- `docs/reports/stage-2-production-acceptance.md`
- `docs/reports/stage-2-production-acceptance-1a53c93a.md`
- `docs/reports/stage-2-production-acceptance-acf63b43.md`
- `docs/reports/stage-2-production-acceptance-0c42b0ec.md`
- `deploy/saas/README.md`
- `deploy/kubernetes/README.md`
- `deploy/kubernetes/monitoring/prometheus-rules.yaml`
