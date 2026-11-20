# Sources Portal — 小型外部扩展

单个 Go 服务承载首页/内容编辑、个人令牌余额监测及普通 Key 工作台授权。没有 Sub 数据库连接，不管理用户余额，不持有管理员 API Key。媒体引擎已接入 Main 原生资金边界，USDT 使用 Main 原生 BEpusdt 适配，扣费都不经过 Portal。能力总表见 [SOURCES](../../SOURCES.md)。

## 已实现

- 保留原首页非订阅 UI、3D 日历和动画；“前往福利中心”只导航到现有站内福利中心的自定义菜单页。
- 管理员内容编辑、预览、草稿、发布、版本恢复和并发冲突检查；配置在扩展存储，不在 Main DB。
- New API 账号个人访问令牌读取 `/api/user/self` 钱包；`/api/status` 读取单位而不发送PAT。误填模型sk Key拒绝。
- 原生账号列表余额列与配置抽屉；来源独立加密保存，一来源可绑定多个账号，同站不同钱包分别保存。Main 只提供管理员只读 HMAC 身份标记，换模型 Key/地址需重新确认关联。USD/CNY/TOKENS/CUSTOM、30分钟轮询、5分钟错误退避、手动15秒防抖、最大并发2、保留最后成功值。低额/恢复 outbox 至少一次投递，接收端按事件ID去重。
- 原版用户 API 发放普通、可见、可撤销的工作台 Key；每人/组30天有效。外部只保存Key ID/操作号，不保存用户JWT或Key。撤销/过期不补建；用户通过原版Key页面管理/续期。
- 管理员在工作台/画布页面里在线登记原生分组并选择模型，保存即生效，不需要改服务器文件或重启；媒体引擎分组仍只由登记文件配置。见“在线分组登记”。
- 自定义连接：用户用自己在本站已有的一把 API 密钥连接工作台/画布，只保存该 Key 的 ID；站点域名固定，用户选择管理员允许的 API 子域名，路径由系统推导。见“自定义连接”。

- 智商测试复用同一Portal进程与锁：糖果/鹈鹕异步计划、管理员开关、原生限额普通Key、持久结果和完整作品预览。该监测Key需加密持久保存供定时执行，与只保存工作台Key ID的授权桥分开；不持久保存管理员JWT。

## 同源部署

扩展使用 **Main 相同 origin 的明确路由**，避免又建一套 SSO/session：

| 路径 | 内容 |
|---|---|
| `/brand-home` | 公共首页，填入原版 `home_content` 的完整HTTPS URL |
| `/admin/home` | 管理员可见自定义菜单；服务端仍检查管理员 |
| `/admin/balances` | 旧余额链接，跳转主站 `/admin/accounts` |
| `/studio` | 用户自定义菜单，普通Key授权与图片工作台容器 |
| `/canvas` | 可选用户自定义菜单，普通Key授权与独立无限画布容器 |
| `/public/home`、`/public/runtime` | 无敏感信息的公开配置 |
| `/intelligence-tests` | 独立智商测试页面；内嵌时隐藏自身标题，初始管理员可见 |
| `/api/intelligence-tests/*` | 管理员计划、限额、私有记录及作品 |
| `/public/intelligence-tests`、`/public/intelligence-tests/runs/*` | 只在显式开启公开模式后读取；管理员模式下即使带Bearer也拒绝公开命名空间 |
| `/api/home/*`、`/api/balance/*`、`/api/studio/*`、`/api/canvas/*` | 扩展API；由本服务鉴权 |
| `/home-assets/*`、`/ops-assets/*` | 只读静态文件 |

只把上述固定路径送到Portal，**`/api/v1/*`、`/v1/*`、登录及其它路由仍给原版 Sub**。不要把主站根路由或全部 `/api` 交给Portal。主站原自定义菜单照常创建；管理员可见只是入口控制，API检查不可省略。

