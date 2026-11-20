"""Portal release entry for deploy.py.

    deploy.py portal <image>   preflight, start-up check, create, hand off,
                               switch, guard (automatic fallback)
    deploy.py portal-rollback  return to the previous Portal container
    deploy.py portal-status    current/previous container and health
    deploy.py portal-adopt ... one-time: record the running Portal

The Portal is one container whose state directory has a single writer (a
non-blocking file lock taken at start), so the new process can only start
after the old one has exited. The tool keeps that gap short: everything that
can be checked or prepared happens before the old Portal is signalled, the
new container already exists, and Nginx is switched as soon as the new one
answers /health. It never sends SIGKILL, and Docker does not restart a
container stopped by `docker kill --signal=TERM`, so once the old Portal is
signalled the only way back is to wait for it to exit and start a container:
after exit_timeout the tool warns every 30 s and keeps waiting; only after
exit_abort_timeout does it stop and leave the rest to an operator. A current
container that is not running is not signalled (rollback after a crash).
Command timeouts and Ctrl-C between the signal and the end of the guard fall
back to the old Portal and still end the run with an error.

Settings live in /opt/sources-deploy/portal.json. Usage and failure handling:
deploy/sources/README.md. Shared pieces (Ops, errors, lock, state files, port
checks, the Nginx install sequence) come from deploy.py.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import stat
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import deploy
from deploy import DeployError, FallbackFailed, append_history, load_state, now, port_problems, release_lock, save_state

DEFAULT_CONFIG = "/opt/sources-deploy/portal.json"
DEFAULT_WORK_DIR = "/opt/sources-deploy/portal"
DEFAULT_NAME_PREFIX = "sources-portal"
# The same values Main's adopt writes into main.json; the lock file in
# particular must be the one Main uses so the two releases never overlap.
DEFAULT_NGINX_BIN = "/usr/sbin/nginx"
DEFAULT_NGINX_ERROR_LOG = "/var/log/nginx/error.log"
DEFAULT_LOCK_FILE = "/var/lock/sources-clean-release.lock"
ENTRYPOINT = "/app/sources-portal"
CONFIG_OK = "SOURCES_PORTAL_CONFIG_OK"
LEGACY = "legacy"
POLL = 0.25
NOTE_EVERY = 30
NEW_EXIT_TIMEOUT = 120
# Never copied from an adopted container into the env file: Docker sets the
# first three itself and the tool sets the listen address.
ADOPT_DROP_ENV = {"PATH", "HOSTNAME", "HOME", "PORTAL_LISTEN"}


class OldStillRunning(DeployError):
    """A signalled Portal did not exit within the abort timeout; nothing was
    killed and nothing was started."""


# --------------------------------------------------------------------------
# Configuration


REQUIRED = ("name_prefix", "slots", "networks", "memory", "cpus", "pids_limit", "log_max_size", "log_max_file",
            "env_file", "mounts", "upstream_file", "nginx_bin", "nginx_error_log", "work_dir", "state_file",
            "history_file", "lock_file", "public_health_url")


@dataclass
class PortalConfig:
    name_prefix: str
    slots: dict[str, int]
    networks: list[str]
    memory: str
    cpus: str
    pids_limit: int
    log_max_size: str
    log_max_file: str
    env_file: str
    mounts: list[deploy.Mount]
    upstream_file: str
    nginx_bin: str
    nginx_error_log: str
    work_dir: str
    state_file: str
    history_file: str
    lock_file: str
    public_health_url: str
    user: str = ""
    security_opts: list[str] = field(default_factory=list)
    upstream_name: str = "sources_portal"
    container_port: int = 18090
    ready_timeout: int = 60
    exit_timeout: int = 420
    exit_abort_timeout: int = 3600
    guard_seconds: int = 60
    guard_interval: int = 5
    guard_connect_errors: int = 5
    extra_env: dict[str, str] = field(default_factory=dict)

    @classmethod
    def load(cls, path: str) -> "PortalConfig":
        try:
            raw = json.loads(Path(path).read_text())
        except OSError as err:
            raise DeployError(f"config: cannot read {path}: {err.strerror}") from None
        except ValueError as err:
            raise DeployError(f"config: {path} is not valid JSON: {err}") from None
        return cls.from_dict(raw)

    @classmethod
    def from_dict(cls, raw: Any) -> "PortalConfig":
        if not isinstance(raw, dict):
            raise DeployError("config: expected a JSON object")
        known = set(cls.__dataclass_fields__)  # type: ignore[attr-defined]
        for key in sorted(raw):
            if key not in known:
                raise DeployError(f"config: unknown key {key!r}")
        for key in REQUIRED:
            if key not in raw:
                raise DeployError(f"config: missing required key {key!r}")
        mounts = []
        for i, m in enumerate(raw["mounts"] if isinstance(raw["mounts"], list) else [None]):
            if not isinstance(m, dict) or not {"source", "target"} <= set(m) \
                    or set(m) - {"source", "target", "readonly"}:
                raise DeployError(f"config: mounts[{i}] must be an object with source, target and optional readonly")
            mounts.append(deploy.Mount(source=str(m["source"]), target=str(m["target"]),
                                       readonly=bool(m.get("readonly", True))))
        cfg = cls(**{**raw, "mounts": mounts})
        cfg.validate()
        return cfg

    def validate(self) -> None:
        if not isinstance(self.slots, dict) or set(self.slots) != {"a", "b"}:
            raise DeployError("config: slots must be exactly a and b")
        if not all(isinstance(p, int) and not isinstance(p, bool) and 0 < p < 65536 for p in self.slots.values()):
            raise DeployError("config: slot ports must be port numbers")
        if self.slots["a"] == self.slots["b"]:
            raise DeployError("config: slot ports must be distinct")
        if not isinstance(self.networks, list) or not self.networks:
            raise DeployError("config: at least one network is required")
        if not isinstance(self.security_opts, list) or not isinstance(self.extra_env, dict):
            raise DeployError("config: security_opts must be a list and extra_env an object")
        if not (0 < self.exit_timeout <= self.exit_abort_timeout):
            raise DeployError("config: exit_abort_timeout must be at least exit_timeout")
        if not re.fullmatch(r"[A-Za-z0-9_]+", self.upstream_name):
            raise DeployError("config: upstream_name may only contain letters, digits and underscores")

    def container(self, slot: str) -> str:
        return f"{self.name_prefix}-{slot}"

    def view(self, slot: str, image: str) -> dict[str, Any]:
        return {"slot": slot, "image": image, "container": self.container(slot), "port": self.slots[slot],
                "restart_policy": "unless-stopped", "deployed_at": None}


# --------------------------------------------------------------------------
# Side effects beyond deploy.Ops (overridden in tests)


class PortalOps(deploy.Ops):
    def run(self, args: list[str], *, mutate: bool, timeout: float = 600,
            check: bool = True) -> subprocess.CompletedProcess[str]:
        """deploy.Ops.run, with a hung or unstartable command reported as a
        DeployError so the handover falls back instead of ending in a traceback."""
        try:
            return super().run(args, mutate=mutate, timeout=timeout, check=check)
        except subprocess.TimeoutExpired:
            raise DeployError(f"command timed out after {int(timeout)}s: {shlex.join(args[:4])} ...") from None
        except OSError as err:
            raise DeployError(f"command could not run: {shlex.join(args[:4])} ...: {err.strerror or err}") from None

    def path_mode(self, path: str) -> int | None:
        """Permission bits of a path, or None when it does not exist."""
        try:
            return stat.S_IMODE(os.stat(path).st_mode)
        except OSError:
            return None


def render_upstream(name: str, port: int) -> str:
    return f"""# Managed by deploy/sources/deploy.py (portal); rewritten on every switch. Do not edit.
