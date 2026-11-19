# 轻量主题色：浏览器偏好，不是站点业务

侧栏提供“主题色”选择和深浅色切换，使用 CSS 变量、保存在浏览器本地，首页与控制台同步；不新增数据库字段，不引入后台主题管理系统。

## 实现选择

**一份色板、一份无框架的偏好控制器、一个侧栏选择器。** 不增加后端接口、配置表、Pinia 主题 store、同步服务或跨域消息协议。

- `extensions/sources-brand/tokens.mjs`：主站和独立首页共享的8套色板、RGB变量、阴影/渐变及可读前景色。替代此前只支持黄色的 `palette.cjs`，不并存两份色板。
- `extensions/sources-brand/appearance.ts`：唯一存储/初始化/同步入口；不依赖 Vue、登录身份或 Sub 内部模块。
- `frontend/src/features/sources/useAppearance.ts`：薄 Vue 订阅层，组件卸载时退订。
- `SidebarThemePicker.vue`：选择、当前值、键盘、焦点恢复、外部点击关闭、折叠/移动端展示。
- 主站原侧栏、原首页、Key查询页只替换重复的深浅初始化；独立首页引用同一控制器。原业务、认证、账单、Logo和状态色不改变。

## 保留的原选项

| 原名称 | 本地值 | 500色阶 |
|---|---|---|
| Blue | `sources-blue` | `#00b4d8` |
| Teal | `teal` | `#14b8a6` |
| Warm Natural | `warm-natural` | `#d4a373` |
| Forest Green | `forest-green` | `#4f772d` |
| Orange | `orange` | `#f97316` |
| Yellow | `yellow` | `#f2c14e` |
| Red | `red` | `#ef4444` |
| Slate | `slate` | `#64748b` |

色板沿用旧版本的全部色阶、名称、键和值，并做哈希回归；用户已保存的任一有效选择继续生效。无偏好或非法值使用黄色默认，不由异步加载的站点配置覆盖。

## 同步合同

- 保留旧键 `theme-color` 和 `theme`，分别保存色板与深浅模式。没有另造存储格式或复制用户身份。
- 初始化在 Vue 挂载前执行；CSS也有完整默认值。
- 同源标签页/同源首页 iframe通过浏览器 `storage` 事件同步，不重新加载 iframe；返回浏览器页面缓存时再次读取。
- 未指定深浅模式时跟随系统；明确选择后不被系统变化覆盖。颜色选择和深浅模式互不覆盖。
- 本地偏好属于当前浏览器和 origin，**不是跨设备或跨域账号同步**。Portal与主站继续使用既定同源部署。
- 存储被禁用或写入失败时保留当前页面内选择并提示临时生效，不阻止渲染、不伪称已经保存。
- 只接受上述命名色板，不接受任意 CSS、URL或可执行配置。处理存储通知时只读，不回写制造事件循环。

## 可读性与布局

Tailwind 使用 `rgb(var(--color-primary-*-rgb) / <alpha-value>)`，兼容原透明度工具类。按钮、渐变、阴影、链接和首页日历读取同一组变量，而不是仅改变选色圆点。

普通主按钮根据背景选深/浅文字；无法让同一文字颜色同时满足原渐变两端时，使用该色板相邻的更深色阶。静态色板测试要求按钮默认/hover 两端文字对比度至少 4.5。强调文字、用户名链接、选中标签和图标沿用原色板 600 / 深色 400 色阶，与侧栏一致；不再全局压深浅色文字。语义红/绿状态、模型品牌色和上传Logo不重染。

首页原CTA尺寸与布局保留，浏览器测试同时断言实际按钮颜色和最小点击尺寸，避免“变量正确但布局丢失”的假通过。

## 验证与回退

入口：

```sh
node --test extensions/sources-brand/tokens.test.mjs
pnpm --dir frontend exec vitest run src/features/sources/__tests__/appearance.spec.ts src/features/sources/__tests__/SidebarThemePicker.spec.ts
pnpm --dir frontend run typecheck
pnpm --dir frontend run lint:check
pnpm --dir extensions/sources-portal/webapp run build
```

仓库命令使用固定 pnpm 9.15.9。真实浏览器检查应覆盖：主站页面与构建后的独立首页、全部 8 色、跨标签与 iframe 同步、刷新、深浅色、键盘、折叠、窄屏与手机横屏，以及存储写入失败；同时确认没有产生主题或设置的写请求。

回退只恢复主题源码和构建产物，不删除用户的 `theme-color` 或 `theme`。旧版本仍认识这两个键；只支持固定黄色的版本会忽略选色但保留偏好，重新启用后可以继续读取。主题改动不涉及数据库、支付或路由。
