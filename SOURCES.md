# Sources 与原版的差异总表

本仓库基于 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 二次开发，许可证沿用原版的 LGPL-3.0。本文逐项说明相对原版改了什么、为什么改、代码在哪里，同时作为 LGPL 要求的修改说明。

后续升级、改页面或删代码前，先核对本表和逐文件清单，避免丢失已有能力。**“差异已登记”不等于“功能验收通过”；删除一项能力必须明确决定并说明迁移影响。**

## 对照基线与完整性

- 原版基线：[.upstream-sub2api](.upstream-sub2api) 记录固定的原版版本、提交和镜像摘要（当前为 v0.2.24）。本仓库版本见 `backend/cmd/server/VERSION`，变更记录见 [CHANGELOG.md](CHANGELOG.md)。
- 核心逐文件登记：[CORE-PATCHES.json](docs/sources/CORE-PATCHES.json) 列出 `backend/`、`frontend/`、`Dockerfile` 中每个相对原版有变化的文件、修改后的 sha256 和功能标签；说明见 [CORE-PATCHES.md](docs/sources/CORE-PATCHES.md)。不允许目录级豁免。
- 全仓路径对照方法见 [UPSTREAM-DIFF.md](docs/sources/UPSTREAM-DIFF.md)。
- 二开代码按位置分三类：原版文件上的小挂接（受上面的清单约束）、`backend/` 和 `frontend/` 中带 `sources` 前缀或位于 `features/sources/` 的新增文件、`extensions/` 下的独立模块。

## 二开能力

“入口”列给出查找起点，完整路径和标签以机器清单为准。原版已有的用户、Key、认证、调度和计费能力继续保留，不因不在下表而删除。

