# Sources Portal — 小型外部扩展

单个 Go 服务承载首页/内容编辑、个人令牌余额监测及普通 Key 工作台授权。没有 Sub 数据库连接，不管理用户余额，不持有管理员 API Key。媒体引擎已接入 Main 原生资金边界，USDT 使用 Main 原生 BEpusdt 适配，扣费都不经过 Portal。能力总表见 [SOURCES](../../SOURCES.md)。

## 已实现

- 保留原首页非订阅 UI、3D 日历和动画；“前往福利中心”只导航到现有站内福利中心的自定义菜单页。
- 管理员内容编辑、预览、草稿、发布、版本恢复和并发冲突检查；配置在扩展存储，不在 Main DB。
- New API 账号个人访问令牌读取 `/api/user/self` 钱包；`/api/status` 读取单位而不发送PAT。误填模型sk Key拒绝。
- 原生账号列表余额列与配置抽屉；来源独立加密保存，一来源可绑定多个账号，同站不同钱包分别保存。Main 只提供管理员只读 HMAC 身份标记，换模型 Key/地址需重新确认关联。USD/CNY/TOKENS/CUSTOM、30分钟轮询、5分钟错误退避、手动15秒防抖、最大并发2、保留最后成功值。低额/恢复 outbox 至少一次投递，接收端按事件ID去重。
- 原版用户 API 发放普通、可见、可撤销的工作台 Key；每人/组30天有效。外部只保存Key ID/操作号，不保存用户JWT或Key。撤销/过期不补建；用户通过原版Key页面管理/续期。

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

工作台静态客户端由 `STUDIO_URL` 指向固定HTTPS页面，必须独立origin（可以仍由同一Nginx提供静态文件，不增加业务服务），防止画布直接读取Main登录存储。每轮使用新iframe且校验窗口、origin、组ID、applied ACK；旧页面迟到消息不确认新授权。

无限画布通过可选 `CANVAS_URL` 指向真实 Canvas 静态客户端。例如 `https://YOUR_SEPARATE_CANVAS_ORIGIN/infinite/canvas`；构建必须设置 `VITE_BASE=/infinite/`，同一站点根路径保留 Studio。Canvas 必须与 Main 不同 origin。仅设置环境变量不会创建菜单或发布静态包；发布时还须固定 Canvas 制品、为 Main 增加 `/canvas` 用户菜单和上述 Portal 路由，并验证模型 API 的精确跨域白名单。原 `STUDIO_URL` 和工作台路由保持独立。

## 构建/运行

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

已有登记或 Studio 客户端在使用早期引擎值的部署，必须为 Portal 与 Main 设置同一个私有变量 `SOURCES_MEDIA_ENGINE_LEGACY_NAME=<旧引擎名>`（`^[a-z0-9]{2,32}$`，不能是 `sub2api`）。设置后登记中旧值与 `media` 都被接受，返回给 Studio 客户端的 `engine` 始终是旧值，客户端不需改动；未设置时只接受并返回 `media`，登记里的旧值会让 Portal 启动失败。该值只放在服务器私有配置里，不写入本仓库。

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
node --experimental-vm-modules --test extensions/sources-portal/session.test.mjs
python3 scripts/sources_extensions/core_parity.py
python3 scripts/sources_extensions/check_boundaries.py
```

测试身份与供应商夹具只编译进测试二进制，正式程序没有 mock 鉴权开关。

`internal/portal/browser_fixture_test.go` 中的 `TestBrowserFixture` 在本机启动一个使用本地身份、刷新和模型夹具的 Portal，用于真实浏览器检查 Main 自定义菜单 → Portal → Studio/Canvas 静态客户端的三层页面：过期后经 Main 现有 apiClient 刷新、仅提交预期次数的原生图片任务、刷新与切组只查询原任务、冷重载恢复旧授权、结果回填原画布槽位和账号隔离。所需变量包括 `PORTAL_HOME_DIR`（构建后的 homeweb 绝对路径）、`PORTAL_BROWSER_STUDIO_URL` 或 `PORTAL_BROWSER_CANVAS_URL`（Canvas 开发服务需设置 `VITE_BASE=/infinite/`）。所有模型请求被本地夹具拦截，不产生真实收费。驱动浏览器的脚本不在本仓库中。

正式构建不能放宽 Canvas 的 HTTPS 父站检查；`PORTAL_BROWSER_ORIGIN` 只存在于 Go 测试二进制中，正式 Portal 没有此开关。本地夹具通过不能代替真实环境的检查。

备份小型扩展state目录及独立master.key，停止服务或持有进程锁取得一致副本；不需要为编辑首页全备Sub。内容历史恢复先回草稿。应用回退只撤下菜单/固定路由、停止Portal并保留state；不回滚用户账本或Sub数据库。工作台Key为普通Key，撤销通过原版操作，不盲删历史任务关联Key。

关闭时会等待已接受的 HTTP 请求和后台 worker 全部完成，再释放存储锁。慢退出不得升级为强杀。

### Logo/二维码上传

编辑器支持PNG/JPEG上传（单张2MB以内、最大2048×2048），也保留URL输入。文件按内容哈希保存在独立state/assets，原文件字节不改写；草稿图片仅管理员鉴权读取，预览使用短期浏览器Blob URL。发布前公共地址404，发布后及历史已发布引用可读取；不通过不可猜URL代替权限。不支持脚本/SVG上传。备份state时须包含assets。

### Grok 能力声明

原版 Grok 分组使用 `grok_image` 客户端协议，声明原版 1k/2k 与 13 种比例；生成/编辑仍调用原版 Images 202 接口，不恢复私有 managed 网关。图片登记只包含图片模型和兼容别名；Grok 视频继续走原版 `/v1/videos/*` API，不伪装成图片能力。协议变化时工作台清理旧模型发现缓存，防止缓存中的视频模型重新混入图片选择。

## 智商测试接入与恢复

应用位于同一Vue/Vite多入口构建，复用`sources-brand`。主站精确同源链接只传`ui_mode`/`lang`和可选临时外观，不传Token/用户ID；跨帧消息只接受同源父窗口，作品预览使用独立不透明origin且不能联网的沙箱。默认完整适应窗口，100%保持同一iframe，仅改变缩放/滚动。

`<PORTAL_DATA_DIR>/intelligence/state.json`保存计划、加密普通Key、远程任务ID和幂等记录，结果正文单独存放；原生余额/计费由Main完成。检测服务来源固定为 `INTELLIGENCE_PROVIDER_ORIGIN`、无HTTP重定向、提交意图先持久化。未知提交不重发，已知任务重启后只GET；最多两个并行远程任务，超时不冒称成功。停止时等已开始的有界调用保存结果，再释放Portal锁。

新增 Portal 路由时，必须同时在反向代理配置中加入对应的精确路径；主站其余路由保持由原版 Sub 处理。检测计划会定期调用付费模型，本地夹具、真实模型和故障模拟的结果要分开记录。
