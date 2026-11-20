# 原版核心文件的补丁说明

原版基线见 [.upstream-sub2api](../../.upstream-sub2api)，能力总表见 [SOURCES](../../SOURCES.md)。本文按功能分组说明对原版 `backend/`、`frontend/` 和 `Dockerfile` 中文件的改动；逐文件的路径、sha256 和功能标签在机器清单 [CORE-PATCHES.json](CORE-PATCHES.json) 中。

## 登记规则

- 相对原版基线有变化（修改或新增）的每个核心文件都必须单独登记，不接受目录级豁免。
- 登记的哈希是审查后的文件内容。不得为通过检查而只刷新哈希；上游同步时，未变化的登记沿用原哈希，只有逐项审查过的文件才更新。
- 文件被新的原版版本完整覆盖（字节相同）后，从清单中移除。
- 校验：

```sh
python3 scripts/sources_extensions/fetch_baseline.py
python3 scripts/sources_extensions/core_parity.py
python3 scripts/sources_extensions/check_boundaries.py
```

`core_parity.py` 要求核心范围内的差异集合与清单完全相等，并逐个核对哈希。它只证明“没有未登记的差异”，不代替功能测试。

## 上游同步

同步一个新的原版版本时：先固定新基线，再把原版增量应用到本仓库；与已登记文件重叠的路径逐个审查、合并，保留二开语义；原版新增的迁移保持原字节，并通过 `migration_lint.py`（例外登记在 `deploy/sources/migration-allow.txt`）。本仓库的 `VERSION` 不跟随原版。

## 分组说明

### 进程角色、唯一后台与发布命令

标签：`native_process_lifecycle`、`live_instance_concurrency_preservation`、`media_engine_admission_isolation`、`release_handoff_p1`、`release_handoff_p2`、`native_startup077`、`release_validation`、`background_handoff_read_only`。

- `internal/processrole`：serving / background 角色、后台唯一文件锁、只读 serving 初始化、`/internal/sources/runtime` 与 `/drain` 接口；排空时让已有 HTTP、流式、图片和 WebSocket 请求完成，并保留活跃并发槽位。
- `-migrate-only`：只执行经审查的待执行迁移，复用原版校验与执行器，不实例化完整服务。
- 管理 MCP 与 OAuth 回调归后台；用户授权页留在 serving。支付 provider 与图片存储配置跨实例读取（图片存储使用短时缓存，配置变化才新建客户端）。
- `token_refresh_service.go`：停止时不再发起新的刷新轮次、重试和限速等待，但不取消已经在和供应商通信的刷新；该刷新使用与调用方解耦的上下文，仍受单次超时限制，完成后照常保存，超过宽限期才强制取消。降级路径在单次超时后一律不保存凭据。
- `channel_monitor_runner.go`、`channel_monitor_service.go`、`scheduled_test_runner_service.go`：停止时取消在途探测和测试；被调用方取消的探测不写历史，被中断的定时测试不保存结果、计划保持到期。
- `cmd/server`：`-list-pending-migrations`、`-check-startup`、`-export-web-assets` 三个一次性命令，`main.go` 只增加一处调用；启动阶段的致命错误直接写 stderr，不受日志级别过滤。设计见 [DEPLOY](DEPLOY.md)。
- 后台只读排空（`background_handoff_read_only`）：`processrole/handoff.go` 在 background 排空后只放行管理 `GET`/`HEAD`（会访问上游供应商的查询和媒体引擎审阅除外，清单逐项写明原因），其它方法、MCP、WebSocket 升级在处理器之前返回 503（原生错误信封、`Retry-After`）；统计 SIGTERM 前进入、排空期间会被拒绝的管理请求（升级连接除外）并可等待其结束；取消已升级管理连接的请求上下文；运行时状态报告 `admin_during_drain`、`active_admin_writes`、`stopping`。`cmd/server/sources_shutdown.go` 定义关闭顺序：serving、standalone 不变（排空、关监听、等在途请求、停其余服务）；background 先等写请求、停后台服务、关升级连接，最后才关监听。`wire.go`、`wire_gen.go` 的 `provideCleanup` 拆成“后台服务”和“其余”两段，各执行一次、顺序不变，后台服务每一步在停止期间计入 `stopping`。锁的取得与释放不变。

### V1 渠道监控

标签：`monitor_timing`、`monitor_v1_ordering`、`monitor_streaming`、`channel_monitor_usage_group`。

