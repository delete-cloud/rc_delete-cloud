# rc-delete-cloud

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

本仓库用 Go 标准库实现了一个最小可运行版本：

- `POST /notifications`：创建通知任务，返回 `202 Accepted`。
- `GET /notifications/{id}`：查询任务状态。
- `POST /notifications/{id}/retry`：把 `FAILED` 任务重新放回重试队列。
- 文件持久化队列：默认保存到 `data/notifications.json`。
- 后台 worker：扫描 `PENDING` / `RETRYING` / 超过租约的 `SENDING` 任务。
- HTTP 投递：2xx 视为成功；网络错误、超时、5xx、408、409、429 视为可重试；其他 4xx 视为永久失败。
- 指数退避：10s、20s、40s ... 上限 5min。
- 默认最大尝试次数：5 次。
- `idempotencyKey`：相同 key 重复提交时返回已有任务，减少业务系统重试造成的重复任务。

## 运行方式

```bash
go test ./...
go run ./cmd/server
```

默认监听 `:8080`。也可以指定地址和存储文件：

```bash
go run ./cmd/server -addr :8081 -data /tmp/notifications.json
```

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

手动重试失败任务：

```bash
curl -X POST http://localhost:8080/notifications/{notification_id}/retry
```

## 系统边界

本系统解决：

1. 接收业务系统提交的外部 HTTP(S) 通知任务。
2. 在投递前持久化任务，避免进程退出导致任务直接丢失。
3. 异步调用外部供应商 API，避免业务系统被供应商延迟拖慢。
4. 对网络错误、超时、5xx、429 等临时失败做有限重试。
5. 记录任务状态、重试次数、下次重试时间和最后错误。
6. 提供状态查询和失败后的手动重试入口。

本系统第一版明确不解决：

1. 不保证 exactly-once。外部 HTTP 请求成功但本地状态更新前进程崩溃时，恢复后可能再次投递。
2. 不解析供应商业务响应。第一版只关注 HTTP 投递结果，不理解 CRM、广告、库存等业务语义。
3. 不做供应商配置后台或模板 DSL。当前由业务系统直接提交 URL、Header、Body，先验证可靠投递链路。
4. 不替外部系统处理业务幂等。系统会透传 `Idempotency-Key`，但外部系统仍需要能接受重复请求。
5. 不做多租户、复杂权限、审计和供应商级限流。这些是生产化能力，不是 MVP 的核心。

## 可靠性与失败处理

投递语义选择 **at-least-once**。

原因是外部 HTTP API 和本地存储之间没有共同事务。只要存在“外部系统已经处理成功，但本服务还没来得及标记成功就崩溃”的窗口，严格 exactly-once 就无法仅靠本服务实现。第一版选择 at-least-once，并通过 `idempotencyKey` 和 `Idempotency-Key` Header 降低重复投递影响。

失败策略：

- 2xx：标记 `SUCCEEDED`。
- 网络错误 / 超时：标记 `RETRYING`，按指数退避重试。
- 5xx：认为供应商临时不可用，重试。
- 408 / 409 / 429：按临时失败处理；如果供应商返回 `Retry-After` 且晚于本地退避时间，则采用更晚的时间。
- 其他 4xx：认为请求参数或认证配置错误，直接标记 `FAILED`。
- 超过最大尝试次数：标记 `FAILED`，等待人工排查或手动补偿。

长期不可用时，本系统不会无限重试。无限重试看似更可靠，但会让故障供应商持续消耗资源，并可能造成队列积压。第一版用有限重试和失败状态把问题显性化。

## 关键工程决策

1. **先持久化，再异步投递**  
   API 收到请求后先保存任务再返回，外部供应商慢或不可用时不会拖垮业务系统。

2. **至少一次，而不是 exactly-once**  
   exactly-once 在外部 HTTP 场景下成本过高，并且需要供应商幂等或事务配合。at-least-once 更现实。

3. **第一版使用文件仓储表达队列语义**  
   作业环境下不引入数据库驱动或消息队列，仓库可以零依赖运行。生产版本会把 `Store` 接口替换成 PostgreSQL/MySQL 或 MQ-backed 实现。

4. **有限重试**  
   供应商长期不可用时进入 `FAILED`，避免无限重试造成资源耗尽。

5. **用状态机而不是内存 channel 保存任务**  
   状态和时间字段能表达可恢复的任务生命周期，也便于后续迁移到 SQL 表。

## 取舍与演进

AI 曾建议过 Kafka、RabbitMQ、Redis Stream、分布式锁、供应商配置 DSL、管理后台、Prometheus/Grafana、熔断器、多租户权限等能力。第一版没有采用，原因是它们会显著增加部署和理解成本，而题目更关注系统边界、可靠性语义和工程取舍。

如果未来流量或复杂度增长，可以按下面方向演进：

1. 存储从文件迁移到 PostgreSQL/MySQL，使用行级锁或状态租约支持多 worker。
2. 高吞吐场景引入 Kafka/RabbitMQ/Redis Stream，API 服务只负责持久化和入队，worker 横向消费。
3. 增加供应商维度限流、熔断和隔离，避免单个供应商故障影响全局投递。
4. 增加死信队列和补偿后台，支持批量查看、重新投递和人工修复。
5. 增加指标和日志：成功率、失败率、重试次数、供应商延迟、积压量。
6. 增加供应商配置中心，把认证、URL、Header、Body 模板从业务请求中抽离。

## 代码结构

```text
cmd/server/main.go              # 服务入口，启动 API 和 worker
internal/notification/model.go  # 通知任务模型和输入校验
internal/notification/store.go  # 文件持久化 Store
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
- `idempotencyKey` 重复提交。
- 创建和查询 API。
- worker 成功投递。
- 临时失败进入 `RETRYING`。
- 永久失败进入 `FAILED`。
- 超过最大尝试次数后失败。
