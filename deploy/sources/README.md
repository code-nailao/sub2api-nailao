# Main 固定发布工具

本目录是一套固定的发布工具：在一台 Docker 主机上，用两个固定槽位轮流替换 Main serving 和唯一后台，Nginx 只引用固定的 upstream 名称。工具不随版本修改；如果某次发版需要改它们，先停下来走 PR，不要在服务器上现场修。

| 文件 | 作用 |
| --- | --- |
| `deploy.py` | 唯一的发布入口：`main <image>`、`rollback`、`status`、`watch`、`adopt` |
| `migration_lint.py` | CI 检查：新迁移只能扩展结构，破坏性变更必须放进带 `contract` 的迁移 |
| `sources-main-watch.service` | 常驻监测（只写 journal，第一版不推送） |

以下命令中的 `<host>` 是部署主机，`ghcr.io/<owner>/<image>@sha256:<digest>` 是本次发布的镜像（始终用 digest 固定）。

## 每次发版

用一个临时 systemd 单元运行，这样 SSH 断开也不会打断发版：

```bash
ssh <host> 'sudo -n systemd-run --unit=sources-deploy --collect /usr/bin/python3 /opt/sources-deploy/bin/deploy.py main ghcr.io/<owner>/<image>@sha256:<digest>'
```

```bash
ssh <host> 'sudo -n journalctl -fu sources-deploy'
```

整个过程无人值守，耗时主要是 5 分钟的自动守护（在一次演练中，守护之前的全部步骤约十几秒）。步骤和失败时的处理：

| 步骤 | 内容 | 失败时 |
| --- | --- | --- |
| 预检 | Nginx 已用固定名称；目标槽位没有仍在排空的容器；宿主可用内存；列出待执行迁移 | 退出，现场不变 |
| 迁移 | 用新镜像执行待执行迁移（带 `contract` 的需要 `--allow-contract`） | 退出；迁移只增不删，旧版本继续服务 |
| 启动检查 | `-check-startup`：用生产配置构造全部依赖 | 退出，没有创建任何容器 |
| 静态资源 | `-export-web-assets`：把新版本资源追加到固定目录（不删除、不覆盖） | 退出 |
| 启动新 serving | 目标槽位的主、备两个容器，等待就绪 | 删除新容器，退出 |
| 后台交接 | 旧后台收到一次 SIGTERM，处理完在途任务后退出（不发 SIGKILL）；新后台拿锁、就绪 | 3 分钟未就绪：重新启动旧后台，删除新 serving，退出 |
| 切流 | 重写 `00-sources-main-upstreams.conf`，`nginx -t`，reload 一次 | `nginx -t` 失败：恢复原文件，把后台交还旧版本 |
| 守护 | 5 分钟：新主实例就绪、无重启和 OOM、内存不超过上限 70% 且 30 秒内涨幅不超过 4GiB、公网健康、Nginx 连不上新端口的次数 | 流量切回旧槽位，后台交还旧版本，新 serving 停止并保留供排查 |
| 退役 | 旧 serving 重启策略改为 `no`，排空，发 SIGTERM，不等待 | — |

每次运行在 `history.jsonl` 追加一行，记录结果和各步骤耗时。`--dry-run` 是全局参数，写在子命令之前（`deploy.py --dry-run main <镜像>`），只打印会执行的变更命令；只读的步骤（列出迁移、启动检查）照常执行。有待执行迁移时演练不执行迁移，因此跳过启动检查并在输出中说明；两个环境的配置在正式运行中迁移之后、创建任何容器之前才校验。

两个槽位 A、B 轮流使用，容器名固定为 `sources-main-{a,b}-{serving,standby,background}`。发到某个槽位时，那里原有的容器（上上个版本）会被删除，所以始终只保留一个可回滚的上一版本。

## 回滚和查看状态

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py rollback'
```

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py status'
```

回滚会复用上一槽位已停止的容器（不重新创建），流程与发版相同：启动 → 后台交接 → 切流 → 守护 → 退役。

如果某次运行没有走完（`status` 里 `in_progress` 不为空），下一次 `main` 会拒绝执行。先用 `status` 和 `docker ps` 看清现场，处理完后再把 `state.json` 的 `in_progress` 清空。

## 首次接入

在一台已经运行着 Main（serving、备用 serving、后台各一个容器）的主机上接入本工具。每步都可单独回退。

1. **安装工具**：把本目录复制到 `/opt/sources-deploy/bin/`，只有 root 可写。
2. **记录现状**：

   ```bash
   sudo -n /opt/sources-deploy/bin/deploy.py adopt --serving <主容器> --standby <备容器> --background <后台容器> \
     --public-health-url https://example.com/health
   ```

   它从正在运行的容器生成 `/opt/sources-deploy/main.json`、两个只有 root 可读的 env 文件和固定输入目录，并把这些容器记为当前版本（槽位 `legacy`）。不碰运行中的容器，不打印任何变量值。`--public-health-url` 必填，是守护阶段检查的公网健康地址，写入 `main.json` 的 `public_health_url`；以后要改，直接编辑该文件。可先加全局参数 `--dry-run`（`deploy.py --dry-run adopt ...`）查看会执行的命令。
3. **Nginx 改用固定名称**：所有反向代理改为 `proxy_pass http://sources_main_serving`，管理后台改为 `http://sources_main_admin`。这两个 upstream 定义在 `/etc/nginx/conf.d/00-sources-main-upstreams.conf`（由 `deploy.py` 在每次切流时重写，不要手工编辑；第一次可按当前容器端口手写一份同名 upstream）。静态资源指向固定目录 `/opt/sources-web/main/assets`。改完先 `nginx -t` 再 reload；预检会拒绝仍引用按版本命名 upstream 的配置。
4. **常驻监测**：安装并启用 `sources-main-watch.service`，停用此前按版本命名的监测服务。
5. **第一次正式发版**：`deploy.py main <image>`，它会把 `legacy` 槽位换到 A 槽位。旧容器在确认没有依赖后手动删除。

## 已知限制

- 手机推送第一版不做，监测结果只写 journal。

## 内部调用方使用稳定别名

`deploy.py` 创建的每个 serving 容器（主和备）都在它所在的每个网络上带一个固定别名 `sources-main-serving`（`main.json` 的 `serving_alias`），后台容器没有。旧版本容器退役停止后，会自动从这个名字的解析结果里消失，所以同一 Docker 网络内的调用方（例如 Portal）应使用 `http://sources-main-serving:8080`，不要写死某个容器的 IP 或名称，否则那个容器就无法退役。

注意：Docker 只能在创建容器时附加网络别名。`adopt` 记录的旧容器没有这个别名，只有经 `deploy.py` 发布后的容器才有；调用方应在第一次正式发版之后再切换到别名，并在确认旧地址不再有连接后再退役旧容器。
