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
        self.loaded_upstreams = None
        self.upstream_file = None
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
            network = args[args.index("--network") + 1]
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
                c["State"].update(Running=True, Status="running", StartedAt=f"t{self.clock}")
        elif args[:2] == ["docker", "kill"]:
            c = self.containers.get(name)
            if c and name in self.drain_seconds and c["State"]["Running"]:
                c["draining_until"] = self.clock + self.drain_seconds[name]
            elif c:
                c["State"].update(Running=False, Status="exited", ExitCode=0)
        elif args[:2] == ["docker", "rm"]:
            self.containers.pop(name, None)
        elif args[:2] == ["docker", "update"]:
            c = self.containers.get(name)
            if c:
                c["HostConfig"]["RestartPolicy"]["Name"] = args[2].split("=", 1)[1]
        elif args[:3] == ["docker", "network", "disconnect"]:
            self.containers[name]["NetworkSettings"]["Networks"].pop(args[-2], None)
        elif args[:3] == ["docker", "network", "connect"]:
            if name in self.containers:
                aliases = [args[i + 1] for i, arg in enumerate(args) if arg == "--alias"]
                self.containers[name]["NetworkSettings"]["Networks"][args[-2]] = {"Aliases": aliases}
        elif args[:3] == ["docker", "image", "inspect"]:
            rc = 0 if args[3] in self.local_images else 1
        elif args[1:] == ["-T"]:
            out = self.nginx_dump
        elif args[1:] == ["-t"]:
            rc = self.nginx_test_rc
        elif args[1:] == ["-s", "reload"]:
            self.switched = True
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
        self.ops.files[self.cfg.upstream_file] = deploy.render_upstreams(**{
            role + "_port": port for role, port in LEGACY["ports"].items()})
        self.ops.loaded_upstreams = self.ops.files[self.cfg.upstream_file]
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

    def assert_live_release(self, view):
        upstreams = self.upstreams_for(view)
        self.assertEqual(upstreams, self.ops.loaded_upstreams)
        self.assertEqual(upstreams, self.ops.files[self.cfg.upstream_file])
        runtime = deploy.Deployer(self.cfg, self.ops).runtime
        token = self.ops.read_text(self.cfg.runtime_token_file).strip()
        roles = [(name, runtime(container['port'], token) or {})
                 for name, container in self.ops.containers.items() if container["role"] == "background"]
        self.assertEqual([view["containers"]["background"]], [n for n, r in roles if r.get("owns_background")])
        self.assertTrue(dict(roles)[view["containers"]["background"]]["ready"])

    def test_main_guard_docker_query_failure_restores_live_release(self):
        target = self.cfg.slot_view("a")
        inspect = self.ops.inspect
        query_failed = False

        def fail_after_switch(name):
            nonlocal query_failed
            if name == target["containers"]["serving"] and self.ops.switched and not query_failed:
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
        target = self.cfg.slot_view("a")
        inspect, run = self.ops.inspect, self.ops.run

        def fail_guard(name):
            if name == target["containers"]["serving"] and self.ops.switched:
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
        self.assertEqual(LEGACY, self.state()["current"])
        progress = self.state()["in_progress"]
        self.assertEqual("guard", progress["failed_step"])
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
        self.assertEqual(self.upstreams_for(LEGACY), self.ops.loaded_upstreams)
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
                self.assertIn(type(failure).__name__, self.state()["in_progress"]["reason"])
                self.assertEqual("failed", self.history()[-1]["result"])
                self.assertFalse(self.created())

    def test_watch_detects_abandoned_deploy_and_rejects_reused_process_identity(self):
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
                raise subprocess.TimeoutExpired(args, 1)
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", interrupt_after_mark):
            with self.assertRaises(subprocess.TimeoutExpired):
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

    def test_resume_background_failure_restores_old_owner_and_clears_pause(self):
        self.hold()
        self.ops.unready_ports.add(self.cfg.slots["a"]["background"])
        with self.assertRaisesRegex(deploy.DeployError, "previous background .* running again"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertIsNone(self.state()["in_progress"])
        self.assert_live_release(LEGACY)
        self.assertFalse(self.ops.switched)

    def test_resume_guard_failure_restores_traffic_and_background(self):
        self.hold()
        self.ops.memory_after_switch = 20 * GIB
        with self.assertRaisesRegex(deploy.DeployError, "guard failed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertFalse(self.ops.containers["sources-main-a-serving"]["State"]["Running"])
        self.assert_live_release(LEGACY)

    def test_resume_unrestored_failure_is_never_reported_as_a_pause(self):
        self.hold()
        self.ops.drain_seconds["old-bg"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])
        self.assertTrue(any("release_incomplete" in alert["alert"]
                            for alert in deploy.watch_once(self.cfg, self.ops, {})))
        for action in ("resume", "abort"):
            with self.assertRaisesRegex(deploy.DeployError, "not a pause"):
                getattr(deploy.Deployer(self.cfg, self.ops), action)()

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

    def test_cli_held_dry_run_does_not_create_a_lock_file_or_state(self):
        lock = self.tmp / "missing-lock-directory" / "release.lock"
        self.cfg.lock_file = str(lock)
        self.hold()
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
            with self.assertRaisesRegex(deploy.DeployError, "network disappeared.*pause restored"):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual([self.cfg.networks], network_checks)
        self.assertEqual(containers, self.ops.containers)
        self.assertEqual(held, self.state()["in_progress"])
        self.assert_live_release(LEGACY)

    def test_resume_nginx_failure_hands_background_back(self):
        self.hold()
        self.ops.nginx_test_rc = 1
        with self.assertRaisesRegex(deploy.DeployError, "owns the background again"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

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
                alias_joins.append(args[-2])
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
        self.hold()
        endpoint = self.ops.containers["sources-main-a-serving"]["NetworkSettings"]["Networks"][self.cfg.networks[0]]
        endpoint["Aliases"].append(self.cfg.serving_alias)
        with self.assertRaisesRegex(deploy.DeployError, "network isolation changed"):
            deploy.Deployer(self.cfg, self.ops).resume()
        self.assertEqual("held", self.state()["in_progress"]["phase"])

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

    def test_main_reload_error_after_route_changes_restores_previous_release(self):
        run = self.ops.run
        old_upstreams = self.ops.loaded_upstreams
        for error in (deploy.DeployError("simulated reload failure"), subprocess.TimeoutExpired([], 1)):
            with self.subTest(error=type(error).__name__):
                failed = False

                def fail_reload(args, **kw):
                    nonlocal failed
                    result = run(args, **kw)
                    if args == [self.cfg.nginx_bin, "-s", "reload"] and not failed:
                        failed = True
                        self.assertNotEqual(old_upstreams, self.ops.loaded_upstreams)
                        raise error
                    return result

                with mock.patch.object(self.ops, "run", fail_reload):
                    with self.assertRaisesRegex(deploy.DeployError, "previous routing restored"):
                        self.deploy()
                self.assertTrue(failed)
                self.assert_live_release(LEGACY)
                self.assertIsNone(self.state()["in_progress"])

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
        self.assertEqual(LEGACY, self.state()["current"])
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

    def test_resume_network_restore_failure_keeps_failed_progress_instead_of_idle_state(self):
        self.hold()
        run = self.ops.run

        def fail(args, **kw):
            if args[:3] == ["docker", "network", "connect"]:
                raise deploy.DeployError("simulated connection failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaises(deploy.FallbackFailed):
                deploy.Deployer(self.cfg, self.ops).resume()
        self.assertIn("restoring the pause failed", self.state()["in_progress"]["reason"])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assert_live_release(LEGACY)
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_main_reload_restore_failure_keeps_failed_progress(self):
        run = self.ops.run
        applied = False

        def fail(args, **kw):
            nonlocal applied
            if args == [self.cfg.nginx_bin, "-s", "reload"]:
                if not applied:
                    run(args, **kw)
                    applied = True
                raise deploy.DeployError("simulated reload failure")
            return run(args, **kw)

        with mock.patch.object(self.ops, "run", fail):
            with self.assertRaises(deploy.FallbackFailed):
                self.deploy()
        progress = self.state()["in_progress"]
        self.assertEqual("switch", progress["failed_step"])
        self.assertIn("restoring previous routing failed", progress["reason"])
        self.assertEqual(self.upstreams_for(progress["target"]), self.ops.loaded_upstreams)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertTrue(self.ops.inspect(progress["target"]["containers"]["background"])["State"]["Running"])
        self.assertFalse(self.ops.inspect(LEGACY["containers"]["background"])["State"]["Running"])

    def test_main_nginx_test_without_backup_keeps_fault(self):
        del self.ops.files[self.cfg.upstream_file]
        self.ops.nginx_test_rc = 1
        with self.assertRaisesRegex(deploy.FallbackFailed, "no previous upstream file"):
            self.deploy()
        self.assertEqual(self.upstreams_for(LEGACY), self.ops.loaded_upstreams)
        owner = deploy.Deployer(self.cfg, self.ops).owner_runtime(LEGACY) or {}
        self.assertTrue(owner.get("owns_background"))
        target = self.state()["in_progress"]["target"]
        self.assertFalse(self.ops.inspect(target["containers"]["background"])["State"]["Running"])
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertEqual("switch", self.state()["in_progress"]["failed_step"])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])

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
        read = self.ops.read_text

        def unreadable(path):
            if path == self.cfg.upstream_file:
                raise PermissionError("simulated upstream read failure")
            return read(path)

        with mock.patch.object(self.ops, "read_text", unreadable):
            with self.assertRaisesRegex(deploy.DeployError, "cannot read the previous upstream file"):
                self.deploy()
        self.assertFalse(self.ops.switched)
        self.assert_live_release(LEGACY)
        self.assertIsNone(self.state()["in_progress"])

    def test_pause_write_failure_preserves_failed_progress_and_running_target(self):
        real = deploy.save_state

        def fail_pause(cfg, state):
            if (state.get("in_progress") or {}).get("phase") == "held":
                raise OSError("simulated pause write failure")
            return real(cfg, state)

        with mock.patch.object(deploy, "save_state", fail_pause):
            with self.assertRaises(deploy.FallbackFailed):
                self.hold()
        self.assertEqual("start serving", self.state()["in_progress"]["failed_step"])
        self.assertEqual("failed", self.state()["in_progress"]["phase"])
        self.assertTrue(self.state()["in_progress"]["reason"])
        self.assertTrue(self.ops.containers["sources-main-a-serving"]["State"]["Running"])
        self.assertFalse(self.ops.switched)

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
        self.assertEqual([[self.cfg.nginx_bin, "-T"]], self.ops.commands)

    def test_abort_state_write_failure_after_removal_can_retry_without_containers(self):
        self.hold()
        real = deploy.save_state

        def fail_clear(cfg, state):
            if not state.get("in_progress"):
                raise OSError("simulated state write failure")
            return real(cfg, state)

        with mock.patch.object(deploy, "save_state", fail_clear):
            with self.assertRaisesRegex(deploy.DeployError, "metadata write failed"):
                deploy.Deployer(self.cfg, self.ops).abort()
        self.assertEqual("aborting", self.state()["in_progress"]["phase"])
        self.assertNotIn("sources-main-a-serving", self.ops.containers)
        deploy.Deployer(self.cfg, self.ops).abort()
        self.assertIsNone(self.state()["in_progress"])

    def test_main_handoff_create_failure_restores_old_background_restart_policy(self):
        real = self.ops.run

        def fail_create(args, **kw):
            if args[:2] == ["docker", "create"] and "sources-main-a-background" in args:
                raise deploy.DeployError("simulated create failure")
            return real(args, **kw)

        with mock.patch.object(self.ops, "run", fail_create):
            with self.assertRaises(deploy.DeployError):
                self.deploy()
        old = self.ops.containers["old-bg"]
        self.assertTrue(old["State"]["Running"])
        self.assertEqual("unless-stopped", old["HostConfig"]["RestartPolicy"]["Name"])
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

    def test_rollback_with_an_owner_that_never_exits_keeps_in_progress(self):
        self.deploy()
        self.ops.drain_seconds["sources-main-a-background"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed):
            deploy.Deployer(self.cfg, self.ops).rollback()
        in_progress = self.state()["in_progress"]
        self.assertEqual("background handoff", in_progress["failed_step"])
        # Traffic is still on slot a while its background drains; the restarted
        # previous serving containers have none.
        for part in ("sources-main-a-background is still draining", "Traffic is on slot a.",
                     "Serving containers left outside the Nginx route: old-serving, old-spare",
                     "'docker start sources-main-a-background'"):
            self.assertIn(part, in_progress["reason"])

    def test_handing_back_to_an_owner_that_never_exits_names_the_previous_background(self):
        # Guard fails, traffic goes back to the previous slot, and the new
        # background never exits: the person must start the previous release's
        # background, not restart the newer one.
        self.ops.memory_after_switch = 20 * GIB
        self.ops.drain_seconds["sources-main-a-background"] = float("inf")
        with self.assertRaises(deploy.FallbackFailed) as ctx:
            self.deploy()
        message = str(ctx.exception)
        slot = self.cfg.slot_view("a")
        serving = ", ".join(slot["containers"][role] for role in deploy.SERVING_ROLES)
        for part in ("sources-main-a-background is still draining", "old-bg was not started",
                     "Traffic is on slot legacy",
                     f"Serving containers left outside the Nginx route: {serving}",
                     "start the background of slot legacy (not sources-main-a-background): "
                     "'docker update --restart=unless-stopped old-bg' and 'docker start old-bg'"):
            self.assertIn(part, message)
        self.assertNotIn("docker start sources-main-a-background", message)
        self.assertIn("127.0.0.1:9426 max_fails", self.ops.files[self.cfg.upstream_file], "traffic is back")
        self.assertEqual(message, self.state()["in_progress"]["reason"])

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

    def test_rollback_guard_failure_restores_release_and_clears_progress(self):
        self.deploy()
        original = self.state()
        inspect = self.ops.inspect
        failed = False

        def fail_guard(name):
            nonlocal failed
            if (name == LEGACY["containers"]["serving"] and self.ops.loaded_upstreams == self.upstreams_for(LEGACY)
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
