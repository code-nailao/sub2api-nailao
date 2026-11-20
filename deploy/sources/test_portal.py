import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import deploy  # noqa: E402
import portal  # noqa: E402
from test_deploy import FakeOps  # noqa: E402

SECRET = "portal-secret-value-do-not-print"
OLD = "portal-old"
NEW_A, NEW_B = "sources-portal-a", "sources-portal-b"
PORT_OLD, PORT_A, PORT_B = 9300, 9301, 9302
PUBLIC = "https://example.com/health"
NGINX_DUMP = ("upstream sources_portal {\n    server 127.0.0.1:9300;\n}\n"
              "server { location / { proxy_pass http://sources_portal; } }\n")


class PortalSim(FakeOps):
    """FakeOps plus what the Portal adds: a state lock shared by all Portal
    containers (a second one exits at once with code 1), SIGTERM that takes
    a moment to finish, the -check-config one-off, hung commands and file
    modes. Docker never restarts a signalled container."""

    def __init__(self, tmp: Path, dry_run: bool = False):
        super().__init__(tmp, dry_run=dry_run)
        self.nginx_dump = NGINX_DUMP
        self.exit_delay = 1.0
        self.ignore_term: set[str] = set()
        self.crash_on_start: set[str] = set()
        self.unhealthy_ports: set[int] = set()
        self.check_rc = 0
        self.check_out = f"INFO starting check\n{portal.CONFIG_OK}\n"
        self.checks: list[list[str]] = []
        self.modes: dict[str, int] = {}
        self.on_reload: list = []
        self.at_clock: list = []
        self.writes: list[str] = []
        self.hang: set[tuple[str, ...]] = set()
        self.on_create: list = []

    # time ----------------------------------------------------------------------
    def settle(self):
        due = [fn for t, fn in self.at_clock if self.clock >= t]
        self.at_clock = [(t, fn) for t, fn in self.at_clock if self.clock < t]
        for fn in due:
            fn()
        for c in self.containers.values():
            if c.get("exit_at") is not None and self.clock >= c["exit_at"]:
                c["State"].update(Running=False, Status="exited", ExitCode=0)
                c["exit_at"] = None

    def inspect(self, name):
        self.settle()
        return super().inspect(name)

    def is_portal(self, name):
        return "-check-" not in name

    def run(self, args, *, mutate, timeout=600, check=True):
        self.settle()
        if tuple(args) in self.hang and not (self.dry_run and mutate):
            self.commands.append(list(args))
            raise subprocess.TimeoutExpired(args, timeout)
        if self.dry_run and mutate:
            return super().run(args, mutate=mutate, timeout=timeout, check=check)
        if args[:2] == ["docker", "create"] and "-check-" in args[args.index("--name") + 1]:
            self.commands.append(list(args))
            name = args[args.index("--name") + 1]
            self.add(name, 0, running=False, image=args[-2])
            self.checks.append(list(args))
            return subprocess.CompletedProcess(args, 0, "", "")
        if args[:3] == ["docker", "start", "--attach"]:
            self.commands.append(list(args))
            rc = self.check_rc
            if check and rc:
                raise deploy.DeployError("check failed")
            return subprocess.CompletedProcess(args, rc, self.check_out, "")
        if args[:2] == ["docker", "kill"]:
            self.commands.append(list(args))
            c = self.containers.get(args[-1])
            if not c or not c["State"]["Running"]:
                if check:
                    raise deploy.DeployError("not running")
                return subprocess.CompletedProcess(args, 1, "", "")
            if args[-1] not in self.ignore_term:
                c["exit_at"] = self.clock + self.exit_delay
            return subprocess.CompletedProcess(args, 0, "", "")
        if args[:2] == ["docker", "start"]:
            name = args[-1]
            holder = [n for n, c in self.containers.items()
                      if n != name and self.is_portal(n) and c["State"]["Running"]]
            if holder or name in self.crash_on_start:
                # The Portal starts, cannot take the state lock and exits.
                self.commands.append(list(args))
                self.containers[name]["State"].update(Running=False, Status="exited", ExitCode=1,
                                                      StartedAt=f"t{self.clock}")
                self.containers[name]["lock_refused"] = holder
                return subprocess.CompletedProcess(args, 0, "", "")
        result = super().run(args, mutate=mutate, timeout=timeout, check=check)
        if args[:2] == ["docker", "create"]:
            hooks, self.on_create = self.on_create, []
            for hook in hooks:
                hook()
        if args[1:] == ["-s", "reload"]:
            hooks, self.on_reload = self.on_reload, []
            for hook in hooks:
                hook()
        return result

    def http_json(self, url, token=None, method="GET", body=None, timeout=5):
        self.settle()
        if url.startswith("https://"):
            return super().http_json(url, token, method, body, timeout)
        port = int(url.split(":")[2].split("/")[0])
        c = next((c for c in self.containers.values() if c["port"] == port and c["State"]["Running"]), None)
        if not c or c.get("exit_at") is not None:
            return 0, None  # a signalled Portal closes its listener at once
        if port in self.unhealthy_ports:
            return 503, None
        return 200, {"status": "ok"}

    def write_file_atomic(self, path, text, mode=0o644):
        if self.dry_run:
            self.commands.append(["DRY", "write", path])
            return
        self.files[path] = text
        self.writes.append(path)
        if path.startswith(str(self.tmp)):
            Path(path).parent.mkdir(parents=True, exist_ok=True)
            Path(path).write_text(text)
            os.chmod(path, mode)



class PortalFakeOps(portal.PortalOps, PortalSim):
    """PortalOps.run (error conversion) on top of the simulation."""

    def path_mode(self, path):
        if path in self.modes:
            return self.modes[path]
        if path.startswith(str(self.tmp)):
            return portal.PortalOps.path_mode(self, path)
        return None


def make_config(tmp: Path, **kw) -> portal.PortalConfig:
    raw = {
        "name_prefix": "sources-portal", "slots": {"a": PORT_A, "b": PORT_B}, "networks": ["net-one", "net-two"],
        "memory": "2g", "cpus": "2", "pids_limit": 1024, "log_max_size": "10m", "log_max_file": "3",
        "env_file": "/srv/example/portal.env",
        "mounts": [{"source": "/srv/example/state", "target": "/data", "readonly": False},
                   {"source": "/srv/example/registry.json", "target": "/config/registry.json"}],
        "upstream_file": str(tmp / "nginx" / "portal-upstream.conf"), "nginx_bin": "/usr/sbin/nginx",
        "nginx_error_log": "/var/log/nginx/error.log", "work_dir": str(tmp / "work"),
        "state_file": str(tmp / "state.json"), "history_file": str(tmp / "history.jsonl"),
        "lock_file": str(tmp / "release.lock"), "public_health_url": PUBLIC,
        "user": "1001:1001", "security_opts": ["no-new-privileges"],
    }
    raw.update(kw)
    return portal.PortalConfig.from_dict(raw)


