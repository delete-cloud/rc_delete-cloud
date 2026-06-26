# rc_delete-cloud

一个内部 HTTP 通知投递服务 MVP。业务系统把外部供应商 API 的调用请求提交给本服务；本服务先持久化任务并立即返回，再由后台 worker 异步投递、失败重试、记录最终状态。

## 问题理解

企业内部多个业务系统会在关键事件发生后通知外部系统，例如广告注册回传、CRM 状态变更、库存系统变更。不同供应商的 URL、Header、Body 都不同，业务系统不需要同步等待供应商的业务响应，只需要把通知请求尽可能可靠地送达。

因此第一版系统的核心不是“封装所有供应商协议”，而是提供一条可靠的异步投递链路：

```text
Business System
  -> POST /notifications
  -> Notification Store
  -> Delivery Worker
  -> External HTTP(S) Vendor API
```

## 当前实现

本仓库用 Go 标准库 + SQLite 实现了一个最小可运行版本：

- `POST /notifications`：创建通知任务，返回 `202 Accepted`。
- `GET /notifications?status=FAILED`：查看指定状态的任务，可用于 MVP 阶段的逻辑 DLQ 排查。
- `GET /notifications/{id}`：查询任务状态。
- `GET /notifications/{id}/attempts`：查询每一次投递尝试记录。
- `POST /notifications/{id}/retry`：把 `FAILED` 任务重新放回重试队列。手动 retry 不会重置 `attempt_count`，也不会重新给一轮默认 5 次自动重试机会；它保留 attempt history 的单调性，并触发一次新的人工补偿投递。
- SQLite 持久化任务表：默认保存到 `data/notifications.db`。
- 后台 worker：通过 SQLite 事务领取 `PENDING` / `RETRYING` / 超过租约的 `SENDING` 任务。
- HTTP 投递：第一版只按 HTTP status 判断投递结果，不解析响应 body。2xx 视为成功；网络错误、超时、5xx、408、409、429 视为可重试；其他 4xx 视为永久失败。
- 指数退避：10s、20s、40s ... 上限 5min。
- 默认最大尝试次数：5 次。
- `idempotencyKey`：业务事件唯一键。相同 key 且请求内容一致时返回已有任务；相同 key 但请求内容不同时返回冲突。
- `DeliveryAttempt` 历史：记录每次投递的尝试序号、状态、HTTP 状态码、错误原因、开始/结束时间和下次重试时间。
- HTTP envelope 安全边界：限制 method、请求大小、Header 白名单，投递前拦截私网/metadata IP，并禁止自动跟随 redirect。

## 运行方式

```bash
go test ./...
go run ./cmd/server
```

默认监听 `:8080`，SQLite 数据库文件为 `data/notifications.db`。也可以指定地址和数据库文件：

```bash
go run ./cmd/server -addr :8081 -db /tmp/notifications.db
```

可以用 `-allowed-hosts` 配置供应商域名白名单：

```bash
go run ./cmd/server -allowed-hosts api.vendor.example,crm.vendor.example
```

如果不配置 host allowlist，系统仍会拦截私网、loopback、link-local、metadata IP 和危险 Header，但不会限制公网域名集合。

## 示例

创建通知：

```bash
curl -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -d '{
    "targetUrl": "https://httpbin.org/post",
    "method": "POST",
    "headers": {"Content-Type": "application/json"},
    "body": {"event": "user_registered", "userId": "u_123"},
    "idempotencyKey": "demo-001"
  }'
```

查询状态：

```bash
curl http://localhost:8080/notifications/{notification_id}
```

查询投递历史：

```bash
curl http://localhost:8080/notifications/{notification_id}/attempts
```

查看失败任务：

```bash
curl 'http://localhost:8080/notifications?status=FAILED'
```

手动重试失败任务：

```bash
curl -X POST http://localhost:8080/notifications/{notification_id}/retry
```

## 系统边界

本系统解决：

1. 接收业务系统提交的外部 HTTP(S) 通知任务。
2. 在投递前持久化任务，避免进程退出导致任务直接丢失。
3. 用 SQLite 任务表表达队列语义，并通过事务领取待投递任务。
4. 异步调用外部供应商 API，避免业务系统被供应商延迟拖慢。
5. 对网络错误、超时、5xx、429 等临时失败做有限重试。
6. 记录任务状态、重试次数、下次重试时间和最后错误。
7. 记录每一次投递尝试，便于排查供应商错误、网络超时和重试路径。
8. 提供状态查询、失败任务列表和失败后的手动重试入口。