工作台静态客户端由 `STUDIO_URL` 指向固定 HTTPS 页面，必须与 Main 使用独立源，防止读取 Main 登录存储。同一 Nginx 可以同时提供这两处静态文件。Portal 先规范化协议、主机和端口，再比较工作台和 Canvas 是否与 Main 同源。ASCII 大小写、标准默认端口和 IPv6 的等价写法统一处理。工作台和 Canvas 的配置仍要求 HTTPS，便于在主站页面内嵌并传递授权。每轮使用新 iframe，校验窗口、源、组 ID 和 applied ACK。旧页面迟到的消息不确认新授权。

`SUB_ORIGIN`、`PORTAL_ORIGIN`、`STUDIO_URL` 和 `CANVAS_URL` 参与这条同源边界时遵守相同规则：

- 主机使用 ASCII DNS 名或规范的 IP 文本；域名标签只含字母、数字和连字符，标签首尾不能是连字符。不做 Unicode/IDNA 转换，非 ASCII、全角字符、Unicode 点号、末尾点号及其它含糊的 authority 写法直接拒绝；已用 ASCII 表达的 Punycode 域名可用。
- 端口省略或使用不带前导零的十进制形式；显式空端口、前导零端口均拒绝。标准默认 HTTPS 端口与省略端口视为同源，规范的非默认端口仍可用。
- 可能触发浏览器旧式 IPv4 解释的非规范地址、数值式域名尾标签、带 zone 的 IPv6 或 IPv4 映射地址均拒绝。
- 工作台和 Canvas 指向独立源时，可使用规范的私网、回环 IP 或 `localhost`。浏览器页面地址只做格式和同源校验。
- 错误信息说明对应变量要求规范 ASCII 主机和端口，不回显配置值。正常启动与 `-check-config` 结果相同，这些地址配置校验失败时不写状态。

无限画布通过可选 `CANVAS_URL` 指向真实 Canvas 静态客户端。例如 `https://YOUR_SEPARATE_CANVAS_ORIGIN/infinite/canvas`；构建必须设置 `VITE_BASE=/infinite/`，同一站点根路径保留 Studio。Canvas 必须与 Main 不同 origin。仅设置环境变量不会创建菜单或发布静态包；发布时还须固定 Canvas 制品、为 Main 增加 `/canvas` 用户菜单和上述 Portal 路由，并验证模型 API 的精确跨域白名单。原 `STUDIO_URL` 和工作台路由保持独立。

## 构建/运行

Portal 镜像标签沿用仓库的发布版本，来源为 [`backend/cmd/server/VERSION`](../../backend/cmd/server/VERSION)，不维护独立版本文件。

Go 1.26+、Node和pnpm 9.15.9；外部首页采用已锁定的 Vue/Vite，色板来自 `../sources-brand`。从仓库根执行：

```sh
npm exec --yes --package=pnpm@9.15.9 -- pnpm --dir extensions/sources-portal/webapp install --frozen-lockfile
npm exec --yes --package=pnpm@9.15.9 -- pnpm --dir extensions/sources-portal/webapp build
cd extensions/sources-portal
go build -trimpath -o /YOUR_BUILD_DIR/sources-portal ./cmd/portal
```

生产凭据由运维文件提供，0600；不要提交至Git或输出到日志：