| 能力 | 相对原版的改动 | 入口 / 说明 |
| --- | --- | --- |
| 品牌、主题、首页 | Sources 品牌；8 套本地色板和深浅色，首页与控制台同源同步；独立首页与内容编辑器；福利按钮只导航 | `extensions/sources-brand`、`extensions/sources-portal`、`frontend/src/features/sources/`；[主题偏好](docs/sources/THEME-PREFERENCES.md) |
| API 域名的主站提示 | 直接用浏览器打开 API 域名时，404 JSON 的错误信息里附带主站地址；状态码、类型和模型接口不变 | `deploy/nginx/sub2api-api-only-locations.conf` |
| 上游账号余额 | 账号列表增加上游钱包余额列和配置抽屉；支持 New API 个人访问令牌和 Sub2API 复用账号 Key 两种来源；来源加密存于 Portal；凭据、地址或代理变化需要重新确认关联；查询失败不显示为 0；只改模型映射等元数据时保留已验证的绑定和快照 | `account_wallet*`、`features/sources/balances`、Portal；[上游余额](docs/sources/UPSTREAM-BALANCE.md) |
| 余额自动刷新 | 持久化余额快照，成功后定时刷新，长查询与短轮询分离并限制并发 | `account_wallet*`、Portal |
| 共享钱包与提醒 | 管理员把多个账号关联到同一钱包、指定代表账号；低余额与恢复提醒按事件去重投递 | `wallet*`、Portal |
| V1 渠道监控 | 默认黄色 20 秒、总超时 60 秒且可热更新；管理员手动排序；覆盖请求为 stream 时按 SSE 终态判断；管理员列表附带探测 Key 当前所属计费分组 | `channel_monitor*` |
| V2 渠道卡片 | 按品牌分区的卡片，显示倍率、缓存率、可用率、首 Token 延迟和状态条；管理员可两级排序、覆盖分类、选择生图分组方式，配置与版本号同事务保存；普通用户只看授权展示的数据；V1 保留 | `features/sources/monitor-v2-cards`、`channel_monitor_v2_display_order*`；本地预览见 [extensions/sources-monitor-local](extensions/sources-monitor-local/README.md) |
| 用量缓存列 | 管理员用量表的“性能”列改为缓存比例；缺失或零分母显示为未知；原生 TPS 保留 | `features/sources/usage` |
| Fast / Ultrafast 档位 | 分组档位策略、自定义价卡、倍率预设和不可变账单快照；显式开启的分组可按 Key 档位选择固定供应商账号，普通分组行为不变；覆盖 HTTP、SSE、WebSocket | `request_speed*`、`openai_group_service_tier*`、`features/sources/pricing` |
| Grok 内容拒绝费 | 可选的固定拒绝费：请求前预留余额和 Key 额度，每个请求只结算一次；已发送的同步请求结果未知时，超过持久化截止时间（30 分钟生命周期+10 分钟宽限，未知时缩短为 10 分钟）由后台释放固定费预留；从未知释放的迟到结果只记录不扣费，从已发送释放（进程推定已停止）后迟到的普通用量仍按普通路径扣一次，迟到拒绝不扣费；绑定任务的请求与升级前已在等待的历史冻结仍待人工核对；结果未知或免费释放的预留记录上游响应的状态码和请求标识（`X-Client-Request-ID`、`X-Request-ID`、`CF-Ray` 及账号指定的请求号头），超时释放后保留，只用于对账、不参与扣费；设置项 `upstream_rejection_deadline_release_paused` 可暂停后台超时释放；一次预留只覆盖一个上游请求，因此开启该策略的分组里 composer 模型不接受图片输入（原版会先为每张图另发一次描述请求），直接返回 400，不发出上游请求、预留立即释放，未开启的分组照常桥接；规范化 Grok 模型名 | `grok_media_rejection*`、`group_upstream_rejection_billing*`、`upstream_rejection_trace*`、`handler/upstream_rejection_*` |
| DeepSeek 工具 | 可选的联网搜索与抓取：分组售价、共享供应凭据、Key 级自动工具；在 Responses、Chat、Messages 三种协议中执行；保持真实流式；按确认的用量结算并可从检查点恢复；工具失败时降级为不联网回答；注入的工具名稳定，以利于前缀缓存；识别两族工具类型：`web_search*` / `web_fetch*` 与供应商风格的 `deepseek_web_search` / `deepseek_read_url`；托管循环不接手时（分组未开启工具或 `tool_choice` 为 `none`）转发前移除供应商风格的条目，避免供应商执行未计费的工具。关闭工具的分组收到 Chat 请求时，按联网工具家族的类型前缀过滤，客户端函数保留。固定 Chat 账号接收 Responses 形状的请求也走此过滤。`tools` 为数组时才严格解析两个工具字段的内部内容。没有联网条目时原样返回。需要追加不可用提示时写入最后一个 `messages`，固定 Chat 账号的 Responses 形状则写入 `input`。`tool_choice` 统一按目标名匹配，缺少名称时按类型匹配。显式命名的保留工具优先。还有工具时把失效选择改为 `auto`。工具删空时省略 `tools` 以及失效选择或字符串 `required`，保留 `auto` 和 `none`。本地校验拒绝及过滤期间的上下文取消或超时都不计入账号失败。同一分组发往 DeepSeek Chat 上游时，取得连接或写出请求后遇到传输错误会结束本次请求。此路径禁止 HTTP 客户端重放已写出的请求体，包括 HTTP/2 的 `REFUSED_STREAM` 重试。在缓存满、客户端获取失败、DNS 或拨号失败等场景，传输能确认请求尚未取得连接且未写出时仍可换号。未取得用量的失败请求不扣费。工具在准备阶段无法使用时请求按普通路径回答而不报错（客户端声明的联网工具被移除并附上不可用提示）；未使用的原生透传分支已删除。独立工具接口 `POST /v1/tools/{search,fetch,ocr,asr,tts}` 是单次转发：OCR 按确认页数、ASR 按秒（向上取整，按小时定价）、TTS 按输入字符（汉字计 2，按 1 万字符定价）计费，价格固定、不乘分组倍率，未定价的工具不提供；请求在发给供应商之前检查大小、格式和时长并预留预估金额；在本分组启用工具的账号中调度，只在供应商明确拒绝且尚未转发任何内容时换号，结果未知的请求不重发；失败、空结果和错误包体不计费（ASR 按时长计费，静音同样计费）；供应商采购价改为可选 | `deepseek*`、工具 journal |
| 工具上传准入与付费超时 | 独立工具在读取正文前检查计费资格并应用单工具大小限制，复用原版入站重复字段检查。独立工具、对话内托管工具、OpenAI 和 Grok 的联网搜索及媒体生成、`alpha/search` 等付费提交收到 HTTP 408 后按结果未知处理，停止自动重发。Responses、Chat 和 Messages 按实际转发的工具判断是否可能执行供应商检索，包括公网、文件、集合与连接器。强制已声明的客户端函数或 `allowed_tools` 明确只允许此类函数时，未选中的检索工具不影响重试。函数按其名称和命名空间匹配。已开始的 Responses SSE 流以 `response.failed` 报告结果未知。搜索模拟和管理端测试搜索都只有确认请求尚未发送时才换供应商。用量仍按已确认的结果结算 | `AdmitDeepSeekToolBody`、`openai_gateway_upstream_errors.go`、`openai_images*.go`、`openai_alpha_search.go`、`websearch/manager.go` |
| 网关准入与搜索凭据 | 重复字段检查按原拼写识别自定义 `metadata` 键，并保留结构体绑定的协议字段比较规则。搜索模拟的 Exa 供应商只跟随同源重定向 | `requestmodel/conflict.go`、`websearch/exa.go` |
| DeepSeek 强制工具调用 | 客户端强制 `tool_choice`（`required` 或指定函数）时对该请求关闭思考，避免上游以“思考模式不支持该 tool_choice”拒绝；其余请求不变 | `deepseek_forced_tool_choice.go` |
| 媒体引擎持久图片任务 | 专用分组的异步图片任务：冻结价格快照、唯一执行者、提交结果未知不重发、结算与释放互斥 | `extensions/sources-media-engine`、`sources_media_engine*.go`、`sources_image_ledger.go`；[媒体引擎实施](docs/sources/MEDIA-ENGINE.md) |
| 图片任务自动收尾 | 可选的恢复策略：有上游任务号只查询；超期且无成功证据时按原冻结释放客户余额，供应结果仍标为未知；不重新生成 | 媒体引擎恢复器、`features/sources/ImageTaskReview.vue` |
| USDT 支付保护 | 原生 BEpusdt provider、主动查单、唯一付款凭证、旧订单配置版本、迟到与重复回调、跨实例读取配置 | 原支付服务与 provider；[支付实施](docs/sources/PAYMENT-IMPLEMENTATION.md) |
| 工作台 / Canvas 授权 | 内嵌会话恢复；为用户发放普通、可见、可撤销的 Key；原生“使用密钥”与“导入到 CCS”保留；登记的原生 Gemini 分组可用于工作台；管理员可在页面内在线登记原生分组；用户可用自己的 Key 建立自定义连接，选择管理员允许的本站 API 子域名；路径由系统推导，Antigravity 固定走主站，兼容旧连接 | 前端挂接、Portal；[Portal 说明](extensions/sources-portal/README.md) |
| Portal 启动校验 | 正常启动与只读检查共用配置校验，在打开状态前校验监听主机和数字端口，实际绑定在打开状态后进行。比较源时按协议、主机和端口比较，规范化 ASCII 大小写、标准默认端口和等价 IPv6 写法。工作台与画布必须使用独立的 HTTPS 源，可使用符合格式的私网与回环地址。登记文件接受 `[]`，拒绝 `null` | `extensions/sources-portal/cmd/portal`；[Portal 说明](extensions/sources-portal/README.md) |
| 智商检测计划 | Portal 内的定时模型检测：管理员写、用户只读，可见性、排序、删除；兼容不限期、不限总额的 Key；Key 失效时可暂停计划 | `extensions/sources-portal` |
| serving / 唯一后台 | 进程分为接流（serving）和后台（background）两种角色，后台唯一性由文件锁保证；正常排空；`-migrate-only` 迁移入口；管理 MCP 与 OAuth 回调归后台；后台退出时等待在途 OAuth 刷新保存。管理入口复用 OpenAI OAuth 刷新锁及条件写入。受限 OpenAI 入口在 PostgreSQL 会话上先等待范围锁，再在独立时限内提交凭据条件写入。HTTP 取消和锁等待不会用掉保存时限；后台交接期间旧后台只读排空，停后台服务时仍应答管理读请求（写、MCP、WebSocket 升级返回 503），最后才关闭监听；探测类任务快速取消且不记假失败；启动致命错误直接写 stderr；发布用一次性命令 `-list-pending-migrations`、`-check-startup`、`-export-web-assets` | `internal/processrole`、`cmd/server/sources_*.go`；[发布与部署](docs/sources/DEPLOY.md) |
| Portal 退出交接 | 关闭时取消只读余额查询，包括同步刷新；退出取消不写假失败，余额记录、检查计划和告警状态保持原样；日志显示在途 HTTP 请求、后台等待、检测当前轮次与排空耗时；已接受的写入和已开始的检测提交或轮询保存完成后才释放状态目录文件锁，不强杀、不重放结果未知的付费请求 | `extensions/sources-portal/cmd/portal/lifecycle.go`、Portal Service；[Portal 说明](extensions/sources-portal/README.md) |
| 日志保留归属 | 外部日志保留归属开关（默认关闭）；开启后拒绝原生用量清理；管理员统计在明细删除后仍保留汇总 | `external_log_cleanup.go`、retention 统计；开关只表示归属，不会触发删除 |
| 备份时限与磁盘 | 备份总时限两小时，租约与截止时间隔离；完整 gzip 只生成一次，各分片按偏移直接从该文件上传，不再另存分片临时文件，避免两倍磁盘占用 | 原备份服务 |
| 依赖安全与协议兼容 | 已登记的安全依赖升级、图片解码修复和上游兼容修复 | 核心清单中的 `dependency_security`、`image_decoder_security`、`upstream_compatibility` 标签 |

## 已撤销的能力

- 自定义 OpenAI 会话粘性与 provenance 增强已整体撤销，恢复原版调度、粘性会话与历史恢复路径。不再恢复。
- 旧私有导入向导、订阅升级、邀请注册、站内签到奖励、私有调度与媒体包装没有迁入；相关历史数据保留。

## 不在本仓库的内容

工作台/Canvas 客户端、福利系统、支付商户服务、QQ 机器人等是独立项目。本仓库只包含与它们对接的适配代码，不能视为它们的完整实现。

## 如何更新本表

1. 改能力时更新对应行和单项文档；新增、改变、撤销分别写明，未验证的部分写清楚。
2. 上游同步先固定新基线，对比全部路径；每项核心差异要有结论：保留、移植、被原版替代或明确退出。更新机器清单必须经过源码审查。
3. 运行 `python3 scripts/sources_extensions/core_parity.py` 核对核心范围和哈希。
4. 对受影响功能做回归测试。计费、支付、身份和持久任务不能只测页面；差异清单不能代替测试。
5. 规则见 [AGENTS.md](AGENTS.md)，开发方式见 [DEV_GUIDE.md](DEV_GUIDE.md)，文档索引见 [docs/sources/README.md](docs/sources/README.md)。