本系统第一版明确不解决：

1. 不保证 exactly-once。外部 HTTP 请求成功但本地状态更新前进程崩溃时，恢复后可能再次投递。
2. 不解析供应商业务响应。第一版只按 HTTP status 判断是否投递成功；响应 body 属于供应商业务语义，例如 `{ "success": false }`，MVP 不处理。
3. 不做供应商配置后台或模板 DSL。当前用通用 HTTP envelope 支持不同供应商的 URL、Header、Body，先验证可靠投递链路。
4. 不替外部系统处理业务幂等。系统会透传 `Idempotency-Key`，但外部系统仍需要能接受重复请求。
5. 不托管供应商认证信息。第一版允许业务系统提交必要 Header；生产版本应将供应商 token 收敛到通知服务配置中，避免密钥散落。
6. 不做多租户、复杂权限、审计和供应商级限流。这些是生产化能力，不是 MVP 的核心。

## 可靠性与失败处理

投递语义选择 **at-least-once**。

原因是外部 HTTP API 和本地存储之间没有共同事务。只要存在“外部系统已经处理成功，但本服务还没来得及标记成功就崩溃”的窗口，严格 exactly-once 就无法仅靠本服务实现。第一版选择 at-least-once，并通过 `idempotencyKey` 和 `Idempotency-Key` Header 降低重复投递影响。`idempotencyKey` 表示业务事件唯一键，用于避免重复提交并帮助下游幂等；如果相同 key 对应不同请求，系统返回冲突，避免误把两个不同业务事件合并。

失败策略：

- 2xx：标记 `SUCCEEDED`，不解析响应 body。
- 网络错误 / 超时：标记 `RETRYING`，按指数退避重试。
- 5xx：认为供应商临时不可用，重试。
- 408 / 409 / 429：按临时失败处理；如果供应商返回 `Retry-After` 且晚于本地退避时间，则采用更晚的时间。
- 其他 4xx：认为请求参数或认证配置错误，直接标记 `FAILED`。
- 超过最大尝试次数：标记 `FAILED`，等待人工排查或手动补偿。

长期不可用时，本系统不会无限重试。无限重试看似更可靠，但会让故障供应商持续消耗资源，并可能造成队列积压。第一版用有限重试和失败状态把问题显性化。

第一版不引入外部 MQ。系统使用 SQLite 持久化任务表表达队列语义，超过最大重试次数后进入 `FAILED` 状态，作为 MVP 阶段的逻辑 DLQ。这样可以保留失败任务、错误原因和投递历史，支持人工排查与手动重试。未来当吞吐、隔离和横向扩展需求增长时，再演进为 MQ + DLQ，例如 Kafka、RabbitMQ 或 SQS，由主队列承接正常投递，死信队列承接长期失败任务。

## 关键工程决策

1. **先持久化，再异步投递**  
   API 收到请求后先保存任务再返回，外部供应商慢或不可用时不会拖垮业务系统。

2. **至少一次，而不是 exactly-once**  
   exactly-once 在外部 HTTP 场景下成本过高，并且需要供应商幂等或事务配合。at-least-once 更现实。

3. **第一版使用 SQLite 表达队列语义**  
   SQLite 是本地嵌入式数据库，不需要额外部署外部中间件；相比 JSON 文件，它能用唯一索引、普通索引和事务更自然地表达任务表、投递历史和任务领取语义。

4. **有限重试**  
   供应商长期不可用时进入 `FAILED`，避免无限重试造成资源耗尽。

5. **用状态机和数据库事务，而不是内存 channel 保存任务**  
   状态和时间字段能表达可恢复的任务生命周期；Worker 通过 `ClaimDue` 在事务中领取任务，减少重复领取同一任务的风险。

6. **保留投递尝试历史**  
   `Notification` 记录当前状态，`DeliveryAttempt` 记录每次尝试的历史事实。这样排查时不只依赖最后一次错误，也能看到多次重试是否都是同类失败。

7. **HTTP envelope 不是任意 HTTP 代理**  
   第一版采用 HTTP envelope 以降低供应商接入复杂度，但仍保留安全边界：method 限制、请求大小限制、Header 白名单、私网/metadata IP 拦截、redirect 不自动跟随，以及可配置的目标域名白名单。
   它的优点是简单直接，能快速支持不同供应商的 URL、Header 和 Body；缺点是业务系统仍需了解供应商协议，也会带来 URL/Header 安全风险。
   当前实现会在投递前解析目标域名并拦截私网地址；生产版本还应配合网络出口策略或自定义 Dialer，进一步收敛 DNS rebinding 等边界风险。