```sh
SUB_ORIGIN=https://YOUR_MAIN_ORIGIN
PORTAL_ORIGIN=https://YOUR_MAIN_ORIGIN
PORTAL_HOME_DIR=/YOUR_BUILD_DIR/homeweb
PORTAL_DATA_DIR=/YOUR_PRIVATE_DATA_DIR/state
PORTAL_KEY_FILE=/YOUR_PRIVATE_DATA_DIR/master.key
PORTAL_LISTEN=127.0.0.1:YOUR_PORTAL_PORT
# 同机隔离私网身份端及可信反代CIDR，需经实际IP/UA绑定测试。
SUB_INTERNAL_ORIGIN=http://YOUR_FIXED_MAIN_INTERNAL_HOST:8080
PORTAL_TRUSTED_PROXIES=YOUR_EXACT_PROXY_CIDR
STUDIO_CONFIG_FILE=/YOUR_PRIVATE_DATA_DIR/studio.json
STUDIO_URL=https://YOUR_SEPARATE_STUDIO_ORIGIN/
CANVAS_URL=https://YOUR_SEPARATE_CANVAS_ORIGIN/infinite/canvas
# 可选；逗号分隔的小写 DNS 标签，最多 16 个，第一个为初始默认值；非法时启动检查和启动都会失败。
STUDIO_CUSTOM_API_LABELS=YOUR_API_LABEL,YOUR_SECOND_API_LABEL
# 必填；智商测试的外部检测服务，固定HTTPS源（无路径、查询串或账号），无内置默认值，缺失或非法时启动失败。
INTELLIGENCE_PROVIDER_ORIGIN=https://YOUR_DETECTION_SERVICE_ORIGIN
# 可选；未配置时UI明确显示未接外部告警。
BALANCE_ALERT_WEBHOOK=https://YOUR_ALERT_RECEIVER/balance
```

master.key是64位十六进制、32随机字节，长期保存，不随镜像重建。上游PAT使用AES-256-GCM及记录ID+origin关联认证；配置0600原子写、fsync。进程锁确保单写者；Rename后目录同步失败时对齐内存并停止后续写入，运维核对后重启，不静默回退旧配置。

studio.json是外部能力登记，不授予分组权限；每次仍交原版判断用户可见分组。支持 `sub2api` 与 `media`（媒体引擎；已有数据的部署见下）。Main 和 Portal 共用同一份登记；媒体引擎还须显式配置 `account_ids`（Portal 仅消费用户可见能力，Main 才执行账号选择）：

```json
[{"group_id":101,"engine":"sub2api","models":["gpt-image-2"]},{"group_id":102,"engine":"media","models":["gpt-image-2"],"account_ids":[201]}]
```

### 在线分组登记

登记文件只在启动时读取。此后加到主站的分组要让用户用上，由管理员在 `/studio` 或 `/canvas` 页面的“分组设置”里登记：勾选分组、选择模型、保存。

- 在线登记保存在 `<PORTAL_DATA_DIR>/studio-registry.json`，只含分组 ID、模型列表和开关，0600 原子写；带版本号，两个管理员同时保存时后一个收到冲突并重新载入，不会互相覆盖。
- 生效的列表 = 登记文件 + 在线登记。在线登记的分组一律是原生（`sub2api`）分组，引擎不能从请求指定。同一原生分组两边都有时以在线登记为准，可用来改模型或隐藏；撤销在线修改即回到文件内容。
- 媒体引擎分组只认登记文件：Main 读取同一文件选择账号并校验模型，界面里只读显示，保存请求里出现它会被拒绝。文件后来把某个在线登记过的分组交给媒体引擎时，以文件为准。
- 登记不授予权限。用户仍然只看到 Main 判定其可用、状态为启用并已开启图片生成的分组；尚不满足条件的分组可以先登记，界面会标出原因。
- 登记文件可以是空数组 `[]`，此时全部原生分组都在界面里管理；`null` 不是数组，会被启动校验拒绝。`STUDIO_CONFIG_FILE` 仍须设置，它同时是工作台功能的开关。
- 接口在已有的 `/api/studio/` 前缀下（`/api/studio/admin/registry`、`/api/studio/admin/groups/{id}/models`），由 Portal 逐个校验管理员身份，不需要新增反向代理路由或主站菜单。Portal 为此只多调用 Main 的两个管理员只读接口：启用分组列表和单个分组的候选模型。

### 自定义连接

分组没有登记时，用户可以在同一页面用“自定义连接”自助接入：

