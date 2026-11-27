# Sub2API 开发指南

本指南在固定上游版本的[原版指南](https://github.com/Wei-Shaw/sub2api/blob/b0fa9f3e6eeab1968a53e1289b69daa4bb40ca2b/DEV_GUIDE.md)基础上，按本仓库实际依赖与流程整理。保留原生 Go / Ent / Vue / pnpm 开发方式。

## 开始前

读 [AGENTS](AGENTS.md) 和 [二开差异总表](SOURCES.md)。从最新 `main` 按类型创建短期分支（新功能 `feature/<主题>`、修复 `fix/<主题>` 等），规范见[工作区规范](docs/sources/WORKSPACE.md)。改动先有 issue，PR 正文关联它或写明为什么没有，写法见同一文档的“Issue 与 PR”。不要直接合并上游 `main`：上游同步是单独任务，需要更新固定基线、检查二开差异并做回归。

## 工具链以仓库文件为准

| 项目 | 来源与约定 |
| --- | --- |
| Go、模块依赖 | [backend/go.mod](backend/go.mod)（当前 go 1.27.4），同时核对 CI 与构建镜像使用的版本 |
| ORM / HTTP | Ent + Gin；改 schema 或 interface 时同步生成代码和测试替身 |
| 前端 | Vue 3 + TypeScript + Vite；以 [package.json](frontend/package.json) 和 `pnpm-lock.yaml` 为准，固定 pnpm 9.15.9，不混用 npm/yarn 锁文件 |
| PostgreSQL / Redis | 使用独立的本地或测试实例，不连接任何生产库、Redis 或凭据 |
| 原版基线 | [.upstream-sub2api](.upstream-sub2api)，不从旧分支或 README 推断版本 |

依赖升级时一起核对锁文件、Go 指令、构建镜像、CI 和安全扫描；不为通过测试静默换版本。数据库口令放在本地私有配置里，不写入文档或提交。

## 模块与修改位置

| 目录 | 职责 |
| --- | --- |
| `backend/cmd/server`、`backend/internal` | 进程入口、HTTP、服务和持久化；改动优先复用原版实现 |
| `backend/ent/schema`、`backend/ent`、`backend/migrations` | 数据结构、生成代码和迁移；不修改已有迁移的字节 |
| `frontend/src` | 原版页面、状态和 API 客户端；二开组件集中在 `features/sources/`，原版文件只保留小挂接 |
| `extensions/sources-brand` | 品牌色板与外观偏好 |
| `extensions/sources-portal` | 首页与编辑器、上游余额、工作台授权、智商检测等独立服务 |
| `extensions/sources-pricing-feed` | 只读公开模型广场的价格协议适配，独立构建和持久快照 |
| `extensions/sources-media-engine` | 媒体引擎（持久图片任务引擎） |
| `extensions/sources-monitor-*` | 渠道状态页的本地预览，不进入生产构建 |
| `scripts/sources_extensions` | 基线获取、核心差异和扩展边界检查，公开目录与 PR 正文检查 |
| `scripts/sources_release` | 固定源码构建、CI 结果核对、镜像发布 |
| `deploy/sources` | 固定槽位部署工具和迁移检查，见[发布与部署](docs/sources/DEPLOY.md) |
| `docs/sources` | 二开设计文档，见[索引](docs/sources/README.md) |

各扩展有自己的依赖和测试，前端统一命令不能代替它们的验证。

## 本地命令

在自己的分支和工作区运行。按改动范围选择检查。

```sh
# 前端：安装固定依赖、启动开发服务器
cd frontend
pnpm install --frozen-lockfile
pnpm dev

# 前端：类型、只检查 lint、一次性测试与构建
pnpm typecheck
pnpm lint:check
pnpm test:run
pnpm build
```

`pnpm lint` 会自动修复；只检查时用 `lint:check`。修改依赖要一并提交 `pnpm-lock.yaml`。交互或样式变化要在真实浏览器中检查桌面、手机和不同权限，编译成功不等于页面验收。

```sh
# 后端
cd backend
make test-unit
make test-integration
# 按影响范围选择包或用例，必要时加 -race
# go test -tags=unit ./internal/<package> -run '<TestName>'
# go test -race -tags=integration ./internal/<package> -run '<TestName>'
golangci-lint run ./...

# 修改生成源后按 backend/Makefile 重新生成并检查差异
make generate
```

CI 中的后端单元测试使用 `scripts/sources_validation/unit.sh`：它把一个测量整个进程堆内存的原版用例放到单独进程执行，其余用例照常运行，不跳过也不放宽任何用例。

集成测试先确认使用专用的数据库、Redis 和网络；真实支付、模型和 SMTP 出口默认不可达。给 interface 加方法后补全测试替身；Ent 生成物和 schema 一起提交，避免只交半套代码。

## 二开检查

```sh
python3 scripts/sources_extensions/fetch_baseline.py     # 拉取固定原版提交到本地引用（core_parity 需要）
python3 scripts/sources_extensions/core_parity.py        # 核心文件路径与哈希
python3 scripts/sources_extensions/check_boundaries.py   # 扩展不依赖 Sub 内部包、SQL 或管理员余额接口
python3 deploy/sources/migration_lint.py --base origin/main  # 新迁移只扩展结构
python3 -m unittest discover -s deploy/sources -p 'test_*.py'
python3 -m unittest discover -s scripts/sources_release -p 'test_*.py'
python3 -m unittest discover -s scripts/sources_extensions -p 'test_*.py'
PR_BODY="$(cat <正文文件>)" python3 scripts/sources_extensions/check_pr_issue.py  # 开 PR 前检查正文是否关联了 issue
```

- 对照 [SOURCES](SOURCES.md) 找到受影响的能力；原版已支持的能力优先沿用原实现。
- 核心改动审查后登记到 `docs/sources/CORE-PATCHES.json`，再运行 `core_parity.py`。它只核对路径和哈希，不代替业务测试。
- 模型映射、Key、钱包、分组或价格的变化，先弄清身份和配置语义；不要用删账号重建、恢复旧库或重放未知请求来“修复”。
- 运行时、资金或迁移相关的变化需要相应的真实验证（真实 PostgreSQL、并发、中断恢复）；失败、跳过和未验证的边界写在 PR 里。
- 提交前检查 `git diff --check`、暂存范围和敏感信息（见 AGENTS 的“公开仓库的边界”）。

## 发布

发布由维护者执行，不是开发步骤。合并到 `main` 不自动发版；版本号、CHANGELOG 和 tag 由发布负责人更新。打 tag 后 CI 构建并发布镜像，部署使用 `deploy/sources/deploy.py`，设计与步骤见[发布与部署](docs/sources/DEPLOY.md)。

CHANGELOG 用中文写，面向用户，参考原版 sub2api 的发布说明格式：每个版本一条短总述，条目按「新增功能」「优化改进」「Bug 修复」「破坏性变更」归类，没有的类别不写。写清楚用户能感知到什么变化，不写内部实现细节和纯工程改动。
