# go-subscription-change

订阅套餐的**预约变更**与**生效结算**服务。支持当前计费周期内立即变更（按精确金额补/退差额）与下一周期边界变更，覆盖申请冻结、单待生效约束、乐观并发、外部申请号幂等/冲突、原子到期推进、取消/生效/终止的确定性顺序，并以事件溯源持久化全部变更历史。

## 快速开始

```bash
# 内存模式（重启丢失，适合本地试验）
go run ./cmd/server
# ADDR=:8080

# 持久化模式（JSONL WAL，重启自动重放）
WAL_PATH=./data/events.jsonl go run ./cmd/server
```

健康检查与首个套餐：

```bash
curl -s localhost:8080/api/v1/plans -H 'Content-Type: application/json' \
  -d '{"id":"basic","name":"基础版","cycle":"monthly","price_cents":3000}'
```

## 目录结构

```
subscription/
  domain.go   领域模型、常量、错误、精确金额分摊（math/big，半额向上取整到分）、计费周期
  events.go   事件定义与状态投影（reducer）
  store.go    事件溯源存储：内存 + JSONL WAL（追加/fsync/重放），单次原子事务
  service.go  业务用例：套餐配置/调价、订阅创建/终止、申请/替换/取消、到期推进、查询
  http.go     JSON HTTP API
cmd/server/   服务入口
*_test.go     23 个自动化测试（含 -race 并发场景与 WAL 重放）
```

## 核心设计

### 1. 申请即冻结，历史价格不可改写

受理变更时把以下信息快照进 `ChangeRequest`，之后与套餐当前配置完全解耦：

- 原套餐 / 新套餐的 ID、名称、周期、**价格版本号、价格**；
- 当前计费周期 `[period_start, period_end)`；
- 申请时间、计划生效时间、差额金额与方向。

调价通过追加不可变的 `PlanVersion` 实现；到期推进只读取冻结快照，因此受理后的任何价格变化都不会改写已受理申请，也不会改变即将开出的账单金额。

### 2. 精确金额（无浮点误差）

立即变更差额按受理时刻冻结的周期，用 `math/big.Rat` 计算：

```
diff = (newPrice - oldPrice) * remainDays / totalDays
diff > 0 应补（charge），diff < 0 应退（refund）
```

金额单位为整数分，半分按**远离零方向半额向上取整**（如 `1548.5 → 1549`、`-1548.5 → -1549`），结果确定且无浮点误差。天数按 24h 整天计；月/年周期推进时把日钳制到目标月末（1/31 + 1 月 = 2/28，而非 Go 默认的 3/3）。

`next_cycle` 不在受理时重算当前周期费用，而在周期边界按冻结的新套餐价格开出整周期 `renewal` 账单。

### 3. 单一待生效 + 乐观并发

- 同一订阅同一时刻只允许**一笔**待生效变更，重复申请返回 `409 pending_change_exists`。
- 每个待生效变更带单调递增的 `version`。替换/取消可携带 `expected_version`（或指定具体变更 ID）做 CAS：版本不匹配返回 `409 version_mismatch`，并发修改不会静默覆盖。
- 替换把旧版本置为 `superseded`。**旧版本任务永远不能再次收费**：推进旧版本返回 `409`。
- 外部申请号：相同号 + 相同目标套餐/生效方式为幂等重放（返回原申请，不重复出账）；相同号但目标或方式不同返回 `409 conflict`。

### 4. 到期推进是一次原子操作

推进（`immediate` 在受理事务内即时结算；`next_cycle` 由 `advance` 触发）在**同一个存储事务**中追加：

1. `bill_written`（差额 charge/refund，或新周期 renewal；零差额不出账）
2. `notification_sent`（以 `change-effective:<changeID>` 去重，唯一通知）
3. `change_effective`（切换套餐/周期、回写账单与通知 ID）

事件组先整体追加 WAL 并 `fsync`，再投影到内存——要么全发生，要么全不发生。

- 多执行者并发推进同一申请：只生效一次，重复调用返回原账单/通知（`advanced=false`），**绝不二次收费**。
- 未到 `scheduled_at` 推进：`409 change_not_due`。

### 5. 取消 / 生效 / 终止的确定顺序

