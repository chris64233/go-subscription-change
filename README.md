# go-subscription-change

订阅套餐的**预约变更与生效结算**服务（Go，仅依赖标准库）。支持在当前计费周期内立即变更
或在下一周期边界变更，受理时冻结原/新套餐、计费周期与价格版本并精确计算补退差额；
通过单仓储原子事务、乐观并发控制与唯一索引，保证并发场景下的正确性。

## 核心能力

1. **预约变更与冻结结算**
   - `IMMEDIATE`：当前周期内立即切换，按剩余日历日比例精确计算
     `差额 = roundHalfUp((新价 − 旧价) × 剩余天数 / 周期总天数)`，
     正数为应补、负数为应退；金额全程用整数“最小货币单位”（分），
     比例分摊用纯整数运算（`(2n+d)/2d` 半进位），不经过浮点，无二进制误差。
   - `NEXT_PERIOD`：在下一计费周期边界切换，受理时冻结新套餐价格，边界按整周期收取。
   - 申请受理时把**原套餐价格版本、新套餐价格版本、计费周期起止、差额金额**全部写入申请快照；
     之后无论套餐怎么调价，都不会改写已受理的申请（历史价格变化不可回溯）。
2. **同一订阅至多一笔待生效变更**：生效前可以用当前版本**替换**（旧申请置 `SUPERSEDED`）
   或**取消**。替换携带 `expectedChangeId` 做乐观锁，并发修改返回 `409 CONCURRENT_UPDATE`，
   不会静默互相覆盖。
3. **外部申请号（clientToken）幂等与冲突**：同号 + 同订阅 + 同目标套餐 + 同生效方式
   视为重试，原样返回原申请；同号但目标套餐或生效方式不同（或绑到别的订阅）返回冲突。
4. **到期推进原子化**：在一次事务中完成“切换套餐 → 写差额账单 → 生成唯一生效通知 →
   申请置 EFFECTIVE”。账单与通知都以 `changeId` 为唯一键，多个执行者并发推进只有一个成功，
   旧版本任务重试不可能二次收费。
5. **确定的终局结果**：
   - 订阅终止是终态，未生效申请随其 `TERMINATED`，终止后套餐不能再切换、推进被拒绝；
   - 已生效的变更不能被迟到的取消还原（取消只接受 `PENDING`）；
   - 取消 / 生效 / 终止并发到达时，以先提交事务者为准，结果恒为合法状态之一。
6. **变更历史持久化**：创建、受理、替换、取消、生效、终止均追加不可变事件流水。

## 状态机

```
                          ┌────────────── 到期推进(原子) ──────────────┐
PENDING ──申请受理──┐      │  切换套餐 + 差额账单 + 唯一通知            ▼
                   ├──────┴──────────────────────────────────────► EFFECTIVE（终态）
                   ├── 用户取消 ─────────────────────────────────► CANCELLED（终态）
                   ├── 被新申请替换 ───────────────────────────────► SUPERSEDED（终态）
                   └── 订阅终止 ───────────────────────────────────► TERMINATED（终态）
```

## 目录结构

| 文件 | 职责 |
| --- | --- |
| `plan.go` | 套餐与不可变价格版本，按生效时间选价 |
| `subscription.go` | 订阅、计费周期、按日历日分摊、月末边界钳制 |
| `change.go` | 变更申请、状态机、两种差额精确计算 |
| `bill.go` | 账单、通知、历史事件 |
| `store.go` | 线程安全内存仓储：快照事务 + 版本 CAS + 唯一索引 |
| `service.go` | 业务用例（所有写操作单事务原子提交） |
| `api.go` | `net/http` JSON 接口与错误码映射 |
| `cmd/subscription-server` | 可运行的 HTTP 服务入口 |

## 运行