- 用户从自己在本站的 API 密钥里选一把（列出前 100 把启用且未过期的），不粘贴密钥。Portal 通过 Main 以用户会话读取该 Key，确认属于当前用户、启用且未过期；只保存 Key ID、接口类型、API 子域名标签、系统推导的前缀和模型（`<PORTAL_DATA_DIR>/studio-custom.json`），不保存 Key 本身。刷新或重开后按 Key ID 恢复；Key 被撤销或过期时报告失效，不创建、不换用其他 Key。
- 站点域名 H 固定为 `SUB_ORIGIN` 的主机名。用户从允许列表选择标签，请求源为 `https://<YOUR_API_LABEL>.H`，不能自由填写地址；主站地址不出现在可选列表中。未配置允许列表时显示固定主站地址，继续使用 `SUB_ORIGIN`。
- 管理员在“分组设置”的“自定义连接可选的 API 子域名”中添加、移除标签并选择默认值。每项必须是一个小写 DNS 标签：字母、数字或中划线，首尾必须为字母或数字，最长 63 个字符；最多 16 项且不能重复，默认值必须在列表内。此列表沿用分组设置的版本冲突检查和原子保存。
- 生效顺序：非空在线列表 > 部署变量 `STUDIO_CUSTOM_API_LABELS` > 主站地址。部署变量为逗号分隔的标签，第一个为默认值；在线列表留空则恢复部署初始值。弹窗会显示当前来源和部署初始值。该变量非法时正常启动和 `-check-config` 都拒绝继续，不回显变量值。
- 路径不再允许用户填写。服务器按 Key 查询返回的分组平台和所选接口推导，弹窗只读预览完整生成/编辑地址：原生 Gemini 为 `/v1beta/models/<模型>:generateContent`；OpenAI、Grok 为 `/v1/images/generations`、`/v1/images/edits`，两个客户端实际沿用现有异步任务接口及任务查询。Antigravity 只允许 Gemini 原生接口，前缀为 `/antigravity/v1beta`，暂时固定走主站地址，不显示子域名选择。每次下发连接时按 Key 当前所在分组的平台重新推导：Key 换到 Antigravity 分组后改走主站和 `/antigravity/v1beta`；原来选的接口在新平台上不能用时拒绝下发（409 `custom_connection_needs_update`），提示用户重新保存。下发不改写已保存的连接。
- 模型先获取、后勾选：弹窗打开、换 Key 或换接口时，Portal 用该 Key 向 Main 的系统推导前缀下的 `/models` 读取列表（OpenAI 和 Gemini 两种列表格式都识别），按名称预选图片模型供用户调整。读取失败或列表里缺少某个模型时，才手动填写模型名称。“分组设置”同样在勾选分组时自动读取该分组的候选模型。
- 管理员移除某标签后，使用该标签的连接下发时改用当前默认地址；没有有效列表则回到主站地址。用户重开弹窗会看到提示。已有无标签的旧连接继续使用主站及原前缀，含义不变；重新保存后才采用当前选择和系统路径。前缀字段仍保存，供回滚到旧版 Portal 读取。
- 除 Antigravity 的接口限制外，不按平台拒绝 Key，能否调用由 Main 判定。媒体引擎分组的 Key 除外，其请求格式不同，应从分组列表进入。管理员登记的分组继续使用主站地址，不受自定义连接地址设置影响。
- 管理员可在“分组设置”里关闭自定义连接；关闭后已保存的连接不会被删除，只是不再下发。
- 密钥换到平台不兼容的分组后，工作台和画布会提示重新保存。即使后台刷新失败或画布正在尝试连接另一分组，“打开自定义连接”仍可用。保存后读取分组或连接超时，页面会提供重试。画布切组失败时，如果原授权仍可用，重试会复用该授权。关闭设置弹窗后，迟到的保存结果不会触发页面重新连接。
- 每个用户一个自定义连接。费用照常由 Main 按该 Key 所在分组从余额扣除，Portal 不参与计费。