upstream {name} {{
    server 127.0.0.1:{port};
}}
"""


def nginx_problems(dump: str, name: str) -> list[str]:
    problems = []
    if not re.search(rf"\bupstream\s+{re.escape(name)}\s*\{{", dump):
        problems.append(f"Nginx defines no 'upstream {name}' (create {name} in the upstream file; see deploy/sources/README.md)")
    if not re.search(rf"\bproxy_pass\s+http://{re.escape(name)}(?![A-Za-z0-9_.-])", dump):
        problems.append(f"no proxy_pass uses http://{name} (point the Portal location at it; see deploy/sources/README.md)")
    return problems


# --------------------------------------------------------------------------
# Release


class PortalDeployer(deploy.Deployer):
    """Reuses Deployer's logging, step timing, result parsing, connect-error
    counting and Nginx install sequence; every Portal action is its own."""

    cfg: PortalConfig  # type: ignore[assignment]

    def __init__(self, cfg: PortalConfig, ops: PortalOps):
        super().__init__(cfg, ops)  # type: ignore[arg-type]
        self.signalled_old = False

    # docker helpers -----------------------------------------------------------
    def common_args(self) -> list[str]:
        args = ["--entrypoint", ENTRYPOINT]
        if self.cfg.user:
            args += ["--user", self.cfg.user]
        for opt in self.cfg.security_opts:
            args += ["--security-opt", opt]
        args += ["--env-file", self.cfg.env_file, "-e", f"PORTAL_LISTEN=0.0.0.0:{self.cfg.container_port}"]
        for key, value in sorted(self.cfg.extra_env.items()):
            args += ["-e", f"{key}={value}"]
        return args

    def running(self, name: str) -> bool:
        return bool(((self.ops.inspect(name) or {}).get("State") or {}).get("Running"))

    def health(self, port: int) -> int:
        status, _ = self.ops.http_json(f"http://127.0.0.1:{port}/health", timeout=2)
        return status

    def check_config(self, image: str) -> str:
        """Run the image once with -check-config. Read-only: every mount is
        forced read-only, nothing is published and the container is removed."""
        name = f"{self.cfg.name_prefix}-check-{os.getpid()}-{int(self.ops.monotonic())}"
        mounts = [deploy.Mount(m.source, m.target, readonly=True) for m in self.cfg.mounts]
        args = ["docker", "create", "--name", name, "--restart", "no", "--network", self.cfg.networks[0],
                "--label", "com.sources.deploy.role=portal-check", *self.common_args(),
                *self.mount_args(mounts), image, "-check-config"]
        self.ops.run(args, mutate=False)
        try:
            for network in self.cfg.networks[1:]:
                self.ops.run(["docker", "network", "connect", network, name], mutate=False)
            proc = self.ops.run(["docker", "start", "--attach", name], mutate=False, timeout=300, check=False)
            lines = [line.strip() for line in ((proc.stdout or "") + "\n" + (proc.stderr or "")).splitlines()
                     if line.strip()]
            if proc.returncode == 0 and CONFIG_OK in lines:
                return CONFIG_OK
            last = lines[-1][:300] if lines else "(no output)"
            raise DeployError(f"start-up check of {image} failed (exit {proc.returncode}): {last}; nothing changed "
                              "(an image without -check-config fails here too)")
        finally:
            self.ops.run(["docker", "rm", "-f", name], mutate=False, check=False)

    def create(self, view: dict[str, Any]) -> None:  # type: ignore[override]
        name = view["container"]
        self.ops.run(["docker", "rm", "-f", name], mutate=True, check=False)
        args = ["docker", "create", "--name", name, "--hostname", name, "--restart", "no",
                "--memory", self.cfg.memory, "--cpus", self.cfg.cpus, "--pids-limit", str(self.cfg.pids_limit),
                "--log-opt", f"max-size={self.cfg.log_max_size}", "--log-opt", f"max-file={self.cfg.log_max_file}",
                "--publish", f"127.0.0.1:{view['port']}:{self.cfg.container_port}",
                "--network", self.cfg.networks[0], "--network-alias", name,
                "--label", f"com.sources.deploy.slot={view['slot']}",
                "--label", "com.sources.deploy.role=portal",
                "--label", f"com.sources.deploy.created={now()}",
                *self.common_args(), *self.mount_args(self.cfg.mounts), view["image"]]
        self.ops.run(args, mutate=True)
        for network in self.cfg.networks[1:]:
            self.ops.run(["docker", "network", "connect", "--alias", name, network, name], mutate=True)

    def wait_exit_portal(self, name: str, warn_after: float, abort_after: float, draining_portal: bool) -> int | None:
        """Wait for a signalled container to exit; never kill it. Notes every
        NOTE_EVERY seconds (by the clock, not by iteration); after warn_after
        they become warnings; after abort_after OldStillRunning is raised."""
        start = self.ops.monotonic()
        next_note = start + NOTE_EVERY
        while True:
            info = self.ops.inspect(name)
            if info is None:
                return None
            state = info.get("State") or {}
            if not state.get("Running"):
                return int(state.get("ExitCode", 0))
            elapsed = self.ops.monotonic() - start
            if elapsed >= abort_after:
                raise OldStillRunning(f"{name} is still running {int(elapsed)}s after SIGTERM; nothing was killed")
            if self.ops.monotonic() >= next_note:
                if elapsed >= warn_after and draining_portal:
                    self.log(f"   WARNING: {name} is still running {int(elapsed)}s after SIGTERM, longer than the "
                             f"longest expected drain ({int(warn_after)}s). The Portal's pages are unavailable. The "
                             "tool keeps waiting and starts the new container as soon as it exits; it will not "
                             f"kill anything (it gives up after {int(abort_after)}s)")
                else:
                    self.log(f"   {name} still finishing in-flight work ({int(elapsed)}s since SIGTERM) ...")
                next_note += NOTE_EVERY
            self.ops.sleep(POLL)

    def wait_healthy(self, name: str, port: int, timeout: float) -> None:
        deadline = self.ops.monotonic() + timeout
        last = 0
        while self.ops.monotonic() < deadline:
            state = (self.ops.inspect(name) or {}).get("State") or {}
            if not state.get("Running") or state.get("Restarting"):
                raise DeployError(f"{name} exited before answering /health (exit {state.get('ExitCode')}); "
                                  "see its logs")
            last = self.health(port)
            if last == 200:
                return
            self.ops.sleep(POLL)
        raise DeployError(f"{name} did not answer /health with 200 on port {port} within {int(timeout)}s "
                          f"(last status {last})")

    def wait_public_healthy(self, new: dict[str, Any]) -> None:
        """Let the reloaded workers take over before starting guard counters."""
        if self.ops.dry_run:
            self.log(f"DRY-RUN wait up to {self.cfg.ready_timeout}s for public health 200 after reload")
            return
        self.log("   waiting for public health 200 after reload before starting the guard")
        initial = self.ops.inspect(new["container"]) or {}
        started = (initial.get("State") or {}).get("StartedAt")
        restarts = initial.get("RestartCount", 0)
        deadline = self.ops.monotonic() + self.cfg.ready_timeout
        last = 0
        while (remaining := deadline - self.ops.monotonic()) > 0:
            info = self.ops.inspect(new["container"]) or {}
            state = info.get("State") or {}
            if not state.get("Running") or state.get("Restarting") or state.get("StartedAt") != started \
                    or info.get("RestartCount", 0) != restarts:
                raise DeployError("new Portal stopped or restarted while waiting for public health after reload")
            if state.get("OOMKilled"):
                raise DeployError("new Portal was OOM-killed while waiting for public health after reload")
            last, _ = self.ops.http_json(self.cfg.public_health_url, timeout=min(10, remaining))
            if last == 200:
                return
            self.ops.sleep(min(POLL, remaining))
        raise DeployError(f"public health did not return 200 after reload within {self.cfg.ready_timeout}s "
                          f"(last status {last})")

    def upstream_text(self) -> str | None:
        try:
            return self.ops.read_text(self.cfg.upstream_file)
        except OSError:
            return None

    def install(self, port: int) -> None:
        self.install_upstreams(render_upstream(self.cfg.upstream_name, port))

    # steps ------------------------------------------------------------------------
    def preflight(self, current: dict[str, Any], previous: dict[str, Any] | None,  # type: ignore[override]
                  target: dict[str, Any], allow_public_health_failure: bool = False) -> None:
        problems: list[str] = []
        if not current:
            problems.append("no current Portal release recorded; run 'portal-adopt' once first")
        else:
            info = self.ops.inspect(current["container"])
            if not info:
                problems.append(f"current container {current['container']} does not exist")
            elif not (info.get("State") or {}).get("Running"):
                problems.append(f"current container {current['container']} is not running")
            else:
                status = self.health(current["port"])
                if status != 200:
                    problems.append(f"current Portal /health on port {current['port']} returned {status}")
        dump = self.ops.run([self.cfg.nginx_bin, "-T"], mutate=False, timeout=60, check=False).stdout or ""
        problems += nginx_problems(dump, self.cfg.upstream_name)
        if current:
            if self.upstream_text() != render_upstream(self.cfg.upstream_name, current["port"]):
                problems.append(f"{self.cfg.upstream_file} is missing or differs from what the tool renders for the "
                                f"current port {current['port']}; write it once (see deploy/sources/README.md)")
            if current["container"] == target["container"]:
                problems.append(f"target container {target['container']} is the current container")
        if (previous or {}).get("slot") == LEGACY and previous.get("container") == target["container"]:
            # The create step removes the target container; never a legacy one.
            problems.append(f"target container {target['container']} is the adopted legacy container")
        info = self.ops.inspect(target["container"])
        if info and (info.get("State") or {}).get("Running"):
            problems.append(f"{target['container']} (slot {target['slot']}) is running")
        else:
            problems += port_problems(self.ops, [target["port"]])
        mode = self.ops.path_mode(self.cfg.env_file)  # type: ignore[attr-defined]
        if mode is None:
            problems.append(f"env file {self.cfg.env_file} does not exist")
        elif mode & 0o044:
            problems.append(f"env file {self.cfg.env_file} is readable by group or others (mode {mode:04o}); "
                            "chmod 600 it")
        for m in self.cfg.mounts:
            if self.ops.path_mode(m.source) is None:  # type: ignore[attr-defined]
                problems.append(f"mount source {m.source} does not exist")
        status, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
        if status != 200:
            seen = f"public health check returned status {status} (0 means no response)"
            if allow_public_health_failure:
                self.log(f"   WARNING: {seen}; continuing because of --allow-public-health-failure; "
                         "the guard still requires 200 after the switch")
            else:
                problems.append(f"{seen}; the guard after the switch would fail the same way and switch back; "
                                "pass --allow-public-health-failure to deploy anyway")
        if problems:
            raise DeployError("preflight failed, nothing changed:\n  - " + "\n  - ".join(problems))

    def stop_old(self, old: dict[str, Any], new: dict[str, Any]) -> None:
        """Signal the old Portal once and wait for it to exit. A container
        that is not running is left alone."""
        # Harmless, and keeps a daemon restart during the gap from bringing it
        # back while the new one starts.
        self.ops.run(["docker", "update", "--restart=no", old["container"]], mutate=True)
        if not self.running(old["container"]):
            self.log(f"   {old['container']} is not running; nothing to stop")
            return
        self.log(f"   TERM {old['container']}; the Portal is unavailable until the switch (no SIGKILL)")
        # Set before the signal: an interrupt while it drains must still make
        # the fallback wait for it to exit before starting it again.
        self.signalled_old = True
        self.ops.run(["docker", "kill", "--signal=TERM", old["container"]], mutate=True)
        if self.ops.dry_run:
            return
        try:
            code = self.wait_exit_portal(old["container"], self.cfg.exit_timeout, self.cfg.exit_abort_timeout,
                                         draining_portal=True)
        except OldStillRunning as err:
            self.ops.run(["docker", "update", f"--restart={old.get('restart_policy') or 'unless-stopped'}",
                          old["container"]], mutate=True, check=False)
            raise OldStillRunning(
                f"{err}. The Portal stays unavailable until that process exits, and Docker will not restart it. "
                f"Once it has exited, an operator restores service with 'docker start {old['container']}' (the "
                "unchanged previous version) and then clears in_progress. The new container "
                f"{new['container']} was created but never started; Nginx was not changed") from err
        self.log(f"   {old['container']} exited with code {code}")

    def handover(self, state: dict[str, Any], old: dict[str, Any], new: dict[str, Any]) -> None:
        """Steps 5-7: stop old, start new, switch, guard. Any failure (an
        interrupt included) after the old Portal was touched falls back to it."""
        self.signalled_old = False
        try:
            self._mark(state, new, "handoff")
            with self.step("handoff"):
                self.stop_old(old, new)
                self.ops.run(["docker", "start", new["container"]], mutate=True)
                if not self.ops.dry_run:
                    self.wait_healthy(new["container"], new["port"], self.cfg.ready_timeout)
            self._mark(state, new, "switch")
            with self.step("switch"):
                self.install(new["port"])
                self.ops.run(["docker", "update", "--restart=unless-stopped", new["container"]], mutate=True)
                self.wait_public_healthy(new)
            if self.ops.dry_run:
                self.log(f"DRY-RUN guard {self.cfg.guard_seconds}s: {new['container']} running, /health 200, "
                         "public health, Nginx connect errors")
                return
            self._mark(state, new, "guard")
            with self.step("guard"):
                self.guard_portal(new)
        except (OldStillRunning, FallbackFailed):
            raise
        except DeployError as err:
            if self.ops.dry_run:
                raise
            self.fall_back(old, new, str(err), self.signalled_old)
        except KeyboardInterrupt:
            if self.ops.dry_run:
                raise
            # No signal handlers: an interrupt is treated as a failure, the
            # previous Portal is restored, and the run still ends in an error.
            self.fall_back(old, new, "interrupted", self.signalled_old)

    def fall_back(self, old: dict[str, Any], new: dict[str, Any], cause: str, signalled: bool = True) -> None:
        """(a) stop the new Portal, which holds the state lock; (b) start the
        old one (waiting first for it to exit if it was signalled and is still
        draining); (c) point Nginx back at it if the upstream file was changed
        (a rejected file was already restored by install_upstreams)."""
        self.log(f"   {cause}; restoring {old['container']}")
        try:
            self.ops.run(["docker", "update", "--restart=no", new["container"]], mutate=True)
            if self.running(new["container"]):
                self.ops.run(["docker", "kill", "--signal=TERM", new["container"]], mutate=True)
                try:
                    self.wait_exit_portal(new["container"], NEW_EXIT_TIMEOUT, NEW_EXIT_TIMEOUT, draining_portal=False)
                except OldStillRunning as err:
                    raise DeployError(f"new container still running: {err}") from err
            if signalled and self.running(old["container"]):
                if self.health(old["port"]) == 200:
                    # The signal never arrived (the kill failed or was interrupted).
                    self.log(f"   {old['container']} is still serving; nothing to wait for")
                else:
                    # A draining Portal has closed its listener: it must exit
                    # before it can serve again.
                    self.wait_exit_portal(old["container"], self.cfg.exit_timeout, self.cfg.exit_abort_timeout,
                                          draining_portal=True)
            policy = old.get("restart_policy") or "unless-stopped"
            self.ops.run(["docker", "update", f"--restart={policy}", old["container"]], mutate=True)
            self.ops.run(["docker", "start", old["container"]], mutate=True)
            self.wait_healthy(old["container"], old["port"], self.cfg.ready_timeout)
            if self.upstream_text() != render_upstream(self.cfg.upstream_name, old["port"]):
                self.install(old["port"])
        except DeployError as err:
            raise FallbackFailed(f"{cause}; restoring the previous Portal {old['container']} failed too ({err})") \
                from err
        raise DeployError(f"{cause}; the previous Portal {old['container']} is serving again; the new container "
                          f"{new['container']} was stopped and kept for inspection")

    def guard_portal(self, new: dict[str, Any]) -> None:
        name, port = new["container"], new["port"]
        info = self.ops.inspect(name) or {}
        started = (info.get("State") or {}).get("StartedAt")
        restarts = info.get("RestartCount", 0)
        offset = self.ops.file_size(self.cfg.nginx_error_log)
        public_failures = 0
        deadline = self.ops.monotonic() + self.cfg.guard_seconds
        while True:
            reason = None
            info = self.ops.inspect(name) or {}
            state = info.get("State") or {}
            if not state.get("Running") or state.get("StartedAt") != started or info.get("RestartCount", 0) != restarts:
                reason = "new Portal stopped or restarted"
            elif state.get("OOMKilled"):
                reason = "new Portal was OOM-killed"
            if reason is None:
                status = self.health(port)
                if status != 200:
                    reason = f"new Portal /health returned {status}"
            if reason is None:
                public_status, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
                public_failures = public_failures + 1 if public_status != 200 else 0
                if public_failures >= 3:
                    reason = f"public health failed 3 times in a row (last status {public_status})"
            if reason is None:
                errors = self.connect_errors(self.ops.read_from(self.cfg.nginx_error_log, offset), {str(port)})
                if errors >= self.cfg.guard_connect_errors:
                    reason = f"{errors} Nginx connect failures to port {port}"
            if reason:
                raise DeployError(f"guard failed ({reason})")
            if self.ops.monotonic() >= deadline:
                return
            self.ops.sleep(self.cfg.guard_interval)

    # entrypoints ------------------------------------------------------------------
    def deploy_portal(self, image: str, allow_public_health_failure: bool = False) -> None:
        state = load_state(self.cfg)
        if state.get("in_progress"):
            raise DeployError(f"a previous Portal release did not finish: {json.dumps(state['in_progress'])}; "
                              "inspect with 'portal-status' and resolve before deploying again")
        current = state.get("current") or {}
        slot = "b" if current.get("slot") == "a" else "a"
        target = self.cfg.view(slot, image)
        record: dict[str, Any] = {"started_at": now(), "action": "portal", "image": image, "slot": slot}
        try:
            with self.step("pull image"):
                present = self.ops.run(["docker", "image", "inspect", image], mutate=False, check=False, timeout=30)
                if present.returncode != 0:
                    self.ops.run(["docker", "pull", image], mutate=False, timeout=1800)
            with self.step("preflight"):
                self.preflight(current, state.get("previous"), target, allow_public_health_failure)
            with self.step("startup check"):
                self.log("   " + self.check_config(image))
            self._mark(state, target, "create")
            with self.step("create"):
                self.create(target)
            self.handover(state, current, target)
            self._finish(state, current, target, record)
        except DeployError as err:
            self._fail(state, target, record, err, remove_target=True)
            raise

    def rollback_portal(self) -> None:
        state = load_state(self.cfg)
        if state.get("in_progress"):
            raise DeployError(f"a previous Portal release did not finish: {json.dumps(state['in_progress'])}; "
                              "inspect with 'portal-status' and resolve before rolling back")
        current, previous = state.get("current"), state.get("previous")
        if not current or not previous:
            raise DeployError("no previous Portal release recorded; nothing to roll back to")
        problems = []
        info = self.ops.inspect(previous["container"])
        if not info:
            problems.append(f"previous container {previous['container']} no longer exists")
        else:
            if (info.get("State") or {}).get("Running"):
                problems.append(f"previous container {previous['container']} is already running")
            # The image the container was created from (by ID), which is what
            # `docker start` needs; a tag may have moved since.
            image_ref = info.get("Image") or previous.get("image")
            if self.ops.run(["docker", "image", "inspect", image_ref], mutate=False, check=False,
                            timeout=30).returncode != 0:
                problems.append(f"previous image {previous.get('image')} is no longer present")
        if not self.ops.inspect(current["container"]):
            problems.append(f"current container {current['container']} does not exist")
        if problems:
            raise DeployError("rollback refused, nothing changed:\n  - " + "\n  - ".join(problems))
        target = dict(previous)
        record: dict[str, Any] = {"started_at": now(), "action": "portal-rollback", "image": target.get("image"),
                                  "slot": target["slot"]}
        try:
            self.handover(state, current, target)
            self._finish(state, current, target, record)
        except DeployError as err:
            self._fail(state, target, record, err, remove_target=False)
            raise

    def _finish(self, state: dict[str, Any], old: dict[str, Any], new: dict[str, Any],
                record: dict[str, Any]) -> None:
        if self.ops.dry_run:
            self.log(f"DRY-RUN record current={new['container']} previous={old.get('container')} (no state or history "
                     "written in a dry run)")
            return
        view = {**new, "restart_policy": "unless-stopped", "deployed_at": now()}
        state.update(previous=old or None, current=view, in_progress=None)
        save_state(self.cfg, state)
        record.update(result="ok", steps=self.timings, finished_at=now())
        append_history(self.cfg, record)
        self.log(f"done: {new['container']} serving {new.get('image')} on port {new['port']}")

    def _mark(self, state: dict[str, Any], target: Any, step: str) -> None:
        state["in_progress"] = {"target": target, "step": step, "since": now()}
        if not self.ops.dry_run:
            save_state(self.cfg, state)

    def _fail(self, state: dict[str, Any], target: dict[str, Any], record: dict[str, Any], err: DeployError,
              remove_target: bool) -> None:
        record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
        if self.ops.dry_run:
            self.log(f"FAILED: {err}")
            return
        step = (state.get("in_progress") or {}).get("step")
        if isinstance(err, (FallbackFailed, OldStillRunning)):
            # Keep in_progress so the next run refuses until a person resolves it.
            label = "FAILED AND NOT RESTORED" if isinstance(err, FallbackFailed) else "FAILED, NEEDS AN OPERATOR"
            self.log(f"{label}: {err}")
        else:
            if remove_target and step == "create":
                # Created but never started: the old Portal was not touched.
                self.ops.run(["docker", "rm", "-f", target["container"]], mutate=True, check=False)
            if step is not None:
                state["in_progress"] = None
                save_state(self.cfg, state)
            self.log(f"FAILED: {err}")
        append_history(self.cfg, record)


# --------------------------------------------------------------------------
# Status and adopt


def status(cfg: PortalConfig, ops: deploy.Ops) -> dict[str, Any]:
    state = load_state(cfg)
    in_progress = state.get("in_progress")
    report: dict[str, Any] = {"in_progress": in_progress}

    def describe(view: dict[str, Any] | None) -> dict[str, Any] | None:
        if not view:
            return None
        info = ops.inspect(view["container"])
        s = (info or {}).get("State") or {}
        entry = {**view, "status": s.get("Status", "missing"),
                 "restarts": (info or {}).get("RestartCount")}
        if s.get("Running"):
            entry["health"] = (s.get("Health") or {}).get("Status")
        return entry

    for key in ("current", "previous"):
        report[key] = describe(state.get(key))
        if in_progress and report[key] is not None:
            report[key]["label"] = f"{key} before this release"
    if in_progress:
        report["target"] = describe(in_progress.get("target"))
    return report


def published_loopback_port(info: dict[str, Any], container_port: int) -> int | None:
    bindings = ((info.get("HostConfig") or {}).get("PortBindings") or {}).get(f"{container_port}/tcp") or []
    for b in bindings:
        if b.get("HostIp") == "127.0.0.1" and str(b.get("HostPort", "")).isdigit():
            return int(b["HostPort"])
    return None


def adopt(cfg_path: str, ops: PortalOps, container: str, slot_ports: tuple[int, int], upstream_file: str,
          public_health_url: str, work_dir: str = DEFAULT_WORK_DIR, name_prefix: str = DEFAULT_NAME_PREFIX,
          nginx_bin: str = DEFAULT_NGINX_BIN, nginx_error_log: str = DEFAULT_NGINX_ERROR_LOG,
          lock_file: str = DEFAULT_LOCK_FILE) -> None:
    """Record the running Portal as the current (legacy) release and derive
    portal.json from it. Changes nothing that is running and never prints an
    environment value."""
    state_file = str(Path(work_dir) / "state.json")
    for path in (cfg_path, state_file):
        if ops.path_mode(path) is not None:
            raise DeployError(f"{path} already exists; portal-adopt runs once")
    if slot_ports[0] == slot_ports[1]:
        raise DeployError("--slot-ports must be two different ports")
    info = ops.inspect(container)
    if not info or not (info.get("State") or {}).get("Running"):
        raise DeployError(f"{container} is not a running container")
    env = info.get("Config", {}).get("Env") or []
    listen = next((e.split("=", 1)[1] for e in env if e.startswith("PORTAL_LISTEN=")), "")
    inner = int(listen.rsplit(":", 1)[1]) if listen.rsplit(":", 1)[-1].isdigit() else 18090
    port = published_loopback_port(info, inner)
    if port is None:
        raise DeployError(f"{container} publishes no loopback port (127.0.0.1) for {inner}/tcp")
    if port in slot_ports:
        raise DeployError(f"--slot-ports must not include the running Portal's port {port}")
    unsupported = [m.get("Destination") for m in info.get("Mounts") or [] if m.get("Type", "bind") != "bind"]
    if unsupported:
        raise DeployError(f"{container} has non-bind mounts {unsupported}; only bind mounts can be carried over")

    kept = [e for e in env if e.split("=", 1)[0] not in ADOPT_DROP_ENV]
    env_path = str(Path(work_dir) / "portal.env")
    host = info.get("HostConfig") or {}
    log_cfg = (host.get("LogConfig") or {}).get("Config") or {}
    notes = []
    memory = host.get("Memory") or 0
    if not memory:
        notes.append("memory is not limited on the running container; wrote 2g")
    cpus = (host.get("NanoCpus") or 0) / 1e9
    if not cpus:
        notes.append("cpus are not limited on the running container; wrote 2")
    pids = host.get("PidsLimit") or 0
    if not pids or pids < 0:
        notes.append("pids are not limited on the running container; wrote 1024")
    config = {
        "name_prefix": name_prefix,
        "slots": {"a": slot_ports[0], "b": slot_ports[1]},
        "networks": sorted((info.get("NetworkSettings") or {}).get("Networks", {}).keys()),
        "user": info.get("Config", {}).get("User") or "",
        "memory": str(memory) if memory else "2g",
        "cpus": f"{cpus:g}" if cpus else "2",
        "pids_limit": pids if pids and pids > 0 else 1024,
        "security_opts": list(host.get("SecurityOpt") or []),
        "log_max_size": log_cfg.get("max-size", "10m"),
        "log_max_file": log_cfg.get("max-file", "3"),
        "env_file": env_path,
        "mounts": sorted(({"source": m["Source"], "target": m["Destination"], "readonly": not m.get("RW", False)}
                          for m in info.get("Mounts") or []), key=lambda m: m["target"]),
        "upstream_file": upstream_file,
        "nginx_bin": nginx_bin,
        "nginx_error_log": nginx_error_log,
        "work_dir": work_dir,
        "state_file": state_file,
        "history_file": str(Path(work_dir) / "history.jsonl"),
        "lock_file": lock_file,
        "public_health_url": public_health_url,
    }
    cfg = PortalConfig.from_dict(config)  # validate before writing anything
    ops.write_file_atomic(env_path, "\n".join(sorted(kept)) + "\n", mode=0o600)
    ops.write_file_atomic(cfg_path, json.dumps(config, indent=2) + "\n", mode=0o600)
    legacy = {"slot": LEGACY, "image": info.get("Config", {}).get("Image"), "container": container, "port": port,
              "restart_policy": (host.get("RestartPolicy") or {}).get("Name") or "no", "deployed_at": None}
    if not ops.dry_run:
        save_state(cfg, {"current": legacy, "previous": None, "in_progress": None})
    for line in [f"env file {env_path} ({len(kept)} variables, values not shown)"] + notes:
        ops.log("portal-adopt: " + line)
    ops.log(f"portal-adopt: wrote {cfg_path} and recorded {container} (port {port}) as the current (legacy) release")
    ops.log(f"portal-adopt: before the first 'portal' run, write {upstream_file} with 'upstream {cfg.upstream_name}' "
            f"pointing at 127.0.0.1:{port} exactly as the tool renders it, point the Portal's Nginx location at "
            f"http://{cfg.upstream_name}, then nginx -t and reload; the preflight checks exactly that")


# --------------------------------------------------------------------------
# CLI (parsers are defined in deploy.py)


def parse_slot_ports(text: str) -> tuple[int, int]:
    try:
        a, b = (int(p) for p in text.split(","))
    except ValueError:
        raise DeployError("--slot-ports must be two ports: A,B") from None
    return a, b


def run(args: argparse.Namespace, config_path: str) -> int:
    ops = PortalOps(dry_run=args.dry_run)
    if args.command == "portal-adopt":
        if not args.dry_run:
            Path(args.work_dir).mkdir(parents=True, exist_ok=True, mode=0o700)
        adopt(config_path, ops, args.container, parse_slot_ports(args.slot_ports), args.upstream_file,
              args.public_health_url, work_dir=args.work_dir, name_prefix=args.name_prefix, nginx_bin=args.nginx_bin,
              nginx_error_log=args.nginx_error_log, lock_file=args.lock_file)
        return 0
    cfg = PortalConfig.load(config_path)
    if args.command == "portal-status":
        print(json.dumps(status(cfg, ops), indent=2))
        return 0
    with release_lock(cfg.lock_file):
        deployer = PortalDeployer(cfg, ops)
        if args.command == "portal":
            deployer.deploy_portal(args.image, allow_public_health_failure=args.allow_public_health_failure)
        elif args.command == "portal-rollback":
            deployer.rollback_portal()
    return 0