LEGACY = {"slot": "legacy", "image": "old@sha256:1", "container": OLD, "port": PORT_OLD,
          "restart_policy": "unless-stopped", "deployed_at": None}


class PortalTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.fresh()

    def fresh(self):
        for p in list(self.tmp.iterdir()) if self.tmp.exists() else []:
            if p.is_file():
                p.unlink()
        self.cfg = make_config(self.tmp)
        self.ops = PortalFakeOps(self.tmp)
        self.ops.upstream_file = self.cfg.upstream_file
        self.ops.add(OLD, PORT_OLD)
        self.ops.modes.update({self.cfg.env_file: 0o600, "/srv/example/state": 0o755,
                               "/srv/example/registry.json": 0o644})
        self.ops.files["/proc/sys/net/ipv4/ip_local_reserved_ports"] = "\n"
        self.ops.write_file_atomic(self.cfg.upstream_file, portal.render_upstream("sources_portal", PORT_OLD))
        self.ops.loaded_upstreams = self.ops.files[self.cfg.upstream_file]
        self.ops.writes.clear()
        deploy.save_state(self.cfg, {"current": dict(LEGACY), "previous": None, "in_progress": None})

    def tearDown(self):
        self._tmp.cleanup()

    # helpers ----------------------------------------------------------------------
    def deployer(self):
        return portal.PortalDeployer(self.cfg, self.ops)

    def deploy(self, image="new@sha256:2", **kw):
        self.deployer().deploy_portal(image, **kw)

    def state(self):
        return deploy.load_state(self.cfg)

    def history(self):
        path = Path(self.cfg.history_file)
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def index(self, cmd):
        return self.ops.commands.index(cmd)

    def created(self):
        return [c[c.index("--name") + 1] for c in self.ops.commands
                if c[:2] == ["docker", "create"] and "-check-" not in c[c.index("--name") + 1]]

    def upstream(self):
        return self.ops.files[self.cfg.upstream_file]

    def running(self, name):
        self.ops.settle()
        return self.ops.containers[name]["State"]["Running"]

    def policy(self, name):
        return self.ops.containers[name]["HostConfig"]["RestartPolicy"]["Name"]

    def touched_old(self):
        return [c for c in self.ops.commands if c[:2] in (["docker", "kill"], ["docker", "update"], ["docker", "start"])
                and c[-1] == OLD]

    # happy path ---------------------------------------------------------------------
    def test_happy_path_order_and_result(self):
        self.deploy()
        create = next(c for c in self.ops.commands if c[:2] == ["docker", "create"] and NEW_A in c)
        i_create = self.ops.commands.index(create)
        i_old_no = self.index(["docker", "update", "--restart=no", OLD])
        i_term = self.index(["docker", "kill", "--signal=TERM", OLD])
        i_start = self.index(["docker", "start", NEW_A])
        i_reload = self.index(["/usr/sbin/nginx", "-s", "reload"])
        i_new_keep = self.index(["docker", "update", "--restart=unless-stopped", NEW_A])
        self.assertLess(i_create, i_old_no)
        self.assertLess(i_old_no, i_term)
        self.assertLess(i_term, i_start)
        self.assertLess(i_start, i_reload)
        self.assertLess(i_reload, i_new_keep)
        self.assertEqual(1, sum(1 for c in self.ops.commands if c[:2] == ["docker", "kill"]))
        self.assertTrue(all(c[2] == "--signal=TERM" for c in self.ops.commands if c[:2] == ["docker", "kill"]))
        self.assertNotIn("lock_refused", self.ops.containers[NEW_A], "new started while old still held the lock")
        self.assertTrue(self.running(NEW_A))
        self.assertFalse(self.running(OLD))
        self.assertIn(OLD, self.ops.containers, "the legacy container is kept")
        self.assertEqual("unless-stopped", self.policy(NEW_A))
        self.assertEqual("no", self.policy(OLD))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_A), self.upstream())
        state = self.state()
        self.assertEqual({"slot": "a", "image": "new@sha256:2", "container": NEW_A, "port": PORT_A,
                          "restart_policy": "unless-stopped"},
                         {k: v for k, v in state["current"].items() if k != "deployed_at"})
        self.assertEqual("legacy", state["previous"]["slot"])
        self.assertIsNone(state["in_progress"])
        self.assertEqual(0o600, os.stat(self.cfg.state_file).st_mode & 0o777)
        record = self.history()[-1]
        self.assertEqual(("portal", "ok"), (record["action"], record["result"]))
        self.assertEqual(["pull image", "preflight", "startup check", "create", "handoff", "switch", "guard"],
                         [s["step"] for s in record["steps"]])

    def test_create_arguments(self):
        self.deploy()
        create = next(c for c in self.ops.commands if c[:2] == ["docker", "create"] and NEW_A in c)
        joined = " ".join(create)
        self.assertEqual("no", create[create.index("--restart") + 1])
        self.assertEqual(f"127.0.0.1:{PORT_A}:18090", create[create.index("--publish") + 1])
        self.assertEqual(NEW_A, create[create.index("--hostname") + 1])
        self.assertEqual(portal.ENTRYPOINT, create[create.index("--entrypoint") + 1])
        self.assertEqual("net-one", create[create.index("--network") + 1])
        self.assertEqual([NEW_A], [create[i + 1] for i, a in enumerate(create) if a == "--network-alias"])
        self.assertIn("PORTAL_LISTEN=0.0.0.0:18090", create)
        self.assertIn("--env-file /srv/example/portal.env", joined)
        self.assertIn("com.sources.deploy.role=portal", create)
        self.assertIn("com.sources.deploy.slot=a", create)
        self.assertIn("--security-opt no-new-privileges", joined)
        self.assertIn("--user 1001:1001", joined)
        self.assertIn("type=bind,source=/srv/example/state,target=/data", create)
        self.assertEqual("new@sha256:2", create[-1])
        self.assertIn(["docker", "network", "connect", "--alias", NEW_A, "net-two", NEW_A], self.ops.commands)

    def test_startup_check_is_read_only(self):
        self.deploy()
        self.assertEqual(1, len(self.ops.checks))
        check = self.ops.checks[0]
        name = check[check.index("--name") + 1]
        self.assertEqual(["new@sha256:2", "-check-config"], check[-2:])
        self.assertNotIn("--publish", check)
        self.assertEqual("no", check[check.index("--restart") + 1])
        mounts = [check[i + 1] for i, a in enumerate(check) if a == "--mount"]
        self.assertEqual(2, len(mounts))
        self.assertTrue(all(m.endswith(",readonly") for m in mounts), mounts)
        self.assertIn("--user", check)
        self.assertIn("--security-opt", check)
        self.assertIn(["docker", "rm", "-f", name], self.ops.commands)
        self.assertLess(self.index(["docker", "rm", "-f", name]),
                        self.index(["docker", "kill", "--signal=TERM", OLD]))

    # preflight ---------------------------------------------------------------------
    def test_each_preflight_problem_changes_nothing(self):
        cases = {
            "no current Portal release": lambda: deploy.save_state(
                self.cfg, {"current": None, "previous": None, "in_progress": None}),
            f"current container {OLD} is not running": lambda: self.ops.containers[OLD]["State"].update(Running=False),
            f"current container {OLD} does not exist": lambda: self.ops.containers.pop(OLD),
            "current Portal /health on port 9300 returned 503": lambda: self.ops.unhealthy_ports.add(PORT_OLD),
            "defines no 'upstream sources_portal'": lambda: setattr(
                self.ops, "nginx_dump", "proxy_pass http://sources_portal;"),
            "no proxy_pass uses http://sources_portal": lambda: setattr(
                self.ops, "nginx_dump", "upstream sources_portal {}\nproxy_pass http://sources_portal_x;"),
            "differs from what the tool renders": lambda: self.ops.files.update(
                {self.cfg.upstream_file: portal.render_upstream("sources_portal", 9999)}),
            f"{NEW_A} (slot a) is running": lambda: self.ops.add(NEW_A, PORT_A),
            f"ports [{PORT_A}] are already in use": lambda: self.ops.busy_ports.add(PORT_A),
            "env file /srv/example/portal.env does not exist": lambda: self.ops.modes.pop(self.cfg.env_file),
            "is readable by group or others (mode 0640)": lambda: self.ops.modes.update({self.cfg.env_file: 0o640}),
            "mount source /srv/example/registry.json does not exist":
                lambda: self.ops.modes.pop("/srv/example/registry.json"),
            "public health check returned status 503": lambda: setattr(self.ops, "public_status", 503),
        }
        for expected, breaks in cases.items():
            with self.subTest(expected):
                self.fresh()
                breaks()
                state_before = Path(self.cfg.state_file).read_bytes()
                upstream_before = self.ops.files[self.cfg.upstream_file]
                with self.assertRaises(deploy.DeployError) as ctx:
                    self.deploy()
                message = str(ctx.exception)
                self.assertIn("preflight failed, nothing changed", message)
                self.assertIn(expected, message)
                self.assertEqual([], self.created())
                self.assertEqual([], self.ops.checks, "the start-up check runs after the preflight")
                self.assertFalse(any(c[:2] in (["docker", "kill"], ["docker", "update"]) for c in self.ops.commands))
                self.assertEqual([], self.ops.writes)
                self.assertEqual(upstream_before, self.ops.files[self.cfg.upstream_file])
                self.assertEqual(state_before, Path(self.cfg.state_file).read_bytes())
                self.assertEqual("failed", self.history()[-1]["result"])

    def test_preflight_collects_all_problems(self):
        self.ops.modes.pop(self.cfg.env_file)
        self.ops.public_status = 0
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("does not exist", str(ctx.exception))
        self.assertIn("returned status 0", str(ctx.exception))

    def test_allow_public_health_failure_warns_and_deploys(self):
        self.ops.public_status = 503
        self.ops.public_status_after_reload = 200
        self.deploy(allow_public_health_failure=True)
        self.assertTrue(any("WARNING" in line and "status 503" in line for line in self.ops.logs))
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_unfinished_release_blocks_the_next_one(self):
        state = self.state()
        state["in_progress"] = {"step": "handoff"}
        deploy.save_state(self.cfg, state)
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("did not finish", str(ctx.exception))
        self.assertEqual([], self.ops.commands)

    # start-up check -----------------------------------------------------------------
    def test_startup_check_failure_reports_its_line_and_touches_nothing(self):
        for out in ("config: PORTAL_EXAMPLE_KEY is not a valid value\n",
                    "state directory is locked by another process\n"):  # image without the flag
            with self.subTest(out):
                self.fresh()
                self.ops.check_rc, self.ops.check_out = 1, "INFO starting\n" + out
                with self.assertRaises(deploy.DeployError) as ctx:
                    self.deploy()
                self.assertIn(out.strip(), str(ctx.exception))
                self.assertIn("nothing changed", str(ctx.exception))
                self.assertEqual([], self.created())
                self.assertEqual([], self.touched_old())
                self.assertTrue(self.running(OLD))
                name = self.ops.checks[0][self.ops.checks[0].index("--name") + 1]
                self.assertIn(["docker", "rm", "-f", name], self.ops.commands)
                self.assertIsNone(self.state()["in_progress"])

    def test_startup_check_needs_the_ok_line(self):
        self.ops.check_out = "exited without saying anything useful\n"
        with self.assertRaises(deploy.DeployError):
            self.deploy()
        self.assertEqual([], self.created())

    # handoff failures ------------------------------------------------------------------
    def test_slow_old_exit_keeps_waiting_and_completes(self):
        self.ops.exit_delay = 500  # longer than exit_timeout (420), shorter than the abort timeout
        self.deploy()
        warnings = [line for line in self.ops.logs if "WARNING" in line and OLD in line]
        self.assertTrue(warnings, self.ops.logs)
        self.assertIn("pages are unavailable", warnings[0])
        self.assertIn("starts the new container as soon as it exits", warnings[0])
        self.assertIn("will not kill anything", warnings[0])
        self.assertEqual(1, sum(1 for c in self.ops.commands if c[:2] == ["docker", "kill"]))
        self.assertNotIn("lock_refused", self.ops.containers[NEW_A])
        self.assertEqual("a", self.state()["current"]["slot"])
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_wait_notes_are_time_based(self):
        self.ops.exit_delay = 100
        self.deploy()
        notes = [line for line in self.ops.logs if "still finishing in-flight work" in line]
        self.assertEqual(3, len(notes), notes)  # at 30, 60 and 90 seconds
        self.assertEqual(0.25, portal.POLL)

    def test_old_never_exiting_aborts_without_killing(self):
        self.ops.ignore_term = {OLD}
        with self.assertRaises(portal.OldStillRunning) as ctx:
            self.deploy()
        message = str(ctx.exception)
        self.assertGreaterEqual(self.ops.clock, self.cfg.exit_abort_timeout)
        self.assertIn("nothing was killed", message)
        self.assertIn("stays unavailable until that process exits", message)
        self.assertIn("Docker will not restart it", message)
        self.assertIn(f"docker start {OLD}", message)
        self.assertIn("clears in_progress", message)
        self.assertIn("created but never started", message)
        self.assertNotIn("Docker starts it again", message)
        kills = [c for c in self.ops.commands if c[:2] == ["docker", "kill"]]
        self.assertEqual([["docker", "kill", "--signal=TERM", OLD]], kills)
        self.assertNotIn(["docker", "start", NEW_A], self.ops.commands)
        self.assertIn(NEW_A, self.ops.containers)
        self.assertTrue(self.running(OLD))
        self.assertEqual("unless-stopped", self.policy(OLD), "recorded policy restored (harmless)")
        self.assertEqual("handoff", self.state()["in_progress"]["step"])
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertFalse(self.ops.switched)
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertTrue(any("FAILED, NEEDS AN OPERATOR" in line for line in self.ops.logs))
        with self.assertRaises(deploy.DeployError):
            self.deploy("other@sha256:9")

    def test_current_dying_between_preflight_and_handover_proceeds(self):
        def die():
            self.ops.containers[OLD]["State"].update(Running=False, Status="exited", ExitCode=2)
        self.ops.on_create.append(die)
        self.deploy()
        self.assertFalse(any(c[:2] == ["docker", "kill"] for c in self.ops.commands))
        self.assertTrue(any("is not running; nothing to stop" in line for line in self.ops.logs))
        self.assertTrue(self.running(NEW_A))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_A), self.upstream())
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_command_timeout_while_starting_new_restores_old(self):
        self.ops.hang = {("docker", "start", NEW_A)}
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("timed out", str(ctx.exception))
        self.assertIn("is serving again", str(ctx.exception))
        self.assertTrue(self.running(OLD))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_oserror_from_a_command_is_a_deploy_error(self):
        with mock.patch.object(portal.deploy.subprocess, "run", side_effect=FileNotFoundError(2, "No such file")):
            with self.assertRaises(deploy.DeployError) as ctx:
                portal.PortalOps().run(["docker", "start", "x"], mutate=True)
        self.assertIn("could not run", str(ctx.exception))
        with mock.patch.object(portal.deploy.subprocess, "run",
                               side_effect=subprocess.TimeoutExpired(["docker"], 5)):
            with self.assertRaises(deploy.DeployError):
                portal.PortalOps().run(["docker", "start", "x"], mutate=True, timeout=5)

    def test_interrupt_during_guard_restores_old_and_fails(self):
        def interrupt():
            raise KeyboardInterrupt
        self.ops.on_reload.append(lambda: self.ops.at_clock.append((self.ops.clock + 3, interrupt)))
        try:
            with self.assertRaises(deploy.DeployError) as ctx:
                self.deploy()
        except KeyboardInterrupt:
            self.fail("the interrupt escaped without restoring the old Portal")
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("interrupted", str(ctx.exception))
        self.assertIn("is serving again", str(ctx.exception))
        self.assertTrue(self.running(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_failed_kill_with_old_still_serving_falls_back_without_waiting(self):
        self.ops.hang = {("docker", "kill", "--signal=TERM", OLD)}
        started = self.ops.clock
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn(f"the previous Portal {OLD} is serving again", str(ctx.exception))
        self.assertLess(self.ops.clock - started, 10, "no waiting for a Portal that was never signalled")
        self.assertTrue(any("is still serving; nothing to wait for" in line for line in self.ops.logs))
        self.assertTrue(self.running(OLD))
        self.assertNotIn(["docker", "start", NEW_A], self.ops.commands)
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertIsNone(self.state()["in_progress"])

    def test_interrupt_while_old_drains_waits_then_restores_old(self):
        def interrupt():
            raise KeyboardInterrupt
        self.ops.exit_delay = 5
        self.ops.on_create.append(lambda: self.ops.at_clock.append((self.ops.clock + 1, interrupt)))
        try:
            with self.assertRaises(deploy.DeployError) as ctx:
                self.deploy()
        except KeyboardInterrupt:
            self.fail("the interrupt escaped without restoring the old Portal")
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("interrupted", str(ctx.exception))
        self.assertTrue(self.running(OLD))
        self.assertNotIn("lock_refused", self.ops.containers[OLD])
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())

    def test_new_never_healthy_restarts_old(self):
        self.ops.unhealthy_ports = {PORT_A}
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("is serving again", str(ctx.exception))
        self.assertTrue(self.running(OLD))
        self.assertEqual("unless-stopped", self.policy(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertIn(NEW_A, self.ops.containers, "kept for inspection")
        self.assertNotIn("lock_refused", self.ops.containers[OLD])
        self.assertFalse(self.ops.switched, "Nginx never reloaded")
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertEqual("legacy", self.state()["current"]["slot"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_new_crashing_fails_fast(self):
        self.ops.crash_on_start = {NEW_A}
        started = self.ops.clock
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("exited before answering /health", str(ctx.exception))
        self.assertLess(self.ops.clock - started, 10)
        self.assertTrue(self.running(OLD))

    def test_nginx_test_failure_restores_file_and_old(self):
        self.ops.nginx_test_rc = 1
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("nginx -t rejected", str(ctx.exception))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD),
                         Path(self.cfg.upstream_file).read_text())
        self.assertTrue(self.running(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertFalse(self.ops.switched)
        self.assertEqual("legacy", self.state()["current"]["slot"])

    def guard_breaks(self):
        def restart():
            self.ops.containers[NEW_A]["RestartCount"] += 1

        def oom():
            self.ops.containers[NEW_A]["State"]["OOMKilled"] = True

        def stop():
            self.ops.containers[NEW_A]["State"]["Running"] = False

        def errors():
            line = f'connect() failed (111) while connecting to upstream, upstream: "http://127.0.0.1:{PORT_A}/"'
            self.ops.error_log = "\n".join([line] * 5)

        return {
            "stopped or restarted": restart,
            "stopped or restarted ": stop,
            "OOM-killed": oom,
            "/health returned 503": lambda: self.ops.unhealthy_ports.add(PORT_A),
            "public health failed 3 times in a row (last status 502)":
                lambda: setattr(self.ops, "public_status", 502),
            "5 Nginx connect failures to port 9301": errors,
        }

    def test_each_guard_failure_switches_back(self):
        for expected, breaks in self.guard_breaks().items():
            with self.subTest(expected):
                self.fresh()
                # Break the new Portal a few seconds into the guard.
                self.ops.on_reload.append(lambda b=breaks: self.ops.at_clock.append((self.ops.clock + 3, b)))
                with self.assertRaises(deploy.DeployError) as ctx:
                    self.deploy()
                self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
                self.assertIn("guard failed", str(ctx.exception))
                self.assertIn(expected.strip(), str(ctx.exception))
                self.assertIn("is serving again", str(ctx.exception))
                self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
                self.assertTrue(self.running(OLD))
                self.assertEqual("unless-stopped", self.policy(OLD))
                self.assertFalse(self.running(NEW_A))
                self.assertEqual("no", self.policy(NEW_A))
                self.assertIn(NEW_A, self.ops.containers, "kept for inspection")
                self.assertEqual("legacy", self.state()["current"]["slot"])
                self.assertIsNone(self.state()["in_progress"])

    def test_guard_ignores_connect_failures_to_other_ports(self):
        line = 'connect() failed (111) while connecting to upstream, upstream: "http://127.0.0.1:{}/"'
        self.ops.on_reload.append(lambda: setattr(self.ops, "error_log", "\n".join([line.format(PORT_OLD)] * 50)))
        self.deploy()
        self.assertEqual("a", self.state()["current"]["slot"])

    def test_reload_transient_public_failures_precede_the_guard(self):
        checks = []
        pending = [502, 0, 200]
        probe = self.ops.http_json

        def public_health(url, *args, **kw):
            result = probe(url, *args, **kw)
            if url == PUBLIC:
                if self.ops.switched and pending:
                    result = (pending.pop(0), None)
                step = (self.state().get("in_progress") or {}).get("step")
                checks.append((step, result[0], self.ops.clock))
            return result

        with mock.patch.object(self.ops, "http_json", side_effect=public_health):
            self.deploy()

        self.assertFalse(pending, "wait for a successful public check after reload")
        guarded = [check for check in checks if check[0] == "guard"]
        self.assertTrue(guarded)
        self.assertTrue(all(status == 200 for _, status, _ in guarded), checks)
        self.assertGreaterEqual(guarded[-1][2] - guarded[0][2], self.cfg.guard_seconds)
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_public_health_never_recovers_after_reload_restores_old(self):
        self.cfg.ready_timeout = 2
        self.ops.public_status_after_reload = 502
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn("public health did not return 200 after reload", str(ctx.exception))
        self.assertIn("last status 502", str(ctx.exception))
        self.assertTrue(self.running(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertIsNone(self.state()["in_progress"])
        self.assertNotIn("guard", [step["step"] for step in self.history()[-1]["steps"]])

    def test_container_failure_while_waiting_for_public_health_restores_old(self):
        failures = {
            "stopped or restarted": {"RestartCount": 1},
            "stopped": {"State": {"Running": False, "Status": "exited"}},
            "restarted": {"State": {"StartedAt": "restarted"}},
            "OOM-killed": {"State": {"OOMKilled": True}},
        }
        for reason, change in failures.items():
            with self.subTest(reason):
                self.fresh()
                self.ops.public_status_after_reload = 502

                def fail(change=change):
                    container = self.ops.containers[NEW_A]
                    container["State"].update(change.get("State", {}))
                    container.update({key: value for key, value in change.items() if key != "State"})

                self.ops.on_reload.append(lambda: self.ops.at_clock.append((self.ops.clock + 0.5, fail)))
                with self.assertRaises(deploy.DeployError) as ctx:
                    self.deploy()
                self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
                self.assertIn(reason, str(ctx.exception))
                self.assertIn("while waiting for public health", str(ctx.exception))
                self.assertTrue(self.running(OLD))
                self.assertFalse(self.running(NEW_A))
                self.assertIsNone(self.state()["in_progress"])

    def test_restart_during_successful_public_health_check_restores_old(self):
        for action in ("deploy", "rollback"):
            for changed in ("RestartCount", "StartedAt", "both"):
                with self.subTest(action=action, changed=changed):
                    self.fresh()
                    if action == "rollback":
                        self.deploy()
                        self.ops.local_images.add(LEGACY["image"])
                        old, target, old_port = NEW_A, OLD, PORT_A
                        run = self.deployer().rollback_portal
                    else:
                        old, target, old_port = OLD, NEW_A, PORT_OLD
                        run = self.deploy
                    probe = self.ops.http_json
                    injected = False

                    def public_health(url, *args, **kw):
                        nonlocal injected
                        result = probe(url, *args, **kw)
                        step = (self.state().get("in_progress") or {}).get("step")
                        if url == PUBLIC and step == "switch" and not injected:
                            injected = True
                            container = self.ops.containers[target]
                            if changed in ("RestartCount", "both"):
                                container["RestartCount"] += 1
                            if changed in ("StartedAt", "both"):
                                container["State"]["StartedAt"] = "restarted-during-public-check"
                            return 200, {"status": "ok"}
                        return result

                    with mock.patch.object(self.ops, "http_json", side_effect=public_health):
                        with self.assertRaises(deploy.DeployError) as ctx:
                            run()

                    self.assertTrue(injected, "restart happened during the first public check after reload")
                    self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
                    self.assertIn("stopped or restarted while waiting for public health", str(ctx.exception))
                    self.assertTrue(self.running(old))
                    self.assertFalse(self.running(target))
                    self.assertEqual(portal.render_upstream("sources_portal", old_port), self.upstream())
                    self.assertEqual(old, self.state()["current"]["container"])
                    self.assertIsNone(self.state()["in_progress"])
                    self.assertEqual("failed", self.history()[-1]["result"])
                    self.assertNotIn("guard", [step["step"] for step in self.history()[-1]["steps"]])

    def test_fallback_failure_keeps_in_progress(self):
        self.ops.unhealthy_ports = {PORT_A}
        self.ops.crash_on_start = {OLD}
        with self.assertRaises(deploy.FallbackFailed) as ctx:
            self.deploy()
        self.assertIn("failed too", str(ctx.exception))
        self.assertIsNotNone(self.state()["in_progress"])
        self.assertTrue(any("FAILED AND NOT RESTORED" in line for line in self.ops.logs))
        with self.assertRaises(deploy.DeployError):
            self.deploy("other@sha256:9")

    def test_fallback_failure_when_switching_back_fails(self):
        def break_nginx():
            self.ops.unhealthy_ports.add(PORT_A)
            self.ops.nginx_test_rc = 1
        self.ops.on_reload.append(break_nginx)
        with self.assertRaises(deploy.FallbackFailed):
            self.deploy()
        self.assertIsNotNone(self.state()["in_progress"])

    def test_reload_error_runs_portal_recovery_before_reloading_the_old_route(self):
        run = self.ops.run
        reloads = []

        def fail_after_first_reload(args, **kw):
            result = run(args, **kw)
            if args[1:] == ["-s", "reload"]:
                reloads.append(self.upstream())
                if len(reloads) == 1:
                    raise deploy.DeployError("fixture: reload outcome is unknown")
                self.assertTrue(self.running(OLD))
                self.assertFalse(self.running(NEW_A))
            return result

        with mock.patch.object(self.ops, "run", fail_after_first_reload):
            with self.assertRaisesRegex(deploy.DeployError, "previous Portal.*serving again"):
                self.deploy()
        self.assertEqual([portal.render_upstream(self.cfg.upstream_name, port) for port in (PORT_A, PORT_OLD)], reloads)
        self.assertEqual(LEGACY, self.state()["current"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertEqual("failed", self.history()[-1]["result"])

    def test_failed_upstream_file_restore_still_recovers_portal(self):
        run, write = self.ops.run, self.ops.write_file_atomic
        original = self.upstream()
        failed_restore = False
        failed_test = False

        def reject_once(args, **kw):
            nonlocal failed_test
            if args[1:] == ["-t"] and not failed_test:
                failed_test = True
                return subprocess.CompletedProcess(args, 1, "", "fixture: rejected upstream")
            return run(args, **kw)

        def fail_file_restore(path, text, **kw):
            nonlocal failed_restore
            if path == self.cfg.upstream_file and text == original and not failed_restore:
                failed_restore = True
                raise OSError("fixture: could not restore old upstream")
            return write(path, text, **kw)

        with mock.patch.object(self.ops, "run", reject_once), mock.patch.object(self.ops, "write_file_atomic", fail_file_restore):
            with self.assertRaisesRegex(deploy.RoutingRestored, "previous Portal.*serving again"):
                self.deploy()
        self.assertTrue(failed_restore)
        self.assertTrue(self.running(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertEqual(original, self.upstream())
        self.assertEqual(original, self.ops.loaded_upstreams)
        self.assertIsNone(self.state()["in_progress"])

    # slots ---------------------------------------------------------------------------
    def test_slots_alternate_and_never_remove_legacy(self):
        self.ops.public_status = 200
        self.deploy("one@sha256:1")
        self.deploy("two@sha256:2")
        self.assertEqual(("b", "a"), (self.state()["current"]["slot"], self.state()["previous"]["slot"]))
        self.assertTrue(self.running(NEW_B))
        self.assertFalse(self.running(NEW_A))
        self.deploy("three@sha256:3")
        self.assertEqual(("a", "b"), (self.state()["current"]["slot"], self.state()["previous"]["slot"]))
        self.assertEqual("three@sha256:3", self.ops.containers[NEW_A]["Config"]["Image"])
        self.assertIn(["docker", "rm", "-f", NEW_A], self.ops.commands)
        self.assertIn(OLD, self.ops.containers)
        self.assertFalse(any(c[:2] == ["docker", "rm"] and c[-1] == OLD for c in self.ops.commands))

    def test_target_named_like_the_legacy_container_is_refused(self):
        state = self.state()
        state["previous"] = {**LEGACY, "container": NEW_B, "port": 9400}
        state["current"] = {**LEGACY, "slot": "a", "container": OLD}
        deploy.save_state(self.cfg, state)
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertIn("adopted legacy container", str(ctx.exception))

    # rollback ------------------------------------------------------------------------
    def test_rollback_happy_path(self):
        self.deploy()
        self.ops.local_images.add("old@sha256:1")
        self.ops.public_status = 502  # rollback is not blocked by a failing public check
        self.ops.public_status_after_reload = 200
        self.ops.commands.clear()
        self.ops.checks.clear()
        self.deployer().rollback_portal()
        self.assertEqual([], self.ops.checks, "no start-up check on rollback")
        self.assertEqual([], self.created(), "rollback creates nothing")
        self.assertEqual([], [c for c in self.ops.commands if c[:2] == ["docker", "rm"]])
        self.assertLess(self.index(["docker", "kill", "--signal=TERM", NEW_A]), self.index(["docker", "start", OLD]))
        self.assertTrue(self.running(OLD))
        self.assertFalse(self.running(NEW_A))
        self.assertNotIn("lock_refused", self.ops.containers[OLD])
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        self.assertEqual("unless-stopped", self.policy(OLD))
        state = self.state()
        self.assertEqual((OLD, NEW_A), (state["current"]["container"], state["previous"]["container"]))
        self.assertIsNone(state["in_progress"])
        self.assertEqual(("portal-rollback", "ok"), (self.history()[-1]["action"], self.history()[-1]["result"]))

    def test_rollback_when_current_is_not_running(self):
        self.deploy()
        self.ops.local_images.add("old@sha256:1")
        self.ops.containers[NEW_A]["State"].update(Running=False, Status="exited", ExitCode=2)
        self.ops.commands.clear()
        self.deployer().rollback_portal()
        self.assertFalse(any(c[:2] == ["docker", "kill"] for c in self.ops.commands))
        self.assertTrue(self.running(OLD))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_OLD), self.upstream())
        state = self.state()
        self.assertEqual((OLD, NEW_A), (state["current"]["container"], state["previous"]["container"]))
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_rollback_waits_for_public_health_before_the_guard(self):
        self.deploy()
        self.ops.local_images.add("old@sha256:1")
        self.ops.public_status_after_reload = 502
        self.ops.on_reload.append(lambda: self.ops.at_clock.append(
            (self.ops.clock + 0.5, lambda: setattr(self.ops, "public_status", 200))))
        guard = portal.PortalDeployer.guard_portal

        def guarded(deployer, new):
            self.assertEqual(200, self.ops.public_status)
            guard(deployer, new)

        with mock.patch.object(portal.PortalDeployer, "guard_portal", new=guarded):
            self.deployer().rollback_portal()
        self.assertEqual(OLD, self.state()["current"]["container"])
        self.assertEqual("ok", self.history()[-1]["result"])

    def test_rollback_failure_keeps_current(self):
        self.deploy()
        self.ops.local_images.add("old@sha256:1")
        self.ops.crash_on_start = {OLD}
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deployer().rollback_portal()
        self.assertNotIsInstance(ctx.exception, deploy.FallbackFailed)
        self.assertIn(f"previous Portal {NEW_A} is serving again", str(ctx.exception))
        self.assertTrue(self.running(NEW_A))
        self.assertEqual(portal.render_upstream("sources_portal", PORT_A), self.upstream())
        self.assertEqual(NEW_A, self.state()["current"]["container"])
        self.assertIsNone(self.state()["in_progress"])
        self.assertIn(OLD, self.ops.containers, "rollback never removes containers")

    def test_rollback_refusals(self):
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deployer().rollback_portal()
        self.assertIn("no previous Portal release", str(ctx.exception))
        self.deploy()
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deployer().rollback_portal()
        self.assertIn("previous image old@sha256:1 is no longer present", str(ctx.exception))
        self.ops.local_images.add("old@sha256:1")
        self.ops.containers.pop(OLD)
        self.ops.commands.clear()
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deployer().rollback_portal()
        self.assertIn(f"previous container {OLD} no longer exists", str(ctx.exception))
        self.assertFalse(any(c[:2] in (["docker", "kill"], ["docker", "update"]) for c in self.ops.commands))

    # dry run -------------------------------------------------------------------------
    def test_dry_run_checks_but_changes_nothing(self):
        self.ops.dry_run = True
        state_before = Path(self.cfg.state_file).read_bytes()
        self.deploy()
        self.assertEqual(1, len(self.ops.checks), "the start-up check runs for real")
        real = [c for c in self.ops.commands if c[0] != "DRY"]
        for cmd in real:
            self.assertFalse(cmd[:2] in (["docker", "kill"], ["docker", "update"])
                             or (cmd[:2] == ["docker", "start"] and "--attach" not in cmd)
                             or (cmd[:2] == ["docker", "create"] and "-check-" not in " ".join(cmd))
                             or cmd[1:] == ["-s", "reload"], cmd)
        dry = [c[1:] for c in self.ops.commands if c[0] == "DRY"]
        self.assertIn(["docker", "kill", "--signal=TERM", OLD], dry)
        self.assertIn(["write", self.cfg.upstream_file], dry)
        self.assertTrue(self.running(OLD))
        self.assertEqual([], self.ops.writes)
        self.assertEqual(state_before, Path(self.cfg.state_file).read_bytes())
        self.assertEqual([], self.history())
        self.assertTrue(any("DRY-RUN guard" in line for line in self.ops.logs))
        self.assertTrue(any("DRY-RUN wait" in line for line in self.ops.logs))
        self.assertEqual(1, self.ops.public_requests, "only the preflight sends a public request")

    def test_dry_run_rollback_prints_the_plan(self):
        self.deploy()
        self.ops.local_images.add("old@sha256:1")
        state_before = Path(self.cfg.state_file).read_bytes()
        history_before = self.history()
        self.ops.dry_run = True
        self.ops.commands.clear()
        self.deployer().rollback_portal()
        self.assertTrue(all(c[0] == "DRY" for c in self.ops.commands
                            if c[:2] in (["docker", "kill"], ["docker", "update"], ["docker", "start"])))
        self.assertIn(["DRY", "docker", "start", OLD], self.ops.commands)
        self.assertTrue(self.running(NEW_A))
        self.assertEqual(state_before, Path(self.cfg.state_file).read_bytes())
        self.assertEqual(history_before, self.history())

    # status --------------------------------------------------------------------------
    def test_status(self):
        self.deploy()
        report = portal.status(self.cfg, self.ops)
        self.assertIsNone(report["in_progress"])
        self.assertEqual((NEW_A, "running", 0), (report["current"]["container"], report["current"]["status"],
                                                  report["current"]["restarts"]))
        self.assertEqual((OLD, "exited"), (report["previous"]["container"], report["previous"]["status"]))
        self.assertNotIn("target", report)
        self.assertNotIn("label", report["current"])
        self.assertNotIn("label", report["previous"])

    def test_status_omits_health_for_stopped_containers(self):
        self.deploy()
        self.ops.containers[OLD]["State"]["Health"] = {"Status": "unhealthy"}
        self.ops.containers[NEW_A]["State"]["Health"] = {"Status": "healthy"}
        report = portal.status(self.cfg, self.ops)
        self.assertNotIn("health", report["previous"])
        self.assertEqual("healthy", report["current"]["health"])

    def test_status_during_guard_labels_history_and_lists_live_target(self):
        self.deploy()
        self.ops.containers[OLD]["State"]["Health"] = {"Status": "unhealthy"}
        self.ops.containers[NEW_A]["State"]["Health"] = {"Status": "unhealthy"}
        self.ops.on_reload.append(
            lambda: self.ops.containers[NEW_B]["State"].update(Health={"Status": "healthy"}))
        reports = []
        guard = portal.PortalDeployer.guard_portal

        def guarded(deployer, new):
            reports.append(portal.status(self.cfg, self.ops))
            guard(deployer, new)

        with mock.patch.object(portal.PortalDeployer, "guard_portal", new=guarded):
            self.deploy()

        report, = reports
        self.assertEqual("guard", report["in_progress"]["step"])
        self.assertEqual("current before this release", report["current"]["label"])
        self.assertEqual("previous before this release", report["previous"]["label"])
        self.assertEqual((NEW_A, "exited"), (report["current"]["container"], report["current"]["status"]))
        self.assertEqual((OLD, "exited"), (report["previous"]["container"], report["previous"]["status"]))
        self.assertEqual((NEW_B, "running", "healthy"),
                         (report["target"]["container"], report["target"]["status"], report["target"]["health"]))
        self.assertNotIn("health", report["current"])
        self.assertNotIn("health", report["previous"])
        self.assertNotIn("label", self.state()["current"])
        self.assertNotIn("label", self.state()["previous"])

    def test_status_lists_a_missing_target_without_health(self):
        target = self.cfg.view("a", "new@sha256:2")
        self.deployer()._mark(self.state(), target, "create")
        report = portal.status(self.cfg, self.ops)
        self.assertIsNone(report["previous"])
        self.assertEqual("current before this release", report["current"]["label"])
        self.assertEqual((NEW_A, "missing"), (report["target"]["container"], report["target"]["status"]))
        self.assertNotIn("health", report["target"])

    # config --------------------------------------------------------------------------
    def test_config_validation(self):
        base = json.loads(json.dumps({**make_config(self.tmp).__dict__, "mounts": []}))
        for raw, expected in (
            ({**base, "surprise": 1}, "unknown key 'surprise'"),
            ({k: v for k, v in base.items() if k != "env_file"}, "missing required key 'env_file'"),
            ({**base, "slots": {"a": 9301, "b": 9301}}, "distinct"),
            ({**base, "slots": {"a": 9301}}, "exactly a and b"),
            ({**base, "networks": []}, "at least one network"),
            ({**base, "exit_timeout": 420, "exit_abort_timeout": 60}, "exit_abort_timeout"),
            ({**base, "mounts": [{"source": "/x", "target": "/y", "rw": True}]}, "mounts[0]"),
        ):
            with self.subTest(expected):
                with self.assertRaises(deploy.DeployError) as ctx:
                    portal.PortalConfig.from_dict(raw)
                self.assertIn(expected, str(ctx.exception))
        cfg = portal.PortalConfig.from_dict({**base, "mounts": [{"source": "/x", "target": "/y"}]})
        self.assertTrue(cfg.mounts[0].readonly)
        self.assertEqual((18090, 60, 420, 3600, 60, 5, 5, "sources_portal"),
                         (cfg.container_port, cfg.ready_timeout, cfg.exit_timeout, cfg.exit_abort_timeout,
                          cfg.guard_seconds,
                          cfg.guard_interval, cfg.guard_connect_errors, cfg.upstream_name))

    def test_config_load_from_file(self):
        path = self.tmp / "portal.json"
        raw = dict(self.cfg.__dict__, mounts=[{"source": m.source, "target": m.target, "readonly": m.readonly}
                                              for m in self.cfg.mounts])
        path.write_text(json.dumps(raw))
        cfg = portal.PortalConfig.load(str(path))
        self.assertEqual(self.cfg.slots, cfg.slots)

    # CLI -----------------------------------------------------------------------------
    def test_default_config_resolution(self):
        seen = []
        with mock.patch.object(portal, "run", lambda args, path: seen.append((args.command, path)) or 0):
            self.assertEqual(0, deploy.main(["portal-status"]))
            self.assertEqual(0, deploy.main(["--config", "/tmp/x.json", "--dry-run", "portal", "img@sha256:1",
                                             "--allow-public-health-failure"]))
        self.assertEqual([("portal-status", portal.DEFAULT_CONFIG), ("portal", "/tmp/x.json")], seen)

        def refuse(path):
            seen.append(("main", path))
            raise deploy.DeployError("stop")

        with mock.patch.object(deploy.Config, "load", staticmethod(refuse)), mock.patch("sys.stderr"):
            self.assertEqual(1, deploy.main(["status"]))
            self.assertEqual(1, deploy.main(["--config", "/tmp/m.json", "rollback"]))
        self.assertEqual([("main", deploy.DEFAULT_CONFIG), ("main", "/tmp/m.json")], seen[2:])

    def test_cli_reports_portal_errors(self):
        with mock.patch("sys.stderr") as err:
            self.assertEqual(1, deploy.main(["--config", str(self.tmp / "missing.json"), "portal-status"]))
        self.assertIn("cannot read", "".join(str(c.args[0]) for c in err.write.call_args_list))

    # adopt ---------------------------------------------------------------------------
    def adopt_container(self, ops, env=None, bindings=None, running=True, mounts=None):
        ops.containers["portal-running"] = {
            "Id": "x", "RestartCount": 0, "Image": "sha256:abc",
            "State": {"Running": running, "Status": "running"},
            "Config": {"Image": "portal@sha256:1", "User": "1001:1001",
                       "Env": env if env is not None else
                       [f"PORTAL_SECRET={SECRET}", "PATH=/bin", "HOSTNAME=abc", "HOME=/root",
                        "PORTAL_LISTEN=0.0.0.0:18090", "TZ=UTC"]},
            "HostConfig": {"Memory": 2 << 30, "NanoCpus": 2_000_000_000, "PidsLimit": 512,
                           "SecurityOpt": ["no-new-privileges"], "RestartPolicy": {"Name": "unless-stopped"},
                           "PortBindings": {"18090/tcp": bindings if bindings is not None
                                            else [{"HostIp": "127.0.0.1", "HostPort": "9300"}]},
                           "LogConfig": {"Config": {"max-size": "20m", "max-file": "2"}}},
            "NetworkSettings": {"Networks": {"net-two": {}, "net-one": {}}},
            "Mounts": mounts if mounts is not None else
            [{"Type": "bind", "Source": "/srv/example/state", "Destination": "/data", "RW": True},
             {"Type": "bind", "Source": "/srv/example/registry.json", "Destination": "/config/registry.json",
              "RW": False}],
            "port": 9300,
        }

    def adopt(self, ops, **kw):
        work = self.tmp / "adopt"
        work.mkdir(exist_ok=True)
        cfg_path = str(self.tmp / "adopt-portal.json")
        args = dict(container="portal-running", slot_ports=(9301, 9302),
                    upstream_file="/etc/nginx/conf.d/example-portal-upstream.conf",
                    public_health_url=PUBLIC, work_dir=str(work))
        args.update(kw)
        portal.adopt(cfg_path, ops, **args)
        return cfg_path, work

    def test_adopt_writes_files_and_never_logs_secrets(self):
        ops = PortalFakeOps(self.tmp)
        self.adopt_container(ops)
        cfg_path, work = self.adopt(ops)
        self.assertNotIn(SECRET, "\n".join(ops.logs))
        env = (work / "portal.env").read_text()
        self.assertEqual(f"PORTAL_SECRET={SECRET}\nTZ=UTC\n", env)
        for path in (cfg_path, str(work / "portal.env"), str(work / "state.json")):
            self.assertEqual(0o600, os.stat(path).st_mode & 0o777, path)
        cfg = portal.PortalConfig.load(cfg_path)
        self.assertEqual({"a": 9301, "b": 9302}, cfg.slots)
        self.assertEqual(["net-one", "net-two"], cfg.networks)
        self.assertEqual(("1001:1001", ["no-new-privileges"], "20m", "2", 512),
                         (cfg.user, cfg.security_opts, cfg.log_max_size, cfg.log_max_file, cfg.pids_limit))
        self.assertEqual(str(2 << 30), cfg.memory)
        self.assertEqual("2", cfg.cpus)
        self.assertEqual([("/data", False), ("/config/registry.json", True)][::-1],
                         [(m.target, m.readonly) for m in cfg.mounts])
        self.assertEqual(portal.DEFAULT_LOCK_FILE, cfg.lock_file)
        self.assertEqual(str(work / "portal.env"), cfg.env_file)
        state = deploy.load_state(cfg)
        self.assertEqual({"slot": "legacy", "image": "portal@sha256:1", "container": "portal-running", "port": 9300,
                          "restart_policy": "unless-stopped", "deployed_at": None}, state["current"])
        self.assertTrue(any("http://sources_portal" in line for line in ops.logs))
        self.assertFalse(any(c[:2] in (["docker", "kill"], ["docker", "update"], ["docker", "start"])
                             or c[0] == "/usr/sbin/nginx" for c in ops.commands))

    def test_adopt_refuses(self):
        cases = {
            "already exists": lambda ops: Path(self.tmp / "adopt-portal.json").write_text("{}"),
            "is not a running container": lambda ops: self.adopt_container(ops, running=False),
            "publishes no loopback port": lambda ops: self.adopt_container(
                ops, bindings=[{"HostIp": "0.0.0.0", "HostPort": "9300"}]),
            "must not include the running Portal's port": lambda ops: self.adopt_container(
                ops, bindings=[{"HostIp": "127.0.0.1", "HostPort": "9301"}]),
            "non-bind mounts": lambda ops: self.adopt_container(
                ops, mounts=[{"Type": "volume", "Source": "v", "Destination": "/data", "RW": True}]),
        }
        for expected, setup in cases.items():
            with self.subTest(expected):
                for p in (self.tmp / "adopt-portal.json", self.tmp / "adopt" / "portal.env"):
                    p.unlink(missing_ok=True)
                ops = PortalFakeOps(self.tmp)
                self.adopt_container(ops)
                setup(ops)
                with self.assertRaises(deploy.DeployError) as ctx:
                    self.adopt(ops)
                self.assertIn(expected, str(ctx.exception))
                self.assertFalse((self.tmp / "adopt" / "portal.env").exists())
                self.assertNotIn(SECRET, str(ctx.exception))
        ops = PortalFakeOps(self.tmp)
        self.adopt_container(ops)
        (self.tmp / "adopt").mkdir(exist_ok=True)
        (self.tmp / "adopt" / "state.json").write_text("{}")
        (self.tmp / "adopt-portal.json").unlink(missing_ok=True)
        with self.assertRaises(deploy.DeployError) as ctx:
            self.adopt(ops)
        self.assertIn("state.json already exists", str(ctx.exception))

    def test_env_file_content_never_reaches_logs_or_errors(self):
        # The release passes the env file by path only; even a failing
        # start-up check reports the check's line, never the file.
        self.ops.files[self.cfg.env_file] = f"PORTAL_SECRET={SECRET}\n"
        self.ops.check_rc, self.ops.check_out = 1, "config: invalid value\n"
        with self.assertRaises(deploy.DeployError) as ctx:
            self.deploy()
        self.assertNotIn(SECRET, str(ctx.exception))
        self.assertNotIn(SECRET, "\n".join(self.ops.logs))
        self.assertNotIn(SECRET, Path(self.cfg.history_file).read_text())


if __name__ == "__main__":
    unittest.main()
