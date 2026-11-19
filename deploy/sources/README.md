# Main 固定发布工具

本目录是一套固定的发布工具：在一台 Docker 主机上，用两个固定槽位轮流替换 Main serving 和唯一后台，Nginx 只引用固定的 upstream 名称。工具不随版本修改；如果某次发版需要改它们，先停下来走 PR，不要在服务器上现场修。

| 文件 | 作用 |
| --- | --- |
| `deploy.py` | 唯一的发布入口：`main <image>`、`rollback`、`status`、`watch`、`adopt` |
| `portal.py` | Portal 的发布逻辑，由 `deploy.py` 的 `portal <image>`、`portal-rollback`、`portal-status`、`portal-adopt` 调用（见下文“Portal”） |
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
| 预检 | Nginx 已用固定名称；目标槽位没有仍在排空的容器；宿主可用内存；公网健康地址返回 200（与守护同一个请求）；列出待执行迁移 | 退出，现场不变 |
| 迁移 | 用新镜像执行待执行迁移（带 `contract` 的需要 `--allow-contract`） | 退出；迁移只增不删，旧版本继续服务 |
| 启动检查 | `-check-startup`：用生产配置构造全部依赖 | 退出，没有创建任何容器 |
| 静态资源 | `-export-web-assets`：把新版本资源追加到固定目录（不删除、不覆盖） | 退出 |
| 启动新 serving | 目标槽位的主、备两个容器，等待就绪 | 删除新容器，退出 |
| 后台交接 | 旧后台收到一次 SIGTERM，处理完在途任务后退出（不发 SIGKILL）；新后台拿锁、就绪 | 3 分钟未就绪：重新启动旧后台，删除新 serving，退出 |
| 切流 | 重写 `00-sources-main-upstreams.conf`，`nginx -t`，reload 一次 | `nginx -t` 失败：恢复原文件，把后台交还旧版本 |
| 守护 | 5 分钟：新主实例就绪、无重启和 OOM、内存不超过上限 70% 且 30 秒内涨幅不超过 4GiB、公网健康、Nginx 连不上新端口的次数 | 流量切回旧槽位，后台交还旧版本，新 serving 停止并保留供排查 |
| 退役 | 旧 serving 重启策略改为 `no`，排空，发 SIGTERM，不等待 | — |

每次正式运行在 `history.jsonl` 追加一行，记录结果和各步骤耗时；失败的演练不写记录。`--dry-run` 是全局参数，写在子命令之前（`deploy.py --dry-run main <镜像>`），只打印会执行的变更命令；只读的步骤（列出迁移、启动检查）照常执行。有待执行迁移时演练不执行迁移，因此跳过启动检查并在输出中说明；两个环境的配置在正式运行中迁移之后、创建任何容器之前才校验。公网健康预检在演练中同样执行。

公网健康请求带固定的 `User-Agent`，因为公网入口前的部分边缘防护会拒绝 urllib 的默认标识。预检时公网健康不是 200 就停止，现场不变，错误里写明看到的状态码（0 表示没有响应）。只有在当前版本已经让公网健康失败、要向前发布修复时，才加 `--allow-public-health-failure`（`deploy.py main --allow-public-health-failure <镜像>`）：预检只打印警告，守护照常要求切流后返回 200。`rollback` 不做这项预检。

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

## Portal

Portal 用同一个入口发布：`deploy.py portal <镜像>`。逻辑在 `portal.py`，复用 `deploy.py` 的执行、锁、状态文件和 Nginx 安装步骤。设置文件默认是 `/opt/sources-deploy/portal.json`（全局 `--config` 显式给出时以它为准；Main 命令的默认值不变）。

### 为什么会有一小段不可用

Portal 只有一个容器。它的状态目录只允许一个写入者：进程启动时对状态目录加非阻塞文件锁，第二个 Portal 拿不到锁会立即以代码 1 退出；监听端口要等锁和全部存储初始化之后才打开。所以新进程只能在旧进程退出之后启动，**从旧 Portal 收到 SIGTERM 到 Nginx 切到新容器之间，Portal 的页面不可用**。单写入者状态下这段间隔无法避免，工具只能让它尽量短：

- 拉取镜像、预检、启动检查和创建新容器都在给旧 Portal 发信号之前完成；
- 旧进程一退出就启动已经创建好的新容器，每 0.5 秒检查一次退出和 `/health`；
- 新容器 `/health` 返回 200 后立即切换 Nginx。

旧 Portal 收到 SIGTERM 后立即停止接受连接，处理完在途请求后退出，通常只需几秒；极少数同步余额刷新最长约 375 秒，所以默认等待 420 秒（`exit_timeout`）。工具从不发送 SIGKILL。

### 每次发布

和 Main 一样用临时 systemd 单元运行：

```bash
ssh <host> 'sudo -n systemd-run --unit=sources-deploy-portal --collect /usr/bin/python3 /opt/sources-deploy/bin/deploy.py portal ghcr.io/<owner>/<portal-image>@sha256:<digest>'
```

```bash
ssh <host> 'sudo -n journalctl -fu sources-deploy-portal'
```

演练：`deploy.py --dry-run portal <镜像>`。它真实执行拉取、预检和启动检查（这些不改变任何东西），然后只打印创建、交接、切流和守护会执行的命令，不写状态，也不写历史。`--allow-public-health-failure` 与 `main` 含义相同：预检时公网健康失败只打印警告，守护仍要求切流后返回 200。