- `channel_monitor_timing.go` 统一参数校验与快照，复用原版 settings。默认黄色 20 秒、总超时 60 秒；黄色阈值 ≥1 且小于超时，超时 2–300 秒。单次探测使用不可变快照，在途请求与历史色条不重判；旧客户端省略字段时不覆盖已有设置。探测 Client 按请求浅拷贝超时，响应头和 Body 共用同一预算。设置页增加两个输入，无新表或迁移。
- 手动排序：列表按 `sort_order ASC, id ASC`，新建项追加，普通编辑不写排序字段；批量排序在同一事务中按稳定顺序加行锁保存，目标缺失时整批失败。迁移 `246_sources_channel_monitor_sort_order.sql` 只在列不存在时添加并回填。
- 覆盖请求的最终 Body 中 `stream` 为真时按 SSE 解析，分别校验完成事件、流内错误、断流和响应大小；支持 OpenAI 兼容的 Chat 与 Responses。
- 管理员列表附带探测 Key 当前所属的计费分组 ID：按本地 `api_keys` 的精确关系批量解析，不输出 Key；Key 或分组不存在时返回 null。

### V2 渠道卡片

标签：`channel_monitor_v2_cards`。

原 V2 页面接入独立展示模块：用户看到卡片，管理员可展开原诊断。倍率沿用用户分组接口。展示排序写入独立的 settings 键，与原配置版本号在同一事务中保存，旧客户端省略时保留。公共统计只增加两个是否有数据的布尔值，区分“无数据”与“为零”而不暴露请求量。生图可集中在末尾分类或放回原品牌末尾，逐渠道分类覆盖优先。管理员降权后立即清除旧作用域并拒绝晚到的响应。V1、网关、账本和调度不变。

### 品牌与本地主题

标签：`brand_build`、`brand_palette`、`local_theme_preferences`。

`extensions/sources-brand/tokens.mjs` 提供 8 套色板和 CSS 变量，`appearance.ts` 管理本地偏好与同源同步；Vue 订阅层和侧栏选择器在 `features/sources/`，原组件只保留小挂接。无新增 API 或数据库设置。详见 [THEME-PREFERENCES](THEME-PREFERENCES.md)。

### 管理员用量缓存列

标签：`admin_cache_display`。

原“性能”列改为“缓存”：缓存读取 /（普通输入 + 创建 + 读取），缺失、非法或零分母显示短横线。原生 TPS 留在延迟列。组件在 `features/sources/usage/`，原表格只接一个插槽，旧的隐藏列偏好映射到缓存列。计费与导出逻辑不变。

原版的“使用密钥”和“导入到 CCS”入口保持原版字节，不属于补丁。

### 原生支付核验

标签：`verified_native_payment`、`cross_instance_payment_provider`、`mobile_payment_labels`。

这是有意保留的资金保护，支付核心不宣称与原版零差异：原生 BEpusdt provider、集中的付款证据事务、配置版本与收据存储、原履约与持久 post-commit 补偿、待核验订单状态；移动端显示完整链名。详见 [PAYMENT-IMPLEMENTATION](PAYMENT-IMPLEMENTATION.md)。

### 媒体引擎与任务核验

标签：`media_engine_native_boundary`。

最小的异步 handler 与 Wire 钩子、可选的原生资金 ledger、外置 SQLite 模块依赖、对象引用换签、CORS `Idempotency-Key` 和管理员核验界面。普通 OpenAI/Grok 处理保持原版。共享结算增加“已持久冻结余额，不再扣减余额缓存”的选项，默认关闭，不影响普通请求。定价、调度和平台额度复用原生方法。详见 [MEDIA-ENGINE](MEDIA-ENGINE.md)。

### 上游余额与钱包

标签：`account_upstream_balance`、`account_wallet_refresh`、`account_wallet_management`、`wallet_metadata_edit`。

- 原生账号表挂接余额列、批量刷新和右侧抽屉；逻辑集中在 `features/sources/balances`，复用原登录刷新。管理员只读接口对最多 100 个账号 ID 返回 HMAC 身份标记，不返回凭据。
- 独立 Sub 余额查询地址的窄范围配置接口、HMAC 测试凭证和配置子树 CAS。
- 余额快照、自动刷新租约、共享钱包成员和提醒。
- 只把 `model_mapping`、`compact_model_mapping`、`openai_capabilities` 视为与余额查询无关的元数据：保存账号时在事务内读取加锁的旧值，用完整身份哈希 CAS 续接钱包成员和快照。Key、URL、代理、协议、请求头或账号代次变化时不续接。快照领取与账号编辑使用同一行锁，防止晚到的旧请求清空刚续接的快照。

