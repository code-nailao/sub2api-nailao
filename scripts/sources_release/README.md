# Sources 干净发布脚本

本目录负责把主站（Main）与 Portal 打包成一次可复核的发布：先在 CI 中构建并封存制品，再在同一源码提交的原生 push CI 通过后发布固定版本镜像与 GitHub Release 资产。不会推送浮动标签、不会提升为 Latest、不会切流或部署。

仓库名与镜像仓库只从仓库根目录的 `.sources-identity.json` 读取（`scripts/sources_baseline.py` 的 `load_identity()`）；上游基线从 `.upstream-sub2api` 读取（`load_baseline()`）。发布工作流在开始时核对 `github.repository` 与身份文件一致，不一致直接失败。

## 脚本

| 文件 | 作用 |
| --- | --- |
| `build.sh` | 要求干净工作树；安装锁定依赖并构建前端与 Portal 前端，运行扩展模块测试、核心一致性与边界检查、本目录单元测试，编译 Main/Portal 二进制，生成 `core-source.tar.gz` 和记录全部文件哈希的 `build-manifest.json`。 |
| `verify_bundle.py` | 按 `build-manifest.json` 校验制品目录：源码提交、上游基线、必需文件、无符号链接、逐文件哈希。 |
| `build_context.py` | 生成内容确定的 Docker 构建上下文 tar（固定属主与时间戳）。 |
| `ci_gate.py` | 等待并确认同一仓库、同一提交最新一次 `push` 触发的 `backend-ci.yml` 已完成且成功，输出 CI 回执。PR 运行不算数。 |
| `publish.py` / `publish.sh` | 只接受成功的原生 push CI 回执；预检所有标签后，用 buildx 推送 `v<版本>` 固定标签并回读注册表摘要。已存在但无原始回执的标签一律拒绝覆盖；部分发布只能用原始回执续传。 |
| `release_assets.py` | 创建或核对非 Latest 的 GitHub Release，只补传缺失资产（`build-manifest.json`、`release-images.json`、`core-source.tar.gz`），已有资产不同则停止，上传后全部回读校验。 |
| `audit_gate.py` | 区分“依赖审计完成”与“审计因网络/仓库错误未完成”，后者不视为通过。 |
| `test_*.py` | 以上脚本的单元测试，不访问网络或注册表。 |

发布范围（仅 Main 或 Main + Portal）由 `.github/release-tools/clean_components.py` 按版本选择，它包装本目录的 `publish.py` 与 `release_assets.py`。

## 顺序

1. **构建**：`scripts/sources_release/build.sh <新输出目录>`，产出制品并上传为 CI artifact。
2. **校验**：`python3 scripts/sources_release/verify_bundle.py <制品目录> --sha <提交>`，发布前必须再次通过。
3. **CI 门禁**：`python3 scripts/sources_release/ci_gate.py --sha <提交> --output ci-receipt.json`。
4. **发布镜像**：`clean_components.py images ...`（内部调用 `publish.py`），生成 `published-images.json`。
5. **发布资产**：`clean_components.py assets ...`（内部调用 `release_assets.py`）。

以上步骤由 `.github/workflows/release.yml` 在推送 `v*` 标签或手动触发时执行；手动触发默认是 dry run，只构建与校验。

本地只需运行测试：

```sh
python3 -m unittest discover -s scripts/sources_release -p 'test_*.py'
```