```bash
go run ./cmd/subscription-server -addr :8080
```

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/plans` | 创建套餐（含首期价格） |
| GET | `/plans` | 套餐列表 |
| GET | `/plans/{planID}` | 套餐详情（含全部价格版本） |
| POST | `/plans/{planID}/prices` | 追加不可变新价格版本 |
| POST | `/subscriptions` | 创建订阅（当前价，按月计费） |
| GET | `/subscriptions/{subID}` | 订阅详情 |
| POST | `/subscriptions/{subID}/changes` | 提交变更申请 |
| PUT | `/subscriptions/{subID}/changes` | 替换待生效变更（可用 `expectedChangeId` 乐观锁） |
| DELETE | `/subscriptions/{subID}/changes` | 取消待生效变更 |
| POST | `/subscriptions/{subID}/advance` | 到期推进（原子生效 + 结算 + 通知） |
| POST | `/subscriptions/{subID}/terminate` | 终止订阅 |
| GET | `/subscriptions/{subID}/bills` | 账单查询 |
| GET | `/subscriptions/{subID}/history` | 变更历史 |
| GET | `/changes/{changeID}` | 变更申请详情 |

### 快速上手

```bash
# 1) 两个套餐（单位：分）
curl -s -XPOST localhost:8080/plans -d '{"id":"basic","name":"Basic","currency":"CNY","price":1000}'
curl -s -XPOST localhost:8080/plans -d '{"id":"pro","name":"Pro","currency":"CNY","price":4100}'

# 2) 开通订阅
curl -s -XPOST localhost:8080/subscriptions -d '{"id":"s1","planId":"basic"}'

# 3) 申请立即变更（带外部申请号，安全重试）
curl -s -XPOST localhost:8080/subscriptions/s1/changes \
  -d '{"clientToken":"req-1","targetPlanId":"pro","effectiveType":"IMMEDIATE"}'
# => settlementAmount 即冻结的应补/应退净额（按剩余日历日精确计算）

# 4) 到期推进：原子切换 + 账单 + 通知
curl -s -XPOST localhost:8080/subscriptions/s1/advance

# 5) 查账单与历史
curl -s localhost:8080/subscriptions/s1/bills
curl -s localhost:8080/subscriptions/s1/history
```

下一周期变更只需把 `effectiveType` 换成 `"NEXT_PERIOD"`（在周期边界前推进返回 `400`，
到边界才生效并按冻结价格收取下一整周期）。

### 错误码

| HTTP | error.code | 触发场景 |
| --- | --- | --- |
| 400 | `INVALID_ARGUMENT` | 参数非法 / 尚未到期就推进 |
| 404 | `NOT_FOUND` | 套餐、订阅、申请不存在 |
| 409 | `PENDING_CHANGE_EXISTS` | 已存在待生效变更 |
| 409 | `CONFLICT` | 外部申请号目标不同 / 唯一约束冲突 |
| 409 | `CONCURRENT_UPDATE` | 乐观锁失败（并发修改） |
| 409 | `NOT_PENDING` | 申请已不在 PENDING（重复推进、迟到取消等） |
| 409 | `SUBSCRIPTION_ENDED` | 订阅已终止 |
| 409 | `SAME_PLAN` | 目标套餐与当前套餐相同 |

## 并发正确性的实现要点

- **单仓储互斥锁 + 快照拷贝事务**：每个用例在一份深拷贝上执行，全部成功才整包替换，
  任一校验失败整体回滚；读事务永远拿到一致快照。
- **版本号 CAS**：订阅与变更申请都带单调 `version`，写入必须带期望版本，
  并发提交中版本不符者失败。
- **唯一索引**：`billByChange`、`notifByChange`、`changeByToken` 三张索引把
  “一笔变更一张账单/一条通知”“外部申请号唯一”下沉为存储约束，作为应用层判断之外的硬保证。
- **终态校验 + CAS 双保险**：推进/取消/终止先校验状态再 CAS 写入，
  因此多个执行者同时处理同一申请时恰好一个生效。

## 测试

```bash
go test ./... -race -count=1
```

测试覆盖：整数按比例分摊与半进位舍入（含大额与负值）、按日精确补退、
价格版本边界选价、冻结不被后续调价改写、申请号幂等/冲突（含终止后重试仍稳定返回原申请）、
替换重算 / 替换幂等重放 / 旧申请号归属保留、乐观锁（服务层与 HTTP 层）、
取消与迟到取消、32 并发推进只生效一次、按申请号推进、
推进×终止 / 取消×推进 各 40 轮竞态不变量、月末钳制（1/31 → 2/28 → 3/28）、
批量到期扫描、终止后全部写操作守卫，以及完整 HTTP 冒烟与错误码断言。
