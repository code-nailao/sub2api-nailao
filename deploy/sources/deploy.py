#!/usr/bin/env python3
"""Fixed release entrypoint for Sources Main.

One command replaces Main serving and the unique background with one image:

    deploy.py main <image>     preflight, migrate, check, start, hand off,
                               switch, guard (auto-rollback), retire
    deploy.py rollback         return to the previous slot's containers
    deploy.py status           current/previous slot and container health
    deploy.py watch            resident sampler for systemd (journal only)
    deploy.py adopt ...        one-time: record running legacy containers
    deploy.py portal ...       the Portal (portal.py): portal <image>,
                               portal-rollback, portal-status, portal-adopt

Usage: deploy/sources/README.md. Two fixed slots (a, b)
with fixed container names and ports alternate; Nginx references only the
fixed upstreams sources_main_serving and sources_main_admin, rendered into
one file. The background ownership lock (flock) guarantees a single owner;
this script only orders the handoff and never sends SIGKILL.

Settings live on the server in /opt/sources-deploy/main.json, secrets in
root-only env files referenced by it. Nothing here is release-specific; if
a step fails the script leaves the old release serving and stops.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import http.client
import json
import os
import re
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterator, NoReturn

DEFAULT_CONFIG = "/opt/sources-deploy/main.json"
ROLES = ("serving", "standby", "background")
SERVING_ROLES = ("serving", "standby")
PENDING_PREFIX = "SOURCES_PENDING_MIGRATIONS"
CHECK_PREFIX = "SOURCES_STARTUP_CHECK_OK"
ASSETS_PREFIX = "SOURCES_WEB_ASSETS_EXPORTED"
GIB = 1 << 30
# Sent on every request. Some edge protection in front of a public endpoint
# rejects the default urllib client identity, which would fail the public
# health check for reasons unrelated to the release.
USER_AGENT = "sources-deploy"
# Poll interval inside the background handoff. The admin API waits on it twice
# (old owner exited, new owner ready); the checks are local and cheap.
HANDOFF_POLL_SECONDS = 0.5
# First progress note after TERM, then one per minute while the old owner drains.
HANDOFF_FIRST_NOTE_SECONDS = 5
HANDOFF_TERM_CONFIRM_SECONDS = 30
HANDOFF_RECOVERY_SECONDS = 30
# Reserve time to record failure and restore the policy if exit is still unknown.
HANDOFF_RECOVERY_CLEANUP_SECONDS = 5
DOCKER_STATUS_TIMEOUT_SECONDS = 30
RUNTIME_STATUS_TIMEOUT_SECONDS = 5
# A backup can still be cleaning up an interrupted upload at its own deadline.
BACKUP_FINISH_GRACE_SECONDS = 180
BACKUP_STATUS_POLL_SECONDS = 10


class DeployError(Exception):
    """A step failed; the previous release serves and owns the background."""

    def __init__(self, *args: Any, cleanup_deadline: float | None = None) -> None:
        super().__init__(*args)
        self.cleanup_deadline = cleanup_deadline


class FallbackFailed(DeployError):
    """Automatic recovery could not finish; needs a person."""


class ServingCleanupFailed(FallbackFailed):
    """Traffic and background are back, but serving cleanup is incomplete."""


class BackgroundRestored(DeployError):
    """The handoff failed, and the original background is ready again."""


class BackgroundExitTimeout(DeployError):
    def __init__(self, name: str, deadline: float):
        super().__init__(f"could not confirm exit of {name} before the deadline")
        self.deadline = deadline


@dataclass(frozen=True)
class BackgroundExit:
    code: int | None  # None only when Docker confirmed container removal
    timed_out_at: float | None = None


@dataclass(frozen=True)
class BackgroundRecovery:
    deadline: float
    failure_recorded: bool = False

    @property
    def work_until(self) -> float:
        # Once the failure is durable, a late exit can use the remaining time.
        if self.failure_recorded:
            return self.deadline
        return self.deadline - HANDOFF_RECOVERY_CLEANUP_SECONDS


# --------------------------------------------------------------------------
# Configuration and state


@dataclass
class Mount:
    source: str
    target: str
    readonly: bool = True
    # copy: per-container copy of a template directory (e.g. pricing cache)
    per_container_copy: bool = False

    @classmethod
    def parse(cls, raw: dict[str, Any]) -> "Mount":
        return cls(source=raw["source"], target=raw["target"],
                   readonly=bool(raw.get("readonly", True)),
                   per_container_copy=bool(raw.get("per_container_copy", False)))


@dataclass
class Config:
    slots: dict[str, dict[str, int]]
    name_prefix: str
    networks: list[str]
    user: str
    memory: str
    cpus: str
    pids_limit: int
    log_max_size: str
    log_max_file: str
    env_files: dict[str, str]
    mounts: dict[str, list[Mount]]
    runtime_token_file: str
    upstream_file: str
    nginx_bin: str
    nginx_error_log: str
    assets_dir: str
    work_dir: str
    state_file: str
    history_file: str
    lock_file: str
    public_health_url: str
    cgroup_memory_path: str
    # Stable DNS name of the current release's serving containers on every
    # network. Internal clients (Portal) use it instead of a container IP, so
    # Main releases never leave them pinned to a retired container.
    serving_alias: str = "sources-main-serving"
    min_host_available_gib: float = 8.0
    ready_timeout: int = 120
    background_ready_timeout: int = 60
    background_exit_timeout: int = 2100
    guard_seconds: int = 300
    guard_interval: int = 10
    guard_memory_fraction: float = 0.7
    guard_memory_jump_gib: float = 4.0
    guard_connect_errors: int = 10
    watch_active_http: int = 1200
    watch_memory_gib: float = 12.0
    extra_env: dict[str, dict[str, str]] = field(default_factory=dict)

    @classmethod
    def load(cls, path: str) -> "Config":
        raw = json.loads(Path(path).read_text())
        mounts = {role: [Mount.parse(m) for m in raw.get("mounts", {}).get(role, [])]
                  for role in ("common", "serving", "background")}
        known = {f for f in cls.__dataclass_fields__}  # type: ignore[attr-defined]
        values = {k: v for k, v in raw.items() if k in known and k != "mounts"}
        cfg = cls(mounts=mounts, **values)
        cfg.validate()
        return cfg

    def validate(self) -> None:
        if set(self.slots) != {"a", "b"}:
            raise DeployError("config: slots must be exactly a and b")
        ports = [p for slot in self.slots.values() for p in (slot[r] for r in ROLES)]
        if len(set(ports)) != len(ports):
            raise DeployError("config: slot ports must be distinct")
        if not self.networks:
            raise DeployError("config: at least one network is required")
        for role in ("serving", "background"):
            if role not in self.env_files:
                raise DeployError(f"config: env file for {role} is required")

    def container(self, slot: str, role: str) -> str:
        return f"{self.name_prefix}-{slot}-{role}"

    def slot_view(self, slot: str) -> dict[str, Any]:
        return {"slot": slot,
                "containers": {r: self.container(slot, r) for r in ROLES},
                "ports": {r: self.slots[slot][r] for r in ROLES}}


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")


def load_state(cfg: Config) -> dict[str, Any]:
    path = Path(cfg.state_file)
    if not path.exists():
        return {"current": None, "previous": None, "in_progress": None}
    return json.loads(path.read_text())


def save_state(cfg: Config, state: dict[str, Any]) -> None:
    path = Path(cfg.state_file)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(state, indent=2, sort_keys=True) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def append_history(cfg: Config, record: dict[str, Any]) -> None:
    path = Path(cfg.history_file)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as fh:
        fh.write(json.dumps(record, sort_keys=True) + "\n")


@contextlib.contextmanager
def release_lock(path: str) -> Iterator[None]:
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DeployError(f"another release holds {path}; nothing changed") from None
        yield
    finally:
        os.close(fd)


@contextlib.contextmanager
def finish_handoff_cleanup(*, ignore: bool = False) -> Iterator[Callable[[], bool]]:
    """Record Ctrl-C without interrupting an ownership change or its cleanup."""
    interrupted = False

    def request_interrupt(_signum: int, _frame: Any) -> None:
        nonlocal interrupted
        interrupted = True

    def interrupt_requested() -> bool:
        return interrupted

    if threading.current_thread() is not threading.main_thread():
        yield interrupt_requested
        return
    previous = signal.signal(signal.SIGINT, signal.SIG_IGN if ignore else request_interrupt)
    try:
        yield interrupt_requested
    finally:
        signal.signal(signal.SIGINT, previous)


# --------------------------------------------------------------------------
# Side effects (overridden in tests)


class Ops:
    """Every command and request goes through here. In dry-run mode
    mutating commands are printed instead of executed."""

    def __init__(self, dry_run: bool = False, log: Callable[[str], None] | None = None):
        self.dry_run = dry_run
        self.log = log or (lambda msg: print(msg, flush=True))

    # commands ---------------------------------------------------------------
    def run(self, args: list[str], *, mutate: bool, timeout: float = 600,
            check: bool = True) -> subprocess.CompletedProcess[str]:
        if self.dry_run and mutate:
            self.log("DRY-RUN " + shlex.join(args))
            return subprocess.CompletedProcess(args, 0, "", "")
        proc = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
        if check and proc.returncode != 0:
            tail = (proc.stderr or proc.stdout).strip().splitlines()[-5:]
            raise DeployError(f"command failed ({proc.returncode}): {shlex.join(args[:4])} ...: " + " | ".join(tail))
        return proc

    def inspect(self, name: str, *, timeout: float = DOCKER_STATUS_TIMEOUT_SECONDS) -> dict[str, Any] | None:
        proc = self.run(["docker", "inspect", name], mutate=False, check=False, timeout=timeout)
        if proc.returncode != 0:
            return None
        data = json.loads(proc.stdout)
        return data[0] if data else None

    def container_missing(self, name: str, *, timeout: float = DOCKER_STATUS_TIMEOUT_SECONDS) -> bool:
        # Inspect can fail while Docker is unavailable. Only a successful
        # listing that omits the exact name confirms removal.
        proc = self.run(["docker", "container", "ls", "--all", "--format", "{{.Names}}"],
                        mutate=False, check=False, timeout=timeout)
        return proc.returncode == 0 and name not in proc.stdout.splitlines()

    # http ---------------------------------------------------------------------
    def http_json(self, url: str, token: str | None = None, method: str = "GET",
                  body: dict[str, Any] | None = None, timeout: float = 5) -> tuple[int, Any]:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("User-Agent", USER_AGENT)
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - fixed loopback/config URLs
                raw = resp.read()
                return resp.status, json.loads(raw) if raw.strip().startswith(b"{") else raw.decode(errors="replace")
        except urllib.error.HTTPError as err:
            return err.code, None
        except (urllib.error.URLError, TimeoutError, ConnectionError, OSError,
                http.client.HTTPException, ValueError):
            # A process that exits mid-response leaves a bad status line, a
            # short read or truncated JSON: no usable answer, same as no answer.
            return 0, None

    # filesystem / time ------------------------------------------------------------
    def read_text(self, path: str) -> str:
        return Path(path).read_text()

    def write_file_atomic(self, path: str, text: str, mode: int = 0o644) -> None:
        if self.dry_run:
            self.log(f"DRY-RUN write {path} ({len(text)} bytes)")
            return
        target = Path(path)
        fd, tmp = tempfile.mkstemp(dir=target.parent, prefix=".deploy-")
        with os.fdopen(fd, "w") as fh:
            fh.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, target)

    def meminfo_available(self) -> int:
        for line in Path("/proc/meminfo").read_text().splitlines():
            if line.startswith("MemAvailable:"):
                return int(line.split()[1]) * 1024
        return 0

    def file_size(self, path: str) -> int:
        try:
            return os.path.getsize(path)
        except OSError:
            return 0

    def read_from(self, path: str, offset: int) -> str:
        try:
            with open(path, "rb") as fh:
                fh.seek(offset)
                return fh.read().decode(errors="replace")
        except OSError:
            return ""

    def port_free(self, port: int) -> bool:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            try:
                sock.bind(("127.0.0.1", port))
            except OSError:
                return False
        return True

    def sleep(self, seconds: float) -> None:
        time.sleep(seconds)

    def monotonic(self) -> float:
        return time.monotonic()


# --------------------------------------------------------------------------
# Nginx


def render_upstreams(serving_port: int, standby_port: int, background_port: int) -> str:
    return f"""# Managed by deploy/sources/deploy.py; rewritten on every switch. Do not edit.