- 终止订阅在同一事务内取消其待生效变更；**终止后任何推进都不能再切换套餐**（reducer 对 terminated 聚合再做一次防御）。
- 已经生效的变更不能被迟到取消还原（`409`），切换不可逆。
- 取消与推进并发到达时，二者互斥，恰好一方胜出，且账单数与套餐状态与胜出方严格一致（测试以 20 轮重复验证）。

### 6. 事件溯源与持久化

所有状态变更都是不可变事件，顺序追加到 JSONL WAL（每行一个事件）。启动时重放重建内存状态；变更历史（含已取消/已替换/已生效的全部版本）天然持久保留。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/plans` | 配置套餐（含首个价格版本） |
| GET | `/api/v1/plans` · `/api/v1/plans/{id}` | 套餐列表/详情（含全部价格版本） |
| POST | `/api/v1/plans/{id}/prices` | 追加价格版本（调价） |
| POST | `/api/v1/subscriptions` | 创建订阅（返回订阅 + 首期账单） |
| GET | `/api/v1/subscriptions/{id}` | 订阅详情（当前套餐/周期/待生效变更） |
| POST | `/api/v1/subscriptions/{id}/terminate` | 终止（原子取消待生效变更） |
| POST | `/api/v1/changes` | 提交变更申请 |
| PUT | `/api/v1/changes/{id}` | 替换待生效变更（乐观版本） |
| DELETE | `/api/v1/changes/{id}` | 取消待生效变更 |
| POST | `/api/v1/changes/{id}/advance` | 到期推进（原子切换+账单+通知，幂等） |
| GET | `/api/v1/changes/{id}` | 变更申请详情 |
| GET | `/api/v1/subscriptions/{id}/changes` | 变更历史 |
| GET | `/api/v1/subscriptions/{id}/bills` · `/api/v1/bills/{id}` | 账单列表/详情 |

### 典型流程

```bash
# 1) 两个套餐
curl -s -XPOST localhost:8080/api/v1/plans -d '{"id":"basic","name":"基础版","cycle":"monthly","price_cents":3000}'
curl -s -XPOST localhost:8080/api/v1/plans -d '{"id":"pro","name":"专业版","cycle":"monthly","price_cents":6000}'

# 2) 创建订阅（返回 subscription 与 initial 账单）
curl -s -XPOST localhost:8080/api/v1/subscriptions -d '{"plan_id":"basic"}'

# 3) 预约下一周期升级（冻结 basic/pro 价格版本与当前周期）
curl -s -XPOST localhost:8080/api/v1/changes -d '{
  "subscription_id":"<subID>","external_id":"REQ-42",
  "to_plan_id":"pro","effective_mode":"next_cycle"}'

# 4) 周期边界推进：切换套餐 + 按冻结价 6000 出 renewal 账单 + 唯一通知
curl -s -XPOST localhost:8080/api/v1/changes/<changeID>/advance
# 重复调用返回相同账单/通知且 "advanced":false

# 5) 当前周期内立即升级（按剩余天数精确补差价，受理事务内即结算）
curl -s -XPOST localhost:8080/api/v1/changes -d '{
  "subscription_id":"<subID>","to_plan_id":"pro","effective_mode":"immediate"}'
```

### 错误响应

统一为 `{"error":..., "code":...}`，常见 code：`invalid_argument`、`not_found`、`already_exists`、`pending_change_exists`、`version_mismatch`、`change_not_due`、`subscription_terminated`、`conflict`（除前两者外均为 HTTP 409/400）。

替换/取消请求体可带 `"expected_version": 1` 实现乐观锁；也可直接以 URL 中的变更 ID 作为 CAS 目标（对旧版本操作返回 `version_mismatch`）。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖（23 个）：

- 精确分摊与半额取整、月末钳制、价格版本冻结点；
- 立即变更补/退差额与原子结算、零差额不出账；
- 受理后调价不改写申请，边界按冻结价续费；
- 单待生效约束、替换版本 CAS、旧版本不可收费；
- 外部申请号幂等重放与目标/方式冲突；
- 并发替换仅一方胜出、并发推进只生效一次且账单/通知唯一；
- 终止后不可切换、迟到取消拒绝、取消/推进并发的确定结果（20 轮）；
- WAL 落盘事件纯度与重放恢复、变更历史持久化；
- HTTP API 端到端（含 409/404 状态码与幂等行为）。