| 步骤 | 内容 | 失败时 |
| --- | --- | --- |
| 预检 | 已记录当前版本，其容器在运行且本机 `/health` 为 200；`nginx -T` 定义了 `upstream sources_portal` 并有 `proxy_pass http://sources_portal`；upstream 文件内容与工具为当前端口生成的完全一致；目标槽位容器没有在运行，目标端口空闲；env 文件存在且组和其他用户不可读；每个挂载源存在；公网健康返回 200。一次列出全部问题 | 退出，现场不变 |
| 启动检查 | 用新镜像、真实环境和挂载（全部强制只读）、不发布端口运行一次 `-check-config`，必须输出 `SOURCES_PORTAL_CONFIG_OK` | 退出，错误里带检查输出的那一行；旧 Portal 没被碰过。没有该参数的旧镜像会因拿不到锁而失败，同样在这里停下 |
| 创建 | 删除目标槽位上已停止的容器（上上个版本），创建新容器，重启策略 `no`，尚不启动 | 删除新容器，退出 |
| 交接（不可用开始） | 旧容器重启策略改为 `no`，发送一次 SIGTERM，等它退出；随后启动新容器，等 `/health` 返回 200（默认 60 秒，容器退出则立即失败） | 见下文“回退”。旧容器 420 秒内没有退出：不杀进程，恢复它的重启策略（它自行退出后 Docker 会重新启动它），不启动新容器、不改 Nginx，保留 `in_progress`，需要人工接手 |
| 切流（不可用结束） | 重写 upstream 文件，`nginx -t`，通过后 reload；然后新容器重启策略改为 `unless-stopped` | `nginx -t` 失败：恢复原文件，再回退 |
| 守护 | 默认 60 秒、每 5 秒：新容器在运行且 `StartedAt`、重启次数不变、没有被 OOM 终止；本机 `/health` 为 200；公网健康连续 3 次失败算失败；Nginx 错误日志中连接新端口失败（`connect() failed`、`upstream timed out`）累计 5 次算失败 | 回退 |

成功后 `state.json` 的 `previous` 是旧版本、`current` 是新版本，`history.jsonl` 追加一行 `action` 为 `portal` 的记录（演练不写）。

### 回退

交接开始后的任何失败都按顺序：新容器改为不自动重启，如果在运行就发一次 SIGTERM 并最多等 120 秒（它持有状态锁，旧容器在它退出前无法启动）；恢复旧容器原来的重启策略并启动，等待 `/health`；如果 upstream 文件已经切到新端口，再切回旧端口。完成后报错说明旧 Portal 已重新服务，新容器停止并保留供排查。任何一步失败则报告 `FAILED AND NOT RESTORED` 并保留 `in_progress`，下一次运行会被拒绝，需要人工处理。交接之前的失败从不碰旧容器。

### 回滚和查看状态

```bash
ssh <host> 'sudo -n systemd-run --unit=sources-deploy-portal --collect /usr/bin/python3 /opt/sources-deploy/bin/deploy.py portal-rollback'
```

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py portal-status'
```

`portal-rollback` 要求记录了上一版本，且它的容器和镜像都还在；不做启动检查（上一版本的镜像可能还没有 `-check-config`），用同样的交接、切流、守护和回退流程反向切换，成功后交换 `current` 和 `previous`。注意：新版本可能已经改写了状态目录里的数据，较旧的 Portal 有可能拒绝较新版本写下的状态；回滚失败时会自动回到当前版本。`portal-status` 不取锁，输出 `in_progress` 以及当前、上一版本的容器状态、健康状态和重启次数。

运行没有走完时（`portal-status` 里 `in_progress` 不为空），下一次 `portal` 或 `portal-rollback` 会拒绝执行。先用 `portal-status`、`docker ps -a` 和 `docker logs` 看清哪个 Portal 在运行、Nginx upstream 文件指向哪个端口，恢复服务后再把 Portal 的 `state.json` 中 `in_progress` 改为 `null`。创建步骤会先删除目标槽位上已停止的容器，所以重新运行是安全的。

### 首次接入

1. **记录现状**（只写文件，不动运行中的容器和 Nginx）：

   ```bash
   sudo -n /opt/sources-deploy/bin/deploy.py portal-adopt --container <运行中的 Portal 容器> \
     --slot-ports <A 端口>,<B 端口> --upstream-file /etc/nginx/conf.d/<portal-upstream>.conf \
     --public-health-url https://example.com/health
   ```

   它从运行中的容器生成只有 root 可读的 `<work_dir>/portal.env`（不含 `PATH`、`HOSTNAME`、`HOME`、`PORTAL_LISTEN`，不打印任何值）和 `portal.json`（网络、用户、资源限制、安全选项、日志选项和绑定挂载取自该容器），并把它记为当前版本（槽位 `legacy`）。`--work-dir` 默认 `/opt/sources-deploy/portal`，`--name-prefix` 默认 `sources-portal`，`--nginx-bin`、`--nginx-error-log`、`--lock-file` 的默认值与 Main 的 `adopt` 相同。配置或状态文件已存在、容器不在运行、或没有在 127.0.0.1 上发布端口时拒绝执行。工具从不删除 `legacy` 容器，确认不再需要后手动删除。
2. **准备 Nginx**：按工具生成的格式为当前端口手写 upstream 文件，把 Portal 的 location 改为 `proxy_pass http://sources_portal`，`nginx -t` 后 reload。预检正是核对这两项。upstream 文件之后由工具重写，不要手工编辑。
3. **第一次发布**：`deploy.py portal <镜像>`，镜像必须支持 `-check-config`。

### 与 Main 共用发布锁

`portal.json` 的 `lock_file` 应与 `main.json` 相同（`portal-adopt` 默认就是同一个文件），这样 Portal 和 Main 的发布不会同时进行；一方运行时另一方会立即报告锁被占用并退出，现场不变。