两个客户端只接受名为 `group-<正整数>` 的连接，所以自定义连接使用一个保留的分组编号（9×10¹⁵ 以上，不会与 Main 的分组 ID 相交），Studio 分组菜单里的“自定义连接…”“管理工作台分组…”两个入口也是保留编号：选中后 Portal 打开设置弹窗并回复“未切换”，不会授权或换组。登记文件和在线登记都拒绝保留编号。

已有登记或 Studio 客户端在使用早期引擎值的部署，必须为 Portal 与 Main 设置同一个私有变量 `SOURCES_MEDIA_ENGINE_LEGACY_NAME=<旧引擎名>`（`^[a-z0-9]{2,32}$`，不能是 `sub2api`）。设置后登记中旧值与 `media` 都被接受，返回给 Studio 客户端的 `engine` 始终是旧值，客户端不需改动；未设置时只接受并返回 `media`，登记里的旧值会让 Portal 启动失败。该值只放在服务器私有配置里，不写入本仓库。

### 启动前检查 `-check-config`

Portal 的状态目录由进程锁保证单写者，新进程要等旧进程退出后才能启动，配置错误（例如缺少必填变量）原本要到旧进程停掉之后才暴露。部署工具应在停止旧进程之前，用新镜像和真实配置（同样的环境变量、状态目录和密钥文件挂载）运行：

```sh
/app/sources-portal -check-config
```

- 与正常启动共用配置校验，失败信息一致：先校验各环境变量、master.key 与钱包桥密钥文件的权限和格式、`PORTAL_HOME_DIR/index.html`、`STUDIO_CONFIG_FILE`，再读取状态。正常启动也只在这些配置全部通过后才获取状态锁、创建目录或执行迁移，配置错误不会改写中断任务或旧版状态。
- 状态校验包含正常启动会读取的文件——`sources.json`（含用当前密钥解密每条来源）、`account-bindings.json`、`intelligence/state.json`（含解密监测 Key）、设置 `PORTAL_HOME_DIR` 时的 `home.json`、设置 `STUDIO_CONFIG_FILE` 时的 `studio-bindings.json`、`studio-registry.json`、`studio-custom.json`。不存在的状态文件按正常启动的方式视为空状态。
- `PORTAL_DATA_DIR` 必须是已存在的目录（空目录可以）；不存在说明挂载错了，正常启动会在那里静默建一份空状态。
- 两种启动模式都在打开状态前检查 `PORTAL_LISTEN` 的 `host:port` 格式。主机部分可以留空，也可以填写 IP 或合法主机名。端口必须填写十进制数字，范围为 0 至 65535。实际绑定由正常启动在打开状态后执行，绑定失败时可能已经发生状态写入。
- 刻意不做：不取也不等待进程锁，不建目录，不写、改名、改权限或删除任何文件（启动迁移和中断任务的改写只校验输入、不落盘），不监听端口，不启动 worker，不访问网络。状态目录和密钥文件只读挂载时也能运行。旧进程运行中原子替换文件，检查期间读取是安全的。
- 检查成功时 stdout 只输出一行 `SOURCES_PORTAL_CONFIG_OK`，退出码 0。检查失败时 stderr 输出一行错误信息，退出码 1。两种启动模式的配置错误使用相同消息，且不输出密钥或变量值。其他参数输出用法，退出码 2。

## 权限和会话边界

- 主站JWT不进URL日志。原版iframe的token查询参数立即清除；代理日志使用不带查询串的 `$uri`。
- Portal只读原版localStorage的 `auth_token`，不写它、不保存refresh token、不另存Main JWT。服务端每次从固定主站身份端验证active角色，不信URL user_id/role。
- 同源 `/admin/home`、`/studio`、`/canvas` 自定义菜单内，Portal遇到身份中间件在业务执行前返回的 `admin_login_required` 401，会通过校验父窗口/origin/随机请求ID的消息，让Main复用已有身份请求及刷新锁。消息不携带凭据；只在同一用户和sid的access token更新后重试一次。旧401迟到时直接采用Main已刷新的token，网络未知结果、其他401/403及5xx不重放。单独打开且没有Main父窗口的页面过期后需要从主站菜单重新进入。
- 同源跨标签退出或换用户会锁定编辑器/画布；工作台普通Key可由用户独立撤销。真实已受理任务不因关页面自动取消或重提。
- IP/UA通过可信反代链传递。JWT未验签payload的绑定预检**只能用于提前拒绝**，不是身份认证；最终由Sub验签。禁止以关闭binding/2FA解决接入。
- 原版普通注销/revoke-all主要撤销refresh session，旧access JWT仍可存活到过期。页面退出联动不冒充API即时撤销；改密失效已通过真实原版验证。
- 上游钱包请求仅允许HTTPS公网IP，防DNS重绑定、禁环境代理和重定向；无自动充值/停调/账本写入。

