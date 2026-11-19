# 渠道状态本地交互验收

启动真实 Sub Vue 前端，连接仅限本机的合成 API。用于检查 V1/V2 切换、卡片、分类、管理员排序和普通用户只读行为。**不是真实数据，也不代替 Go 服务／数据库验证。**

```sh
# 先在 frontend 目录安装固定依赖：pnpm install --frozen-lockfile
node extensions/sources-monitor-local/server.mjs
```

打开 `http://127.0.0.1:4182/monitor`。底部控制台可切换 V1/V2 及管理员／普通用户身份。入口自动建立只在本地 API 有效的模拟登录。

- 4181：本机 API；4182：实际前端 Vite；都只绑定 `127.0.0.1`。
- 如存在 `.dev/channel-monitor-design/source-snapshot.json`，只读其中分组显示名称和倍率；指标、时间线、账户均是合成数据。没有文件时使用通用样例名称。
- 默认包含 28 个通用渠道（本机已有快照时为快照里的渠道数）及 8 个边界样例：无请求、无缓存输入、真实零命中、无首 Token、样本不足、Kimi、长标题和管理员专属渠道。
- 大分类按展示配置划分为 OpenAI、Anthropic、Grok、Gemini、国模、生图。默认生图集中在最后，也可切回原 OpenAI/Gemini 分类末尾；业务平台不变，不自动写入逐渠道分类覆写。`other` 配置契约保留供未知平台或用户手动归类使用，没有对应渠道时不显示。生图的首 Token 与缓存输入保持未知。
- 重启读取新增分组时保留本机已有排序，追加缺失分类与分组元数据；其他配置照常由 config GET/PUT 流程更新。
- `/groups/rates` 在追加样例组 91001/91002 特设 0 倍率和 0.038 倍率，便于验证零值和小数不会丢失；只读快照中的真实倍率保持原样。
- 普通用户的计数脱敏归零，但保留 `has_requests` / `has_cache_input`，与新版真实接口保持一致。
- 排序通过真实前端 config GET/PUT 流程保存到 `.dev/channel-monitor-local/state.json`，模拟配置版本冲突。普通用户访问 admin API 返回 403；V1 模式调用 V2 接口返回 403。
- 仅实现已列出的本地接口；未实现的路由返回明确错误，不向主站转发。开发响应 CSP 阻断外部数据连接、外部 iframe。
- 此扩展不改变生产入口、后端配置、发布文件或正常前端构建。底部控制台通过此启动器的 Vite 插件注入。

端口可用 `MONITOR_FIXTURE_API_PORT` / `MONITOR_FIXTURE_UI_PORT` 调整。Ctrl-C 同时停止两个本地服务。

管理员在“自定义排序 → 生图展示”可选择集中到生图分类（默认）或放回原平台末尾；每个渠道可手动指定分类或选择“跟随默认”。`image_grouping`随config保存、公共快照回读，重启本地预览保留设置。关注筛选只对管理员展示。
