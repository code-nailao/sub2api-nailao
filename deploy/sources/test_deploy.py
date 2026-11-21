import http.client
import io
import json
import os
import signal
import socketserver
import threading
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

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
        self.oneoffs: list[list[str]] = []
        self.local_images: set[str] = set()
        self.busy_ports: set[int] = set()
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

    def add(self, name: str, port: int, running: bool = True, env=None, mounts=None, image="old@sha256:1"):
        self.containers[name] = {
            "Id": name + "-id", "Name": "/" + name, "RestartCount": 0,
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
        out, rc = "", 0
        if args[:2] == ["docker", "create"]:
            name = args[args.index("--name") + 1]
            port = 0
            if "--publish" in args:
                port = int(args[args.index("--publish") + 1].split(":")[1])
            role = next((a.split("=", 1)[1] for a in args if a.startswith("SOURCES_PROCESS_ROLE=")), "serving")
            self.add(name, port, running=False, image=args[-1] if "oneoff" not in name else "x")
            self.containers[name]["role"] = role
            # One-off command flags use a single dash (-check-startup ...); docker's
            # own flags here are "--long" or "-e".
            start = next((i for i, a in enumerate(args)
                          if i > 2 and a.startswith("-") and not a.startswith("--") and a != "-e"
                          and not args[i - 1] == "-e"), len(args))
            self.containers[name]["cmd"] = args[start:] if "oneoff" in name else []
        elif args[:2] == ["docker", "start"]:
            name = args[-1]
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
                c["State"].update(Running=True, Status="running", StartedAt=f"t{self.clock}")
        elif args[:2] == ["docker", "kill"]:
            c = self.containers.get(args[-1])
            if c and args[-1] in self.drain_seconds and c["State"]["Running"]:
                c["draining_until"] = self.clock + self.drain_seconds[args[-1]]
            elif c:
                c["State"].update(Running=False, Status="exited", ExitCode=0)
        elif args[:2] == ["docker", "rm"]:
            self.containers.pop(args[-1], None)
        elif args[:2] == ["docker", "update"]:
            c = self.containers.get(args[-1])
            if c:
                c["HostConfig"]["RestartPolicy"]["Name"] = args[2].split("=", 1)[1]
        elif args[:3] == ["docker", "container", "ls"]:
            out = "\n".join(self.containers)
        elif args[:3] == ["docker", "image", "inspect"]:
            rc = 0 if args[3] in self.local_images else 1
        elif args[1:] == ["-T"]:
            out = self.nginx_dump
        elif args[1:] == ["-t"]:
            rc = self.nginx_test_rc
        elif args[1:] == ["-s", "reload"]:
            self.switched = True
            if self.public_status_after_reload is not None:
                self.public_status, self.public_status_after_reload = self.public_status_after_reload, None
        if check and rc != 0:
            raise deploy.DeployError(f"command failed: {args[:3]}")
        return subprocess.CompletedProcess(args, rc, out, "")

    def settle_drain(self, c):
        if c and "draining_until" in c and self.clock >= c["draining_until"]:
            c.pop("draining_until")
            c["State"].update(Running=False, Status="exited", ExitCode=0)

    def inspect(self, name, *, timeout=30):
        c = self.containers.get(name)
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
        if "draining_until" in c and url.endswith("/internal/sources/runtime") and token == "tok":
            return 200, {"ready": False, "draining": True, "role": c["role"],
                         "owns_background": c["role"] == "background", "instance_id": c["Id"], **extra}
        if url.endswith("/health"):
            return 200, "ok"
        if url.endswith("/drain"):
            return 200, {}
        if token != "tok" or port in self.unready_ports:
            return (404, None) if token != "tok" else (200, {"ready": False, "role": c["role"]})
        return 200, {"ready": True, "role": c["role"], "owns_background": c["role"] == "background",
                     "instance_id": c["Id"], "active_http": c.get("active_http", 0),
                     **{k: v for k, v in extra.items() if k in ("admin_during_drain", "scheduled_backup_remaining_seconds")}}

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

    def write_file_atomic(self, path, text, mode=0o644):
        if self.dry_run:
            self.commands.append(["DRY", "write", path])
            return
        self.files[path] = text

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
        for role, name in LEGACY["containers"].items():
            self.ops.add(name, LEGACY["ports"][role])
            self.ops.containers[name]["role"] = "background" if role == "background" else "serving"
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
        self.ops.nginx_dump = "proxy_pass http://sources_main_001_serving;"
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("fixed upstream names", str(ctx.exception))
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
        self.ops.check_fails = True
        with self.assertRaises(deploy.DeployError):
            self.deploy()
        self.assertEqual([], self.created())
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

    def test_guard_rollback_keeps_admin_on_the_answering_owner_until_the_previous_background_is_ready(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        new_admin = target["ports"]["background"]
        old_background = LEGACY["containers"]["background"]
        old_admin = LEGACY["ports"]["background"]
        writes: list[tuple[float, str]] = []
        started = None
        healthy = False

        run, http_json, write = self.ops.run, self.ops.http_json, self.ops.write_file_atomic

        def start(args, **kw):
            nonlocal started
            result = run(args, **kw)
            if args == ["docker", "start", old_background]:
                started = self.ops.clock
            return result

        def probe(url, **kw):
            # The previous background passes /health only once it has restarted
            # and owns the lock; until then admin must not move to its port.
            nonlocal healthy
            if started is not None and url.endswith(f":{old_admin}/health"):
                if self.ops.clock - started < 3:
                    return 0, None
                healthy = True
            return http_json(url, **kw)

        def record(path, text, **kw):
            if path == self.cfg.upstream_file:
                writes.append((self.ops.clock, text))
            return write(path, text, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "http_json", probe), \
                mock.patch.object(self.ops, "write_file_atomic", record):
            with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
                self.deploy()

        serving = LEGACY["ports"]["serving"]
        rollback_writes = [(at, text) for at, text in writes if f"127.0.0.1:{serving} max_fails" in text]
        self.assertEqual(2, len(rollback_writes), "serving returns to the previous slot, then admin follows")
        first_at, first = rollback_writes[0]
        self.assertIn(f"127.0.0.1:{new_admin};", first, "admin stays on the owner that still answers")
        self.assertIsNotNone(started)
        self.assertLess(first_at, started, "serving moved before the previous background was started")
        second_at, second = rollback_writes[1]
        self.assertIn(f"127.0.0.1:{old_admin};", second)
        self.assertTrue(healthy)
        self.assertGreaterEqual(second_at - started, 3, "admin moved before the previous background was healthy")
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).background_answering(LEGACY))
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_guard_rollback_records_incomplete_recovery_when_the_previous_background_never_answers(self):
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_ready_timeout = 6
        target = self.cfg.slot_view("a")
        old_admin = LEGACY["ports"]["background"]
        new_admin = target["ports"]["background"]
        started = False

        run, http_json = self.ops.run, self.ops.http_json

        def start(args, **kw):
            nonlocal started
            result = run(args, **kw)
            if args == ["docker", "start", LEGACY["containers"]["background"]]:
                started = True
            return result

        def probe(url, **kw):
            if started and url.endswith(f":{old_admin}/health"):
                return 0, None
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "http_json", probe):
            with self.assertRaisesRegex(deploy.FallbackFailed, "handing the background back to slot legacy did not finish"):
                self.deploy()

        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertIn(f"127.0.0.1:{new_admin};", upstream, "admin stays on the owner that still answers")
        self.assertNotIn(f"127.0.0.1:{old_admin};", upstream)
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).background_answering(target))
        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("handing the background back to slot legacy did not finish", progress["failed"])
        self.assertTrue(progress.get("failed_at"))
        self.assertEqual(progress, deploy.status(self.cfg, self.ops)["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_guard_rollback_does_not_move_admin_to_a_background_without_the_lock(self):
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_ready_timeout = 6
        target = self.cfg.slot_view("a")
        old_admin = LEGACY["ports"]["background"]
        new_admin = target["ports"]["background"]
        old_background = LEGACY["containers"]["background"]
        started = False
        lost_lock = False

        run, http_json = self.ops.run, self.ops.http_json

        def start(args, **kw):
            nonlocal started
            result = run(args, **kw)
            if args == ["docker", "start", old_background]:
                started = True
            return result

        def probe(url, **kw):
            # The background passes its readiness probe and answers /health,
            # then loses the kernel lock before admin routing moves.
            nonlocal lost_lock
            if started and url.endswith(f":{old_admin}/health") and not lost_lock:
                lost_lock = True
                return 200, "ok"
            if lost_lock and url.endswith(f":{old_admin}/internal/sources/runtime"):
                status, body = http_json(url, **kw)
                if status == 200 and isinstance(body, dict):
                    return status, {**body, "owns_background": False}
                return status, body
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "http_json", probe):
            with self.assertRaises(deploy.FallbackFailed) as ctx:
                self.deploy()

        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertTrue(lost_lock)
        self.assertIn(f"127.0.0.1:{new_admin};", upstream)
        self.assertNotIn(f"127.0.0.1:{old_admin};", upstream,
                         "a background that does not hold the lock must not receive admin")
        self.assertIn(f"admin still uses slot {target['slot']}", str(ctx.exception))
        self.assertEqual("guard", self.state()["in_progress"]["step"])
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).background_answering(target),
                        "admin keeps an owner that answers")

    def test_guard_rollback_restarts_the_answering_background_when_the_previous_one_exits(self):
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_exit_timeout = 2
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]
        candidate_admin = target["ports"]["background"]
        previous_background = LEGACY["containers"]["background"]
        self.ops.drain_seconds[candidate_background] = 8

        run = self.ops.run

        def start(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "start", previous_background]:
                # The previous background starts and exits at once.
                self.ops.containers[previous_background]["State"].update(
                    Running=False, Status="exited", ExitCode=1)
            return result

        with mock.patch.object(self.ops, "run", start):
            with self.assertRaises(deploy.FallbackFailed) as ctx:
                self.deploy()

        message = str(ctx.exception)
        self.assertIn(f"admin still uses slot {target['slot']}", message, "admin keeps an owner that answers")
        self.assertNotIn("no background answers admin now", message)
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertIn(f"127.0.0.1:{candidate_admin};", upstream)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['background']};", upstream)
        deployer = deploy.Deployer(self.cfg, self.ops)
        self.assertTrue(deployer.background_answering(target), "the drained background was restarted as owner")
        self.assertEqual(2, self.ops.commands.count(["docker", "start", candidate_background]))
        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("restarted", progress["failed"])

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

    def test_preflight_refuses_running_backup_before_changing_release_state(self):
        old = self.state()["current"]["containers"]["background"]
        self.ops.runtime_extra[old] = {"scheduled_backup_remaining_seconds": 0}
        before = self.state()
        with self.assertRaisesRegex(deploy.DeployError, "scheduled backup is running.*--allow-running-backup"):
            self.deploy()
        self.assertEqual(before, self.state())
        self.assertEqual([], self.ops.commands)
        self.assertEqual([], self.history())

    def test_running_backup_can_be_allowed_to_finish_before_handoff(self):
        old = self.state()["current"]["containers"]["background"]
        self.ops.runtime_extra[old] = {"scheduled_backup_remaining_seconds": 5}
        self.ops.drain_seconds[old] = 5
        self.cfg.background_exit_timeout = 2
        self.deploy(allow_running_backup=True)
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertFalse(self.ops.containers[old]["State"]["Running"])
        self.assertIn("scheduled backup still active", "\n".join(self.ops.logs))
        self.assertEqual([["docker", "kill", "--signal=TERM", old]],
                         [cmd for cmd in self.ops.commands if cmd[:2] == ["docker", "kill"] and cmd[-1] == old])

    def test_handoff_waits_for_backup_started_after_preflight(self):
        old = self.state()["current"]["containers"]["background"]
        self.ops.drain_seconds[old] = 5
        self.cfg.background_exit_timeout = 2
        original_run = self.ops.run

        def run(args, **kw):
            if args[:2] == ["docker", "kill"] and args[-1] == old:
                self.ops.runtime_extra[old] = {"scheduled_backup_remaining_seconds": 5}
            return original_run(args, **kw)

        with mock.patch.object(self.ops, "run", run):
            self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertIn("scheduled backup still active", "\n".join(self.ops.logs))

    def run_backup_handoff(self, backup_finishes, owner_exits, fail_probe_at=None):
        old = self.state()["current"]["containers"]["background"]
        old_id = self.ops.containers[old]["Id"]
        self.ops.drain_seconds[old] = owner_exits
        original_http = self.ops.http_json
        original_run = self.ops.run
        successor = self.cfg.container("a", "background")
        observations = []
        failed = False

        def http_json(*args, **kwargs):
            nonlocal failed
            status, body = original_http(*args, **kwargs)
            if not isinstance(body, dict) or body.get("instance_id") != old_id or not body.get("draining"):
                return status, body
            since_term = self.ops.clock - (self.ops.containers[old]["draining_until"] - owner_exits)
            if not failed and fail_probe_at is not None and since_term >= fail_probe_at:
                failed = True
                observations.append((since_term, "unavailable"))
                return 0, None
            active = since_term < backup_finishes
            observations.append((since_term, "active" if active else "finished"))
            if active:
                body["scheduled_backup_remaining_seconds"] = max(0, int(backup_finishes - since_term))
            return status, body

        def run(args, **kwargs):
            if args[:2] == ["docker", "start"] and args[-1] == successor:
                self.assertFalse(self.ops.inspect(old)["State"]["Running"], "lease owner still running")
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "http_json", http_json), mock.patch.object(self.ops, "run", run):
            self.deploy()
        self.assertEqual(successor, self.state()["current"]["containers"]["background"])
        self.assertTrue(self.ops.containers[successor]["State"]["Running"])
        return observations

    def test_handoff_waits_for_owner_when_backup_finishes_near_timeout(self):
        cutoff = self.cfg.background_exit_timeout
        observations = self.run_backup_handoff(backup_finishes=cutoff - 0.5, owner_exits=cutoff + 1)
        self.assertTrue(any(when < cutoff and state == "active" for when, state in observations))
        self.assertTrue(any(cutoff <= when < cutoff + 1 and state == "finished" for when, state in observations))

    def test_handoff_rechecks_backup_that_finishes_just_after_timeout(self):
        cutoff = self.cfg.background_exit_timeout
        exit_at = cutoff + deploy.BACKUP_STATUS_POLL_SECONDS + 2
        observations = self.run_backup_handoff(backup_finishes=cutoff + 1, owner_exits=exit_at)
        self.assertTrue(any(cutoff <= when < cutoff + 1 and state == "active" for when, state in observations))
        self.assertTrue(any(cutoff + 1 < when < exit_at and state == "finished" for when, state in observations))

    def test_handoff_retries_a_failed_backup_probe_at_timeout(self):
        cutoff = self.cfg.background_exit_timeout
        observations = self.run_backup_handoff(backup_finishes=cutoff + 6, owner_exits=cutoff + 8, fail_probe_at=cutoff)
        failed_at = next(when for when, state in observations if state == "unavailable")
        self.assertLess(failed_at - cutoff, 1)
        self.assertTrue(any(failed_at < when < failed_at + 3 and state == "active" for when, state in observations))

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

        def wait_exit(deployer, name, deadline, **kw):
            calls.append(("exit", name, kw.get("poll", 2)))
            return real_exit(deployer, name, deadline, **kw)

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

    def test_handoff_timeout_restores_owner_after_it_exits(self):
        old = self.state()["current"]["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = 8
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        starts = []
        original_run = self.ops.run

        def run(args, **kw):
            if args[:2] == ["docker", "start"] and args[-1] == old:
                self.assertFalse(self.ops.inspect(old)["State"]["Running"], "the draining owner still holds the lock")
                starts.append(args[-1])
            return original_run(args, **kw)

        with mock.patch.object(self.ops, "run", run):
            with self.assertRaisesRegex(deploy.DeployError, "previous background .* is running again"):
                self.deploy()
        self.assertEqual([old], starts)
        self.assertFalse(self.ops.switched)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertIsNone(self.state()["in_progress"])

    def test_sigint_in_normal_drain_restores_old_background_after_exit(self):
        old = self.state()["current"]["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.ops.drain_seconds[old] = 3
        original_sleep = self.ops.sleep
        interrupted = False

        def sleep(seconds):
            nonlocal interrupted
            if not interrupted and "draining_until" in self.ops.containers[old]:
                interrupted = True
                signal.raise_signal(signal.SIGINT)
            original_sleep(seconds)

        with mock.patch.object(self.ops, "sleep", sleep):
            with self.assertRaisesRegex(deploy.BackgroundRestored, "previous background .* is running again"):
                self.deploy()
        self.assertTrue(interrupted)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)

    def test_stuck_background_without_backup_releases_publish_lock(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = float("inf")
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run
        original_sleep = self.ops.sleep
        term_at = None
        interrupts = 0

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            return original_run(args, **kwargs)

        def sleep(seconds):
            nonlocal interrupts
            if term_at is not None:
                elapsed = self.ops.clock - term_at
                if elapsed >= 6 and interrupts < 2:
                    interrupts += 1
                    original_sleep(1)
                    raise KeyboardInterrupt
                self.assertLess(elapsed + seconds, 33, "a stuck owner kept the release lock past recovery")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "sleep", sleep), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "could not confirm exit"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertIn("could not confirm exit", self.state()["in_progress"]["failed"])
        self.assertEqual(2, interrupts)
        self.assertLess(self.ops.clock - term_at, 33)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertFalse(self.ops.switched)

    def test_slow_docker_queries_cannot_extend_the_ordinary_drain(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = float("inf")
        original_run = self.ops.run
        original_inspect = self.ops.inspect
        term_at = None
        slow_inspects = []
        lists = []

        def inspect(name, *, timeout=30):
            if name == old and term_at is not None and self.ops.clock - term_at >= 1.5:
                slow_inspects.append((self.ops.clock - term_at, timeout))
                self.ops.clock += timeout
                raise subprocess.TimeoutExpired("inspect", timeout)
            return original_inspect(name, timeout=timeout)

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            if args[:3] == ["docker", "container", "ls"] and term_at is not None:
                timeout = kwargs["timeout"]
                lists.append(self.ops.clock - term_at)
                self.ops.clock += timeout
                raise subprocess.TimeoutExpired("list", timeout)
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "inspect", inspect), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "could not confirm exit"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertIn("could not confirm exit", self.state()["in_progress"]["failed"])
        self.assertLess(self.ops.clock - term_at, 33)
        start, timeout = slow_inspects[0]
        self.assertLess(start, 2)
        self.assertLess(start + timeout, 2.1)
        self.assertEqual([], lists, "an inspect that consumes the budget leaves no time to list")
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertEqual("unless-stopped", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])

    def run_handoff_without_backup_evidence(self, *, status_available, status_delay=0, exit_after=float("inf")):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = exit_after
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run
        original_http = self.ops.http_json
        term_at = None
        runtime_reads = []

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            return original_run(args, **kwargs)

        def http_json(url, **kwargs):
            if term_at is not None and url.endswith("/internal/sources/runtime"):
                status, body = original_http(url, **kwargs)
                if body and body.get("draining"):
                    runtime_reads.append(self.ops.clock - term_at)
                    self.ops.clock += status_delay
                    return (status, body) if status_available else (0, None)
                return status, body
            return original_http(url, **kwargs)

        error = deploy.FallbackFailed if exit_after == float("inf") else deploy.BackgroundRestored
        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaises(error):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            progress = self.state()["in_progress"]
            if error is deploy.FallbackFailed:
                self.assertIn("could not confirm exit", progress["failed"])
            else:
                self.assertIsNone(progress)
                self.assertIn(["docker", "start", old], self.ops.commands)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertFalse(self.ops.switched)
        self.assertLess(self.ops.clock - term_at, 33)
        return runtime_reads

    def test_delayed_cutoff_status_counts_towards_recovery(self):
        self.assertTrue(self.run_handoff_without_backup_evidence(status_available=True, status_delay=2))

    def test_unavailable_cutoff_status_retries_within_recovery_budget(self):
        reads = self.run_handoff_without_backup_evidence(status_available=False)
        self.assertGreater(len(reads), 1)
        self.assertLess(reads[-1], 32)

    def test_old_owner_exiting_after_unavailable_cutoff_status_is_restored(self):
        self.run_handoff_without_backup_evidence(status_available=False, exit_after=8)

    def test_old_owner_exiting_near_recovery_deadline_is_restored(self):
        self.run_handoff_without_backup_evidence(status_available=True, exit_after=28)

    def test_expired_handoff_limits_restart_policy_update_to_remaining_budget(self):
        old = self.state()["current"]["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = float("inf")
        original_run = self.ops.run
        term_at = None
        update_timeouts = []

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            if args == ["docker", "update", "--restart=unless-stopped", old]:
                self.assertIn("could not confirm exit", self.state()["in_progress"]["failed"])
                timeout = kwargs["timeout"]
                update_timeouts.append(timeout)
                self.ops.clock += timeout
                raise subprocess.TimeoutExpired("update", timeout)
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "run", run), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "Could not restore the restart policy"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertIn("Could not restore the restart policy", self.state()["in_progress"]["failed"])
        self.assertLess(self.ops.clock - term_at, 33)
        self.assertEqual(1, len(update_timeouts))
        self.assertGreater(update_timeouts[0], 0)

    def test_recovered_owner_readiness_preserves_time_to_record_failure(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.cfg.background_ready_timeout = 120
        self.ops.drain_seconds[old] = 8
        self.ops.unready_ports.add(current["ports"]["background"])
        original_run = self.ops.run
        original_save_state = deploy.save_state
        term_at = None
        failure_times = []

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            return original_run(args, **kwargs)

        def save_state(cfg, state):
            if (state.get("in_progress") or {}).get("failed"):
                failure_times.append(self.ops.clock - term_at)
            original_save_state(cfg, state)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(deploy, "save_state", save_state), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "could not restart"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertIn("recovery deadline", self.state()["in_progress"]["failed"])
        self.assertIn(["docker", "start", old], self.ops.commands)
        self.assertFalse(self.ops.switched)
        self.assertLess(failure_times[0], 27.5, "the readiness check consumed the failure reserve")
        self.assertLess(self.ops.clock - term_at, 33)

    def test_late_background_recovery_bounds_serving_cleanup(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        target_serving = [self.cfg.container("a", role) for role in deploy.SERVING_ROLES]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = 30.5
        original_run = self.ops.run
        original_http = self.ops.http_json
        term_at = None
        restarted = False
        cleanup_attempts = []

        def run(args, **kwargs):
            nonlocal term_at, restarted
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
            if args == ["docker", "start", old]:
                restarted = True
            if restarted and args[:3] == ["docker", "rm", "-f"] and args[-1] in target_serving:
                cleanup_attempts.append(args[-1])
                if args[-1] == target_serving[0]:
                    timeout = kwargs.get("timeout", 600)
                    self.ops.clock += min(5, timeout)
                    if timeout < 5:
                        raise subprocess.TimeoutExpired(args, timeout)
            return original_run(args, **kwargs)

        def http_json(url, **kwargs):
            status, body = original_http(url, **kwargs)
            if term_at is not None and isinstance(body, dict) and body.get("role") == "background" and body.get("draining"):
                return 0, None
            return status, body

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaises(deploy.BackgroundRestored):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertLess(self.ops.clock - term_at, 33, "serving cleanup outlasted background recovery")
            self.assertEqual(current, self.state()["current"])
            failure = self.state()["in_progress"]["failed"]
            self.assertIn("serving cleanup", failure)
        self.assertEqual(target_serving[:1], cleanup_attempts)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertFalse(self.ops.switched)

    def test_sigint_to_restart_clients_does_not_abandon_old_background(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = 8
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run
        restart_commands = [["docker", "update", "--restart=always", old], ["docker", "start", old]]
        attempted = []

        def run(args, **kwargs):
            if args in restart_commands:
                self.assertFalse(self.ops.inspect(old)["State"]["Running"])
                attempted.append(args)
                child = "import os, signal; os.kill(os.getppid(), signal.SIGINT); signal.raise_signal(signal.SIGINT)"
                deploy.Ops.run(self.ops, [sys.executable, "-c", child], mutate=True, timeout=5)
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "run", run), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaises(deploy.BackgroundRestored):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(restart_commands, attempted)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertFalse(self.ops.switched)

    def test_ctrl_c_during_recovery_wait_still_restores_background(self):
        old = self.state()["current"]["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = 8
        original_sleep = self.ops.sleep
        interrupted = False

        def sleep(seconds):
            nonlocal interrupted
            exit_at = self.ops.containers[old].get("draining_until")
            if not interrupted and exit_at and exit_at - self.ops.clock < 5:
                interrupted = True
                raise KeyboardInterrupt
            original_sleep(seconds)

        with mock.patch.object(self.ops, "sleep", sleep):
            try:
                with self.assertRaisesRegex(deploy.BackgroundRestored, "is running again"):
                    self.deploy()
            except KeyboardInterrupt:
                self.fail("Ctrl-C abandoned recovery before a background could start")
        self.assertTrue(interrupted)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertIsNone(self.state()["in_progress"])

    def test_undelivered_term_restores_restart_policy_without_waiting(self):
        old = self.state()["current"]["containers"]["background"]
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        self.cfg.background_exit_timeout = 2
        original_run = self.ops.run
        original_sleep = self.ops.sleep
        sent_at = None

        def run(args, **kwargs):
            nonlocal sent_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                sent_at = self.ops.clock
                self.ops.commands.append(list(args))
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def sleep(seconds):
            if sent_at is not None:
                self.assertLess(self.ops.clock - sent_at, 10, "a rejected signal must not wait for exit")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "sleep", sleep):
            with self.assertRaisesRegex(deploy.DeployError, "SIGTERM"):
                self.deploy()
        self.assertIsNotNone(sent_at)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", old], self.ops.commands)
        self.assertFalse(self.ops.switched)
        self.assertIsNone(self.state()["in_progress"])

    def test_failed_term_with_unreadable_old_runtime_releases_publish_lock(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run
        original_http = self.ops.http_json
        original_sleep = self.ops.sleep
        term_at = None

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                self.ops.commands.append(list(args))
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def http_json(*args, **kwargs):
            status, body = original_http(*args, **kwargs)
            if isinstance(body, dict) and body.get("instance_id") == self.ops.containers[old]["Id"]:
                return 0, None
            return status, body

        def sleep(seconds):
            if term_at is not None:
                self.assertLess(self.ops.clock - term_at, 4 * 60 * 60, "unconfirmed SIGTERM kept the release lock")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                mock.patch.object(self.ops, "sleep", sleep), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
        self.assertTrue(self.state()["in_progress"]["failed"])
        self.assertIsNotNone(term_at)
        self.assertLess(self.ops.clock - term_at, 60)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertFalse(self.ops.switched)

    def test_rejected_term_to_already_draining_background_stays_bounded(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.cfg.background_exit_timeout = 2
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run
        original_http = self.ops.http_json
        original_sleep = self.ops.sleep
        term_at = None

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def http_json(*args, **kwargs):
            status, body = original_http(*args, **kwargs)
            if isinstance(body, dict) and body.get("instance_id") == self.ops.containers[old]["Id"]:
                # The internal drain endpoint has already made this instance
                # read-only. It stays running because TERM was rejected.
                body.update(ready=False, draining=True)
            return status, body

        def sleep(seconds):
            if term_at is not None:
                self.assertLess(self.ops.clock - term_at, 7200, "existing drain state must not acknowledge SIGTERM")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                mock.patch.object(self.ops, "sleep", sleep), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
        self.assertIsNotNone(term_at)
        self.assertLess(self.ops.clock - term_at, 60)
        self.assertTrue(self.state()["in_progress"]["failed"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)

    def test_term_confirmation_spends_one_deadline_across_docker_and_runtime_queries(self):
        for inspect_delay, list_delay, expected_probes in (
            (30, 30, ["inspect"]),
            (18, 30, ["inspect", "container"]),
            (28, 0, ["inspect", "container", "runtime"]),
        ):
            with self.subTest(inspect_delay=inspect_delay, list_delay=list_delay), tempfile.TemporaryDirectory() as tmp:
                cfg = make_config(Path(tmp))
                ops = FakeOps(Path(tmp))
                for role, name in LEGACY["containers"].items():
                    ops.add(name, LEGACY["ports"][role])
                    ops.containers[name]["role"] = "background" if role == "background" else "serving"
                deploy.save_state(cfg, {"current": LEGACY, "previous": None, "in_progress": None})
                old = LEGACY["containers"]["background"]
                successor = cfg.container("a", "background")
                original_inspect = ops.inspect
                original_run = ops.run
                original_http = ops.http_json
                term_at = None
                probes = []

                def inspect(name, *args, **kwargs):
                    if name == old and term_at is not None:
                        return deploy.Ops.inspect(ops, name, *args, **kwargs)
                    return original_inspect(name, *args, **kwargs)

                def run(args, **kwargs):
                    nonlocal term_at
                    if args == ["docker", "kill", "--signal=TERM", old]:
                        term_at = ops.clock
                        return subprocess.CompletedProcess(args, 1, "", "signal rejected")
                    if term_at is not None and (args == ["docker", "inspect", old] or args[:3] == ["docker", "container", "ls"]):
                        timeout = kwargs["timeout"]
                        probes.append((args[1], ops.clock - term_at, timeout))
                        delay = inspect_delay if args[1] == "inspect" else list_delay
                        if not delay:
                            return subprocess.CompletedProcess(args, 1, "", "Docker unavailable")
                        ops.clock += min(delay, timeout)
                        raise subprocess.TimeoutExpired(args, min(delay, timeout))
                    return original_run(args, **kwargs)

                def http_json(*args, **kwargs):
                    if term_at is not None:
                        timeout = kwargs.get("timeout", 5)
                        probes.append(("runtime", ops.clock - term_at, timeout))
                        ops.clock += timeout
                        return 0, None
                    return original_http(*args, **kwargs)

                with mock.patch.object(ops, "inspect", inspect), mock.patch.object(ops, "run", run), \
                        mock.patch.object(ops, "http_json", http_json), deploy.release_lock(cfg.lock_file):
                    with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                        deploy.Deployer(cfg, ops).deploy("fixture-image")
                with deploy.release_lock(cfg.lock_file):
                    self.assertTrue(deploy.load_state(cfg)["in_progress"]["failed"])
                self.assertEqual(expected_probes, [probe for probe, _, _ in probes])
                for probe, started, timeout in probes:
                    self.assertLess(started, deploy.HANDOFF_TERM_CONFIRM_SECONDS, probe)
                    self.assertLessEqual(started + timeout, deploy.HANDOFF_TERM_CONFIRM_SECONDS + 0.25, probe)
                self.assertLess(ops.clock - term_at, deploy.HANDOFF_TERM_CONFIRM_SECONDS + 1)
                self.assertEqual("unless-stopped", ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
                self.assertNotIn(["docker", "start", successor], ops.commands)

    def test_term_confirmation_rejects_runtime_response_completed_after_deadline(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        original_run = self.ops.run
        original_inspect = self.ops.inspect
        original_http = self.ops.http_json
        term_at = None
        probes = []

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def inspect(name, **kwargs):
            if name == old and term_at is not None:
                probes.append("inspect")
                if len(probes) == 1:
                    self.ops.clock += 27
                else:
                    # Only a query started outside the deadline would see exit.
                    self.ops.containers[old]["State"].update(Running=False, Status="exited")
            return original_inspect(name, **kwargs)

        case = self

        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

            def read(self):
                case.ops.clock += 2.2
                return b'{"draining":true,"stopping":["BackupService"]}'

        def urlopen(req, timeout):
            # Neither phase times out on its own. The full response is late.
            self.assertGreater(timeout, 2.2)
            self.ops.clock += 2.2
            return Response()

        def http_json(url, **kwargs):
            if term_at is not None and url.endswith(f':{current["ports"]["background"]}/internal/sources/runtime'):
                probes.append("runtime")
                with mock.patch.object(deploy.urllib.request, "urlopen", urlopen):
                    return deploy.Ops.http_json(self.ops, url, **kwargs)
            return original_http(url, **kwargs)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "inspect", inspect), \
                mock.patch.object(self.ops, "http_json", http_json), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                self.deploy()
        self.assertEqual(["inspect", "runtime"], probes)
        self.assertTrue(self.state()["in_progress"]["failed"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)
        self.assertFalse(self.ops.switched)

    def test_sigint_between_confirmation_timeout_and_cleanup_preserves_failure(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        original_run = self.ops.run
        original_http = self.ops.http_json
        term_at = None
        interrupted = False

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def http_json(url, **kwargs):
            if term_at is not None and url.endswith("/internal/sources/runtime"):
                return 200, {"ready": False, "draining": True}
            return original_http(url, **kwargs)

        def trace(frame, event, _arg):
            nonlocal interrupted
            if frame.f_code is not deploy.Deployer.drain_background.__code__:
                return None
            # Deliver an actual SIGINT after the timeout was caught, before
            # the first line in its handler can perform any cleanup.
            if (event == "line" and not interrupted
                    and isinstance(frame.f_locals.get("err"), deploy.BackgroundExitTimeout)):
                self.assertGreaterEqual(self.ops.clock - term_at, deploy.HANDOFF_TERM_CONFIRM_SECONDS)
                interrupted = True
                signal.raise_signal(signal.SIGINT)
            return trace

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                deploy.release_lock(self.cfg.lock_file):
            previous_trace = sys.gettrace()
            try:
                sys.settrace(trace)
                with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                    self.deploy()
            except KeyboardInterrupt:
                self.fail("SIGINT escaped at the handoff timeout before cleanup")
            finally:
                sys.settrace(previous_trace)

        self.assertTrue(interrupted)
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertIn("SIGTERM", self.state()["in_progress"]["failed"])
        self.assertEqual("unless-stopped", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)

    def test_ctrl_c_during_restart_policy_restore_still_records_failed_handoff(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        original_run = self.ops.run
        original_http = self.ops.http_json
        term_rejected = False
        restored = 0

        def run(args, **kwargs):
            nonlocal term_rejected, restored
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_rejected = True
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            if term_rejected and args == ["docker", "update", "--restart=unless-stopped", old]:
                restored += 1
                if restored == 1:
                    self.assertIn("failed", self.state()["in_progress"])
                    raise KeyboardInterrupt
            return original_run(args, **kwargs)

        def http_json(url, **kwargs):
            if term_rejected and url.endswith("/internal/sources/runtime"):
                return 200, {"ready": False, "draining": True}
            return original_http(url, **kwargs)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json), \
                deploy.release_lock(self.cfg.lock_file):
            try:
                with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                    self.deploy()
            except KeyboardInterrupt:
                self.fail("Ctrl-C aborted failure handling before the policy and state were restored")
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertTrue(self.state()["in_progress"]["failed"])
        self.assertEqual(2, restored)
        self.assertEqual("unless-stopped", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", successor], self.ops.commands)

    def test_failed_term_to_unready_candidate_releases_publish_lock(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.ops.unready_ports = {self.cfg.slots["a"]["background"]}
        self.cfg.background_ready_timeout = 2
        self.cfg.background_exit_timeout = 2
        original_run = self.ops.run
        original_sleep = self.ops.sleep
        term_at = None

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", successor]:
                self.ops.commands.append(list(args))
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "signal rejected")
            return original_run(args, **kwargs)

        def sleep(seconds):
            if term_at is not None:
                self.assertLess(self.ops.clock - term_at, 4 * 60 * 60, "failed candidate stop kept the release lock")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "sleep", sleep), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.FallbackFailed, "SIGTERM"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
        self.assertIsNotNone(term_at)
        self.assertLess(self.ops.clock - term_at, 60)
        self.assertTrue(self.state()["in_progress"]["failed"])
        self.assertTrue(self.ops.containers[successor]["State"]["Running"])
        self.assertEqual("unless-stopped", self.ops.containers[successor]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "start", old], self.ops.commands)
        self.assertFalse(self.ops.switched)

    def test_failed_term_response_can_still_be_followed_by_drain(self):
        old = self.state()["current"]["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = deploy.HANDOFF_TERM_CONFIRM_SECONDS + 8
        original_run = self.ops.run
        original_http = self.ops.http_json
        term_at = None

        def run(args, **kwargs):
            nonlocal term_at
            result = original_run(args, **kwargs)
            if args == ["docker", "kill", "--signal=TERM", old]:
                term_at = self.ops.clock
                return subprocess.CompletedProcess(args, 1, "", "response lost")
            return result

        def http_json(*args, **kwargs):
            status, body = original_http(*args, **kwargs)
            if term_at is not None and isinstance(body, dict) and body.get("draining"):
                if self.ops.clock - term_at < 1:
                    body.update(ready=True, draining=False)
                else:
                    remaining = self.ops.containers[old]["draining_until"] - self.ops.clock
                    body.update(stopping=["BackupService"], scheduled_backup_remaining_seconds=max(0, int(remaining)))
            return status, body

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "http_json", http_json):
            self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertFalse(self.ops.containers[old]["State"]["Running"])

    def test_removed_background_allows_handoff_after_docker_confirms_absence(self):
        old = self.state()["current"]["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.ops.drain_seconds[old] = 5
        self.cfg.background_exit_timeout = 2
        original_inspect = self.ops.inspect
        original_run = self.ops.run
        original_sleep = self.ops.sleep
        missing_since = None
        failed_list = False

        def inspect(name, *, timeout=30):
            nonlocal missing_since
            info = original_inspect(name)
            if name == old and info and not info["State"]["Running"]:
                del self.ops.containers[old]
                missing_since = self.ops.clock
                return None
            return info

        def run(args, **kwargs):
            nonlocal failed_list
            if args[:3] == ["docker", "container", "ls"] and missing_since is not None and not failed_list:
                failed_list = True
                return subprocess.CompletedProcess(args, 1, "", "Docker unavailable")
            if args == ["docker", "start", successor]:
                self.assertTrue(failed_list, "a failed inspect alone must not confirm exit")
                self.assertGreater(self.ops.clock, missing_since, "a failed Docker listing must be retried")
            return original_run(args, **kwargs)

        def sleep(seconds):
            if missing_since is not None and not self.ops.containers[successor]["State"]["Running"]:
                self.assertLess(self.ops.clock - missing_since, 12, "Docker confirmed that the old owner is gone")
            original_sleep(seconds)

        with mock.patch.object(self.ops, "inspect", inspect), mock.patch.object(self.ops, "run", run), \
                mock.patch.object(self.ops, "sleep", sleep):
            self.deploy()
        self.assertNotIn(old, self.ops.containers)
        self.assertTrue(self.ops.containers[successor]["State"]["Running"])

    def test_inspect_timeout_after_container_removal_still_confirms_absence(self):
        for removed_before_handoff in (True, False):
            with self.subTest(removed_before_handoff=removed_before_handoff), tempfile.TemporaryDirectory() as tmp:
                cfg = make_config(Path(tmp))
                ops = FakeOps(Path(tmp))
                for role, name in LEGACY["containers"].items():
                    ops.add(name, LEGACY["ports"][role])
                    ops.containers[name]["role"] = "background" if role == "background" else "serving"
                deploy.save_state(cfg, {"current": LEGACY, "previous": None, "in_progress": None})
                old = LEGACY["containers"]["background"]
                successor = cfg.container("a", "background")
                ops.drain_seconds[old] = 2
                removed_at = None
                if removed_before_handoff:
                    del ops.containers[old]
                    removed_at = ops.clock
                original_inspect = ops.inspect
                original_sleep = ops.sleep

                def inspect(name, *, timeout=30):
                    nonlocal removed_at
                    info = original_inspect(name)
                    if name == old:
                        if info and not info["State"]["Running"]:
                            del ops.containers[old]
                            removed_at = ops.clock
                        if removed_at is not None:
                            raise subprocess.TimeoutExpired(["docker", "inspect", name], 30)
                    return info

                def sleep(seconds):
                    if removed_at is not None and not (ops.containers.get(successor) or {}).get("State", {}).get("Running"):
                        self.assertLess(ops.clock - removed_at, 12, "confirmed removal must allow the successor to start")
                    original_sleep(seconds)

                with mock.patch.object(ops, "inspect", inspect), mock.patch.object(ops, "sleep", sleep):
                    deploy.Deployer(cfg, ops).deploy("fixture-image")
                self.assertNotIn(old, ops.containers)
                self.assertTrue(ops.containers[successor]["State"]["Running"])
                self.assertLess(ops.commands.index(["docker", "container", "ls", "--all", "--format", "{{.Names}}"]),
                                ops.commands.index(["docker", "start", successor]))

    def test_sigint_after_disabling_restart_restores_original_policy(self):
        current = self.state()["current"]
        old = current["containers"]["background"]
        successor = self.cfg.container("a", "background")
        self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"] = "always"
        original_run = self.ops.run

        def run(args, **kwargs):
            result = original_run(args, **kwargs)
            if args == ["docker", "update", "--restart=no", old]:
                # Docker has applied the update. Ctrl-C reaches both the
                # deploy process and the client before the client returns.
                child = "import os, signal; os.kill(os.getppid(), signal.SIGINT); signal.raise_signal(signal.SIGINT)"
                return deploy.Ops.run(self.ops, [sys.executable, "-c", child], mutate=True, timeout=5)
            return result

        with mock.patch.object(self.ops, "run", run), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaises(deploy.DeployError):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            self.assertIsNone(self.state()["in_progress"])
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertEqual("always", self.ops.containers[old]["HostConfig"]["RestartPolicy"]["Name"])
        self.assertNotIn(["docker", "kill", "--signal=TERM", old], self.ops.commands)
        self.assertNotIn(["docker", "start", successor], self.ops.commands)

    def test_background_removed_before_restart_policy_change_allows_handoff(self):
        old = self.state()["current"]["containers"]["background"]
        original_run = self.ops.run

        def run(args, **kwargs):
            if args == ["docker", "update", "--restart=no", old]:
                del self.ops.containers[old]
                raise deploy.DeployError("container removed")
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "run", run):
            self.deploy()
        self.assertNotIn(["docker", "kill", "--signal=TERM", old], self.ops.commands)
        active = self.state()["current"]["containers"]["background"]
        self.assertTrue(self.ops.containers[active]["State"]["Running"])
        self.assertNotEqual(active, old)

    def test_rollback_cli_checks_backup_before_changing_slots(self):
        self.deploy()
        before = self.state()
        old = before["current"]["containers"]["background"]
        self.ops.runtime_extra[old] = {"scheduled_backup_remaining_seconds": 0}
        self.ops.commands.clear()
        stderr = io.StringIO()
        with mock.patch.object(deploy, "Ops", return_value=self.ops), \
                mock.patch.object(deploy.Config, "load", return_value=self.cfg), \
                mock.patch.object(sys, "stderr", stderr):
            self.assertEqual(1, deploy.main(["rollback"]))
            self.assertIn("--allow-running-backup", stderr.getvalue())
            self.assertEqual(before, self.state())
            self.assertEqual([], self.ops.commands)
            self.assertEqual(0, deploy.main(["rollback", "--allow-running-backup"]))
        self.assertEqual(before["previous"]["slot"], self.state()["current"]["slot"])

    def test_rollback_timeout_restores_owner_of_current_traffic_slot(self):
        self.deploy()
        current = self.state()["current"]
        old = current["containers"]["background"]
        previous = self.state()["previous"]["containers"]["background"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[old] = 8
        with self.assertRaisesRegex(deploy.DeployError, "previous background .* is running again"):
            deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(current, self.state()["current"])
        self.assertTrue(self.ops.containers[old]["State"]["Running"])
        self.assertFalse(self.ops.containers[previous]["State"]["Running"])
        self.assertIsNone(self.state()["in_progress"])

    def test_guard_handback_timeout_still_restores_the_previous_background(self):
        previous = self.state()["current"]["containers"]["background"]
        new = self.cfg.container("a", "background")
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[new] = 8
        with self.assertRaisesRegex(deploy.DeployError, "guard failed.*previous release serves traffic"):
            self.deploy()
        self.assertEqual(previous, self.state()["current"]["containers"]["background"])
        self.assertTrue(self.ops.containers[previous]["State"]["Running"])
        self.assertFalse(self.ops.containers[new]["State"]["Running"])
        self.assertIsNone(self.state()["in_progress"])

    def run_handback_with_slow_serving_cleanup(self, *, guard_failure):
        current = self.state()["current"]
        previous_bg = current["containers"]["background"]
        candidate_bg = self.cfg.container("a", "background")
        candidate_serving = self.cfg.container("a", "serving")
        candidate_standby = self.cfg.container("a", "standby")
        cleanup = ["docker", "update", "--restart=no"] if guard_failure else ["docker", "rm", "-f"]
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[candidate_bg] = 8
        if guard_failure:
            self.ops.memory_after_switch = 20 * GIB
        else:
            self.ops.nginx_test_rc = 1
        original_run = self.ops.run
        term_at = None
        cleanup_attempts = []

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", candidate_bg]:
                term_at = self.ops.clock
            if term_at is not None and args[:3] == cleanup and args[-1] in (candidate_serving, candidate_standby):
                cleanup_attempts.append(args[-1])
                if args[-1] == candidate_serving:
                    self.assertTrue(self.ops.inspect(previous_bg)["State"]["Running"])
                    self.assertFalse(self.ops.inspect(candidate_bg)["State"]["Running"])
                    timeout = kwargs.get("timeout", 600)
                    self.ops.clock += min(40, timeout)
                    if timeout < 40:
                        raise subprocess.TimeoutExpired(args, timeout)
            return original_run(args, **kwargs)

        reason = "guard failed" if guard_failure else "nginx -t rejected"
        with mock.patch.object(self.ops, "run", run), deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.DeployError, reason):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertIsNotNone(term_at)
            self.assertLess(self.ops.clock - term_at, 33, "serving cleanup outlasted the handback window")
            self.assertEqual(current, self.state()["current"])
            self.assertIn("serving cleanup", self.state()["in_progress"]["failed"])
        self.assertEqual([candidate_serving], cleanup_attempts)
        self.assertTrue(self.ops.containers[previous_bg]["State"]["Running"])
        self.assertFalse(self.ops.containers[candidate_bg]["State"]["Running"])
        if guard_failure:
            ports = current["ports"]
            self.assertEqual(deploy.render_upstreams(ports["serving"], ports["standby"], ports["background"]),
                             self.ops.files[self.cfg.upstream_file])
            self.assertIn(candidate_serving, self.ops.containers)
        else:
            self.assertFalse(self.ops.switched)

    def test_switch_failure_bounds_serving_cleanup_after_background_handback(self):
        self.run_handback_with_slow_serving_cleanup(guard_failure=False)

    def test_guard_failure_bounds_serving_cleanup_after_background_handback(self):
        self.run_handback_with_slow_serving_cleanup(guard_failure=True)

    def run_guard_with_failed_serving_inspect(self, *, removed):
        current = self.state()["current"]
        previous_bg = current["containers"]["background"]
        candidate_bg = self.cfg.container("a", "background")
        candidate_serving = self.cfg.container("a", "serving")
        candidate_standby = self.cfg.container("a", "standby")
        self.cfg.background_exit_timeout = 2
        self.ops.drain_seconds[candidate_bg] = 8
        self.ops.memory_after_switch = 20 * GIB
        original_run = self.ops.run
        original_inspect = self.ops.inspect
        term_at = None
        lists = []

        def inspect(name, **kwargs):
            if term_at is not None and name == candidate_serving:
                return deploy.Ops.inspect(self.ops, name, **kwargs)
            return original_inspect(name, **kwargs)

        def run(args, **kwargs):
            nonlocal term_at
            if args == ["docker", "kill", "--signal=TERM", candidate_bg]:
                term_at = self.ops.clock
                if removed:
                    self.ops.containers.pop(candidate_serving)
            if args == ["docker", "inspect", candidate_serving]:
                return subprocess.CompletedProcess(args, 1, "", "inspect failed")
            if term_at is not None and args[:3] == ["docker", "container", "ls"]:
                lists.append((self.ops.clock - term_at, kwargs["timeout"]))
            return original_run(args, **kwargs)

        with mock.patch.object(self.ops, "run", run), mock.patch.object(self.ops, "inspect", inspect), \
                deploy.release_lock(self.cfg.lock_file):
            with self.assertRaisesRegex(deploy.DeployError, "guard failed.*previous release serves traffic"):
                self.deploy()
        with deploy.release_lock(self.cfg.lock_file):
            self.assertEqual(current, self.state()["current"])
            progress = self.state()["in_progress"]
            if removed:
                self.assertIsNone(progress)
                self.assertFalse(self.ops.containers[candidate_standby]["State"]["Running"])
            else:
                self.assertIsNotNone(progress, "an unreadable serving state must leave a failure record")
                self.assertIn("Incomplete serving cleanup", progress["failed"])
                self.assertIn(candidate_serving, progress["failed"])
                self.assertTrue(self.ops.containers[candidate_serving]["State"]["Running"])
                self.assertTrue(self.ops.containers[candidate_standby]["State"]["Running"])
        self.assertTrue(self.ops.containers[previous_bg]["State"]["Running"])
        self.assertFalse(self.ops.containers[candidate_bg]["State"]["Running"])
        self.assertEqual(1, len(lists))
        started, timeout = lists[0]
        self.assertLess(started + timeout, 33, "the container list must share the handback deadline")

    def test_guard_records_failure_when_serving_inspect_cannot_confirm_state(self):
        self.run_guard_with_failed_serving_inspect(removed=False)

    def test_guard_skips_serving_only_when_docker_confirms_removal(self):
        self.run_guard_with_failed_serving_inspect(removed=True)

    def test_a_reply_cut_off_mid_way_never_ends_the_handoff(self):
        # The old owner exits while answering a runtime read: the progress note
        # and the pre-signal read must not raise out of the handoff.
        self.ops.drain_seconds["old-bg"] = 70
        real = self.ops.http_json
        broken = iter([http.client.IncompleteRead(b"{\"ready\": tr"), ValueError("truncated JSON"),
                       http.client.BadStatusLine("")] * 10)

        def http_json(url, token=None, method="GET", body=None, timeout=5):
            if url == "http://127.0.0.1:9430/internal/sources/runtime":
                raise next(broken)
            return real(url, token=token, method=method, body=body, timeout=timeout)

        self.ops.http_json = http_json
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])
        text = "\n".join(self.ops.logs)
        self.assertIn("the admin API is unavailable", text)
        self.assertIn("old-bg still finishing in-flight work ...", text)

    def test_rollback_reuses_previous_containers(self):
        self.deploy()
        created_before = len(self.created())
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(created_before, len(self.created()), "rollback creates nothing")
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertTrue(self.ops.containers["old-serving"]["State"]["Running"])
        self.assertTrue(self.ops.containers["old-bg"]["State"]["Running"])
        self.assertFalse(self.ops.containers["sources-main-a-background"]["State"]["Running"])
        self.assertIn("127.0.0.1:9426 max_fails", self.ops.files[self.cfg.upstream_file])

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