余额列使用 Main 原生 API 客户端与登录刷新；上游认证错误作为业务状态显示，不触发主站退出。`account-bindings.json` 与 `sources.json` 分别保存关联和来源，复用同一个进程锁；绑定不改写来源凭据，来源被关联时禁止删除。旧版本忽略关联文件，回退不删除该文件。凭据选择与单位处理见[上游余额](../../docs/sources/UPSTREAM-BALANCE.md)。

## 本地历史

工作台按主站origin＋服务端验证的userID分开IndexedDB、设置和跨窗口锁。切组保留已授权凭据供原任务查询，余额由Sub扣费。旧未分区的浏览器历史不自动归属新登录账号，也不会删除；可用工作台已有导出/导入功能迁入确认属于自己的内容。

Canvas 使用相同身份边界，先完成父窗口授权，再打开当前账号本地存储。普通 Key 仅在内存；刷新后以原 Key ID 恢复授权，`restore_only` 不创建新 Key，撤销、过期或换 Key 后不改用其他 Key 查询旧任务。Canvas 与工作台复用同一用户/组授权绑定，不维护第二套钱包。

本次 Canvas 接入只声明图片能力：原生生成/编辑异步接口受理后记录任务ID，刷新和切组只查询原任务，结果按操作号回到原画布槽位。未拿到普通任务ID时不自动重提；媒体引擎只按原幂等号查询。视频、音频、文字没有因此获得 Main 能力。暂停页面查询不代表服务端取消或免计费。

## 测试与回退

```sh
(cd extensions/sources-portal && go test -race -cover ./... && go vet ./...)
(cd extensions/sources-portal && node --experimental-vm-modules --test session.test.mjs studio.test.mjs canvas.test.mjs settings.test.mjs)
python3 scripts/sources_extensions/core_parity.py
python3 scripts/sources_extensions/check_boundaries.py
```

测试身份与供应商夹具只编译进测试二进制，正式程序没有 mock 鉴权开关。

`internal/portal/browser_fixture_test.go` 中的 `TestBrowserFixture` 在本机启动一个使用本地身份、刷新和模型夹具的 Portal，用于真实浏览器检查 Main 自定义菜单 → Portal → Studio/Canvas 静态客户端的三层页面：过期后经 Main 现有 apiClient 刷新、仅提交预期次数的原生图片任务、刷新与切组只查询原任务、冷重载恢复旧授权、结果回填原画布槽位和账号隔离。所需变量包括 `PORTAL_HOME_DIR`（构建后的 homeweb 绝对路径）、`PORTAL_BROWSER_STUDIO_URL` 或 `PORTAL_BROWSER_CANVAS_URL`（Canvas 开发服务需设置 `VITE_BASE=/infinite/`）。两个客户端的正式构建只接受本站域名下的 HTTPS 地址，用它们联调时再设置 `PORTAL_BROWSER_API_ORIGIN`（下发给客户端的模型 API 源，默认是一个占位地址）。所有模型请求被本地夹具拦截，不产生真实收费。驱动浏览器的脚本不在本仓库中。夹具配置了两个中性 API 子域名标签，还有一个未登记的分组、用户自己的一把普通 Key 和一把 Antigravity 分组的 Key，用于检查“分组设置”和“自定义连接”两个弹窗；弹窗是页面 DOM 代码，`*.test.mjs` 只覆盖其规则和页面间的消息。

