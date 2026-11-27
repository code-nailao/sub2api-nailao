# Sources 二开文档索引

## 入口

- [二开差异总表](../../SOURCES.md)：相对原版的全部能力差异及代码入口。
- [贡献与 Agent 规则](../../AGENTS.md)：分支、核心登记、产品不变量和公开仓库边界。
- [开发指南](../../DEV_GUIDE.md)：工具链、目录职责和验证命令。
- [变更记录](../../CHANGELOG.md)：各版本面向用户的变化。
- [原版基线](../../.upstream-sub2api)：固定的原版版本与提交。

## 本目录

- [ARCHITECTURE.md](ARCHITECTURE.md)：扩展架构的决策顺序、能力归属和升级门禁。
- [CORE-PATCHES.md](CORE-PATCHES.md)：原版核心文件补丁的分组说明；机器清单为 [CORE-PATCHES.json](CORE-PATCHES.json)。
- [DEPLOY.md](DEPLOY.md)：固定 A/B 槽位发布、唯一后台交接、自动守护与回滚。
- [EXTENSIONS.md](EXTENSIONS.md)：从旧私有分支迁移时的保留与退出范围。
- [MEDIA-ENGINE.md](MEDIA-ENGINE.md)：媒体引擎持久图片任务的状态机、资金边界与运行配置。
- [PARITY.md](PARITY.md)：保留能力的逐项验收场景。
- [PAYMENT-IMPLEMENTATION.md](PAYMENT-IMPLEMENTATION.md)：原生 BEpusdt 支付、唯一付款凭证与旧单恢复。
- [THEME-PREFERENCES.md](THEME-PREFERENCES.md)：本地主题色与深浅色偏好。
- [UPSTREAM-BALANCE.md](UPSTREAM-BALANCE.md)：用个人访问令牌查询上游账号余额。
- [UPSTREAM-DIFF.md](UPSTREAM-DIFF.md)：全仓与原版的路径对照方法。
- [WORKSPACE.md](WORKSPACE.md)：分支、工作区与文档分工。

## 扩展模块

- [extensions/sources-pricing-feed](../../extensions/sources-pricing-feed/README.md)：只读公开价格适配与持久报价快照。
- [extensions/sources-portal](../../extensions/sources-portal/README.md)：首页、余额、工作台授权与智商检测服务。
- [extensions/sources-media-engine](../../extensions/sources-media-engine/README.md)：媒体引擎（任务引擎）。
- [extensions/sources-monitor-local](../../extensions/sources-monitor-local/README.md)、[extensions/sources-monitor-design](../../extensions/sources-monitor-design/README.md)：渠道状态页的本地预览。
- [deploy/sources](../../deploy/sources/README.md)：发布工具用法。
