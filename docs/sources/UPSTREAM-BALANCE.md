# 上游余额监测：保留能力，简化接入

管理员可以在账号列表看到每个上游账号的钱包余额。实现位于 Portal（来源、凭据、查询与缓存）和原生账号页的小挂接（余额列、配置抽屉）。本文说明凭据选择和单位处理的依据。

## 最小方案

- 保留上游列表、账号剩余/已用、单位、刷新时间、手动刷新、低余额告警及异常提示。
- **默认使用上游账号的个人访问令牌**查询该账号自己的钱包。普通模型调用 Key 不作为默认余额来源。
- 上游同为 Sub2API 时，自动复用该账号已有的地址、Key、代理和传输配置读取钱包余额，也可单独配置余额查询地址；只显示明确的钱包余额，订阅额度和 Key 限额不当作余额。
- 每个上游账号配置站点地址＋个人访问令牌；支持scope的站点只授予读取自身资料权限（已核实New API版本为`profile:read`）。旧版如要求User ID则按其合同补充，不索取管理员Key。
- 不保存登录密码，不复制Cookie，不做浏览器自动登录，不搬旧多层猜接口/宽泛JSON扫描逻辑。
- 首版仅为实际使用的上游登记已核实适配，不为了“通用”预建大量供应商框架。不是New API的站点仍需各自合同，不能硬套同一种返回格式。
- 默认30分钟轮询、手动刷新限流；失败有退避，保留上次成功结果并标为过期，查询失败不写成余额0。
- 外部配置/凭据与Sub主库隔离；只读查询和告警，不修改余额、不自动充值、不接管调度。
- 同一上游账号只统计一份钱包，不因配置多个模型Key重复累计。基础地址之外还要核对个人令牌返回的账号ID，不能按域名把不同用户的钱包合并。

**少改核心，也少拆服务。** 余额监测与首页编辑放在同一个轻量扩展服务（Portal）中，使用独立模块、存储空间和管理员鉴权；公开首页只能读取已发布内容。沿用已有福利系统、工作台和BEpusdt，不给每个功能再建一套服务。管理员内嵌入口不代替服务端权限检查。

## New API 选定接口与对照资料

正式接入选定 **个人访问令牌 → `GET /api/user/self` → 账号quota**。下面保留普通Key和兼容billing的对照，仅用于解释区别，不作为默认探测或静默兜底。

已核对官方 **v1.0.0-rc.43** 源码；第三方站可能运行旧版或私有修改，必须实际验证。

| 凭据与接口 | 数字含义 | 处理 |
|---|---|---|
| 普通模型调用Key + `GET /api/usage/token/` | `total_available`是该Key的RemainQuota；另有used和unlimited标志 | 标签必须是“Key剩余额度”，不是账号钱包；无限额度显示无限而非大额存款 |
| 账号个人访问令牌 + `GET /api/user/self` | `data.quota`是当前账号剩余；`used_quota`是账号已用 | 查询“账号余额”；新版权限为`profile:read`，不需要管理员身份。旧版User ID请求头按站点合同兼容 |
| 普通调用Key + `/dashboard/billing/subscription`与`/dashboard/billing/usage`（也有/v1形式） | 取决于`DisplayTokenStatEnabled`：令牌统计或账号统计 | 先确认统计对象和单位，再决定可否作为钱包来源；不能仅因HTTP200就切换含义 |

官方默认`DisplayTokenStatEnabled=true`。因此“普通sk Key一定能查账号余额”不成立；它能查Key额度，部分站的兼容billing配置也能展示账号数值。

另外，调用Key额度未必等于账户可消费金额：Key额度100、账号余额10时，不应显示可用钱包100。余额监测不会列出、复制或新增用户全部Key；本方案直接配置一枚个人只读令牌，不再绕调用Key猜测钱包。

## 单位必须核实

- 新版 `/api/status`提供`quota_per_unit`、`quota_display_type`、`usd_exchange_rate`以及自定义币种信息，可作为站点元数据依据。
- 原始quota先按上游确定的比例换算；保留来源与单位。未知时显示未确认，而非默认标USD。
- 兼容billing字段虽然叫`*_usd`，当前实现会按站点展示类型调整，名称不构成币种证据。
- 上游显示的 USD/CNY/自定义积分与实际采购成本的汇率是两层概念；监测余额不自动换算成本或利润，也不套用其他站点的比例。

## 验收

1. 正确/错误个人令牌、权限不足、过期/撤销、钱包为零或不足；只有允许的只读GET请求。误填普通模型Key时明确提示凭据类型，不静默切换为Key额度。
2. 同账号多条监测配置、同域不同账号、接口版本差异、人民币/美元/自定义展示、未知换算比例。
3. 401/403/404/429/超时、HTML错误页、success=false、数据不完整；不把它们报成余额0或正常。
4. 低额通知去重、恢复通知、刷新限流、数据过期显示、凭据脱敏；没有管理员Key或上游Key进入公开页面。
5. 站点地址与调用地址可能不同；有的直连域名只开放/v1，余额接口需使用配置的管理站域名，不能为读余额而放开全部后台路由。

## 官方依据

- [令牌查询](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/controller/token.go)
- [用户自身资料与quota](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/controller/user.go)
- [个人令牌scope](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/middleware/access_token_routes.go)
- [兼容billing的统计对象与展示单位](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/controller/billing.go)
- [billing路由使用模型Key鉴权](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/router/dashboard.go)
- [站点额度元数据](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.43/controller/misc.go)