正式构建不能放宽 Canvas 的 HTTPS 父站检查；`PORTAL_BROWSER_ORIGIN` 只存在于 Go 测试二进制中，正式 Portal 没有此开关。本地夹具通过不能代替真实环境的检查。

备份小型扩展state目录及独立master.key，停止服务或持有进程锁取得一致副本；不需要为编辑首页全备Sub。内容历史恢复先回草稿。应用回退只撤下菜单/固定路由、停止Portal并保留state；不回滚用户账本或Sub数据库。工作台Key为普通Key，撤销通过原版操作，不盲删历史任务关联Key。

关闭时取消只读余额查询，包括不带 `request_id` 的同步来源刷新、账号批量刷新和异步查询，不再等上游余额查询的长时限。因进程退出而中断的读取不写失败：余额、状态、检查及调度时间、错误文字、告警状态和待投递通知均保持原样，不推送失败快照，新进程继续按原计划读取。请求自身取消或超时，以及真实上游失败仍按原路径处理。余额读取不产生付费提交。

日志在开始关闭、HTTP 排空、后台停止和排空完成时记录 `active_requests`（在途 HTTP 请求）、`http_pending`、`background_pending`、`intelligence_round_active`（检测当前轮次是否仍在进行）及 `elapsed`（排空耗时）；等待期间也会定时报告同样的状态，不输出路径、请求身份、凭据或结果正文。

仍等待已接受的 HTTP 请求和后台 worker 全部完成，再释放存储锁。本次不增加进程强制退出期限：检测 worker 已开始的提交或轮询继续使用原有调用时限，完成并保存结果后才能退出；结果未知的付费提交不重放，已有远程任务只查询恢复。取消余额读取不会中断这些付费调用，也不会让新进程提前取得状态目录锁。慢退出不得升级为强杀。

### Logo/二维码上传

编辑器支持PNG/JPEG上传（单张2MB以内、最大2048×2048），也保留URL输入。文件按内容哈希保存在独立state/assets，原文件字节不改写；草稿图片仅管理员鉴权读取，预览使用短期浏览器Blob URL。发布前公共地址404，发布后及历史已发布引用可读取；不通过不可猜URL代替权限。不支持脚本/SVG上传。备份state时须包含assets。

### Grok 能力声明

原版 Grok 分组使用 `grok_image` 客户端协议，声明原版 1k/2k 与 13 种比例；生成/编辑仍调用原版 Images 202 接口，不恢复私有 managed 网关。图片登记只包含图片模型和兼容别名；Grok 视频继续走原版 `/v1/videos/*` API，不伪装成图片能力。协议变化时工作台清理旧模型发现缓存，防止缓存中的视频模型重新混入图片选择。

## 智商测试接入与恢复

应用位于同一Vue/Vite多入口构建，复用`sources-brand`。主站精确同源链接只传`ui_mode`/`lang`和可选临时外观，不传Token/用户ID；跨帧消息只接受同源父窗口，作品预览使用独立不透明origin且不能联网的沙箱。默认完整适应窗口，100%保持同一iframe，仅改变缩放/滚动。

`<PORTAL_DATA_DIR>/intelligence/state.json`保存计划、加密普通Key、远程任务ID和幂等记录，结果正文单独存放；原生余额/计费由Main完成。检测服务来源固定为 `INTELLIGENCE_PROVIDER_ORIGIN`、无HTTP重定向、提交意图先持久化。未知提交不重发，已知任务重启后只GET；最多两个并行远程任务，超时不冒称成功。停止时等已开始的有界调用保存结果，再释放Portal锁。

新增 Portal 路由时，必须同时在反向代理配置中加入对应的精确路径；主站其余路由保持由原版 Sub 处理。检测计划会定期调用付费模型，本地夹具、真实模型和故障模拟的结果要分开记录。
