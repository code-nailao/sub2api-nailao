import copy
import hashlib
import http.client
import io
import json
import os
import re
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
        # Upstream file content Nginx was last told to load. A route write is
        # only observable through this, not through the file on disk (which is
        # written before `nginx -t` and may be restored when the test fails).
        self.upstream_file: str | None = None
        self.loaded_upstreams: str | None = None
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
            if self.upstream_file is not None:
                self.loaded_upstreams = self.files.get(self.upstream_file)
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
        self.ops.upstream_file = self.cfg.upstream_file
        for role, name in LEGACY["containers"].items():
            self.ops.add(name, LEGACY["ports"][role])
            self.ops.containers[name]["role"] = "background" if role == "background" else "serving"
        self.ops.files[self.cfg.upstream_file] = self.upstreams_for(LEGACY)
        self.ops.loaded_upstreams = self.ops.files[self.cfg.upstream_file]
        deploy.save_state(self.cfg, {"current": LEGACY, "previous": None, "in_progress": None,
                                     "slots": {"legacy": LEGACY}})

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

    def upstreams_for(self, view):
        return deploy.render_upstreams(*(view["ports"][role] for role in deploy.ROLES))

    def test_render_upstreams_uses_fixed_names(self):
        text = deploy.render_upstreams(9401, 9402, 9403)
        self.assertIn("upstream sources_main_serving", text)
        self.assertIn("server 127.0.0.1:9402 backup", text)
        self.assertIn("upstream sources_main_admin", text)
        self.assertIn("server 127.0.0.1:9403;", text)

    def test_render_upstreams_omits_a_backup_that_is_not_written(self):
        text = deploy.render_upstreams(9401, None, 9403)
        self.assertNotIn("backup", text)
        self.assertEqual(([9401], 9403), deploy.loaded_upstream_ports(text))

    def test_admin_only_write_leaves_the_loaded_serving_block_alone(self):
        text = deploy.render_upstreams(9401, 9402, 9403)
        updated = deploy.replace_admin_port(text, 9413)
        self.assertEqual(([9401, 9402], 9413), deploy.loaded_upstream_ports(updated))
        self.assertEqual(text.replace("127.0.0.1:9403;", "127.0.0.1:9413;"), updated)

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

    def test_main_without_hold_preserves_the_baseline_command_sequence(self):
        # Captured on the pre-hold baseline with this fixture, first from
        # legacy and then between fixed slots. Include Docker inspect calls.
        baseline = (
            "58117aa3fa340e1d466a7f8479ae5f5edd1234874a318c1ecab335f7e7d37b81",
            "6744365c378d13dc26b97acb463ca1026fc453433b078e5f2a9cf9239cedecf3",
        )
        inspect = self.ops.inspect

        def record_inspect(name, *, timeout=30):
            self.ops.commands.append(["docker", "inspect", name])
            return inspect(name, timeout=timeout)

        with mock.patch.object(self.ops, "inspect", record_inspect), \
                mock.patch.object(deploy, "now", return_value="2020-01-01T00:00:00+00:00"):
            for image, expected in zip(("new@sha256:2", "other@sha256:3"), baseline):
                with self.subTest(image=image):
                    self.ops.commands.clear()
                    self.deploy(image=image)
                    commands = [
                        [re.sub(r"oneoff-\d+-\d+", "oneoff", arg.replace(self._tmp.name, "<TMP>")) for arg in args]
                        for args in self.ops.commands
                    ]
                    self.assertEqual(69, len(commands))
                    self.assertEqual(expected, hashlib.sha256(json.dumps(commands).encode()).hexdigest(), commands)

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
        expected = self.upstreams_for({"ports": {**LEGACY["ports"],
                                                 "background": target["ports"]["background"]}})
        self.assertEqual(expected, self.ops.loaded_upstreams)
        self.assertEqual(expected, self.ops.files[self.cfg.upstream_file])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        log = "\n".join(self.ops.logs)
        self.assertIn("FAILED AND NOT RESTORED", log)
        self.assertIn("restore one background", log)

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

    def failed_handback_admin_owner(self, *, previous_stays_up):
        """A drain timeout ends the hand-back with a Docker error while the
        candidate's background cannot take the lock back."""
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_exit_timeout = 2
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]
        candidate_admin = target["ports"]["background"]
        previous_background = LEGACY["containers"]["background"]
        previous_admin = LEGACY["ports"]["background"]
        self.ops.drain_seconds[candidate_background] = 8
        run = self.ops.run
        started_previous = False

        def start(args, **kw):
            nonlocal started_previous
            result = run(args, **kw)
            if args == ["docker", "start", previous_background]:
                started_previous = True
                if not previous_stays_up:
                    self.ops.containers[previous_background]["State"].update(
                        Running=False, Status="exited", ExitCode=1)
                raise subprocess.TimeoutExpired(args, 600)
            if args == ["docker", "start", candidate_background] and started_previous:
                self.ops.containers[candidate_background]["State"].update(
                    Running=False, Status="exited", ExitCode=1)
            return result

        with mock.patch.object(self.ops, "run", start):
            with self.assertRaises(deploy.FallbackFailed) as ctx:
                self.deploy()
        upstream = self.ops.files[self.cfg.upstream_file]
        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("handing the background back to slot legacy did not finish", progress["failed"])
        return ctx, upstream, candidate_admin, previous_admin, target

    def test_guard_rollback_selects_the_previous_background_that_still_answers(self):
        ctx, upstream, candidate_admin, previous_admin, target = self.failed_handback_admin_owner(
            previous_stays_up=True)
        message = str(ctx.exception)
        self.assertIn(f"127.0.0.1:{previous_admin};", upstream,
                      "admin moves to the background that holds the lock and answers")
        self.assertNotIn(f"127.0.0.1:{candidate_admin};", upstream)
        self.assertIn("admin was moved to slot legacy, whose background answers", message)
        deployer = deploy.Deployer(self.cfg, self.ops)
        self.assertTrue(deployer.background_answering(LEGACY))
        self.assertFalse(deployer.background_answering(target))

    def test_guard_rollback_keeps_the_current_admin_pointer_when_no_background_answers(self):
        ctx, upstream, candidate_admin, previous_admin, target = self.failed_handback_admin_owner(
            previous_stays_up=False)
        message = str(ctx.exception)
        self.assertIn(f"127.0.0.1:{candidate_admin};", upstream, "the admin upstream is left where it is")
        self.assertNotIn(f"127.0.0.1:{previous_admin};", upstream)
        self.assertIn("No background answers admin now", message)
        self.assertIn("admin entry is unavailable", message)
        self.assertIn("needs a person", message)
        deployer = deploy.Deployer(self.cfg, self.ops)
        self.assertFalse(deployer.background_answering(LEGACY))
        self.assertFalse(deployer.background_answering(target))

    def test_guard_rollback_keeps_admin_on_the_candidate_after_a_restart_timeout(self):
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_exit_timeout = 2
        self.cfg.background_ready_timeout = 6
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]
        candidate_admin = target["ports"]["background"]
        previous_background = LEGACY["containers"]["background"]
        previous_admin = LEGACY["ports"]["background"]
        self.ops.drain_seconds[candidate_background] = 8
        started_previous = False

        run, http_json = self.ops.run, self.ops.http_json

        def start(args, **kw):
            nonlocal started_previous
            result = run(args, **kw)
            if args == ["docker", "start", previous_background]:
                started_previous = True
            if args == ["docker", "start", candidate_background] and started_previous:
                # The restart times out even though the candidate came up.
                raise subprocess.TimeoutExpired(args, 600)
            return result

        def probe(url, **kw):
            if started_previous and url.endswith(f":{LEGACY['ports']['background']}/internal/sources/runtime"):
                status, body = http_json(url, **kw)
                if status == 200 and isinstance(body, dict):
                    return status, {**body, "owns_background": False}
                return status, body
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "http_json", probe):
            with self.assertRaises(deploy.FallbackFailed) as ctx:
                self.deploy()
            deployer = deploy.Deployer(self.cfg, self.ops)
            self.assertTrue(deployer.background_answering(target), "the re-probe confirms the restarted candidate")
            self.assertFalse(deployer.background_answering(LEGACY))

        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{candidate_admin};", upstream)
        self.assertNotIn(f"127.0.0.1:{previous_admin};", upstream,
                         "admin must not move to a background that lost the lock")
        self.assertIn(f"admin still uses slot {target['slot']}", str(ctx.exception))
        self.assertEqual("guard", self.state()["in_progress"]["step"])

    def test_guard_rollback_keeps_admin_off_a_missing_previous_background(self):
        self.ops.memory_after_switch = 20 * GIB
        self.cfg.background_exit_timeout = 2
        self.cfg.background_ready_timeout = 6
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]
        candidate_admin = target["ports"]["background"]
        previous_background = LEGACY["containers"]["background"]
        previous_admin = LEGACY["ports"]["background"]
        self.ops.drain_seconds[candidate_background] = 8
        run = self.ops.run
        handback_failed = False
        candidate_restarts = 0

        def start(args, **kw):
            nonlocal handback_failed, candidate_restarts
            if args == ["docker", "start", previous_background]:
                handback_failed = True
                del self.ops.containers[previous_background]
                raise deploy.DeployError("fixture: previous background is missing")
            if args == ["docker", "start", candidate_background] and handback_failed:
                candidate_restarts += 1
                if candidate_restarts == 1:
                    raise subprocess.TimeoutExpired(args, 600)  # the first restart fails
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", start):
            with self.assertRaises(deploy.FallbackFailed) as ctx:
                self.deploy()

        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{candidate_admin};", upstream)
        self.assertNotIn(f"127.0.0.1:{previous_admin};", upstream,
                         "admin must not point at a background that no longer exists")
        self.assertIn("container is missing", str(ctx.exception))
        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("failed", progress)
        # The candidate stays the only restorable owner: starting it answers admin.
        self.ops.run(["docker", "update", "--restart=unless-stopped", candidate_background], mutate=True)
        self.ops.run(["docker", "start", candidate_background], mutate=True)
        self.assertTrue(deploy.Deployer(self.cfg, self.ops).background_answering(target))

    def test_guard_rollback_records_failure_when_a_probe_times_out(self):
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]
        candidate_admin = target["ports"]["background"]
        previous_background = LEGACY["containers"]["background"]
        previous_admin = LEGACY["ports"]["background"]
        run, inspect = self.ops.run, self.ops.inspect
        probe_timed_out = False

        def start(args, **kw):
            nonlocal probe_timed_out
            if args == ["docker", "start", previous_background]:
                probe_timed_out = True
                raise deploy.DeployError("fixture: previous background cannot start")
            if args == ["docker", "start", candidate_background] and probe_timed_out:
                raise deploy.DeployError("fixture: candidate background cannot restart")
            return run(args, **kw)

        def check(name, **kw):
            if probe_timed_out and name == previous_background:
                raise subprocess.TimeoutExpired(["docker", "inspect", name], 30)
            return inspect(name, **kw)

        with mock.patch.object(self.ops, "run", start), mock.patch.object(self.ops, "inspect", check):
            with self.assertRaises(Exception) as ctx:
                self.deploy()

        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("failed", progress, "a probe timeout must still record the failure")
        self.assertIn("failed_at", progress)
        self.assertIn("timed out", progress["failed"], "the reason must name the failed probe")
        self.assertIsInstance(ctx.exception, deploy.FallbackFailed)
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{candidate_admin};", upstream)
        self.assertNotIn(f"127.0.0.1:{previous_admin};", upstream)

    def test_guard_admin_owner_survives_an_unreadable_docker_state(self):
        """Docker cannot report the background, yet it holds the lock and
        answers /health. Ownership comes from the probes, not Docker's view."""
        self.ops.memory_after_switch = 20 * GIB
        deployer = deploy.Deployer(self.cfg, self.ops)

        def unreadable(name, **kw):
            raise subprocess.TimeoutExpired(["docker", "inspect", name], 30)

        with mock.patch.object(self.ops, "inspect", unreadable):
            owner, why = deployer.admin_owner_status(LEGACY)
        self.assertTrue(owner, why)
        self.assertIn("holds the background lock and answers", why)

    def test_guard_interrupt_still_records_the_failure(self):
        """Ctrl-C anywhere in the cleanup must record the failure first."""
        self.ops.memory_after_switch = 20 * GIB
        with mock.patch.object(deploy.Deployer, "hand_back",
                               side_effect=deploy.DeployError("fixture: drain timeout")), \
                mock.patch.object(deploy.Deployer, "settle_admin_owner",
                                  side_effect=KeyboardInterrupt):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()

        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["step"])
        self.assertIn("failed", progress, "an interrupt must still record the failure")
        self.assertIn("failed_at", progress)

    # -- invariants of the guard rollback route ------------------------------
    def test_guard_rollback_is_not_written_when_the_previous_primary_stops_answering(self):
        """Invariant 1: a route decision holds only at the probe before the
        write. If the previous primary is gone by then, nothing is written and
        the loaded candidate route stays."""
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        http_json = self.ops.http_json

        def probe(url, **kw):
            # The legacy primary answered until traffic switched; by the time
            # the rollback probes it, it no longer does.
            if self.ops.switched and url.startswith(f"http://127.0.0.1:{LEGACY['ports']['serving']}/"):
                return 0, None
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "http_json", probe):
            with self.assertRaisesRegex(deploy.FallbackFailed, "rollback service route was not written"):
                self.deploy()
        self.assertEqual(self.upstreams_for(target), self.ops.loaded_upstreams,
                         "the failing candidate route is what stayed loaded")
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertIn("verify slot legacy's serving containers", progress["failed"])

    def test_guard_rollback_records_a_standby_that_exits_after_the_route_is_written(self):
        """Invariant 2: a role that was probed into the route and then exits is
        found again in the same round, with slot, role and time, and progress
        is not cleared."""
        self.ops.memory_after_switch = 20 * GIB
        write = self.ops.write_file_atomic

        def stop_standby_after_write(path, text, **kw):
            result = write(path, text, **kw)
            if path == self.cfg.upstream_file and f"127.0.0.1:{LEGACY['ports']['serving']} max_fails" in text:
                self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")
            return result

        with mock.patch.object(self.ops, "write_file_atomic", stop_standby_after_write):
            with self.assertRaisesRegex(deploy.FallbackFailed, "before clearing in_progress"):
                self.deploy()

        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        issues = progress["route_issues"]
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "standby" and i.get("found_at")
                            for i in issues), issues)
        self.assertEqual(progress, deploy.status(self.cfg, self.ops)["in_progress"])
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails",
                      self.ops.files[self.cfg.upstream_file], "healthy primary still serves")
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a["alert"] == ["in_progress"] and a.get("route_issues") for a in alerts), alerts)

    def test_guard_rollback_keeps_the_record_of_a_role_that_recovers_before_the_next_probe(self):
        """B segment: a role that exits between probes and comes back before
        anyone looks again still leaves slot, role and time behind."""
        self.ops.memory_after_switch = 20 * GIB
        candidate_serving = self.cfg.container("a", "serving")
        write, run = self.ops.write_file_atomic, self.ops.run

        def stop_after_write(path, text, **kw):
            result = write(path, text, **kw)
            if path == self.cfg.upstream_file and f"127.0.0.1:{LEGACY['ports']['serving']} max_fails" in text:
                self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")
            return result

        def restart_during_retirement(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "kill", "--signal=TERM", candidate_serving]:
                self.ops.containers["old-spare"]["State"].update(Running=True, Status="running")
            return result

        with mock.patch.object(self.ops, "write_file_atomic", stop_after_write), \
                mock.patch.object(self.ops, "run", restart_during_retirement):
            with self.assertRaisesRegex(deploy.FallbackFailed, "before clearing in_progress"):
                self.deploy()

        self.assertTrue(self.ops.containers["old-spare"]["State"]["Running"],
                        "the role is back, but nobody verified the route since it dropped")
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        recorded = [i for i in progress["route_issues"] if i["slot"] == "legacy" and i["role"] == "standby"]
        self.assertEqual(1, len(recorded))
        self.assertTrue(recorded[0]["found_at"])
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a["alert"] == ["in_progress"] and a.get("route_issues") for a in alerts), alerts)

    def test_guard_rollback_reports_the_admin_owner_that_exits_during_retirement(self):
        """Invariant 2 and 5: the admin entry must still name a probe-backed
        owner after the last step. If it exits there, the finding is kept and
        the slot to restore is named."""
        self.ops.memory_after_switch = 20 * GIB
        candidate_serving = self.cfg.container("a", "serving")
        run = self.ops.run

        def stop_owner_during_retirement(args, **kw):
            result = run(args, **kw)
            if args == ["docker", "kill", "--signal=TERM", candidate_serving]:
                self.ops.containers["old-bg"]["State"].update(Running=False, Status="exited")
            return result

        with mock.patch.object(self.ops, "run", stop_owner_during_retirement):
            with self.assertRaisesRegex(deploy.FallbackFailed, "before clearing in_progress"):
                self.deploy()

        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "background"
                            for i in progress["route_issues"]), progress["route_issues"])
        admin = progress["admin_route"]
        self.assertEqual("legacy", admin["slot"])
        self.assertEqual(LEGACY["ports"]["background"], admin["port"])
        self.assertEqual("probed", admin["source"])
        self.assertEqual("answers", admin["probe"]["outcome"],
                         "the move was justified by this probe; the later exit is the finding")
        log = "\n".join(self.ops.logs)
        self.assertIn(f"admin route written: slot legacy port {LEGACY['ports']['background']}", log)
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails",
                      self.ops.files[self.cfg.upstream_file], "user traffic still reaches the healthy primary")

    def test_guard_rollback_serves_from_the_previous_primary_when_its_standby_is_down(self):
        """Invariant 3: the standby only decides whether it is written. The
        primary alone brings traffic back."""
        self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")
        self.ops.memory_after_switch = 20 * GIB
        with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
            self.deploy()
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['standby']}", upstream,
                         "a standby that did not answer is not written as a backup")
        self.assertEqual(upstream, self.ops.loaded_upstreams)
        self.assertIsNone(self.state()["in_progress"],
                          "the rollback completed; the missing backup is not an unfinished recovery")

    def test_route_write_includes_a_standby_only_after_it_answers_again(self):
        """Invariant 4: usability is a probe result, not a property. The same
        slot is excluded and then included across two writes."""
        deployer = deploy.Deployer(self.cfg, self.ops)
        self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")
        omitted = deployer.publish_routes(LEGACY)
        self.assertFalse(omitted.probes["standby"].usable)
        self.assertEqual("stopped", omitted.probes["standby"].outcome)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['standby']}", deployer.current_upstreams())

        self.ops.containers["old-spare"]["State"].update(Running=True, Status="running")
        included = deployer.publish_routes(LEGACY)
        self.assertTrue(included.probes["standby"].usable)
        self.assertEqual("answers", included.probes["standby"].outcome)
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['standby']} backup", deployer.current_upstreams())

    def test_guard_rollback_names_the_probe_that_justified_the_admin_move(self):
        """Invariant 5: every admin move states which probe allowed it."""
        self.ops.memory_after_switch = 20 * GIB
        with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
            self.deploy()
        log = "\n".join(self.ops.logs)
        self.assertIn(f"admin route written: slot legacy port {LEGACY['ports']['background']}", log)
        self.assertIn("holds the background lock and answers /health 200", log)

    def test_guard_rollback_status_names_the_background_to_restore_when_none_answers(self):
        """Invariant 5: no owner is reported as such, never as "unconfirmed"."""
        ctx, _, _, _, _ = self.failed_handback_admin_owner(previous_stays_up=False)
        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertEqual("no background answers", progress["admin_route"]["source"])
        self.assertEqual("legacy", progress["admin_route"]["restore"])
        self.assertIn("restore one background", str(ctx.exception))
        self.assertIn("admin entry is unavailable", str(ctx.exception))

    def test_role_probe_separates_timeout_refusal_and_unreadable_state(self):
        """The three failure shapes are reported separately: a timeout, an
        exited container and a Docker read that never answered."""
        deployer = deploy.Deployer(self.cfg, self.ops)
        http_json = self.ops.http_json

        def slow(url, **kw):
            if url.startswith(f"http://127.0.0.1:{LEGACY['ports']['serving']}/"):
                self.ops.clock += deploy.RUNTIME_STATUS_TIMEOUT_SECONDS + 1
                return 0, None
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "http_json", slow):
            timed_out = deployer.probe_role(LEGACY, "serving")
        self.assertFalse(timed_out.usable)
        self.assertEqual("timeout", timed_out.outcome)
        self.assertIn("probe timeout", timed_out.detail)

        self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")
        stopped = deployer.probe_role(LEGACY, "standby")
        self.assertFalse(stopped.usable)
        self.assertEqual("stopped", stopped.outcome)

        self.ops.containers.pop("old-bg")
        missing = deployer.probe_role(LEGACY, "background")
        self.assertFalse(missing.usable)
        self.assertEqual("missing", missing.outcome)

        inspect = self.ops.inspect

        def unreadable(name, **kw):
            if name == "old-serving":
                raise subprocess.TimeoutExpired(["docker", "inspect", name], 30)
            return inspect(name, **kw)

        with mock.patch.object(self.ops, "inspect", unreadable), \
                mock.patch.object(self.ops, "http_json", slow):
            unknown = deployer.probe_role(LEGACY, "serving")
        self.assertFalse(unknown.usable)
        self.assertEqual("state unknown", unknown.outcome)
        self.assertIn("unreadable", unknown.detail)

    # -- the three rollback paths share the same write gate -------------------
    def test_manual_rollback_writes_a_verified_route_and_clears_progress(self):
        self.deploy()
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertEqual(self.upstreams_for(LEGACY), self.ops.loaded_upstreams)
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_manual_rollback_keeps_progress_when_a_written_role_exits(self):
        self.deploy()
        current_serving = self.cfg.container("a", "serving")
        run = self.ops.run

        def stop_primary_during_retirement(args, **kw):
            result = run(args, **kw)
            # The rollback route is loaded and the guard passed; the previous
            # primary exits while the old slot is being retired.
            if args == ["docker", "kill", "--signal=TERM", current_serving]:
                self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")
            return result

        with mock.patch.object(self.ops, "run", stop_primary_during_retirement):
            with self.assertRaisesRegex(deploy.FallbackFailed, "before clearing in_progress"):
                deploy.Deployer(self.cfg, self.ops).rollback()
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "serving"
                            for i in progress["route_issues"]), progress["route_issues"])

    def test_normal_release_probes_both_upstreams_before_clearing_progress(self):
        self.deploy()
        target = self.state()["current"]
        self.assertEqual(self.upstreams_for(target), self.ops.loaded_upstreams)
        self.assertEqual({"a", "legacy"}, set(self.state()["slots"]))
        log = "\n".join(self.ops.logs)
        self.assertIn("route written: serving slot a port 9401", log)
        self.assertIn("admin slot a port 9403", log)
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("ok", self.history()[-1]["result"])

    # -- property: no dead port survives a route write -------------------------
    # The review findings are one family: a probe goes stale between the moment
    # it is taken and the moment its answer is used. One property, swept over
    # every (role x probe point), instead of one test per reproduction.
    PROPERTY_FLOWS = ("normal release", "guard rollback", "manual rollback")

    def property_views(self):
        """Every slot a flow can write into, with its ports and containers."""
        views = {}
        for view in (LEGACY, self.cfg.slot_view("a")):
            views.setdefault(view["slot"], view)
        state = self.state()
        for view in (state.get("slots") or {}).values():
            views.setdefault(view["slot"], view)
        return views

    def property_flow(self, flow):
        """One whole flow. A guard failure is the flow's own outcome, so the
        error a person would see is returned instead of raised."""
        if flow == "guard rollback":
            self.ops.memory_after_switch = 20 * GIB
        try:
            self.deploy()
            if flow == "manual rollback":
                deploy.Deployer(self.cfg, self.ops).rollback()
        except deploy.DeployError as err:
            return err
        return None

    def property_port_index(self):
        views = self.property_views()
        return {view["ports"][role]: (view["slot"], role)
                for view in views.values() for role in deploy.ROLES}

    def property_probe_counts(self, flow):
        """How many HTTP probes the flow makes to each role's port. Every one
        of them is a probe point the property has to stop a role at."""
        counts: dict[tuple[str, str], int] = {}
        index = self.property_port_index()
        real = self.ops.http_json

        def counting(url, **kw):
            if url.startswith("http://127.0.0.1:"):
                port = int(url.split(":")[2].split("/")[0])
                if port in index:
                    counts[index[port]] = counts.get(index[port], 0) + 1
            return real(url, **kw)

        with mock.patch.object(self.ops, "http_json", counting):
            self.property_flow(flow)
        return counts

    def property_run(self, flow, slot, role, index, after):
        """Stop one role at one probe point, then run the whole flow.

        `after` stops it just after the given probe answers, so the write or
        the next settlement probe is the one that has to notice; otherwise it
        stops just before that probe, which then answers "not there"."""
        views = self.property_views()
        view = views[slot]
        port, name = view["ports"][role], view["containers"][role]
        writes: list[str] = []
        real_write, real_http = self.ops.write_file_atomic, self.ops.http_json
        seen = {"probe": 0, "stopped": False}

        def stop():
            container = self.ops.containers.get(name)
            if container is not None:
                seen["stopped"] = True
                container["State"].update(Running=False, Status="exited")

        def tracked_write(path, text, **kw):
            if path == self.cfg.upstream_file:
                writes.append(text)
            return real_write(path, text, **kw)

        def probe(url, **kw):
            if url.startswith(f"http://127.0.0.1:{port}/"):
                n = seen["probe"]
                seen["probe"] += 1
                if not after and n == index:
                    stop()
                result = real_http(url, **kw)
                if after and n == index:
                    stop()
                return result
            return real_http(url, **kw)

        with mock.patch.object(self.ops, "write_file_atomic", tracked_write), \
                mock.patch.object(self.ops, "http_json", probe):
            error = self.property_flow(flow)
        return {"error": error, "writes": writes, "stopped": seen["stopped"]}

    def assert_reported(self, slot, role, label):
        """B segment: status and the resident watch both name the slot, the
        role and the time it was found."""
        progress = deploy.status(self.cfg, self.ops).get("in_progress") or {}
        found = [i for i in progress.get("route_issues") or []
                 if i["slot"] == slot and i["role"] == role and i.get("found_at")]
        self.assertTrue(found, f"{label}: status does not name {slot} {role} with the time it "
                               f"was found: {progress}")
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        named = [a for a in alerts if a["alert"] == ["in_progress"]
                 and any(i["slot"] == slot and i["role"] == role and i.get("found_at")
                         for i in a.get("route_issues") or [])]
        self.assertTrue(named, f"{label}: the watch alert does not carry {slot} {role}: {alerts}")

    def assert_route_property(self, flow, slot, role, run):
        """The final loaded route names no role that stopped answering, unless
        nothing could have replaced it; then that role must be reported."""
        label = (f"{flow}: {slot} {role} stopped at probe {run['index']}"
                 f"{' (after)' if run['after'] else ''}")
        if not run["writes"]:
            return  # nothing was written in this flow; there is no route decision to judge
        views = self.property_views()
        loaded = self.ops.loaded_upstreams
        serving_ports, admin_port = deploy.loaded_upstream_ports(loaded or "")
        written = set()
        for text in run["writes"]:
            ports, admin = deploy.loaded_upstream_ports(text)
            written.update(ports)
            if admin is not None:
                written.add(admin)
        self.assertEqual(loaded, self.ops.files[self.cfg.upstream_file],
                         f"{label}: the file on disk is not what Nginx was told to load")
        deployer = deploy.Deployer(self.cfg, self.ops)
        dead = []
        for view in views.values():
            for r in deploy.SERVING_ROLES:
                if view["ports"][r] in serving_ports:
                    probe = deployer.probe_role(view, r)
                    if not probe.usable:
                        dead.append((view, r, probe))
            if admin_port is not None and view["ports"]["background"] == admin_port:
                probe = deployer.probe_role(view, "background")
                if not probe.usable:
                    dead.append((view, "background", probe))
        for view, r, probe in dead:
            if r == "standby":
                # A backup is always droppable: it never has to be replaced.
                self.fail(f"{label}: the loaded route still names slot {view['slot']} standby "
                          f"port {probe.port}, which does not answer")
            if r == "serving":
                replaceable = any(deployer.probe_role(other, "standby").usable
                                  for other in views.values() if other["slot"] == view["slot"])
            else:
                replaceable = any(deployer.probe_role(other, "background").usable
                                  for other in views.values() if other["slot"] != view["slot"])
            self.assertFalse(replaceable,
                             f"{label}: the loaded route names slot {view['slot']} {r} port "
                             f"{probe.port}, which does not answer, while another role could "
                             f"have replaced it")
            self.assert_reported(view["slot"], r, label)
        # The role we stopped: if it was written into the route, its slot is
        # still part of the final route and it is now gone, a person has to be
        # able to see that from status and from the watch - even if the role is
        # back before the next look. A slot the route moved away from entirely
        # (the rollback went to the other slot) is not a dropped role.
        view = views[slot]
        final_ports = set(serving_ports) | ({admin_port} if admin_port is not None else set())
        slot_ports = {view["ports"][r] for r in deploy.ROLES}
        if (run["stopped"] and view["ports"][role] in written
                and final_ports & slot_ports and view["ports"][role] not in final_ports
                and not deployer.probe_role(view, role).usable):
            self.assert_reported(slot, role, label)

    def property_fresh(self):
        case = DeployTest("test_first_deploy_from_legacy_moves_everything_to_slot_a")
        case.setUp()
        return case

    def property_scenarios(self, flow):
        case = self.property_fresh()
        try:
            counts = case.property_probe_counts(flow)
        finally:
            case.tearDown()
        scenarios = []
        for (slot, role), count in sorted(counts.items()):
            for index in range(count):
                scenarios.append((slot, role, index, False))
            # A probe with no later probe of the same role cannot be followed:
            # stopping after it is "after the last look", not a race.
            for index in range(count - 1):
                scenarios.append((slot, role, index, True))
        return scenarios

    def test_property_no_role_is_left_written_after_it_stops_answering(self):
        """The whole family, one sweep: for every flow, every role and every
        probe point, stop that role and require the final loaded route to name
        no role that stopped answering - or, when nothing could replace it, to
        report it with slot, role and time in status and in the watch alert."""
        failures = []
        checked = 0
        for flow in self.PROPERTY_FLOWS:
            for slot, role, index, after in self.property_scenarios(flow):
                case = self.property_fresh()
                try:
                    run = case.property_run(flow, slot, role, index, after)
                    run.update(index=index, after=after)
                    checked += 1
                    try:
                        case.assert_route_property(flow, slot, role, run)
                    except AssertionError as err:
                        failures.append(str(err))
                finally:
                    case.tearDown()
        self.assertGreater(checked, 20, "the sweep checked too few stop points to mean anything")
        if failures:
            self.fail(f"{len(failures)} of {checked} stop points left a route decision that "
                      "does not hold:\n- " + "\n- ".join(failures[:12]))

    # -- watch coverage (B segment: report, do not silently skip) -------------
    def test_watch_keeps_both_slots_after_two_releases(self):
        self.deploy()
        self.deploy(image="newer@sha256:3")
        self.assertEqual({"legacy", "a", "b"}, set(self.state()["slots"]))
        self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a.get("slot") == "legacy" and a.get("role") == "serving"
                            and "unhealthy" in a["alert"] for a in alerts), alerts)

    def test_watch_reports_a_slot_it_cannot_inspect_instead_of_skipping_it(self):
        self.deploy()
        inspect = self.ops.inspect

        def timeout(name, **kw):
            if name == "sources-main-a-standby":
                raise subprocess.TimeoutExpired(["docker", "inspect", name], 30)
            return inspect(name, **kw)

        with mock.patch.object(self.ops, "inspect", timeout):
            alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a.get("alert") == ["docker_inspect_failed"] and a.get("slot") == "a"
                            and a.get("role") == "standby" for a in alerts), alerts)

    # -- review round: probe ordering, retirement and admin evidence ----------
    BASE_SUCCESS_COMMANDS = """\
docker image inspect new@sha256:2
docker pull new@sha256:2
/usr/sbin/nginx -T
inspector sources-main-a-serving
inspector sources-main-a-standby
inspector sources-main-a-background
docker create --name sources-main-oneoff-<pid>-<t> --user 1001:1001 --entrypoint /app/sub2api --network app-data --label com.sources.deploy.role=oneoff --env-file /opt/x/background.env -e SOURCES_PROCESS_ROLE=background --mount type=bind,source=/state/main,target=/app/data new@sha256:2 -list-pending-migrations
docker network connect app-upstream sources-main-oneoff-<pid>-<t>
docker start --attach sources-main-oneoff-<pid>-<t>
docker rm -f sources-main-oneoff-<pid>-<t>
docker create --name sources-main-oneoff-<pid>-<t> --user 1001:1001 --entrypoint /app/sub2api --network app-data --label com.sources.deploy.role=oneoff --env-file /opt/x/background.env -e SOURCES_PROCESS_ROLE=background --mount type=bind,source=/state/main,target=/app/data new@sha256:2 -migrate-only 001_add.sql
docker network connect app-upstream sources-main-oneoff-<pid>-<t>
docker start --attach sources-main-oneoff-<pid>-<t>
docker rm -f sources-main-oneoff-<pid>-<t>
docker create --name sources-main-oneoff-<pid>-<t> --user 1001:1001 --entrypoint /app/sub2api --network app-data --label com.sources.deploy.role=oneoff --env-file /opt/x/serving.env -e SOURCES_PROCESS_ROLE=serving --mount type=bind,source=/state/main,target=/app/data new@sha256:2 -check-startup
docker network connect app-upstream sources-main-oneoff-<pid>-<t>
docker start --attach sources-main-oneoff-<pid>-<t>
docker rm -f sources-main-oneoff-<pid>-<t>
docker create --name sources-main-oneoff-<pid>-<t> --user 1001:1001 --entrypoint /app/sub2api --network app-data --label com.sources.deploy.role=oneoff --env-file /opt/x/background.env -e SOURCES_PROCESS_ROLE=background --mount type=bind,source=/state/main,target=/app/data new@sha256:2 -check-startup
docker network connect app-upstream sources-main-oneoff-<pid>-<t>
docker start --attach sources-main-oneoff-<pid>-<t>
docker rm -f sources-main-oneoff-<pid>-<t>
rm -rf <tmp>/work/assets-staging
install -d -o 1001 -m 0755 <tmp>/work/assets-staging
docker create --name sources-main-oneoff-<pid>-<t> --user 1001:1001 --entrypoint /app/sub2api --network app-data --label com.sources.deploy.role=oneoff --env-file /opt/x/serving.env -e SOURCES_PROCESS_ROLE=serving --mount type=bind,source=/state/main,target=/app/data --mount type=bind,source=<tmp>/work/assets-staging,target=/export new@sha256:2 -export-web-assets /export
docker network connect app-upstream sources-main-oneoff-<pid>-<t>
docker start --attach sources-main-oneoff-<pid>-<t>
docker rm -f sources-main-oneoff-<pid>-<t>
cp -Rn <tmp>/work/assets-staging/. /opt/sources-web/main/assets/
docker rm -f sources-main-a-serving
docker rm -f sources-main-a-standby
docker rm -f sources-main-a-background
rm -rf <tmp>/work/containers/sources-main-a-serving/pricing-cache
mkdir -p <tmp>/work/containers/sources-main-a-serving
cp -a /opt/x/inputs/serving/pricing-cache <tmp>/work/containers/sources-main-a-serving/pricing-cache
docker create --name sources-main-a-serving --hostname sources-main-a-serving --user 1001:1001 --entrypoint /app/sub2api --restart unless-stopped --memory 24g --cpus 12 --pids-limit 4096 --log-opt max-size=10m --log-opt max-file=3 --publish 127.0.0.1:9401:8080 --network app-data --network-alias sources-main-a-serving --network-alias sources-main-serving --label com.sources.deploy.slot=a --label com.sources.deploy.role=serving --label com.sources.deploy.created=<ts> --env-file /opt/x/serving.env -e SOURCES_PROCESS_ROLE=serving --mount type=bind,source=/state/main,target=/app/data --mount type=bind,source=<tmp>/work/containers/sources-main-a-serving/pricing-cache,target=/runtime/pricing-cache new@sha256:2
docker network connect --alias sources-main-a-serving --alias sources-main-serving app-upstream sources-main-a-serving
docker start sources-main-a-serving
rm -rf <tmp>/work/containers/sources-main-a-standby/pricing-cache
mkdir -p <tmp>/work/containers/sources-main-a-standby
cp -a /opt/x/inputs/serving/pricing-cache <tmp>/work/containers/sources-main-a-standby/pricing-cache
docker create --name sources-main-a-standby --hostname sources-main-a-standby --user 1001:1001 --entrypoint /app/sub2api --restart unless-stopped --memory 24g --cpus 12 --pids-limit 4096 --log-opt max-size=10m --log-opt max-file=3 --publish 127.0.0.1:9402:8080 --network app-data --network-alias sources-main-a-standby --network-alias sources-main-serving --label com.sources.deploy.slot=a --label com.sources.deploy.role=standby --label com.sources.deploy.created=<ts> --env-file /opt/x/serving.env -e SOURCES_PROCESS_ROLE=serving --mount type=bind,source=/state/main,target=/app/data --mount type=bind,source=<tmp>/work/containers/sources-main-a-standby/pricing-cache,target=/runtime/pricing-cache new@sha256:2
docker network connect --alias sources-main-a-standby --alias sources-main-serving app-upstream sources-main-a-standby
docker start sources-main-a-standby
inspector old-bg
docker create --name sources-main-a-background --hostname sources-main-a-background --user 1001:1001 --entrypoint /app/sub2api --restart unless-stopped --memory 24g --cpus 12 --pids-limit 4096 --log-opt max-size=10m --log-opt max-file=3 --publish 127.0.0.1:9403:8080 --network app-data --network-alias sources-main-a-background --label com.sources.deploy.slot=a --label com.sources.deploy.role=background --label com.sources.deploy.created=<ts> --env-file /opt/x/background.env -e SOURCES_PROCESS_ROLE=background --mount type=bind,source=/state/main,target=/app/data new@sha256:2
docker network connect --alias sources-main-a-background app-upstream sources-main-a-background
docker update --restart=no old-bg
docker kill --signal=TERM old-bg
inspector old-bg
docker update --restart=unless-stopped sources-main-a-background
docker start sources-main-a-background
inspector sources-main-a-background
/usr/sbin/nginx -t
/usr/sbin/nginx -s reload
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector sources-main-a-serving
inspector old-serving
docker update --restart=no old-serving
docker kill --signal=TERM old-serving
inspector old-spare
docker update --restart=no old-spare
docker kill --signal=TERM old-spare
"""

    def normalised_commands(self):
        """Every Docker, Nginx and inspect call, in order, with volatile parts
        replaced. `inspect` is recorded because the real tool runs it as a
        Docker command; the stock fake answers it without going through run()."""
        original = self.ops.inspect

        def recording_inspect(name, **kw):
            self.ops.commands.append(["inspector", name])
            return original(name, **kw)

        with mock.patch.object(self.ops, "inspect", recording_inspect):
            self.deploy()
        lines = []
        for cmd in self.ops.commands:
            text = " ".join(cmd)
            text = __import__("re").sub(r"sources-main-oneoff-\d+-\d+", "sources-main-oneoff-<pid>-<t>", text)
            text = text.replace(str(self.tmp), "<tmp>")
            text = __import__("re").sub(r"created=[0-9T:+.\-]+", "created=<ts>", text)
            lines.append(text)
        return lines

    def test_success_command_sequence_matches_base(self):
        """A successful release runs exactly the commands it ran before the
        guard work: same calls, same order, no probe-specific `docker inspect`."""
        expected = self.BASE_SUCCESS_COMMANDS.splitlines()
        actual = self.normalised_commands()
        self.assertEqual(expected, actual, "the successful release command sequence changed")

    def stop_during_probe(self, stop, *, port, path="/internal/sources/runtime", step=None):
        """Stop containers while a chosen probe is in flight, then answer it.
        `step` limits the hook to the rollback (the guard step), so the stop
        happens during the rollback's own probes, not earlier in the release."""
        real = self.ops.http_json
        fired = []

        def probe(url, **kw):
            if not fired and url.startswith(f"http://127.0.0.1:{port}{path}"):
                current = (self.state().get("in_progress") or {}).get("step")
                if step is None or current == step:
                    fired.append(1)
                    stop()
            return real(url, **kw)

        return mock.patch.object(self.ops, "http_json", probe)

    def test_rollback_prewrite_serving_exits_during_admin_probe(self):
        """Invariant 1: the serving primary is probed after the background, so a
        primary that exits during the background probe is not written."""
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")

        def stop_primary():
            self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")

        with self.stop_during_probe(stop_primary, port=target["ports"]["background"], step="guard"):
            with self.assertRaisesRegex(deploy.FallbackFailed, "rollback service route was not written"):
                self.deploy()
        self.assertEqual(self.upstreams_for(target), self.ops.loaded_upstreams,
                         "a dead primary was not written into the service route")
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['serving']}",
                         self.ops.files[self.cfg.upstream_file])
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertIn("slot legacy's serving containers", progress["failed"])

    def test_rollback_prewrite_standby_exits_during_admin_probe(self):
        """Invariant 3: a standby that exits during the background probe is
        simply not written; the primary still takes traffic."""
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")

        def stop_standby():
            self.ops.containers["old-spare"]["State"].update(Running=False, Status="exited")

        with self.stop_during_probe(stop_standby, port=target["ports"]["background"], step="guard"):
            with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
                self.deploy()
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['standby']}", upstream)
        self.assertIsNone(self.state()["in_progress"])

    def test_manual_rollback_accepts_healthy_primary_without_standby(self):
        """Invariant 3: a missing standby does not block returning to a healthy
        previous primary."""
        self.deploy()
        self.ops.containers.pop("old-spare")
        deploy.Deployer(self.cfg, self.ops).rollback()
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['standby']}", upstream)
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])

    def test_manual_rollback_serves_from_the_primary_when_its_standby_never_becomes_ready(self):
        """Invariant 3: the standby only decides whether it is written. A
        container that exists but never becomes ready must not keep a healthy
        previous primary from serving."""
        self.deploy()
        self.ops.unready_ports.add(LEGACY["ports"]["standby"])
        deploy.Deployer(self.cfg, self.ops).rollback()
        upstream = self.ops.files[self.cfg.upstream_file]
        self.assertIn(f"127.0.0.1:{LEGACY['ports']['serving']} max_fails", upstream)
        self.assertNotIn(f"127.0.0.1:{LEGACY['ports']['standby']}", upstream,
                         "a standby that never became ready is not written as a backup")
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertIn("did not become ready", "\n".join(self.ops.logs))

    def test_normal_release_does_not_write_a_background_that_stopped_before_the_write(self):
        """Invariant 1: the admin target has no fallback inside the file, so it
        is the last probe before the write. A background that exits while the
        serving roles are probed is not written."""
        target = self.cfg.slot_view("a")
        writes: list[str] = []
        write, http_json = self.ops.write_file_atomic, self.ops.http_json
        fired = []

        def record(path, text, **kw):
            if path == self.cfg.upstream_file:
                writes.append(text)
            return write(path, text, **kw)

        def stop_during_serving_probe(url, **kw):
            if (not fired
                    and url.startswith(f"http://127.0.0.1:{target['ports']['serving']}/internal/sources/runtime")
                    and (self.state().get("in_progress") or {}).get("step") == "switch"):
                fired.append(1)
                self.ops.containers[target["containers"]["background"]]["State"].update(
                    Running=False, Status="exited")
            return http_json(url, **kw)

        with mock.patch.object(self.ops, "http_json", stop_during_serving_probe), \
                mock.patch.object(self.ops, "write_file_atomic", record):
            with self.assertRaisesRegex(deploy.DeployError, "not writing the admin route"):
                self.deploy()
        self.assertTrue(fired)
        self.assertEqual([], [text for text in writes
                              if f"127.0.0.1:{target['ports']['background']};" in text],
                         "a background that stopped before the write was not written")

    def test_rollback_checks_written_roles_when_retirement_fails(self):
        """Invariant 2: a retirement error must not skip the check of the roles
        already written; a primary that exited is named."""
        self.ops.memory_after_switch = 20 * GIB
        candidate_serving = self.cfg.container("a", "serving")
        run = self.ops.run

        def fail_retirement(args, **kw):
            if args == ["docker", "update", "--restart=no", candidate_serving]:
                self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")
                raise deploy.DeployError("fixture: cannot retire the candidate")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_retirement):
            with self.assertRaisesRegex(deploy.ServingCleanupFailed, "slot legacy serving"):
                self.deploy()
        progress = self.state()["in_progress"]
        self.assertEqual("failed", progress["phase"])
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "serving"
                            for i in progress["route_issues"]), progress["route_issues"])

    def test_transient_exit_during_failed_retire_surfaces_in_status_and_watch(self):
        """B segment 8: the exit is recorded even when the role is back before
        the next watch sample."""
        self.ops.memory_after_switch = 20 * GIB
        candidate_serving = self.cfg.container("a", "serving")
        run = self.ops.run
        retired_failed = False

        def fail_retirement(args, **kw):
            nonlocal retired_failed
            if args == ["docker", "update", "--restart=no", candidate_serving] and not retired_failed:
                retired_failed = True
                self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")
                raise deploy.DeployError("fixture: cannot retire the candidate")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail_retirement):
            with self.assertRaises(deploy.ServingCleanupFailed):
                self.deploy()
        # The role is back by the time anyone looks again.
        self.ops.containers["old-serving"]["State"].update(Running=True, Status="running")

        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "serving" and i.get("found_at")
                            for i in progress["route_issues"]), progress["route_issues"])
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a["alert"] == ["in_progress"] and a.get("route_issues") for a in alerts), alerts)
        self.assertIn("route issue", "\n".join(self.ops.logs))

    def test_successful_publish_preserves_prewrite_admin_probe(self):
        """Invariant 4: a successful release keeps the probe that justified the
        admin move, after in_progress is cleared."""
        self.deploy()
        self.assertIsNone(self.state()["in_progress"])
        route = deploy.status(self.cfg, self.ops)["last_route"]
        admin = route["admin"]
        self.assertEqual("probed", admin["source"])
        self.assertEqual("a", admin["slot"])
        self.assertEqual(9403, admin["port"])
        self.assertEqual("answers", admin["probe"]["outcome"])
        self.assertTrue(admin["at"])
        self.assertEqual([9401, 9402], route["serving"]["written"])
        log = "\n".join(self.ops.logs)
        self.assertIn("admin slot a port 9403 (probed; slot a background", log)

    def test_successful_rollback_preserves_prewrite_admin_probe(self):
        self.deploy()
        deploy.Deployer(self.cfg, self.ops).rollback()
        self.assertIsNone(self.state()["in_progress"])
        admin = deploy.status(self.cfg, self.ops)["last_route"]["admin"]
        self.assertEqual("probed", admin["source"])
        self.assertEqual("legacy", admin["slot"])
        self.assertEqual(LEGACY["ports"]["background"], admin["port"])
        self.assertEqual("answers", admin["probe"]["outcome"])

    def test_guard_reports_no_background_when_service_route_is_refused(self):
        """Invariant 5: a refused serving write still says that no background
        answers admin and names the one to restore."""
        self.ops.memory_after_switch = 20 * GIB
        target = self.cfg.slot_view("a")
        candidate_background = target["containers"]["background"]

        def stop_everything():
            self.ops.containers["old-serving"]["State"].update(Running=False, Status="exited")
            self.ops.containers["old-bg"]["State"].update(Running=False, Status="exited")
            self.ops.containers[candidate_background]["State"].update(Running=False, Status="exited")

        # The previous primary is the last probe before the rollback write.
        with self.stop_during_probe(stop_everything, port=LEGACY["ports"]["serving"], step="guard"):
            with self.assertRaisesRegex(deploy.FallbackFailed, "No background answers admin now") as ctx:
                self.deploy()
        self.assertIn("restore one background (slot legacy", str(ctx.exception))
        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertEqual("no background answers", progress["admin_route"]["source"])
        self.assertEqual("legacy", progress["admin_route"]["restore"])

    def test_guard_keeps_loaded_admin_after_failed_postwrite_probe(self):
        """Invariant 5: after the admin write, a failed probe stops on the
        loaded route. No second reload, no trying the other slot."""
        self.ops.memory_after_switch = 20 * GIB
        old_admin = LEGACY["ports"]["background"]
        candidate_background = self.cfg.container("a", "background")
        write = self.ops.write_file_atomic
        admin_writes = []

        def stop_after_admin_write(path, text, **kw):
            result = write(path, text, **kw)
            if path == self.cfg.upstream_file and f"127.0.0.1:{old_admin};" in text:
                admin_writes.append(len(self.ops.commands))
                self.ops.containers["old-bg"]["State"].update(Running=False, Status="exited")
            return result

        with mock.patch.object(self.ops, "write_file_atomic", stop_after_admin_write):
            with self.assertRaisesRegex(deploy.FallbackFailed, "no further reload"):
                self.deploy()
        self.assertEqual(1, len(admin_writes), "admin moved exactly once")
        after = self.ops.commands[admin_writes[0]:]
        self.assertEqual(1, sum(1 for c in after if c[1:] == ["-s", "reload"]),
                         "only the admin move's own reload happens after the write")
        self.assertNotIn(["docker", "start", candidate_background], after,
                         "the candidate background is not restarted after the failed probe")
        self.assertIn(f"127.0.0.1:{old_admin};", self.ops.loaded_upstreams)
        progress = self.state()["in_progress"]
        self.assertTrue(any(i["slot"] == "legacy" and i["role"] == "background"
                            for i in progress["route_issues"]), progress["route_issues"])

    def test_watch_alerts_on_runtime_timeout_with_healthy_port(self):
        """B segment 10: a running container with /health 200 but no runtime
        answer is reported, not skipped."""
        self.deploy()
        real = self.ops.http_json

        def slow_runtime(url, **kw):
            if url.startswith("http://127.0.0.1:9401/internal/sources/runtime"):
                return 0, None
            return real(url, **kw)

        with mock.patch.object(self.ops, "http_json", slow_runtime):
            alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a.get("alert") == ["runtime_unavailable"] and a.get("slot") == "a"
                            and a.get("role") == "serving" for a in alerts), alerts)

    def test_watch_alerts_when_the_runtime_token_cannot_be_read(self):
        """B segment 10: the container is up and /health is 200, but the token
        that would let the tool read its runtime state cannot be read. That is
        an alert, not a silent pass."""
        self.deploy()
        # The token files still exist for the other roles; only these cannot be read.
        readable = self.ops.read_text

        def unreadable(path):
            if path == "/opt/x/runtime.token" or path.startswith("/legacy/"):
                raise PermissionError(f"fixture: cannot read {path}")
            return readable(path)

        with mock.patch.object(self.ops, "read_text", unreadable):
            alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertTrue(any(a.get("alert") == ["runtime_unavailable"] and a.get("slot") == "a"
                            and a.get("role") == "serving" for a in alerts), alerts)

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