### Fast / Ultrafast 档位与 Key 分流

标签：`group_service_tier_pricing`、`upstream_022_request_speed_*`。

分组档位策略与倍率、不可变账单快照；显式开启的分组可让 Key 选择档位并固定到对应的供应商账号，选路、重试和恢复只在所选档位内进行，覆盖 HTTP、SSE 和 WebSocket；普通分组保持原行为。原生 Key、分组、账号的档位配置在同一事务中保存。上游返回的实际档位只用于观察，不改变选路和计费。

GPT-6.2 价卡的 Ultrafast 倍率留空时，按本仓库既有售价固定为 6 倍（`keepSourcesGPT62CardUltrafastDefault`），不采用原版 2027-03-01 前 3 倍、之后 6 倍的默认值；需要原版口径时在价卡上填写 Ultrafast 倍率，价卡值始终优先。没有价卡的 GPT-6.2 请求按原版目录价和原版默认倍率计费。原版对自带强制标志的 Pro 套餐检查保持原样，不扩展到本仓库的显式 Ultrafast 策略和 Key 档位。

### Grok 拒绝费与模型目录

标签：`grok_content_rejection_billing`、`content_rejection_public_disclosure`、`grok_canonical_model_catalog`。

可选的固定内容拒绝费：请求前预留余额和 Key 额度，每个请求只结算一次；已发送的同步请求结果未知时，按持久化的截止时间由后台释放，绑定任务的请求由任务终态结算；面向用户的说明不暴露供应商细节。导入规范的 Grok 模型名。

- `openai_plugin_transport.go`、`grok_media.go`：在实际发出请求的位置各加一行，记下上游响应的状态码和请求标识（`X-Client-Request-ID`、`X-Request-ID`、`CF-Ray` 及账号的 `upstream_request_id_header`）。
- `upstream_rejection_trace.go`、`openai_gateway_rejection_billing.go`：把这些标识写入账本证据。结果未知、免费释放和收取固定费的预留都带上；超时释放时保留释放前记下的标识。记录的状态码是上游响应实际携带的，网关自己合成的错误码不写入。它们只用于与供应商账单逐笔核对，不参与任何扣费判断。
- 三个固定标识只保留可见 ASCII、最长 128 字节，不符合的整个丢弃。账号声明的请求号在所有结算路径上（含 `grok_media_rejection.go` 的视频任务核对）先确认数据库能存：含控制字符或非法 UTF-8 时丢弃，超过用量表列宽时按用量行的方式截断。响应头的内容不会让任何一次结算失败。
- `upstream_rejection_recovery.go`：设置项 `upstream_rejection_deadline_release_paused` 为 `true` 时跳过超时释放并保留预留；不存在或为 `false` 时照常执行；其他取值（含空值）或读不到时按暂停处理。每次释放前都重新读取，设置后正在进行的一轮也立即停下。释放汇总和暂停状态的变化以 warn 级记录，只记 warn 及以上的部署也能看到；有释放失败的一轮记为 error。
- `openai_gateway_grok.go`、`openai_gateway_chat_completions_raw.go`：原版为 composer 模型的每张输入图片先发一次描述请求，再发正式请求。一次预留只覆盖一个上游请求，所以请求带着预留时，桥接在发出第一个描述请求之前返回，直转入口据此回 400（该分组下此模型不支持图片输入）。此时没有任何上游请求，预留按“未发送”立即释放、不收费。没有预留的请求（未开启该策略的分组）桥接行为不变。判断与错误定义在 `upstream_rejection_gateway.go`。

### DeepSeek 联网工具

标签：`deepseek_tools`、`deepseek_builtin_tools`、`deepseek_tool_compat`、`deepseek_tool_endpoints`、`deepseek_in_chat_tool_types`。

托管工具执行器：保留客户端 `stream`，复用现有协议事件转换器实时转发公开文本，内部工具调用留在执行器内；首字在有效内容写出后记录。工具名在请求之间保持稳定。工具失败时降级为不联网回答。逐轮确认用量，预留、结算和去重沿用原有路径，客户端断开后不再启动新工具。

