import http.client
import json
import os
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

    def inspect(self, name):
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

    def test_rejected_upstream_file_restores_previous_file_state(self):
        # FakeOps is the filesystem here. The host path never has an old file.
        path = self.cfg.upstream_file = str(self.tmp / "upstreams.conf")
        self.ops.nginx_test_rc = 1
        deployer = deploy.Deployer(self.cfg, self.ops)
        for previous in ("previous config\n", None):
            with self.subTest(had_file=previous is not None):
                if previous is None:
                    self.ops.files.pop(path, None)
                else:
                    self.ops.files[path] = previous
                with self.assertRaisesRegex(deploy.DeployError, "nginx -t rejected"):
                    deployer.install_upstreams("candidate config\n")
                self.assertEqual(previous, self.ops.files.get(path))
                self.assertFalse(self.ops.switched)

    def test_migration_command_timeout_clears_progress_and_records_failure(self):
        previous = {**self.cfg.slot_view("a"), "image": "previous@sha256:1"}
        deploy.save_state(self.cfg, {**self.state(), "previous": previous})
        for role, name in previous["containers"].items():
            self.ops.add(name, previous["ports"][role], running=False)
        run = self.ops.run

        def timeout_migration(args, **kwargs):
            if (args[:3] == ["docker", "start", "--attach"]
                    and self.ops.containers[args[-1]]["cmd"][0] == "-migrate-only"):
                with mock.patch.object(deploy.subprocess, "run",
                                       side_effect=subprocess.TimeoutExpired(args, kwargs["timeout"])):
                    return deploy.Ops.run(self.ops, args, **kwargs)
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "run", side_effect=timeout_migration):
            with self.assertRaisesRegex(deploy.DeployError, "timed out"):
                self.deploy()
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual(previous, self.state()["previous"])
        self.assertTrue(set(previous["containers"].values()) <= self.ops.containers.keys())
        self.assertFalse(any("oneoff" in name for name in self.ops.containers))
        record, = self.history()
        self.assertEqual("failed", record["result"])
        self.assertIn("timed out", record["error"])

    def test_reload_timeout_keeps_candidates_when_routing_is_unconfirmed(self):
        run = self.ops.run

        def timeout_after_reload(args, **kwargs):
            result = run(args, **kwargs)
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                with mock.patch.object(deploy.subprocess, "run",
                                       side_effect=subprocess.TimeoutExpired(args, 600)):
                    return deploy.Ops.run(self.ops, args, **kwargs)
            return result

        with mock.patch.object(self.ops, "run", side_effect=timeout_after_reload):
            with self.assertRaisesRegex(deploy.FallbackFailed, "routing is unconfirmed"):
                self.deploy()
        self.assertTrue(self.ops.switched)
        for name in self.cfg.slot_view("a")["containers"].values():
            self.assertTrue(self.ops.containers[name]["State"]["Running"])
        self.assertEqual("switch", self.state()["in_progress"]["step"])
        self.assertIn("routing is unconfirmed", self.state()["in_progress"]["failed"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_failed_handoff_restores_previous_background_retry_limit(self):
        old = LEGACY["containers"]["background"]
        self.ops.containers[old]["HostConfig"]["RestartPolicy"] = {"Name": "on-failure", "MaximumRetryCount": 3}
        self.ops.unready_ports.add(self.cfg.slots["a"]["background"])
        with self.assertRaisesRegex(deploy.DeployError, "is running again"):
            self.deploy()
        updates = [cmd[2] for cmd in self.ops.commands if cmd[:2] == ["docker", "update"] and cmd[-1] == old]
        self.assertEqual(["--restart=no", "--restart=on-failure:3"], updates)
        self.assertTrue(self.ops.containers[old]["State"]["Running"])

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
        for part in ("old-bg is still draining after 2100s", "nothing was killed",
                     "sources-main-a-background was not started", "Traffic is on slot legacy",
                     "no background owner runs",
                     "Left running without traffic: sources-main-a-serving, sources-main-a-standby",
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
        self.assertEqual("background handoff", in_progress["step"])
        self.assertEqual(message, in_progress["failed"])
        self.assertIn("failed_at", in_progress)
        # 'status' shows it, and the next deploy refuses until a person clears it.
        report = deploy.status(self.cfg, self.ops)
        self.assertEqual(message, report["in_progress"]["failed"])
        self.assertEqual("running", report["current"]["containers"]["background"]["status"])
        self.assertEqual("no", report["current"]["containers"]["background"]["restart_policy"])
        self.assertIn("FAILED AND NOT RESTORED", "\n".join(self.ops.logs))
        commands = len(self.ops.commands)
        with self.assertRaises(deploy.DeployError) as again:
            self.deploy()
        self.assertIn("did not finish", str(again.exception))
        self.assertIn("Do not deploy again", str(again.exception))
        self.assertEqual(commands, len(self.ops.commands))

    def test_rollback_with_an_owner_that_never_exits_keeps_in_progress(self):
        self.deploy()
        self.ops.drain_seconds["sources-main-a-background"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).rollback()
        in_progress = self.state()["in_progress"]
        self.assertEqual("background handoff", in_progress["step"])
        # Traffic is still on slot a while its background drains; the restarted
        # previous serving containers have none.
        for part in ("sources-main-a-background is still draining", "Traffic is on slot a.",
                     "Left running without traffic: old-serving, old-spare",
                     "'docker start sources-main-a-background'"):
            self.assertIn(part, in_progress["failed"])

    def test_handing_back_to_an_owner_that_never_exits_names_the_previous_background(self):
        # Guard fails, traffic goes back to the previous slot, and the new
        # background never exits: the person must start the previous release's
        # background, not restart the newer one.
        self.ops.memory_after_switch = 20 * GIB
        self.ops.drain_seconds["sources-main-a-background"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed) as ctx:
            self.deploy()
        message = str(ctx.exception)
        for part in ("sources-main-a-background is still draining", "old-bg was not started",
                     "Traffic is on slot legacy",
                     "Left running without traffic: sources-main-a-serving, sources-main-a-standby",
                     "start the background of slot legacy (not sources-main-a-background): "
                     "'docker update --restart=unless-stopped old-bg' and 'docker start old-bg'"):
            self.assertIn(part, message)
        self.assertNotIn("docker start sources-main-a-background", message)
        self.assertIn("127.0.0.1:9426 max_fails", self.ops.files[self.cfg.upstream_file], "traffic is back")
        self.assertEqual(message, self.state()["in_progress"]["failed"])

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
