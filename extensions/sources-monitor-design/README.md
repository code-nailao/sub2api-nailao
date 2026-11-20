# 渠道状态交互设计预览

独立 Vue 设计稿，复用 Sub 的 ProviderIcon 与固定前端依赖。它不替代真正的 `/monitor` 页面，也不进入主站生产构建。

```sh
cd frontend
pnpm install --frozen-lockfile
cd ..
node extensions/sources-monitor-design/preview.mjs
# http://127.0.0.1:4180/
```

构建检查：`node extensions/sources-monitor-design/preview.mjs --build`。

## 主站导航

在 `extensions/sources-monitor-design/.env.local` 中为 `VITE_MONITOR_PREVIEW_MAIN_ORIGIN` 填入主站源。地址需包含 `http://` 或 `https://`，可以带端口和末尾斜杠。路径、查询参数、片段和凭据会使导航保持禁用。该文件已被 Git 忽略。

Vite 在启动预览或静态构建时读取这项配置，两种模式共用同一份配置。修改后重启预览或重新构建。配置为空或无效时，导航入口保留并显示「未配置主站地址」。配置有效时，链接会在新标签页打开对应的主站页面。填入真实地址的构建产物仅供本机使用。

在仓库根目录运行导航回归测试：

```sh
pnpm --dir frontend exec vitest run --config ../extensions/sources-monitor-design/vite.config.mjs
```

## 可交互内容

- OpenAI、Anthropic、Grok、Gemini、国模、其他分类，搜索、只看需关注、时间范围和深浅色。
- 缓存率、可用率、首 Token P50、精确到三位小数的倍率、小绿条及点击详情。
- 大分类排序与分类内排序：拖动、上下移动、取消、保存及恢复默认。此设计稿只保存在浏览器本地；真正页面使用管理员配置持久化。
- 沿用 V1 的视口断点：小于 768px 一列，768px 起两列，1280px 起三列，1536px 起四列。

## 数据边界

默认“设计演示”的性能指标与历史条为合成数据，页面明确标注。可选 `.dev/channel-monitor-design/source-snapshot.json` 仅放本机允许展示的渠道信息；目录已被 Git 忽略。没有文件时使用演示名称回退。

快照只应包含名称、关联 ID、倍率、可用率、对话延迟和探测时刻/状态，不包含 Key、登录凭据、用户身份、余额或业务数据。V1 快照中缓存率和首 Token 保持缺失，不用合成数据补齐；预览不会刷新任何线上数据，也不会发出模型探测。

开发服务只绑定127.0.0.1，无生产API代理。不要把本机快照复制进公开构建目录。
