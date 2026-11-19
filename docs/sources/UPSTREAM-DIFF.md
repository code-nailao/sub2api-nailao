# 全仓与原版的路径对照

本仓库与原版的差异分两层检查：

1. **核心范围**（`backend/`、`frontend/`、`Dockerfile`）：差异集合必须与 [CORE-PATCHES.json](CORE-PATCHES.json) 完全相等，并逐个核对修改后文件的 sha256。由 `python3 scripts/sources_extensions/core_parity.py` 执行，CI 在每个 PR 上运行。
2. **全仓路径**：上游同步或审计时，对固定的原版提交（见 [.upstream-sub2api](../../.upstream-sub2api)）和本仓库的一个固定提交，各自用 `git ls-tree -rz` 列出全部路径，取并集逐路径比较新增、修改、删除及文件模式，再按目录职责归类。

全仓对照的归类：

| 类别 | 内容 |
| --- | --- |
| core/build | 核心范围，必须全部登记在 CORE-PATCHES.json |
| extension | `extensions/` 下的独立模块 |
| deployment | `deploy/` 下的部署配置与工具 |
| validation/tooling | `scripts/` 下的检查、构建与发布工具 |
| ci | `.github/` 下的工作流 |
| documentation | 文档 |
| repository/config | 仓库级配置文件 |

注意：

- 路径数包含测试、生成代码和文档，**不等于功能数**，也不代表运行验收结果。
- 对照必须基于固定的提交，未忽略任何新增或删除路径。
- 某次同步的逐路径清单是那一次的审查记录，不随后续提交改写；现行差异以当前代码和 CORE-PATCHES.json 为准。

能力层面的差异说明见 [SOURCES](../../SOURCES.md)。
