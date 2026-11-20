# Main 固定发布工具

本目录是一套固定的发布工具：在一台 Docker 主机上，用两个固定槽位轮流替换 Main serving 和唯一后台，Nginx 只引用固定的 upstream 名称。工具不随版本修改；如果某次发版需要改它们，先停下来走 PR，不要在服务器上现场修。

| 文件 | 作用 |
| --- | --- |
| `deploy.py` | 唯一的发布入口：`main <image>`、`resume`、`abort`、`rollback`、`status`、`watch`、`adopt` |
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
| 后台交接 | 旧后台收到一次 SIGTERM，处理完在途任务后退出（不发 SIGKILL），排空期间管理接口只读；新后台拿锁、就绪。日志写明管理接口在排空期间是只读还是不可用，以及旧后台还在停止哪些服务 | 未就绪：重新启动旧后台，删除新 serving，退出。旧后台在 `background_exit_timeout` 内没有退出：`FAILED AND NOT RESTORED`，保留 `in_progress`（见下文） |
| 切流 | 重写 `00-sources-main-upstreams.conf`，`nginx -t`，reload 一次 | `nginx -t` 失败：恢复原文件，把后台交还旧版本 |
| 守护 | 5 分钟：新主实例就绪、无重启和 OOM、内存不超过上限 70% 且 30 秒内涨幅不超过 4GiB、公网健康、Nginx 连不上新端口的次数 | 先确认旧后台健康并持锁，再切回旧槽位。退役的 serving 排空后保留供排查 |
| 退役 | 旧 serving 重启策略改为 `no`，排空，发 SIGTERM，不等待 | — |

Main 的 `history.jsonl` 记录发布结果和各步骤耗时。追加失败时只告警一次，已保存的状态和命令的退出结果仍有效。`--dry-run` 是全局参数，写在子命令之前（`deploy.py --dry-run main <镜像>`）。演练仍按需拉取镜像，实际运行用于列出迁移或检查启动的一次性容器，不写历史。有待执行迁移时跳过启动检查，正式发布会在迁移完成后检查两个环境。公网健康预检在演练中同样执行。

公网健康请求带固定的 `User-Agent`，因为公网入口前的部分边缘防护会拒绝 urllib 的默认标识。预检时公网健康不是 200 就停止，现场不变，错误里写明看到的状态码（0 表示没有响应）。只有在当前版本已经让公网健康失败、要向前发布修复时，才加 `--allow-public-health-failure`（`deploy.py main --allow-public-health-failure <镜像>`）：预检只打印警告，守护照常要求切流后返回 200。`rollback` 不做这项预检。

两个槽位 A、B 轮流使用，容器名固定为 `sources-main-{a,b}-{serving,standby,background}`。发到某个槽位时，那里原有的容器（上上个版本）会被删除，所以始终只保留一个可回滚的上一版本。

## 回滚和查看状态