## 取舍与演进

AI 曾建议过 Kafka、RabbitMQ、Redis Stream、分布式锁、供应商配置 DSL、管理后台、Prometheus/Grafana、熔断器、多租户权限等能力。第一版没有采用，原因是它们会显著增加部署和理解成本，而题目更关注系统边界、可靠性语义和工程取舍。

如果未来流量或复杂度增长，可以按下面四个方向演进：

1. **可靠性扩展：数据库与 MQ 的边界**  
   当单机 SQLite 无法支撑更高并发、百万级积压或多实例 worker 时，可以将存储迁移到 PostgreSQL/MySQL。数据库继续作为任务状态、幂等键、投递历史和人工补偿记录的 source of truth。高吞吐场景下再引入 Kafka、RabbitMQ 或 Redis Stream 作为异步投递通道，用于削峰和 worker 横向消费。为避免“DB 写成功但 MQ 发布失败”，生产版本应采用 Outbox Pattern：在同一事务中写入通知任务和 outbox 事件，再由 publisher 可靠发布到 MQ。

2. **吞吐扩展：供应商维度隔离**  
   随着供应商数量增加，需要引入供应商维度的限流、熔断和隔离。单个供应商长期不可用时，不应该占满全局 worker、重试资源和数据库吞吐。可以按供应商维护独立并发额度、重试策略和熔断状态，必要时拆分队列或 worker pool，保证一个供应商故障不会拖垮整体通知链路。

3. **故障补偿：DLQ 与人工处理后台**  
   当前 MVP 使用 `FAILED` 状态表达逻辑 DLQ。生产版本可以增加死信队列和补偿后台，支持按供应商、错误类型、时间范围查看失败任务；在修复 token、签名、Header、URL 或供应商权限配置后，支持批量重新投递。补偿操作需要保留 attempt history 和操作记录，避免问题被“重试成功”掩盖。

4. **供应商治理：从 HTTP envelope 到事件模型**  
   当前 HTTP envelope 简单直接，适合作为第一版 MVP：它能快速验证可靠投递链路，也能接入不同供应商的 URL、Header 和 Body。但它没有做到和业务系统完全解耦，业务系统仍需了解供应商协议和认证细节，也会带来 URL/Header 安全风险。第一版没有直接实现 `vendorId + eventType + payload`，是因为这需要配套维护供应商配置中心，包括目标地址、认证信息、Header 模板、Body 模板、响应解析、限流策略和安全 allowlist；这些能力会显著增加实现和运维复杂度。生产版本应逐步演进到 `vendorId + eventType + payload`：业务系统只提交业务事件，通知服务统一管理供应商配置、认证、模板、限流和响应解析，同时收敛目标域名、私网地址拦截、Header 模板和认证托管等安全策略。

同时需要补充指标和日志，包括成功率、失败率、重试次数、供应商延迟、积压量、最老待投递任务年龄和供应商维度失败率，用于发现供应商故障和队列堆积。

当前 SQLite 存储使用两张核心表：

```text
notifications
  id, target_url, method, headers, body, status,
  attempt_count, max_attempts, next_retry_at,
  last_error, idempotency_key, created_at, updated_at

delivery_attempts
  id, notification_id, attempt_no, status, status_code,
  retryable, error, next_retry_at, started_at, finished_at
```

## 代码结构

```text
cmd/server/main.go              # 服务入口，启动 API 和 worker
internal/notification/model.go  # 通知任务模型和输入校验
internal/notification/store.go  # Store 接口与通用校验
internal/notification/sqlite_store.go
internal/notification/handler.go
internal/notification/delivery.go
internal/notification/worker.go
internal/notification/retry.go
```

## 测试

```bash
go test ./...
```

当前测试覆盖：

- 重试退避策略。
- `idempotencyKey` 重复提交和同 key 不同请求冲突。
- 创建和查询 API。
- HTTP envelope 安全校验，包括 host allowlist、Header allowlist、私网/metadata IP 拦截和 redirect 禁止。
- 失败任务列表和投递历史查询。
- worker 成功投递。
- 临时失败进入 `RETRYING`。
- 永久失败进入 `FAILED`。
- 超过最大尝试次数后失败。
- SQLite 持久化投递历史。
- `ClaimDue` 领取任务时标记 `SENDING` 并递增尝试次数。