upstream sources_main_serving {{
    zone sources_main_serving 64k;
    server 127.0.0.1:{serving_port} max_fails=1 fail_timeout=5s;
    server 127.0.0.1:{standby_port} backup max_fails=1 fail_timeout=5s;
}}

upstream sources_main_admin {{
    zone sources_main_admin 64k;
    server 127.0.0.1:{background_port};
}}
"""


LEGACY_UPSTREAM = re.compile(r"sources_main_\d{3}_serving")


def nginx_ready_for_fixed_upstreams(nginx_dump: str) -> list[str]:
    """Problems that block switching via the fixed upstream file."""
    problems = []
    if "http://sources_main_serving" not in nginx_dump:
        problems.append("no proxy_pass uses http://sources_main_serving (point Nginx at the fixed upstream names; see deploy/sources/README.md)")
    if "http://sources_main_admin" not in nginx_dump:
        problems.append("admin routing does not use http://sources_main_admin (point Nginx at the fixed upstream names; see deploy/sources/README.md)")
    legacy = sorted(set(LEGACY_UPSTREAM.findall(nginx_dump)))
    if legacy:
        problems.append("version-named upstreams still referenced: " + ", ".join(legacy))
    return problems


def parse_port_list(text: str) -> set[int]:
    ports: set[int] = set()
    for part in text.replace("\n", ",").split(","):
        part = part.strip()
        if not part:
            continue
        lo, _, hi = part.partition("-")
        ports.update(range(int(lo), int(hi or lo) + 1))
    return ports


def port_problems(ops: "Ops", ports: list[int]) -> list[str]:
    """Slot ports must be free, and reserved when inside the ephemeral range:
    otherwise any outgoing connection on the host can briefly occupy one and
    make `docker start` fail with 'address already in use'."""
    problems = []
    try:
        lo, hi = (int(x) for x in ops.read_text("/proc/sys/net/ipv4/ip_local_port_range").split())
        reserved = parse_port_list(ops.read_text("/proc/sys/net/ipv4/ip_local_reserved_ports"))
    except (OSError, ValueError):
        lo, hi, reserved = 1, 0, set()
    unreserved = [p for p in ports if lo <= p <= hi and p not in reserved]
    if unreserved:
        problems.append(f"ports {unreserved} are in the ephemeral range {lo}-{hi} but not in "
                        "net.ipv4.ip_local_reserved_ports; reserve them once (sysctl.d)")
    busy = [p for p in ports if not ops.port_free(p)]
    if busy:
        problems.append(f"ports {busy} are already in use")
    return problems


# --------------------------------------------------------------------------
# Deployment


@dataclass
class Target:
    slot: str
    image: str
    containers: dict[str, str]
    ports: dict[str, int]
    token_files: dict[str, str] = field(default_factory=dict)

    def view(self) -> dict[str, Any]:
        return {"slot": self.slot, "image": self.image, "containers": self.containers, "ports": self.ports,
                "runtime_token_files": self.token_files}

    @classmethod
    def from_view(cls, view: dict[str, Any], default_token: str) -> "Target":
        tokens = {role: (view.get("runtime_token_files") or {}).get(role) or default_token for role in ROLES}
        return cls(slot=view["slot"], image=view.get("image", ""), containers=dict(view["containers"]),
                   ports=dict(view["ports"]), token_files=tokens)


class Deployer:
    def __init__(self, cfg: Config, ops: Ops):
        self.cfg = cfg
        self.ops = ops
        self.timings: list[dict[str, Any]] = []

    # helpers ---------------------------------------------------------------
    def log(self, msg: str) -> None:
        self.ops.log(f"[{dt.datetime.now().strftime('%H:%M:%S')}] {msg}")

    @contextlib.contextmanager
    def step(self, name: str) -> Iterator[None]:
        start = self.ops.monotonic()
        self.log(f"== {name}")
        try:
            yield
        finally:
            self.timings.append({"step": name, "seconds": round(self.ops.monotonic() - start, 1)})

    def token_for(self, view: dict[str, Any], role: str) -> str:
        path = (view.get("runtime_token_files") or {}).get(role) or self.cfg.runtime_token_file
        return self.ops.read_text(path).strip()

    def runtime(self, port: int, token: str, *, timeout: float = RUNTIME_STATUS_TIMEOUT_SECONDS) -> dict[str, Any] | None:
        status, body = self.ops.http_json(f"http://127.0.0.1:{port}/internal/sources/runtime", token=token, timeout=timeout)
        return body if status == 200 and isinstance(body, dict) else None

    def recovery_timeout(self, deadline: float | None, maximum: float) -> float:
        if deadline is None:
            return maximum
        remaining = deadline - self.ops.monotonic()
        if remaining <= 0:
            raise DeployError("background recovery deadline reached")
        return min(maximum, remaining)

    def wait_ready(self, port: int, token: str, role: str, timeout: int,
                   container: str | None = None, poll: float = 2, *, deadline: float | None = None) -> dict[str, Any]:
        """Wait for a role to report ready. During recovery, share its deadline."""
        ready_until = self.ops.monotonic() + timeout
        if deadline is not None:
            ready_until = min(ready_until, deadline)
        last: dict[str, Any] | None = None
        while self.ops.monotonic() < ready_until:
            if container and not self.ops.dry_run:
                inspect_timeout = self.recovery_timeout(ready_until if deadline is not None else None,
                                                        DOCKER_STATUS_TIMEOUT_SECONDS)
                info = self.ops.inspect(container, timeout=inspect_timeout) or {}
                state = info.get("State", {})
                if not state.get("Running") or state.get("Restarting"):
                    raise DeployError(f"{container} exited or is restarting before becoming ready "
                                      f"(exit {state.get('ExitCode')}); see its logs")
            if deadline is not None:
                last = self.runtime_request_before(lambda limit: self.runtime(port, token, timeout=limit), ready_until)
            else:
                last = self.runtime(port, token)
            if last and last.get("ready") and last.get("role") == role:
                if role != "background" or last.get("owns_background"):
                    return last
            delay = poll if deadline is None else min(poll, max(0, ready_until - self.ops.monotonic()))
            self.ops.sleep(delay)
        limit = f"within {timeout}s" if deadline is None else "before the recovery deadline"
        raise DeployError(f"port {port} not ready as {role} {limit} (last runtime: {json.dumps(last)[:300]})")

    def background_container(self, name: str, *, deadline: float | None = None) -> tuple[dict[str, Any] | None, bool]:
        """Return Docker's inspection and whether Docker confirmed removal."""
        timeout = DOCKER_STATUS_TIMEOUT_SECONDS
        if deadline is not None:
            timeout = min(timeout, deadline - self.ops.monotonic())
            if timeout <= 0:
                return None, False
        with contextlib.suppress(OSError, ValueError, subprocess.SubprocessError):
            info = self.ops.inspect(name, timeout=timeout)
            if info is not None:
                return info, False
        if deadline is not None:
            timeout = min(DOCKER_STATUS_TIMEOUT_SECONDS, deadline - self.ops.monotonic())
            if timeout <= 0:
                return None, False
        with contextlib.suppress(OSError, ValueError, subprocess.SubprocessError):
            return None, self.ops.container_missing(name, timeout=timeout)
        return None, False

    def wait_exit(self, name: str, deadline: float, poll: float = 2, first_note: float = 60,
                  read_runtime: Callable[[float], dict[str, Any] | None] | None = None,
                  initial_runtime: dict[str, Any] | None = None, allow_backup_wait: bool = True) -> BackgroundExit:
        """Wait for a verified exit, distinguishing a timely handoff from recovery."""
        started = self.ops.monotonic()
        next_note = started + first_note
        next_probe = next_note
        cutoff_probed = False
        logged_extension = False
        retry_delay = 1
        runtime = initial_runtime
        remaining = self.scheduled_backup_remaining(runtime) if allow_backup_wait else None
        backup_until = None if remaining is None else started + remaining + BACKUP_FINISH_GRACE_SECONDS
        backup_running = remaining is not None

        def forward_deadline() -> float:
            return max(deadline, backup_until or deadline)

        def probe_until() -> float:
            if cutoff_probed and runtime is None:
                # Retry an unknown backup status in the existing recovery
                # window. An exit during this retry is not a timely handoff.
                retry_until = deadline + HANDOFF_RECOVERY_SECONDS - HANDOFF_RECOVERY_CLEANUP_SECONDS
                return max(forward_deadline(), retry_until)
            return forward_deadline()

        while True:
            probe_deadline = probe_until()
            info, removed = self.background_container(name, deadline=probe_deadline)
            seen_at = self.ops.monotonic()
            if seen_at < probe_deadline:
                cutoff = forward_deadline()
                timed_out_at = cutoff if seen_at >= cutoff else None
                if removed:
                    return BackgroundExit(None, timed_out_at)
                state = (info or {}).get("State") or {}
                if state.get("Running") is False and not state.get("Restarting"):
                    return BackgroundExit(int(state.get("ExitCode", 0)), timed_out_at)

            # Recheck at the ordinary cutoff in case a backup started after
            # preflight. This read has a short budget even at the cutoff.
            at_backup_cutoff = allow_backup_wait and seen_at >= deadline and not cutoff_probed
            if read_runtime and (at_backup_cutoff or (seen_at < probe_deadline and seen_at >= next_probe)):
                cutoff_probed = cutoff_probed or at_backup_cutoff
                if at_backup_cutoff:
                    probe_deadline = max(probe_deadline, deadline + RUNTIME_STATUS_TIMEOUT_SECONDS)
                runtime_deadline = min(probe_deadline, seen_at + RUNTIME_STATUS_TIMEOUT_SECONDS)
                runtime = read_runtime(runtime_deadline)
                seen_at = self.ops.monotonic()
                remaining = self.scheduled_backup_remaining(runtime)
                if (allow_backup_wait and runtime is not None and remaining is None
                        and "scheduled_backup_remaining_seconds" in runtime):
                    runtime = None
                if runtime is None:
                    # A failed read proves nothing about the backup. Keep the
                    # last confirmed wait and retry with capped backoff.
                    next_probe = seen_at + retry_delay
                    retry_delay = min(retry_delay * 2, BACKUP_STATUS_POLL_SECONDS)
                else:
                    if allow_backup_wait and remaining is not None and not backup_running:
                        backup_until = seen_at + remaining + BACKUP_FINISH_GRACE_SECONDS
                    elif remaining is None and backup_running and backup_until is not None:
                        # Completion of the backup record can precede release of
                        # the lease and process exit. Retain a finish window.
                        backup_until = min(backup_until, seen_at + BACKUP_FINISH_GRACE_SECONDS)
                    backup_running = remaining is not None
                    next_probe = seen_at + BACKUP_STATUS_POLL_SECONDS
                    retry_delay = 1

            wait_until = probe_until()
            if seen_at >= wait_until:
                raise BackgroundExitTimeout(name, forward_deadline())
            if seen_at >= deadline and not logged_extension:
                detail = "backup status unavailable, retrying" if runtime is None else self.drain_progress(runtime)
                self.log(f"   {name} still draining past the ordinary timeout" + (f": {detail}" if detail else ""))
                logged_extension = True
            if seen_at >= next_note:
                detail = self.drain_progress(runtime)
                self.log(f"   {name} still finishing in-flight work" + (f": {detail}" if detail else " ..."))
                next_note += 60
            self.ops.sleep(min(poll, wait_until - seen_at))

    def wait_recovery_exit(self, name: str, deadline: float) -> BackgroundExit:
        while self.ops.monotonic() < deadline:
            try:
                return self.wait_exit(name, deadline, poll=HANDOFF_POLL_SECONDS, allow_backup_wait=False)
            except KeyboardInterrupt:
                self.log(f"   Ctrl-C received while recovering the background. Waiting for {name} to exit.")
        raise BackgroundExitTimeout(name, deadline)

    def mounts_for(self, role: str, name: str) -> list[Mount]:
        group = "background" if role == "background" else "serving"
        resolved = []
        for mount in self.cfg.mounts["common"] + self.cfg.mounts[group]:
            if mount.per_container_copy:
                copy = os.path.join(self.cfg.work_dir, "containers", name, os.path.basename(mount.target.rstrip("/")))
                resolved.append(Mount(copy, mount.target, readonly=False, per_container_copy=True))
            else:
                resolved.append(mount)
        return resolved

    def prepare_per_container_copies(self, role: str, name: str) -> None:
        group = "background" if role == "background" else "serving"
        for template in self.cfg.mounts["common"] + self.cfg.mounts[group]:
            if not template.per_container_copy:
                continue
            dest = os.path.join(self.cfg.work_dir, "containers", name, os.path.basename(template.target.rstrip("/")))
            self.ops.run(["rm", "-rf", dest], mutate=True)
            self.ops.run(["mkdir", "-p", os.path.dirname(dest)], mutate=True)
            self.ops.run(["cp", "-a", template.source, dest], mutate=True)

    def docker_env_args(self, role: str) -> list[str]:
        group = "background" if role == "background" else "serving"
        args = ["--env-file", self.cfg.env_files[group],
                "-e", f"SOURCES_PROCESS_ROLE={'background' if role == 'background' else 'serving'}"]
        for key, value in sorted(self.cfg.extra_env.get(group, {}).items()):
            args += ["-e", f"{key}={value}"]
        return args

    def mount_args(self, mounts: list[Mount]) -> list[str]:
        args = []
        for m in mounts:
            spec = f"type=bind,source={m.source},target={m.target}"
            if m.readonly:
                spec += ",readonly"
            args += ["--mount", spec]
        return args

    def create(self, target: Target, role: str) -> None:
        name = target.containers[role]
        self.prepare_per_container_copies(role, name)
        args = ["docker", "create", "--name", name, "--hostname", name,
                "--user", self.cfg.user, "--entrypoint", "/app/sub2api",
                "--restart", "unless-stopped",
                "--memory", self.cfg.memory, "--cpus", self.cfg.cpus,
                "--pids-limit", str(self.cfg.pids_limit),
                "--log-opt", f"max-size={self.cfg.log_max_size}", "--log-opt", f"max-file={self.cfg.log_max_file}",
                "--publish", f"127.0.0.1:{target.ports[role]}:8080",
                "--network", self.cfg.networks[0], "--network-alias", name,
                *(["--network-alias", self.cfg.serving_alias]
                  if role in SERVING_ROLES and self.cfg.serving_alias else []),
                "--label", f"com.sources.deploy.slot={target.slot}",
                "--label", f"com.sources.deploy.role={role}",
                "--label", f"com.sources.deploy.created={now()}"]
        args += self.docker_env_args(role)
        args += self.mount_args(self.mounts_for(role, name))
        args.append(target.image)
        self.ops.run(args, mutate=True)
        aliases = ["--alias", name]
        if role in SERVING_ROLES and self.cfg.serving_alias:
            aliases += ["--alias", self.cfg.serving_alias]
        for network in self.cfg.networks[1:]:
            self.ops.run(["docker", "network", "connect", *aliases, network, name], mutate=True)

    def one_off(self, image: str, role: str, command: list[str], extra_mounts: list[Mount] | None = None,
                timeout: float = 600, mutate: bool = False) -> str:
        """Run the image once with the role's env and mounts; returns stdout.
        Read-only commands also run in dry-run mode; mutating ones are printed."""
        if mutate and self.ops.dry_run:
            self.ops.log(f"DRY-RUN one-off {image} {' '.join(command)}")
            return ""
        name = f"{self.cfg.name_prefix}-oneoff-{os.getpid()}-{int(self.ops.monotonic())}"
        group = "background" if role == "background" else "serving"
        mounts = [m for m in self.cfg.mounts["common"] + self.cfg.mounts[group] if not m.per_container_copy]
        args = ["docker", "create", "--name", name, "--user", self.cfg.user, "--entrypoint", "/app/sub2api",
                "--network", self.cfg.networks[0], "--label", "com.sources.deploy.role=oneoff"]
        args += self.docker_env_args(role)
        args += self.mount_args(mounts + (extra_mounts or []))
        args += [image] + command
        self.ops.run(args, mutate=False)
        try:
            for network in self.cfg.networks[1:]:
                self.ops.run(["docker", "network", "connect", network, name], mutate=False)
            proc = self.ops.run(["docker", "start", "--attach", name], mutate=False, timeout=timeout, check=False)
            if proc.returncode != 0:
                tail = (proc.stderr or "").strip().splitlines()[-3:]
                raise DeployError(f"{' '.join(command)} failed: " + " | ".join(tail))
            return proc.stdout
        finally:
            self.ops.run(["docker", "rm", "-f", name], mutate=False, check=False)

    @staticmethod
    def result_line(output: str, prefix: str) -> str:
        for line in output.splitlines():
            if line.startswith(prefix + " ") or line == prefix:
                return line[len(prefix):].strip()
        raise DeployError(f"no '{prefix}' line in command output")

    def install_upstreams(self, text: str) -> None:
        previous = self.ops.read_text(self.cfg.upstream_file) if Path(self.cfg.upstream_file).exists() else None
        self.ops.write_file_atomic(self.cfg.upstream_file, text)
        test = self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True, check=False)
        if test.returncode != 0:
            if previous is not None:
                self.ops.write_file_atomic(self.cfg.upstream_file, previous)
            raise DeployError("nginx -t rejected the upstream file; previous file restored, routing unchanged")
        self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True)

    def switch_to(self, view: dict[str, Any]) -> None:
        ports = view["ports"]
        self.install_upstreams(render_upstreams(ports["serving"], ports["standby"], ports["background"]))

    # steps ------------------------------------------------------------------
    def preflight(self, state: dict[str, Any], target: Target, reuse: bool,
                  allow_public_health_failure: bool = False) -> list[str]:
        problems = nginx_ready_for_fixed_upstreams(
            self.ops.run([self.cfg.nginx_bin, "-T"], mutate=False, timeout=60).stdout)
        current = state.get("current")
        if not current:
            problems.append("no current release recorded; run 'adopt' once first")
        elif current.get("slot") == target.slot:
            problems.append(f"target slot {target.slot} is the current slot")
        available = self.ops.meminfo_available()
        if available and available < self.cfg.min_host_available_gib * GIB:
            problems.append(f"host MemAvailable {available / GIB:.1f}GiB below {self.cfg.min_host_available_gib}GiB")
        if not reuse:
            running = []
            for role, name in target.containers.items():
                info = self.ops.inspect(name)
                if info and info.get("State", {}).get("Running"):
                    running.append(name)
                    problems.append(f"{name} (previous release in slot {target.slot}) is still running/draining")
            if not running:
                problems += port_problems(self.ops, [target.ports[r] for r in ROLES])
        # The same request the guard makes after the switch: if it fails now,
        # the guard would fail too and roll back a healthy release.
        status, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
        if status != 200:
            seen = f"public health check returned status {status} (0 means no response)"
            if allow_public_health_failure:
                self.log(f"   WARNING: {seen}; continuing because of --allow-public-health-failure; "
                         "the guard still requires 200 after the switch")
            else:
                problems.append(f"{seen}; the guard after the switch would fail the same way and roll back; "
                                "pass --allow-public-health-failure to deploy anyway")
        if problems:
            raise DeployError("preflight failed, nothing changed:\n  - " + "\n  - ".join(problems))
        pending = self.result_line(self.one_off(target.image, "background", ["-list-pending-migrations"]), PENDING_PREFIX)
        return [p for p in pending.split(",") if p]

    def export_assets(self, image: str) -> None:
        staging = os.path.join(self.cfg.work_dir, "assets-staging")
        self.ops.run(["rm", "-rf", staging], mutate=True)
        self.ops.run(["install", "-d", "-o", self.cfg.user.split(":")[0], "-m", "0755", staging], mutate=True)
        out = self.one_off(image, "serving", ["-export-web-assets", "/export"],
                           extra_mounts=[Mount(staging, "/export", readonly=False)], mutate=True)
        if not self.ops.dry_run:
            self.log(f"   exported {self.result_line(out, ASSETS_PREFIX)} asset files (existing ones are kept)")
        # Union into the served directory: never delete, never overwrite.
        self.ops.run(["cp", "-Rn", staging + "/.", self.cfg.assets_dir + "/"], mutate=True)

    def owner_runtime(self, view: dict[str, Any], *, deadline: float | None = None) -> dict[str, Any] | None:
        return self.runtime_request_before(
            lambda timeout: self.runtime(view["ports"]["background"], self.token_for(view, "background"), timeout=timeout),
            deadline,
        )

    def runtime_request_before(self, request: Callable[[float], dict[str, Any] | None],
                               deadline: float | None) -> dict[str, Any] | None:
        """Limit waiting for a runtime request to the handoff deadline."""
        result = None

        def probe() -> None:
            nonlocal result
            try:
                timeout = RUNTIME_STATUS_TIMEOUT_SECONDS
                if deadline is not None:
                    timeout = min(timeout, deadline - self.ops.monotonic())
                    if timeout <= 0:
                        return
                result = request(timeout)
            except Exception:  # noqa: BLE001 - a failed request gives no runtime status
                return

        if deadline is None:
            probe()
            return result

        if self.ops.monotonic() >= deadline:
            return None
        # urllib's socket timeout covers one I/O, not the full response. A
        # delayed header and body must not hold up the failure handling.
        pending = threading.Thread(target=probe, daemon=True)
        pending.start()
        pending.join(max(0, deadline - self.ops.monotonic()))
        if pending.is_alive() or self.ops.monotonic() >= deadline:
            return None
        return result

    @staticmethod
    def scheduled_backup_remaining(runtime: dict[str, Any] | None) -> int | None:
        remaining = (runtime or {}).get("scheduled_backup_remaining_seconds")
        return remaining if type(remaining) is int and remaining >= 0 else None

    @staticmethod
    def drain_progress(runtime: dict[str, Any] | None) -> str | None:
        if not runtime:
            return None
        parts = []
        if Deployer.scheduled_backup_remaining(runtime) is not None:
            parts.append("scheduled backup still active")
        writes = runtime.get("active_admin_writes")
        if type(writes) is int and writes > 0:
            parts.append(f"{writes} admin request(s) in flight")
        stopping = runtime.get("stopping")
        if isinstance(stopping, list) and stopping:
            parts.append("still stopping " + ", ".join(str(name) for name in stopping))
        return "; ".join(parts) or None

    @staticmethod
    def background_restart_policy(info: dict[str, Any] | None) -> str:
        policy = ((info or {}).get("HostConfig") or {}).get("RestartPolicy") or {}
        restart = policy.get("Name") or "no"
        if restart == "on-failure" and (limit := policy.get("MaximumRetryCount")):
            restart += f":{limit}"
        return restart

    def start_background(self, view: dict[str, Any], *, restart: str = "unless-stopped",
                         deadline: float | None = None) -> None:
        name = view["containers"]["background"]
        # Once ownership is vacant, Ctrl-C must not stop the Docker clients
        # that put an owner back in service. They inherit the ignored signal.
        with finish_handoff_cleanup(ignore=True):
            self.ops.run(["docker", "update", f"--restart={restart}", name], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))
            self.ops.run(["docker", "start", name], mutate=True, timeout=self.recovery_timeout(deadline, 600))
            if not self.ops.dry_run:
                self.wait_ready(view["ports"]["background"], self.token_for(view, "background"),
                                "background", self.cfg.background_ready_timeout, container=name,
                                poll=HANDOFF_POLL_SECONDS, deadline=deadline)

    def restore_background(self, current: dict[str, Any], failed: DeployError, *, restart: str,
                           recovery: BackgroundRecovery | None = None) -> NoReturn:
        old = current["containers"]["background"]
        try:
            self.start_background(current, restart=restart, deadline=recovery.work_until if recovery else None)
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as restore_err:
            error = FallbackFailed(f"background handoff failed ({failed}); could not restart {old}: {restore_err}")
            if recovery is not None and not self.ops.dry_run:
                # Persist failure before the caller checks for container removal.
                self._record_unrestored(load_state(self.cfg), error)
            raise error from restore_err
        raise BackgroundRestored(f"background handoff failed ({failed}); previous background {old} is running again",
                                 cleanup_deadline=recovery.deadline if recovery else None) from failed

    def handoff_background(self, current: dict[str, Any], target: Target, create: bool,
                           back: bool = False) -> float | None:
        """Move background ownership from current to target. The old owner gets
        one SIGTERM and finishes in-flight work; the kernel lock passes when it
        exits. If the new owner does not become ready, the old one restarts.
        An old owner that reports admin_during_drain keeps answering admin
        reads until it closes its listener, just before it exits. back: the
        handoff returns ownership to target after traffic already moved there.
        Return the active recovery deadline, if any, for subsequent cleanup."""
        old = current["containers"]["background"]
        new = target.containers["background"]
        before = self.owner_runtime(current)  # read-only, also in a dry run
        old_info, old_removed = self.background_container(old)
        if old_info is None and not old_removed:
            raise DeployError(f"could not confirm the state of background container {old} before handoff")
        if create:
            self.create(target, "background")
        view = target.view()
        # Protect the transition before changing the restart policy or sending
        # TERM. Installing the handler after a timeout leaves a signal gap.
        with finish_handoff_cleanup() as interrupted:
            recovery = None
            if old_info is not None:
                recovery = self.drain_background(current, old_info, before, successor=new, back=back,
                                                 interrupted=interrupted)
            else:
                self.log(f"   {old} is absent. Starting the prepared background {new}.")
            try:
                self.start_background(view, deadline=recovery.work_until if recovery else None)
            except (DeployError, OSError, subprocess.SubprocessError) as err:
                if recovery is not None:
                    raise FallbackFailed(f"could not make {new} ready during background recovery: {err}") from err
                self.log(f"   new background failed: {err}. Restoring {old} once {new} exits.")
                new_info, new_removed = self.background_container(new)
                if new_info is not None:
                    try:
                        recovery = self.drain_background(view, new_info, self.owner_runtime(view), successor=old,
                                                         back=True, interrupted=interrupted)
                    except (DeployError, OSError, subprocess.SubprocessError) as stop_err:
                        raise FallbackFailed(f"new background {new} did not become ready ({err}). "
                                             f"Could not finish stopping it before restoring {old}: {stop_err}") from stop_err
                elif not new_removed:
                    raise FallbackFailed(f"could not confirm the state of new background {new} before restoring {old}") from err
                self.restore_background(current, DeployError(f"new background did not become ready: {err}"),
                                        restart=self.background_restart_policy(old_info), recovery=recovery)
            return recovery.deadline if recovery is not None else None

    def drain_background(self, current: dict[str, Any], old_info: dict[str, Any],
                         before: dict[str, Any] | None, successor: str, back: bool,
                         interrupted: Callable[[], bool]) -> BackgroundRecovery | None:
        old = current["containers"]["background"]
        restart = self.background_restart_policy(old_info)

        follow_up = (f"Check container state and verify {old} has exited before starting "
                     "the background for the active slot.")

        def restore_restart_policy(reason: str, deadline: float | None = None) -> None:
            try:
                # A terminal Ctrl-C reaches the Docker CLI too. The child
                # inherits SIG_IGN for this cleanup command.
                with finish_handoff_cleanup(ignore=True):
                    try:
                        self.ops.run(["docker", "update", f"--restart={restart}", old], mutate=True,
                                     timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
                    except KeyboardInterrupt:
                        # Docker may already have applied the update.
                        self.ops.run(["docker", "update", f"--restart={restart}", old], mutate=True,
                                     timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                failure = FallbackFailed(f"{reason} Could not restore the restart policy of {old}: {err}. {follow_up}")
                if not self.ops.dry_run:
                    self._record_unrestored(load_state(self.cfg), failure)
                raise failure from err

        try:
            self.ops.run(["docker", "update", "--restart=no", old], mutate=True)
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            with contextlib.suppress(OSError, subprocess.SubprocessError):
                if self.ops.container_missing(old):
                    self.log(f"   {old} was removed before SIGTERM. Starting the prepared background {successor}.")
                    return
            # The daemon can apply the update before the CLI is interrupted.
            reason = f"could not disable automatic restart of {old}: {err}."
            restore_restart_policy(reason)
            raise DeployError(f"{reason} Its restart policy was restored.") from err
        if not back and interrupted():
            reason = f"handoff interrupted before SIGTERM to {old}."
            restore_restart_policy(reason)
            raise DeployError(f"{reason} Its restart policy was restored.")
        self.log(f"   TERM {old}. Waiting for it to finish in-flight work.")
        if (before or {}).get("admin_during_drain") == "read-only":
            self.log(f"   while {old} drains it answers admin reads and refuses admin writes with 503. "
                     "The admin API is down only from its exit until the new background is ready.")
        else:
            self.log(f"   the admin API is unavailable until the new background is ready "
                     f"({old} does not report read-only draining)")
        signal_failed = False
        try:
            result = self.ops.run(["docker", "kill", "--signal=TERM", old], mutate=True, check=False)
            signal_failed = result.returncode != 0
        except (OSError, subprocess.SubprocessError, KeyboardInterrupt):
            signal_failed = True
        if self.ops.dry_run:
            return

        # A failed Docker response can precede a delivered signal taking effect.
        # Give the owner a short window to report drain or exit.
        term_confirmed = not signal_failed
        check_term_after = self.ops.monotonic() + HANDOFF_FIRST_NOTE_SECONDS

        def restore_policy_after_timeout(reason: str, deadline: float | None = None) -> FallbackFailed:
            # Persist the recovery instructions even if restoring the policy fails.
            self._record_unrestored(load_state(self.cfg), FallbackFailed(
                f"{reason} Its restart policy may still need to be restored. {follow_up}"))
            restore_restart_policy(reason, deadline)
            return FallbackFailed(f"{reason} Its restart policy was restored. {follow_up}")

        def read_runtime(deadline: float) -> dict[str, Any] | None:
            nonlocal term_confirmed, before
            runtime = self.owner_runtime(current, deadline=deadline)
            if self.ops.monotonic() >= deadline:
                return None
            # The drain endpoint can make an owner read-only without shutting
            # it down. Only the shutdown cleanup reports stopping services.
            stopping = (runtime or {}).get("stopping")
            if isinstance(stopping, list) and stopping:
                term_confirmed = True
                before = runtime
            if (not term_confirmed and runtime and runtime.get("ready") is True
                    and self.ops.monotonic() >= check_term_after):
                restore_restart_policy(f"SIGTERM to {old} failed.")
                raise BackgroundRestored(f"SIGTERM to {old} failed. The background is still ready and its restart policy was restored.")
            return runtime

        normal_deadline = None

        def wait_for_drain() -> BackgroundExit:
            nonlocal normal_deadline
            normal_deadline = self.ops.monotonic() + self.cfg.background_exit_timeout
            return self.wait_exit(old, normal_deadline, poll=HANDOFF_POLL_SECONDS,
                                  first_note=HANDOFF_FIRST_NOTE_SECONDS, read_runtime=read_runtime, initial_runtime=before)

        failure = None
        recovery = None
        try:
            if term_confirmed:
                exited = wait_for_drain()
            else:
                self.log(f"   could not confirm SIGTERM to {old}. Checking whether the owner starts draining.")
                try:
                    exited = self.wait_exit(old, self.ops.monotonic() + HANDOFF_TERM_CONFIRM_SECONDS,
                                            poll=HANDOFF_POLL_SECONDS, first_note=HANDOFF_FIRST_NOTE_SECONDS,
                                            read_runtime=read_runtime, allow_backup_wait=False)
                except BackgroundExitTimeout:
                    if not term_confirmed:
                        raise
                    exited = wait_for_drain()
        except (BackgroundExitTimeout, KeyboardInterrupt) as err:
            if not term_confirmed:
                raise restore_policy_after_timeout(f"could not confirm SIGTERM or exit of {old}.") from err
            if isinstance(err, BackgroundExitTimeout):
                failure = err
                cutoff = err.deadline
            else:
                failure = DeployError("handoff interrupted")
                interrupted_at = self.ops.monotonic()
                cutoff = min(normal_deadline, interrupted_at) if normal_deadline is not None else interrupted_at
            recovery = BackgroundRecovery(cutoff + HANDOFF_RECOVERY_SECONDS)
            self.log(f"   {failure}. Trying to restore the background within the recovery window.")
            try:
                exited = self.wait_recovery_exit(old, recovery.work_until)
            except BackgroundExitTimeout:
                unresolved = restore_policy_after_timeout(f"could not confirm exit of {old} after the handoff and recovery waits.",
                                                          deadline=recovery.deadline)
                # A fast policy update leaves time to catch an exit near the
                # end of recovery. The update and final poll share the deadline.
                recovery = BackgroundRecovery(recovery.deadline, failure_recorded=True)
                try:
                    exited = self.wait_recovery_exit(old, recovery.deadline)
                except BackgroundExitTimeout as cleanup_err:
                    raise unresolved from cleanup_err
        if exited.timed_out_at is not None:
            failure = BackgroundExitTimeout(old, exited.timed_out_at)
            recovery = BackgroundRecovery(exited.timed_out_at + HANDOFF_RECOVERY_SECONDS)
        code = exited.code
        if not back and code is not None and (failure is not None or interrupted()):
            try:
                self.restore_background(current, failure or DeployError("handoff interrupted"), restart=restart,
                                        recovery=recovery)
            except FallbackFailed:
                _, removed = self.background_container(old, deadline=recovery.deadline if recovery else None)
                if not removed:
                    raise
                if recovery is not None:
                    recovery = BackgroundRecovery(recovery.deadline, failure_recorded=True)
                code = None
        if code is None:
            self.log(f"   {old} was removed. Starting the prepared background {successor}.")
        else:
            self.log(f"   {old} exited with code {code}")
        return recovery

    def guard(self, target: Target, previous: dict[str, Any]) -> None:
        name = target.containers["serving"]
        port = target.ports["serving"]
        token = self.token_for(target.view(), "serving")
        info = self.ops.inspect(name) or {}
        started = info.get("State", {}).get("StartedAt")
        restarts = info.get("RestartCount", 0)
        limit = int(info.get("HostConfig", {}).get("Memory") or 0)
        log_offset = self.ops.file_size(self.cfg.nginx_error_log)
        new_ports = {str(target.ports[r]) for r in SERVING_ROLES}
        samples: list[tuple[float, int]] = []
        public_failures = 0
        public_status = 200
        deadline = self.ops.monotonic() + self.cfg.guard_seconds
        while True:
            reason = None
            runtime = self.runtime(port, token)
            info = self.ops.inspect(name) or {}
            state = info.get("State", {})
            if not runtime or not runtime.get("ready"):
                reason = "new serving is not ready"
            elif not state.get("Running") or state.get("StartedAt") != started or info.get("RestartCount", 0) != restarts:
                reason = "new serving restarted"
            elif state.get("OOMKilled"):
                reason = "new serving was OOM-killed"
            memory = self.container_memory(info)
            if reason is None and memory is not None:
                t = self.ops.monotonic()
                samples = [(ts, m) for ts, m in samples if t - ts <= 30] + [(t, memory)]
                if limit and memory > limit * self.cfg.guard_memory_fraction:
                    reason = f"memory {memory / GIB:.1f}GiB above {self.cfg.guard_memory_fraction:.0%} of limit"
                elif memory - min(m for _, m in samples) > self.cfg.guard_memory_jump_gib * GIB:
                    reason = f"memory grew more than {self.cfg.guard_memory_jump_gib}GiB within 30s"
            if reason is None:
                public_status, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
                public_failures = public_failures + 1 if public_status != 200 else 0
                if public_failures >= 3:
                    reason = f"public health failed 3 times in a row (last status {public_status})"
            if reason is None:
                errors = self.connect_errors(self.ops.read_from(self.cfg.nginx_error_log, log_offset), new_ports)
                if errors >= self.cfg.guard_connect_errors:
                    reason = f"{errors} Nginx connect failures to the new serving ports"
            if reason:
                self.log(f"   guard failed: {reason}; switching traffic back to slot {previous['slot']}")
                try:
                    self.switch_to(previous)
                except DeployError as err:
                    raise FallbackFailed(f"guard failed ({reason}) and switching back failed ({err})") from err
                self.log("   handing the background back to the previous release")
                recovery_deadline = self.hand_back(target, previous)
                restored = f"guard failed ({reason}); the previous release serves traffic and owns the background again"
                protect = finish_handoff_cleanup(ignore=True) if recovery_deadline is not None else contextlib.nullcontext()
                with protect:
                    try:
                        self.retire_serving(target.view(), deadline=recovery_deadline)
                    except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                        raise ServingCleanupFailed(f"{restored}. Incomplete serving cleanup: {err}. "
                                                   "Check the target slot before clearing in_progress.") from err
                raise DeployError(f"{restored}; the new serving containers were stopped and kept for inspection")
            if self.ops.monotonic() >= deadline:
                return
            self.ops.sleep(self.cfg.guard_interval)

    def hand_back(self, target: Target, previous: dict[str, Any]) -> float | None:
        """Return background ownership to the release that still serves traffic."""
        try:
            return self.handoff_background(target.view(), Target.from_view(previous, self.cfg.runtime_token_file),
                                           create=False, back=True)
        except FallbackFailed:
            raise
        except DeployError as err:
            raise FallbackFailed(f"handing the background back failed: {err}") from err

    def container_memory(self, info: dict[str, Any]) -> int | None:
        cid = info.get("Id")
        if not cid:
            return None
        path = self.cfg.cgroup_memory_path.format(id=cid)
        try:
            return int(self.ops.read_text(path).strip())
        except (OSError, ValueError):
            return None

    @staticmethod
    def connect_errors(log_text: str, ports: set[str]) -> int:
        count = 0
        for line in log_text.splitlines():
            if "connect() failed" in line or "upstream timed out" in line:
                if any(f"127.0.0.1:{p}" in line for p in ports):
                    count += 1
        return count

    def retire_serving(self, view: dict[str, Any], *, deadline: float | None = None) -> None:
        for role in SERVING_ROLES:
            name = view["containers"].get(role)
            if not name:
                continue
            token = None
            with contextlib.suppress(OSError):
                token = self.token_for(view, role)
            info = self.ops.inspect(name, timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
            if not info or not info.get("State", {}).get("Running"):
                continue
            self.ops.run(["docker", "update", "--restart=no", name], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))
            port = view["ports"][role]
            runtime = self.runtime_request_before(
                lambda timeout: self.runtime(port, token, timeout=timeout), deadline,
            ) if token else None
            if runtime and runtime.get("instance_id") and not self.ops.dry_run:
                self.drain_serving(port, token, runtime["instance_id"], deadline=deadline)
            # TERM without waiting: the process finishes requests already routed
            # to it (streams included) and exits by itself.
            self.ops.run(["docker", "kill", "--signal=TERM", name], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))

    def drain_serving(self, port: int, token: str, instance_id: str, *, deadline: float | None) -> None:
        def request(timeout: float) -> None:
            self.ops.http_json(f"http://127.0.0.1:{port}/internal/sources/drain", token=token,
                               method="POST", body={"instance_id": instance_id}, timeout=timeout)

        self.runtime_request_before(request, deadline)

    # entrypoints --------------------------------------------------------------
    def check_running_backup(self, current: dict[str, Any], allow_running_backup: bool) -> None:
        # A backup can start after preflight. The handoff still waits for it.
        if self.scheduled_backup_remaining(self.owner_runtime(current)) is None:
            return
        if not allow_running_backup:
            raise DeployError("preflight failed, nothing changed: a scheduled backup is running. "
                              "Wait for it to finish or pass --allow-running-backup.")
        self.log("   WARNING: a scheduled backup is running. Continuing because of --allow-running-backup.")

    def deploy(self, image: str, allow_contract: bool = False,
               allow_public_health_failure: bool = False,
               allow_running_backup: bool = False) -> None:
        state = load_state(self.cfg)
        if state.get("in_progress"):
            raise DeployError(f"a previous deploy did not finish: {json.dumps(state['in_progress'])}; "
                              "inspect with 'status' and resolve before deploying again")
        current = state.get("current") or {}
        if current:
            self.check_running_backup(current, allow_running_backup)
        slot = "a" if current.get("slot") != "a" else "b"
        target = Target.from_view({**self.cfg.slot_view(slot), "image": image}, self.cfg.runtime_token_file)
        record: dict[str, Any] = {"started_at": now(), "action": "deploy", "image": image, "slot": slot}
        try:
            with self.step("pull image"):
                present = self.ops.run(["docker", "image", "inspect", image], mutate=False, check=False, timeout=30)
                if present.returncode != 0:
                    self.ops.run(["docker", "pull", image], mutate=False, timeout=1800)
            with self.step("preflight"):
                pending = self.preflight(state, target, reuse=False,
                                         allow_public_health_failure=allow_public_health_failure)
                contract = [p for p in pending if "contract" in p]
                if contract and not allow_contract:
                    raise DeployError(f"contract migrations need --allow-contract: {contract}; nothing changed")
                self.log(f"   pending migrations: {pending or 'none'}")
            self._mark(state, target, "migrate")
            if pending:
                with self.step("migrate"):
                    self.one_off(image, "background", ["-migrate-only", ",".join(pending)], timeout=900, mutate=True)
            # A dry run only prints -migrate-only, and the binary's startup
            # check refuses a database with pending migrations, so running it
            # here would report a failure a real run would not have.
            skip_check = self.ops.dry_run and bool(pending)
            with self.step("startup check"):
                if skip_check:
                    self.log("   startup check skipped because this is a dry run: pending migrations "
                             f"{', '.join(pending)} are not applied; a real run applies them first, then checks "
                             "the serving and background environments before creating any container")
                else:
                    # Serving and background use different env files, so a wrong
                    # value in either would otherwise only surface at the handoff,
                    # where the background gap lasts for the readiness timeout.
                    for role in ("serving", "background"):
                        out = self.one_off(image, role, ["-check-startup"])
                        self.log(f"   {role}: " + self.result_line(out, CHECK_PREFIX)[:120])
            with self.step("export assets"):
                self.export_assets(image)
            self._mark(state, target, "start serving")
            with self.step("start serving"):
                for role in ROLES:
                    self.ops.run(["docker", "rm", "-f", target.containers[role]], mutate=True, check=False)
                for role in SERVING_ROLES:
                    self.create(target, role)
                    self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                        self.cfg.ready_timeout)
            self._finish_switch(state, current, target, record, create_background=True)
            if skip_check:
                self.log("note: this dry run did NOT verify the serving and background environment files "
                         "(startup check skipped because migrations are pending)")
        except DeployError as err:
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            self._abort(state, target, err)
            # A failed dry run changed nothing; a record would read like a failed release.
            if not self.ops.dry_run:
                append_history(self.cfg, record)
            raise

    def rollback(self, allow_running_backup: bool = False) -> None:
        state = load_state(self.cfg)
        current, previous = state.get("current"), state.get("previous")
        if not current or not previous:
            raise DeployError("no previous release recorded; nothing to roll back to")
        self.check_running_backup(current, allow_running_backup)
        for role in ROLES:
            if not self.ops.inspect(previous["containers"][role]):
                raise DeployError(f"previous container {previous['containers'][role]} no longer exists")
        target = Target.from_view(previous, self.cfg.runtime_token_file)
        record: dict[str, Any] = {"started_at": now(), "action": "rollback", "image": target.image, "slot": target.slot}
        try:
            with self.step("start previous serving"):
                for role in SERVING_ROLES:
                    self.ops.run(["docker", "update", "--restart=unless-stopped", target.containers[role]], mutate=True)
                    self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        self.wait_ready(target.ports[role], self.token_for(previous, role), "serving",
                                        self.cfg.ready_timeout)
            self._finish_switch(state, current, target, record, create_background=False)
        except DeployError as err:
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            if not self.ops.dry_run:
                if isinstance(err, FallbackFailed):
                    self._record_unrestored(state, err)
                elif isinstance(err, BackgroundRestored):
                    state["in_progress"] = None
                    save_state(self.cfg, state)
                append_history(self.cfg, record)
            raise

    def _finish_switch(self, state: dict[str, Any], current: dict[str, Any], target: Target,
                       record: dict[str, Any], create_background: bool) -> None:
        view = target.view()
        self._mark(state, target, "background handoff")
        with self.step("background handoff"):
            self.handoff_background(current, target, create=create_background)
        self._mark(state, target, "switch")
        with self.step("switch"):
            try:
                self.switch_to(view)
            except FallbackFailed:
                raise
            except DeployError as err:
                self.log("   switch failed; handing the background back")
                recovery_deadline = self.hand_back(target, current)
                raise DeployError(f"{err}; the previous release owns the background again",
                                  cleanup_deadline=recovery_deadline) from err
        if not self.ops.dry_run:
            self._mark(state, target, "guard")
            with self.step("guard"):
                self.guard(target, current)
        with self.step("retire previous serving"):
            self.retire_serving(current)
        view["deployed_at"] = now()
        state.update(previous=current, current=view, in_progress=None)
        if not self.ops.dry_run:
            save_state(self.cfg, state)
        record.update(result="ok", steps=self.timings, finished_at=now())
        if not self.ops.dry_run:
            append_history(self.cfg, record)
        self.log(f"done: slot {target.slot} serving {target.image}")

    def _mark(self, state: dict[str, Any], target: Target, step: str) -> None:
        state["in_progress"] = {"target": target.view(), "step": step, "since": now()}
        if not self.ops.dry_run:
            save_state(self.cfg, state)

    def _record_unrestored(self, state: dict[str, Any], err: FallbackFailed) -> None:
        """Keep in_progress, with why, so the next run refuses and 'status'
        shows what a person has to do."""
        if state.get("in_progress"):
            state["in_progress"].update(failed=str(err), failed_at=now())
            save_state(self.cfg, state)

    def _abort(self, state: dict[str, Any], target: Target, err: DeployError) -> None:
        """Record failed recovery or clean up after the previous release is ready."""
        step = (state.get("in_progress") or {}).get("step")
        if self.ops.dry_run:
            self.log(f"FAILED: {err}")
            return
        if isinstance(err, FallbackFailed):
            # Keep in_progress so the next run refuses until a person resolves
            # it, and keep why, so 'status' shows what needs doing.
            self._record_unrestored(state, err)
            status = "FAILED" if isinstance(err, ServingCleanupFailed) else "FAILED AND NOT RESTORED"
            self.log(f"{status}: {err}")
            return
        if step in (None, "migrate", "start serving", "background handoff", "switch"):
            deadline = err.cleanup_deadline
            protect = finish_handoff_cleanup(ignore=True) if deadline is not None else contextlib.nullcontext()
            with protect:
                try:
                    for role in SERVING_ROLES:
                        name = target.containers[role]
                        result = self.ops.run(["docker", "rm", "-f", name], mutate=True, check=False,
                                              timeout=self.recovery_timeout(deadline, 600))
                        if deadline is not None and result.returncode:
                            timeout = self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS)
                            if not self.ops.container_missing(name, timeout=timeout):
                                raise DeployError(f"could not remove {name} (exit {result.returncode})")
                except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as cleanup_err:
                    if deadline is None:
                        raise
                    failure = FallbackFailed(f"{err}. Incomplete serving cleanup: {cleanup_err}. "
                                             "Check the target slot before clearing in_progress.")
                    self._record_unrestored(state, failure)
                    self.log(f"FAILED: {failure}")
                    return
        state["in_progress"] = None
        save_state(self.cfg, state)
        self.log(f"FAILED: {err}")


