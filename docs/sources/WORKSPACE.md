# 分支、工作区与文档规范

`main` 始终可交付；每项开发使用按类型命名的短期分支，完成后合并并删除。

## 开始与同步

1. 先确认当前目录、分支和未提交内容，避免在别人的工作上继续。
2. 同步 `main`：`git fetch origin` 后 `git merge --ff-only origin/main`。有分歧或本地修改时先弄清归属并保存，不强制覆盖。
3. 从最新 `origin/main` 创建短期分支，例如 `fix/deepseek-stream-timing`；不要在旧功能分支上顺手继续新任务。
4. 需要并行处理多项任务时，每项任务使用独立的 worktree：`git worktree add -b fix/<主题> <目录> origin/main`。主工作目录只用于阅读和快进同步。

## 分支命名

采用轻量主干开发：`main` 可交付，短期分支经审查和验证后合并。不引入常驻 `develop`、多层合并或没有用途的 release 分支。

| 前缀 | 用途 | 示例 |
| --- | --- | --- |
| `feature/` | 新能力 | `feature/wallet-alerts` |
| `fix/` | 已有行为的缺陷修复 | `fix/deepseek-stream-timing` |
| `perf/` | 不改变功能语义的性能优化 | `perf/usage-query` |
| `refactor/` | 不改变对外行为的结构调整 | `refactor/tool-executor` |
| `docs/` | 纯文档 | `docs/development-guide` |
| `chore/` | 工具、依赖、工程维护 | `chore/update-lint-rules` |

- 格式为 `<type>/<可选 issue 号->英文短主题>`，小写、连字符；不放人名、密钥、日期或版本号。有真实 issue 时才写编号。
- 统一使用 `feature/`，不同时使用 `feat/`。提交信息仍用 Conventional Commit 的 `feat:`、`fix:` 等，分支前缀和提交类型不必逐字相同。
- 修复中顺带的性能改善仍用 `fix/`。紧急修复也用 `fix/`，紧急程度由发布流程表达，不设常驻 `hotfix` 分支。
- 不建立长期 `dev` 或 `develop` 集成分支。测试随所属改动提交；独立的测试基础设施用 `chore/`。
- 已有的历史分支名不为统一而批量改名。

## 开发、提交与合并

- 一个分支处理一个明确目标。临时 SQL、私有配置、下载文件、缓存和原始日志不放进仓库，也不要整包 `git add`。
- 新增、改变或退出一项二开能力时更新 [SOURCES](../../SOURCES.md)；核心文件变化更新 [CORE-PATCHES.json](CORE-PATCHES.json)。
- 提交信息使用简短英文 `type: summary`。纯文档提交可以加 `[skip ci]`；功能、计费或运行逻辑的变更不能靠跳过 CI 合并。
- 合并只带当前任务的改动。旧分支先与最新 `main` 比较，不整文件覆盖较新的实现。
- `main` 受保护时通过 PR 合并，不强推绕过。已提交、已推送、已合并、已发布是不同状态。

## 收尾与清理

1. 确认改动已合并或明确取消；未完成的事项记录到 issue 或对应设计文档。
2. 删除分支前核对它是否还有独有提交；squash 合并后不能只靠祖先关系判断。删除远端分支时用期望的提交 SHA 约束，避免删掉别人的新提交。
3. 工作树干净且不再需要时用 `git worktree remove` 删除。未提交或被忽略的文件要先单独保存：bundle 不包含未提交和被忽略的文件，patch 不包含完整的新文件。
4. 已发布的 tag 和镜像不改写、不覆盖。

## 文档分工

| 入口 | 职责 |
| --- | --- |
| `AGENTS.md` | 贡献与 Agent 的长期规则 |
| `DEV_GUIDE.md` | 工具链、目录结构、开发命令、验证方法 |
| `SOURCES.md` | 相对原版的能力差异及代码入口 |
| `CHANGELOG.md` | 各版本面向用户的变化 |
| `docs/sources/README.md` | 二开文档索引 |
| `docs/sources/*.md` | 单项能力的设计：为什么做、怎么做、怎么验证 |

仓库根目录不放 `PLAN.md`、`progress.md`、`task_plan.md`、`findings.md` 之类的临时状态文件。有价值的结论写进对应的设计文档。具体某次发布、事故或核对的执行记录属于运维资料，不放进本仓库。文档只链接仓库内实际存在的文件。
