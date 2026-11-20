import contextlib
import hashlib
import http.client
import io
import json
import multiprocessing
import os
import re
import socketserver
import threading
import subprocess
import sys
import tempfile
import unittest
from email.message import Message
from pathlib import Path
from unittest import mock
from urllib.response import addinfourl

sys.path.insert(0, str(Path(__file__).resolve().parent))
import deploy  # noqa: E402

GIB = 1 << 30
SECRET = "super-secret-db-password"
FIXED_NGINX = ("upstream sources_main_serving {}\nupstream sources_main_admin {}\n"
               "proxy_pass http://sources_main_serving;\nproxy_pass http://sources_main_admin;\n")


class FakeOps(deploy.Ops):
    """In-memory Docker/Nginx/HTTP. Containers move through created ->
    running -> exited as the script issues commands."""

    def __init__(self, tmp: Path, dry_run: bool = False):
        super().__init__(dry_run=dry_run, log=self.logs_append)
        self.tmp = tmp
        self.logs: list[str] = []
        self.commands: list[list[str]] = []
        self.containers: dict[str, dict] = {}
        self.files: dict[str, str] = {}
        self.clock = 0.0
        self.pending = "001_add.sql"
        self.nginx_dump = FIXED_NGINX
        self.nginx_test_rc = 0
        self.check_fails = False
        # The real binary refuses to start against a database with pending
        # migrations until -migrate-only has applied them.
        self.migrated = False
        self.unready_ports: set[int] = set()
        self.memory: dict[str, int] = {}
        self.memory_after_switch: int | None = None
        self.error_log = ""
        self.switched = False
        self.loaded_upstreams = None
        self.upstream_file = None
        self.oneoffs: list[list[str]] = []
        self.local_images: set[str] = set()
        self.busy_ports: set[int] = set()
        self.network_ids = {name: f"fixture-network-{name}" for name in ("app-data", "app-upstream")}
        # Status of the public health URL; when set, the second value replaces
        # it at the next Nginx reload (i.e. once traffic switches).
        self.public_status = 200
        self.public_status_after_reload: int | None = None
        self.public_requests = 0
        # Seconds a container keeps running after TERM (inf: never exits), and
        # extra runtime fields per container (e.g. what a draining owner reports).
        self.drain_seconds: dict[str, float] = {}
        self.runtime_extra: dict[str, dict] = {}
        self.sleeps: list[float] = []
        self.files["/proc/sys/net/ipv4/ip_local_port_range"] = "10240\t65535\n"
        self.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = "9400-9404,9410-9414,9500\n"

    def logs_append(self, msg: str) -> None:
        self.logs.append(msg)

    def network_name(self, ref: str) -> str:
        return next((name for name, ident in self.network_ids.items() if ident == ref), ref)

    def add(self, name: str, port: int, running: bool = True, env=None, mounts=None, image="old@sha256:1"):
        self.containers[name] = {
            "Id": name + "-id", "Name": "/" + name, "RestartCount": 0,
            "Image": image,
            "State": {"Running": running, "Status": "running" if running else "exited", "ExitCode": 0,
                      "OOMKilled": False, "StartedAt": "t0"},
            "Config": {"Image": image, "User": "1001:1001", "Env": env or []},
            "HostConfig": {"Memory": 24 * GIB, "NanoCpus": 12_000_000_000, "PidsLimit": 4096,
                           "RestartPolicy": {"Name": "unless-stopped"},
                           "PortBindings": {"8080/tcp": [{"HostPort": str(port)}]},
                           "LogConfig": {"Config": {"max-size": "10m", "max-file": "3"}}},
            "NetworkSettings": {"Networks": {"app-data": {}, "app-upstream": {}}},
            "Mounts": mounts or [], "port": port, "role": None,
        }

    def run(self, args, *, mutate, timeout=600, check=True):
        if self.dry_run and mutate:
            self.commands.append(["DRY"] + list(args))
            return subprocess.CompletedProcess(args, 0, "", "")
        self.commands.append(list(args))
        # Docker accepts either the name or the immutable ID for container operations.
        name = next((n for n, c in self.containers.items() if c["Id"] == args[-1]), args[-1])
        out, rc = "", 0
        if args[:2] == ["docker", "create"]:
            name = args[args.index("--name") + 1]
            port = 0
            if "--publish" in args:
                port = int(args[args.index("--publish") + 1].split(":")[1])
            role = next((a.split("=", 1)[1] for a in args if a.startswith("SOURCES_PROCESS_ROLE=")), "serving")
            self.add(name, port, running=False, image=args[-1] if "oneoff" not in name else "x")
            out = self.containers[name]["Id"] + "\n"
            self.containers[name]["role"] = role
            self.containers[name]["Config"]["Labels"] = dict(
                args[i + 1].split("=", 1) for i, arg in enumerate(args) if arg == "--label"
            )
            network = self.network_name(args[args.index("--network") + 1])
            aliases = [args[i + 1] for i, arg in enumerate(args) if arg == "--network-alias"]
            self.containers[name]["NetworkSettings"]["Networks"] = {network: {"Aliases": aliases}}
            # One-off command flags use a single dash (-check-startup ...); docker's
            # own flags here are "--long" or "-e".
            start = next((i for i, a in enumerate(args)
                          if i > 2 and a.startswith("-") and not a.startswith("--") and a != "-e"
                          and not args[i - 1] == "-e"), len(args))
            self.containers[name]["cmd"] = args[start:] if "oneoff" in name else []
        elif args[:2] == ["docker", "start"]:
            c = self.containers[name]
            if "--attach" in args:
                cmd = c["cmd"]
                self.oneoffs.append(cmd)
                if cmd[0] == "-list-pending-migrations":
                    out = f"INFO log line\n{deploy.PENDING_PREFIX} {self.pending}\n"
                elif cmd[0] == "-migrate-only":
                    self.migrated = True
                elif cmd[0] == "-check-startup":
                    refuses = self.check_fails or (bool(self.pending) and not self.migrated)
                    out, rc = (("", 1) if refuses else (f"{deploy.CHECK_PREFIX} {{}}\n", 0))
                elif cmd[0] == "-export-web-assets":
                    out = f"{deploy.ASSETS_PREFIX} 3\n"
            else:
                c.pop("draining_until", None)
                c.pop("drain_marked", None)
                c["State"].update(Running=True, Status="running", StartedAt=f"t{self.clock}")
        elif args[:2] == ["docker", "kill"]:
            c = self.containers.get(name)
            if c and name in self.drain_seconds and c["State"]["Running"]:
                c.setdefault("draining_until", self.clock + self.drain_seconds[name])
            elif c:
                c["State"].update(Running=False, Status="exited", ExitCode=0)
        elif args[:2] == ["docker", "rm"]:
            self.containers.pop(name, None)
        elif args[:2] == ["docker", "update"]:
            c = self.containers.get(name)
            if c:
                policy, _, retries = args[2].split("=", 1)[1].partition(":")
                c["HostConfig"]["RestartPolicy"] = {"Name": policy, "MaximumRetryCount": int(retries or 0)}
        elif args[:3] == ["docker", "network", "disconnect"]:
            self.containers[name]["NetworkSettings"]["Networks"].pop(self.network_name(args[-2]), None)
        elif args[:3] == ["docker", "network", "connect"]:
            if name in self.containers:
                aliases = [args[i + 1] for i, arg in enumerate(args) if arg == "--alias"]
                endpoint = {"Aliases": aliases}
                ipam = {key: args[args.index(flag) + 1] for flag, key in
                        (("--ip", "IPv4Address"), ("--ip6", "IPv6Address")) if flag in args}
                link_local = [args[i + 1] for i, arg in enumerate(args) if arg == "--link-local-ip"]
                if link_local:
                    ipam["LinkLocalIPs"] = link_local
                if ipam:
                    endpoint["IPAMConfig"] = ipam
                self.containers[name]["NetworkSettings"]["Networks"][self.network_name(args[-2])] = endpoint
        elif args[:3] == ["docker", "network", "inspect"]:
            networks = []
            for ref in args[3:]:
                network = self.network_name(ref)
                if network not in self.network_ids:
                    rc = 1
                    break
                networks.append({"Name": network, "Id": self.network_ids[network]})
            out = json.dumps(networks)
        elif args[:3] == ["docker", "image", "inspect"]:
            rc = 0 if args[3] in self.local_images else 1
        elif args[1:] == ["-T"]:
            out = self.nginx_dump
        elif args[1:] == ["-t"]:
            rc = self.nginx_test_rc
        elif args[1:] == ["-s", "reload"]:
            previous = (self.loaded_upstreams or "").partition("\n# Local verification")[0]
            self.loaded_upstreams = self.files.get(self.upstream_file)
            changed = (self.loaded_upstreams or "").partition("\n# Local verification")[0] != previous
            self.switched = self.switched or changed
            if changed and self.public_status_after_reload is not None:
                self.public_status, self.public_status_after_reload = self.public_status_after_reload, None
        if check and rc != 0:
            raise deploy.DeployError(f"command failed: {args[:3]}")
        return subprocess.CompletedProcess(args, rc, out, "")

    def settle_drain(self, c):
        if c and "draining_until" in c and self.clock >= c["draining_until"]:
            c.pop("draining_until")
            c["State"].update(Running=False, Status="exited", ExitCode=0)

    def inspect(self, name):
        c = self.containers.get(name) or next((c for c in self.containers.values() if c["Id"] == name), None)
        self.settle_drain(c)
        return json.loads(json.dumps(c, default=str)) if c else None

    def by_port(self, port):
        return next((c for c in self.containers.values() if c["port"] == port), None)

    def http_json(self, url, token=None, method="GET", body=None, timeout=5):
        if url.startswith("https://"):
            self.public_requests += 1
            return self.public_status, ("ok" if self.public_status == 200 else None)
        port = int(url.split(":")[2].split("/")[0])
        c = self.by_port(port)
        self.settle_drain(c)
        if not c or not c["State"]["Running"]:
            return 0, None
        extra = self.runtime_extra.get(c["Name"][1:], {})
        if ("draining_until" in c or c.get("drain_marked")) and url.endswith("/internal/sources/runtime") and token == "tok":
            return 200, {"ready": False, "draining": True, "role": c["role"],
                         "owns_background": c["role"] == "background", "instance_id": c["Id"], **extra}
        if url.endswith("/health"):
            return 200, "ok"
        if url.endswith("/drain"):
            if method != "POST" or token != "tok" or body != {"instance_id": c["Id"]}:
                return 409, None
            c["drain_marked"] = True
            return 200, {}
        if token != "tok" or port in self.unready_ports:
            return (404, None) if token != "tok" else (200, {"ready": False, "role": c["role"]})
        return 200, {"ready": True, "role": c["role"], "owns_background": c["role"] == "background",
                     "instance_id": c["Id"], "active_http": c.get("active_http", 0),
                     **{k: v for k, v in extra.items() if k == "admin_during_drain"}}

    def read_text(self, path):
        if path in self.files:
            return self.files[path]
        if path.endswith("runtime.token"):
            return "tok\n"
        if path.endswith("memory.current"):
            cid = path.split("docker-")[1].split(".scope")[0]
            name = cid[: -len("-id")]
            if self.switched and self.memory_after_switch is not None and name.startswith("sources-main-a"):
                return str(self.memory_after_switch)
            return str(self.memory.get(name, GIB))
        raise FileNotFoundError(path)

    def read_bytes(self, path):
        if path in self.files:
            return self.files[path].encode()
        return super().read_bytes(path)

    def probe_nginx_serving(self, path, timeout):
        # Consult the configuration accepted by the workers, even when the
        # disk file already contains a different upstream.
        config = self.loaded_upstreams or ""
        revision = re.search(r"add_header X-Sources-Route ([0-9a-f]+) always;", config)
        upstream = re.search(r"upstream sources_main_serving \{[^}]*?\bserver (\S+) ", config)
        if not revision or not upstream:
            return 0, None, None
        address = upstream[1]
        status, _ = self.http_json(f"http://{address}/health", timeout=timeout)
        return status, revision[1], address

    def nginx_workers(self, path, *, shutting_down=False):
        return []

    def write_file_atomic(self, path, text, mode=0o644):
        if self.dry_run:
            self.commands.append(["DRY", "write", path])
            return
        self.files[path] = text

    def remove_file(self, path):
        if not self.dry_run:
            self.files.pop(path, None)

    def port_free(self, port):
        return port not in self.busy_ports

    def meminfo_available(self):
        return 40 * GIB

    def file_size(self, path):
        return 0

    def read_from(self, path, offset):
        return self.error_log

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.clock += seconds

    def monotonic(self):
        self.clock += 0.01
        return self.clock


def make_config(tmp: Path) -> deploy.Config:
    return deploy.Config(
        slots={"a": {"serving": 9401, "standby": 9402, "background": 9403},
               "b": {"serving": 9411, "standby": 9412, "background": 9413}},
        name_prefix="sources-main", networks=["app-data", "app-upstream"],
        user="1001:1001", memory="24g", cpus="12", pids_limit=4096, log_max_size="10m", log_max_file="3",
        env_files={"serving": "/opt/x/serving.env", "background": "/opt/x/background.env"},
        mounts={"common": [deploy.Mount("/state/main", "/app/data", readonly=False)],
                "serving": [deploy.Mount("/opt/x/inputs/serving/pricing-cache", "/runtime/pricing-cache",
                                         readonly=False, per_container_copy=True)],
                "background": []},
        runtime_token_file="/opt/x/runtime.token", upstream_file="/etc/nginx/conf.d/00-sources-main-upstreams.conf",
        nginx_bin="/usr/sbin/nginx", nginx_error_log="/var/log/nginx/error.log",
        assets_dir="/opt/sources-web/main/assets", work_dir=str(tmp / "work"),
        state_file=str(tmp / "state.json"), history_file=str(tmp / "history.jsonl"),
        lock_file=str(tmp / "release.lock"), public_health_url="https://example.com/health",
        cgroup_memory_path="/sys/fs/cgroup/system.slice/docker-{id}.scope/memory.current",
        guard_seconds=60, guard_interval=10,
    )


LEGACY = {"slot": "legacy", "image": "old@sha256:1", "deployed_at": None,
          "containers": {"serving": "old-serving", "standby": "old-spare", "background": "old-bg"},
          "ports": {"serving": 9426, "standby": 9428, "background": 9430},
          "runtime_token_files": {"serving": "/legacy/s/runtime.token", "standby": "/legacy/s/runtime.token",
                                  "background": "/legacy/bg/runtime.token"}}


class DeployTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.cfg = make_config(self.tmp)
        self.ops = FakeOps(self.tmp)
        for path in self.cfg.env_files.values():
            self.ops.files[path] = "FIXTURE=environment\n"
        self.ops.upstream_file = self.cfg.upstream_file
        for role, name in LEGACY["containers"].items():
            self.ops.add(name, LEGACY["ports"][role])
            self.ops.containers[name]["role"] = "background" if role == "background" else "serving"
        self.load_fixture_route(LEGACY)
        deploy.save_state(self.cfg, {"current": LEGACY, "previous": None, "in_progress": None})

    def tearDown(self):
        self._tmp.cleanup()

    def state(self):
        return deploy.load_state(self.cfg)

    def history(self):
        path = Path(self.cfg.history_file)
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def created(self):
        return [c[c.index("--name") + 1] for c in self.ops.commands
                if c[:2] == ["docker", "create"] and "oneoff" not in c[c.index("--name") + 1]]

    def deploy(self, image="new@sha256:2", **kw):
        deploy.Deployer(self.cfg, self.ops).deploy(image, **kw)

    def hold(self):
        self.deploy(hold_before_switch=True)
        return self.state()["in_progress"]

    def upstreams_for(self, view):
        return deploy.render_upstreams(*(view["ports"][role] for role in deploy.ROLES))

    def load_fixture_route(self, view, *, proof=True):
        text = self.upstreams_for(view)
        if proof:
            socket_name = hashlib.sha256(os.fsencode(self.cfg.upstream_file)).hexdigest()[:12] + ".sock"
            path = str(Path(self.cfg.state_file).absolute().with_name(socket_name))
            text += deploy.render_serving_probe(path, "0" * 32)
        self.ops.files[self.cfg.upstream_file] = self.ops.loaded_upstreams = text

    @staticmethod
    def upstreams_only(text):
        return text.partition("\n# Local verification")[0]

    def assert_live_release(self, view):
        upstreams = self.upstreams_for(view)
        self.assertEqual(upstreams, self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(self.ops.loaded_upstreams, self.ops.files[self.cfg.upstream_file])
        runtime = deploy.Deployer(self.cfg, self.ops).runtime
        token = self.ops.read_text(self.cfg.runtime_token_file).strip()
        roles = [(name, runtime(container['port'], token) or {})
                 for name, container in self.ops.containers.items() if container["role"] == "background"]
        self.assertEqual([view["containers"]["background"]], [n for n, r in roles if r.get("owns_background")])
        self.assertTrue(dict(roles)[view["containers"]["background"]]["ready"])

    def assert_release_failure(self, phase, reason=None):
        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertIsNotNone(progress)
        self.assertEqual(phase, progress["phase"])
        if reason is not None:
            self.assertIn(reason, progress["reason"])
        self.assertEqual(phase == "failed", any(
            "release_incomplete" in sample["alert"] for sample in deploy.watch_once(self.cfg, self.ops, {})))

    def assert_no_shared_serving(self, view, roles=None):
        for role in roles or deploy.SERVING_ROLES:
            info = self.ops.inspect(view["containers"][role]) or {}
            if info.get("State", {}).get("Running"):
                endpoints = info["NetworkSettings"]["Networks"]
                self.assertFalse(any(self.cfg.serving_alias in (ep.get("Aliases") or []) for ep in endpoints.values()),
                                 f"{role} is still reachable through the shared alias")

    def assert_loaded_interfaces(self, serving, admin, *, admin_healthy=True):
        route = deploy.render_upstreams(*(serving["ports"][role] for role in deploy.SERVING_ROLES),
                                        admin["ports"]["background"])
        self.assertEqual(route, self.upstreams_only(self.ops.loaded_upstreams))
        runner = deploy.Deployer(self.cfg, self.ops)
        for role in deploy.SERVING_ROLES:
            self.assertTrue(runner.health(serving["ports"][role]))
        self.assertEqual(admin_healthy, runner.background_answering(admin))
        self.assertEqual(admin_healthy, runner.health(admin["ports"]["background"]))

    def assert_resume_rejects_mount_edit(self, *, explicit):
        data = self.tmp / "mounted-data"
        data.mkdir()
        configured = data / ("selected-input" if explicit else "config.yaml")
        configured.write_text("fixture: before\n")
        (data / "mutable-data.db").write_text("before")
        self.cfg.mounts["background"] = [deploy.Mount(str(data), "/inputs", readonly=False)]
        # Exercise the effective Docker environment, including a CLI override.
        self.ops.files[self.cfg.env_files["background"]] = "CONFIG_FILE=/inputs/unused\n"
        self.cfg.extra_env["background"] = ({"CONFIG_FILE": "/inputs/selected-input"} if explicit
                                            else {"CONFIG_FILE": ""})
        extra = self.tmp / "another-input"
        extra.write_text("before")
        self.cfg.mounts["background"].append(deploy.Mount(str(extra), "/inputs/another-input"))
        held = self.hold()
        (configured if explicit else extra).write_text("fixture: after\n")
        self.ops.commands.clear()
        with self.assertRaisesRegex(deploy.DeployError, "abort and prepare a new hold"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual([], self.ops.commands)
        self.assert_no_shared_serving(held["target"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_release_failure("held")

    def test_resume_rejects_changed_config_file_under_directory_mount(self):
        self.assert_resume_rejects_mount_edit(explicit=True)

    def test_resume_rejects_changed_single_file_bind_mount(self):
        self.assert_resume_rejects_mount_edit(explicit=False)

    def test_resume_rechecks_mounted_config_after_alias_activation(self):
        config = self.tmp / "runtime.yaml"
        config.write_text("fixture: before\n")
        self.cfg.mounts["background"] = [deploy.Mount(str(config), "/inputs/runtime.yaml")]
        self.ops.files[self.cfg.env_files["background"]] = "CONFIG_FILE=/inputs/runtime.yaml\n"
        held = self.hold()
        run = self.ops.run

        def edit_after_start(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", held["containers"]["standby"]["id"]]:
                config.write_text("fixture: changed during activation\n")
            return result

        with mock.patch.object(self.ops, "run", edit_after_start):
            with self.assertRaisesRegex(deploy.DeployError, "abort and prepare a new hold"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(held["target"])
        self.assert_release_failure("held")

    def test_resume_isolation_timeout_still_drains_standby_and_alerts(self):
        held = self.hold()
        view = held["target"]
        run, sleep = self.ops.run, self.ops.sleep
        primary = view["containers"]["serving"]

        def busy_after_activation(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", held["containers"]["standby"]["id"]] and (
                    self.cfg.serving_alias in self.ops.inspect(args[-1])["NetworkSettings"]["Networks"]
                    [self.cfg.networks[0]]["Aliases"]):
                self.ops.busy_ports.add(view["ports"]["background"])
                self.ops.drain_seconds[view["containers"]["serving"]] = float("inf")
            return result

        def check_while_waiting(seconds):
            if "draining_until" in self.ops.containers[primary]:
                self.assert_no_shared_serving(view, roles=("standby",))
            sleep(seconds)

        with mock.patch.object(self.ops, "run", busy_after_activation), \
                mock.patch.object(self.ops, "sleep", check_while_waiting):
            with self.assertRaises(deploy.DeployError):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(view, roles=("standby",))
        self.assertTrue(self.ops.inspect(view["containers"]["serving"])["State"]["Running"])
        self.assert_release_failure("failed", "still draining")

    def test_resume_alerts_when_initial_serving_drain_never_completes(self):
        held = self.hold()
        view = held["target"]
        primary, standby = (view["containers"][role] for role in deploy.SERVING_ROLES)
        self.ops.drain_seconds[primary] = float("inf")
        with self.assertRaises(deploy.DeployError):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assertTrue(self.ops.inspect(primary)["State"]["Running"])
        self.assertFalse(self.ops.inspect(standby)["State"]["Running"])
        self.assert_no_shared_serving(view)
        self.assert_release_failure("failed", "still draining")
        for role in deploy.SERVING_ROLES:
            command = ["docker", "kill", "--signal=TERM", held["containers"][role]["id"]]
            self.assertEqual(1, self.ops.commands.count(command))

    def test_resume_rejects_changed_inherited_background_environment(self):
        self.ops.files[self.cfg.env_files["background"]] = "DATABASE_HOST\n"
        with mock.patch.dict(os.environ, {"DATABASE_HOST": "fixture-inherited-before"}):
            held = self.hold()
        self.ops.commands.clear()
        with mock.patch.dict(os.environ, {"DATABASE_HOST": "fixture-inherited-after"}):
            with self.assertRaisesRegex(deploy.DeployError, "abort and prepare a new hold"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual([], self.ops.commands)
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(held["target"])
        self.assert_release_failure("held")
        self.assertNotIn("fixture-inherited", Path(self.cfg.state_file).read_text())

    def test_resume_rejects_fifo_input_and_releases_lock(self):
        source = self.tmp / "mounted-input"
        source.mkdir()
        config = source / "runtime-input"
        config.write_text("fixture: before\n")
        self.cfg.mounts["background"] = [deploy.Mount(str(source), "/fixture-input")]
        self.ops.files[self.cfg.env_files["background"]] = "CONFIG_FILE=/fixture-input/runtime-input\n"
        held = self.hold()
        config.unlink()
        os.mkfifo(config)
        errors = []

        def resume():
            try:
                deploy.Deployer(self.cfg, self.ops).resume()
            except Exception as err:
                errors.append(err)

        thread = threading.Thread(target=resume, daemon=True)
        thread.start()
        thread.join(1)
        blocked = thread.is_alive()
        if blocked:
            # Release the baseline reader so the regression cannot leak a lock.
            with os.fdopen(os.open(config, os.O_RDWR | os.O_NONBLOCK), "wb") as fifo:
                fifo.write(b"fixture: recovery\n")
            thread.join(1)
        self.assertFalse(blocked, "a FIFO blocked resume while it held the release lock")
        self.assertEqual(1, len(errors))
        self.assertIsInstance(errors[0], deploy.DeployError)
        self.assertIn("regular file", str(errors[0]))
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(held, self.state()["in_progress"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)

    def test_resume_times_out_stalled_fingerprint_read_and_releases_lock(self):
        held = self.hold()
        read = self.ops.read_bytes
        unblock = threading.Event()

        def stall(path):
            if path == self.cfg.env_files["background"]:
                unblock.wait(0.5)
            return read(path)

        with mock.patch.object(self.ops, "read_bytes", stall), \
                mock.patch.object(deploy, "FINGERPRINT_TIMEOUT", 0.05, create=True):
            try:
                with self.assertRaisesRegex(deploy.DeployError, "configuration fingerprint.*timed out"):
                    deploy.Deployer(self.cfg, self.ops).resume()
                with deploy.release_lock(self.cfg.lock_file):
                    self.assertEqual(held, self.state()["in_progress"])
            finally:
                unblock.set()
        self.assert_loaded_interfaces(LEGACY, LEGACY)

    def assert_failure_before_reload(self, command, failure, *, restores_owner=True):
        if command == "rollback":
            self.deploy()
            self.deploy(image="another@sha256:3")
            target = self.state()["previous"]
        elif command == "resume":
            target = self.hold()["target"]
        else:
            target = self.cfg.slot_view("a")
        current = self.state()["current"]
        target_bg, current_bg = (view["containers"]["background"] for view in (target, current))
        run, read, save, log = self.ops.run, self.ops.read_bytes, deploy.save_state, self.ops.log
        injected = False

        def fail_save(cfg, state):
            nonlocal injected
            progress = state.get("in_progress") or {}
            if failure == "state" and progress.get("step") == "switch" and progress["phase"] == "deploying":
                self.assertTrue(self.ops.inspect(target_bg)["State"]["Running"])
                injected = True
                raise OSError("fixture switch state write failed")
            return save(cfg, state)

        def interrupt_read(path):
            nonlocal injected
            if (failure == "read" and not injected and path == self.cfg.upstream_file
                    and (self.ops.inspect(target_bg) or {}).get("State", {}).get("Running")):
                injected = True
                raise KeyboardInterrupt
            return read(path)

        def fail_restore(args, **kw):
            if not restores_owner and injected and args == ["docker", "start", current_bg]:
                raise deploy.DeployError("fixture original background cannot start")
            return run(args, **kw)

        def interrupt_switch_log(message):
            nonlocal injected
            if failure == "step" and message.endswith("== switch") and not injected:
                self.assertTrue(self.ops.inspect(target_bg)["State"]["Running"])
                self.assertFalse(self.ops.inspect(current_bg)["State"]["Running"])
                injected = True
                raise KeyboardInterrupt
            log(message)

        self.ops.commands.clear()
        with mock.patch.object(deploy, "save_state", fail_save), mock.patch.object(self.ops, "read_bytes", interrupt_read), \
                mock.patch.object(self.ops, "run", fail_restore), mock.patch.object(self.ops, "log", interrupt_switch_log):
            with self.assertRaises(deploy.DeployError) as outcome:
                if command == "main":
                    self.deploy()
                else:
                    getattr(deploy.Deployer(self.cfg, self.ops), command)()
        self.assertTrue(injected)
        self.assert_loaded_interfaces(current, current, admin_healthy=restores_owner)
        if restores_owner:
            self.assert_live_release(current)
        if restores_owner:
            self.assert_no_shared_serving(target)
        self.assertNotIn([self.cfg.nginx_bin, "-s", "reload"], self.ops.commands)
        if command == "main":
            self.assertIsNone(self.state()["in_progress"])
            self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
        else:
            self.assert_release_failure("failed", "state write" if failure == "state" else "KeyboardInterrupt")
        if not restores_owner:
            self.assertIn("docker start", str(outcome.exception))
            self.assertIn("ownership", str(outcome.exception))
            self.assertTrue(deploy.Deployer(self.cfg, self.ops).background_answering(target))
            self.assertIn("original background cannot start", deploy.status(self.cfg, self.ops)["in_progress"]["reason"])

    def test_resume_switch_state_write_failure_restores_original_background(self):
        self.assert_failure_before_reload("resume", "state")

    def test_resume_upstream_read_interrupt_restores_original_background(self):
        self.assert_failure_before_reload("resume", "read")

    def test_rollback_switch_state_write_failure_restores_original_background(self):
        self.assert_failure_before_reload("rollback", "state")

    def test_rollback_upstream_read_interrupt_restores_original_background(self):
        self.assert_failure_before_reload("rollback", "read")

    def test_resume_switch_step_interrupt_restores_loaded_admin(self):
        self.assert_failure_before_reload("resume", "step")

    def test_rollback_switch_step_interrupt_restores_loaded_admin(self):
        self.assert_failure_before_reload("rollback", "step")

    def test_main_switch_state_write_failure_restores_original_background(self):
        self.assert_failure_before_reload("main", "state")

    def test_main_upstream_read_interrupt_restores_original_background(self):
        self.assert_failure_before_reload("main", "read")

    def test_resume_reports_manual_recovery_if_original_background_cannot_restart(self):
        self.assert_failure_before_reload("resume", "read", restores_owner=False)

    def test_rollback_reports_manual_recovery_if_original_background_cannot_restart(self):
        self.assert_failure_before_reload("rollback", "read", restores_owner=False)

    def assert_rollback_preparation_failure(self, *, missing_background):
        self.deploy()
        self.deploy(image="another@sha256:3")
        before = self.state()
        previous = before["previous"]
        if missing_background:
            del self.ops.containers[previous["containers"]["background"]]
            reason = "rollback background container is missing"
        else:
            self.ops.unready_ports.add(previous["ports"]["standby"])
            reason = "not ready as serving"
        original_start = self.ops.run

        def verify_isolated_start(args, **kw):
            result = original_start(args, **kw)
            if args[:2] == ["docker", "start"]:
                self.assert_no_shared_serving(previous)
            return result

        with mock.patch.object(self.ops, "run", verify_isolated_start):
            with self.assertRaisesRegex(deploy.DeployError, reason):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assert_loaded_interfaces(before["current"], before["current"])
        self.assert_no_shared_serving(previous)
        self.assert_release_failure("failed", reason)

    def test_rollback_missing_background_keeps_current_routes_and_alerts(self):
        self.assert_rollback_preparation_failure(missing_background=True)

    def test_rollback_standby_timeout_keeps_current_routes_and_alerts(self):
        self.assert_rollback_preparation_failure(missing_background=False)

    def test_rollback_keeps_serving_if_a_degraded_reload_might_have_succeeded(self):
        self.deploy()
        self.deploy(image="another@sha256:3")
        before = self.state()
        current, previous = before["current"], before["previous"]
        run = self.ops.run

        def fail(args, **kw):
            if args == ["docker", "start", previous["containers"]["background"]]:
                raise deploy.DeployError("fixture rollback background would not start")
            result = run(args, **kw)
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                raise KeyboardInterrupt
            return result

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.FallbackFailed, "serving-only switch interrupted"):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assert_loaded_interfaces(previous, current)
        self.assert_no_shared_serving(previous)
        self.assert_release_failure("failed", "serving-only switch interrupted")

    def test_manual_rollback_keeps_alias_reachable_on_each_network_during_transfer(self):
        self.deploy()
        self.deploy(image="fixture:second")
        before = self.state()
        runner = deploy.Deployer(self.cfg, self.ops)
        command = self.ops.run
        coverage = set(runner.alias_networks(before["current"]))
        self.assertEqual(len(self.cfg.networks), len(coverage))

        def sample(args, **kwargs):
            result = command(args, **kwargs)
            alive = runner.alias_networks(before["current"]) | runner.alias_networks(before["previous"])
            self.assertTrue(coverage.issubset(alive), f"shared alias unavailable after {args[:3]}")
            return result

        with mock.patch.object(self.ops, "run", sample):
            runner.rollback()
        self.assertEqual(coverage, runner.alias_networks(before["previous"]))
        self.assertFalse(runner.alias_networks(before["current"]))
        self.assert_live_release(before["previous"])
        self.assertIsNone(self.state()["in_progress"])

    def test_degraded_rollback_waits_for_old_worker_before_signalling_serving(self):
        self.deploy()
        self.deploy(image="fixture:second")
        before = self.state()
        original, destination = before["current"], before["previous"]
        worker_id = {"pid": 43, "boot_id": "fixture-boot", "started_at_tick": "fixture-start"}
        calls = []
        run, sleep = self.ops.run, self.ops.sleep
        retire_name = original["containers"]["serving"]
        pid_stat = f"/proc/{worker_id['pid']}/stat"
        self.ops.files["/proc/sys/kernel/random/boot_id"] = worker_id["boot_id"]
        self.ops.files[pid_stat] = "43 (nginx) S 0 " + "0 " * 17 + worker_id["started_at_tick"]
        worker_exit_at = None

        def track_workers(path, *, shutting_down=False):
            return [worker_id] if pid_stat in self.ops.files else []

        def wait(seconds):
            sleep(seconds)
            if worker_exit_at is not None and self.ops.clock >= worker_exit_at:
                self.ops.files.pop(pid_stat, None)

        def run_with_old_request(args, **kwargs):
            nonlocal worker_exit_at
            if args == ["docker", "start", destination["containers"]["background"]]:
                raise deploy.DeployError("fixture rollback background fails")
            if args == [self.cfg.nginx_bin, "-s", "reload"] and worker_exit_at is None:
                worker_exit_at = self.ops.clock + 2.5
            if args == ["docker", "kill", "--signal=TERM", retire_name]:
                self.assertNotIn(pid_stat, self.ops.files, "old Nginx worker still handles a request body")
                calls.append(args)
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "nginx_workers", track_workers, create=True), \
                mock.patch.object(self.ops, "sleep", wait), mock.patch.object(self.ops, "run", run_with_old_request):
            with self.assertRaises(deploy.RollbackIncomplete):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertTrue(calls)
        self.assert_loaded_interfaces(destination, original)
        self.assert_release_failure("failed", "rollback background unavailable")

    def test_rollback_drains_each_serving_before_reconnecting_aliases(self):
        self.deploy()
        self.deploy(image="fixture:second")
        before = self.state()
        destination, retiring = before["previous"], before["current"]
        run = self.ops.run
        names = {view["containers"][role]: (view, role)
                 for view in (destination, retiring) for role in deploy.SERVING_ROLES}
        identities = {self.ops.inspect(name)["Id"]: name for name in names}
        detached = []
        for name in names:
            self.ops.drain_seconds[name] = 3

        def reconnect(args, **kwargs):
            if args[:3] == ["docker", "network", "disconnect"]:
                name = identities[args[-1]]
                view, role = names[name]
                info = self.ops.inspect(name)
                self.assertFalse(info["State"]["Running"], name)
                if view == destination and self.upstreams_only(self.ops.loaded_upstreams) != self.upstreams_for(retiring):
                    serving = self.upstreams_only(self.ops.loaded_upstreams).split("upstream sources_main_admin")[0]
                    port = view["ports"][role]
                    self.assertNotIn(f":{port} ", serving)
                    code, _, _ = self.ops.probe_nginx_serving("", 1)
                    self.assertEqual(200, code)
                    detached.append(role)
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "run", reconnect):
            deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(set(deploy.SERVING_ROLES), set(detached))
        self.assert_live_release(destination)
        self.assertIsNone(self.state()["in_progress"])

    def test_rollback_alias_publication_failure_keeps_the_verified_route(self):
        self.deploy()
        self.deploy(image="another@sha256:3")
        before = self.state()
        current, previous = before["current"], before["previous"]
        standby = self.ops.inspect(previous["containers"]["standby"])["Id"]
        run = self.ops.run
        failed = False

        def fail(args, **kw):
            nonlocal failed
            result = run(args, **kw)
            if (not failed and args[:3] == ["docker", "network", "connect"]
                    and self.cfg.serving_alias in args and args[-1] == standby):
                failed = True
                raise deploy.DeployError("fixture alias connect did not report success")
            return result

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.DeployError, "alias connect did not report success"):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertTrue(failed)
        checked = self.state()["in_progress"]["verified_route"]
        self.assertEqual("standby", checked["excluded_serving"])
        runner = deploy.Deployer(self.cfg, self.ops)
        self.assertEqual(runner.route_text(deploy.MainRoute(**checked)), self.ops.loaded_upstreams)
        self.assertTrue(runner.health(previous["ports"]["serving"]))
        self.assertTrue(runner.background_answering(previous))
        self.assertFalse(self.ops.inspect(standby)["State"]["Running"])
        self.assertTrue(runner.alias_networks(current))
        self.assertEqual(previous["slot"], self.state()["current"]["slot"])
        self.assert_release_failure("failed", "alias connect did not report success")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assert_loaded_interfaces(current, current)
        for role in deploy.SERVING_ROLES:
            endpoints = self.ops.inspect(current["containers"][role])["NetworkSettings"]["Networks"]
            self.assertTrue(all(self.cfg.serving_alias in ep.get("Aliases", ()) for ep in endpoints.values()))
        self.assert_no_shared_serving(previous)
        self.assertIsNone(self.state()["in_progress"])

    def test_rollback_publishes_aliases_after_verifying_routes_while_old_aliases_serve(self):
        self.deploy()
        self.deploy(image="another@sha256:3")
        previous = self.state()["previous"]
        run = self.ops.run
        runner = deploy.Deployer(self.cfg, self.ops)
        published = []
        identities = {self.ops.inspect(previous["containers"][role])["Id"]: role for role in deploy.SERVING_ROLES}

        def check(args, **kw):
            if args[:3] == ["docker", "network", "connect"] and self.cfg.serving_alias in args:
                cid = args[-1]
                stopped_role = identities[cid]
                self.assertFalse(self.ops.inspect(cid)["State"]["Running"])
                live_role = next(role for role in deploy.SERVING_ROLES if role != stopped_role)
                status, _, address = self.ops.probe_nginx_serving("", 1)
                self.assertEqual(200, status)
                self.assertTrue(address.endswith(f":{previous['ports'][live_role]}"))
                self.assertTrue(runner.background_answering(previous))
                self.assertTrue(runner.alias_networks(self.state()["previous"]))
                published.append(cid)
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", check):
            runner.rollback()
        self.assertEqual({self.ops.inspect(previous["containers"][r])["Id"] for r in deploy.SERVING_ROLES}, set(published))
        self.assert_loaded_interfaces(previous, previous)
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))

    def test_degraded_rollback_removes_shared_alias_from_unselected_slot_after_drain(self):
        self.deploy()
        self.deploy(image="another@sha256:3")
        state = self.state()
        current, previous = state["current"], state["previous"]
        current_names = [current["containers"][role] for role in deploy.SERVING_ROLES]
        current_ids = {self.ops.inspect(name)["Id"]: name for name in current_names}
        self.ops.drain_seconds.update(dict.fromkeys(current_names, 3))
        expected_route = self.upstreams_for({"ports": {**previous["ports"], "background": current["ports"]["background"]}})
        run = self.ops.run
        exited = False

        def exit_background_before_reload(args, **kw):
            nonlocal exited
            if args[-1] in current_ids or args[-1] in current_names:
                if args[:2] == ["docker", "kill"]:
                    self.assertEqual(expected_route, self.upstreams_only(self.ops.loaded_upstreams))
                if args[:3] == ["docker", "network", "disconnect"]:
                    self.assertFalse(self.ops.inspect(args[-1])["State"]["Running"])
            result = run(args, **kw)
            if (args[1:] == ["-t"] and not exited
                    and self.upstreams_only(self.ops.files[self.cfg.upstream_file]) == self.upstreams_for(previous)):
                self.ops.containers[previous["containers"]["background"]]["State"].update(Running=False, Status="exited")
                exited = True
            return result

        with mock.patch.object(self.ops, "run", exit_background_before_reload):
            with self.assertRaises(deploy.RollbackIncomplete):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertTrue(exited)
        self.assert_loaded_interfaces(previous, current)
        for name in current_names:
            info = self.ops.inspect(name)
            self.assertFalse(info["State"]["Running"], name)
            self.assertTrue(all(self.cfg.serving_alias not in endpoint.get("Aliases", [])
                                for endpoint in info["NetworkSettings"]["Networks"].values()))
        for role in deploy.SERVING_ROLES:
            self.assertTrue(all(self.cfg.serving_alias in endpoint.get("Aliases", []) for endpoint in
                                self.ops.inspect(previous["containers"][role])["NetworkSettings"]["Networks"].values()))
        self.assert_release_failure("failed", "not answering health checks")

        # Simulate the operator completing the split recovery. The retired
        # slot can advertise again on a later rollback despite its withdrawn aliases.
        retired = self.state()["in_progress"]["origin"]["current"]
        self.ops.run(["docker", "kill", "--signal=TERM", current["containers"]["background"]], mutate=True)
        self.ops.run(["docker", "start", previous["containers"]["background"]], mutate=True)
        self.load_fixture_route(previous)
        deploy.save_state(self.cfg, {"current": previous, "previous": retired, "in_progress": None})
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assert_live_release(current)
        for role in deploy.SERVING_ROLES:
            networks = self.ops.inspect(current["containers"][role])["NetworkSettings"]["Networks"]
            self.assertTrue(all(self.cfg.serving_alias in networks[network]["Aliases"] for network in self.cfg.networks))

    def test_rollback_keeps_adopted_serving_static_ipam_when_reconnecting_alias(self):
        original = {}
        for role in deploy.SERVING_ROLES:
            name = LEGACY["containers"][role]
            endpoints = self.ops.containers[name]["NetworkSettings"]["Networks"]
            for network, endpoint in endpoints.items():
                endpoint.update(Aliases=[name, self.cfg.serving_alias], IPAMConfig={
                    "IPv4Address": f"fixture-ipv4-{role}-{network}",
                    "IPv6Address": f"fixture-ipv6-{role}-{network}",
                    "LinkLocalIPs": [f"fixture-link-local-{role}-{network}"],
                })
            original[name] = json.loads(json.dumps(endpoints))
        config = self.tmp / "adopted.json"
        work = self.tmp / "adopted"
        work.mkdir()
        (work / "runtime.token").write_text("tok\n")
        write = self.ops.write_file_atomic

        def write_adoption(path, text, mode=0o644):
            if Path(path) == config:
                config.write_text(text)
            write(path, text, mode)

        with mock.patch.object(deploy, "DEFAULT_RELEASE_LOCK", self.cfg.lock_file), \
                mock.patch.object(self.ops, "write_file_atomic", write_adoption):
            deploy.adopt(str(config), self.ops, *(LEGACY["containers"][role] for role in deploy.ROLES),
                         str(work), self.cfg.public_health_url)
        self.cfg = deploy.Config.load(str(config))
        self.load_fixture_route(LEGACY, proof=False)
        self.deploy()
        previous = self.state()["previous"]
        run = self.ops.run
        names_by_id = {self.ops.inspect(name)["Id"]: name for name in original}
        snapshots = []

        def verify_each_reconnect(args, **kw):
            result = run(args, **kw)
            if args[:3] == ["docker", "network", "connect"] and args[-1] in names_by_id:
                network = self.ops.network_name(args[-2])
                endpoint = self.ops.inspect(args[-1])["NetworkSettings"]["Networks"][network]
                snapshots.append((original[names_by_id[args[-1]]][network]["IPAMConfig"], endpoint.get("IPAMConfig")))
            return result

        self.ops.commands.clear()
        with mock.patch.object(self.ops, "run", verify_each_reconnect):
            deploy.Deployer(self.cfg, self.ops).rollback()
        self.assert_loaded_interfaces(previous, previous)
        self.assertIsNone(self.state()["in_progress"])
        self.assertTrue(any(cmd[:3] == ["docker", "network", "disconnect"] for cmd in self.ops.commands))
        self.assertTrue(snapshots)
        for expected, actual in snapshots:
            self.assertEqual(expected, actual)
        for name, endpoints in original.items():
            self.assertEqual(endpoints, self.ops.inspect(name)["NetworkSettings"]["Networks"])

    def assert_rollback_waits_for_background(self, *, manual):
        self.cfg.serving_alias = ""
        target = self.cfg.slot_view("a")
        if manual:
            self.deploy()
        else:
            self.ops.memory_after_switch = 20 * GIB
        old_background = LEGACY["containers"]["background"]
        old_port = LEGACY["ports"]["background"]
        old_route = self.upstreams_for(LEGACY)
        live_route = self.upstreams_for(target)
        run, http_json, write = self.ops.run, self.ops.http_json, self.ops.write_file_atomic
        started = None
        health_confirmed = False
        probes_while_starting = []

        def start(args, **kw):
            nonlocal started
            if args == ["docker", "kill", "--signal=TERM", target["containers"]["serving"]]:
                self.assertEqual(old_route, self.upstreams_only(self.ops.loaded_upstreams), "serving retires after the rollback reload")
            result = run(args, **kw)
            if args == ["docker", "start", old_background]:
                started = self.ops.clock
            return result

        def probe(url, **kw):
            nonlocal health_confirmed
            if started is not None and f":{old_port}/" in url and url.endswith("/health"):
                if self.ops.clock - started < 3:
                    self.assertEqual(live_route, self.upstreams_only(self.ops.loaded_upstreams))
                    self.assertEqual(live_route, self.upstreams_only(self.ops.files[self.cfg.upstream_file]))
                    probes_while_starting.append(url)
                    return 0, None
                health_confirmed = True
            return http_json(url, **kw)

        def check_write(path, text, **kw):
            if path == self.cfg.upstream_file and self.upstreams_only(text) == old_route:
                self.assertIsNotNone(started, "upstream was changed before the rollback background was started")
                self.assertGreaterEqual(self.ops.clock - started, 3)
                self.assertTrue(health_confirmed, "reload requires a successful health probe")
            return write(path, text, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "http_json", probe), \
                mock.patch.object(self.ops, "write_file_atomic", check_write):
            if manual:
                deploy.Deployer(self.cfg, self.ops).rollback()
            else:
                with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
                    self.deploy()
        self.assertTrue(probes_while_starting)
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("ok" if manual else "failed", self.history()[-1]["result"])

    def test_guard_rollback_waits_for_healthy_background_before_writing_upstream(self):
        self.assert_rollback_waits_for_background(manual=False)

    def test_manual_rollback_waits_for_healthy_background_before_writing_upstream(self):
        self.assert_rollback_waits_for_background(manual=True)

    def test_guard_rollback_keeps_live_admin_route_if_old_background_cannot_start(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        for role in deploy.SERVING_ROLES:
            for endpoint in self.ops.containers[LEGACY["containers"][role]]["NetworkSettings"]["Networks"].values():
                endpoint["Aliases"] = [self.cfg.serving_alias]
        run = self.ops.run

        def fail_old_background(args, **kw):
            if args == ["docker", "start", LEGACY["containers"]["background"]]:
                raise deploy.DeployError("fixture: old background failed to start")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_old_background):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        route = deploy.render_upstreams(*(LEGACY["ports"][role] for role in deploy.SERVING_ROLES),
                                        target["ports"]["background"])
        self.assertEqual(route, self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(self.ops.loaded_upstreams, self.ops.files[self.cfg.upstream_file])
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).owner_runtime(target)["owns_background"])
        self.assertFalse(self.ops.inspect(target["containers"]["serving"])["State"]["Running"])
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertIn("old background failed to start", progress["reason"])
        self.assertEqual(progress, deploy.status(self.cfg, self.ops)["in_progress"])
        self.assertTrue(any(alert.get("in_progress") == progress for alert in deploy.watch_once(self.cfg, self.ops, {})))
        self.assertIn("ROLLBACK INCOMPLETE", "\n".join(self.ops.logs))

    def test_degraded_rollback_keeps_both_slots_when_successful_reload_is_ignored(self):
        self.ops.memory_after_switch = 20 * GIB
        current = self.cfg.slot_view("a")
        for role in deploy.SERVING_ROLES:
            info = self.ops.containers[LEGACY["containers"][role]]
            for endpoint in info["NetworkSettings"]["Networks"].values():
                endpoint["Aliases"] = [self.cfg.serving_alias]
        run = self.ops.run
        ignored = []

        def fail_background_and_ignore_reload(args, **kw):
            if args == ["docker", "start", LEGACY["containers"]["background"]]:
                raise deploy.DeployError("fixture rollback background could not start")
            if args[1:] == ["-s", "reload"] and self.ops.switched:
                ignored.append(self.ops.files[self.cfg.upstream_file])
                return subprocess.CompletedProcess(args, 0, "", "")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_background_and_ignore_reload):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        self.assertEqual(1, len(ignored))
        self.assert_loaded_interfaces(current, current)
        for role in deploy.SERVING_ROLES:
            self.assertTrue(self.ops.inspect(LEGACY["containers"][role])["State"]["Running"])
            self.assertNotIn(["docker", "kill", "--signal=TERM",
                              self.ops.inspect(current["containers"][role])["Id"]], self.ops.commands)
        self.assert_release_failure("failed", "route unconfirmed")

    def test_degraded_rollback_allows_old_worker_to_finish_reloading_before_retirement(self):
        for role in deploy.SERVING_ROLES:
            for endpoint in self.ops.containers[LEGACY["containers"][role]]["NetworkSettings"]["Networks"].values():
                endpoint["Aliases"] = [self.cfg.serving_alias]
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        run, probe = self.ops.run, self.ops.probe_nginx_serving
        old_route_proof = None
        after_reload = 0
        retirement_samples = []

        def change_route(args, **kw):
            nonlocal old_route_proof
            if args == ["docker", "start", LEGACY["containers"]["background"]]:
                raise deploy.DeployError("fixture original background failed")
            if args == [self.cfg.nginx_bin, "-s", "reload"] and self.ops.switched:
                old_route_proof = probe("", 1)
            if args == ["docker", "kill", "--signal=TERM", target["containers"]["serving"]]:
                retirement_samples.append(after_reload)
            return run(args, **kw)

        def reply(path, timeout):
            nonlocal after_reload
            if old_route_proof is not None:
                after_reload += 1
                if after_reload in (2, 4):
                    return old_route_proof
            return probe(path, timeout)

        with mock.patch.object(self.ops, "run", change_route), mock.patch.object(self.ops, "probe_nginx_serving", reply):
            with self.assertRaises(deploy.RollbackIncomplete):
                self.deploy()
        self.assertTrue(retirement_samples)
        self.assertGreaterEqual(retirement_samples[0], 6)
        self.assert_loaded_interfaces(LEGACY, target)
        self.assert_no_shared_serving(target)
        self.assert_release_failure("failed", "rollback background unavailable")

    def test_resume_keeps_both_serving_pairs_when_reload_is_ignored(self):
        target = self.hold()["target"]
        run = self.ops.run

        def ignore(args, **kw):
            if (args[1:] == ["-s", "reload"] and
                    self.upstreams_only(self.ops.files[self.cfg.upstream_file]) == self.upstreams_for(target)):
                return subprocess.CompletedProcess(args, 0, "", "")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", ignore):
            with self.assertRaises(deploy.FallbackFailed):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(self.upstreams_for(LEGACY), self.upstreams_only(self.ops.loaded_upstreams))
        for view in (LEGACY, target):
            for role in deploy.SERVING_ROLES:
                self.assertTrue(self.ops.inspect(view["containers"][role])["State"]["Running"])
        self.assertEqual(LEGACY, self.state()["current"])
        self.assert_release_failure("failed", "route unconfirmed")

    def test_failed_main_abort_restores_original_route_even_if_the_written_config_disagrees(self):
        self.assert_abort_after_ignored_main_reload()

    def test_failed_first_main_abort_restores_route_without_previous_probe(self):
        self.load_fixture_route(LEGACY, proof=False)
        self.assert_abort_after_ignored_main_reload()

    def assert_abort_after_ignored_main_reload(self):
        self.cfg.serving_alias = ""
        target = self.cfg.slot_view("a")
        run = self.ops.run

        def ignore(args, **kw):
            if args[1:] == ["-s", "reload"]:
                return subprocess.CompletedProcess(args, 0, "", "")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", ignore):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        self.assert_release_failure("failed", "route unconfirmed")
        self.assertEqual(self.upstreams_for(target), self.upstreams_only(self.ops.files[self.cfg.upstream_file]))
        self.assertEqual(self.upstreams_for(LEGACY), self.upstreams_only(self.ops.loaded_upstreams))
        for view in (LEGACY, target):
            for role in deploy.SERVING_ROLES:
                self.assertTrue(self.ops.inspect(view["containers"][role])["State"]["Running"])
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])
        for role in deploy.SERVING_ROLES:
            self.assertIsNone(self.ops.inspect(target["containers"][role]))

    def test_main_failed_fingerprint_after_nginx_test_restores_loaded_admin(self):
        self.deploy()
        original = self.state()["current"]
        run, read = self.ops.run, self.ops.read_bytes
        injecting = False
        failures = []

        def test_nginx(args, **kw):
            nonlocal injecting
            result = run(args, **kw)
            if args == [self.cfg.nginx_bin, "-t"]:
                injecting = True
            return result

        def fail_fingerprint(path):
            if path == self.cfg.upstream_file and injecting and not failures:
                failures.append(path)
                raise OSError("fixture fingerprint read failure")
            return read(path)

        self.ops.commands.clear()
        with mock.patch.object(self.ops, "run", test_nginx), mock.patch.object(self.ops, "read_bytes", fail_fingerprint):
            with self.assertRaisesRegex(deploy.DeployError, "fingerprint"):
                self.deploy(image="fixture:second")
        self.assertEqual([self.cfg.upstream_file], failures)
        self.assert_live_release(original)
        self.assertNotIn([self.cfg.nginx_bin, "-s", "reload"], self.ops.commands)
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_main_rejected_candidate_file_is_removed_if_previous_file_disappears(self):
        self.deploy()
        original = self.state()["current"]
        run = self.ops.run
        new_background = self.cfg.slot_view("b")["containers"]["background"]

        def remove_file_after_handoff(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", new_background]:
                self.ops.files.pop(self.cfg.upstream_file)
            return result

        self.ops.commands.clear()
        self.ops.nginx_test_rc = 1
        with mock.patch.object(self.ops, "run", remove_file_after_handoff):
            with self.assertRaisesRegex(deploy.DeployError, "nginx -t rejected"):
                self.deploy(image="fixture:second")
        self.assertNotIn(self.cfg.upstream_file, self.ops.files)
        self.assert_loaded_interfaces(original, original)
        self.assertNotIn([self.cfg.nginx_bin, "-s", "reload"], self.ops.commands)
        self.assert_release_failure("failed", "upstream file")

    def test_main_can_install_a_probe_with_a_long_state_path(self):
        original_state = self.state()
        self.cfg.state_file = str(self.tmp / ("nested-" * 15) / "state.json")
        deploy.save_state(self.cfg, original_state)
        self.load_fixture_route(LEGACY, proof=False)
        self.deploy()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertIn("X-Sources-Route", self.ops.loaded_upstreams)

    def test_main_inspect_timeout_before_slot_changes_allows_retry(self):
        self.deploy()
        self.deploy(image="fixture:second")
        before = self.state()
        run, inspect = self.ops.run, self.ops.inspect
        exported = False
        seen = []

        def export_then_inject(args, **kw):
            nonlocal exported
            result = run(args, **kw)
            if args[:2] == ["cp", "-Rn"]:
                exported = True
            return result

        def inspect_once(name):
            if exported and not seen:
                seen.append(name)
                raise deploy.CommandTimedOut(subprocess.TimeoutExpired(["docker", "inspect", name], 1))
            return inspect(name)

        with mock.patch.object(self.ops, "run", export_then_inject), mock.patch.object(self.ops, "inspect", inspect_once):
            with self.assertRaisesRegex(deploy.DeployError, "timed out"):
                self.deploy(image="fixture:third")
        self.assertEqual(1, len(seen))
        self.assertEqual(before, self.state())
        self.assert_live_release(before["current"])
        for container in before["previous"]["containers"].values():
            self.assertIsNotNone(self.ops.inspect(container))
        self.assertEqual("failed", self.history()[-1]["result"])
        self.deploy(image="fixture:retry")
        self.assert_live_release(self.state()["current"])

    def test_first_hold_without_probe_keeps_upstream_and_loaded_route_unchanged(self):
        self.load_fixture_route(LEGACY, proof=False)
        original = self.ops.loaded_upstreams
        self.hold()
        self.assertEqual(original, self.ops.files[self.cfg.upstream_file])
        self.assertEqual(original, self.ops.loaded_upstreams)
        self.assertFalse(any(args[0] == self.cfg.nginx_bin and args[1:] in (["-t"], ["-s", "reload"])
                             for args in self.ops.commands))
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_main_accepts_keepalive_in_adopted_fixed_upstreams(self):
        self.load_fixture_route(LEGACY, proof=False)
        text = self.ops.loaded_upstreams.replace("zone sources_main_serving 64k;",
                                                "zone sources_main_serving 64k;\n    keepalive 16;")
        self.ops.loaded_upstreams = self.ops.files[self.cfg.upstream_file] = text
        self.deploy()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_main_accepts_localhost_peers_in_adopted_fixed_upstreams(self):
        self.load_fixture_route(LEGACY, proof=False)
        text = self.ops.loaded_upstreams.replace("server 127.0.0.1:", "server localhost:")
        self.ops.loaded_upstreams = self.ops.files[self.cfg.upstream_file] = text
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_abort_removes_candidate_after_hold_process_dies_in_docker_start(self):
        target = self.cfg.slot_view("a")
        primary = target["containers"]["serving"]
        run = self.ops.run

        class ProcessKilled(BaseException):
            pass

        def lose_process(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", primary]:
                raise ProcessKilled
            return result

        with mock.patch.object(self.ops, "run", lose_process):
            with self.assertRaises(ProcessKilled):
                self.hold()
        self.assertTrue(self.ops.inspect(primary)["State"]["Running"])
        self.assertEqual("deploying", self.state()["in_progress"]["phase"])
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.ops.inspect(primary))
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

    def assert_abort_recovers_hold_during_create(self, *, unrelated=False):
        target = self.cfg.slot_view("a")
        primary = target["containers"]["serving"]
        run = self.ops.run

        class ProcessKilled(BaseException):
            pass

        def lose_process(args, **kw):
            result = run(args, **kw)
            if args[:2] == ["docker", "create"] and args[args.index("--name") + 1] == primary:
                if unrelated:
                    self.ops.containers[primary]["Config"]["Labels"] = {}
                raise ProcessKilled
            return result

        with mock.patch.object(self.ops, "run", lose_process):
            with self.assertRaises(ProcessKilled):
                self.hold()
        candidate = self.ops.inspect(primary)
        self.assertFalse(candidate["State"]["Running"])
        self.assertNotIn("serving", self.state()["in_progress"].get("owned", {}))
        if unrelated:
            with self.assertRaisesRegex(deploy.DeployError, "ownership is unconfirmed"):
                deploy.Deployer(self.cfg, self.ops).abort()
            self.assertEqual(candidate, self.ops.inspect(primary))
            self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        else:
            deploy.Deployer(self.cfg, self.ops).abort()
            self.assertIsNone(self.ops.inspect(primary))
            self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_abort_finds_held_candidate_created_before_docker_returns_id(self):
        self.assert_abort_recovers_hold_during_create()

    def test_abort_preserves_untracked_container_without_the_hold_creation_label(self):
        self.assert_abort_recovers_hold_during_create(unrelated=True)

    def test_abort_can_recover_resume_abandoned_at_background_handoff(self):
        held = self.hold()
        runner = deploy.Deployer(self.cfg, self.ops)

        class ProcessKilled(BaseException):
            pass

        with mock.patch.object(runner, "handoff_background", side_effect=ProcessKilled):
            with self.assertRaises(ProcessKilled):
                runner.resume()
        self.assertEqual("deploying", self.state()["in_progress"]["phase"])
        self.assertIn(["release_incomplete"], [item["alert"] for item in deploy.watch_once(self.cfg, self.ops, {})])
        runner.abort()
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])
        for role in deploy.SERVING_ROLES:
            self.assertIsNone(self.ops.inspect(held["target"]["containers"][role]))

    def test_abort_rejects_fifo_without_holding_release_lock(self):
        self.cfg.upstream_file = self.ops.upstream_file = str(self.tmp / "upstreams.conf")
        self.load_fixture_route(LEGACY)
        self.hold()
        os.mkfifo(self.cfg.upstream_file)
        read_text = self.ops.read_text
        read_bytes = self.ops.read_bytes

        def text_input(path):
            return deploy.Ops.read_text(self.ops, path) if path == self.cfg.upstream_file else read_text(path)

        def bytes_input(path):
            return deploy.Ops.read_bytes(self.ops, path) if path == self.cfg.upstream_file else read_bytes(path)

        def abort():
            try:
                deploy.Deployer(self.cfg, self.ops).abort()
            except deploy.DeployError as err:
                if "regular file" not in str(err):
                    raise
            else:
                raise AssertionError("abort accepted a FIFO")

        with mock.patch.object(self.ops, "read_text", text_input), mock.patch.object(self.ops, "read_bytes", bytes_input):
            child = multiprocessing.get_context("fork").Process(target=abort)
            child.start()
            try:
                child.join(2)
                self.assertEqual(0, child.exitcode)
                with deploy.release_lock(self.cfg.lock_file):
                    self.assertIsNotNone(self.state()["in_progress"])
            finally:
                if child.is_alive():
                    child.terminate()
                child.join(3)

    def test_main_reuses_switch_reload_to_install_probe_on_first_release(self):
        self.load_fixture_route(LEGACY, proof=False)
        self.deploy()
        self.assertEqual([[self.cfg.nginx_bin, flag] for flag in ("-T", "-t")]
                         + [[self.cfg.nginx_bin, "-s", "reload"]],
                         [args for args in self.ops.commands if args[0] == self.cfg.nginx_bin])
        self.assert_live_release(self.state()["current"])

    def test_main_full_command_trace_does_not_repeat_docker_queries_within_a_check(self):
        self.load_fixture_route(LEGACY, proof=False)
        inspect = self.ops.inspect

        def record_inspect(name):
            self.ops.commands.append(["docker", "inspect", name])
            return inspect(name)

        with mock.patch.object(self.ops, "inspect", record_inspect):
            for sequence, slot in enumerate(("a", "b")):
                with self.subTest(release=sequence + 1):
                    original = self.state()["current"]["containers"]
                    target = self.cfg.slot_view(slot)["containers"]
                    old_serving = [original[role] for role in deploy.SERVING_ROLES]
                    new_serving = [target[role] for role in deploy.SERVING_ROLES]
                    self.ops.commands.clear()
                    self.deploy(image=f"fixture:release-{sequence}")
                    inspected = [args[-1] for args in self.ops.commands if args[:2] == ["docker", "inspect"]]
                    self.assertEqual(52, len(self.ops.commands) - len(inspected))
                    expected = [target[role] for role in deploy.ROLES]  # preflight
                    expected += old_serving + (new_serving if sequence else [])  # confirm before slot reuse
                    expected += [original["background"], original["background"], target["background"]]
                    expected += new_serving * 2  # check each port before and after reload
                    expected += [target["serving"]] * 8  # guard polls
                    expected += new_serving + old_serving * 2  # readiness, alias coverage and retirement
                    self.assertEqual(expected, inspected)

    def test_main_second_release_publishes_alias_at_create_using_baseline_commands(self):
        self.deploy()
        source = self.state()["current"]
        target = self.cfg.slot_view("b")
        self.ops.commands.clear()
        self.deploy(image="another@sha256:3")
        roles = deploy.SERVING_ROLES
        names = {target["containers"][role] for role in roles}
        previous_names = {source["containers"][role] for role in roles}
        endpoints = set(target["containers"].values()) | previous_names
        endpoints |= {self.ops.inspect(name)["Id"] for name in endpoints}
        sequence = []
        for args in self.ops.commands:
            if args[:2] == ["docker", "create"] and args[args.index("--name") + 1] in names:
                aliases = [args[i + 1] for i, arg in enumerate(args) if arg == "--network-alias"]
                sequence.append(["docker", "create", args[args.index("--name") + 1], *aliases])
            elif ((args[:3] in (["docker", "network", "connect"], ["docker", "network", "disconnect"])
                   and args[-1] in endpoints)
                  or (args[:2] == ["docker", "start"] and args[-1] in names)
                  or (args[0] == self.cfg.nginx_bin and args[1:] in (["-t"], ["-s", "reload"]))
                  or (args[-1] in previous_names and args[:2] in (["docker", "update"], ["docker", "kill"]))):
                sequence.append(args)
        expected = []
        for role in roles:
            name = target["containers"][role]
            alias = self.cfg.serving_alias
            expected.append(["docker", "create", name, name, alias])
            expected.extend(["docker", "network", "connect", "--alias", name, "--alias", alias, network, name]
                            for network in self.cfg.networks[1:])
            expected.append(["docker", "start", name])
        expected.append(["docker", "network", "connect", "--alias", target["containers"]["background"],
                         self.cfg.networks[1], target["containers"]["background"]])
        expected.extend(([self.cfg.nginx_bin, "-t"], [self.cfg.nginx_bin, "-s", "reload"]))
        for role in roles:
            name = source["containers"][role]
            expected.extend((["docker", "update", "--restart=no", name], ["docker", "kill", "--signal=TERM", name]))
        self.assertEqual(expected, sequence)
        self.assert_live_release(target)
        self.assertIsNone(self.state()["in_progress"])

    def test_main_guard_can_restore_original_release_during_expected_alias_overlap(self):
        self.deploy()
        previous = self.state()["current"]
        self.ops.public_status_after_reload = 503
        with self.assertRaisesRegex(deploy.DeployError, "guard failed.*previous release serves traffic"):
            self.deploy(image="another@sha256:3")
        self.assert_live_release(previous)
        self.assertEqual(previous, self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        self.assert_no_shared_serving(self.cfg.slot_view("b"))

    def test_main_keeps_original_alias_provider_on_uncovered_network(self):
        self.deploy()
        previous = self.state()["current"]
        networks = self.ops.containers[previous["containers"]["serving"]]["NetworkSettings"]["Networks"]
        networks["fixture-extra-network"] = {"Aliases": [self.cfg.serving_alias]}
        with self.assertRaisesRegex(deploy.FallbackFailed, "shared alias on all required networks"):
            self.deploy(image="another@sha256:3")
        self.assert_live_release(self.cfg.slot_view("b"))
        self.assertTrue(self.ops.inspect(previous["containers"]["serving"])["State"]["Running"])
        self.assert_release_failure("failed", "shared alias on all required networks")

    def test_abort_restores_original_alias_after_ordinary_retirement_fails(self):
        self.deploy()
        previous = self.state()["current"]
        primary = previous["containers"]["serving"]
        standby = previous["containers"]["standby"]
        run = self.ops.run

        def fail(args, **kw):
            if args == ["docker", "kill", "--signal=TERM", standby]:
                raise deploy.DeployError("fixture standby retirement failed")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.FallbackFailed, "retirement incomplete"):
                self.deploy(image="another@sha256:3")
        self.assert_live_release(self.cfg.slot_view("b"))
        self.assertFalse(self.ops.inspect(primary)["State"]["Running"])
        self.assert_release_failure("failed", "retirement incomplete")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assert_live_release(previous)
        self.assertEqual(previous, self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        for name in (primary, standby):
            endpoints = self.ops.inspect(name)["NetworkSettings"]["Networks"]
            self.assertTrue(all(self.cfg.serving_alias in endpoints[network]["Aliases"]
                                for network in self.cfg.networks))
        self.assert_no_shared_serving(self.cfg.slot_view("b"))

    def test_watch_ignores_expected_overlap_during_main_release_and_retirement(self):
        self.deploy()
        previous = self.state()["current"]
        previous_primary = previous["containers"]["serving"]
        self.ops.drain_seconds[previous_primary] = 5
        target = self.cfg.slot_view("b")
        primary = target["containers"]["serving"]
        pid = os.getpid()
        stat = f"/proc/{pid}/stat"
        process = f"{pid} (release) R " + "0 " * 20
        self.ops.files["/proc/sys/kernel/random/boot_id"] = "fixture-boot\n"
        self.ops.files[stat] = process
        run = self.ops.run
        active, interrupted = [], []

        def sample_after_start(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", primary]:
                active.extend(deploy.watch_once(self.cfg, self.ops, {}))
                self.ops.files.pop(stat)
                try:
                    interrupted.extend(deploy.watch_once(self.cfg, self.ops, {}))
                finally:
                    self.ops.files[stat] = process
            return result

        with mock.patch.object(self.ops, "run", sample_after_start):
            self.deploy(image="fixture@sha256:second")
        self.assertNotIn(["shared_alias_conflict"], [item["alert"] for item in active])
        self.assertIn(["shared_alias_conflict"], [item["alert"] for item in interrupted])
        self.assertIn(["release_incomplete"], [item["alert"] for item in interrupted])
        self.assertTrue(self.ops.inspect(previous_primary)["State"]["Running"])
        self.assertNotIn(["shared_alias_conflict"], [item["alert"] for item in deploy.watch_once(self.cfg, self.ops, {})])
        run(["docker", "start", previous_primary], mutate=True)
        self.assertIn(["shared_alias_conflict"], [item["alert"] for item in deploy.watch_once(self.cfg, self.ops, {})])

    def test_watch_reports_an_instance_restarted_before_retirement_finishes(self):
        self.deploy()
        primary = self.state()["current"]["containers"]["serving"]
        self.ops.drain_seconds[primary] = 10
        pid = os.getpid()
        self.ops.files["/proc/sys/kernel/random/boot_id"] = "fixture-boot\n"
        self.ops.files[f"/proc/{pid}/stat"] = f"{pid} (release) R " + "0 " * 20
        run = self.ops.run
        samples = []

        def monitor_retirement(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "kill", "--signal=TERM", primary]:
                self.assertEqual("deploying", self.state()["in_progress"]["phase"])
                samples.append([alert["alert"] for alert in deploy.watch_once(self.cfg, self.ops, {})])
                self.ops.sleep(11)
                run(["docker", "start", primary], mutate=True)
                samples.append([alert["alert"] for alert in deploy.watch_once(self.cfg, self.ops, {})])
            return result

        with mock.patch.object(self.ops, "run", monitor_retirement):
            self.deploy(image="fixture:second")
        self.assertNotIn(["shared_alias_conflict"], samples[0])
        self.assertIn(["shared_alias_conflict"], samples[1])

    def test_resume_failed_second_alias_network_with_existing_provider_returns_to_hold(self):
        self.assert_resume_second_alias_network_failure(persistent=False)

    def test_resume_repairs_isolation_after_persistent_second_network_failure_clears(self):
        self.assert_resume_second_alias_network_failure(persistent=True)

    def test_resume_alias_network_failure_after_switch_attempts_to_restore_hold(self):
        self.assert_resume_second_alias_network_failure(persistent=False, only_after_switch=True)

    def assert_resume_second_alias_network_failure(self, *, persistent, only_after_switch=False):
        self.deploy()
        paused = self.hold()
        original, target = paused["current"], paused["target"]
        ids = {value["id"] for value in paused["containers"].values()}
        run = self.ops.run
        attempted = []

        def fail_alias_network(args, **kw):
            if (args[:3] == ["docker", "network", "connect"] and args[-1] in ids
                    and self.ops.network_name(args[-2]) == self.cfg.networks[1]
                    and (self.cfg.serving_alias in args or persistent)):
                activated = self.state()["current"]["slot"] == target["slot"]
                if only_after_switch and not activated:
                    return run(args, **kw)
                attempted.append(activated)
                raise deploy.DeployError("fixture second alias network unavailable")
            return run(args, **kw)

        runner = deploy.Deployer(self.cfg, self.ops)
        with mock.patch.object(self.ops, "run", fail_alias_network):
            with self.assertRaisesRegex(deploy.DeployError, "second alias network unavailable"):
                runner.resume()
        self.assertTrue(attempted)
        self.assertEqual({only_after_switch}, set(attempted))
        self.assert_live_release(original)
        self.assertEqual(original, self.state()["current"])
        self.assert_release_failure("held")
        self.assertEqual(paused["containers"], self.state()["in_progress"]["containers"])
        self.assert_no_shared_serving(target)
        self.assertIsNone(self.ops.inspect(target["containers"]["background"]))
        for role in deploy.SERVING_ROLES:
            endpoints = self.ops.inspect(original["containers"][role])["NetworkSettings"]["Networks"]
            self.assertTrue(all(self.cfg.serving_alias in endpoints[net]["Aliases"] for net in self.cfg.networks))
        if persistent:
            self.assertIn("network restoration is incomplete", self.state()["in_progress"]["reason"])
            deploy.Deployer(self.cfg, self.ops).resume()
            self.assert_live_release(target)
        else:
            deploy.Deployer(self.cfg, self.ops).resume()
            self.assert_live_release(self.state()["current"])
            self.assertEqual(target["slot"], self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_continuously_provides_the_shared_alias_on_each_network(self):
        self.deploy()
        source = self.state()["current"]
        target = self.hold()["target"]
        run = self.ops.run

        def alias_owners():
            owners = {net: set() for net in self.cfg.networks}
            for view in (source, target):
                for role in deploy.SERVING_ROLES:
                    info = self.ops.inspect(view["containers"][role]) or {}
                    if info.get("State", {}).get("Running"):
                        for net, ep in info["NetworkSettings"]["Networks"].items():
                            if self.cfg.serving_alias in ep.get("Aliases", ()):
                                owners[net].add(view["slot"])
            return owners

        samples = []

        def sample(args, **kw):
            result = run(args, **kw)
            samples.append((args[:3], alias_owners()))
            return result

        self.assertEqual({net: {source["slot"]} for net in self.cfg.networks}, alias_owners())
        with mock.patch.object(self.ops, "run", sample):
            deploy.Deployer(self.cfg, self.ops).resume()
        for args, owners in samples:
            self.assertTrue(all(owners.values()), (args, owners))
        self.assertTrue(any(all(slots == {source["slot"], target["slot"]} for slots in owners.values())
                            for _, owners in samples))
        self.assertEqual({net: {target["slot"]} for net in self.cfg.networks}, alias_owners())
        self.assert_live_release(target)
        self.assertIsNone(self.state()["in_progress"])

    def test_abort_reconnects_original_endpoint_after_failed_alias_withdrawal(self):
        self.deploy()
        source = self.state()["current"]
        self.hold()
        primary = source["containers"]["serving"]
        cid = self.ops.inspect(primary)["Id"]
        run = self.ops.run
        network = self.cfg.networks[0]

        def fail(args, **kw):
            if args[:3] == ["docker", "network", "connect"] and args[-2:] == [network, cid]:
                raise deploy.DeployError("fixture original alias removal reconnect failed")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.FallbackFailed, "alias removal reconnect failed"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertNotIn(network, self.ops.inspect(primary)["NetworkSettings"]["Networks"])
        self.assert_release_failure("failed")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assert_live_release(source)
        self.assertIn(self.cfg.serving_alias, self.ops.inspect(primary)["NetworkSettings"]["Networks"][network]["Aliases"])
        self.assertIsNone(self.state()["in_progress"])

    def test_guard_rollback_keeps_both_serving_pairs_when_reload_is_ignored(self):
        self.cfg.serving_alias = ""
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        run = self.ops.run

        def ignore(args, **kw):
            if args[1:] == ["-s", "reload"] and self.ops.switched:
                return subprocess.CompletedProcess(args, 0, "", "")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", ignore):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        self.assertEqual(self.upstreams_for(target), self.upstreams_only(self.ops.loaded_upstreams))
        for view in (LEGACY, target):
            for role in deploy.SERVING_ROLES:
                self.assertTrue(self.ops.inspect(view["containers"][role])["State"]["Running"])
        self.assertEqual(target["slot"], self.state()["current"]["slot"])
        self.assert_release_failure("failed", "route unconfirmed")

    def test_rollback_does_not_remove_the_only_shared_alias_provider(self):
        self.deploy()
        current = self.state()["current"]
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).rollback()
        for role in deploy.SERVING_ROLES:
            info = self.ops.inspect(current["containers"][role])
            self.assertTrue(info["State"]["Running"])
            self.assertTrue(all(self.cfg.serving_alias in endpoint.get("Aliases", ()) for endpoint in
                                info["NetworkSettings"]["Networks"].values()))
        self.assert_release_failure("failed", "shared alias")

    def test_rollback_leaves_ambiguous_shared_alias_ownership_untouched(self):
        self.deploy()
        state = self.state()
        for role in deploy.SERVING_ROLES:
            info = self.ops.containers[LEGACY["containers"][role]]
            info["State"].update(Running=True, Status="running")
            for endpoint in info["NetworkSettings"]["Networks"].values():
                endpoint["Aliases"] = [self.cfg.serving_alias]
        before = json.loads(json.dumps(self.ops.containers))
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(before, self.ops.containers)
        self.assert_loaded_interfaces(state["current"], state["current"])
        self.assert_release_failure("failed", "shared alias")

    def test_degraded_rollback_keeps_only_alias_provider_when_legacy_has_no_alias(self):
        self.ops.memory_after_switch = 20 * GIB
        current = self.cfg.slot_view("a")
        run = self.ops.run

        def fail_background(args, **kw):
            if args == ["docker", "start", LEGACY["containers"]["background"]]:
                raise deploy.DeployError("fixture rollback background could not start")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_background):
            with self.assertRaises(deploy.RollbackIncomplete):
                self.deploy()
        self.assert_loaded_interfaces(LEGACY, current)
        for role in deploy.SERVING_ROLES:
            info = self.ops.inspect(current["containers"][role])
            self.assertTrue(info["State"]["Running"])
            self.assertTrue(all(self.cfg.serving_alias in endpoint["Aliases"] for endpoint in
                                info["NetworkSettings"]["Networks"].values()))
        self.assert_release_failure("failed", "target does not provide the shared alias")

    def test_guard_rollback_does_not_reload_when_neither_background_is_available(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        run = self.ops.run
        starts = []

        def fail_backgrounds(args, **kw):
            if args[:2] == ["docker", "start"] and args[-1] in (
                    LEGACY["containers"]["background"], target["containers"]["background"]):
                starts.append(args[-1])
                if starts != [target["containers"]["background"]]:
                    raise deploy.DeployError("fixture: background failed to start")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_backgrounds):
            with self.assertRaisesRegex(deploy.FallbackFailed, "verify.*before clearing in_progress"):
                self.deploy()
        self.assertEqual([target["containers"]["background"], LEGACY["containers"]["background"],
                          target["containers"]["background"]], starts)
        self.assertEqual(self.upstreams_for(target), self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(self.ops.loaded_upstreams, self.ops.files[self.cfg.upstream_file])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        log = "\n".join(self.ops.logs)
        self.assertIn("FAILED AND NOT RESTORED", log)
        self.assertIn("restore one background", log)

    def test_guard_rollback_precheck_preserves_running_background(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        self.ops.busy_ports.add(LEGACY["ports"]["background"])
        with self.assertRaisesRegex(deploy.RollbackIncomplete, "port is already in use"):
            self.deploy()
        self.assertNotIn(["docker", "kill", "--signal=TERM", target["containers"]["background"]], self.ops.commands)
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        expected = {**LEGACY["ports"], "background": target["ports"]["background"]}
        self.assertEqual(self.upstreams_for({"ports": expected}), self.upstreams_only(self.ops.loaded_upstreams))

    def test_rollback_reloads_held_state_after_lock_acquisition(self):
        self.deploy()
        self.ops.commands.clear()
        current = self.state()
        lock = deploy.release_lock

        @contextlib.contextmanager
        def publish_hold_before_lock(path, **kw):
            deploy.save_state(self.cfg, {**current, "in_progress": {"phase": "held"}})
            with lock(path, **kw):
                yield

        with mock.patch.object(deploy, "release_lock", publish_hold_before_lock):
            with self.assertRaisesRegex(deploy.DeployError, "resume.*abort"):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual([], self.ops.commands)
        self.assertEqual("held", self.state()["in_progress"]["phase"])
        self.assert_live_release(current["current"])

    def test_hold_slot_cleanup_failure_can_resume_preparation_or_abort(self):
        for action in ("resume", "abort"):
            with self.subTest(action=action):
                target = self.cfg.slot_view("a")
                name = target["containers"]["background"]
                self.ops.add(name, target["ports"]["background"], running=False)
                cid = self.ops.inspect(name)["Id"]
                run = self.ops.run

                def cannot_remove(args, **kw):
                    if args[:2] == ["docker", "rm"] and args[-1] in (name, cid):
                        if kw.get("check", True):
                            raise deploy.DeployError("fixture: removal failed")
                        return subprocess.CompletedProcess(args, 1, "", "fixture: removal failed")
                    return run(args, **kw)

                with mock.patch.object(self.ops, "run", cannot_remove):
                    with self.assertRaisesRegex(deploy.DeployError, "retry.*resume.*abort"):
                        self.hold()
                    self.assertEqual("failed", self.state()["in_progress"]["phase"])
                    self.assertFalse(any(n in self.ops.containers for role, n in target["containers"].items()
                                         if role in deploy.SERVING_ROLES))
                    if action == "abort":
                        deploy.Deployer(self.cfg, self.ops).abort()
                        self.assertIsNotNone(self.ops.inspect(cid))
                if action == "resume":
                    deploy.Deployer(self.cfg, self.ops).resume()
                    self.assertEqual("held", self.state()["in_progress"]["phase"])
                    self.assertIsNone(self.ops.inspect(cid))
                    deploy.Deployer(self.cfg, self.ops).abort()
                self.assertIsNone(self.state()["in_progress"])
                self.assert_live_release(LEGACY)

    def test_resume_rejects_in_place_changes_to_background_env(self):
        path = self.tmp / "background.env"
        path.write_bytes(b"FIXTURE=before\n")
        self.cfg.env_files["background"] = str(path)
        held = self.hold()
        path.write_bytes(b"FIXTURE=after\n")
        before = list(self.ops.commands)
        with self.assertRaisesRegex(deploy.DeployError, "environment files changed.*abort"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(before, self.ops.commands)
        self.assertEqual(held, self.state()["in_progress"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(held["target"])
        self.assert_release_failure("held")

    def test_resume_retry_preparation_failure_keeps_healthy_routes_and_alerts(self):
        target = self.cfg.slot_view("a")
        name = target["containers"]["background"]
        self.ops.add(name, target["ports"]["background"], running=False)
        run = self.ops.run

        def fail_remove(args, **kw):
            if args == ["docker", "rm", self.ops.inspect(name)["Id"]]:
                raise deploy.DeployError("fixture slot cleanup failed")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_remove):
            with self.assertRaisesRegex(deploy.DeployError, "held slot cleanup failed"):
                self.hold()
        self.ops.check_fails = True
        with self.assertRaises(deploy.DeployError):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(target)
        self.assert_release_failure("failed")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
        self.assert_live_release(LEGACY)

    def assert_rollback_degrades_on_late_background_exit(self, *, manual):
        self.cfg.serving_alias = ""
        source = self.cfg.slot_view("a")
        if manual:
            self.deploy()
        else:
            self.ops.memory_after_switch = 20 * GIB
        run = self.ops.run
        previous_route = self.upstreams_for(LEGACY)
        reloads = []
        exited = False

        def stop_destination_after_test(args, **kw):
            nonlocal exited
            candidate = self.ops.files[self.cfg.upstream_file]
            if args[1:] == ["-s", "reload"]:
                reloads.append(self.upstreams_only(candidate))
            result = run(args, **kw)
            if args[1:] == ["-t"] and self.upstreams_only(candidate) == previous_route:
                name = LEGACY["containers"]["background"]
                self.ops.containers[name]["State"].update(Running=False, Status="exited")
                exited = True
            return result

        with mock.patch.object(self.ops, "run", stop_destination_after_test):
            with self.assertRaisesRegex(deploy.RollbackIncomplete, "Serving uses slot.*admin uses slot"):
                if manual:
                    deploy.Deployer(self.cfg, self.ops).rollback()
                else:
                    self.deploy()
        self.assertTrue(exited)
        expected = self.upstreams_for({"ports": {**LEGACY["ports"], "background": source["ports"]["background"]}})
        self.assertEqual([expected] if manual else [self.upstreams_for(source), expected], reloads)
        self.assertEqual(expected, self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(self.ops.loaded_upstreams, self.ops.files[self.cfg.upstream_file])
        self.assert_loaded_interfaces(LEGACY, source)
        self.assert_release_failure("failed", "not answering health checks")
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_manual_rollback_background_exit_before_reload_rolls_back_serving_only(self):
        self.assert_rollback_degrades_on_late_background_exit(manual=True)

    def test_guard_rollback_background_exit_before_reload_rolls_back_serving_only(self):
        self.assert_rollback_degrades_on_late_background_exit(manual=False)

    def test_port_probe_sets_reuseaddr_before_bind(self):
        conn = mock.MagicMock()
        conn.__enter__.return_value = conn
        with mock.patch.object(deploy.socket, "socket", return_value=conn):
            self.assertTrue(deploy.Ops().port_free(self.cfg.slots["a"]["background"]))
        self.assertEqual(mock.call.setsockopt(deploy.socket.SOL_SOCKET, deploy.socket.SO_REUSEADDR, 1),
                         conn.method_calls[0])
        conn.listen.assert_called_once_with(1)

    def test_manual_rollback_can_recover_a_stopped_background(self):
        self.cfg.serving_alias = ""
        self.deploy()
        current = self.state()["current"]
        name = current["containers"]["background"]
        self.ops.containers[name]["State"].update(Running=False, Status="exited")
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertNotIn(["docker", "kill", "--signal=TERM", name], self.ops.commands)
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

    def test_guard_rollback_refuses_a_serving_port_that_stops_answering_during_switch(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        run, http_json = self.ops.run, self.ops.http_json
        failed_port = LEGACY["ports"]["standby"]
        reloads = []
        offline = False

        def observe_switch(args, **kw):
            nonlocal offline
            if args == [self.cfg.nginx_bin, "-t"] and self.ops.switched:
                offline = True
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                reloads.append(self.upstreams_only(self.ops.files[self.cfg.upstream_file]))
            return run(args, **kw)

        def probe(url, **kw):
            if offline and url.endswith(f":{failed_port}/health"):
                return 0, None
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "run", observe_switch), mock.patch.object(self.ops, "http_json", probe):
            with self.assertRaisesRegex(deploy.FallbackFailed, "rollback standby.*not answering"):
                self.deploy()
        self.assertTrue(offline)
        self.assertEqual([self.upstreams_for(target)], reloads)
        self.assert_live_release(target)
        self.assertEqual("failed", self.state()["in_progress"]["phase"])

    def test_main_guard_docker_query_failure_restores_live_release(self):
        self.cfg.serving_alias = ""
        target = self.cfg.slot_view("a")
        inspect = self.ops.inspect
        query_failed = False

        def fail_after_switch(name):
            nonlocal query_failed
            if name == target["containers"]["serving"] and (self.state()["in_progress"] or {}).get("step") == "guard" and not query_failed:
                query_failed = True
                raise deploy.DeployError("simulated Docker inspect failure")
            return inspect(name)

        with mock.patch.object(self.ops, "inspect", fail_after_switch):
            with self.assertRaisesRegex(deploy.DeployError, "simulated Docker inspect failure"):
                self.deploy()
        self.assertTrue(query_failed)
        self.assert_live_release(LEGACY)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_main_guard_exception_with_failed_route_restore_keeps_fault(self):
        self.cfg.serving_alias = ""
        target = self.cfg.slot_view("a")
        inspect, run = self.ops.inspect, self.ops.run

        interrupted = False

        def fail_guard(name):
            nonlocal interrupted
            if (name == target["containers"]["serving"] and not interrupted
                    and (self.state()["in_progress"] or {}).get("step") == "guard"):
                interrupted = True
                raise KeyboardInterrupt
            return inspect(name)

        def fail_restore(args, **kw):
            if args == [self.cfg.nginx_bin, "-t"] and self.ops.switched:
                return subprocess.CompletedProcess(args, 1, "", "simulated restore failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "inspect", fail_guard), mock.patch.object(self.ops, "run", fail_restore):
            with self.assertRaisesRegex(deploy.FallbackFailed, "switching back failed"):
                self.deploy()
        self.assert_live_release(target)
        self.assertEqual(target["slot"], self.state()["current"]["slot"])
        progress = self.state()["in_progress"]
        self.assertEqual("switch", progress["failed_step"])
        self.assertIn("KeyboardInterrupt", progress["reason"])

    def test_resume_busy_or_unreserved_background_port_keeps_pause(self):
        paused = self.hold()
        port = paused["target"]["ports"]["background"]
        for failure in ("already in use", "ephemeral range"):
            with self.subTest(failure=failure):
                self.ops.busy_ports = {port} if failure == "already in use" else set()
                if failure == "ephemeral range":
                    self.ops.files["/proc/sys/net/ipv4/ip_local_port_range"] = f"{port} {port}"
                    self.ops.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = ""
                before = len(self.ops.commands)
                with self.assertRaisesRegex(deploy.DeployError, failure):
                    deploy.Deployer(self.cfg, self.ops).resume()
                self.assertEqual([], self.ops.commands[before:])
                self.assert_live_release(LEGACY)
                self.assertEqual(paused, self.state()["in_progress"])

    def test_background_start_error_recovers_after_old_owner_exits(self):
        # The address can become busy after the preflight check. Test both
        # entry points, including the case where Docker started but did not reply.
        for held, outcome_unknown in ((True, False), (False, True)):
            with self.subTest(held=held, outcome_unknown=outcome_unknown):
                target = self.hold()["target"] if held else self.cfg.slot_view("a")
                run = self.ops.run

                def fail_start(args, **kw):
                    if args == ["docker", "start", target["containers"]["background"]]:
                        self.assertFalse(self.ops.inspect(LEGACY["containers"]["background"])["State"]["Running"])
                        if outcome_unknown:
                            run(args, **kw)
                            raise subprocess.TimeoutExpired(args, 1)
                        raise deploy.DeployError("simulated port binding failure")
                    return run(args, **kw)

                with mock.patch.object(self.ops, "run", fail_start):
                    with self.assertRaises(deploy.DeployError):
                        if held:
                            deploy.Deployer(self.cfg, self.ops).resume()
                        else:
                            self.deploy()
                self.assert_live_release(LEGACY)
                self.assertEqual(LEGACY, self.state()["current"])
                if held:
                    self.assert_no_shared_serving(target)
                    self.assert_release_failure("failed", "simulated port binding failure")
                    deploy.save_state(self.cfg, {**self.state(), "in_progress": None})
                else:
                    self.assertIsNone(self.state()["in_progress"])
                info = self.ops.inspect(LEGACY["containers"]["background"])
                self.assertEqual("unless-stopped", info["HostConfig"]["RestartPolicy"]["Name"])

    def test_main_old_background_exit_query_error_restarts_exited_owner(self):
        old = LEGACY["containers"]["background"]
        inspect = self.ops.inspect
        failed = False

        def fail_after_term(name):
            nonlocal failed
            info = inspect(name)
            if name == old and info and not info["State"]["Running"] and not failed:
                failed = True
                raise deploy.DeployError("simulated exit query failure")
            return info

        with mock.patch.object(self.ops, "inspect", fail_after_term):
            with self.assertRaisesRegex(deploy.DeployError, "simulated exit query failure"):
                self.deploy()
        self.assertTrue(failed)
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

    def test_main_background_restore_failure_records_gap(self):
        target = self.cfg.slot_view("a")
        run = self.ops.run
        attempted = []

        def fail(args, **kw):
            if args[:2] == ["docker", "start"] and args[-1] in (
                    target["containers"]["background"], LEGACY["containers"]["background"]):
                attempted.append(args[-1])
                raise deploy.DeployError("simulated background start failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.FallbackFailed, "verify and restore background ownership"):
                self.deploy()
        self.assertEqual([target["containers"]["background"], LEGACY["containers"]["background"]], attempted)
        self.assertEqual(self.upstreams_for(LEGACY), self.upstreams_only(self.ops.loaded_upstreams))
        self.assertTrue(all(not self.ops.inspect(name)["State"]["Running"] for name in attempted))
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])

    def test_main_cleanup_drains_and_verifies_removal(self):
        target = self.cfg.slot_view("a")
        name = target["containers"]["serving"]
        self.ops.drain_seconds[name] = 3
        self.ops.nginx_test_rc = 1
        run = self.ops.run
        removal_attempts = []

        def fail_remove(args, **kw):
            info = self.ops.inspect(name)
            if info and args == ["docker", "rm", info["Id"]]:
                self.assertFalse(info["State"]["Running"], "removal must wait for draining")
                removal_attempts.append(args)
                return subprocess.CompletedProcess(args, 0, "", "")  # No removal despite success.
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_remove):
            with self.assertRaises(deploy.DeployError):
                self.deploy()
        self.assertTrue(removal_attempts)
        self.assert_live_release(LEGACY)
        self.assertIn("cleanup incomplete", self.state()["in_progress"]["reason"])
        self.assertIn("still exists", self.state()["in_progress"]["reason"])
        self.assertFalse(self.ops.inspect(name)["State"]["Running"])

    def test_main_missing_create_identity_leaves_progress_for_manual_cleanup(self):
        target = self.cfg.slot_view("a")
        name = target["containers"]["serving"]
        run = self.ops.run

        def omit_identity(args, **kw):
            result = run(args, **kw)
            if args[:4] == ["docker", "create", "--name", name]:
                result.stdout = ""
            return result

        with mock.patch.object(self.ops, "run", omit_identity):
            with self.assertRaisesRegex(deploy.FallbackFailed, "did not identify"):
                self.deploy()
        self.assert_live_release(LEGACY)
        self.assertIsNotNone(self.ops.inspect(name))
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])

    def test_main_asset_command_timeout_records_failure_and_preserves_rollback(self):
        self.deploy()
        self.deploy(image="fixture@sha256:second")
        before = self.state()
        run = self.ops.run

        def timeout_install(args, **kw):
            if args[0] == "install":
                self.assertEqual("deploying", self.state()["in_progress"]["phase"])
                with mock.patch.object(deploy.subprocess, "run", side_effect=subprocess.TimeoutExpired(args, kw.get("timeout", 600))):
                    return deploy.Ops.run(self.ops, args, **kw)
            return run(args, **kw)

        self.ops.commands.clear()
        with mock.patch.object(self.ops, "run", timeout_install):
            with self.assertRaisesRegex(deploy.DeployError, "timed out.*install"):
                self.deploy(image="fixture@sha256:third")
        self.assertEqual(before, self.state())
        self.assert_live_release(before["current"])
        for container in before["previous"]["containers"].values():
            self.assertIsNotNone(self.ops.inspect(container))
        failed = self.history()[-1]
        self.assertEqual("failed", failed["result"])
        self.assertIn("timed out", failed["error"])
        self.assertFalse(self.created())

    def test_main_migration_timeout_cleans_one_off_and_records_failure(self):
        run = self.ops.run

        def timeout_migration(args, **kw):
            if (args[:3] == ["docker", "start", "--attach"]
                    and self.ops.containers[args[-1]]["cmd"][0] == "-migrate-only"):
                raise subprocess.TimeoutExpired(args, kw["timeout"])
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", timeout_migration):
            with self.assertRaisesRegex(deploy.DeployError, "timed out"):
                self.deploy()
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)
        self.assertFalse(any("oneoff" in name for name in self.ops.containers))
        failed, = self.history()
        self.assertEqual("failed", failed["result"])
        self.assertIn("timed out", failed["error"])

    def test_hold_records_preparation_timeout_and_interrupt_after_progress_is_written(self):
        run = self.ops.run
        initial = self.state()
        for failure in (subprocess.TimeoutExpired([], 1), KeyboardInterrupt()):
            with self.subTest(failure=type(failure).__name__):
                deploy.save_state(self.cfg, initial)

                def fail(args, **kw):
                    if args[0] == "install":
                        raise failure
                    return run(args, **kw)

                with mock.patch.object(self.ops, "run", fail):
                    with self.assertRaises(deploy.FallbackFailed):
                        self.deploy(hold_before_switch=True)
                self.assert_live_release(LEGACY)
                self.assertEqual(LEGACY, self.state()["current"])
                reason = "timed out" if isinstance(failure, subprocess.TimeoutExpired) else "KeyboardInterrupt"
                self.assertIn(reason, self.state()["in_progress"]["reason"])
                self.assertEqual("failed", self.history()[-1]["result"])
                self.assertFalse(self.created())

    def test_watch_detects_abandoned_deploy_and_rejects_reused_process_identity(self):
        class ProcessKilled(BaseException):
            pass

        pid = os.getpid()
        stat = f"/proc/{pid}/stat"
        boot = "/proc/sys/kernel/random/boot_id"

        def process_stat(start, state="R"):
            return f"{pid} (release (fixture)) {state} " + "0 " * 18 + f"{start} 0\n"

        self.ops.files[boot] = "fixture-boot\n"
        self.ops.files[stat] = process_stat("first-start")
        run = self.ops.run

        def interrupt_after_mark(args, **kw):
            if args[0] == "install":
                self.assertEqual("deploying", self.state()["in_progress"]["phase"])
                self.assertTrue(deploy.status(self.cfg, self.ops)["in_progress"]["process_running"])
                self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
                raise ProcessKilled
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", interrupt_after_mark):
            with self.assertRaises(ProcessKilled):
                self.deploy()
        progress = self.state()["in_progress"]
        self.assertEqual("deploying", progress["phase"])
        self.assertEqual("first-start", progress["process"]["started_at_tick"])
        self.assertEqual([], self.history())
        for process_status in (None, process_stat("later-start"), process_stat("first-start", "Z")):
            with self.subTest(process_status=process_status):
                if process_status is None:
                    del self.ops.files[stat]
                else:
                    self.ops.files[stat] = process_status
                result = deploy.watch_once(self.cfg, self.ops, {})
                self.assertEqual(["release_incomplete"], result[0]["alert"])
                self.assertFalse(deploy.status(self.cfg, self.ops)["in_progress"]["process_running"])
        self.ops.files[stat] = process_stat("first-start")
        self.ops.files[boot] = "another-boot\n"
        self.assertEqual(["release_incomplete"], deploy.watch_once(self.cfg, self.ops, {})[0]["alert"])
        self.assertEqual(progress, self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_hold_keeps_traffic_background_and_internal_alias_on_previous_release(self):
        upstream = self.ops.files[self.cfg.upstream_file]
        paused = self.hold()
        self.assertEqual("held", paused["phase"])
        self.assertEqual("new@sha256:2", paused["target"]["image"])
        self.assertIn("held_at", paused)
        self.assertNotIn("paused", self.state())
        self.assertEqual(set(deploy.SERVING_ROLES), set(paused["containers"]))
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual(upstream, self.ops.files[self.cfg.upstream_file])
        self.assertFalse(self.ops.switched)
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertNotIn("sources-main-a-background", self.ops.containers)
        for role in deploy.SERVING_ROLES:
            container = self.ops.containers[paused["target"]["containers"][role]]
            self.assertTrue(container["State"]["Running"])
            self.assertEqual("serving", container["role"])
            for endpoint in container["NetworkSettings"]["Networks"].values():
                self.assertNotIn(self.cfg.serving_alias, endpoint["Aliases"])
            self.assertIn(f"http://127.0.0.1:{paused['target']['ports'][role]}/", "\n".join(self.ops.logs))
        self.assertEqual("paused", self.history()[-1]["result"])
        self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))

    def test_hold_startup_and_serving_failure_do_not_create_a_pause(self):
        for failure in ("startup", "serving"):
            with self.subTest(failure=failure):
                self.ops.check_fails = failure == "startup"
                self.ops.unready_ports = {self.cfg.slots["a"]["standby"]} if failure == "serving" else set()
                with self.assertRaises(deploy.DeployError):
                    self.deploy(hold_before_switch=True)
                self.assertIsNone(self.state()["in_progress"])
                self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
                self.assertNotIn("sources-main-a-serving", self.ops.containers)

    def test_hold_refuses_contract_migrations_even_when_allowed(self):
        self.ops.pending = "change_contract.sql"
        with self.assertRaisesRegex(deploy.DeployError, "additive migrations"):
            self.deploy(hold_before_switch=True, allow_contract=True)
        self.assertFalse(self.ops.migrated)
        self.assertEqual([], self.created())

    def test_resume_and_normal_deploy_call_the_same_switch_pipeline(self):
        real = deploy.Deployer._finish_switch
        with mock.patch.object(deploy.Deployer, "_finish_switch", autospec=True, side_effect=real) as finish:
            self.deploy()
            self.hold()
            self.assertEqual(1, finish.call_count, "hold stops before the shared switch pipeline")
            deploy.Deployer(self.cfg, self.ops).resume()
            self.assertEqual(2, finish.call_count)
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("b", self.state()["current"]["slot"])
        self.assertEqual("resume", self.history()[-1]["action"])
        self.assertEqual("ok", self.history()[-1]["result"])
        self.assertEqual(["background handoff", "switch", "guard", "retire previous serving"],
                         [step["step"] for step in self.history()[-1]["steps"]][-4:])
        for role in deploy.SERVING_ROLES:
            container = self.ops.containers[f"sources-main-b-{role}"]
            for endpoint in container["NetworkSettings"]["Networks"].values():
                self.assertIn(self.cfg.serving_alias, endpoint["Aliases"])

    def test_resume_uses_the_recorded_image_id_for_background_not_a_moved_tag(self):
        real = self.ops.run

        def run(args, **kw):
            result = real(args, **kw)
            if args[:2] == ["docker", "create"] and args[-1] == "local:tag":
                self.ops.containers[args[args.index("--name") + 1]]["Image"] = "sha256:held-image"
            return result

        self.ops.run = run
        self.deploy("local:tag", hold_before_switch=True)
        self.ops.local_images.add("local:tag")  # The reference may now point anywhere.
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("sha256:held-image", self.ops.containers["sources-main-a-background"]["Config"]["Image"])
        self.assertEqual("local:tag", self.state()["current"]["image"])

    def test_resume_validation_failures_keep_the_pause_and_do_not_mutate(self):
        paused = self.hold()
        name = paused["target"]["containers"]["serving"]
        original = json.loads(json.dumps(self.ops.containers[name]))
        failures = ("missing", "identity", "image", "stopped", "restarting", "unready", "wrong-role", "owner", "draining")
        for failure in failures:
            with self.subTest(failure=failure):
                self.ops.containers[name] = json.loads(json.dumps(original))
                self.ops.unready_ports.clear()
                container = self.ops.containers[name]
                if failure == "missing":
                    del self.ops.containers[name]
                elif failure == "identity":
                    container["Id"] = "replacement-id"
                elif failure == "image":
                    container["Image"] = "sha256:different-image"
                elif failure == "stopped":
                    container["State"]["Running"] = False
                elif failure == "restarting":
                    container["State"]["Restarting"] = True
                elif failure == "unready":
                    self.ops.unready_ports.add(container["port"])
                elif failure == "wrong-role":
                    container["role"] = "background"
                runtime = deploy.Deployer.runtime

                def runtime_reply(deployer, port, token):
                    reply = runtime(deployer, port, token)
                    if reply and port == original["port"]:
                        if failure == "owner":
                            reply["owns_background"] = True
                        if failure == "draining":
                            reply["draining"] = True
                    return reply

                self.ops.commands.clear()
                with mock.patch.object(deploy.Deployer, "runtime", runtime_reply):
                    with self.assertRaises(deploy.DeployError):
                        deploy.Deployer(self.cfg, self.ops).resume()
                self.assertEqual([], self.ops.commands)
                self.assertEqual(paused, self.state()["in_progress"])
                self.assertFalse(self.ops.switched)
        self.ops.containers[name] = original
        self.ops.unready_ports.clear()
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_resume_rejects_changed_configuration_routing_or_current_owner(self):
        self.hold()
        original = self.ops.files[self.cfg.upstream_file]
        self.ops.files[self.cfg.upstream_file] = "changed upstream"
        with self.assertRaisesRegex(deploy.DeployError, "upstream file changed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.ops.files[self.cfg.upstream_file] = original
        self.cfg.serving_alias = "changed-alias"
        with self.assertRaisesRegex(deploy.DeployError, "configuration changed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.cfg.serving_alias = "sources-main-serving"
        self.ops.containers["old-bg"]["State"]["Running"] = False
        with self.assertRaises(deploy.DeployError):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("held", self.state()["in_progress"]["phase"])

    def test_resume_rechecks_nginx_memory_and_public_health_without_mutating(self):
        held = self.hold()
        for failure in ("fixed upstream names", "MemAvailable", "public health check"):
            with self.subTest(failure=failure):
                self.ops.public_status = 503 if failure == "public health check" else 200
                self.ops.nginx_dump = "" if failure == "fixed upstream names" else FIXED_NGINX
                before = len(self.ops.commands)
                available = GIB if failure == "MemAvailable" else self.ops.meminfo_available()
                with mock.patch.object(self.ops, "meminfo_available", return_value=available):
                    with self.assertRaisesRegex(deploy.DeployError, failure):
                        deploy.Deployer(self.cfg, self.ops).resume()
                self.assertEqual([[self.cfg.nginx_bin, "-T"]], self.ops.commands[before:])
                self.assertEqual(held, self.state()["in_progress"])
                self.assert_live_release(LEGACY)

    def test_resume_honors_public_health_override_recorded_at_hold(self):
        self.deploy(hold_before_switch=True, allow_public_health_failure=True)
        self.ops.public_status = 503
        self.ops.public_status_after_reload = 200
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_background_failure_restores_old_owner_and_alerts(self):
        target = self.hold()["target"]
        self.ops.unready_ports.add(self.cfg.slots["a"]["background"])
        with self.assertRaisesRegex(deploy.DeployError, "previous background .* running again"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(target)
        self.assert_release_failure("failed", "not ready as background")
        self.assertFalse(self.ops.switched)

    def test_resume_guard_failure_restores_traffic_and_background(self):
        self.cfg.serving_alias = ""
        self.hold()
        self.ops.memory_after_switch = 20 * GIB
        with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_release_failure("failed", "guard failed")
        self.assertEqual(LEGACY, self.state()["current"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(self.cfg.slot_view("a"))

    def test_resume_guard_entry_interrupt_rolls_back_both_loaded_interfaces(self):
        self.cfg.serving_alias = ""
        target = self.hold()["target"]
        log = self.ops.log
        interrupted = []

        def interrupt(message):
            if message.endswith("== guard") and not interrupted:
                self.assert_loaded_interfaces(target, target)
                interrupted.append(self.state()["current"]["slot"])
                raise KeyboardInterrupt
            log(message)

        with mock.patch.object(self.ops, "log", interrupt):
            with self.assertRaises(deploy.DeployError):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual([target["slot"]], interrupted)
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_live_release(LEGACY)
        self.assert_no_shared_serving(target)
        self.assert_release_failure("failed", "KeyboardInterrupt")
        self.assertEqual(LEGACY, self.state()["current"])

    def test_resume_switch_exit_interrupt_restores_original_release(self):
        self.deploy()
        original = self.state()["current"]
        target = self.hold()["target"]
        runner = deploy.Deployer(self.cfg, self.ops)
        step = runner.step
        observed = []

        @contextlib.contextmanager
        def interrupt_on_exit(name):
            with step(name):
                yield
            if name == "switch" and not observed:
                self.assert_loaded_interfaces(target, target)
                observed.append(self.state()["current"]["slot"])
                raise KeyboardInterrupt

        with mock.patch.object(runner, "step", interrupt_on_exit):
            with self.assertRaises(deploy.DeployError):
                runner.resume()
        self.assert_live_release(original)
        self.assertEqual([target["slot"]], observed)
        self.assertEqual(original, self.state()["current"])
        self.assert_release_failure("failed", "KeyboardInterrupt")

    def test_resume_interrupt_after_route_proof_before_record_restores_original_release(self):
        self.deploy()
        original = self.state()["current"]
        target = self.hold()["target"]
        runner = deploy.Deployer(self.cfg, self.ops)
        record = runner.record_live_route
        interrupted = []

        def interrupt_before_record(state, route):
            if route.serving["slot"] == target["slot"] and not interrupted:
                self.assert_loaded_interfaces(target, target)
                self.assertEqual(original, self.state()["current"])
                interrupted.append(route.revision)
                raise KeyboardInterrupt
            record(state, route)

        with mock.patch.object(runner, "record_live_route", interrupt_before_record):
            with self.assertRaises(deploy.DeployError):
                runner.resume()
        self.assertTrue(interrupted)
        self.assert_live_release(original)
        self.assertEqual(original, self.state()["current"])
        self.assert_release_failure("failed", "KeyboardInterrupt")

    def test_resume_failed_current_write_restores_verified_original_route(self):
        self.cfg.serving_alias = ""
        target = self.hold()["target"]
        save = deploy.save_state
        attempted = []

        def fail(cfg, state):
            if state["current"]["slot"] == target["slot"]:
                attempted.append((state.get("in_progress") or {}).get("step", "completed"))
                self.assert_loaded_interfaces(target, target)
                raise OSError("fixture current write failed")
            save(cfg, state)

        with mock.patch.object(deploy, "save_state", fail):
            with self.assertRaisesRegex(deploy.DeployError, "fixture current write failed"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(["switch"], attempted)
        self.assert_live_release(LEGACY)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assert_release_failure("failed")

    def test_resume_unrestored_failure_is_never_reported_as_a_pause(self):
        self.hold()
        self.ops.drain_seconds["old-bg"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])
        self.assertTrue(any("release_incomplete" in alert["alert"]
                            for alert in deploy.watch_once(self.cfg, self.ops, {})))
        runner = deploy.Deployer(self.cfg, self.ops)
        with self.assertRaisesRegex(deploy.DeployError, "not a pause"):
            runner.resume()
        with self.assertRaisesRegex(deploy.DeployError, "still draining"):
            runner.abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])

    def test_abort_failed_resume_verifies_original_route_before_removing_held_pair(self):
        held = self.hold()
        self.ops.nginx_test_rc = 1
        runner = deploy.Deployer(self.cfg, self.ops)
        with self.assertRaises(deploy.DeployError):
            runner.resume()
        self.assert_release_failure("failed", "nginx -t rejected")
        self.ops.nginx_test_rc = 0
        discover = self.ops.probe_nginx_serving
        with mock.patch.object(self.ops, "probe_nginx_serving", return_value=(0, None, None)):
            with self.assertRaisesRegex(deploy.DeployError, "route generations could not be verified"):
                runner.abort()
        for role in deploy.SERVING_ROLES:
            self.assertIsNotNone(self.ops.inspect(held["target"]["containers"][role]))
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertTrue(any("abort_incomplete" in row["alert"] for row in deploy.watch_once(self.cfg, self.ops, {})))
        with mock.patch.object(self.ops, "probe_nginx_serving", wraps=discover) as probe:
            runner.abort()
        self.assertTrue(probe.called)
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        for role in deploy.SERVING_ROLES:
            self.assertIsNone(self.ops.inspect(held["target"]["containers"][role]))
        self.assertIsNone(self.state()["in_progress"])

    def test_abort_removes_only_held_serving_and_keeps_migrations(self):
        self.hold()
        upstream = self.ops.files[self.cfg.upstream_file]
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(set(LEGACY["containers"].values()), set(self.ops.containers))
        self.assertTrue(self.ops.migrated)
        self.assertEqual(upstream, self.ops.files[self.cfg.upstream_file])
        self.assertEqual("abort", self.history()[-1]["action"])
        self.assertFalse(any(c[:3] == ["docker", "rm", "-f"] for c in self.ops.commands[-6:]))

    def test_abort_partial_failure_is_durable_and_retryable(self):
        paused = self.hold()
        real = self.ops.run

        def fail_remove(args, **kw):
            if args == ["docker", "rm", paused["containers"]["standby"]["id"]]:
                raise deploy.DeployError("simulated removal failure")
            return real(args, **kw)

        with mock.patch.object(self.ops, "run", fail_remove):
            with self.assertRaisesRegex(deploy.DeployError, "retry 'abort'"):
                deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])
        self.assert_live_release(LEGACY)
        self.assertIn(["abort_incomplete"], [alert["alert"] for alert in deploy.watch_once(self.cfg, self.ops, {})])
        self.assertNotIn("sources-main-a-serving", self.ops.containers)
        self.assertIn("sources-main-a-standby", self.ops.containers)
        with self.assertRaisesRegex(deploy.DeployError, "cleanup has started"):
            deploy.Deployer(self.cfg, self.ops).resume()
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_abort_refuses_a_replacement_container(self):
        paused = self.hold()
        name = paused["target"]["containers"]["serving"]
        self.ops.containers[name]["Id"] = "unrelated-id"
        with self.assertRaisesRegex(deploy.DeployError, "unrelated container"):
            deploy.Deployer(self.cfg, self.ops).abort()
        self.assertTrue(self.ops.containers[name]["State"]["Running"])
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])

    def test_abort_timeout_waits_without_killing_and_can_retry_after_exit(self):
        self.hold()
        self.ops.drain_seconds["sources-main-a-serving"] = float("inf")
        with self.assertRaisesRegex(deploy.DeployError, "nothing was killed"):
            deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.ops.containers["sources-main-a-serving"]["State"]["Running"] = False
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_abort_signals_serving_already_marked_draining(self):
        held = self.hold()
        port = held["target"]["ports"]["serving"]
        cid = held["containers"]["serving"]["id"]
        serving = held["target"]["containers"]["serving"]
        self.ops.drain_seconds[serving] = 3
        token = self.ops.read_text(self.cfg.runtime_token_file).strip()
        url = f"http://loopback.invalid:{port}/internal/sources/drain"
        self.assertEqual(200, self.ops.http_json(url, token=token, method="POST", body={"instance_id": cid})[0])
        self.assertTrue(self.ops.inspect(cid)["State"]["Running"])
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).runtime(port, token)["draining"])
        run = self.ops.run

        def verify_graceful_removal(args, **kw):
            if args[:2] == ["docker", "rm"] and args[-1] == cid:
                self.assertEqual(["docker", "rm", cid], args)
                self.assertFalse(self.ops.inspect(cid)["State"]["Running"])
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", verify_graceful_removal):
            deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIn(["docker", "kill", "--signal=TERM", cid], self.ops.commands)
        self.assertIsNone(self.ops.inspect(cid))
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_watch_alerts_after_abort_is_interrupted_before_cleanup(self):
        class ProcessKilled(BaseException):
            pass

        pid = os.getpid()
        stat = f"/proc/{pid}/stat"
        self.ops.files["/proc/sys/kernel/random/boot_id"] = "fixture-boot\n"
        self.ops.files[stat] = f"{pid} (release) R " + "0 " * 20
        self.deploy(hold_before_switch=True)
        run = self.ops.run

        def interrupt_before_cleanup(args, **kw):
            if args[:3] == ["docker", "update", "--restart=no"]:
                self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
                raise ProcessKilled
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", interrupt_before_cleanup):
            with self.assertRaises(ProcessKilled):
                deploy.Deployer(self.cfg, self.ops).abort()
        phase = self.state().get("in_progress") or self.state().get("paused")
        self.assertEqual("aborting", phase["phase"])
        self.assertNotIn("reason", phase)
        self.assertNotIn("failed", phase)
        del self.ops.files[stat]
        self.assertEqual([["abort_incomplete"]], [item["alert"] for item in deploy.watch_once(self.cfg, self.ops, {})])
        self.assert_live_release(LEGACY)
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))

    def test_hold_blocks_main_and_rollback_and_commands_require_a_pause(self):
        for action in ("resume", "abort"):
            with self.assertRaisesRegex(deploy.DeployError, "no paused release"):
                getattr(deploy.Deployer(self.cfg, self.ops), action)()
        self.hold()
        self.ops.commands.clear()
        for action in (lambda: self.deploy(), lambda: deploy.Deployer(self.cfg, self.ops).rollback()):
            with self.assertRaisesRegex(deploy.DeployError, "resume.*abort"):
                action()
        self.assertEqual([], self.ops.commands)

    def test_status_reports_age_and_warning_without_expiring_the_pause(self):
        self.hold()
        state = self.state()
        state["in_progress"]["held_at"] = "2000-01-01T00:00:00+00:00"
        deploy.save_state(self.cfg, state)
        report = deploy.status(self.cfg, self.ops)
        self.assertGreater(report["in_progress"]["age_seconds"], self.cfg.hold_warning_seconds)
        self.assertTrue(report["in_progress"]["warning"])
        self.assertEqual("held", report["in_progress"]["phase"])
        self.assertTrue(report["in_progress"]["live_containers"]["serving"]["image_matches"])
        self.assertEqual(state, self.state())

    def test_host_reboot_retains_pause_until_original_containers_return(self):
        paused = self.hold()
        state_on_disk = Path(self.cfg.state_file).read_bytes()
        daemon = FakeOps(self.tmp)
        daemon.files = dict(self.ops.files)
        daemon.loaded_upstreams = self.ops.loaded_upstreams
        daemon.upstream_file = self.cfg.upstream_file
        daemon.containers = json.loads(json.dumps(self.ops.containers))
        running = [name for name, c in daemon.containers.items() if c["State"]["Running"]]
        for c in daemon.containers.values():
            c["State"].update(Running=False, Status="exited")
        self.ops = daemon
        self.assertEqual("exited", deploy.status(self.cfg, daemon)["in_progress"]["live_containers"]["serving"]["status"])
        with self.assertRaisesRegex(deploy.DeployError, "exited"):
            deploy.Deployer(self.cfg, daemon).resume()
        self.assertEqual([], daemon.commands)
        self.assertEqual(state_on_disk, Path(self.cfg.state_file).read_bytes())
        for name in running:
            daemon.run(["docker", "start", name], mutate=True)
        self.assert_live_release(LEGACY)
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(paused["target"])
        self.assertEqual(paused["target"]["slot"], self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_dry_run_hold_resume_and_abort_preserve_state_history_and_containers(self):
        self.ops.dry_run = True
        before = Path(self.cfg.state_file).read_text()
        self.deploy(hold_before_switch=True)
        self.assertEqual(before, Path(self.cfg.state_file).read_text())
        self.assertFalse(self.ops.switched)
        self.assertEqual([], self.history())
        self.assertTrue(any(cmd[:2] == ["docker", "pull"] for cmd in self.ops.commands))
        checks = [cmd for cmd in self.ops.commands if cmd[:2] == ["docker", "create"]]
        self.assertTrue(checks)
        for cmd in checks:
            name = cmd[cmd.index("--name") + 1]
            self.assertIn(["docker", "start", "--attach", name], self.ops.commands)
            self.assertIn(["docker", "rm", "-f", name], self.ops.commands)
        self.ops.dry_run = False
        self.hold()
        before = Path(self.cfg.state_file).read_text()
        containers = json.loads(json.dumps(self.ops.containers))
        history = self.history()
        self.ops.dry_run = True
        for action in ("resume", "abort"):
            getattr(deploy.Deployer(self.cfg, self.ops), action)()
            self.assertEqual(before, Path(self.cfg.state_file).read_text())
            self.assertEqual(containers, self.ops.containers)
            self.assertEqual(history, self.history())

    def test_cli_routes_hold_resume_and_abort_under_release_lock(self):
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops):
            self.assertEqual(0, deploy.main(["main", "new@sha256:2", "--hold-before-switch"]))
            self.assertEqual(0, deploy.main(["resume"]))
            self.assertEqual(0, deploy.main(["main", "newer@sha256:3", "--hold-before-switch"]))
            self.assertEqual(0, deploy.main(["abort"]))

    def test_cli_hold_succeeds_when_history_is_unwritable(self):
        self.cfg.history_file = str(self.tmp)  # Opening a directory for append fails even as root.
        stderr = io.StringIO()
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                mock.patch.object(deploy, "append_history", wraps=deploy.append_history) as history, \
                contextlib.redirect_stderr(stderr):
            self.assertEqual(0, deploy.main(["main", "fixture-image:held", "--hold-before-switch"]))
        self.assertEqual("held", self.state()["in_progress"]["phase"])
        self.assert_live_release(LEGACY)
        history.assert_called_once()
        self.assertEqual("paused", history.call_args.args[1]["result"])
        self.assertEqual(1, len(stderr.getvalue().splitlines()))
        self.assertIn("warning", stderr.getvalue())
        output = "\n".join(self.ops.logs) + stderr.getvalue()
        self.assertIn("'resume' or 'abort'", output)
        self.assertNotIn("Traceback", output)

    def test_cli_switch_keeps_committed_state_when_history_is_unwritable(self):
        held = self.hold()
        history = Path(self.cfg.history_file)
        history.unlink()
        history.mkdir()
        stderr = io.StringIO()
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                mock.patch.object(deploy, "append_history", wraps=deploy.append_history) as append, \
                contextlib.redirect_stderr(stderr):
            self.assertEqual(0, deploy.main(["resume"]))
        self.assert_live_release(held["target"])
        self.assertEqual(held["target"]["slot"], self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])
        append.assert_called_once()
        self.assertEqual("ok", append.call_args.args[1]["result"])
        self.assertEqual(1, len(stderr.getvalue().splitlines()))
        self.assertIn("warning", stderr.getvalue())

    def test_cli_release_failure_keeps_original_error_when_history_is_unwritable(self):
        self.cfg.history_file = str(self.tmp)
        self.ops.nginx_test_rc = 1
        stderr = io.StringIO()
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                mock.patch.object(deploy, "append_history", wraps=deploy.append_history) as history, \
                contextlib.redirect_stderr(stderr):
            self.assertEqual(1, deploy.main(["main", "fixture-image:nginx-fails"]))
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])
        history.assert_called_once()
        self.assertEqual("failed", history.call_args.args[1]["result"])
        self.assertIn("warning", stderr.getvalue())
        self.assertIn("nginx -t rejected", stderr.getvalue())

    def test_cli_unexpected_error_reports_next_step_from_saved_state(self):
        held = self.hold()
        for phase, next_step in (
            ("held", "'resume' or 'abort'"),
            ("aborting", "retry 'abort'"),
            ("failed", "'abort' to verify the original route"),
            ("deploying", "verify routing and background ownership"),
        ):
            with self.subTest(phase=phase):
                deploy.save_state(self.cfg, {**self.state(), "in_progress": {**held, "phase": phase}})
                before = self.state()
                stderr = io.StringIO()
                with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                        mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                        mock.patch.object(deploy.Deployer, "resume", side_effect=RuntimeError("fixture\nerror")), \
                        contextlib.redirect_stderr(stderr):
                    self.assertEqual(1, deploy.main(["resume"]))
                self.assertEqual(before, self.state())
                self.assertEqual(1, len(stderr.getvalue().splitlines()))
                self.assertIn("RuntimeError", stderr.getvalue())
                self.assertIn(next_step, stderr.getvalue())
                self.assertNotIn("Traceback", stderr.getvalue())

    def test_cli_unreadable_state_reports_recovery_hint_without_traceback(self):
        path = Path(self.cfg.state_file)
        path.write_text("invalid state")
        stderr = io.StringIO()
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                contextlib.redirect_stderr(stderr):
            self.assertEqual(1, deploy.main(["status"]))
        self.assertEqual("invalid state", path.read_text())
        self.assertEqual(1, len(stderr.getvalue().splitlines()))
        self.assertIn("state file", stderr.getvalue())
        self.assertNotIn("Traceback", stderr.getvalue())

    def test_cli_held_dry_run_does_not_create_a_lock_file_or_state(self):
        lock = self.tmp / "missing-lock-directory" / "release.lock"
        self.cfg.lock_file = str(lock)
        self.hold()
        lock.unlink()
        lock.parent.rmdir()
        state_bytes = Path(self.cfg.state_file).read_bytes()
        history = self.history()
        self.ops.dry_run = True
        with mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops):
            for command in ("resume", "abort"):
                self.assertEqual(0, deploy.main(["--dry-run", command]))
        self.assertFalse(lock.parent.exists())
        self.assertEqual(state_bytes, Path(self.cfg.state_file).read_bytes())
        self.assertEqual(history, self.history())
        self.assertIn(["DRY", self.cfg.nginx_bin, "-T"], self.ops.commands)

    def test_legacy_pause_is_read_without_migration_and_consumed_by_resume(self):
        held = self.hold()
        legacy = {key: value for key, value in held.items() if key not in ("held_at", "allow_public_health_failure")}
        legacy.update(phase="ready", since=held["held_at"])
        deploy.save_state(self.cfg, {**self.state(), "paused": legacy, "in_progress": None})
        disk = Path(self.cfg.state_file).read_bytes()
        report = deploy.status(self.cfg, self.ops)
        self.assertEqual("held", report["in_progress"]["phase"])
        self.assertEqual(held["held_at"], report["in_progress"]["held_at"])
        self.assertEqual(disk, Path(self.cfg.state_file).read_bytes())
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertNotIn("paused", self.state())

    def test_resume_checks_all_networks_before_touching_held_containers(self):
        held = self.hold()
        containers = json.loads(json.dumps(self.ops.containers))
        run = self.ops.run
        network_checks = []

        def missing_network(args, **kw):
            if args[:3] == ["docker", "network", "inspect"]:
                network_checks.append(args[3:])
                raise deploy.DeployError("simulated network disappeared")
            self.assertFalse(kw["mutate"], args)
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", missing_network):
            with self.assertRaisesRegex(deploy.DeployError, "network disappeared.*pause unchanged"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual([self.cfg.networks], network_checks)
        self.assertEqual(containers, self.ops.containers)
        self.assertEqual(held, self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_resume_accepts_network_ids_when_container_inspect_uses_names(self):
        networks = tuple(self.cfg.networks)
        self.cfg.networks = [self.ops.network_ids[network] for network in networks]
        held = self.hold()
        for role in deploy.SERVING_ROLES:
            name = held["target"]["containers"][role]
            self.assertEqual(set(networks), set(self.ops.inspect(name)["NetworkSettings"]["Networks"]))
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(held["target"]["slot"], self.state()["current"]["slot"])
        self.assert_live_release(held["target"])
        self.assertIsNone(self.state()["in_progress"])
        for role in deploy.SERVING_ROLES:
            networks_after = self.ops.inspect(held["target"]["containers"][role])["NetworkSettings"]["Networks"]
            self.assertEqual(set(networks), set(networks_after))
            self.assertTrue(all(self.cfg.serving_alias in endpoint["Aliases"] for endpoint in networks_after.values()))

    def test_resume_preserves_static_ipam_when_reconnecting_held_endpoints(self):
        target = self.hold()["target"]
        original = {}
        for role in deploy.SERVING_ROLES:
            name = target["containers"][role]
            networks = self.ops.containers[name]["NetworkSettings"]["Networks"]
            for endpoint in networks.values():
                endpoint["IPAMConfig"] = {"IPv4Address": f"fixture-ipv4-{role}",
                                          "IPv6Address": f"fixture-ipv6-{role}",
                                          "LinkLocalIPs": [f"fixture-link-local-{role}"]}
                endpoint["Aliases"].append(f"fixture-alias-{role}")
            original[role] = json.loads(json.dumps(networks))
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(target, target)
        for role, networks in original.items():
            for endpoint in networks.values():
                endpoint["Aliases"].append(self.cfg.serving_alias)
            self.assertEqual(networks, self.ops.inspect(target["containers"][role])["NetworkSettings"]["Networks"])

    def test_resume_inspect_timeout_leaves_pause_retryable_and_abortable(self):
        held = self.hold()
        inspect = self.ops.inspect
        before = list(self.ops.commands)

        def timeout(name):
            if name == held["target"]["containers"]["serving"]:
                raise subprocess.TimeoutExpired("docker inspect", 1)
            return inspect(name)

        with mock.patch.object(self.ops, "inspect", timeout):
            with self.assertRaisesRegex(deploy.DeployError, "timed out.*retry or abort"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(before, self.ops.commands)
        self.assertEqual(held, self.state()["in_progress"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_release_failure("held")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_nginx_failure_hands_background_back(self):
        target = self.hold()["target"]
        self.ops.nginx_test_rc = 1
        with self.assertRaisesRegex(deploy.DeployError, "owns the background again"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(LEGACY)
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(target)
        self.assert_release_failure("failed", "nginx -t rejected")

    def test_resume_partial_alias_activation_drains_and_restores_pause(self):
        paused = self.hold()
        names = set(paused["target"]["containers"][role] for role in deploy.SERVING_ROLES)
        ids = {info["id"] for info in paused["containers"].values()}
        self.ops.drain_seconds.update(dict.fromkeys(names, 3))
        before = len(self.ops.commands)
        real = self.ops.run
        alias_joins = []
        exposed_while_joining = []

        def fail(args, **kw):
            if args[:3] == ["docker", "network", "connect"] and self.cfg.serving_alias in args:
                alias_joins.append(self.ops.network_name(args[-2]))
                for name in names:
                    info = self.ops.inspect(name)
                    if info["State"]["Running"] and any(
                            self.cfg.serving_alias in ep["Aliases"]
                            for ep in info["NetworkSettings"]["Networks"].values()):
                        exposed_while_joining.append(name)
                if len(alias_joins) == len(self.cfg.networks):
                    raise deploy.DeployError("simulated network failure after an alias was staged")
            if args[:3] == ["docker", "network", "disconnect"] and args[-1] in ids:
                self.assertFalse(self.ops.inspect(args[-1])["State"]["Running"], "drain before disconnect")
            return real(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.DeployError, "network failure"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(self.cfg.networks, alias_joins)
        self.assertEqual([], exposed_while_joining)
        self.assertNotIn(["docker", "rm", "-f"], [cmd[:3] for cmd in self.ops.commands[before:]
                                                 if cmd[-1] in names | ids])
        self.assert_live_release(LEGACY)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual(paused, self.state()["in_progress"])
        deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_network_timeout_reconciles_unknown_effect_before_restoring_pause(self):
        paused = self.hold()
        real = self.ops.run
        timeouts = []

        def fail(args, **kw):
            if args[:3] == ["docker", "network", "disconnect"] and not timeouts:
                real(args, **kw)
                timeouts.append(args)
                raise subprocess.TimeoutExpired(args, 1)
            return real(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.DeployError, "pause restored"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(paused, self.state()["in_progress"])
        self.assert_live_release(LEGACY)
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_drains_already_activated_serving_if_standby_start_fails(self):
        paused = self.hold()
        serving_id, standby_id = (paused["containers"][role]["id"] for role in deploy.SERVING_ROLES)
        self.ops.drain_seconds[paused["target"]["containers"]["serving"]] = 3
        run = self.ops.run
        served = False

        def fail_start(args, **kw):
            nonlocal served
            if args == ["docker", "start", standby_id] and not served:
                info = self.ops.inspect(serving_id)
                self.assertTrue(info["State"]["Running"])
                self.assertTrue(all(self.cfg.serving_alias in ep["Aliases"]
                                    for ep in info["NetworkSettings"]["Networks"].values()))
                served = True
                raise deploy.DeployError("simulated standby start failure")
            if served and args[:3] == ["docker", "network", "disconnect"] and args[-1] == serving_id:
                self.assertFalse(self.ops.inspect(serving_id)["State"]["Running"])
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_start):
            with self.assertRaisesRegex(deploy.DeployError, "pause restored"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertTrue(served)
        self.assert_live_release(LEGACY)
        self.assertEqual(paused, self.state()["in_progress"])

    def test_resume_rechecks_background_port_after_activation(self):
        paused = self.hold()
        busy_port = paused["target"]["ports"]["background"]
        run = self.ops.run

        def port_becomes_busy(args, **kw):
            if args == ["docker", "start", paused["containers"]["standby"]["id"]]:
                self.ops.busy_ports.add(busy_port)
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", port_becomes_busy):
            with self.assertRaisesRegex(deploy.DeployError, "already in use.*pause restored"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(LEGACY)
        self.assertEqual(paused, self.state()["in_progress"])
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assert_no_shared_serving(paused["target"])
        self.assert_release_failure("held")

    def test_abort_command_errors_and_interruptions_keep_retryable_cleanup(self):
        self.hold()
        for error in (OSError("simulated command error"), subprocess.TimeoutExpired("docker", 1), KeyboardInterrupt()):
            with self.subTest(error=type(error).__name__):
                real = self.ops.run

                def fail(args, **kw):
                    if args[:2] == ["docker", "update"]:
                        raise error
                    return real(args, **kw)

                with mock.patch.object(self.ops, "run", fail):
                    with self.assertRaisesRegex(deploy.DeployError, "retry 'abort'"):
                        deploy.Deployer(self.cfg, self.ops).abort()
                self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_resume_rejects_network_alias_added_while_paused(self):
        held = self.hold()
        before = Path(self.cfg.state_file).read_bytes()
        endpoint = self.ops.containers["sources-main-a-serving"]["NetworkSettings"]["Networks"][self.cfg.networks[0]]
        endpoint["Aliases"].append(self.cfg.serving_alias)
        with self.assertRaisesRegex(deploy.DeployError, "network isolation changed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assertEqual(before, Path(self.cfg.state_file).read_bytes())
        self.assert_release_failure("held")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])
        for role in deploy.SERVING_ROLES:
            self.assertIsNone(self.ops.inspect(held["target"]["containers"][role]))
        self.assert_live_release(LEGACY)

    def test_resume_refuses_a_changed_current_release_or_unexpected_background(self):
        self.hold()
        state = self.state()
        state["current"]["image"] = "sha256:changed-current"
        deploy.save_state(self.cfg, state)
        with self.assertRaisesRegex(deploy.DeployError, "current release changed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        state["current"] = LEGACY
        deploy.save_state(self.cfg, state)
        self.ops.add("sources-main-a-background", self.cfg.slots["a"]["background"])
        for action in ("resume", "abort"):
            with self.assertRaisesRegex(deploy.DeployError, "unexpected target background"):
                getattr(deploy.Deployer(self.cfg, self.ops), action)()
        self.assertEqual("held", self.state()["in_progress"]["phase"])

    def test_abort_does_not_remove_current_release_even_if_state_was_edited(self):
        paused = self.hold()
        state = self.state()
        state["current"] = paused["target"]
        deploy.save_state(self.cfg, state)
        with self.assertRaisesRegex(deploy.DeployError, "overlap the current release"):
            deploy.Deployer(self.cfg, self.ops).abort()
        self.assertTrue(self.ops.containers["sources-main-a-serving"]["State"]["Running"])

    def test_abort_docker_unavailable_is_not_treated_as_missing_containers(self):
        self.hold()
        real = self.ops.inspect

        def inspect(name):
            if name.endswith("-serving"):
                raise deploy.DeployError("Docker state unknown")
            return real(name)

        with mock.patch.object(self.ops, "inspect", inspect):
            with self.assertRaisesRegex(deploy.DeployError, "retry 'abort'"):
                deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertIn("sources-main-a-serving", self.ops.containers)
        deploy.Deployer(self.cfg, self.ops).abort()

    def test_inspect_distinguishes_missing_container_from_unavailable_docker(self):
        ops = deploy.MainOps()
        for message in ("Cannot connect to Docker", "permission denied"):
            with mock.patch.object(ops, "run", return_value=subprocess.CompletedProcess([], 1, "", message)):
                with self.assertRaisesRegex(deploy.DeployError, "state is unknown"):
                    ops.inspect("placeholder-container")
        with mock.patch.object(ops, "run", return_value=subprocess.CompletedProcess([], 1, "", "Error: No such object: placeholder")):
            self.assertIsNone(ops.inspect("placeholder-container"))
        for bad_response in ("[]", "invalid JSON"):
            with mock.patch.object(ops, "run", return_value=subprocess.CompletedProcess([], 0, bad_response, "")):
                with self.assertRaisesRegex(deploy.DeployError, "state is unknown"):
                    ops.inspect("placeholder-container")

    def test_main_accepts_an_applied_route_even_when_reload_reports_failure(self):
        run = self.ops.run
        attempted = []

        def fail_reload(args, **kw):
            result = run(args, **kw)
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                attempted.append(args)
                raise subprocess.TimeoutExpired(args, 1)
            return result

        with mock.patch.object(self.ops, "run", fail_reload):
            self.deploy()
        self.assertEqual(1, len(attempted))
        self.assert_live_release(self.state()["current"])
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_main_accepts_a_verified_route_after_a_nonzero_reload(self):
        run = self.ops.run

        def fail_after_reload(args, **kw):
            result = run(args, **kw)
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                return subprocess.CompletedProcess(args, 1, "", "fixture reload failed after applying")
            return result

        with mock.patch.object(self.ops, "run", fail_after_reload):
            self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assert_live_release(self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])

    def test_main_rejected_reload_restores_background_when_old_route_is_confirmed(self):
        self.deploy()
        original = self.state()["current"]
        previous_file = self.ops.loaded_upstreams
        run = self.ops.run

        def reject(args, **kw):
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                if kw.get("check", True):
                    raise deploy.DeployError("fixture reload rejected")
                return subprocess.CompletedProcess(args, 1, "", "fixture reload rejected")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", reject):
            with self.assertRaises(deploy.DeployError):
                self.deploy(image="fixture:second")
        self.assert_live_release(original)
        self.assertEqual(previous_file, self.ops.files[self.cfg.upstream_file])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])
        for role in deploy.SERVING_ROLES:
            self.assertIsNone(self.ops.inspect(self.cfg.slot_view("b")["containers"][role]))

    def test_main_rejected_reload_with_no_route_proof_keeps_both_serving_pairs(self):
        self.deploy()
        original = self.state()["current"]
        target = self.cfg.slot_view("b")
        run = self.ops.run
        probe = self.ops.probe_nginx_serving
        rejected = False

        def reject(args, **kw):
            nonlocal rejected
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                rejected = True
                return subprocess.CompletedProcess(args, 1, "", "fixture reload rejected")
            return run(args, **kw)

        def missing_probe(*args):
            return (0, None, None) if rejected else probe(*args)

        with mock.patch.object(self.ops, "run", reject), \
                mock.patch.object(self.ops, "probe_nginx_serving", missing_probe):
            with self.assertRaises(deploy.RoutingUnconfirmed):
                self.deploy(image="fixture:second")
        for view in (original, target):
            for role in deploy.SERVING_ROLES:
                self.assertTrue(self.ops.inspect(view["containers"][role])["State"]["Running"])
        self.assertEqual(original, self.state()["current"])
        self.assert_release_failure("failed", "nginx reload failed with exit status 1")

    def test_main_failed_retirement_keeps_new_release_live_and_blocks_rollback(self):
        run = self.ops.run

        def fail(args, **kw):
            if args == ["docker", "update", "--restart=no", LEGACY["containers"]["serving"]]:
                raise deploy.DeployError("simulated retirement failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.FallbackFailed, "new release is serving"):
                self.deploy()
        progress = self.state()["in_progress"]
        self.assert_live_release(progress["target"])
        self.assertEqual("retire previous serving", progress["failed_step"])
        self.assertEqual("failed", progress["phase"])
        self.assertTrue(progress["reason"])
        self.assertEqual(progress["target"]["slot"], self.state()["current"]["slot"])
        before = len(self.ops.commands)
        with self.assertRaisesRegex(deploy.DeployError, "did not finish"):
            deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(before, len(self.ops.commands))
        with mock.patch.object(self.ops, "inspect", side_effect=deploy.DeployError("Docker unavailable")):
            report = deploy.status(self.cfg, self.ops)
            self.assertEqual(progress, report["in_progress"])
            self.assertEqual("unknown", report["current"]["containers"]["background"]["status"])
            reasons = [alert["alert"] for alert in deploy.watch_once(self.cfg, self.ops, {})]
            self.assertIn(["docker_inspect_failed"], reasons)
            self.assertIn(["release_incomplete"], reasons)

    def test_dry_run_resume_failure_keeps_pause_and_history(self):
        self.hold()
        before, history = self.state(), self.history()
        self.ops.dry_run = True
        self.ops.unready_ports.add(self.cfg.slots["a"]["serving"])
        with self.assertRaises(deploy.DeployError):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(before, self.state())
        self.assertEqual(history, self.history())

    def test_persistent_second_network_failure_keeps_hold_abortable(self):
        held = self.hold()
        network = self.ops.network_ids[self.cfg.networks[1]]
        run = self.ops.run

        def fail(args, **kw):
            if args[:3] == ["docker", "network", "connect"] and args[-2] == network:
                raise deploy.DeployError("simulated connection failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaisesRegex(deploy.DeployError, "'abort'"):
                deploy.Deployer(self.cfg, self.ops).resume()
            progress = self.state()["in_progress"]
            self.assertEqual("held", progress["phase"])
            self.assertEqual(held["containers"], progress["containers"])
            self.assertTrue(progress["reason"])
            self.assert_loaded_interfaces(LEGACY, LEGACY)
            self.assert_no_shared_serving(held["target"])
            self.assert_release_failure("held")
            deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_main_unconfirmed_reload_keeps_both_serving_pairs(self):
        run = self.ops.run

        def fail(args, **kw):
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                raise deploy.DeployError("simulated reload failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaises(deploy.RoutingUnconfirmed):
                self.deploy()
        self.assertEqual(self.upstreams_for(LEGACY), self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(LEGACY, self.state()["current"])
        self.assert_release_failure("failed", "reload outcome is unknown")
        for view in (LEGACY, self.cfg.slot_view("a")):
            for role in deploy.SERVING_ROLES:
                self.assertTrue(self.ops.inspect(view["containers"][role])["State"]["Running"])

    def test_main_missing_original_upstream_preserves_the_running_release(self):
        del self.ops.files[self.cfg.upstream_file]
        with self.assertRaisesRegex(deploy.FallbackFailed, "cannot read the previous upstream file"):
            self.deploy()
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertFalse(self.created())
        self.assert_release_failure("failed", "routing unchanged")

    def test_main_upstream_write_error_before_reload_restores_old_release(self):
        write = self.ops.write_file_atomic
        attempted = False

        def interrupted_write(path, text, **kw):
            nonlocal attempted
            write(path, text, **kw)
            if path == self.cfg.upstream_file and not attempted:
                attempted = True
                raise OSError("simulated failure after writing upstream")

        with mock.patch.object(self.ops, "write_file_atomic", interrupted_write):
            with self.assertRaisesRegex(deploy.DeployError, "previous file restored, routing unchanged"):
                self.deploy()
        self.assertTrue(attempted)
        self.assertFalse(self.ops.switched)
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

    def test_main_unreadable_upstream_keeps_routing_and_returns_background(self):
        read = self.ops.read_bytes

        def unreadable(path):
            if path == self.cfg.upstream_file:
                raise PermissionError("simulated upstream read failure")
            return read(path)

        with mock.patch.object(self.ops, "read_bytes", unreadable):
            with self.assertRaisesRegex(deploy.DeployError, "cannot read the previous upstream file"):
                self.deploy()
        self.assertFalse(self.ops.switched)
        self.assert_live_release(LEGACY)
        self.assert_release_failure("failed", "cannot read the previous upstream file")

    def test_pause_write_failure_preserves_failed_progress_and_running_target(self):
        real = deploy.save_state

        def fail_pause(cfg, state):
            if (state.get("in_progress") or {}).get("phase") == "held":
                raise OSError("simulated pause write failure")
            return real(cfg, state)

        with mock.patch.object(deploy, "save_state", fail_pause):
            with self.assertRaises(deploy.FallbackFailed):
                self.hold()
        self.assertEqual("save held release", self.state()["in_progress"]["failed_step"])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])
        self.assertTrue(self.ops.containers["sources-main-a-serving"]["State"]["Running"])
        self.assertFalse(self.ops.switched)

    def assert_recovery_after_hold_write_fails(self, command, *, checkpoint):
        save = deploy.save_state
        failures = []

        def fail_hold(cfg, state):
            progress = state.get("in_progress") or {}
            recording_hold = progress.get("phase") == "deploying" and progress.get("step") == "save held release"
            if not failures and (recording_hold if checkpoint else progress.get("phase") == "held"):
                failures.append(progress)
                raise OSError("fixture hold write failure")
            save(cfg, state)

        with mock.patch.object(deploy, "save_state", fail_hold):
            with self.assertRaises(deploy.DeployError) as failure:
                self.hold()
        self.assertEqual(1, len(failures))
        self.assert_loaded_interfaces(LEGACY, LEGACY)
        self.assertEqual(LEGACY, self.state()["current"])
        target = self.cfg.slot_view("a")
        self.assert_no_shared_serving(target)
        ids = {role: self.ops.inspect(target["containers"][role])["Id"] for role in deploy.SERVING_ROLES}
        progress = self.state()["in_progress"]
        runner = deploy.Deployer(self.cfg, self.ops)
        getattr(runner, command)()
        if command == "resume":
            self.assert_loaded_interfaces(target, target)
            self.assertEqual(target["slot"], self.state()["current"]["slot"])
            self.assertEqual(ids, {role: self.ops.inspect(target["containers"][role])["Id"] for role in deploy.SERVING_ROLES})
        else:
            self.assert_loaded_interfaces(LEGACY, LEGACY)
            self.assertEqual(LEGACY, self.state()["current"])
            for role in deploy.SERVING_ROLES:
                self.assertIsNone(self.ops.inspect(target["containers"][role]))
        self.assertIsNone(self.state()["in_progress"])
        held = progress.get("hold") or progress
        self.assertEqual(ids, {role: entry["id"] for role, entry in held.get("containers", {}).items()})
        self.assertEqual(LEGACY, held["current"])
        self.assertIn("'resume' or 'abort'", str(failure.exception))

    def test_resume_recovers_after_hold_checkpoint_write_fails(self):
        self.assert_recovery_after_hold_write_fails("resume", checkpoint=True)

    def test_abort_recovers_after_hold_checkpoint_write_fails(self):
        self.assert_recovery_after_hold_write_fails("abort", checkpoint=True)

    def test_resume_recovers_after_first_held_write_fails(self):
        self.assert_recovery_after_hold_write_fails("resume", checkpoint=False)

    def test_abort_recovers_after_first_held_write_fails(self):
        self.assert_recovery_after_hold_write_fails("abort", checkpoint=False)

    def test_resume_cannot_mutate_until_pause_consumption_is_saved(self):
        paused = self.hold()
        real = deploy.save_state

        def fail_consume(cfg, state):
            if (state.get("in_progress") or {}).get("step") == "activate held serving":
                raise OSError("simulated state write failure")
            return real(cfg, state)

        self.ops.commands.clear()
        with mock.patch.object(deploy, "save_state", fail_consume):
            with self.assertRaisesRegex(deploy.DeployError, "pause retained"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual(paused, self.state()["in_progress"])
        self.assertEqual([[self.cfg.nginx_bin, "-T"], ["docker", "network", "inspect", *self.cfg.networks]],
                         self.ops.commands)

    def test_abort_state_write_failure_after_removal_can_retry_without_containers(self):
        self.hold()
        real = deploy.save_state

        def fail_clear(cfg, state):
            if not state.get("in_progress"):
                raise OSError("simulated state write failure")
            return real(cfg, state)

        with mock.patch.object(deploy, "save_state", fail_clear):
            with self.assertRaisesRegex(deploy.DeployError, "state write failed"):
                deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertNotIn("sources-main-a-serving", self.ops.containers)
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_main_handoff_create_failure_restores_old_background_restart_policy(self):
        old = self.ops.containers[LEGACY["containers"]["background"]]
        policy = {"Name": "on-failure", "MaximumRetryCount": 3}
        old["HostConfig"]["RestartPolicy"] = dict(policy)
        real = self.ops.run

        def fail_create(args, **kw):
            if args[:2] == ["docker", "create"] and "sources-main-a-background" in args:
                raise deploy.DeployError("simulated create failure")
            return real(args, **kw)

        with mock.patch.object(self.ops, "run", fail_create):
            with self.assertRaises(deploy.DeployError):
                self.deploy()
        self.assertTrue(old["State"]["Running"])
        self.assertEqual(policy, old["HostConfig"]["RestartPolicy"])
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_main_handoff_readiness_failure_restores_original_retry_limit(self):
        old = self.ops.containers[LEGACY["containers"]["background"]]
        policy = {"Name": "on-failure", "MaximumRetryCount": 3}
        old["HostConfig"]["RestartPolicy"] = dict(policy)
        self.ops.unready_ports.add(self.cfg.slots["a"]["background"])
        with self.assertRaisesRegex(deploy.DeployError, "is running again"):
            self.deploy()
        self.assertEqual(policy, old["HostConfig"]["RestartPolicy"])
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_main_switch_failure_restores_original_background_restart_policy(self):
        old = self.ops.containers[LEGACY["containers"]["background"]]
        policy = {"Name": "on-failure", "MaximumRetryCount": 3}
        old["HostConfig"]["RestartPolicy"] = dict(policy)
        self.ops.nginx_test_rc = 1
        with self.assertRaisesRegex(deploy.DeployError, "nginx -t rejected"):
            self.deploy()
        self.assertEqual(policy, old["HostConfig"]["RestartPolicy"])
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_abort_restores_original_policy_after_handoff_recovery_fails(self):
        name = LEGACY["containers"]["background"]
        old = self.ops.containers[name]
        policy = {"Name": "on-failure", "MaximumRetryCount": 3}
        old["HostConfig"]["RestartPolicy"] = dict(policy)
        candidate = self.cfg.slot_view("a")["containers"]["background"]
        run = self.ops.run

        def fail(args, **kw):
            if args[:2] == ["docker", "create"] and args[args.index("--name") + 1] == candidate:
                raise deploy.DeployError("fixture create failure")
            if args == ["docker", "update", "--restart=on-failure:3", name]:
                raise deploy.DeployError("fixture restart policy update failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        self.assert_live_release(LEGACY)
        self.assertEqual("no", old["HostConfig"]["RestartPolicy"]["Name"])
        self.assert_release_failure("failed", "restart policy update failure")
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual(policy, old["HostConfig"]["RestartPolicy"])
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_release_lock_blocks_resume_and_abort_without_mutations(self):
        self.hold()
        before = self.state()
        with deploy.release_lock(self.cfg.lock_file), \
                mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                mock.patch("sys.stderr"):
            self.assertEqual(1, deploy.main(["resume"]))
            self.assertEqual(1, deploy.main(["abort"]))
        self.assertEqual(before, self.state())

    def test_render_upstreams_uses_fixed_names(self):
        text = deploy.render_upstreams(9401, 9402, 9403)
        self.assertIn("upstream sources_main_serving", text)
        self.assertIn("server 127.0.0.1:9402 backup", text)
        self.assertIn("upstream sources_main_admin", text)
        self.assertIn("server 127.0.0.1:9403;", text)

    def test_nginx_readiness_reports_version_named_upstreams(self):
        problems = deploy.nginx_ready_for_fixed_upstreams("proxy_pass http://sources_main_001_serving;")
        self.assertEqual(3, len(problems))
        self.assertIn("sources_main_001_serving", problems[2])
        self.assertEqual([], deploy.nginx_ready_for_fixed_upstreams(FIXED_NGINX))

    def test_first_deploy_from_legacy_moves_everything_to_slot_a(self):
        self.deploy()
        state = self.state()
        self.assertEqual("a", state["current"]["slot"])
        self.assertEqual("new@sha256:2", state["current"]["image"])
        self.assertEqual("legacy", state["previous"]["slot"])
        self.assertIsNone(state["in_progress"])
        self.assertEqual(["sources-main-a-serving", "sources-main-a-standby", "sources-main-a-background"],
                         self.created())
        self.assertEqual([["-list-pending-migrations"], ["-migrate-only", "001_add.sql"], ["-check-startup"],
                          ["-check-startup"], ["-export-web-assets", "/export"]], self.ops.oneoffs)
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn("127.0.0.1:9401 max_fails", upstream)
        self.assertIn("127.0.0.1:9403;", upstream)
        # old background got exactly one TERM before the new one started, never SIGKILL
        kills = [c for c in self.ops.commands if c[:2] == ["docker", "kill"]]
        self.assertTrue(all(c[2] == "--signal=TERM" for c in kills))
        term_bg = self.ops.commands.index(["docker", "kill", "--signal=TERM", "old-bg"])
        start_bg = self.ops.commands.index(["docker", "start", "sources-main-a-background"])
        self.assertLess(term_bg, start_bg)
        # old serving retired after the switch, without waiting
        self.assertFalse(self.ops.containers["old-serving"]["State"]["Running"])
        self.assertEqual("no", self.ops.containers["old-serving"]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertEqual("ok", self.history()[-1]["result"])
        steps = [s["step"] for s in self.history()[-1]["steps"]]
        self.assertEqual(["pull image", "preflight", "migrate", "startup check", "export assets", "start serving",
                          "background handoff", "switch", "guard", "retire previous serving"], steps)

    def test_containers_run_the_binary_directly_like_production(self):
        self.deploy()
        creates = [c for c in self.ops.commands if c[:2] == ["docker", "create"]]
        self.assertTrue(creates)
        for cmd in creates:
            self.assertEqual("/app/sub2api", cmd[cmd.index("--entrypoint") + 1])

    def test_serving_containers_share_a_stable_alias_on_every_network(self):
        self.deploy()
        for role in ("serving", "standby"):
            name = f"sources-main-a-{role}"
            create = next(c for c in self.ops.commands if c[:2] == ["docker", "create"] and name in c)
            aliases = [create[i + 1] for i, a in enumerate(create) if a == "--network-alias"]
            self.assertEqual([name, "sources-main-serving"], aliases)
            connect = next(c for c in self.ops.commands if c[:3] == ["docker", "network", "connect"] and c[-1] == name)
            self.assertEqual(["--alias", name, "--alias", "sources-main-serving"], connect[3:7])
        bg = next(c for c in self.ops.commands if c[:2] == ["docker", "create"] and "sources-main-a-background" in c)
        self.assertNotIn("sources-main-serving", bg, "the background never answers the serving alias")

    def test_pull_only_when_image_is_missing(self):
        self.deploy()
        self.assertIn(["docker", "pull", "new@sha256:2"], self.ops.commands)
        self.ops.commands.clear()
        self.ops.local_images.add("local:tag")
        self.ops.containers["sources-main-a-serving"]["State"]["Running"] = True
        self.deploy("local:tag")
        self.assertNotIn(["docker", "pull", "local:tag"], self.ops.commands)

    def test_second_deploy_reuses_slot_b_then_slot_a(self):
        self.deploy()
        self.deploy("newer@sha256:3")
        self.assertEqual("b", self.state()["current"]["slot"])
        self.assertEqual("a", self.state()["previous"]["slot"])
        self.ops.containers["sources-main-a-serving"]["State"]["Running"] = False
        self.deploy("newest@sha256:4")
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertEqual("newest@sha256:4", self.ops.containers["sources-main-a-serving"]["Config"]["Image"])

    def test_preflight_failure_changes_nothing(self):
        old_slot = self.cfg.slot_view("a")
        name = old_slot["containers"]["serving"]
        self.ops.add(name, old_slot["ports"]["serving"])
        self.ops.nginx_dump = "proxy_pass http://sources_main_001_serving;"
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("fixed upstream names", str(ctx.exception))
        self.assertIn("still running/draining", str(ctx.exception))
        self.assertTrue(self.ops.inspect(name)["State"]["Running"])
        self.assert_live_release(LEGACY)
        self.assertEqual([], self.created())
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_preflight_requires_reserved_and_free_ports(self):
        self.ops.files["/proc/sys/net/ipv4/ip_local_port_range"] = "1024\t65535\n"
        self.ops.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = "9400-9402\n"
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("[9403]", str(ctx.exception))
        self.assertIn("ip_local_reserved_ports", str(ctx.exception))
        self.assertEqual([], self.created())
        self.ops.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = "9400-9404,9410-9414\n"
        self.ops.busy_ports = {9402}
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("ports [9402] are already in use", str(ctx.exception))
        self.ops.busy_ports = set()
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_ports_below_the_ephemeral_range_need_no_reservation(self):
        self.ops.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = "\n"
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_port_list_parsing(self):
        self.assertEqual({1, 2, 3, 7}, deploy.parse_port_list("1-3,7\n"))
        self.assertEqual(set(), deploy.parse_port_list("\n"))

    def test_contract_migration_requires_explicit_flag(self):
        self.ops.pending = "002_drop_contract.sql"
        with self.assertRaises(deploy.DeployError):
            self.deploy()
        self.assertNotIn(["-migrate-only", "002_drop_contract.sql"], self.ops.oneoffs)
        self.ops.pending = "002_drop_contract.sql"
        self.deploy(allow_contract=True)
        self.assertIn(["-migrate-only", "002_drop_contract.sql"], self.ops.oneoffs)

    def test_startup_check_failure_starts_nothing(self):
        fallback = self.cfg.slot_view("a")
        survivor = fallback["containers"]["serving"]
        self.ops.add(survivor, fallback["ports"]["serving"], running=False)
        before = self.ops.inspect(survivor)
        self.ops.check_fails = True
        with self.assertRaises(deploy.DeployError):
            self.deploy()
        self.assertEqual([], self.created())
        self.assertEqual(before, self.ops.inspect(survivor))
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertFalse(self.ops.switched)

    def test_new_background_not_ready_restarts_previous_owner(self):
        self.ops.unready_ports = {9403}
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("previous background old-bg is running again", str(ctx.exception))
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertNotIn("sources-main-a-serving", self.ops.containers, "unused new serving is removed")
        self.assertFalse(self.ops.switched, "traffic never moved")
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_guard_failure_returns_traffic_and_background(self):
        self.cfg.serving_alias = ""
        self.ops.memory_after_switch = 20 * GIB
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("guard failed", str(ctx.exception))
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn("127.0.0.1:9426 max_fails", upstream)
        self.assertIn("127.0.0.1:9430;", upstream)
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertFalse(self.ops.containers["sources-main-a-background"]["State"]["Running"])
        self.assertFalse(self.ops.containers["sources-main-a-serving"]["State"]["Running"])
        self.assertIn("sources-main-a-serving", self.ops.containers, "kept for inspection")
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_guard_counts_connect_failures_only_for_new_ports(self):
        line = 'connect() failed (111) while connecting to upstream, upstream: "http://127.0.0.1:{}/v1"'
        text = "\n".join([line.format(9401)] * 3 + [line.format(9426)] * 50)
        self.assertEqual(3, deploy.Deployer.connect_errors(text, {"9401", "9402"}))

    def test_background_that_keeps_crashing_fails_fast(self):
        # Simulate a background container that exits as soon as it starts.
        original_start = FakeOps.run

        def run(args, **kw):
            result = original_start(self.ops, args, **kw)
            if args[:2] == ["docker", "start"] and args[-1].endswith("-background"):
                self.ops.containers[args[-1]]["State"].update(Running=False, Status="exited", ExitCode=1)
            return result

        self.ops.run = run
        self.ops.unready_ports = {9403}
        started = self.ops.clock
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("exited or is restarting", str(ctx.exception))
        self.assertLess(self.ops.clock - started, self.cfg.background_ready_timeout,
                        "a crash must not wait for the whole readiness timeout")
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])

    def test_nginx_test_failure_hands_background_back(self):
        self.ops.nginx_test_rc = 1
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("owns the background again", str(ctx.exception))
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertNotIn("sources-main-a-serving", self.ops.containers)
        self.assertEqual("legacy", self.state()["current"]["slot"])

    def test_unfinished_deploy_blocks_the_next_one(self):
        state = self.state()
        state["in_progress"] = {"step": "background handoff"}
        deploy.save_state(self.cfg, state)
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("did not finish", str(ctx.exception))
        self.assertEqual([], self.ops.commands)

    def test_handoff_says_admin_stays_read_only_when_the_old_owner_reports_it(self):
        self.ops.runtime_extra["old-bg"] = {"admin_during_drain": "read-only"}
        self.deploy()
        text = "\n".join(self.ops.logs)
        self.assertIn("while old-bg drains it answers admin reads and refuses admin writes with 503", text)
        self.assertNotIn("admin API is unavailable", text)

    def test_handoff_says_admin_is_unavailable_for_an_old_binary(self):
        self.deploy()
        self.assertIn("the admin API is unavailable until the new background is ready",
                      "\n".join(self.ops.logs))

    def test_handoff_reports_what_the_old_owner_is_still_stopping(self):
        self.ops.drain_seconds["old-bg"] = 130
        self.ops.runtime_extra["old-bg"] = {"admin_during_drain": "read-only", "active_admin_writes": 1,
                                            "stopping": ["BackupService", "TokenRefreshService"]}
        self.deploy()
        notes = [line for line in self.ops.logs if "old-bg still finishing in-flight work" in line]
        self.assertGreaterEqual(len(notes), 3, "first note a few seconds after TERM, then one per minute")
        self.assertIn("1 admin request(s) in flight; still stopping BackupService, TokenRefreshService", notes[0])
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_handoff_polls_every_half_second_and_nothing_else_does(self):
        calls = []
        real_exit, real_ready = deploy.Deployer.wait_exit, deploy.Deployer.wait_ready

        def wait_exit(deployer, name, timeout, **kw):
            calls.append(("exit", name, kw.get("poll", 2)))
            return real_exit(deployer, name, timeout, **kw)

        def wait_ready(deployer, port, token, role, timeout, **kw):
            calls.append(("ready", role, kw.get("poll", 2)))
            return real_ready(deployer, port, token, role, timeout, **kw)

        self.ops.drain_seconds["old-bg"] = 3
        with mock.patch.object(deploy.Deployer, "wait_exit", wait_exit), \
                mock.patch.object(deploy.Deployer, "wait_ready", wait_ready):
            self.deploy()
        self.assertIn(("exit", "old-bg", deploy.HANDOFF_POLL_SECONDS), calls)
        self.assertIn(("ready", "background", deploy.HANDOFF_POLL_SECONDS), calls)
        self.assertEqual({2}, {poll for kind, role, poll in calls if role == "serving"})
        self.assertEqual(0.5, deploy.HANDOFF_POLL_SECONDS)
        self.assertIn(0.5, self.ops.sleeps)

    def test_old_owner_that_never_exits_keeps_in_progress_with_instructions(self):
        self.ops.drain_seconds["old-bg"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed) as ctx:
            self.deploy()
        message = str(ctx.exception)
        slot = self.cfg.slot_view("a")
        serving = ", ".join(slot["containers"][role] for role in deploy.SERVING_ROLES)
        for part in ("old-bg is still draining after 2100s", "nothing was killed",
                     "sources-main-a-background was not started", "Traffic is on slot legacy",
                     "no background owner runs",
                     f"Serving containers left outside the Nginx route: {serving}",
                     "start the background of slot legacy: 'docker update --restart=unless-stopped old-bg' "
                     "and 'docker start old-bg'", "Do not deploy again"):
            self.assertIn(part, message)
        self.assertTrue(self.ops.containers["sources-main-a-serving"]["State"]["Running"],
                        "as the message says, the new serving containers are left running")
        old = self.ops.containers["old-bg"]
        self.assertTrue(old["State"]["Running"], "never killed")
        self.assertEqual("no", old["HostConfig"]["RestartPolicy"]["Name"])
        self.assertFalse(self.ops.containers["sources-main-a-background"]["State"]["Running"])
        self.assertFalse(self.ops.switched, "traffic never moved")
        in_progress = self.state()["in_progress"]
        self.assertEqual("background handoff", in_progress["failed_step"])
        self.assertEqual(message, in_progress["reason"])
        self.assertIn("failed_at", in_progress)
        # 'status' shows it, and the next deploy refuses until a person clears it.
        report = deploy.status(self.cfg, self.ops)
        self.assertEqual(message, report["in_progress"]["reason"])
        self.assertEqual("running", report["current"]["containers"]["background"]["status"])
        self.assertEqual("no", report["current"]["containers"]["background"]["restart_policy"])
        self.assertIn("FAILED AND NOT RESTORED", "\n".join(self.ops.logs))
        commands = len(self.ops.commands)
        with self.assertRaises(deploy.DeployError) as again:
            self.deploy()
        self.assertIn("did not finish", str(again.exception))
        self.assertIn("Do not deploy again", str(again.exception))
        self.assertEqual(commands, len(self.ops.commands))

    def test_rollback_drain_timeout_preserves_read_only_admin_and_keeps_in_progress(self):
        self.deploy()
        current = self.state()["current"]
        owner = current["containers"]["background"]
        self.ops.drain_seconds[owner] = float("inf")
        self.ops.runtime_extra[owner] = {"admin_during_drain": "read-only"}
        with self.assertRaises(deploy.RollbackIncomplete):
            deploy.Deployer(self.cfg, self.ops).rollback()
        in_progress = self.state()["in_progress"]
        self.assertEqual("failed", in_progress["phase"])
        self.assertEqual("background handoff", in_progress["failed_step"])
        self.assertIn("still draining", in_progress["reason"])
        expected = {**LEGACY["ports"], "background": current["ports"]["background"]}
        self.assertEqual(self.upstreams_for({"ports": expected}), self.upstreams_only(self.ops.loaded_upstreams))
        self.assertTrue(self.ops.inspect(owner)["State"]["Running"])

    def test_guard_rollback_drain_timeout_does_not_select_unavailable_admin(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        owner = target["containers"]["background"]
        self.ops.drain_seconds[owner] = float("inf")
        with self.assertRaisesRegex(deploy.FallbackFailed, "still draining.*'status'") as ctx:
            self.deploy()
        self.assertEqual(self.upstreams_for(target), self.upstreams_only(self.ops.loaded_upstreams))
        self.assertEqual(self.ops.loaded_upstreams, self.ops.files[self.cfg.upstream_file])
        self.assertTrue(self.ops.inspect(owner)["State"]["Running"])
        self.assertFalse(self.ops.inspect(LEGACY["containers"]["background"])["State"]["Running"])
        self.assertEqual(str(ctx.exception), self.state()["in_progress"]["reason"])

    def test_a_reply_cut_off_mid_way_never_ends_the_handoff(self):
        # The old owner exits while answering a runtime read: the progress note
        # and the pre-signal read must not raise out of the handoff.
        self.ops.drain_seconds["old-bg"] = 70
        real = self.ops.http_json
        broken = iter([http.client.IncompleteRead(b"{\"ready\": tr"), ValueError("truncated JSON"),
                       http.client.BadStatusLine("")] * 10)

        def http_json(url, token=None, method="GET", body=None, timeout=5):
            if (url == "http://127.0.0.1:9430/internal/sources/runtime"
                    and (self.state()["in_progress"] or {}).get("step") == "background handoff"):
                raise next(broken)
            return real(url, token=token, method=method, body=body, timeout=timeout)

        self.ops.http_json = http_json
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        text = "\n".join(self.ops.logs)
        self.assertIn("the admin API is unavailable", text)
        self.assertIn("old-bg still finishing in-flight work ...", text)

    def test_rollback_reuses_previous_containers(self):
        self.cfg.serving_alias = ""
        self.deploy()
        created_before = len(self.created())
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(created_before, len(self.created()), "rollback creates nothing")
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertTrue(self.ops.containers["old-serving"]["State"]["Running"])
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertFalse(self.ops.containers["sources-main-a-background"]["State"]["Running"])
        self.assertIn("127.0.0.1:9426 max_fails", self.ops.files[self.cfg.upstream_file])

    def test_rollback_guard_failure_restores_release_and_clears_progress(self):
        self.cfg.serving_alias = ""
        self.deploy()
        original = self.state()
        inspect = self.ops.inspect
        failed = False

        def fail_guard(name):
            nonlocal failed
            if (name == LEGACY["containers"]["serving"] and (self.state()["in_progress"] or {}).get("step") == "guard"
                    and not failed):
                failed = True
                raise deploy.DeployError("simulated guard query failure")
            return inspect(name)

        with mock.patch.object(self.ops, "inspect", fail_guard):
            with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
                deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertTrue(failed)
        self.assert_live_release(original["current"])
        self.assertEqual(original, self.state())

    def test_dry_run_mutates_nothing(self):
        self.ops.dry_run = True
        self.deploy()
        mutating = [c for c in self.ops.commands if c and c[0] != "DRY"]
        for cmd in mutating:
            joined = " ".join(cmd)
            self.assertNotIn("-migrate-only", joined)
            self.assertFalse(cmd[:2] in (["docker", "kill"], ["docker", "update"]) or cmd[1:] == ["-s", "reload"],
                             joined)
        self.assertNotIn(["-migrate-only", "001_add.sql"], self.ops.oneoffs)
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])

    def test_dry_run_with_pending_migrations_skips_startup_check(self):
        self.ops.dry_run = True
        self.deploy()
        self.assertNotIn(["-check-startup"], self.ops.oneoffs)
        self.assertTrue(any("001_add.sql" in line and "skipped" in line and "dry run" in line
                            for line in self.ops.logs), self.ops.logs)
        self.assertTrue(any("did NOT verify" in line for line in self.ops.logs), self.ops.logs)
        self.assertEqual("legacy", self.state()["current"]["slot"])

    def test_dry_run_without_pending_migrations_runs_startup_check(self):
        self.ops.dry_run = True
        self.ops.pending = ""
        self.deploy()
        self.assertEqual(2, self.ops.oneoffs.count(["-check-startup"]))
        self.assertFalse(any("did NOT verify" in line for line in self.ops.logs))

    def test_dry_run_without_pending_migrations_reports_startup_check_failure(self):
        self.ops.dry_run = True
        self.ops.pending = ""
        self.ops.check_fails = True
        with self.assertRaises(deploy.DeployError):
            self.deploy()

    def test_http_json_sends_an_explicit_user_agent(self):
        seen = []

        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

            def read(self):
                return b"ok"

        def urlopen(req, timeout):
            seen.append(req)
            return Response()

        with mock.patch.object(deploy.urllib.request, "urlopen", urlopen):
            status, body = deploy.Ops().http_json("https://example.com/health", timeout=10)
        self.assertEqual((200, "ok"), (status, body))
        self.assertEqual(deploy.USER_AGENT, seen[0].get_header("User-agent"))
        self.assertNotIn("urllib", seen[0].get_header("User-agent"))

    def test_http_json_does_not_follow_redirect_with_a_runtime_token(self):
        request = deploy.urllib.request
        runtime = "http://runtime.invalid/internal/sources/runtime"
        drain = "http://runtime.invalid/internal/sources/drain"
        redirected = "http://other.invalid/"
        token = "FIXTURE_RUNTIME_TOKEN"
        requests = []

        class FixtureHTTP(request.HTTPHandler):
            def http_open(self, req):
                requests.append((req.full_url, req.get_method(), req.get_header("Authorization")))
                headers = Message()
                if req.full_url == redirected:
                    response = addinfourl(io.BytesIO(b"{}"), headers, req.full_url, 200)
                    response.msg = "OK"
                else:
                    headers["Location"] = redirected
                    response = addinfourl(io.BytesIO(), headers, req.full_url, 302)
                    response.msg = "Found"
                return response

        build_opener = request.build_opener

        def fixture_opener(*handlers):
            return build_opener(*handlers, FixtureHTTP(), request.ProxyHandler({}))

        with mock.patch.object(request, "build_opener", side_effect=fixture_opener), \
                mock.patch.object(request, "urlopen", fixture_opener().open):
            ops = deploy.Ops()
            self.assertEqual((302, None), ops.http_json(runtime, token=token))
            self.assertEqual((302, None), ops.http_json(drain, token=token, method="POST", body={"instance_id": "fixture"}))
            self.assertEqual((200, {}), ops.http_json(runtime))
        self.assertEqual([(runtime, "GET", f"Bearer {token}"), (drain, "POST", f"Bearer {token}"),
                          (runtime, "GET", None), (redirected, "GET", None)], requests)

    def test_http_json_runtime_token_bypasses_process_proxy(self):
        request = deploy.urllib.request
        runtime = "http://localhost/internal/sources/runtime"
        addresses = []
        build_opener = request.build_opener

        class FixtureHTTP(request.HTTPHandler):
            def http_open(self, req):
                addresses.append(req.host)
                response = addinfourl(io.BytesIO(b"{}"), Message(), req.full_url, 200)
                response.msg = "OK"
                return response

        with mock.patch.object(request, "getproxies", return_value={"http": "http://proxy.invalid"}), \
                mock.patch.object(request, "proxy_bypass", return_value=False), \
                mock.patch.object(request, "build_opener", side_effect=lambda *h: build_opener(*h, FixtureHTTP())):
            self.assertEqual((200, {}), deploy.Ops().http_json(runtime, token="FIXTURE_RUNTIME_TOKEN"))
        self.assertEqual(["localhost"], addresses)

    def test_http_json_treats_a_reply_cut_off_mid_way_as_no_reply(self):
        # Real sockets: a process that exits while answering leaves one of these.
        replies = {
            "bad status line": b"garbage\r\n\r\n",
            "short body": b"HTTP/1.1 200 OK\r\nContent-Length: 64\r\n\r\n{\"ready\": tr",
            "truncated JSON": b"HTTP/1.1 200 OK\r\nContent-Length: 12\r\nConnection: close\r\n\r\n{\"ready\": tr",
        }
        for name, reply in replies.items():
            with self.subTest(name):
                class Handler(socketserver.BaseRequestHandler):
                    def handle(self):
                        data = b""
                        while b"\r\n\r\n" not in data:
                            chunk = self.request.recv(4096)
                            if not chunk:
                                break
                            data += chunk
                        self.request.sendall(reply)

                with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
                    thread = threading.Thread(target=server.handle_request, daemon=True)
                    thread.start()
                    url = f"http://127.0.0.1:{server.server_address[1]}/internal/sources/runtime"
                    self.assertEqual((0, None), deploy.Ops().http_json(url, token="tok", timeout=5))
                    thread.join(5)

    def assert_public_health_stops_before_any_change(self, status):
        state_before = Path(self.cfg.state_file).read_text()
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        message = str(ctx.exception)
        self.assertIn("preflight failed, nothing changed", message)
        self.assertIn(f"returned status {status}", message)
        self.assertIn("--allow-public-health-failure", message)
        self.assertEqual([], self.created())
        self.assertFalse(any("-migrate-only" in " ".join(c) for c in self.ops.commands))
        self.assertNotIn(["-migrate-only", "001_add.sql"], self.ops.oneoffs)
        self.assertFalse(self.ops.switched)
        self.assertEqual(state_before, Path(self.cfg.state_file).read_text())

    def test_preflight_stops_when_public_health_fails(self):
        for status in (403, 0):
            with self.subTest(status=status):
                self.ops.commands.clear()
                self.ops.public_status = status
                self.assert_public_health_stops_before_any_change(status)
        self.assertEqual(["failed", "failed"], [r["result"] for r in self.history()])

    def test_dry_run_stops_when_public_health_fails_and_records_nothing(self):
        self.ops.dry_run = True
        self.ops.public_status = 403
        self.assert_public_health_stops_before_any_change(403)
        self.assertEqual([], self.history())

    def test_failed_dry_run_records_no_history(self):
        self.ops.dry_run = True
        self.ops.pending = ""
        self.ops.check_fails = True
        with self.assertRaises(deploy.DeployError):
            self.deploy()
        self.assertEqual([], self.history())

    def test_allow_public_health_failure_warns_and_deploys(self):
        self.ops.public_status = 503
        self.ops.public_status_after_reload = 200
        self.deploy(allow_public_health_failure=True)
        self.assertTrue(any("WARNING" in line and "status 503" in line and "--allow-public-health-failure" in line
                            for line in self.ops.logs), self.ops.logs)
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_allowed_public_health_failure_is_still_guarded_after_the_switch(self):
        self.ops.public_status = 503
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy(allow_public_health_failure=True)
        self.assertIn("public health failed 3 times in a row (last status 503)", str(ctx.exception))
        self.assertEqual("legacy", self.state()["current"]["slot"])

    def test_guard_failure_reports_last_public_status(self):
        self.ops.public_status_after_reload = 403
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("guard failed (public health failed 3 times in a row (last status 403))", str(ctx.exception))
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIn("127.0.0.1:9426 max_fails", self.ops.files[self.cfg.upstream_file])

    def test_rollback_is_not_blocked_by_a_failing_public_health_check(self):
        self.cfg.serving_alias = ""
        self.deploy()
        # The current release is broken in public; rollback must not pre-check it.
        self.ops.public_status = 502
        self.ops.public_status_after_reload = 200
        requests_before = self.ops.public_requests
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertEqual("rollback", self.history()[-1]["action"])
        self.assertEqual("ok", self.history()[-1]["result"])
        self.assertTrue(self.ops.public_requests > requests_before, "the guard still checks after the switch")

    def test_release_lock_is_exclusive(self):
        with deploy.release_lock(self.cfg.lock_file):
            with self.assertRaises(deploy.DeployError):
                with deploy.release_lock(self.cfg.lock_file):
                    pass

    def test_watch_flags_active_http_and_memory(self):
        self.ops.containers["old-serving"]["active_http"] = 1500
        self.ops.memory["old-bg"] = 13 * GIB
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        reasons = {a["name"]: a["alert"] for a in alerts}
        self.assertEqual(["active_http"], reasons["old-serving"])
        self.assertEqual(["memory"], reasons["old-bg"])
        self.assertNotIn("old-spare", reasons)

    def test_adopt_refuses_to_replace_a_held_release(self):
        cfg_path = self.tmp / "main-config.json"
        cfg_path.write_text(json.dumps(deploy.asdict(self.cfg)))
        cfg_bytes = cfg_path.read_bytes()
        held = self.hold()
        state_bytes = Path(self.cfg.state_file).read_bytes()
        docker = list(self.ops.commands)
        with self.assertRaisesRegex(deploy.DeployError, "resume.*abort"):
            deploy.adopt(str(cfg_path), self.ops, *(LEGACY["containers"][role] for role in deploy.ROLES),
                         str(self.tmp), self.cfg.public_health_url)
        self.assertEqual(cfg_bytes, cfg_path.read_bytes())
        self.assertEqual(state_bytes, Path(self.cfg.state_file).read_bytes())
        self.assertEqual(docker, self.ops.commands)
        self.assertEqual(held, self.state()["in_progress"])
        self.assert_live_release(LEGACY)
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_adopt_uses_configured_release_lock_before_reading_state_or_touching_work_dir(self):
        cfg_path = self.tmp / "main-config.json"
        cfg_path.write_text(json.dumps(deploy.asdict(self.cfg)))
        work_dir = self.tmp / "new-work-dir"
        args = ["--config", str(cfg_path), "adopt", "--work-dir", str(work_dir),
                "--public-health-url", self.cfg.public_health_url]
        for role in deploy.ROLES:
            args += [f"--{role}", LEGACY["containers"][role]]
        before = Path(self.cfg.state_file).read_bytes()
        with deploy.release_lock(self.cfg.lock_file), \
                mock.patch.object(deploy, "MainOps", return_value=self.ops), \
                mock.patch.object(deploy, "read_state_file") as read_state, \
                mock.patch("sys.stderr") as stderr:
            self.assertEqual(1, deploy.main(args))
            read_state.assert_not_called()
            self.assertIn("another release holds", "".join(call.args[0] for call in stderr.write.call_args_list))
        self.assertEqual(before, Path(self.cfg.state_file).read_bytes())
        self.assertFalse(work_dir.exists())
        self.assertEqual([], self.ops.commands)

    def test_adopt_checks_both_existing_and_destination_state(self):
        held = self.hold()
        cfg_path = self.tmp / "main-config.json"
        cfg_path.write_text(json.dumps(deploy.asdict(self.cfg)))
        destination = self.tmp / "other-work-dir"
        destination.mkdir()
        destination_state = destination / "state.json"
        names = [LEGACY["containers"][role] for role in deploy.ROLES]
        args = [str(cfg_path), self.ops, *names, str(destination), self.cfg.public_health_url]

        for phase, instruction in (("held", "resume.*abort"), ("aborting", "retry 'abort'"),
                                   ("failed", "inspect with 'status'"), ("deploying", "inspect with 'status'")):
            with self.subTest(phase=phase):
                state = {"current": LEGACY, "in_progress": {**held, "phase": phase}}
                deploy.save_state(self.cfg, state)
                with self.assertRaisesRegex(deploy.DeployError, instruction):
                    deploy.adopt(*args)
                self.assertEqual(state, self.state())
                self.assertFalse(destination_state.exists())
        deploy.save_state(self.cfg, {"current": LEGACY, "in_progress": None})
        destination_state.write_text(json.dumps({"current": LEGACY, "in_progress": held}))
        before = destination_state.read_bytes()
        with self.assertRaisesRegex(deploy.DeployError, "resume.*abort"):
            deploy.adopt(*args)
        self.assertEqual(before, destination_state.read_bytes())

    def test_adopt_writes_config_and_never_logs_secrets(self):
        ops = FakeOps(self.tmp)
        mounts = [
            {"Source": "/state/main", "Destination": "/app/data", "RW": True},
            {"Source": "/rel/serving/main-runtime.yaml", "Destination": "/config/main-runtime.yaml", "RW": False},
            {"Source": "/rel/serving/runtime.token", "Destination": "/config/runtime.token", "RW": False},
            {"Source": "/rel/serving/admission", "Destination": "/runtime/admission", "RW": True},
            {"Source": "/rel/serving/pricing-cache", "Destination": "/runtime/pricing-cache", "RW": True},
        ]
        bg_mounts = [m for m in mounts if m["Destination"] != "/runtime/admission"]
        bg_mounts = [dict(m, Source=m["Source"].replace("serving", "bg")) if m["Destination"] != "/app/data" else m
                     for m in bg_mounts]
        env = [f"DATABASE_PASSWORD={SECRET}", "PATH=/bin", "SOURCES_PROCESS_ROLE=serving",
               "SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE=/runtime/admission/state.json", "TZ=Asia/Shanghai"]
        ops.add("old-serving", 9426, env=env, mounts=mounts)
        ops.add("old-spare", 9428, env=env, mounts=mounts)
        ops.add("old-bg", 9430, env=env, mounts=bg_mounts)
        work = self.tmp / "adopt"
        cfg_path = str(self.tmp / "main.json")
        ops.write_file_atomic = lambda path, text, mode=0o644: Path(path).parent.mkdir(parents=True, exist_ok=True) \
            or Path(path).write_text(text)
        with mock.patch.object(deploy, "DEFAULT_RELEASE_LOCK", self.cfg.lock_file):
            deploy.adopt(cfg_path, ops, "old-serving", "old-spare", "old-bg", str(work),
                         "https://example.com/health")

        self.assertNotIn(SECRET, "\n".join(ops.logs))
        serving_env = (work / "serving.env").read_text()
        self.assertIn(f"DATABASE_PASSWORD={SECRET}", serving_env)
        for dropped in ("PATH=", "SOURCES_PROCESS_ROLE=", "SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE="):
            self.assertNotIn(dropped, serving_env)
        self.assertIn("SOURCES_RUNTIME_TOKEN_FILE=/config/runtime.token", serving_env)
        cfg = deploy.Config.load(cfg_path)
        self.assertEqual("https://example.com/health", cfg.public_health_url)
        targets = {m.target for group in cfg.mounts.values() for m in group}
        self.assertNotIn("/runtime/admission", targets)
        common = {m.target: m for m in cfg.mounts["common"]}
        self.assertEqual(str(work / "runtime.token"), common["/config/runtime.token"].source)
        self.assertEqual("/state/main", common["/app/data"].source)
        pricing = [m for m in cfg.mounts["serving"] if m.target == "/runtime/pricing-cache"][0]
        self.assertTrue(pricing.per_container_copy)
        state = deploy.load_state(cfg)
        self.assertEqual("legacy", state["current"]["slot"])
        self.assertEqual("/rel/bg/runtime.token", state["current"]["runtime_token_files"]["background"])


if __name__ == "__main__":
    unittest.main()