可选工具不拖垮普通请求（`deepseek_tools_hosted.go` 入口；`tryForwardDeepSeekTools` 不接手时把要走普通路径的请求体交还 `openai_gateway_forward.go`、`openai_gateway_chat_completions.go`、`openai_gateway_messages.go` 的调用点）：

- 识别两族联网工具类型：`web_search`、`web_search_preview`、`web_search_20*`、`web_fetch`、`web_fetch_20*`，以及供应商风格的 `deepseek_web_search`（等同搜索）和 `deepseek_read_url`（等同抓取）。工具可用时两族都由托管循环执行并计费，三种协议一致；强制选择供应商风格工具与强制 `web_search` 相同。
- 托管循环不接手（分组未开启工具，或 `tool_choice` 为 `none`）时，转发前移除供应商风格的条目：普通路径会把它们原样发给供应商，由供应商执行按次收费的工具，而这类请求只按 token 计费。`tool_choice` 不是 `none` 时附上联网不可用提示。Chat Completions 请求不经协议转换原样转发，`web_search*` / `web_fetch*` 也会到达供应商，被当作未知类型拒绝整个请求，或被当作函数声明而回复调用客户端从未定义的函数，所以在 Chat 路径上这两族条目一并移除；Responses 和 Messages 的协议降级本就会丢弃它们，Messages 路径之后还有原版的搜索模拟需要看到它们，因此这两种协议只移除供应商风格的条目。移除是对请求体的就地编辑，其余字节保持客户端原样。
- 准备阶段失败（后台请求、没有可用的工具账号、价格未配置、请求形态不支持、用量日志不可写）时尚未向上游发送或预留任何内容，并记一条警告：只有网关自动附加的工具时，请求按客户端原样转发；客户端自己声明了联网工具时，移除两族的全部条目和 `max_tool_calls`，附上不可用提示，按普通请求转发和计费，不再返回错误。
- 移除工具后，`tool_choice` 若强制的正是被移除的工具，改为不强制；`tools` 为空时连同 `tool_choice` 一起删除。提示文案与托管循环降级时使用的是同一条。
- 原生透传分支已删除：原来在账号开启 `deepseek_native_tools_enabled` 且使用原生 Responses 协议时把工具交给供应商执行并按其计数结算（原 `deepseek_tools_native.go`），并在共用的 `openai_gateway_response_handling.go` 中挂接了 SSE 观察。账号上已保存的 `deepseek_native_tools_enabled` 仍可通过校验，但不再有任何行为依赖它，账号表单上的开关也已移除。

独立工具接口（`deepseek_tool_requests.go`、`deepseek_tool_endpoints.go`，路由在 `routes/gateway.go`，入口 `handler/openai_deepseek_tools.go`，标签 `deepseek_tool_endpoints`）：`POST /v1/tools/search|fetch|ocr|asr|tts` 按原版独立工具端点（`gateway_web_search.go`）的做法实现为单次转发，不进入网关工具循环。

