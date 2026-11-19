# 原生支付核验：实施说明

设计理由见 [ARCHITECTURE](ARCHITECTURE.md) 的 USDT 一节，核心文件改动见 [CORE-PATCHES](CORE-PATCHES.md)。

## 单一确认边界

- BEpusdt 使用原生 `/api/v1/order/create-transaction`、主动查单和 `/pay/checkout/{trade_id}`。新配置丢弃旧收银台的展示字段，不搬私有页面或隐藏付款 API。
- `payment/proof` 管理已有的配置版本和唯一付款凭证；复用历史 243 号迁移的原始字节，不改迁移账本。
- `recordPaymentEvidence` 是唯一的持久确认事务；`acceptPaymentEvidence` 在证据落库后才履约。已付但缺凭证的旧订单必须重新向原实例查询，不能由本地 PAID 状态直接生成证明。
- 进入 REVIEW 之前绑定权威交易号；管理员只输入交易引用，不输入任意充值金额。核验匹配原业务订单、商户、币种、金额、网络和报价后，才进入原履约。
- EasyPay 创建结果未知时保留原订单；BEpusdt 没有引用且已到期时进入待核验，不重新 POST，不假定已取消，也不永久占用普通待付款槽位。
- 保留原版的收据、付款收据、链上交易唯一性、兑换码 CAS 与余额事务，以及持久的 post-commit 补偿。取消订单的 CAS 失败时重新读取是否已付。
- 状态与管理入口支持“待核验”；用户端提示保留原订单、不要重复付款。批准核验需要显式确认。
- 收据写入与履约是两个明确的边界，历史恢复复用同一个验单事务；没有平行的“管理员加余额”通道。

## 测试

- `payment_verified_postgres_test.go`：收据、订单与审计事务。
- `payment/acceptance/native_postgres_test.go`：真实 PostgreSQL、真实 User/Redeem 仓储、完整 CreateOrder 和签名的 HTTP webhook，覆盖：
  1. 网关未付时余额不变；并发的多个“已付”通知最终只到账一次，只用掉一个兑换码，只有一张收据。
  2. 余额更新失败时，兑换码使用与余额一起回滚；丢弃全部服务和仓储对象重建后，原 worker 扫描能恢复到账。
  3. 余额事务提交后订单完成失败；重建进程对象后修复订单并补偿，不会再次加钱。
- 另有旧 EasyPay 别名与禁用原实例的回归测试。

所有网关均为模拟服务，测试网络与任何生产数据库隔离，不访问真实支付、模型或 SMTP。这些测试证明支付资金状态机，不代替真实链上小额支付验收。