# --------------------------------------------------------------------------
# Status, watch and adopt


def status(cfg: Config, ops: Ops) -> dict[str, Any]:
    state = load_state(cfg)
    report: dict[str, Any] = {"in_progress": state.get("in_progress")}
    for key in ("current", "previous"):
        view = state.get(key)
        if not view:
            report[key] = None
            continue
        entry = {"slot": view["slot"], "image": view.get("image"), "deployed_at": view.get("deployed_at"),
                 "containers": {}}
        for role, name in view["containers"].items():
            info = ops.inspect(name)
            s = (info or {}).get("State", {})
            entry["containers"][role] = {"name": name, "port": view["ports"][role],
                                         "status": s.get("Status", "missing"),
                                         "health": (s.get("Health") or {}).get("Status"),
                                         "restarts": (info or {}).get("RestartCount"),
                                         # "no" on a running background: it was told to
                                         # drain and Docker will not bring it back.
                                         "restart_policy": (((info or {}).get("HostConfig") or {})
                                                            .get("RestartPolicy") or {}).get("Name")}
        report[key] = entry
    return report


def watch_once(cfg: Config, ops: Ops, history: dict[str, list[tuple[float, int]]]) -> list[dict[str, Any]]:
    """One resident sample of the current release; returns alerts."""
    state = load_state(cfg)
    current = state.get("current")
    if not current:
        return []
    deployer = Deployer(cfg, ops)
    alerts = []
    t = ops.monotonic()
    for role, name in current["containers"].items():
        token = None
        with contextlib.suppress(OSError):
            token = deployer.token_for(current, role)
        info = ops.inspect(name) or {}
        s = info.get("State", {})
        port = current["ports"][role]
        status_code, _ = ops.http_json(f"http://127.0.0.1:{port}/health")
        runtime = deployer.runtime(port, token) if token else None
        memory = deployer.container_memory(info)
        sample = {"role": role, "name": name, "running": s.get("Running"), "health": status_code,
                  "restarts": info.get("RestartCount"), "oom": s.get("OOMKilled"),
                  "memory_gib": round(memory / GIB, 2) if memory else None,
                  "active_http": (runtime or {}).get("active_http")}
        reasons = []
        if not s.get("Running") or status_code != 200:
            reasons.append("unhealthy")
        if s.get("OOMKilled"):
            reasons.append("oom")
        if runtime and (runtime.get("active_http") or 0) >= cfg.watch_active_http:
            reasons.append("active_http")
        if memory:
            series = [(ts, m) for ts, m in history.get(name, []) if t - ts <= 30] + [(t, memory)]
            history[name] = series
            if memory >= cfg.watch_memory_gib * GIB:
                reasons.append("memory")
            if memory - min(m for _, m in series) >= cfg.guard_memory_jump_gib * GIB:
                reasons.append("memory_jump")
        if reasons:
            alerts.append({**sample, "alert": reasons, "at": now()})
    return alerts