class HoldOps(FakeOps):
    """Model the image and network identities used by pause and resume."""

    image = "candidate:tag"
    image_id = "sha256:" + "a" * 64

    def __init__(self, tmp):
        super().__init__(tmp)
        self.local_images.add(self.image)
        self.tag_image_id = self.image_id
        self.mutations = []
        self.next_id = 0

    def add(self, name, port, **kwargs):
        super().add(name, port, **kwargs)
        self.next_id += 1
        self.containers[name]["Id"] = f"fixture-{self.next_id}-id"
        self.containers[name]["Image"] = self.containers[name]["Config"]["Image"]

    def run(self, args, *, mutate, timeout=600, check=True):
        if args[:4] == ["docker", "image", "inspect", "--format"]:
            self.commands.append(list(args))
            return subprocess.CompletedProcess(args, 0, self.tag_image_id + "\n", "")
        if mutate and not self.dry_run:
            self.mutations.append(list(args))
        name = next((name for name, c in self.containers.items() if c["Id"] == args[-1]), args[-1])
        result = super().run([*args[:-1], name], mutate=mutate, timeout=timeout, check=check)
        self.commands[-1] = (["DRY"] if self.dry_run and mutate else []) + list(args)
        if self.dry_run and mutate:
            return result
        if args[:2] == ["docker", "create"]:
            name = args[args.index("--name") + 1]
            network = args[args.index("--network") + 1]
            aliases = [arg for i, arg in enumerate(args) if i and args[i - 1] == "--network-alias"]
            self.containers[name]["NetworkSettings"]["Networks"] = {network: {"Aliases": aliases}}
        if args[:2] == ["docker", "network"]:
            networks = self.containers[name]["NetworkSettings"]["Networks"]
            if args[2] == "disconnect":
                del networks[args[-2]]
            if args[2] == "connect":
                aliases = [arg for i, arg in enumerate(args) if i and args[i - 1] == "--alias"]
                networks[args[-2]] = {"Aliases": aliases}
        return result

    def read_text(self, path):
        if path.endswith("memory.current"):
            return str(GIB)
        return super().read_text(path)


class HoldTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.tmp = Path(temporary.name)
        self.cfg = make_config(self.tmp)
        self.ops = HoldOps(self.tmp)
        for role, name in LEGACY["containers"].items():
            self.ops.add(name, LEGACY["ports"][role])
            self.ops.containers[name]["role"] = "background" if role == "background" else "serving"
        self.upstream = deploy.render_upstreams(*(LEGACY["ports"][role] for role in deploy.ROLES))
        self.ops.files[self.cfg.upstream_file] = self.upstream
        for path in self.cfg.env_files.values():
            self.ops.files[path] = "SETTING=placeholder\n"
        deploy.save_state(self.cfg, {"current": LEGACY, "previous": None, "in_progress": None})

    def cli(self, *args):
        def make_ops(*, dry_run):
            self.ops.dry_run = dry_run
            return self.ops

        with (mock.patch.object(deploy.Config, "load", return_value=self.cfg),
              mock.patch.object(deploy, "Ops", side_effect=make_ops),
              mock.patch("sys.stderr", new_callable=io.StringIO) as stderr):
            result = deploy.main(list(args))
        return result, stderr.getvalue()

    def state(self):
        return deploy.load_state(self.cfg)

    def hold(self):
        self.assertEqual((0, ""), self.cli("main", self.ops.image, "--hold-before-switch"))
        held = self.state()["in_progress"]
        self.assertEqual("held", held["phase"])
        return held

    def test_hold_leaves_the_old_release_serving_and_reports_the_preview(self):
        held = self.hold()
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual(self.upstream, self.ops.files[self.cfg.upstream_file])
        self.assertFalse(self.ops.switched)
        self.assertEqual(self.ops.image, held["target"]["image"])
        self.assertEqual("a", held["target"]["slot"])
        self.assertEqual(self.ops.image_id, held["image_id"])
        self.assertLessEqual(deploy.dt.datetime.fromisoformat(held["held_at"]),
                             deploy.dt.datetime.now(deploy.dt.timezone.utc))
        self.assertTrue(self.ops.migrated, "additive migrations have already run")
        for name in LEGACY["containers"].values():
            self.assertTrue(self.ops.containers[name]["State"]["Running"])
            self.assertFalse(any(cmd[-1] in (name, self.ops.containers[name]["Id"]) for cmd in self.ops.mutations))
        target = held["target"]
        self.assertNotIn(target["containers"]["background"], self.ops.containers)
        self.assertEqual(set(deploy.SERVING_ROLES), set(held["containers"]))
        for role in deploy.SERVING_ROLES:
            candidate = self.ops.containers[target["containers"][role]]
            self.assertEqual(candidate["Id"], held["containers"][role])
            self.assertTrue(candidate["State"]["Running"])
            self.assertEqual("serving", candidate["role"])
            for network in self.cfg.networks:
                self.assertNotIn(self.cfg.serving_alias, candidate["NetworkSettings"]["Networks"][network]["Aliases"])
            self.assertIn(f"{role}: http://127.0.0.1:{target['ports'][role]}/", "\n".join(self.ops.logs))

    def test_resume_uses_tested_image_and_finishes_the_normal_release(self):
        held = self.hold()
        self.ops.tag_image_id = "sha256:" + "b" * 64
        self.ops.mutations.clear()
        oneoffs = list(self.ops.oneoffs)
        self.assertEqual((0, ""), self.cli("resume"))
        current = self.state()["current"]
        self.assertEqual(held["target"]["slot"], current["slot"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(LEGACY, self.state()["previous"])
        self.assertEqual(oneoffs, self.ops.oneoffs, "resume must not rerun migrations or recreate serving")
        for role in deploy.SERVING_ROLES:
            candidate = self.ops.containers[current["containers"][role]]
            self.assertEqual(held["containers"][role], candidate["Id"])
            for network in self.cfg.networks:
                self.assertIn(self.cfg.serving_alias, candidate["NetworkSettings"]["Networks"][network]["Aliases"])
        background = self.ops.containers[current["containers"]["background"]]
        self.assertEqual(held["image_id"], background["Image"])
        self.assertTrue(background["State"]["Running"])
        old = LEGACY["containers"]["background"]
        self.assertFalse(self.ops.containers[old]["State"]["Running"])
        term = self.ops.mutations.index(["docker", "kill", "--signal=TERM", old])
        start = self.ops.mutations.index(["docker", "start", current["containers"]["background"]])
        reload = self.ops.mutations.index([self.cfg.nginx_bin, "-s", "reload"])
        self.assertLess(term, start)
        self.assertLess(start, reload)
        self.assertEqual(deploy.render_upstreams(*(current["ports"][role] for role in deploy.ROLES)),
                         self.ops.files[self.cfg.upstream_file])
        history = [json.loads(line) for line in Path(self.cfg.history_file).read_text().splitlines()]
        self.assertEqual(["held", "ok"], [line["result"] for line in history])
        self.assertEqual(["background handoff", "switch", "guard", "retire previous serving"],
                         [entry["step"] for entry in history[-1]["steps"][-4:]])

    def test_resume_rejects_unhealthy_standby_before_changing_anything(self):
        held = self.hold()
        port = held["target"]["ports"]["standby"]
        state_file = Path(self.cfg.state_file)
        saved = state_file.read_bytes()
        http = self.ops.http_json

        def unhealthy(url, **kwargs):
            if url.endswith(f":{port}/health"):
                return 503, None
            return http(url, **kwargs)

        self.ops.mutations.clear()
        with mock.patch.object(self.ops, "http_json", side_effect=unhealthy):
            result, error = self.cli("resume")
        self.assertEqual(1, result)
        self.assertIn("Pause retained", error)
        self.assertIn("held standby is unhealthy", error)
        self.assertEqual([], self.ops.mutations)
        self.assertEqual(saved, state_file.read_bytes())
        self.assertEqual(self.upstream, self.ops.files[self.cfg.upstream_file])

    def test_resume_rejects_a_replacement_a_different_image_or_changed_environment(self):
        held = self.hold()
        name = held["target"]["containers"]["serving"]
        environment = self.cfg.env_files["background"]
        for fixture, change, reason in (
            (self.ops.containers[name], {"Id": "replacement-id"}, "container identity or image changed"),
            (self.ops.containers[name], {"Image": "another-image"}, "container identity or image changed"),
            (self.ops.files, {environment: "SETTING=changed\n"}, "input files changed"),
            (self.ops.files, {self.cfg.upstream_file: "updated-upstream\n"}, "upstream file changed"),
        ):
            with self.subTest(reason=reason), mock.patch.dict(fixture, change):
                self.ops.mutations.clear()
                result, error = self.cli("resume")
                self.assertEqual(1, result)
                self.assertIn(reason, error)
                self.assertEqual([], self.ops.mutations)
                self.assertEqual(held, self.state()["in_progress"])

    def test_hold_requires_both_candidates_to_answer_health_before_exiting_successfully(self):
        standby_port = self.cfg.slots["a"]["standby"]
        http = self.ops.http_json

        def unhealthy(url, **kwargs):
            if url.endswith(f":{standby_port}/health"):
                return 503, None
            return http(url, **kwargs)

        with mock.patch.object(self.ops, "http_json", side_effect=unhealthy):
            result, error = self.cli("main", self.ops.image, "--hold-before-switch")
        self.assertEqual(1, result)
        self.assertIn("not ready as serving", error)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertFalse(self.ops.switched)
        self.assertTrue(self.ops.containers[LEGACY["containers"]["background"]]["State"]["Running"])

    def test_abort_reports_and_retries_partial_cleanup_by_container_id(self):
        held = self.hold()
        standby = held["target"]["containers"]["standby"]
        run = self.ops.run

        def fail_removal(args, **kwargs):
            if args == ["docker", "rm", held["containers"]["standby"]]:
                raise deploy.DeployError("fixture removal failed")
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "run", side_effect=fail_removal):
            result, error = self.cli("abort")
        self.assertEqual(1, result)
        self.assertIn("phase=aborting, serving=missing, standby=exited", error)
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertIn("fixture removal failed", self.state()["in_progress"]["reason"])
        self.assertEqual("exited", deploy.status(self.cfg, self.ops)["in_progress"]["live_containers"]["standby"]["status"])
        self.assertIn(["abort_incomplete"], [a["alert"] for a in deploy.watch_once(self.cfg, self.ops, {})])
        self.assertIn("Retry 'abort'", self.cli("resume")[1])
        self.assertEqual((0, ""), self.cli("abort"))
        self.assertFalse(any(name in self.ops.containers for name in held["target"]["containers"].values()))
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual(self.upstream, self.ops.files[self.cfg.upstream_file])
        self.assertTrue(self.ops.migrated)
        self.assertIn(["docker", "rm", held["containers"]["standby"]], self.ops.mutations)
        self.assertNotIn(standby, self.ops.containers)

    def test_abort_leaves_a_replacement_container_alone(self):
        held = self.hold()
        name = held["target"]["containers"]["serving"]
        self.ops.add(name, held["target"]["ports"]["serving"])
        self.ops.mutations.clear()
        result, error = self.cli("abort")
        self.assertEqual(1, result)
        self.assertIn("identity of held serving", error)
        self.assertEqual([], self.ops.mutations)
        self.assertTrue(self.ops.containers[name]["State"]["Running"])
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])

    def test_status_and_watch_distinguish_a_pause_after_restart_from_a_failed_release(self):
        held = self.hold()
        self.cfg.hold_warning_seconds = 1
        held["held_at"] = "2020-01-01T00:00:00+00:00"
        deploy.save_state(self.cfg, {**self.state(), "in_progress": held})
        # Read the same persisted state using a fresh process/ops fixture.
        restarted = HoldOps(self.tmp)
        restarted.containers = copy.deepcopy(self.ops.containers)
        paused = deploy.status(self.cfg, restarted)["in_progress"]
        self.assertEqual("held", paused["phase"])
        self.assertGreaterEqual(paused["age_seconds"], self.cfg.hold_warning_seconds)
        self.assertIn("resume or abort", paused["warning"])
        self.assertEqual([], deploy.watch_once(self.cfg, restarted, {}))
        self.assertEqual([], restarted.mutations)
        failed = {"step": "switch", "failed": "fixture failure", "target": held["target"]}
        deploy.save_state(self.cfg, {**self.state(), "in_progress": failed})
        self.assertEqual(failed, deploy.status(self.cfg, restarted)["in_progress"])
        self.assertIn(["release_incomplete"], [a["alert"] for a in deploy.watch_once(self.cfg, restarted, {})])
        for command in ("resume", "abort"):
            result, error = self.cli(command)
            self.assertEqual(1, result)
            self.assertIn("unfinished release requires manual recovery", error)

    def test_status_distinguishes_healthy_and_unhealthy_held_candidates(self):
        held = self.hold()
        healthy = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertIsNone(healthy["warning"])
        for role in deploy.SERVING_ROLES:
            candidate = healthy["live_containers"][role]
            self.assertTrue(candidate["ready"])
            self.assertEqual(200, candidate["health"])

        port = held["target"]["ports"]["standby"]
        http = self.ops.http_json

        def unhealthy(url, **kwargs):
            if url.endswith(f":{port}/health"):
                return 503, None
            return http(url, **kwargs)

        with mock.patch.object(self.ops, "http_json", side_effect=unhealthy):
            paused = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertEqual("held", paused["phase"])
        self.assertTrue(paused["live_containers"]["serving"]["ready"])
        standby = paused["live_containers"]["standby"]
        self.assertEqual("running", standby["status"])
        self.assertFalse(standby["ready"])
        self.assertEqual(503, standby["health"])
        self.assertIn("standby", paused["warning"])
        self.assertIn("'abort'", paused["warning"])
        self.assertEqual(held, self.state()["in_progress"])

        # A successful /health alone does not prove the expected serving role.
        candidate = self.ops.by_port(port)
        with mock.patch.dict(candidate, {"role": "background"}):
            standby = deploy.status(self.cfg, self.ops)["in_progress"]["live_containers"]["standby"]
        self.assertEqual(200, standby["health"])
        self.assertFalse(standby["ready"])

    def test_reports_release_interrupted_after_resume_releases_its_lock(self):
        held = self.hold()
        serving_id = held["containers"]["serving"]
        self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
        run = self.ops.run

        def exit_during_activation(args, **kwargs):
            if args == ["docker", "update", "--restart=no", serving_id]:
                progress = deploy.status(self.cfg, self.ops)["in_progress"]
                self.assertEqual("activate held serving", progress["step"])
                self.assertNotEqual("interrupted", progress.get("phase"))
                self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
                # Exit before the deployment can write a failure record.
                raise SystemExit(97)
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "run", side_effect=exit_during_activation):
            with self.assertRaisesRegex(SystemExit, "97"):
                self.cli("resume")

        with deploy.release_lock(self.cfg.lock_file):
            self.assertNotIn("failed", self.state()["in_progress"])
        state_file = Path(self.cfg.state_file)
        saved = state_file.read_bytes()
        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertEqual("interrupted", progress.get("phase"))
        self.assertEqual("activate held serving", progress["step"])
        self.assertIn("release lock is free", progress["warning"])
        alerts = deploy.watch_once(self.cfg, self.ops, {})
        self.assertEqual(["release_incomplete"], alerts[0]["alert"])
        self.assertEqual("activate held serving", alerts[0]["step"])
        self.assertIn("release lock is free", alerts[0]["reason"])
        self.assertEqual(saved, state_file.read_bytes())

    def test_reports_interrupted_abort_and_allows_retry(self):
        held = self.hold()
        serving_id = held["containers"]["serving"]
        run = self.ops.run

        def exit_during_cleanup(args, **kwargs):
            if args == ["docker", "update", "--restart=no", serving_id]:
                self.assertEqual([], deploy.watch_once(self.cfg, self.ops, {}))
                raise SystemExit(97)
            return run(args, **kwargs)

        with mock.patch.object(self.ops, "run", side_effect=exit_during_cleanup):
            with self.assertRaisesRegex(SystemExit, "97"):
                self.cli("abort")

        self.assertNotIn("reason", self.state()["in_progress"])
        progress = deploy.status(self.cfg, self.ops)["in_progress"]
        self.assertEqual("aborting", progress["phase"])
        self.assertIn("release lock is free", progress["warning"])
        self.assertEqual(["abort_incomplete"], deploy.watch_once(self.cfg, self.ops, {})[0]["alert"])
        self.assertEqual((0, ""), self.cli("abort"))
        self.assertIsNone(self.state()["in_progress"])

    def test_main_and_rollback_cannot_replace_a_held_release(self):
        self.hold()
        self.ops.commands.clear()
        for args in (("main", self.ops.image), ("rollback",)):
            with self.subTest(args=args):
                result, error = self.cli(*args)
                self.assertEqual(1, result)
                self.assertIn("Run 'resume' or 'abort' first", error)
        self.assertEqual([], self.ops.commands)
        self.assertEqual("held", self.state()["in_progress"]["phase"])

    def test_dry_runs_leave_the_release_containers_and_state_intact(self):
        history = Path(self.cfg.history_file)
        before = self.state()
        self.assertEqual((0, ""), self.cli("--dry-run", "main", self.ops.image, "--hold-before-switch"))
        self.assertEqual(before, self.state())
        self.assertEqual([], self.ops.mutations)
        self.assertFalse(history.exists())
        self.assertIn("DRY-RUN would pause", "\n".join(self.ops.logs))
        self.hold()
        snapshot = self.state(), history.read_bytes(), copy.deepcopy(self.ops.containers)
        self.ops.mutations.clear()
        for command in ("resume", "abort"):
            with self.subTest(command=command):
                self.ops.commands.clear()
                self.assertEqual((0, ""), self.cli("--dry-run", command))
                self.assertTrue(any(cmd[0] == "DRY" for cmd in self.ops.commands))
                self.assertEqual(snapshot, (self.state(), history.read_bytes(), self.ops.containers))
                self.assertEqual([], self.ops.mutations)
                self.assertEqual(self.upstream, self.ops.files[self.cfg.upstream_file])

    def test_hold_rejects_contract_migrations_even_with_the_override(self):
        self.ops.pending = "fixture_contract.sql"
        result, error = self.cli("main", self.ops.image, "--hold-before-switch", "--allow-contract")
        self.assertEqual(1, result)
        self.assertIn("contract migrations are not allowed", error)
        self.assertEqual([], self.ops.mutations)
        self.assertIsNone(self.state()["in_progress"])


if __name__ == "__main__":
    unittest.main()