- 准入：分组须是开启工具的 DeepSeek 标准分组，且该工具有售价；Search/Fetch 沿用 `search_price_per_1k` 与 `fetch_price_per_1k`，OCR、ASR、TTS 的售价是 `groups.deepseek_tools` JSONB 中的新键 `ocr_price_per_1k_pages`、`asr_price_per_hour`、`tts_price_per_10k_chars`，`null` 表示不提供，`0` 为免费，无迁移。价格按单位固定，不乘分组或用户倍率。
- 请求检查在任何供应商调用之前：每个工具有请求体上限（Search/Fetch 1 MiB、TTS 64 KiB、ASR 9 MiB、OCR 101 MiB）；OCR 与 ASR 必须是只含文档所列字段的 multipart，按文件签名识别类型，ASR 只接受 16 位 PCM WAV 或 Ogg Opus 并读取时长（≤120 秒）；TTS 只接受 `text`、`voice_id`、`format`，计费字符 ≤1 万。Search 查询、Fetch 地址和 TTS 文本经过内容审核。计费准入在读取请求体之前完成。
- 计费：只按供应商成功响应确认的单位结算——OCR 为 `page_count`（至少一页结果，最多 1000），ASR 为供应商返回的 `ceil(duration_ms/1000)` 秒（按音频时长计费，`text` 为字符串即可，静音得到的空文本同样计费；`text` 缺失或不是字符串不计费；文件声明的时长只用于发送前检查，不决定扣费；上限按文件大小推出，每 16 字节最多 1 秒（对按不连续传输编码的静音也成立），只用于挡住离谱的返回值），TTS 为请求文本的计费字符（汉字计 2）。失败、空结果（ASR 除外，见上）和错误包体不计费。五个工具共用 `DeepSeekToolUsage` 快照与 journal；Search/Fetch 的快照字段和账单指纹保持原样。供应商采购价（含三个新工具）改为可选，缺失按 0 计入利润统计。
- 预留：按请求预估金额（1 次；ASR 120 秒；TTS 实际字符；OCR 只有签名校验过的单帧图片（PNG、JPEG、简单 WebP、头部一致的 BMP）按 1 页，PDF、Office、TIFF、GIF 等可能多页的文件一律按 1000 页）做固定费准入与在途预留，请求结束时释放一次。OCR 的预留因此是扣费上限。
- 账号：在本分组可调度、启用了工具的 DeepSeek API Key 账号中用原生调度器选择，`account_id` 仍可固定。列表来自调度快照的精简投影，是否提供工具按每个账号的完整记录判断。只有供应商在转发任何内容之前明确拒绝（401、402、403、404、405、429、500、503 或请求无法构建；一个账号开关同时启用五个工具，上游缺少某个工具时返回 404/405）才换下一个账号，每次尝试使用独立的 journal 条目，账号槽位在每次尝试结束时释放；发出后连接失败、502、504，以及 200 之后响应体被截断、过大或无法解析，都视为结果可能已完成，不重发、不计费并告知客户端不要自动重试。对话内托管工具的选号不变：不固定账号时只使用自身提供工具的模型账号，否则工具不可用（走已有的放行逻辑）。
- 超时与断开：Search/Fetch 60 秒，ASR 5 分钟，OCR 10 分钟，TTS 15 分钟；OCR 与 ASR 在客户端断开后让供应商调用完成并计费，用户并发槽位也持有到处理结束，不随断开释放；TTS 随客户端断开取消上游。OCR 在等待期间转发供应商发送的前导空白以保持连接，并在结算后再发送结果；TTS 音频直接流式转发（音频长度只随音频本身发出），开始收到音频后客户端断开或网关自己的截止时间到达仍按输入字符计费，只有供应商中途中断不计费。
- 未扣费结束的 journal 条目（换账号、被拒、失败）保持 pending，到截止时间后由后台恢复任务按“无用量”关闭，与现有失败路径相同；现有接口不能在不写用量行的情况下提前关闭它们。

强制工具调用（`deepseek_forced_tool_choice.go`，在 `openai_gateway_cc_pipeline.go` 的 Chat Completions 共用出站点调用一次）：DeepSeek 默认开启思考，并在思考开启时拒绝强制的 `tool_choice`（`required` 或指定函数）。对这类请求关闭思考并去掉 `reasoning_effort`，保留客户端要求的工具调用；`auto`、`none`、未设置以及其他上游的请求字节不变。三种入站协议都经过这个出站点。

### 工作台、Canvas 与智商检测嵌入

标签：`embedded_session_recovery`、`canvas_embedded058`、`studio_compact_host_shell`、`studio_conditional_key_name`、`studio_release056`、`intelligence_embed`。

主站只增加精确同源路径的无凭据链接、外观消息和既有会话续期挂接。业务逻辑在 Portal 和独立客户端中。

### 日志保留与备份

标签：`external_log_cleanup_owner`、`admin_history_retention`、`backup_execution_deadline`、`backup_bounded_disk`。

- 外部日志保留归属开关，默认关闭；开启后拒绝原生用量清理。管理员历史统计在明细删除后保留紧凑汇总。
- 备份总时限两小时，租约与截止时间分离。完整 gzip 只生成一次，分片按偏移从该文件上传，不再另存分片临时文件。

### 依赖安全与兼容

标签：`dependency_security*`、`image_decoder_security`、`upstream_compatibility`。

只批准依赖版本与校验和的变化（例如升级 `golang.org/x/image` 修复 WebP 解码漏洞），保留原有解码与尺寸检查。

## 回退

回退到原版基线会恢复原版的监测默认值；新增的 settings 键可以留存，不需要恢复旧数据库或重算历史。余额相关回退保留来源和关联文件，不改变 Sub 账本；Portal 同时承载首页和工作台，不能为退出余额功能而停止整个 Portal。