def adopt(cfg_path: str, ops: Ops, serving: str, standby: str, background: str, work_dir: str,
          public_health_url: str) -> None:
    """Record running legacy containers as the current release and derive
    the fixed configuration from them. Secrets go to root-only env files and
    are never printed. Durable state paths are referenced in place;
    per-release inputs are copied under work_dir."""
    names = {"serving": serving, "standby": standby, "background": background}
    infos = {}
    for role, name in names.items():
        info = ops.inspect(name)
        if not info or not info.get("State", {}).get("Running"):
            raise DeployError(f"{name} is not a running container")
        infos[role] = info
    drop = {"PATH", "SOURCES_PROCESS_ROLE", "SOURCES_MEDIA_ENGINE_ADMISSION_STATE_FILE", "SOURCES_MEDIA_ENGINE_LEGACY_ORIGIN",
            "SOURCES_RUNTIME_TOKEN_FILE"}
    inputs = Path(work_dir) / "inputs"
    plan: list[str] = []

    def env_file(role: str) -> str:
        env = [e for e in infos[role]["Config"].get("Env") or [] if e.split("=", 1)[0] not in drop]
        env.append("SOURCES_RUNTIME_TOKEN_FILE=/config/runtime.token")
        path = str(Path(work_dir) / f"{role}.env")
        ops.write_file_atomic(path, "\n".join(sorted(env)) + "\n", mode=0o600)
        plan.append(f"env file {path} ({len(env)} variables, values not shown)")
        return path

    def mounts(role: str) -> list[dict[str, Any]]:
        result = []
        for m in infos[role]["Mounts"]:
            dst = m["Destination"]
            if dst == "/runtime/admission":
                continue  # media engine admission journal is not used by fixed releases
            entry: dict[str, Any] = {"source": m["Source"], "target": dst, "readonly": not m["RW"]}
            if dst == "/config/runtime.token":
                entry["source"] = str(Path(work_dir) / "runtime.token")
            elif dst == "/config/main-runtime.yaml":
                copy = inputs / role / "main-runtime.yaml"
                ops.run(["install", "-D", "-m", "0444", "-o", cfg_user(infos[role]), m["Source"], str(copy)], mutate=True)
                entry["source"] = str(copy)
                plan.append(f"copied {m['Source']} -> {copy}")
            elif dst == "/runtime/pricing-cache":
                template = inputs / role / "pricing-cache"
                ops.run(["mkdir", "-p", str(template.parent)], mutate=True)
                ops.run(["cp", "-a", m["Source"], str(template)], mutate=True)
                entry.update(source=str(template), per_container_copy=True)
                plan.append(f"pricing cache template {template}")
            result.append(entry)
        return sorted(result, key=lambda e: e["target"])

    def cfg_user(info: dict[str, Any]) -> str:
        return (info["Config"].get("User") or "1001").split(":")[0]

    token_path = Path(work_dir) / "runtime.token"
    if not token_path.exists():
        ops.write_file_atomic(str(token_path), os.urandom(32).hex() + "\n", mode=0o600)
        ops.run(["chown", cfg_user(infos["serving"]), str(token_path)], mutate=True)
    serving_mounts, background_mounts = mounts("serving"), mounts("background")
    common = [m for m in serving_mounts if m in background_mounts and not m.get("per_container_copy")]
    host = infos["serving"]["HostConfig"]
    config = {
        "slots": {"a": {"serving": 9401, "standby": 9402, "background": 9403},
                  "b": {"serving": 9411, "standby": 9412, "background": 9413}},
        "name_prefix": "sources-main",
        "networks": sorted(infos["serving"]["NetworkSettings"]["Networks"].keys()),
        "user": infos["serving"]["Config"].get("User") or "1001:1001",
        "memory": str(host.get("Memory") or "24g"),
        "cpus": str((host.get("NanoCpus") or 12_000_000_000) / 1e9),
        "pids_limit": host.get("PidsLimit") or 4096,
        "log_max_size": host.get("LogConfig", {}).get("Config", {}).get("max-size", "10m"),
        "log_max_file": host.get("LogConfig", {}).get("Config", {}).get("max-file", "3"),
        "env_files": {"serving": env_file("serving"), "background": env_file("background")},
        "mounts": {"common": common,
                   "serving": [m for m in serving_mounts if m not in common],
                   "background": [m for m in background_mounts if m not in common]},
        "runtime_token_file": str(token_path),
        "upstream_file": "/etc/nginx/conf.d/00-sources-main-upstreams.conf",
        "nginx_bin": "/usr/sbin/nginx",
        "nginx_error_log": "/var/log/nginx/error.log",
        "assets_dir": "/opt/sources-web/main/assets",
        "work_dir": work_dir,
        "state_file": str(Path(work_dir) / "state.json"),
        "history_file": str(Path(work_dir) / "history.jsonl"),
        "lock_file": "/var/lock/sources-clean-release.lock",
        "public_health_url": public_health_url,
        "cgroup_memory_path": "/sys/fs/cgroup/system.slice/docker-{id}.scope/memory.current",
        "serving_alias": "sources-main-serving",
    }
    ops.write_file_atomic(cfg_path, json.dumps(config, indent=2) + "\n", mode=0o600)
    legacy = {"slot": "legacy", "image": infos["serving"]["Config"]["Image"], "deployed_at": None,
              "containers": names,
              "ports": {role: int(infos[role]["HostConfig"]["PortBindings"]["8080/tcp"][0]["HostPort"]) for role in names},
              "runtime_token_files": {role: next((m["Source"] for m in infos[role]["Mounts"]
                                                  if m["Destination"] == "/config/runtime.token"), None)
                                      for role in names}}
    if not ops.dry_run:
        Config.load(cfg_path)
        save_state(Config.load(cfg_path), {"current": legacy, "previous": None, "in_progress": None})
    for line in plan:
        ops.log("adopt: " + line)
    ops.log(f"adopt: wrote {cfg_path} and recorded the running containers as the current (legacy) release")