需要先验证新版本时，使用 `deploy.py main <IMAGE_DIGEST> --hold-before-switch`。工具在新 serving 就绪后成功退出，旧版本继续服务；`deploy.py resume` 验证暂停仍成立后继续交接切流，`deploy.py abort` 优雅停止并移除本次暂停实例。三个入口均支持前置全局参数 `--dry-run`。暂停期间拒绝其它 Main 发布与回滚；不要手工清除暂停来绕过。状态、直连验证范围和失败恢复见 [DEPLOY.md 的“暂停发布”](../../docs/sources/DEPLOY.md#暂停发布)。

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py rollback'
```

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py status'
```

回滚先确认上一槽位的后台容器仍在，隔离并启动主备，随后交接后台、切流、守护和退役。准备失败保留当前路由并告警。准备暂停的目标槽位也会删除槽位里原来的容器。即使之后成功 `abort`，先前的回滚目标也可能已经不存在。

查看 `status` 的 `in_progress.phase` 区分暂停、正在发布和故障。若为 `held`，用 `resume` 或 `abort`。若为 `aborting`，重试 `abort`。若失败步骤为 `prepare held slot`，可用 `resume` 重试准备，或用 `abort` 放弃。首次保存暂停记录失败时，`save held release` 可用 `resume` 或 `abort`。其它 `failed` 可运行 `abort`。工具核对实际路由、恢复原槽位，再清理本次候选。确认或清理未完成时可重试 `abort`。失败位置和原因分别记录在 `failed_step` 与 `reason`。容器的 `restart_policy` 为 `no` 时，退出后不会自动重启。通过日志和运行时状态确认它是否已收到 SIGTERM。

后台排空超过 `background_exit_timeout` 时，接手的后台尚未启动。自动回退和手动回滚都要求目标后台返回 `/health` 200 并确认持锁后才切换管理 upstream。若原管理后台仍能应答，工具只回退用户流量，记录 `ROLLBACK INCOMPLETE`。工具确认实际用户路由已切换、目标槽位能接住共享别名流量后，才排空原主备并撤下它们的共享别名。探测未确认或目标缺少共享别名时，保留两组 serving 并告警。两边都无法确认可用时保留已加载的路由，记录 `FAILED AND NOT RESTORED`。原后台可能仍在排空并已关闭自动重启。先用 `status` 和实际路由确认现场，等它退出后再启动选定的后台。确认后台健康并持锁后才更新路由和恢复记录。详见 [DEPLOY.md](../../docs/sources/DEPLOY.md#无法自动恢复的情况)。

## 首次接入

在一台已经运行着 Main（serving、备用 serving、后台各一个容器）的主机上接入本工具。每步都可单独回退。

1. **安装工具**：把本目录复制到 `/opt/sources-deploy/bin/`，只有 root 可写。
2. **记录现状**：

   ```bash
   sudo -n /opt/sources-deploy/bin/deploy.py adopt --serving <主容器> --standby <备容器> --background <后台容器> \
     --public-health-url https://example.com/health
   ```

   它从正在运行的容器生成 `/opt/sources-deploy/main.json`、两个只有 root 可读的 env 文件和固定输入目录，并把这些容器记为当前版本（槽位 `legacy`）。不碰运行中的容器，不打印任何变量值。`adopt` 使用 Main 发布锁，遇到暂停或其它未结束的发布时拒绝写入。`--public-health-url` 必填，是守护阶段检查的公网健康地址，写入 `main.json` 的 `public_health_url`；以后要改，直接编辑该文件。可先加全局参数 `--dry-run`（`deploy.py --dry-run adopt ...`）查看会执行的命令。
3. **Nginx 改用固定名称**：所有反向代理改为 `proxy_pass http://sources_main_serving`，管理后台改为 `http://sources_main_admin`。这两个 upstream 定义在 `/etc/nginx/conf.d/00-sources-main-upstreams.conf`（由 `deploy.py` 在每次切流时重写，不要手工编辑；第一次可按当前容器端口手写一份同名 upstream）。静态资源指向固定目录 `/opt/sources-web/main/assets`。改完先 `nginx -t` 再 reload；预检会拒绝仍引用按版本命名 upstream 的配置。
4. **常驻监测**：安装并启用 `sources-main-watch.service`，停用此前按版本命名的监测服务。
5. **第一次正式发版**：`deploy.py main <image>`，它会把 `legacy` 槽位换到 A 槽位。旧容器在确认没有依赖后手动删除。

## 已知限制

- 手机推送第一版不做，监测结果只写 journal。

## 内部调用方使用稳定别名

同一 Docker 网络内的调用方通过 `main.json` 中的 `serving_alias` 访问主备 serving。普通发布在创建和接网时配置别名，新旧槽位从新主备启动到旧主备排空退出可以同时提供别名。暂停实例在继续之前保持隔离。继续发布和手动回滚在交接时保持单一归属。确认新路由生效和目标网络可接手后，先排空旧主备，再撤下旧别名并交给新槽位。回滚目标缺少别名时保留当前提供方。`watch` 根据发布记录和退役实例身份区分普通发布的预期重叠与别名冲突。

`adopt` 记录的旧容器可能已有其它网络别名。首次正式发布后，确认新版本已提供配置中的共享别名，再将内部调用方切过去。

## Portal

Portal 用同一个入口发布：`deploy.py portal <镜像>`。逻辑在 `portal.py`，复用 `deploy.py` 的执行、锁、状态文件和 Nginx 安装步骤。设置文件默认是 `/opt/sources-deploy/portal.json`（全局 `--config` 显式给出时以它为准；Main 命令的默认值不变）。

### 为什么会有一小段不可用

Portal 只有一个容器。它的状态目录只允许一个写入者：进程启动时对状态目录加非阻塞文件锁，第二个 Portal 拿不到锁会立即以代码 1 退出；监听端口要等锁和全部存储初始化之后才打开。所以新进程只能在旧进程退出之后启动，**从旧 Portal 收到 SIGTERM 到 Nginx 切到新容器之间，Portal 的页面不可用**。单写入者状态下这段间隔无法避免，工具只能让它尽量短：

- 拉取镜像、预检、启动检查和创建新容器都在给旧 Portal 发信号之前完成；
- 旧进程一退出就启动已经创建好的新容器，每 0.25 秒检查一次退出和 `/health`；
- 新容器 `/health` 返回 200 后立即切换 Nginx。

旧 Portal 收到 SIGTERM 后立即关闭监听、停止接受连接，并取消只读余额查询；同步刷新不再按上游查询的长时限等待，退出取消不会改写余额记录、检查计划或告警状态，也不记为上游失败。已接受的其他请求及检测 worker 已开始的提交或轮询仍完成并保存后才释放状态目录锁。Portal 的关闭日志显示在途请求、后台等待、检测当前轮次和排空耗时，等待期间会定时更新。此行为需要包含该退出逻辑的 Portal 版本，旧版本仍可能等待余额查询的原有长时限，因此工具的等待配置不变。工具从不发送 SIGKILL。Docker 也不会重新启动一个被 `docker kill --signal=TERM` 停下的容器（即使重启策略是 `unless-stopped`），所以信号发出后，恢复服务的唯一办法就是等它退出、再启动一个容器。两个超时：

- `exit_timeout`（默认 420 秒，长于预期最长的排空时间）：超过后工具每 30 秒在日志里打印一条 `WARNING`，说明距信号已过多久、已超过预期最长排空时间、Portal 页面不可用、工具继续等待并会在旧进程退出后立即启动新容器、不会杀任何进程。旧进程在这之后退出时，发布照常继续完成。
- `exit_abort_timeout`（默认 3600 秒，不得小于 `exit_timeout`）：到这时旧进程仍未退出，工具才停止，报告 `FAILED, NEEDS AN OPERATOR`，保留 `in_progress`。此时没有杀任何进程；新容器已创建但从未启动；Nginx 没有改动；Portal 一直不可用，直到旧进程自行退出，Docker 不会重启它。运维人员应等它退出（`docker ps -a` 看状态，`docker logs` 看它在处理什么），然后 `docker start <旧容器>`（原来的上一版本，未改动），确认 `portal-status` 正常后清除 `in_progress`，再重新发布。

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
| 交接（不可用开始） | 旧容器重启策略改为 `no`；如果它在运行，发送一次 SIGTERM 并等它退出（它已不在运行时直接跳过，例如当前 Portal 已经崩溃）；随后启动新容器，等 `/health` 返回 200（默认 60 秒，容器退出则立即失败） | 见下文“回退”。旧容器超过 `exit_timeout` 仍未退出：每 30 秒警告并继续等待；超过 `exit_abort_timeout`：见上文，需要人工接手 |
| 切流（不可用结束） | 重写 upstream 文件，`nginx -t`，通过后 reload；然后新容器重启策略改为 `unless-stopped`，在 `ready_timeout` 内等待公网健康返回 200 后再进入守护；等待期间检查新容器没有停止、重启或被 OOM 终止 | `nginx -t` 失败：恢复原文件，再回退；等待公网健康超时或容器失败：回退 |
| 守护 | 默认 60 秒、每 5 秒：新容器在运行且 `StartedAt`、重启次数不变、没有被 OOM 终止；本机 `/health` 为 200；公网健康连续 3 次失败算失败；Nginx 错误日志中连接新端口失败（`connect() failed`、`upstream timed out`）累计 5 次算失败 | 回退 |

成功后 `state.json` 的 `previous` 是旧版本、`current` 是新版本，`history.jsonl` 追加一行 `action` 为 `portal` 的记录（演练不写）。

reload 返回时旧 worker 可能仍在接受连接，所以切流阶段先等到公网健康返回 200，再开始完整的守护时长和连续失败计数。成功响应后还会按等待开始时的 `StartedAt` 和重启次数复核容器，并检查它仍在运行、未处于重启中且没有被 OOM 终止；即使探测返回 200，请求期间发生的重启也会触发回退。等待复用 `ready_timeout`，超时沿用回退流程；这段等待不把旧 worker 引起的短暂失败记入守护。进入守护后，公网健康连续失败的判定不变。

### 回退

交接开始后的任何失败都按顺序回退。命令超时或无法执行、以及在交接到守护之间按下 Ctrl-C（中断）也算失败：工具先回退，然后仍以错误结束本次运行（工具不安装信号处理器，回退过程中再次中断会直接终止）。回退顺序：新容器改为不自动重启，如果在运行就发一次 SIGTERM 并最多等 120 秒（它持有状态锁，旧容器在它退出前无法启动）；如果旧容器已收到信号但仍在排空（在运行但 `/health` 不返回 200），先等它退出（同样的两个超时），若它仍返回 200（信号没有送达）则不等待；然后恢复旧容器原来的重启策略并启动，等待 `/health`；如果 upstream 文件已经切到新端口，再切回旧端口。完成后报错说明旧 Portal 已重新服务，新容器停止并保留供排查。任何一步失败则报告 `FAILED AND NOT RESTORED` 并保留 `in_progress`，下一次运行会被拒绝，需要人工处理。交接之前的失败从不碰旧容器。

### 回滚和查看状态

```bash
ssh <host> 'sudo -n systemd-run --unit=sources-deploy-portal --collect /usr/bin/python3 /opt/sources-deploy/bin/deploy.py portal-rollback'
```

```bash
ssh <host> 'sudo -n /opt/sources-deploy/bin/deploy.py portal-status'
```

`portal-rollback` 要求记录了上一版本，且它的容器和镜像都还在；不做启动检查（上一版本的镜像可能还没有 `-check-config`），用同样的交接、切流、守护和回退流程反向切换，成功后交换 `current` 和 `previous`。当前容器已经不在运行（例如崩溃）时回滚照样可用：工具不向它发信号，直接启动上一版本。注意：新版本可能已经改写了状态目录里的数据，较旧的 Portal 有可能拒绝较新版本写下的状态；回滚失败时会自动回到当前版本。`portal-status` 不取锁，输出 `in_progress` 以及当前、上一版本的容器状态和重启次数，只有运行中的容器显示 `health`。发布进行中，`current`、`previous` 的 `label` 分别标为 `current before this release`、`previous before this release`，说明它们是本次发布之前的记录；目标容器在独立的 `target` 项中展示实时状态，守护阶段正在服务的是这个目标。查看状态不改写发布记录。

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