# --------------------------------------------------------------------------
# CLI


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", default=None,
                        help=f"settings file (default: {DEFAULT_CONFIG} for Main commands, "
                             "/opt/sources-deploy/portal.json for portal commands)")
    parser.add_argument("--dry-run", action="store_true", help="print mutating commands instead of running them")
    sub = parser.add_subparsers(dest="command", required=True)
    p_main = sub.add_parser("main", help="deploy an image to Main serving and background")
    p_main.add_argument("image", help="image reference, preferably pinned by digest")
    p_main.add_argument("--allow-contract", action="store_true", help="allow pending migrations marked 'contract'")
    p_main.add_argument("--allow-public-health-failure", action="store_true",
                        help="warn instead of stopping when the public health check fails before the switch "
                             "(the guard still requires 200 after it)")
    p_main.add_argument("--allow-running-backup", action="store_true",
                        help="start a release while a scheduled backup is running")
    p_rollback = sub.add_parser("rollback", help="switch back to the previous slot")
    p_rollback.add_argument("--allow-running-backup", action="store_true",
                            help="roll back while a scheduled backup is running")
    sub.add_parser("status", help="show current and previous release")
    sub.add_parser("watch", help="resident sampler; one JSON line per alert")
    p_adopt = sub.add_parser("adopt", help="one-time: record running legacy containers")
    p_adopt.add_argument("--serving", required=True)
    p_adopt.add_argument("--standby", required=True)
    p_adopt.add_argument("--background", required=True)
    p_adopt.add_argument("--work-dir", default="/opt/sources-deploy/main")
    p_adopt.add_argument("--public-health-url", required=True,
                         help="public health endpoint checked during the guard, e.g. https://example.com/health")
    p_portal = sub.add_parser("portal", help="deploy an image to the Portal (one container, short gap)")
    p_portal.add_argument("image", help="image reference, preferably pinned by digest")
    p_portal.add_argument("--allow-public-health-failure", action="store_true",
                          help="warn instead of stopping when the public health check fails before the switch "
                               "(the guard still requires 200 after it)")
    sub.add_parser("portal-rollback", help="switch the Portal back to the previous container")
    sub.add_parser("portal-status", help="show current and previous Portal release")
    p_padopt = sub.add_parser("portal-adopt", help="one-time: record the running Portal container")
    p_padopt.add_argument("--container", required=True, help="the running Portal container")
    p_padopt.add_argument("--slot-ports", required=True, help="loopback host ports for slots a and b: A,B")
    p_padopt.add_argument("--upstream-file", required=True, help="Nginx file the tool will own for the upstream")
    p_padopt.add_argument("--public-health-url", required=True,
                          help="public health endpoint checked during the guard, e.g. https://example.com/health")
    p_padopt.add_argument("--work-dir", default="/opt/sources-deploy/portal")
    p_padopt.add_argument("--name-prefix", default="sources-portal")
    p_padopt.add_argument("--nginx-bin", default="/usr/sbin/nginx")
    p_padopt.add_argument("--nginx-error-log", default="/var/log/nginx/error.log")
    p_padopt.add_argument("--lock-file", default="/var/lock/sources-clean-release.lock",
                          help="release lock; keep Main's so Portal and Main releases never overlap")
    args = parser.parse_args(argv)
    if args.command.startswith("portal"):
        try:
            # portal.py imports this module as `deploy`; when this file runs as
            # a script it is `__main__`, so register it under that name first
            # so both share one DeployError and one Ops.
            sys.modules["deploy"] = sys.modules[__name__]
            import portal
            return portal.run(args, args.config or portal.DEFAULT_CONFIG)
        except DeployError as err:
            print(f"deploy: {err}", file=sys.stderr)
            return 1
    args.config = args.config or DEFAULT_CONFIG
    ops = Ops(dry_run=args.dry_run)
    try:
        if args.command == "adopt":
            Path(args.work_dir).mkdir(parents=True, exist_ok=True, mode=0o700)
            adopt(args.config, ops, args.serving, args.standby, args.background, args.work_dir,
                  args.public_health_url)
            return 0
        cfg = Config.load(args.config)
        if args.command == "status":
            print(json.dumps(status(cfg, ops), indent=2))
            return 0
        if args.command == "watch":
            history: dict[str, list[tuple[float, int]]] = {}
            while True:
                for alert in watch_once(cfg, ops, history):
                    print(json.dumps(alert, sort_keys=True), flush=True)
                ops.sleep(cfg.guard_interval)
        with release_lock(cfg.lock_file):
            deployer = Deployer(cfg, ops)
            if args.command == "main":
                deployer.deploy(args.image, allow_contract=args.allow_contract,
                                allow_public_health_failure=args.allow_public_health_failure,
                                allow_running_backup=args.allow_running_backup)
            elif args.command == "rollback":
                deployer.rollback(allow_running_backup=args.allow_running_backup)
        return 0
    except DeployError as err:
        print(f"deploy: {err}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
