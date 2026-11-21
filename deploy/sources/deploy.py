#!/usr/bin/env python3
"""Fixed release entrypoint for Sources Main.

One command replaces Main serving and the unique background with one image:

    deploy.py main <image>     preflight, migrate, check, start, hand off,
                               switch, guard (auto-rollback), retire
    deploy.py rollback         return to the previous slot's containers
    deploy.py resume           validate a held release, then finish switching
    deploy.py abort            restore the original release and discard held candidates
    deploy.py status           current/previous slot and container health
    deploy.py watch            resident sampler for systemd (journal only)
    deploy.py adopt ...        one-time: record running legacy containers
    deploy.py portal ...       the Portal (portal.py): portal <image>,
                               portal-rollback, portal-status, portal-adopt

Usage: deploy/sources/README.md. Two fixed slots (a, b)
with fixed container names and ports alternate; Nginx references only the
fixed upstreams sources_main_serving and sources_main_admin, rendered into
one file. The background ownership lock (flock) permits at most one owner;
there is a gap during handoff. The script drains running release containers.

Settings live on the server in /opt/sources-deploy/main.json, secrets in
root-only env files referenced by it. Nothing here is release-specific; if
a step fails the script leaves the old release serving and stops.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import http.client
import json
import os
import posixpath
import queue
import re
import shlex
import shutil
import signal
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterator, NoReturn

DEFAULT_CONFIG = "/opt/sources-deploy/main.json"
DEFAULT_RELEASE_LOCK = "/var/lock/sources-clean-release.lock"
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
FINGERPRINT_TIMEOUT = 10
ROUTE_PROBE_TIMEOUT = 5
# Give local DNS caches time to see the destination before withdrawing the old addresses.
ALIAS_DNS_GRACE_SECONDS = 2
ROUTE_PROBE_PROXY = "proxy_pass http://sources_main_serving/health; # local route verification"
ROUTE_REVISION = re.compile(r"^\s*add_header X-Sources-Route ([0-9a-f]{32}) always;$", re.MULTILINE)

HOLD_CREATION_LABEL = "com.sources.deploy.hold"

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
    """A release operation failed."""

    def __init__(self, *args: Any, cleanup_deadline: float | None = None) -> None:
        super().__init__(*args)
        self.cleanup_deadline = cleanup_deadline


class CommandTimedOut(DeployError):
    """A subprocess may have had side effects before its client timed out."""

    def __init__(self, err: subprocess.TimeoutExpired):
        args = err.cmd
        command = shlex.join(args[:2]) if isinstance(args, (list, tuple)) else "subprocess"
        super().__init__(f"command timed out after {err.timeout:g}s: {command}")


class FallbackFailed(DeployError):
    """Automatic recovery could not finish; needs a person."""


class ServingCleanupFailed(FallbackFailed):
    """Traffic and background are back, but serving cleanup is incomplete."""


class BackgroundRestored(DeployError):
    """The handoff failed, and the original background is ready again."""


class BackgroundExitTimeout(DeployError):
    def __init__(self, name: str, deadline: float):
        super().__init__(f"{name} is still draining; could not confirm exit before the deadline (nothing was killed)")
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


class RollbackIncomplete(FallbackFailed):
    """Serving rolled back, but admin still uses the background we rolled back from."""


class DrainTimeout(DeployError):
    """A container still runs after its drain deadline."""


class RoutingFileNotRestored(FallbackFailed):
    """The file needs repair, but this attempt never reloaded live routing."""


class SwitchNotRestored(FallbackFailed):
    """The old route is still loaded, but the operator must finish recovery."""


class RoutingUnconfirmed(FallbackFailed):
    """A reload may have routed traffic to the candidate; do not drain it."""


class ReloadRejected(RoutingUnconfirmed):
    """Nginx returned a nonzero status; the loaded route still needs verification."""

    def __init__(self, returncode: int, previous: str | None):
        super().__init__(f"nginx reload failed with exit status {returncode}")
        self.previous = previous


class ReloadNotApplied(DeployError):
    """After a rejected reload, the old route was verified and its file restored."""


class RoutingRestored(DeployError):
    """A failed reload was recovered along with the previous background."""


class RollbackBackgroundUnavailable(DeployError):
    """The selected background stopped answering before reload."""


class AliasPublicationFailed(DeployError):
    """The destination did not acquire its shared alias on every network."""


class HoldIsolationChanged(DeployError):
    """A read-only check found a held container advertising the shared alias."""


class SoleAliasProviderKept(FallbackFailed):
    """A serving instance is the only live provider of the shared alias."""


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
    hold_warning_seconds: int = 86400
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
    return read_state_file(Path(cfg.state_file))


def read_state_file(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {"current": None, "previous": None, "in_progress": None}
    return json.loads(path.read_text())


def load_main_state(cfg: Config) -> dict[str, Any]:
    return normalize_main_state(load_state(cfg))


def normalize_main_state(state: dict[str, Any]) -> dict[str, Any]:
    """Read older Main records without changing shared state readers or writing the file."""
    progress = state.get("in_progress")
    paused = state.pop("paused", None)
    if progress and "phase" not in progress:
        reason = progress.pop("failed", None)
        progress["phase"] = "failed" if reason else "deploying"
        if reason:
            progress.update(reason=reason, failed_step=progress.get("step", "unknown"))
    elif not progress and paused:
        phase = "aborting" if paused.get("phase") == "aborting" else "held"
        reason = paused.pop("failed", None)
        paused.update(phase=phase, held_at=paused.pop("since"))
        if reason:
            paused["reason"] = reason
        state["in_progress"] = paused
    return state


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
def release_lock(path: str, *, create: bool = True) -> Iterator[None]:
    if create:
        Path(path).parent.mkdir(parents=True, exist_ok=True)
    flags = (os.O_RDWR | os.O_CREAT) if create else os.O_RDONLY
    try:
        fd = os.open(path, flags, 0o600)
    except FileNotFoundError:
        if create:
            raise
        # No lock has been taken yet on this host. A read-only plan need not create one.
        yield
        return
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


class NoRuntimeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Ops:
    """Every command and request goes through here. In dry-run mode
    mutating commands are printed instead of executed."""

    def __init__(self, dry_run: bool = False, log: Callable[[str], None] | None = None):
        self.dry_run = dry_run
        self.log = log or (lambda msg: print(msg, flush=True))
        self.open_runtime = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRuntimeRedirect()).open

    # commands ---------------------------------------------------------------
    def run(self, args: list[str], *, mutate: bool, timeout: float = 600,
            check: bool = True) -> subprocess.CompletedProcess[str]:
        if self.dry_run and mutate:
            self.log("DRY-RUN " + shlex.join(args))
            return subprocess.CompletedProcess(args, 0, "", "")
        try:
            proc = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired as err:
            raise CommandTimedOut(err) from None
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

    def inspect_snapshot(self, name: str) -> dict[str, Any] | None:
        return self.inspect(name)

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
        # The runtime bearer token is scoped to the original listener.
        open_url = self.open_runtime if token else urllib.request.urlopen
        try:
            with open_url(req, timeout=timeout) as resp:  # noqa: S310 - fixed loopback/config URLs
                raw = resp.read()
                return resp.status, json.loads(raw) if raw.strip().startswith(b"{") else raw.decode(errors="replace")
        except urllib.error.HTTPError as err:
            return err.code, None
        except (urllib.error.URLError, TimeoutError, ConnectionError, OSError,
                http.client.HTTPException, ValueError):
            # A process that exits mid-response leaves a bad status line, a
            # short read or truncated JSON: no usable answer, same as no answer.
            return 0, None

    def probe_nginx_serving(self, path: str, timeout: float) -> tuple[int, str | None, str | None]:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            connection = http.client.HTTPConnection("localhost", timeout=timeout)
            try:
                sock.settimeout(timeout)
                sock.connect(path)
                connection.sock = sock
                connection.request("GET", "/health", headers={"Connection": "close"})
                with connection.getresponse() as response:
                    return response.status, response.getheader("X-Sources-Route"), response.getheader("X-Sources-Upstream")
            except (OSError, http.client.HTTPException):
                return 0, None, None
            finally:
                connection.close()

    def nginx_workers(self, path: str, *, shutting_down: bool = False) -> list[dict[str, Any]]:
        """Identify the workers of the master that owns our Nginx probe socket."""
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as probe:
            probe.settimeout(1)
            probe.connect(path)
            pid, _, _ = struct.unpack("3i", probe.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i")))
        if pid <= 0 or not self.read_text(f"/proc/{pid}/cmdline").startswith("nginx: master process"):
            raise DeployError("cannot identify the Nginx master serving the local probe")
        children = self.read_text(f"/proc/{pid}/task/{pid}/children").split()
        workers = []
        for child in children:
            try:
                name = self.read_text(f"/proc/{child}/cmdline").rstrip("\0").strip()
            except FileNotFoundError:
                continue
            if name.startswith("nginx: worker process") and (not shutting_down or "shutting down" in name):
                process = release_process(self, int(child))
                if process:
                    workers.append(process)
        return workers

    # filesystem / time ------------------------------------------------------------
    def read_text(self, path: str) -> str:
        return Path(path).read_text()

    def read_bytes(self, path: str) -> bytes:
        if not stat.S_ISREG(os.stat(path).st_mode):
            raise DeployError("configuration input must be a regular file. Fix the input or abort and prepare a new hold")
        # A path may be replaced after stat. Opening a FIFO must never wait
        # for a writer, and only an open regular file is safe to read.
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NONBLOCK), "rb") as stream:
            if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                raise DeployError("configuration input must be a regular file. Fix the input or abort and prepare a new hold")
            return stream.read()

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

    def remove_file(self, path: str) -> None:
        if self.dry_run:
            self.log(f"DRY-RUN remove {path}")
            return
        Path(path).unlink(missing_ok=True)

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
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                sock.bind(("127.0.0.1", port))
                # Confirm there is no active listener, including on platforms that allow a second bind.
                sock.listen(1)
            except OSError:
                return False
        return True

    def sleep(self, seconds: float) -> None:
        time.sleep(seconds)

    def monotonic(self) -> float:
        return time.monotonic()


class MainOps(Ops):
    def inspect(self, name: str, *, timeout: float = 30) -> dict[str, Any] | None:
        proc = self.run(["docker", "inspect", name], mutate=False, check=False, timeout=timeout)
        if proc.returncode != 0:
            if re.search(r"^Error(?: response from daemon)?: No such (?:object|container):", proc.stderr, re.M):
                return None
        else:
            try:
                data = json.loads(proc.stdout)
                if isinstance(data, list) and len(data) == 1 and isinstance(data[0], dict) and data[0].get("Id"):
                    return data[0]
            except ValueError:
                pass
        raise DeployError("cannot inspect container; Docker state is unknown; nothing may be removed")

    def docker_socket_path(self) -> str | None:
        """Resolve the Docker CLI's selected endpoint if it uses a Unix socket."""
        config_dir = Path(os.environ.get("DOCKER_CONFIG") or Path.home() / ".docker")
        context = os.environ.get("DOCKER_CONTEXT")
        host = os.environ.get("DOCKER_HOST") or None
        try:
            if not context and not host:
                try:
                    config = json.loads(self.read_text(str(config_dir / "config.json")))
                except FileNotFoundError:
                    config = {}
                context = config.get("currentContext")
            if context and context != "default":
                digest = hashlib.sha256(context.encode()).hexdigest()
                meta = json.loads(self.read_text(str(config_dir / "contexts" / "meta" / digest / "meta.json")))
                if meta.get("Name") != context:
                    return None
                host = meta["Endpoints"]["docker"]["Host"]
                if not host:
                    return None
        except (OSError, ValueError, AttributeError, KeyError, TypeError):
            # Let the CLI resolve configurations we cannot interpret.
            return None
        if host is None:
            return "/var/run/docker.sock"
        if isinstance(host, str) and host.startswith("unix:///"):
            return host.removeprefix("unix://")
        return None

    def inspect_snapshot(self, name: str) -> dict[str, Any] | None:
        # Topology and restart policy need fresh observations in addition to
        # the original deploy commands. Use the local Engine's read-only API.
        path = self.docker_socket_path()
        if path is None:
            return self.inspect(name)
        timeout = 30
        connection = http.client.HTTPConnection("localhost", timeout=timeout)
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
                sock.settimeout(timeout)
                sock.connect(path)
                connection.sock = sock
                escaped = urllib.parse.quote(name, safe="")
                connection.request("GET", f"/containers/{escaped}/json", headers={"Connection": "close"})
                with connection.getresponse() as response:
                    data = json.load(response)
                    if isinstance(data, dict):
                        if response.status == 200 and data.get("Id"):
                            return data
                        if response.status == 404 and str(data.get("message", "")).lower().startswith("no such container:"):
                            return None
        except TimeoutError as err:
            raise CommandTimedOut(subprocess.TimeoutExpired(["Docker", "inspection"], timeout)) from err
        except (OSError, http.client.HTTPException, ValueError):
            pass
        finally:
            connection.close()
        raise DeployError("cannot inspect container; Docker state is unknown; nothing may be removed")


def release_process(ops: Ops, pid: int) -> dict[str, Any] | None:
    """Identify a Linux process across PID reuse and host reboots."""
    try:
        boot_id = ops.read_text("/proc/sys/kernel/random/boot_id").strip()
        stat = ops.read_text(f"/proc/{pid}/stat").rpartition(")")[2].split()
    except FileNotFoundError:
        return None
    # /proc/<pid>/stat fields 3 (state) and 22 (start time), after `comm`.
    if not boot_id or len(stat) <= 19 or stat[0] in ("Z", "X"):
        return None
    return {"pid": pid, "boot_id": boot_id, "started_at_tick": stat[19]}


def release_running(ops: Ops, progress: dict[str, Any]) -> bool | None:
    process = progress.get("process") or {}
    pid = process.get("pid")
    if type(pid) is not int or pid <= 0:
        return False
    try:
        return release_process(ops, pid) == process
    except OSError:
        return None


# --------------------------------------------------------------------------
# Nginx


def render_upstreams(serving_port: int, standby_port: int | None, background_port: int) -> str:
    backup = f"    server 127.0.0.1:{standby_port} backup max_fails=1 fail_timeout=5s;\n" if standby_port is not None else ""
    return f"""# Managed by deploy/sources/deploy.py; rewritten on every switch. Do not edit.
upstream sources_main_serving {{
    zone sources_main_serving 64k;
    server 127.0.0.1:{serving_port} max_fails=1 fail_timeout=5s;
{backup}}}

upstream sources_main_admin {{
    zone sources_main_admin 64k;
    server 127.0.0.1:{background_port};
}}
"""


def render_serving_probe(path: str, revision: str) -> str:
    quoted_path = path.replace("\\", "\\\\").replace('"', '\\"')
    return f"""
# Local verification of the loaded serving upstream after degraded rollback.
server {{
    listen "unix:{quoted_path}";
    access_log off;
    location = /health {{
        {ROUTE_PROBE_PROXY}
        proxy_cache off;
        proxy_intercept_errors off;
        proxy_pass_request_headers off;
        proxy_hide_header X-Sources-Route;
        proxy_hide_header X-Sources-Upstream;
        add_header X-Sources-Route {revision} always;
        add_header X-Sources-Upstream $upstream_addr always;
    }}
}}
"""


LEGACY_UPSTREAM = re.compile(r"sources_main_\d{3}_serving")


def fixed_upstream_routes(text: str) -> dict[str, list[tuple[str, bool, bool]]]:
    """Read just the peers and their primary/backup roles in an adopted file."""
    text = re.sub(r"#[^\n]*", "", text)
    routes = {}
    for name, body in re.findall(r"\bupstream\s+(sources_main_serving|sources_main_admin)\s*\{([^{}]*)\}", text):
        if name in routes:
            raise DeployError("duplicate fixed upstream")
        peers = []
        for directive in body.split(";"):
            parts = shlex.split(directive)
            if len(parts) >= 2 and parts[0] == "server":
                options = parts[2:]
                host, separator, port = parts[1].partition(":")
                address = f"127.0.0.1:{port}" if separator and host.lower() == "localhost" else parts[1]
                peers.append((address, "backup" in options, "down" in options))
        routes[name] = sorted(peers)
    return routes


def nginx_ready_for_fixed_upstreams(nginx_dump: str) -> list[str]:
    """Problems that block switching via the fixed upstream file."""
    # The local verification listener is not the user-facing proxy.
    nginx_dump = nginx_dump.replace(ROUTE_PROBE_PROXY, "")
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
    image_id: str = ""

    def view(self) -> dict[str, Any]:
        view = {"slot": self.slot, "image": self.image, "containers": self.containers, "ports": self.ports,
                "runtime_token_files": self.token_files}
        if self.image_id:
            view["image_id"] = self.image_id
        return view

    @classmethod
    def from_view(cls, view: dict[str, Any], default_token: str) -> "Target":
        tokens = {role: (view.get("runtime_token_files") or {}).get(role) or default_token for role in ROLES}
        return cls(slot=view["slot"], image=view.get("image", ""), containers=dict(view["containers"]),
                   ports=dict(view["ports"]), token_files=tokens, image_id=view.get("image_id", ""))


@dataclass(frozen=True)
class ServingEndpoint:
    role: str
    network: str
    network_ref: str
    aliases: tuple[str, ...]
    options: tuple[str, ...]

    @staticmethod
    def connect_options(endpoint: dict[str, Any]) -> tuple[str, ...]:
        # IPAddress and GlobalIPv6Address can be dynamic leases. IPAMConfig
        # keeps only the addresses explicitly requested when configuring it.
        ipam = endpoint.get("IPAMConfig") or {}
        options = []
        for key, flag in (("IPv4Address", "--ip"), ("IPv6Address", "--ip6")):
            if ipam.get(key):
                options.extend((flag, ipam[key]))
        for address in sorted(ipam.get("LinkLocalIPs") or ()):
            options.extend(("--link-local-ip", address))
        for link in sorted(endpoint.get("Links") or ()):
            options.extend(("--link", link))
        for key, value in sorted((endpoint.get("DriverOpts") or {}).items()):
            options.extend(("--driver-opt", f"{key}={value}"))
        if endpoint.get("GwPriority"):
            options.extend(("--gw-priority", str(endpoint["GwPriority"])))
        return tuple(options)

    @classmethod
    def from_inspect(cls, role: str, network: str, endpoint: dict[str, Any], aliases: tuple[str, ...]) -> "ServingEndpoint":
        return cls(role, network, endpoint.get("NetworkID") or network, aliases, cls.connect_options(endpoint))

    def matches(self, current: dict[str, Any] | None, aliases: tuple[str, ...]) -> bool:
        return (current is not None and set(current.get("Aliases") or ()) == set(aliases)
                and self.connect_options(current) == self.options)


@dataclass(frozen=True)
class MainRoute:
    serving: dict[str, Any]
    admin: dict[str, Any]
    revision: str = field(default_factory=lambda: os.urandom(16).hex())
    excluded_serving: str | None = None

    def serving_roles(self) -> tuple[str, ...]:
        return tuple(role for role in SERVING_ROLES if role != self.excluded_serving)


ALIAS_OVERLAP_REASONS = ("publish_at_create", "alias_handoff")


def alias_overlap_record(reason: str, slots: list[str], stage: str) -> dict[str, Any]:
    """The recorded window in which both slots of this release may advertise the shared alias."""
    return {"reason": reason, "slots": list(slots), "stage": stage, "started_at": now()}


def recorded_alias_overlap(progress: dict[str, Any]) -> dict[str, Any] | None:
    overlap = progress.get("alias_overlap")
    if not isinstance(overlap, dict) or overlap.get("ended_at"):
        return None
    if overlap.get("reason") not in ALIAS_OVERLAP_REASONS:
        return None
    return overlap


def planned_alias_overlap(progress: dict[str, Any], current: dict[str, Any], peer: dict[str, Any]) -> bool:
    overlap = progress.get("alias_overlap")
    if overlap is not None:
        # Only the two slots named by this release's record may share the
        # alias. An absent or closed record always means a conflict.
        if not isinstance(overlap, dict) or recorded_alias_overlap(progress) is None:
            return False
        slots = overlap.get("slots")
        if not isinstance(slots, list) or set(slots) != {current["slot"], peer["slot"]}:
            return False
    elif not (progress.get("publish_aliases_at_create") or progress.get("alias_handoff")):
        # State files written before the record existed carry only these flags.
        return False
    pair = ((progress.get("origin") or {}).get("current"), progress.get("target"))
    containers = {view["slot"]: view["containers"] for view in pair if view}
    return (containers.get(current["slot"]) == current["containers"]
            and containers.get(peer["slot"]) == peer["containers"])


def alias_slot_views(cfg: Config, state: dict[str, Any], progress: dict[str, Any]) -> dict[str, dict[str, Any]]:
    """Every slot the tool can account for, so watch does not miss a third provider."""
    views: dict[str, dict[str, Any]] = {}

    def add(view: Any) -> None:
        if isinstance(view, dict) and view.get("slot") and view.get("containers"):
            views.setdefault(view["slot"], view)

    add(state.get("current"))
    add(state.get("previous"))
    if progress:
        add(progress.get("target"))
        origin = progress.get("origin") or {}
        add(origin.get("current"))
        add(origin.get("previous"))
        hold = progress.get("hold") or {}
        add(hold.get("current"))
        add(hold.get("target"))
    for slot in cfg.slots:
        add(cfg.slot_view(slot))
    return views


class Deployer:
    def __init__(self, cfg: Config, ops: Ops):
        self.cfg = cfg
        self.ops = ops
        self.timings: list[dict[str, Any]] = []

    # helpers ---------------------------------------------------------------
    def log(self, msg: str) -> None:
        self.ops.log(f"[{dt.datetime.now().strftime('%H:%M:%S')}] {msg}")

    def _record_history(self, record: dict[str, Any]) -> None:
        if self.ops.dry_run:
            return
        try:
            append_history(self.cfg, record)
        except (Exception, KeyboardInterrupt) as err:
            # History is secondary to the saved state. The append may have
            # reached disk, so do not retry or pass the error into recovery.
            with contextlib.suppress(OSError):
                print(f"deploy: warning: could not append release history ({type(err).__name__}). "
                      "Check the history file. The saved release state still applies.", file=sys.stderr)

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

    def health(self, port: int) -> bool:
        return self.ops.http_json(f"http://127.0.0.1:{port}/health")[0] == 200

    def recovery_timeout(self, deadline: float | None, maximum: float) -> float:
        if deadline is None:
            return maximum
        remaining = deadline - self.ops.monotonic()
        if remaining <= 0:
            raise DeployError("background recovery deadline reached")
        return min(maximum, remaining)

    def wait_ready(self, port: int, token: str, role: str, timeout: int,
                   container: str | None = None, poll: float = 2, *,
                   check_health: bool = False, deadline: float | None = None) -> dict[str, Any]:
        """Wait for a role to report ready. When the container is given, a
        container that exited or is crash-restarting fails at once instead of
        holding the background gap for the whole timeout. During recovery the
        wait also shares its deadline."""
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
                if (role != "background" or last.get("owns_background")) and (not check_health or self.health(port)):
                    return last
            delay = poll if deadline is None else min(poll, max(0, ready_until - self.ops.monotonic()))
            self.ops.sleep(delay)
        limit = f"within {timeout}s" if deadline is None else "before the recovery deadline"
        raise DeployError(f"port {port} not ready as {role} {limit} (last runtime: {json.dumps(last)[:300]})")

    def inspect_or_missing(self, name: str, *, deadline: float | None = None) -> tuple[dict[str, Any] | None, bool]:
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
            info, removed = self.inspect_or_missing(name, deadline=probe_deadline)
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

    def create(self, target: Target, role: str, *, serving_alias: bool = True,
               created: dict[str, str] | None = None, hold_creation_id: str | None = None,
               record_created: Callable[[str], None] | None = None) -> None:
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
                  if serving_alias and role in SERVING_ROLES and self.cfg.serving_alias else []),
                "--label", f"com.sources.deploy.slot={target.slot}",
                "--label", f"com.sources.deploy.role={role}",
                "--label", f"com.sources.deploy.created={now()}"]
        if hold_creation_id:
            args += ["--label", f"{HOLD_CREATION_LABEL}={hold_creation_id}"]
        args += self.docker_env_args(role)
        args += self.mount_args(self.mounts_for(role, name))
        args.append(target.image_id or target.image)
        proc = self.ops.run(args, mutate=True)
        if (created is not None or record_created is not None) and not self.ops.dry_run:
            cid = proc.stdout.strip()
            if not cid:
                raise FallbackFailed(f"Docker did not identify the newly created {role} container; inspect status")
            # Record ownership before the remaining network connections, which can fail.
            if created is not None:
                created[role] = cid
            if record_created is not None:
                record_created(cid)
        aliases = ["--alias", name]
        if serving_alias and role in SERVING_ROLES and self.cfg.serving_alias:
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

    def install_upstreams(self, text: str, *, before_reload: Callable[[], None] | None = None,
                          reload_started: Callable[[], None] | None = None,
                          recover_reload: Callable[[str | None, BaseException], NoReturn] | None = None) -> None:
        try:
            previous = self.read_upstream()
        except FileNotFoundError:
            previous = None
        except OSError as err:
            raise DeployError("cannot read the previous upstream file; routing unchanged") from err
        reloading = False
        try:
            self.ops.write_file_atomic(self.cfg.upstream_file, text)
            test = self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True, check=False)
            if test.returncode != 0:
                raise DeployError("nginx -t rejected the upstream file")
            if before_reload is not None and not self.ops.dry_run:
                before_reload()
            reloading = True
            if reload_started is not None:
                reload_started()
            reload = self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True, check=False)
            if reload.returncode != 0:
                raise ReloadRejected(reload.returncode, previous)
        except (Exception, KeyboardInterrupt) as err:
            reason = str(err) or type(err).__name__
            if reloading:
                if recover_reload is not None:
                    recover_reload(previous, err)
                if isinstance(err, ReloadRejected):
                    raise
                raise RoutingUnconfirmed(f"reload outcome is unknown ({reason}). Verify live routing with 'status' "
                                         "and Nginx before changing background ownership") from err
            try:
                if previous is None:
                    self.ops.remove_file(self.cfg.upstream_file)
                else:
                    self.ops.write_file_atomic(self.cfg.upstream_file, previous)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise RoutingFileNotRestored("could not restore the previous upstream file after failure before reload") from restore_err
            if previous is None:
                raise RoutingFileNotRestored(f"{reason}; rejected upstream file removed. The previous upstream file is missing") from err
            if isinstance(err, (RollbackBackgroundUnavailable, RoutingUnconfirmed)):
                raise
            raise DeployError(f"{reason}; previous file restored, routing unchanged") from err

    # steps ------------------------------------------------------------------
    def nginx_preflight(self) -> list[str]:
        return nginx_ready_for_fixed_upstreams(
            self.ops.run([self.cfg.nginx_bin, "-T"], mutate=False, timeout=60).stdout)

    def host_preflight(self) -> list[str]:
        available = self.ops.meminfo_available()
        if available and available < self.cfg.min_host_available_gib * GIB:
            return [f"host MemAvailable {available / GIB:.1f}GiB below {self.cfg.min_host_available_gib}GiB"]
        return []

    def public_health_preflight(self, allow_failure: bool) -> list[str]:
        # Use the same request the guard makes after switching.
        status, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
        if status == 200:
            return []
        seen = f"public health check returned status {status} (0 means no response)"
        if allow_failure:
            self.log(f"   WARNING: {seen}; continuing because of --allow-public-health-failure; "
                     "the guard still requires 200 after the switch")
            return []
        return [f"{seen}; the guard after the switch would fail the same way and roll back; "
                "pass --allow-public-health-failure to deploy anyway"]

    def preflight(self, state: dict[str, Any], target: Target, reuse: bool,
                  allow_public_health_failure: bool = False) -> list[str]:
        problems = self.nginx_preflight()
        current = state.get("current")
        if not current:
            problems.append("no current release recorded; run 'adopt' once first")
        elif current.get("slot") == target.slot:
            problems.append(f"target slot {target.slot} is the current slot")
        problems += self.host_preflight()
        if not reuse:
            running = []
            for role, name in target.containers.items():
                info = self.ops.inspect(name)
                if info and info.get("State", {}).get("Running"):
                    running.append(name)
                    problems.append(f"{name} (previous release in slot {target.slot}) is still running/draining")
            if running and current and self.alias_networks(current) and self.alias_networks(target.view()):
                raise RoutingUnconfirmed("the shared alias is active on both slots. Both serving pairs were kept")
            if not running:
                problems += port_problems(self.ops, [target.ports[r] for r in ROLES])
        problems += self.public_health_preflight(allow_public_health_failure)
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

    def create_held_background(self, state: dict[str, Any], target: Target) -> None:
        # The creation label covers a lost Docker response or a failed ID
        # checkpoint. Once Docker gives us the ID, checkpoint it before networking.
        intent = os.urandom(16).hex()
        self._save_progress(state, {**state["in_progress"], "held_background_creation_id": intent})

        def record(cid: str) -> None:
            self._save_progress(state, {**state["in_progress"], "held_background_id": cid})

        self.create(target, "background", hold_creation_id=intent, record_created=record)

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

    def start_background(self, view: dict[str, Any], *, restart: str | None = "unless-stopped",
                         deadline: float | None = None, check_health: bool = False) -> None:
        name = view["containers"]["background"]
        # Once ownership is vacant, Ctrl-C must not stop the Docker clients
        # that put an owner back in service. They inherit the ignored signal.
        with finish_handoff_cleanup(ignore=True):
            if restart is not None:
                # A container just created for this release already carries the
                # policy, and the ordinary release must issue the same writes
                # as before. Only a reused container needs the update.
                self.ops.run(["docker", "update", f"--restart={restart}", name], mutate=True,
                             timeout=self.recovery_timeout(deadline, 600))
            self.ops.run(["docker", "start", name], mutate=True, timeout=self.recovery_timeout(deadline, 600))
            if not self.ops.dry_run:
                self.wait_ready(view["ports"]["background"], self.token_for(view, "background"),
                                "background", self.cfg.background_ready_timeout, container=name,
                                poll=HANDOFF_POLL_SECONDS, check_health=check_health, deadline=deadline)

    def restore_background(self, current: dict[str, Any], failed: DeployError, *, restart: str,
                           recovery: BackgroundRecovery | None = None) -> NoReturn:
        old = current["containers"]["background"]
        try:
            self.start_background(current, restart=restart, deadline=recovery.work_until if recovery else None)
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as restore_err:
            error = FallbackFailed(f"background handoff failed ({failed}); could not restart {old}: {restore_err}; "
                                   "verify and restore background ownership before clearing in_progress")
            if recovery is not None and not self.ops.dry_run:
                # Persist failure before the caller checks for container removal.
                self._record_unrestored(load_state(self.cfg), error)
            raise error from restore_err
        raise BackgroundRestored(f"background handoff failed ({failed}); previous background {old} is running again",
                                 cleanup_deadline=recovery.deadline if recovery else None) from failed

    def handoff_background(self, state: dict[str, Any], current: dict[str, Any], target: Target,
                           create: bool, *, check_health: bool = False, back: bool = False) -> float | None:
        """Move background ownership from current to target. The old owner gets
        one SIGTERM and finishes in-flight work; the kernel lock passes when it
        exits. If the new owner does not become ready, the old one restarts.
        An old owner that reports admin_during_drain keeps answering admin
        reads until it closes its listener, just before it exits. back: the
        handoff returns ownership to target after traffic already moved there.
        Return the active recovery deadline, if any, for subsequent cleanup."""
        old = current["containers"]["background"]
        new = target.containers["background"]
        progress = state.get("in_progress") or {}
        before = self.owner_runtime(current)  # read-only, also in a dry run
        if self.ops.dry_run:
            old_info, old_removed = self.ops.inspect(old), False
        else:
            # The policy capture needs one fresh observation. Read it over the
            # local Engine's API so the ordinary release keeps the same
            # command sequence; only a failed read falls back to the CLI check
            # that decides whether the container is really gone.
            try:
                old_info = self.ops.inspect_snapshot(old)
                old_removed = old_info is None
            except (DeployError, OSError, ValueError, subprocess.SubprocessError):
                # An unknown read falls back to the CLI check, which consults
                # the container list before deciding the container is gone.
                old_info, old_removed = self.inspect_or_missing(old)
        if old_info is None and not old_removed:
            raise DeployError(f"could not confirm the state of background container {old} before handoff")
        policies = progress.get("background_restart_policies", {})
        if old_info is not None and old not in policies:
            policy = (old_info.get("HostConfig") or {}).get("RestartPolicy")
            if policy is None:
                raise DeployError("cannot read the original background restart policy")
            policies = {**policies, old: self.background_restart_policy(old_info)}
            # Retain the original policy through switch failure and abort.
            self._save_progress(state, {**progress, "background_restart_policies": policies})
        old_state = (old_info or {}).get("State") or {}
        old_running = old_info is not None and bool(old_state.get("Running") or old_state.get("Restarting"))
        restart = policies.get(old) or self.background_restart_policy(old_info)
        disabled = False
        if old_running:
            # Disable the old owner's restart before creating the successor so
            # a failed creation cannot leave two owners. The policy is restored
            # below, or by 'abort', if this step fails.
            try:
                self.ops.run(["docker", "update", "--restart=no", old], mutate=True)
                disabled = True
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                removed = False
                with contextlib.suppress(OSError, subprocess.SubprocessError):
                    removed = self.ops.container_missing(old)
                if removed:
                    # Docker confirmed the old owner is gone; there is nothing
                    # to drain and the successor can take ownership.
                    old_running = False
                    old_info = None
                else:
                    reason = f"could not disable automatic restart of {old}: {err}."
                    self.restore_old_background_policy(old, restart, reason,
                                                       "Inspect the release before clearing in_progress.")
                    raise DeployError(f"{reason} Its restart policy was restored.") from err
        try:
            if create:
                if progress.get("hold"):
                    self.create_held_background(state, target)
                else:
                    self.create(target, "background")
            else:
                # Prepare the stopped rollback container's stored policy before
                # the active owner changes.
                self.ops.run(["docker", "update", f"--restart={policies.get(new, 'unless-stopped')}", new],
                             mutate=True)
        except (Exception, KeyboardInterrupt) as err:
            if disabled:
                self.restore_old_background_policy(
                    old, restart, f"could not prepare the successor background {new}: {err}.",
                    "Inspect the release before clearing in_progress.")
            raise
        view = target.view()
        # Protect the transition before changing the restart policy or sending
        # TERM. Installing the handler after a timeout leaves a signal gap.
        with finish_handoff_cleanup() as interrupted:
            recovery = None
            old_state = (old_info or {}).get("State") or {}
            if old_running:
                recovery = self.drain_background(current, old_info, before, successor=new, back=back,
                                                 interrupted=interrupted, target_view=view,
                                                 policy_disabled=disabled)
            else:
                self.log(f"   {old} is not running. Starting the prepared background {new}.")
            try:
                self.start_background(view, restart=None if create else policies.get(new, "unless-stopped"),
                                      check_health=check_health,
                                      deadline=recovery.work_until if recovery else None)
            except (DeployError, OSError, subprocess.SubprocessError) as err:
                if recovery is not None:
                    raise FallbackFailed(f"could not make {new} ready during background recovery: {err}") from err
                self.log(f"   new background failed: {err}. Restoring {old} once {new} exits.")
                new_info, new_removed = self.inspect_or_missing(new)
                if new_info is not None:
                    try:
                        recovery = self.drain_background(view, new_info, self.owner_runtime(view), successor=old,
                                                         back=True, interrupted=interrupted, target_view=current)
                    except (DeployError, OSError, subprocess.SubprocessError) as stop_err:
                        raise FallbackFailed(f"new background {new} did not become ready ({err}). "
                                             f"Could not finish stopping it before restoring {old}: {stop_err}") from stop_err
                elif not new_removed:
                    raise FallbackFailed(f"could not confirm the state of new background {new} before restoring {old}") from err
                self.restore_background(current, DeployError(f"new background did not become ready: {err}"),
                                        restart=self.background_restart_policy(old_info), recovery=recovery)
            return recovery.deadline if recovery is not None else None

    def restore_old_background_policy(self, old: str, restart: str, reason: str, follow_up: str,
                                      deadline: float | None = None) -> None:
        """Put the original restart policy back, or persist why a person must."""
        try:
            # A terminal Ctrl-C reaches the Docker CLI too. The child inherits
            # SIG_IGN for this cleanup command.
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

    def drain_background(self, current: dict[str, Any], old_info: dict[str, Any],
                         before: dict[str, Any] | None, successor: str, back: bool,
                         interrupted: Callable[[], bool], target_view: dict[str, Any],
                         policy_disabled: bool = False) -> BackgroundRecovery | None:
        old = current["containers"]["background"]
        restart = self.background_restart_policy(old_info)

        idle = [target_view["containers"][role] for role in SERVING_ROLES
                if (target_view.get("containers") or {}).get(role)]
        follow_up = (f"{old} is still draining after {self.cfg.background_exit_timeout}s; nothing was killed and "
                     f"{successor} was not started. Traffic is on slot {current['slot']}. "
                     f"Its restart policy is restored; if it is not running, start it. "
                     f"Serving containers left outside the Nginx route: {', '.join(idle) or 'none'}. "
                     f"Check container state and verify {old} has exited, then start the background of slot "
                     f"{current['slot']}: 'docker update --restart={restart} {old}' and 'docker start {old}'; "
                     "check 'status' and use 'abort' if the record must be cleared. Do not deploy again before that.")

        def restore_restart_policy(reason: str, deadline: float | None = None) -> None:
            self.restore_old_background_policy(old, restart, reason, follow_up, deadline)

        if not policy_disabled:
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
                f"{reason} Its restart policy may still need to be restored. {follow_up}",
                cleanup_deadline=deadline))
            restore_restart_policy(reason, deadline)
            return FallbackFailed(f"{reason} Its restart policy was restored. {follow_up}",
                                  cleanup_deadline=self.ops.monotonic())

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
                # Restore the policy within the remaining budget so a later
                # exit brings the admin back. If the budget is gone, the
                # combined failure says so and a person must restore it.
                unresolved = restore_policy_after_timeout(
                    f"could not confirm exit of {old} after the handoff and recovery waits.",
                    deadline=recovery.deadline)
                # A final poll can still catch an exit near the end of recovery.
                recovery = BackgroundRecovery(recovery.deadline, failure_recorded=True)
                try:
                    exited = self.wait_recovery_exit(old, recovery.deadline)
                except BackgroundExitTimeout as cleanup_err:
                    raise unresolved from cleanup_err
            except KeyboardInterrupt:
                # An operator interrupt is not proof that the owner exited.
                # Put the policy back so an unexpected restart is deliberate,
                # then report the unconfirmed exit.
                unresolved = FallbackFailed(
                    f"could not confirm exit of {old} after the handoff and recovery waits. "
                    f"Its restart policy was restored within the recovery window. {follow_up}")
                with contextlib.suppress(DeployError, OSError, subprocess.SubprocessError):
                    restore_restart_policy(f"could not confirm exit of {old} after an interrupt.",
                                           recovery.deadline)
                recovery = BackgroundRecovery(recovery.deadline, failure_recorded=True)
                try:
                    exited = self.wait_recovery_exit(old, recovery.deadline)
                except (BackgroundExitTimeout, KeyboardInterrupt):
                    raise unresolved
        except (Exception, OSError, subprocess.SubprocessError) as err:
            if self.ops.dry_run:
                raise
            # An unexpected error means the owner's state is unknown. Never
            # leave the background without an owner; put the old one back if
            # it is not answering, then report the original failure.
            try:
                if not self.background_answering(current):
                    self.start_background(current, restart=restart)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise FallbackFailed(f"background handoff failed ({err}); restoring {old} failed too "
                                     f"({restore_err}); verify and restore background ownership before "
                                     "clearing in_progress") from err
            raise
        if exited.timed_out_at is not None:
            failure = BackgroundExitTimeout(old, exited.timed_out_at)
            recovery = BackgroundRecovery(exited.timed_out_at + HANDOFF_RECOVERY_SECONDS)
        code = exited.code
        if not back and code is not None and (failure is not None or interrupted()):
            try:
                self.restore_background(current, failure or DeployError("handoff interrupted"), restart=restart,
                                        recovery=recovery)
            except FallbackFailed:
                _, removed = self.inspect_or_missing(old, deadline=recovery.deadline if recovery else None)
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

    def guard(self, target: Target) -> None:
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
                raise DeployError(reason)
            if self.ops.monotonic() >= deadline:
                return
            self.ops.sleep(self.cfg.guard_interval)

    def serving_info(self, view: dict[str, Any], role: str,
                     observed: dict[str, dict[str, Any]] | None) -> dict[str, Any]:
        name = view["containers"][role]
        if observed is None:
            return self.ops.inspect(name) or {}
        if name not in observed:
            observed[name] = self.ops.inspect_snapshot(name) or {}
        return observed[name]

    def check_rollback_serving(self, view: dict[str, Any], *, observed: dict[str, dict[str, Any]] | None = None,
                               roles: tuple[str, ...] = SERVING_ROLES) -> None:
        for role in roles:
            info = self.serving_info(view, role, observed)
            if not info.get("State", {}).get("Running") or not self.health(view["ports"][role]):
                raise DeployError(f"rollback {role} on slot {view['slot']} is not answering health checks")

    def background_answering(self, view: dict[str, Any], *, allow_draining: bool = False,
                             deadline: float | None = None) -> bool:
        runtime = self.owner_runtime(view, deadline=deadline) or {}
        responding = runtime.get("ready") or (allow_draining and runtime.get("draining")
                                               and runtime.get("admin_during_drain") == "read-only")
        return bool(runtime.get("role") == "background" and runtime.get("owns_background") and responding
                    and self.health(view["ports"]["background"]))

    def check_rollback_route(self, route: MainRoute, *, allow_draining: bool = False,
                             require_standby: bool = True, observed: dict[str, dict[str, Any]] | None = None,
                             allow_missing_background: bool = False,
                             allow_unconfirmed_background: bool = False,
                             allow_unready_background: bool = False) -> None:
        roles = route.serving_roles()
        if not require_standby and "serving" in roles:
            roles = ("serving",)
        self.check_rollback_serving(route.serving, observed=observed, roles=roles)
        runtime = self.owner_runtime(route.admin)
        answering = ((runtime or {}).get("ready")
                     or (allow_unready_background and (runtime or {}).get("role") == "background")
                     or (allow_draining and (runtime or {}).get("draining")
                         and (runtime or {}).get("admin_during_drain") == "read-only"))
        healthy = self.health(route.admin["ports"]["background"])
        owner = ((runtime or {}).get("role") == "background" and (runtime or {}).get("owns_background")
                 and answering and (healthy or allow_unready_background))
        if not owner:
            if allow_unconfirmed_background and runtime is None:
                # A deploy replaces this background next. An unreadable owner
                # cannot be confirmed, but the handoff itself refuses to start
                # a second owner and recovers if the old one does not exit.
                return
            if allow_missing_background and self.ops.container_missing(
                    route.admin["containers"]["background"]):
                # An operator or a failed container already removed the rollback
                # background; the new release creates one. This is not a second
                # source of truth for the shared alias, so it never blocks.
                return
            raise RollbackBackgroundUnavailable(
                f"rollback background on slot {route.admin['slot']} is not answering health checks as owner")

    def route_probe_path(self) -> str:
        socket_name = hashlib.sha256(os.fsencode(self.cfg.upstream_file)).hexdigest()[:12] + ".sock"
        path = str(Path(self.cfg.state_file).absolute().with_name(socket_name))
        if len(os.fsencode(path)) <= 103 and all(ord(ch) >= 32 for ch in path):
            return path
        # Keep the listener local when state is on a deeply nested mount.
        digest = hashlib.sha256(os.fsencode(os.path.abspath(self.cfg.upstream_file))).hexdigest()[:24]
        return f"/run/sources-nginx-{digest}.sock"

    def route_text(self, route: MainRoute) -> str:
        ports = [route.serving["ports"][role] for role in route.serving_roles()]
        text = render_upstreams(ports[0], ports[1] if len(ports) == 2 else None, route.admin["ports"]["background"])
        return text + render_serving_probe(self.route_probe_path(), route.revision) if route.revision else text

    def alias_networks(self, view: dict[str, Any], *, retiring: dict[str, Any] | None = None,
                       roles: tuple[str, ...] = SERVING_ROLES,
                       observed: dict[str, dict[str, Any]] | None = None) -> set[tuple[str, str]]:
        if not view or not self.cfg.serving_alias:
            return set()
        advertised = set()
        for role in roles:
            info = self.serving_info(view, role, observed)
            state = info.get("State", {})
            if not (state.get("Running") or state.get("Restarting")):
                continue
            if retiring and retiring.get(role) == {"id": info.get("Id"), "started_at": state.get("StartedAt")}:
                policy = (info.get("HostConfig") or {}).get("RestartPolicy") or {}
                if not state.get("Restarting") and policy.get("Name") == "no":
                    continue
            for network, endpoint in info.get("NetworkSettings", {}).get("Networks", {}).items():
                if endpoint.get("EndpointID", network) and self.cfg.serving_alias in (endpoint.get("Aliases") or ()):
                    advertised.add((network, endpoint.get("NetworkID") or network))
        return advertised

    def verify_topology(self, state: dict[str, Any], route: MainRoute, peer: dict[str, Any], *,
                        disk: MainRoute | None = None,
                        ready: MainRoute | None = None, check_health: bool = True, allow_draining: bool = False,
                        wait: bool = False, bootstrap: bool = False, require_standby: bool = True,
                        observed: dict[str, dict[str, Any]] | None = None,
                        allow_missing_background: bool = False,
                        allow_unconfirmed_background: bool = False,
                        allow_unready_background: bool = False) -> None:
        """Check the written route, loaded generation, alias owner and readiness for a transition.

        Before reload, `disk` and `ready` describe the destination, while the
        worker must still report the original route. Its health may have failed.
        Checks of a route that is already serving may allow an offline standby.
        """
        if self.ops.dry_run:
            return
        observed = {} if observed is None else observed
        expected = self.route_text(disk or route)
        bootstrap = bootstrap or not route.revision
        try:
            if bootstrap and disk is None:
                matches = fixed_upstream_routes(self.read_upstream()) == fixed_upstream_routes(expected)
            else:
                matches = self.upstream_fingerprint() == hashlib.sha256(expected.encode()).hexdigest()
            if not matches:
                raise DeployError("upstream file does not match the expected topology")
            if not bootstrap:
                addresses = {f"127.0.0.1:{route.serving['ports'][role]}" for role in route.serving_roles()}
                deadline = self.ops.monotonic() + ROUTE_PROBE_TIMEOUT
                matching_since = None
                while True:
                    remaining = deadline - self.ops.monotonic()
                    status, revision, address = self.ops.probe_nginx_serving(self.route_probe_path(), min(max(remaining, 0.01), 1))
                    backend = (address or "").split(",")[-1].strip()
                    # On recovery, a worker can prove its generation even if
                    # its application is down. The destination still has to answer.
                    no_peer = not backend or (status >= 500 and backend == "sources_main_serving")
                    matches = (revision == route.revision and (backend in addresses or (not check_health and no_peer))
                               and (status == 200 or (not check_health and status > 0)))
                    if matches:
                        sampled = self.ops.monotonic()
                        if not wait or (matching_since is not None and sampled - matching_since >= HANDOFF_POLL_SECONDS):
                            break
                        if matching_since is None:
                            matching_since = sampled
                    else:
                        # Workers from both generations can accept briefly on
                        # the same socket. A stale response restarts the check.
                        matching_since = None
                    if not wait or remaining <= 0:
                        raise DeployError("the loaded Nginx generation or upstream response did not match")
                    self.ops.sleep(min(remaining, HANDOFF_POLL_SECONDS))
                if self.upstream_fingerprint() != hashlib.sha256(expected.encode()).hexdigest():
                    raise DeployError("upstream file changed during route verification")
            if (peer and peer["slot"] != route.serving["slot"]
                    and not planned_alias_overlap(state.get("in_progress") or {}, route.serving, peer)
                    and self.alias_networks(route.serving, observed=observed)
                    and self.alias_networks(peer, observed=observed)):
                raise DeployError("the shared alias is active on both slots")
        except (CommandTimedOut, subprocess.TimeoutExpired):
            # The caller knows whether the check preceded any slot mutations.
            raise
        except Exception as err:
            raise RoutingUnconfirmed(f"serving route unconfirmed ({err or type(err).__name__}). Both serving pairs were kept") from err
        if ready is not None or check_health:
            candidate = ready or route
            self.check_rollback_route(candidate, allow_draining=allow_draining,
                                      require_standby=require_standby, observed=observed,
                                      allow_missing_background=allow_missing_background,
                                      allow_unconfirmed_background=allow_unconfirmed_background,
                                      allow_unready_background=allow_unready_background)

    def current_route(self, state: dict[str, Any], current: dict[str, Any], peer: dict[str, Any], *,
                      install_probe: bool = True) -> MainRoute:
        """Check the recorded route, optionally adding a probe to existing installations."""
        if self.ops.dry_run:
            return MainRoute(current, current)
        try:
            text = self.read_upstream()
        except OSError as err:
            raise RoutingUnconfirmed("cannot read the previous upstream file; routing unchanged") from err
        marker = ROUTE_REVISION.search(text)
        if marker:
            route = MainRoute(current, current, marker[1])
            self.verify_topology(state, route, peer, check_health=False)
        else:
            plain = MainRoute(current, current, "")
            self.verify_topology(state, plain, peer, bootstrap=True, check_health=install_probe, require_standby=False)
            if not install_probe:
                # Ordinary deploy installs the probe as part of its switch.
                return plain
            route = MainRoute(current, current)
            # This reload only adds the probe to the same serving/admin route.
            self.apply_route(state, plain, route, peer, require_standby=False)
        return route

    def apply_route(self, state: dict[str, Any], active: MainRoute, pending: MainRoute, peer: dict[str, Any], *,
                    allow_draining: bool = False, require_standby: bool = True,
                    reload_started: Callable[[], None] | None = None) -> None:
        progress = state.get("in_progress")
        if progress and progress.get("origin") and not self.ops.dry_run:
            self._save_progress(state, {**progress, "verified_route": asdict(active), "pending_route": asdict(pending)})
        attempted = False
        reload_error = None
        same_serving = active.serving["slot"] == pending.serving["slot"]

        def start_reload() -> None:
            nonlocal attempted
            attempted = True
            if reload_started is not None:
                reload_started()

        def check_before_reload() -> None:
            self.verify_topology(state, active, peer if same_serving else pending.serving, disk=pending, ready=pending,
                                 check_health=False, allow_draining=allow_draining, bootstrap=not active.revision,
                                 require_standby=require_standby)
            if active.revision and progress and progress.get("origin"):
                older = list(progress.get("nginx_previous_workers", ()))
                for worker in self.ops.nginx_workers(self.route_probe_path()):
                    marked = {**worker, "revision": active.revision}
                    if marked not in older:
                        older.append(marked)
                self._save_progress(state, {**state["in_progress"], "nginx_previous_workers": older})

        def confirm_and_record() -> None:
            rejected = reload_error if isinstance(reload_error, ReloadRejected) else None
            new_generation = False
            try:
                self.verify_topology(state, pending, peer if same_serving else active.serving,
                                     wait=True, allow_draining=allow_draining, check_health=rejected is None,
                                     require_standby=require_standby)
                new_generation = True
                if rejected:
                    # A loaded generation may be unhealthy. It still rules
                    # out handing the background back to the old route here.
                    self.check_rollback_route(pending, allow_draining=allow_draining, require_standby=require_standby)
            except Exception as err:
                if rejected and not new_generation:
                    self.restore_rejected_reload(state, active, pending, peer if same_serving else pending.serving, rejected)
                reason = str(err) or type(err).__name__
                if reload_error is not None:
                    reason = f"{reload_error}. {reason}"
                raise RoutingUnconfirmed(f"serving route unconfirmed ({reason}). Both serving pairs were kept") from err
            self.record_live_route(state, pending)

        try:
            try:
                self.install_upstreams(self.route_text(pending), before_reload=check_before_reload,
                                       reload_started=start_reload)
            except RoutingUnconfirmed as err:
                if not attempted:
                    raise
                reload_error = err
            if self.ops.dry_run:
                return
            confirm_and_record()
        except KeyboardInterrupt:
            # A signal may arrive after the probe succeeded but before the
            # proof reached state. Recheck once so the caller can roll back.
            proof = (state.get("in_progress") or {}).get("verified_route") or {}
            if attempted and not self.ops.dry_run and proof.get("revision") != pending.revision:
                try:
                    confirm_and_record()
                except KeyboardInterrupt as err:
                    raise RoutingUnconfirmed("serving route verification interrupted again. Both serving pairs were kept") from err
            raise
        if reload_error is not None and isinstance(reload_error.__cause__, KeyboardInterrupt):
            raise KeyboardInterrupt

    def restore_rejected_reload(self, state: dict[str, Any], active: MainRoute, pending: MainRoute,
                                peer: dict[str, Any], rejected: ReloadRejected) -> None:
        previous = rejected.previous
        if not active.revision or previous is None or previous != self.route_text(active):
            return
        try:
            # The old background stopped during handoff. Only prove which
            # workers are answering and that the attempted file is unchanged.
            self.verify_topology(state, active, peer, disk=pending, check_health=False, wait=True)
        except DeployError:
            return
        try:
            self.ops.write_file_atomic(self.cfg.upstream_file, previous)
        except (Exception, KeyboardInterrupt) as err:
            raise RoutingFileNotRestored(f"{rejected}; old route is still loaded, but its file could not be restored") from err
        raise ReloadNotApplied(f"{rejected}; previous upstream file restored, routing unchanged") from rejected

    def wait_for_nginx_drain(self, state: dict[str, Any], active: MainRoute) -> None:
        if self.ops.dry_run or not active.revision:
            return
        deadline = self.ops.monotonic() + self.cfg.ready_timeout
        progress = state.get("in_progress") or {}
        retiring = [{key: value for key, value in worker.items() if key != "revision"}
                    for worker in progress.get("nginx_previous_workers", ()) if worker["revision"] != active.revision]
        while True:
            # Include earlier workers still serving existing connections, even
            # when the first release started without a probe or a prior process
            # was interrupted before it could record a generation.
            shutting_down = self.ops.nginx_workers(self.route_probe_path(), shutting_down=True)
            if not any(release_running(self.ops, {"process": worker}) is not False for worker in retiring + shutting_down):
                break
            if self.ops.monotonic() >= deadline:
                raise DrainTimeout("previous Nginx workers still have connections; serving retirement did not start")
            self.ops.sleep(HANDOFF_POLL_SECONDS)
        if "nginx_previous_workers" in progress:
            self._save_progress(state, {key: value for key, value in progress.items() if key != "nginx_previous_workers"})

    def record_live_route(self, state: dict[str, Any], route: MainRoute) -> None:
        progress = state.get("in_progress")
        if not progress or not progress.get("origin"):
            return
        origin = progress["origin"]
        original = route.serving["slot"] == origin["current"]["slot"]
        # The live route has already changed. Keep the proof in memory even
        # when this write fails so the caller can restore the original route.
        state["in_progress"] = {**progress, "verified_route": asdict(route), "pending_route": None}
        update = {"current": origin["current"] if original else route.serving,
                  "previous": origin["previous"] if original else origin["current"]}
        if not self.ops.dry_run:
            save_state(self.cfg, {**state, **update})
        state.update(update)

    def rollback_to(self, state: dict[str, Any], active: MainRoute, target: Target, *, recovery: bool = False,
                    alias_plan: dict[str, list[str]] | None = None,
                    recovery_window: list[float | None] | None = None, back: bool = False) -> MainRoute:
        current = active.admin
        view = target.view()
        if not self.ops.dry_run:
            self.verify_topology(state, active, view, check_health=False)
            self.check_rollback_serving(view)
        self._mark(state, target, "background handoff")

        def handoff() -> None:
            preparing = True
            try:
                if not self.ops.dry_run:
                    info = self.ops.inspect(target.containers["background"])
                    if not info:
                        raise DeployError("rollback background container is missing")
                    if not info.get("State", {}).get("Running") and not self.ops.port_free(target.ports["background"]):
                        raise DeployError("rollback background port is already in use")
                preparing = False
                deadline = self.handoff_background(state, current, target, create=False, check_health=True, back=back)
                if recovery_window is not None:
                    recovery_window.append(deadline)
            except (Exception, KeyboardInterrupt) as err:
                if self.ops.dry_run or (preparing and not recovery):
                    raise
                self.route_rollback_serving_only(state, active, view, err, alias_plan=alias_plan)

        return self.handoff_and_switch(state, active, target, handoff, rollback=True, alias_plan=alias_plan)

    def route_rollback_serving_only(self, state: dict[str, Any], active: MainRoute, view: dict[str, Any],
                                    err: BaseException, *, alias_plan: dict[str, list[str]] | None = None) -> NoReturn:
        pending = MainRoute(view, active.admin)
        attempted = False

        def start_reload() -> None:
            nonlocal attempted
            attempted = True

        try:
            if not self.background_answering(active.admin, allow_draining=True):
                raise SwitchNotRestored("no answering background for the loaded admin route. Wait for any draining owner to exit, "
                                        "restore one background, then retry 'abort'")
            self.apply_route(state, active, pending, view, allow_draining=True, reload_started=start_reload)
        except RoutingUnconfirmed:
            raise
        except (Exception, KeyboardInterrupt) as route_err:
            if attempted:
                raise RoutingUnconfirmed(f"serving-only switch interrupted ({route_err or type(route_err).__name__}). "
                                         "Both serving pairs were kept") from route_err
            raise SwitchNotRestored(f"rollback background unavailable ({err}). Switching serving alone failed "
                                    f"({route_err or type(route_err).__name__}). Run 'status' and verify the route "
                                    "before clearing in_progress. Use 'abort' to restore the original route") from route_err
        try:
            self.retire_and_transfer(state, pending, active.serving, alias_plan=alias_plan, allow_draining=True)
        except (Exception, KeyboardInterrupt) as retire_err:
            err = DeployError(f"{err}. Serving retirement incomplete ({retire_err or type(retire_err).__name__})")
        raise RollbackIncomplete(f"rollback background unavailable ({err}). Serving uses slot {view['slot']} and admin "
                                 f"uses slot {active.admin['slot']}. Use 'status' or 'abort'") from err

    def restore_after_guard(self, state: dict[str, Any], active: MainRoute, previous: dict[str, Any], reason: str) -> None:
        self.log(f"   guard failed: {reason}; switching traffic back to slot {previous['slot']}")
        window: list[float | None] = []
        try:
            route = self.rollback_to(state, active, Target.from_view(previous, self.cfg.runtime_token_file),
                                     recovery=True, recovery_window=window, back=True)
            deadline = window[0] if window else None
            protect = finish_handoff_cleanup(ignore=True) if deadline is not None else contextlib.nullcontext()
            with protect:
                try:
                    self.retire_and_transfer(state, route, active.serving, deadline=deadline,
                                             require_known_state=deadline is not None)
                except ServingCleanupFailed:
                    raise
                except (Exception, KeyboardInterrupt) as cleanup_err:
                    if deadline is not None:
                        raise ServingCleanupFailed(f"guard failed ({reason}); the previous release serves traffic and "
                                               f"owns the background again. Incomplete serving cleanup: "
                                               f"{cleanup_err}. Check the target slot before clearing "
                                               "in_progress.") from cleanup_err
                    raise
        except RollbackIncomplete as err:
            raise RollbackIncomplete(f"guard failed ({reason}); {err}") from err
        except ServingCleanupFailed:
            raise
        except (Exception, KeyboardInterrupt) as err:
            raise FallbackFailed(f"guard failed ({reason}) and switching back failed ({err or type(err).__name__})") from err

    def hand_back(self, state: dict[str, Any], target: Target, previous: dict[str, Any]) -> float | None:
        """Return background ownership to the release that still serves traffic."""
        try:
            return self.handoff_background(state, target.view(), Target.from_view(previous, self.cfg.runtime_token_file),
                                           create=False, check_health=True, back=True)
        except FallbackFailed:
            raise
        except (Exception, KeyboardInterrupt) as err:
            raise FallbackFailed(f"handing the background back failed: {err}") from err

    def restore_loaded_background(self, state: dict[str, Any], displaced: Target, loaded: dict[str, Any],
                                  reason: str) -> float | None:
        deadline = None
        try:
            if not self.background_answering(loaded):
                deadline = self.hand_back(state, displaced, loaded)
            if not self.background_answering(loaded):
                raise DeployError("the background selected by the loaded route did not confirm health and ownership")
        except (Exception, KeyboardInterrupt) as restore_err:
            raise SwitchNotRestored(f"{reason}. Restoring the background selected by the loaded route failed ({restore_err}). "
                                    "Wait for other owners to exit, then restore the restart policy, "
                                    "use 'docker start' and verify ownership or retry 'abort'") from restore_err
        return deadline

    def handoff_and_switch(self, state: dict[str, Any], active: MainRoute, target: Target, handoff: Callable[[], None], *,
                           rollback: bool = False, alias_plan: dict[str, list[str]] | None = None) -> MainRoute:
        view = target.view()
        current = active.admin
        pending = MainRoute(view, view)
        handoff_started = handoff_complete = reload_attempted = False

        def mark_reload_attempted() -> None:
            nonlocal reload_attempted
            reload_attempted = True

        self.verify_topology(state, active, view, check_health=False, bootstrap=not active.revision)
        try:
            with self.step("background handoff"):
                handoff_started = True
                handoff()
                handoff_complete = True
            with self.step("switch"):
                self._mark(state, target, "switch")
                self.apply_route(state, active, pending, view, reload_started=mark_reload_attempted)
            return pending
        except (Exception, KeyboardInterrupt) as err:
            if self.ops.dry_run or isinstance(err, RollbackIncomplete):
                raise
            if isinstance(err, RoutingUnconfirmed) and (reload_attempted or not handoff_complete):
                # The degraded rollback can attempt its own reload during handoff.
                raise
            if reload_attempted and not isinstance(err, (ReloadNotApplied, RoutingFileNotRestored)):
                # A verified route is saved before leaving this range. A
                # subsequent state/logging failure is recovered by the caller.
                proof = (state.get("in_progress") or {}).get("verified_route") or {}
                if proof.get("revision") == pending.revision:
                    raise
                raise RoutingUnconfirmed(f"switch interrupted after starting reload ({err or type(err).__name__})") from err
            if not handoff_started:
                raise
            cleanup_deadline = getattr(err, "cleanup_deadline", None)
            if (not handoff_complete and isinstance(err, DeployError)
                    and not self.background_answering(view, deadline=cleanup_deadline)):
                raise
            loaded_bg = (current.get("containers") or {}).get("background")
            loaded_info = self.ops.inspect(loaded_bg) if loaded_bg else None
            loaded_state = (loaded_info or {}).get("State") or {}
            if (loaded_bg and (loaded_state.get("Running") or loaded_state.get("Restarting"))
                    and not self.background_answering(current, deadline=cleanup_deadline)):
                # The loaded route's owner is still running (usually still
                # draining after the failed handoff). It holds the lock until
                # it exits; restarting it or restoring its policy now would
                # create a surprise owner. The error carries instructions.
                raise
            reason = str(err) or type(err).__name__
            if isinstance(err, OSError):
                reason = f"switch state write or upstream I/O failed ({reason})"
            with contextlib.suppress(OSError, KeyboardInterrupt):
                self.log("   switch failed with the original route still loaded; handing the background back")
            deadline = self.restore_loaded_background(state, target, current, reason)
            if rollback and isinstance(err, RollbackBackgroundUnavailable):
                self.route_rollback_serving_only(state, active, view, err, alias_plan=alias_plan)
            if isinstance(err, RoutingFileNotRestored):
                raise SwitchNotRestored(f"{reason}; the original background owns the lock again. "
                                        "Verify the upstream file before reloading Nginx",
                                        cleanup_deadline=deadline) from err
            raise DeployError(f"{reason}; the previous release owns the background again",
                              cleanup_deadline=deadline) from err

    def container_memory(self, info: dict[str, Any]) -> int | None:
        cid = info.get("Id")
        if not cid:
            return None
        path = self.cfg.cgroup_memory_path.format(id=cid)
        try:
            return int(self.ops.read_text(path).strip())
        except (OSError, ValueError):
            return None

    def alias_views(self, state: dict[str, Any], extra: tuple[dict[str, Any], ...] = ()) -> list[dict[str, Any]]:
        """Every slot this process can inspect, including the one being retired."""
        progress = state.get("in_progress") or {}
        views = alias_slot_views(self.cfg, state, progress)
        for view in extra:
            if isinstance(view, dict) and view.get("slot") and view.get("containers"):
                views.setdefault(view["slot"], view)
        return list(views.values())

    def alias_provider_map(self, views: list[dict[str, Any]]) -> tuple[dict[tuple[str, str], set[str]], bool]:
        """Per network, the live container IDs advertising the alias. Confirmed is
        False when any read failed: the caller must then keep a possible provider."""
        providers: dict[tuple[str, str], set[str]] = {}
        confirmed = True
        seen = set()
        for view in views:
            for role in SERVING_ROLES:
                name = (view.get("containers") or {}).get(role)
                if not name or name in seen:
                    continue
                seen.add(name)
                try:
                    info = self.ops.inspect(name) or {}
                except (DeployError, OSError, subprocess.SubprocessError):
                    confirmed = False
                    continue
                current = info.get("State") or {}
                if not (current.get("Running") or current.get("Restarting")):
                    continue
                identity = info.get("Id") or name
                for network, endpoint in (info.get("NetworkSettings") or {}).get("Networks", {}).items():
                    if (endpoint.get("EndpointID", network)
                            and self.cfg.serving_alias in (endpoint.get("Aliases") or ())):
                        key = (network, endpoint.get("NetworkID") or network)
                        providers.setdefault(key, set()).add(identity)
        return providers, confirmed

    def sole_alias_network(self, name: str, state: dict[str, Any],
                           extra: tuple[dict[str, Any], ...] = ()) -> tuple[str, str] | None:
        """The network where this exact instance is the only live alias provider.

        An unreadable instance or a failed read of another slot is treated as
        reason to keep it: the state file must not overrule live evidence.
        """
        if not self.cfg.serving_alias:
            return None
        try:
            info = self.ops.inspect(name) or {}
        except (DeployError, OSError, subprocess.SubprocessError):
            return ("unknown", "unknown")
        if not info:
            try:
                if self.ops.container_missing(name):
                    return None
            except (DeployError, OSError, subprocess.SubprocessError):
                pass
            return ("unknown", "unknown")
        current = info.get("State") or {}
        if not (current.get("Running") or current.get("Restarting")):
            return None
        identity = info.get("Id") or name
        networks = [(network, endpoint.get("NetworkID") or network)
                    for network, endpoint in (info.get("NetworkSettings") or {}).get("Networks", {}).items()
                    if endpoint.get("EndpointID", network)
                    and self.cfg.serving_alias in (endpoint.get("Aliases") or ())]
        if not networks:
            return None
        providers, confirmed = self.alias_provider_map(self.alias_views(state, extra))
        if not confirmed:
            return networks[0]
        for key in networks:
            if providers.get(key) == {identity}:
                return key
        return None

    def require_not_sole_alias_provider(self, view: dict[str, Any], state: dict[str, Any]) -> None:
        """Refuse to stop or reconfigure an instance that is a network's only alias provider."""
        for role in SERVING_ROLES:
            name = (view.get("containers") or {}).get(role)
            if not name:
                continue
            network = self.sole_alias_network(name, state, extra=(view,))
            if network is not None:
                where = f"network {network[0]}" if network[0] != "unknown" else "a network (state unconfirmed)"
                raise SoleAliasProviderKept(f"{name} is the only serving instance advertising the shared alias on "
                                        f"{where}; kept running. Stop it only after another slot provides the alias")

    @staticmethod
    def connect_errors(log_text: str, ports: set[str]) -> int:
        count = 0
        for line in log_text.splitlines():
            if "connect() failed" in line or "upstream timed out" in line:
                if any(f"127.0.0.1:{p}" in line for p in ports):
                    count += 1
        return count

    def retire_serving(self, view: dict[str, Any], *, state: dict[str, Any] | None = None,
                       deadline: float | None = None,
                       require_known_state: bool = False) -> dict[str, dict[str, str]]:
        signalled = {}
        for role in SERVING_ROLES:
            name = view["containers"].get(role)
            if not name:
                continue
            token = None
            with contextlib.suppress(OSError):
                token = self.token_for(view, role)
            if require_known_state:
                info, removed = self.inspect_or_missing(name, deadline=deadline)
                if removed:
                    continue
            else:
                info = self.ops.inspect(name, timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
            running = ((info or {}).get("State") or {}).get("Running")
            if require_known_state and not isinstance(running, bool):
                raise DeployError(f"could not confirm the state of serving container {name}")
            if not running:
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
            started = info["State"].get("StartedAt")
            if info.get("Id") and started:
                signalled[role] = {"id": info["Id"], "started_at": started}
                if state is not None and (state.get("in_progress") or {}).get("publish_aliases_at_create"):
                    retired = {"slot": view["slot"], "containers": dict(signalled), "recorded_at": now()}
                    self._save_progress(state, {**state["in_progress"], "retiring_serving": retired})
            self.ops.run(["docker", "kill", "--signal=TERM", name], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))
        return signalled

    def stop_owned_serving(self, target: Target, role: str, cid: str, *, allow_missing: bool = False,
                           wait: bool = True, deadline: float | None = None) -> bool:
        """Disable restarts and let this exact serving container finish current requests."""
        name = target.containers[role]
        info = self.ops.inspect(name)
        if not info and allow_missing:
            return False
        if not info or info.get("Id") != cid:
            raise DeployError(f"{role} was removed or replaced; refusing to modify an unrelated container")
        self.ops.run(["docker", "update", "--restart=no", cid], mutate=True,
                     timeout=self.recovery_timeout(deadline, 600))
        if info.get("State", {}).get("Running"):
            token = None
            with contextlib.suppress(OSError):
                token = self.token_for(target.view(), role)
            runtime = self.runtime(target.ports[role], token) if token else None
            if (runtime and runtime.get("instance_id") and not runtime.get("draining")
                    and not self.ops.dry_run):
                self.ops.http_json(f"http://127.0.0.1:{target.ports[role]}/internal/sources/drain", token=token,
                                   method="POST", body={"instance_id": runtime["instance_id"]})
            # The drain endpoint only marks readiness. Serving's SIGTERM handler
            # owns the graceful shutdown, including for containers already draining.
            try:
                self.ops.run(["docker", "kill", "--signal=TERM", cid], mutate=True,
                             timeout=self.recovery_timeout(deadline, 600))
            except DeployError:
                if (self.ops.inspect(cid) or {}).get("State", {}).get("Running"):
                    raise
            if wait and not self.ops.dry_run:
                self.wait_exit(cid, self.ops.monotonic() + self.cfg.ready_timeout, poll=HANDOFF_POLL_SECONDS)
        return True

    def drain_serving_group(self, target: Target, identities: dict[str, str], *,
                            deadline: float | None = None) -> None:
        # Signal both before waiting. A stalled stream on the primary must
        # not leave the standby accepting new requests through the alias.
        failures = []
        signaled = []
        timed_out = False
        for role, cid in identities.items():
            try:
                if self.stop_owned_serving(target, role, cid, allow_missing=True, wait=False, deadline=deadline):
                    signaled.append((role, cid))
            except (Exception, KeyboardInterrupt) as err:
                failures.append(f"{role}: {err or type(err).__name__}")
        if not self.ops.dry_run:
            for role, cid in signaled:
                try:
                    exit_deadline = self.ops.monotonic() + self.cfg.ready_timeout
                    if deadline is not None:
                        exit_deadline = min(exit_deadline, deadline)
                    self.wait_exit(cid, exit_deadline, poll=HANDOFF_POLL_SECONDS)
                except (DrainTimeout, BackgroundExitTimeout) as err:
                    timed_out = True
                    failures.append(f"{role}: {err}")
                except (Exception, KeyboardInterrupt) as err:
                    failures.append(f"{role}: {err or type(err).__name__}")
        if failures:
            failure = DrainTimeout if timed_out else DeployError
            raise failure("; ".join(failures))

    def remove_owned_serving(self, target: Target, role: str, cid: str, *,
                             deadline: float | None = None) -> None:
        name = target.containers[role]
        if self.stop_owned_serving(target, role, cid, allow_missing=True, deadline=deadline):
            self.ops.run(["docker", "rm", cid], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))
        if not self.ops.dry_run and self.ops.inspect(name):
            raise DeployError(f"{name} still exists")

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

    @staticmethod
    def require_idle(state: dict[str, Any]) -> None:
        progress = state.get("in_progress") or {}
        if progress.get("phase") == "held":
            raise DeployError("a release is paused; run 'resume' or 'abort' first (see 'status')")
        if progress.get("step") == "prepare held slot":
            raise DeployError("held slot preparation did not finish; retry 'resume' or 'abort' first (see 'status')")
        if progress.get("phase") == "aborting":
            raise DeployError("abort cleanup has started; retry 'abort' first (see 'status')")
        if progress:
            raise DeployError(f"a previous deploy did not finish: {json.dumps(progress)}; "
                              "inspect with 'status' and resolve before deploying again")

    def deploy(self, image: str, allow_contract: bool = False,
               allow_public_health_failure: bool = False, hold_before_switch: bool = False,
               allow_running_backup: bool = False) -> None:
        state = load_main_state(self.cfg)
        self.require_idle(state)
        if state.get("current"):
            self.check_running_backup(state["current"], allow_running_backup)
        self._deploy(state, image, allow_contract, allow_public_health_failure, hold_before_switch)

    def _deploy(self, state: dict[str, Any], image: str, allow_contract: bool,
                allow_public_health_failure: bool, hold_before_switch: bool) -> None:
        current = state.get("current") or {}
        preparation = state.get("in_progress") or {}
        slot = "a" if current.get("slot") != "a" else "b"
        target = Target.from_view({**self.cfg.slot_view(slot), "image": image}, self.cfg.runtime_token_file)
        record: dict[str, Any] = {"started_at": now(), "action": "deploy", "image": image, "slot": slot}
        created: dict[str, str] = {}
        try:
            with self.step("pull image"):
                present = self.ops.run(["docker", "image", "inspect", image], mutate=False, check=False, timeout=30)
                if present.returncode != 0:
                    self.ops.run(["docker", "pull", image], mutate=False, timeout=1800)
            with self.step("preflight"):
                input_fingerprint = self.input_fingerprint() if hold_before_switch else None
                pending = self.preflight(state, target, reuse=False,
                                         allow_public_health_failure=allow_public_health_failure)
                contract = [p for p in pending if "contract" in p]
                if contract and hold_before_switch:
                    raise DeployError("a held release requires additive migrations; contract migrations are not allowed")
                if contract and not allow_contract:
                    raise DeployError(f"contract migrations need --allow-contract: {contract}; nothing changed")
                self.log(f"   pending migrations: {pending or 'none'}")
            self._mark(state, target, "migrate")
            # Ordinary deployment rechecks both slots just before reusing one.
            peer = target.view() if hold_before_switch else {}
            active = self.current_route(state, current, peer, install_probe=False)
            publish_at_create = not hold_before_switch and bool(self.cfg.serving_alias)
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
            if hold_before_switch:
                self.prepare_held_slot(state, target, allow_public_health_failure)
            self.verify_topology(state, active, target.view(), bootstrap=not active.revision,
                                 allow_draining=True, require_standby=False, allow_missing_background=True,
                                 allow_unconfirmed_background=True, allow_unready_background=True)
            # Save an identity for the creation itself, including the window
            # in which Docker creates a container but does not return its ID.
            hold_creation_id = os.urandom(16).hex() if hold_before_switch else None
            self._mark(state, target, "start serving", publish_aliases_at_create=publish_at_create,
                       hold_creation_id=hold_creation_id)
            with self.step("start serving"):
                if not hold_before_switch:
                    for role in ROLES:
                        self.ops.run(["docker", "rm", "-f", target.containers[role]], mutate=True, check=False)
                for role in SERVING_ROLES:
                    self.create(target, role, serving_alias=publish_at_create, created=created,
                                hold_creation_id=hold_creation_id)
                    if hold_creation_id:
                        self._save_progress(state, {**state["in_progress"], "owned": dict(created)})
                    self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                        self.cfg.ready_timeout, check_health=True)
            if hold_before_switch:
                self._hold(state, target, record, allow_public_health_failure, input_fingerprint)
            else:
                self._save_progress(state, {**state["in_progress"], "owned": dict(created)})
                self._finish_switch(state, current, target, record, create_background=True, active=active)
            if skip_check:
                self.log("note: this dry run did NOT verify the serving and background environment files "
                         "(startup check skipped because migrations are pending)")
        except (DeployError, subprocess.TimeoutExpired) as caught:
            err = CommandTimedOut(caught) if isinstance(caught, subprocess.TimeoutExpired) else caught
            progress = state.get("in_progress") or {}
            if isinstance(err, CommandTimedOut) and (hold_before_switch or progress.get("step") not in (None, "migrate")):
                self._unexpected(err, record, owned=created, hold=progress.get("hold"))
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            if isinstance(err, RoutingUnconfirmed) and not state.get("in_progress") and not self.ops.dry_run:
                self._mark(state, target, "preflight")
            if state.get("in_progress") and created:
                state["in_progress"]["owned"] = dict(created)
            self._abort(state, target, err, created, retry_preparation=preparation if preparation else None)
            self._record_history(record)
            if err is not caught:
                raise err from None
            raise
        except (OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            step = (state.get("in_progress") or {}).get("step")
            if not hold_before_switch and step in (None, "migrate"):
                # Keep ordinary preparation errors outside the hold and switch recovery paths.
                raise
            self._unexpected(err, record, owned=created, hold=(state.get("in_progress") or {}).get("hold"))

    def rollback(self, allow_running_backup: bool = False) -> None:
        self._rollback(allow_running_backup)

    def _rollback(self, allow_running_backup: bool = False) -> None:
        state = load_main_state(self.cfg)
        current = state.get("current")
        previous = state.get("previous")
        # Refuse before any bookkeeping write so a refused command leaves the
        # state byte-for-byte unchanged.
        if current:
            self.check_running_backup(current, allow_running_backup)
        # A finished window is audit state, not a slot change: close it even
        # when the release is then refused.
        self.close_retired_alias_overlap(state)
        self.require_idle(state)
        if not current or not previous:
            raise DeployError("no previous release recorded; nothing to roll back to")
        for role in ROLES:
            if not self.ops.inspect(previous["containers"][role]):
                if role == "background":
                    err = DeployError("rollback background container is missing")
                else:
                    err = DeployError(f"previous container {previous['containers'][role]} no longer exists")
                # Nothing was changed, but the refusal must stay visible and
                # alert until a person restores the previous release.
                if not self.ops.dry_run and not state.get("in_progress"):
                    reason = str(err)
                    self._save_progress(state, {
                        "phase": "failed", "target": previous, "step": "prepare rollback serving",
                        "since": now(), "failed_step": "prepare rollback serving",
                        "reason": reason, "failed": reason, "failed_at": now()})
                raise err
        target = Target.from_view(previous, self.cfg.runtime_token_file)
        record: dict[str, Any] = {"started_at": now(), "action": "rollback", "image": target.image, "slot": target.slot}
        identities: dict[str, str] = {}
        endpoints: list[ServingEndpoint] = []
        try:
            self._mark(state, target, "prepare rollback serving")
            active = self.current_route(state, current, previous)
            if self.cfg.serving_alias and self.alias_networks(current):
                # Starting the rollback slot re-registers its stored alias while
                # the current slot still serves; record that handoff window.
                self.record_alias_overlap(state, "alias_handoff", [current["slot"], target.slot],
                                          "prepare rollback serving")
            with self.step("start previous serving"):
                self.prepare_rollback_serving(current, target, identities, endpoints,
                                              previous.get("withdrawn_serving_alias_networks", {}))
            self._save_progress(state, {**state["in_progress"], "candidate_ids": dict(identities)})
            alias_plan = {role: [ep.network for ep in endpoints if ep.role == role] for role in SERVING_ROLES} if endpoints else None
            self._finish_switch(state, current, target, record, create_background=False, active=active, alias_plan=alias_plan)
        except (Exception, KeyboardInterrupt) as err:
            if state.get("in_progress") and identities:
                state["in_progress"]["candidate_ids"] = dict(identities)
            def stop_candidates() -> None:
                self.verify_topology(state, active, target.view(), require_standby=False)
                self.drain_serving_group(target, identities)
                # Leave the original endpoint configuration on the stopped
                # containers for a retry after the operator clears the fault.
                self.configure_rollback_aliases(target, identities, endpoints, shared=True)

            step = (state.get("in_progress") or {}).get("step")
            unselected = step == "prepare rollback serving" or self.switch_left_serving_unselected(step, err)
            self.fail_switch_operation(state, record, err, cleanup=stop_candidates if identities and unselected else None)

    def prepare_rollback_serving(self, current: dict[str, Any], target: Target, identities: dict[str, str],
                                 endpoints: list[ServingEndpoint], withdrawn: dict[str, list[str]]) -> None:
        if (set(current["containers"].values()) & set(target.containers.values())
                or set(current["ports"].values()) & set(target.ports.values())):
            raise DeployError("rollback target overlaps the current release")
        if not self.ops.inspect(target.containers["background"]):
            raise DeployError("rollback background container is missing")
        candidates = {}
        for role in SERVING_ROLES:
            name = target.containers[role]
            info = self.ops.inspect(name) or {}
            if not info.get("Id"):
                raise DeployError(f"previous container {name} no longer exists")
            candidates[role] = info["Id"]
            networks = info.get("NetworkSettings", {}).get("Networks", {})
            restore_alias = set(withdrawn.get(role, ())) if self.cfg.serving_alias else set()
            if restore_alias.difference(networks):
                raise DeployError(f"previous {role} is missing networks required for its shared alias")
            for network, endpoint in networks.items():
                aliases = tuple(endpoint.get("Aliases") or ())
                if network in restore_alias and self.cfg.serving_alias not in aliases:
                    aliases += (self.cfg.serving_alias,)
                if self.cfg.serving_alias in aliases:
                    endpoints.append(ServingEndpoint.from_inspect(role, network, endpoint, aliases))
        if endpoints:
            roles_with_alias = {endpoint.role for endpoint in endpoints}
            identities.update((role, cid) for role, cid in candidates.items() if role in roles_with_alias)
            self.drain_serving_group(target, identities)
            self.configure_rollback_aliases(target, identities, endpoints, shared=False)
        for role, cid in candidates.items():
            identities[role] = cid
            self.ops.run(["docker", "update", "--restart=unless-stopped", cid], mutate=True)
            self.ops.run(["docker", "start", cid], mutate=True)
        if not self.ops.dry_run:
            for role, cid in candidates.items():
                self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                self.cfg.ready_timeout, container=cid, check_health=True)

    def record_alias_overlap(self, state: dict[str, Any], reason: str, slots: list[str],
                             stage: str, **extra: Any) -> None:
        """Record the handoff window in `in_progress` so `status` and `watch` can audit it."""
        base = state.get("in_progress") or {}
        if not base or len(set(slots)) != 2:
            return
        progress = {**base, **extra}
        record = recorded_alias_overlap(base)
        if record is not None and set(record.get("slots") or ()) == set(slots):
            # Keep the start of a window that is still open.
            if extra:
                self._save_progress(state, progress)
            return
        self._save_progress(state, {**progress, "alias_overlap": alias_overlap_record(reason, slots, stage)})

    def end_alias_overlap(self, state: dict[str, Any], slots: list[str]) -> None:
        """Close the recorded window after the retiring slot no longer advertises."""
        progress = state.get("in_progress") or {}
        record = recorded_alias_overlap(progress)
        if record is None or set(record.get("slots") or ()) != set(slots):
            return
        self._save_progress(state, {**progress, "alias_overlap": {**record, "ended_at": now()}})

    def observed_alias_overlap(self, record: Any) -> Any:
        """Return the window with `ended_at` once every recorded instance has exited."""
        if not isinstance(record, dict) or record.get("ended_at"):
            return record
        retiring = record.get("retiring") or {}
        containers = retiring.get("containers") or {}
        slot = retiring.get("slot")
        names = retiring.get("names") or {}
        if not names and slot:
            try:
                names = self.cfg.slot_view(slot).get("containers") or {}
            except KeyError:
                # An adopted slot such as "legacy" is not in the configured slots.
                names = {}
        if not containers or not names:
            return record
        for role, identity in containers.items():
            name = names.get(role)
            if not name:
                continue
            try:
                info = self.ops.inspect(name) or {}
            except (DeployError, OSError, subprocess.SubprocessError):
                return record
            if info.get("Id") and identity.get("id") and info["Id"] != identity["id"]:
                # The recorded instance was replaced, so it has exited.
                continue
            state = info.get("State") or {}
            if state.get("Running") or state.get("Restarting"):
                return record
        return {**record, "ended_at": now()}

    def close_retired_alias_overlap(self, state: dict[str, Any]) -> None:
        """Persist the end of an ordinary-release window once its instances have exited."""
        record = state.get("alias_overlap")
        closed = self.observed_alias_overlap(record)
        if closed == record:
            return
        state["alias_overlap"] = closed
        if not self.ops.dry_run:
            save_state(self.cfg, state)

    def retire_and_transfer(self, state: dict[str, Any], route: MainRoute, retiring: dict[str, Any], *,
                            alias_plan: dict[str, list[str]] | None = None, allow_draining: bool = False,
                            deadline: float | None = None, require_known_state: bool = False) -> None:
        # Fresh reloads can briefly answer on both worker generations.
        self.verify_topology(state, route, retiring, allow_draining=allow_draining, wait=True)
        if self.ops.dry_run:
            self.retire_serving(retiring)
            return
        old_target = Target.from_view(retiring, self.cfg.runtime_token_file)
        new_target = Target.from_view(route.serving, self.cfg.runtime_token_file)
        old_ids: dict[str, str] = {}
        new_ids: dict[str, str] = {}
        old_aliases: list[ServingEndpoint] = []
        new_aliases: list[ServingEndpoint] = []
        for target, identities, endpoints in ((old_target, old_ids, old_aliases), (new_target, new_ids, new_aliases)):
            for role in SERVING_ROLES:
                info = self.ops.inspect(target.containers[role]) or {}
                if not info:
                    continue
                identities[role] = info["Id"]
                networks = info.get("NetworkSettings", {}).get("Networks", {})
                needed = set((alias_plan or {}).get(role, ())) if target is new_target else set()
                if needed.difference(networks):
                    raise FallbackFailed("target networks for the shared alias are missing. Both serving pairs were kept")
                for network, endpoint in networks.items():
                    aliases = tuple(endpoint.get("Aliases") or ())
                    if self.cfg.serving_alias and network in needed and self.cfg.serving_alias not in aliases:
                        aliases += (self.cfg.serving_alias,)
                    if self.cfg.serving_alias in aliases:
                        endpoints.append(ServingEndpoint.from_inspect(role, network, endpoint, aliases))

        if not self.alias_networks(retiring).issubset({(ep.network, ep.network_ref) for ep in new_aliases}):
            raise FallbackFailed("rollback target does not provide the shared alias on all required networks. "
                                 "Both serving pairs were kept")
        if old_aliases:
            plan = {
                role: [ep.network for ep in old_aliases if ep.role == role] for role in SERVING_ROLES
            }
            retiring["withdrawn_serving_alias_networks"] = plan
            progress = state.get("in_progress") or {}
            if progress and progress["origin"]["current"]["slot"] == retiring["slot"]:
                progress["origin"]["current"]["withdrawn_serving_alias_networks"] = plan
                progress.setdefault("original_endpoints", [asdict(endpoint) for endpoint in old_aliases])
                progress.setdefault("original_serving_ids", old_ids)
                self._save_progress(state, progress)
            # The retiring slot still advertises while the receiving slot is
            # registered or verified. Record the window before anything moves.
            self.record_alias_overlap(state, "alias_handoff", [retiring["slot"], route.serving["slot"]],
                                      "publish shared alias" if alias_plan else "retire previous serving",
                                      alias_handoff=True)
        self.wait_for_nginx_drain(state, route)
        expected_aliases = {(ep.network, ep.network_ref) for ep in new_aliases}
        if alias_plan:
            try:
                route = self.publish_routed_aliases(state, route, retiring, new_ids, new_aliases, allow_draining=allow_draining)
                if old_aliases:
                    self.ops.sleep(ALIAS_DNS_GRACE_SECONDS)
                self.wait_for_nginx_drain(state, route)
                self.verify_topology(state, route, retiring, allow_draining=allow_draining, wait=True)
                if not expected_aliases.issubset(self.alias_networks(route.serving)):
                    raise DeployError("the shared alias is not active on all expected networks")
            except (Exception, KeyboardInterrupt) as err:
                raise AliasPublicationFailed(str(err) or type(err).__name__) from err
        if old_aliases:
            # Confirm coverage before anything is stopped: a gap discovered
            # afterwards would already have removed the last provider.
            if not self.alias_networks(retiring).issubset(self.alias_networks(route.serving)):
                raise FallbackFailed("the receiving slot does not provide the shared alias on every "
                                     "network. Both serving pairs were kept")
        if deadline is not None:
            # PR60's bounded recovery cleanup: never touch the last live provider.
            self.require_not_sole_alias_provider(retiring, state)
        self.retire_serving(retiring, state=state, deadline=deadline, require_known_state=require_known_state)
        if old_aliases:
            # Let in-flight requests finish before removing the old alias
            # configuration, including from stopped containers that can restart.
            for cid in old_ids.values():
                exit_deadline = self.ops.monotonic() + self.cfg.ready_timeout
                if deadline is not None:
                    exit_deadline = min(exit_deadline, deadline)
                self.wait_exit(cid, exit_deadline, poll=HANDOFF_POLL_SECONDS)
            self.configure_rollback_aliases(old_target, old_ids, old_aliases, shared=False)
            self.end_alias_overlap(state, [retiring["slot"], route.serving["slot"]])

    def publish_routed_aliases(self, state: dict[str, Any], route: MainRoute, retiring: dict[str, Any],
                               identities: dict[str, str], endpoints: list[ServingEndpoint], *,
                               allow_draining: bool = False) -> MainRoute:
        self.verify_topology(state, route, retiring, allow_draining=allow_draining, wait=True)
        if self.alias_networks(retiring) and not planned_alias_overlap(state["in_progress"], route.serving, retiring):
            raise RoutingUnconfirmed("the shared alias was not withdrawn from the old slot")
        target = Target.from_view(route.serving, self.cfg.runtime_token_file)
        active = route
        for role in reversed(SERVING_ROLES):
            selected = [endpoint for endpoint in endpoints if endpoint.role == role]
            if not selected:
                continue
            info = self.serving_info(route.serving, role, None)
            if info.get("Id") != identities[role]:
                raise DeployError(f"cannot identify the selected {role} for shared alias publication")
            networks = info.get("NetworkSettings", {}).get("Networks", {})
            if all(endpoint.matches(networks.get(endpoint.network), endpoint.aliases) for endpoint in selected):
                continue
            if active.excluded_serving != role:
                remaining = MainRoute(route.serving, route.admin, excluded_serving=role)
                self.apply_route(state, active, remaining, retiring, allow_draining=allow_draining)
                active = remaining
            self.wait_for_nginx_drain(state, active)
            cid = identities[role]
            # Let the routed peer accept new work; this instance still has all
            # its endpoints until the requests already on it finish.
            self.stop_owned_serving(target, role, cid)
            self.configure_rollback_aliases(target, identities, selected, shared=True)
            self.ops.run(["docker", "start", cid], mutate=True)
            self.wait_ready(target.ports[role], self.token_for(route.serving, role), "serving", self.cfg.ready_timeout,
                            container=cid, check_health=True)
            self.ops.run(["docker", "update", "--restart=unless-stopped", cid], mutate=True)
        if active.excluded_serving is not None:
            complete = MainRoute(route.serving, route.admin)
            self.apply_route(state, active, complete, retiring, allow_draining=allow_draining)
            active = complete
        return active

    def configure_rollback_aliases(self, target: Target, identities: dict[str, str],
                                   endpoints: list[ServingEndpoint], *, shared: bool) -> None:
        if self.ops.dry_run:
            return
        for endpoint in endpoints:
            cid = identities[endpoint.role]
            info = self.ops.inspect(target.containers[endpoint.role]) or {}
            state = info.get("State", {})
            if info.get("Id") != cid:
                raise DeployError(f"cannot safely configure the rollback {endpoint.role} network")
            aliases = tuple(alias for alias in endpoint.aliases if shared or alias != self.cfg.serving_alias)
            current = info.get("NetworkSettings", {}).get("Networks", {}).get(endpoint.network)
            if endpoint.matches(current, aliases):
                continue
            if state.get("Running") or state.get("Restarting"):
                raise DeployError(f"cannot reconnect the {endpoint.role} network until it stops")
            if current is not None:
                self.ops.run(["docker", "network", "disconnect", endpoint.network_ref, cid], mutate=True)
            self.connect_serving_endpoint(endpoint, cid, aliases)

    def connect_serving_endpoint(self, endpoint: ServingEndpoint, cid: str, aliases: tuple[str, ...]) -> None:
        options = [arg for alias in aliases for arg in ("--alias", alias)]
        self.ops.run(["docker", "network", "connect", *options, *endpoint.options, endpoint.network_ref, cid], mutate=True)
        if not self.ops.dry_run:
            info = self.ops.inspect(cid) or {}
            actual = info.get("NetworkSettings", {}).get("Networks", {}).get(endpoint.network)
            if not endpoint.matches(actual, aliases):
                raise DeployError(f"{endpoint.role} network configuration is unconfirmed")

    def config_fingerprint(self) -> str:
        return hashlib.sha256(json.dumps(asdict(self.cfg), sort_keys=True).encode()).hexdigest()

    @staticmethod
    def mounted_path(mounts: list[Mount], container_path: str) -> Path | None:
        # The release image runs from /app. Prefer the most specific bind,
        # e.g. a file bind over a directory mounted at its parent.
        path = posixpath.normpath(posixpath.join("/app", container_path))
        for mount in sorted(mounts, key=lambda m: len(posixpath.normpath(m.target)), reverse=True):
            target = posixpath.normpath(mount.target)
            if path == target or path.startswith(target.rstrip("/") + "/"):
                return Path(mount.source) / posixpath.relpath(path, target)
        return None

    def input_fingerprint(self) -> str:
        return self.fingerprint_with_timeout(self.read_input_fingerprint, "configuration")

    @staticmethod
    def fingerprint_with_timeout(read: Callable[[], str], kind: str) -> str:
        # A regular file on a stalled mount can still block in stat/open/read.
        # The worker only reads and hashes inputs. It cannot keep the release
        # lock after the caller times out and closes the lock descriptor.
        result: queue.Queue[str | Exception | KeyboardInterrupt] = queue.Queue(maxsize=1)

        def read_inputs() -> None:
            try:
                result.put(read())
            except (Exception, KeyboardInterrupt) as err:
                result.put(err)

        threading.Thread(target=read_inputs, daemon=True).start()
        try:
            value = result.get(timeout=FINGERPRINT_TIMEOUT)
        except queue.Empty:
            raise DeployError(f"{kind} fingerprint read timed out. Check the input files and mounts") from None
        if isinstance(value, (Exception, KeyboardInterrupt)):
            raise value
        return value

    def read_input_fingerprint(self) -> str:
        digest = hashlib.sha256()
        for role in ("serving", "background"):
            try:
                content = self.ops.read_bytes(self.cfg.env_files[role])
                digest.update(hashlib.sha256(content).digest())
                env: dict[str, str] = {}
                inherited: dict[str, str | None] = {}
                for line in content.decode("utf-8-sig").split("\n"):
                    key, sep, value = line.removesuffix("\r").lstrip().partition("=")
                    if not key or key.startswith("#"):
                        continue
                    if sep:
                        env[key] = value
                        inherited.pop(key, None)
                    else:
                        host_value = os.environ.get(key)
                        inherited[key] = host_value
                        if host_value is not None:
                            env[key] = host_value
                overrides = self.cfg.extra_env.get(role, {})
                env.update(overrides)
                for key in (*overrides, "SOURCES_PROCESS_ROLE"):
                    inherited.pop(key, None)
                if inherited:
                    # Docker resolves bare keys in --env-file against its
                    # inherited environment. Keep unset distinct from empty.
                    digest.update(hashlib.sha256(json.dumps(inherited, sort_keys=True).encode()).digest())

                mounts = self.cfg.mounts["common"] + self.cfg.mounts[role]
                files: set[Path] = set()
                for mount in mounts:
                    source = Path(mount.source)
                    if source.is_file():
                        files.add(source)
                    elif source.is_dir():
                        if mount.readonly and not mount.per_container_copy:
                            files.update(path for path in source.rglob("*") if not path.is_dir())
                    elif source.exists():
                        files.add(source)
                for key, value in env.items():
                    if key.endswith("_FILE") and value.strip():
                        path = self.mounted_path(mounts, value.strip())
                        if path is not None:
                            files.add(path)
                if not env.get("CONFIG_FILE", "").strip():
                    # Without an explicit config, the app also looks for config.*
                    # in its data and default config directories. Do not hash the
                    # rest of a writable data directory or live cache.
                    for directory in (env.get("DATA_DIR", ""), "/app/data", "/app", "/app/config", "/etc/sub2api"):
                        if directory.strip():
                            for ext in ("yaml", "yml", "json", "toml", "ini", "properties", "props", "prop",
                                        "hcl", "tfvars", "dotenv", "env", "xml"):
                                path = self.mounted_path(mounts, posixpath.join(directory.strip(), f"config.{ext}"))
                                if path is not None and path.exists():
                                    files.add(path)
                for path in sorted(files):
                    digest.update(hashlib.sha256(os.fsencode(path)).digest())
                    digest.update(hashlib.sha256(self.ops.read_bytes(str(path))).digest())
            except (OSError, ValueError) as err:
                raise DeployError(f"cannot read the {role} environment or mounted configuration; "
                                  "check access and abort and prepare a new hold") from err
        return digest.hexdigest()

    def check_held_environment(self, held: dict[str, Any]) -> None:
        if not held.get("input_fingerprint"):
            raise DeployError("pause predates mounted configuration verification; abort and prepare a new hold")
        if self.input_fingerprint() != held["input_fingerprint"]:
            raise DeployError("environment files changed, inherited environment changed, or mounted configuration changed during pause; "
                              "abort and prepare a new hold")

    def read_upstream(self) -> str:
        def text() -> str:
            return self.ops.read_bytes(self.cfg.upstream_file).decode().replace("\r\n", "\n").replace("\r", "\n")

        return self.fingerprint_with_timeout(text, "upstream")

    def upstream_fingerprint(self) -> str:
        def digest() -> str:
            # Match the newline handling of the original text fingerprint.
            content = self.ops.read_bytes(self.cfg.upstream_file).replace(b"\r\n", b"\n").replace(b"\r", b"\n")
            return hashlib.sha256(content).hexdigest()

        return self.fingerprint_with_timeout(digest, "upstream")

    def prepare_held_slot(self, state: dict[str, Any], target: Target, allow_public_health_failure: bool) -> None:
        # Record this before removing anything. There are no held containers to clean up yet.
        self._save_progress(state, self._with_process({
            "phase": "deploying", "step": "prepare held slot", "target": target.view(), "since": now(),
            "current": state["current"], "allow_public_health_failure": allow_public_health_failure,
        }))
        try:
            current_names = set(state["current"]["containers"].values())
            if current_names.intersection(target.containers.values()):
                raise DeployError("target slot overlaps the current release")
            for name in target.containers.values():
                info = self.ops.inspect(name)
                if info:
                    if info.get("State", {}).get("Running") or info.get("State", {}).get("Restarting"):
                        raise DeployError("target slot still has a running or restarting container")
                    self.ops.run(["docker", "rm", info["Id"]], mutate=True)
                if not self.ops.dry_run and self.ops.inspect(name):
                    raise DeployError("target slot container was not removed")
        except (Exception, KeyboardInterrupt) as err:
            raise FallbackFailed(f"held slot cleanup failed ({err}); retry 'resume' or 'abort'") from err

    def _hold(self, state: dict[str, Any], target: Target, record: dict[str, Any],
              allow_public_health_failure: bool, input_fingerprint: str | None) -> None:
        if not self.ops.dry_run:
            if self.input_fingerprint() != input_fingerprint:
                raise DeployError("environment files or mounted configuration changed during startup validation; prepare a new hold")
            containers = {}
            for role in SERVING_ROLES:
                info = self.ops.inspect(target.containers[role]) or {}
                containers[role] = {"id": info.get("Id"), "image_id": info.get("Image")}
                if not containers[role]["id"] or not containers[role]["image_id"]:
                    raise DeployError("cannot record held container identity")
            image_ids = {c["image_id"] for c in containers.values()}
            if len(image_ids) != 1:
                raise DeployError("held serving images differ; nothing will switch")
            target.image_id = image_ids.pop()
            held = {"phase": "held", "target": target.view(), "containers": containers, "held_at": now(),
                    "current": state["current"], "config_fingerprint": self.config_fingerprint(),
                    "input_fingerprint": input_fingerprint, "serving_alias": self.cfg.serving_alias,
                    "upstream_fingerprint": self.upstream_fingerprint(),
                    "allow_public_health_failure": allow_public_health_failure}
            try:
                self._mark(state, target, "save held release", held=held)
            except (OSError, subprocess.SubprocessError, KeyboardInterrupt):
                # The candidate was validated before attempting the checkpoint.
                # Keep its identity and fingerprints for failure recording.
                state["in_progress"]["hold"] = held
                raise
            self._save_progress(state, held)
            record.update(result="paused", steps=self.timings, finished_at=now())
            self._record_history(record)
        self.log("paused before background handoff; use 'resume' or 'abort'; no automatic expiry")
        for role in SERVING_ROLES:
            self.log(f"   {role}: http://127.0.0.1:{target.ports[role]}/ (access via your tunnel)")

    def held_release(self, state: dict[str, Any]) -> tuple[dict[str, Any], Target]:
        held = state.get("in_progress")
        if not held:
            raise DeployError("no paused release; nothing changed")
        if held.get("step") == "save held release" and held.get("hold"):
            held = held["hold"]
        if held["phase"] == "aborting":
            raise DeployError("abort cleanup has started; retry 'abort', not 'resume'")
        if held["phase"] != "held":
            raise DeployError("an unfinished release needs manual recovery; inspect 'status'; this is not a pause")
        return held, Target.from_view(held["target"], self.cfg.runtime_token_file)

    def has_expected_held_networks(self, info: dict[str, Any], expected: dict[str, str], *, shared: bool) -> bool:
        networks = info.get("NetworkSettings", {}).get("Networks", {})
        return networks.keys() == expected.keys() and all(
            (self.cfg.serving_alias in (endpoint.get("Aliases") or [])) == shared
            for endpoint in networks.values())

    def validate_hold(self, state: dict[str, Any], held: dict[str, Any], target: Target) -> list[ServingEndpoint]:
        if state.get("current") != held["current"] or target.slot == state["current"]["slot"]:
            raise DeployError("current release changed during pause; inspect 'status' before recovery")
        if self.config_fingerprint() != held["config_fingerprint"]:
            raise DeployError("configuration changed during pause; restore it or abort")
        self.check_held_environment(held)
        if self.upstream_fingerprint() != held["upstream_fingerprint"]:
            raise DeployError("upstream file changed during pause; restore it or abort")
        if self.ops.inspect(target.containers["background"]):
            raise DeployError("unexpected target background container; inspect and resolve before resuming")
        serving = {}
        repair = held.get("isolated_endpoints")
        for role in SERVING_ROLES:
            info = self.ops.inspect(target.containers[role]) or {}
            endpoints = info.get("NetworkSettings", {}).get("Networks", {})
            if self.cfg.serving_alias and any(self.cfg.serving_alias in (ep.get("Aliases") or ()) for ep in endpoints.values()):
                raise HoldIsolationChanged(f"held {role} network isolation changed: the shared alias is present")
            identity = held["containers"][role]
            if info.get("Id") != identity["id"] or info.get("Image") != identity["image_id"]:
                raise DeployError(f"held {role} is missing or its identity/image changed; restore it or abort")
            serving[role] = info
            status = info.get("State", {})
            if repair:
                if status.get("Running") or status.get("Restarting"):
                    raise DeployError(f"{role} must be stopped to restore its recorded isolation")
                continue
            runtime = self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                      self.cfg.ready_timeout, container=target.containers[role])
            if runtime.get("owns_background") or runtime.get("draining"):
                raise DeployError(f"held {role} is not an ordinary serving instance")
        current = state["current"]
        self.wait_ready(current["ports"]["background"], self.token_for(current, "background"),
                        "background", self.cfg.background_ready_timeout,
                        container=current["containers"]["background"])
        self.check_held_background_port(target)
        # nginx -T may open configured files. A resume plan only prints that check.
        problems = []
        if self.ops.dry_run:
            self.ops.run([self.cfg.nginx_bin, "-T"], mutate=True, timeout=60)
        else:
            problems += self.nginx_preflight()
        problems += self.host_preflight()
        problems += self.public_health_preflight(held.get("allow_public_health_failure", False))
        if problems:
            raise DeployError("resume preflight failed:\n  - " + "\n  - ".join(problems))
        networks = self.inspect_held_networks()
        if repair:
            endpoints = self.load_serving_endpoints(repair)
            expected = {(role, name, ref) for role in SERVING_ROLES for name, ref in networks.items()}
            if {(ep.role, ep.network, ep.network_ref) for ep in endpoints} != expected:
                raise DeployError("recorded isolation networks changed; inspect the held release or abort")
            return endpoints
        endpoints = []
        for role, info in serving.items():
            if not self.has_expected_held_networks(info, networks, shared=False):
                raise DeployError(f"held {role} network isolation changed; restore it or abort")
            for name, ep in info["NetworkSettings"]["Networks"].items():
                aliases = tuple(dict.fromkeys([*(ep.get("Aliases") or ()), target.containers[role]]))
                endpoints.append(ServingEndpoint(role, name, networks[name], aliases, ServingEndpoint.connect_options(ep)))
        return endpoints

    def check_held_background_port(self, target: Target) -> None:
        problems = port_problems(self.ops, [target.ports["background"]])
        if problems:
            raise DeployError("resume background port check failed:\n  - " + "\n  - ".join(problems))

    def inspect_held_networks(self) -> dict[str, str]:
        result = self.ops.run(["docker", "network", "inspect", *self.cfg.networks], mutate=False, timeout=30)
        try:
            endpoints = json.loads(result.stdout)
        except ValueError as err:
            raise DeployError("cannot read configured Docker networks") from err
        if not isinstance(endpoints, list) or len(endpoints) != len(self.cfg.networks):
            raise DeployError("cannot resolve all configured Docker networks")
        networks: dict[str, str] = {}
        for item in endpoints:
            name = item.get("Name") if isinstance(item, dict) else None
            network_id = item.get("Id") if isinstance(item, dict) else None
            if not isinstance(name, str) or not name or not isinstance(network_id, str) or not network_id or name in networks:
                raise DeployError("configured Docker networks must resolve to distinct named networks")
            networks[name] = network_id
        return networks

    def configure_held_networks(self, target: Target, identities: dict[str, str],
                                endpoints: list[ServingEndpoint], *, shared: bool, start: bool = True) -> None:
        if not self.cfg.serving_alias:
            return
        self.drain_serving_group(target, identities)
        # Prepare every endpoint while both containers are stopped. Docker
        # brings up the configured endpoints before it runs the process.
        expected = {ep.network: ep.network_ref for ep in endpoints}
        connect = []
        for role in SERVING_ROLES:
            name = target.containers[role]
            info = self.ops.inspect(name) or {}
            state = info.get("State", {})
            if (info.get("Id") != identities[role] or
                    (not self.ops.dry_run and (state.get("Running") or state.get("Restarting")))):
                raise DeployError(f"cannot configure networks for {role}; container identity or stop is unconfirmed")
            networks = info.get("NetworkSettings", {}).get("Networks", {})
            if networks.keys() - expected.keys():
                raise DeployError(f"{role} joined an unexpected network; inspect before continuing")
            for endpoint in endpoints:
                if endpoint.role != role:
                    continue
                aliases = endpoint.aliases + ((self.cfg.serving_alias,) if shared else ())
                current = networks.get(endpoint.network)
                if endpoint.matches(current, aliases):
                    continue
                if current is not None:
                    self.ops.run(["docker", "network", "disconnect", endpoint.network_ref, identities[role]], mutate=True)
                connect.append((endpoint, identities[role], aliases))
        # In particular, remove every shared endpoint before any reconnect can
        # fail. A pair with missing isolated endpoints can still be aborted.
        for endpoint, cid, aliases in connect:
            self.connect_serving_endpoint(endpoint, cid, aliases)
        # A stopped container can store configuration for a network that no
        # longer exists. Reject that before activating either container.
        if self.inspect_held_networks() != expected:
            raise DeployError("configured Docker networks changed during activation")
        if not self.ops.dry_run:
            for role in SERVING_ROLES:
                info = self.ops.inspect(target.containers[role]) or {}
                if info.get("Id") != identities[role] or not self.has_expected_held_networks(info, expected, shared=shared):
                    raise DeployError(f"prepared networking for held {role} is unconfirmed")
        if not start:
            return
        for role in SERVING_ROLES:
            self.ops.run(["docker", "start", identities[role]], mutate=True)
        if not self.ops.dry_run:
            for role in SERVING_ROLES:
                name = target.containers[role]
                self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                self.cfg.ready_timeout, container=name)
                info = self.ops.inspect(name) or {}
                if info.get("Id") != identities[role] or not self.has_expected_held_networks(info, expected, shared=shared):
                    raise DeployError(f"running networking for held {role} is unconfirmed")
        for role in SERVING_ROLES:
            self.ops.run(["docker", "update", "--restart=unless-stopped", identities[role]], mutate=True)

    def held_aliases_withdrawn(self, target: Target, identities: dict[str, str], alias: str,
                               *, require_stopped: bool = False) -> bool:
        for role, cid in identities.items():
            info = self.ops.inspect(target.containers[role]) or {}
            candidates = [info]
            if info.get("Id") != cid:
                candidates.append(self.ops.inspect(cid) or {})
            for candidate in candidates:
                status = candidate.get("State", {})
                if require_stopped and (status.get("Running") or status.get("Restarting")):
                    return False
                endpoints = candidate.get("NetworkSettings", {}).get("Networks", {})
                if any(alias in (ep.get("Aliases") or []) for ep in endpoints.values()):
                    return False
        return True

    def activate_hold(self, state: dict[str, Any], held: dict[str, Any], active: MainRoute, target: Target,
                      identities: dict[str, str], endpoints: list[ServingEndpoint], *, publish: bool) -> None:
        changed_networks = False
        try:
            if self.cfg.serving_alias:
                changed_networks = True
                self.configure_held_networks(target, identities, endpoints, shared=True, start=publish)
                if not publish:
                    # Exercise the requested endpoint setup before changing the
                    # route or background. The existing owner still serves the
                    # alias, so the candidates must start in isolation.
                    self.configure_held_networks(target, identities, endpoints, shared=False)
            # Drain and reconnect can take time. Check again before the handoff.
            self.check_held_background_port(target)
            self.check_held_environment(held)
        except (Exception, KeyboardInterrupt) as err:
            if self.ops.dry_run:
                raise
            reason = str(err) or type(err).__name__
            if isinstance(err, DrainTimeout):
                # Both candidates were signalled. A second drain attempt
                # cannot restore an isolated pause while either is still up.
                raise FallbackFailed(f"held serving activation failed ({reason}). Verify both candidates have drained "
                                     "and are isolated before clearing in_progress") from err
            reclaimed = held
            if changed_networks:
                self.verify_topology(state, active, target.view(), require_standby=False)
                reclaimed = self.rebuild_held_isolation(target, identities, endpoints, held)
            self._save_progress(state, reclaimed)
            if reclaimed is not held:
                raise DeployError(reclaimed["reason"]) from err
            raise DeployError(f"held serving activation failed ({reason}); pause restored; retry 'resume' or 'abort'") from err

    def rebuild_held_isolation(self, target: Target, identities: dict[str, str], endpoints: list[ServingEndpoint],
                               held: dict[str, Any]) -> dict[str, Any]:
        try:
            self.configure_held_networks(target, identities, endpoints, shared=False)
        except (Exception, KeyboardInterrupt) as err:
            reason = f"network restoration is incomplete ({err or type(err).__name__})"
            try:
                # Missing isolated endpoints are safe for abort only after
                # confirming the candidates stopped and withdrew the alias.
                isolated = self.held_aliases_withdrawn(target, identities, self.cfg.serving_alias, require_stopped=True)
            except (Exception, KeyboardInterrupt):
                isolated = False
            if not isolated:
                raise FallbackFailed(f"{reason}; shared alias withdrawal is unconfirmed. Inspect 'status', "
                                     "wait for both held serving containers to drain and verify their networks") from err
            return {**held, "isolated_endpoints": [asdict(endpoint) for endpoint in endpoints],
                    "reason": f"{reason}; retry 'resume' once the networks are available or run 'abort'"}
        if not self.held_aliases_withdrawn(target, identities, self.cfg.serving_alias):
            raise FallbackFailed("the paused candidates still advertise the shared alias")
        return held

    def resume(self) -> None:
        self._resume()

    def _resume(self) -> None:
        state = load_main_state(self.cfg)
        self.close_retired_alias_overlap(state)
        preparation = state.get("in_progress") or {}
        if preparation.get("step") == "prepare held slot":
            if self.ops.dry_run:
                self.log("DRY-RUN resume would retry cleanup and prepare a held release")
                return
            target = preparation["target"]
            slot = "b" if (state.get("current") or {}).get("slot") == "a" else "a"
            configured = self.cfg.slot_view(slot)
            if state.get("current") != preparation.get("current") or any(target[key] != value for key, value in configured.items()):
                raise DeployError("release layout changed since cleanup failed; inspect 'status' and run 'abort'")
            self._deploy(state, target["image"], False, preparation["allow_public_health_failure"], True)
            return
        held, target = self.held_release(state)
        identities = {role: held["containers"][role]["id"] for role in SERVING_ROLES}
        # A failed read never establishes that anything has changed.
        try:
            endpoints = self.validate_hold(state, held, target)
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            raise DeployError(f"{err or 'resume validation interrupted'}. Held pause unchanged. Fix the cause and retry or abort") from err
        record = {"started_at": now(), "action": "resume", "image": target.image, "slot": target.slot,
                  "paused_at": held["held_at"]}
        try:
            active = self.current_route(state, state["current"], target.view(), install_probe=False)
            self._finish_switch(state, state["current"], target, record, create_background=True,
                                active=active, held=held, endpoints=endpoints)
        except (Exception, KeyboardInterrupt) as err:
            progress = state.get("in_progress") or {}
            if progress.get("phase") == "held" and not isinstance(err, DeployError):
                self._unexpected(err, record)
            step = progress.get("step")
            # activate_hold already reconciles its own network failures. If
            # the next state write fails, activation may have succeeded.
            unselected = (step == "activate held serving" and not isinstance(err, DeployError)
                          or self.switch_left_serving_unselected(step, err))
            def stop_candidates() -> None:
                self.verify_topology(state, active, target.view(), require_standby=False)
                self.drain_serving_group(target, identities)

            cleanup = stop_candidates if unselected else None
            self.fail_switch_operation(state, record, err, cleanup=cleanup)

    @staticmethod
    def switch_left_serving_unselected(step: str | None, err: BaseException) -> bool:
        if isinstance(err, (RollbackIncomplete, RoutingUnconfirmed)):
            return False
        if step == "background handoff":
            return True
        return step == "switch" and (isinstance(err, SwitchNotRestored)
                                     or isinstance(err, DeployError) and not isinstance(err, FallbackFailed))

    def fail_switch_operation(self, state: dict[str, Any], record: dict[str, Any], err: BaseException,
                              *, cleanup: Callable[[], None] | None) -> NoReturn:
        failure = err if isinstance(err, DeployError) else DeployError(str(err) or type(err).__name__)
        if not self.ops.dry_run:
            if cleanup is not None:
                try:
                    cleanup()
                except (Exception, KeyboardInterrupt) as cleanup_err:
                    failure = FallbackFailed(f"{failure}. Candidate serving cleanup failed "
                                             f"({cleanup_err or type(cleanup_err).__name__}). Inspect 'status' and "
                                             "verify both serving containers have drained")
            if (state.get("in_progress") or {}).get("phase") != "held":
                if isinstance(failure, FallbackFailed):
                    self._report_unrestored(state, failure)
                else:
                    self._record_unrestored(state, failure)
        record.update(result="failed", error=str(failure), steps=self.timings, finished_at=now())
        self._record_history(record)
        if failure is err:
            raise failure
        raise failure from err

    def recover_held_creates(self, progress: dict[str, Any], target: Target, owned: dict[str, str]) -> None:
        intent = progress.get("hold_creation_id")
        if not intent and progress.get("step") != "start serving":
            return
        for role in SERVING_ROLES:
            if role in owned:
                continue
            info = self.ops.inspect(target.containers[role])
            if not info:
                continue
            labels = (info.get("Config") or {}).get("Labels") or {}
            cid = info.get("Id")
            if not intent or labels.get(HOLD_CREATION_LABEL) != intent or not cid:
                raise DeployError(f"untracked {role} in the target slot; container ownership is unconfirmed")
            owned[role] = cid

    def abort(self) -> None:
        self._abort_hold()

    def _abort_hold(self) -> None:
        state = load_main_state(self.cfg)
        self.close_retired_alias_overlap(state)
        progress = state.get("in_progress") or {}
        if progress.get("step") == "prepare held slot":
            if not self.ops.dry_run:
                self._save_progress(state, None)
                self._record_history({"action": "abort", "image": progress["target"]["image"],
                                      "slot": progress["target"]["slot"], "result": "ok", "finished_at": now()})
            self.log("aborted hold preparation before creating serving containers")
            return
        phase = progress.get("phase")
        abandoned = phase == "deploying" and release_running(self.ops, progress) is False
        if phase not in ("held", "failed", "aborting") and not abandoned:
            raise DeployError("no paused release or failed release to abort")
        held = progress.get("hold") or (progress if "held_at" in progress else {})
        target = Target.from_view(progress["target"], self.cfg.runtime_token_file)
        origin = progress.get("origin") or {"current": held.get("current") or state["current"], "previous": state["previous"]}
        current = origin["current"]
        if (not current or target.slot == current["slot"] or set(target.containers.values()) & set(current["containers"].values())
                or set(target.ports.values()) & set(current["ports"].values())):
            raise DeployError("held containers overlap the current release; inspect status before manual recovery")
        recovering = abandoned or phase == "failed" or progress.get("abort_recovery", False)
        if not recovering and state["current"] != current:
            raise DeployError("held containers overlap the current release or the recorded layout changed")
        if not recovering and self.ops.inspect(target.containers["background"]):
            raise DeployError("unexpected target background container; inspect and resolve before aborting")
        owned = {**progress.get("owned", {}), **{role: item["id"] for role, item in held.get("containers", {}).items()}}
        if self.ops.dry_run:
            self.log("DRY-RUN abort would verify the original route before cleaning recorded candidates")
            return
        record = {"started_at": now(), "action": "abort", "image": target.image, "slot": target.slot}
        aborting = {**progress, "origin": origin, "phase": "aborting", "abort_recovery": recovering}
        try:
            self._save_progress(state, self._with_process(aborting))
            has_held_background = recovering and bool(held or progress.get("held_background_id")
                                                       or progress.get("held_background_creation_id"))
            if has_held_background:
                self.confirm_held_background(state, target)
            active, disk = self.abort_routes(state, current, target.view())
            if recovering:
                active = self.restore_abort_route(state, active, disk, current, target)
            self.verify_topology(state, active, target.view(), require_standby=False)
            if active.serving["slot"] != current["slot"] or active.admin["slot"] != current["slot"]:
                raise RoutingUnconfirmed("the original route did not take traffic")
            self.wait_for_nginx_drain(state, active)
            candidates = state["in_progress"].get("candidate_ids", {})
            self.recover_held_creates(progress, target, owned)
            if owned != progress.get("owned", {}) and progress.get("hold_creation_id"):
                self._save_progress(state, {**state["in_progress"], "owned": owned})
            alias_plan = current.get("withdrawn_serving_alias_networks") if recovering else None
            if alias_plan:
                for role in SERVING_ROLES:
                    info = self.ops.inspect(target.containers[role]) or {}
                    if info.get("State", {}).get("Running") and info.get("Id") != (owned.get(role) or candidates.get(role)):
                        raise DeployError("candidate serving identity is unconfirmed; keep it outside the restored route")
                self.retire_and_transfer(state, active, target.view(), alias_plan=alias_plan)
            for role, cid in candidates.items():
                if role not in owned:
                    self.stop_owned_serving(target, role, cid, allow_missing=True)
            for role, cid in owned.items():
                self.remove_owned_serving(target, role, cid)
            if has_held_background:
                self.remove_held_background(state, target)
        except (Exception, KeyboardInterrupt) as err:
            reason = str(err) or type(err).__name__
            if (state.get("in_progress") or {}).get("phase") == "aborting":
                latest = {key: value for key, value in state["in_progress"].items() if key != "process"}
                self._save_progress(state, {**latest, "reason": reason, "failed_at": now()})
            raise DeployError(f"abort incomplete; retry 'abort' after resolving: {reason}") from err
        try:
            state.update(current=current, previous=origin["previous"])
            self._save_progress(state, None)
        except OSError as err:
            raise DeployError("candidate cleanup completed but state write failed. Inspect 'status' and retry 'abort'") from err
        record.update(result="ok", finished_at=now())
        self._record_history(record)
        self.log("aborted release; the original route was confirmed")

    def abort_routes(self, state: dict[str, Any], original: dict[str, Any], candidate: dict[str, Any]) -> tuple[MainRoute, MainRoute]:
        progress = state["in_progress"]
        routes = [MainRoute(**progress[key]) for key in ("verified_route", "pending_route") if progress.get(key)]
        text = self.read_upstream()
        marker = ROUTE_REVISION.search(text)
        if not marker:
            route = self.current_route(state, original, candidate, install_probe=False)
            return route, route
        if not routes:
            routes = [MainRoute(original, original, marker[1])]
        disk = next((route for route in routes if self.route_text(route) == text), None)
        if disk is None:
            raise RoutingUnconfirmed("the upstream file does not match either recorded release route")
        for route in routes:
            if not route.revision:
                continue
            peer = candidate if route.serving["slot"] == original["slot"] else original
            with contextlib.suppress(RoutingUnconfirmed):
                self.verify_topology(state, route, peer, disk=disk, check_health=False)
                return route, disk
        for route in routes:
            if not route.revision and route.serving == original and route.admin == original:
                # A first ordinary deploy has no probe on its original route.
                # Restore and verify that route with a new reload before cleanup.
                return route, disk
        raise RoutingUnconfirmed("the original and attempted route generations could not be verified")

    def restore_abort_route(self, state: dict[str, Any], active: MainRoute, disk: MainRoute,
                             original: dict[str, Any], target: Target) -> MainRoute:
        restore = Target.from_view(original, self.cfg.runtime_token_file)
        saved = self.original_serving_endpoints(state)
        original_ids = state["in_progress"].get("original_serving_ids", {})
        for role in SERVING_ROLES:
            name = restore.containers[role]
            info = self.ops.inspect(name) or {}
            if not info.get("Id") or (role in original_ids and info["Id"] != original_ids[role]):
                raise DeployError(f"the original {role} is missing or replaced; recover it before retrying 'abort'")
            if not info.get("State", {}).get("Running"):
                cid = info["Id"]
                endpoints = {ep.network: ep for ep in saved if ep.role == role}
                other_owner = (not state["in_progress"].get("publish_aliases_at_create")
                               and bool(self.alias_networks(target.view())))
                if other_owner:
                    for network, ep in info.get("NetworkSettings", {}).get("Networks", {}).items():
                        aliases = tuple(ep.get("Aliases") or ())
                        if self.cfg.serving_alias in aliases and network not in endpoints:
                            endpoints[network] = ServingEndpoint.from_inspect(role, network, ep, aliases)
                self.configure_rollback_aliases(restore, {role: cid}, list(endpoints.values()), shared=not other_owner)
                self.ops.run(["docker", "update", "--restart=unless-stopped", cid], mutate=True)
                self.ops.run(["docker", "start", cid], mutate=True)
                self.wait_ready(restore.ports[role], self.token_for(original, role), "serving", self.cfg.ready_timeout,
                                container=cid, check_health=True)
        self.check_rollback_serving(original)
        old_name = restore.containers["background"]
        if not self.background_answering(original):
            old = self.ops.inspect(old_name) or {}
            if not old.get("Id"):
                raise DeployError("the original background is missing; recover it before retrying 'abort'")
            if old.get("State", {}).get("Running"):
                self.wait_exit(old["Id"], self.ops.monotonic() + self.cfg.background_exit_timeout,
                               poll=HANDOFF_POLL_SECONDS)
            self.hand_back(state, target, original)
        else:
            restart = state["in_progress"].get("background_restart_policies", {}).get(old_name)
            if restart and restart != "no":
                old = self.ops.inspect(old_name) or {}
                policy = (old.get("HostConfig") or {}).get("RestartPolicy")
                if policy is None:
                    raise DeployError("cannot verify the restored background restart policy")
                if (policy.get("Name") or "no") == "no":
                    # The previous recovery may have restored ownership but
                    # failed to undo the temporary restart policy.
                    self.ops.run(["docker", "update", f"--restart={restart}", old_name], mutate=True)
        progress = state["in_progress"]
        # An adopted file has no probe. If no route update was ever recorded,
        # there is no attempted switch to recover from or verify with a reload.
        unproven_switch = not active.revision and (progress.get("verified_route") or progress.get("pending_route"))
        needs_reload = (unproven_switch or active.excluded_serving is not None or active.serving["slot"] != restore.slot
                        or active.admin["slot"] != restore.slot or active != disk)
        if not needs_reload:
            return active

        attempted = False

        def start_reload() -> None:
            nonlocal attempted
            attempted = True

        route = MainRoute(original, original)
        try:
            self.apply_route(state, active, route, target.view(), reload_started=start_reload)
        except (Exception, KeyboardInterrupt) as err:
            if (not active.revision or isinstance(err, RoutingUnconfirmed)
                    or (attempted and not isinstance(err, (ReloadNotApplied, RoutingFileNotRestored)))):
                # Without proof that the pre-abort generation is still loaded,
                # keep both pairs and the recovery record for a later abort.
                raise
            reason = str(err) or type(err).__name__
            self.restore_loaded_background(state, restore, active.admin, reason)
            raise DeployError(f"{reason}. The background selected by the loaded route is answering again") from err
        return route

    @staticmethod
    def load_serving_endpoints(saved: list[dict[str, Any]]) -> list[ServingEndpoint]:
        return [ServingEndpoint(ep["role"], ep["network"], ep["network_ref"], tuple(ep["aliases"]), tuple(ep["options"]))
                for ep in saved]

    def original_serving_endpoints(self, state: dict[str, Any]) -> list[ServingEndpoint]:
        return self.load_serving_endpoints(state["in_progress"].get("original_endpoints", []))

    def confirm_held_background(self, state: dict[str, Any], target: Target) -> dict[str, Any] | None:
        info = self.ops.inspect(target.containers["background"])
        if not info:
            return None
        progress = state["in_progress"]
        cid = info.get("Id")
        recorded = progress.get("held_background_id")
        if not cid or (recorded and recorded != cid):
            raise DeployError("the candidate background identity is unconfirmed")
        if not recorded:
            intent = progress.get("held_background_creation_id")
            labels = (info.get("Config") or {}).get("Labels") or {}
            if not intent or labels.get(HOLD_CREATION_LABEL) != intent:
                raise DeployError("the candidate background identity is unconfirmed")
            self._save_progress(state, {**progress, "held_background_id": cid})
        return info

    def remove_held_background(self, state: dict[str, Any], target: Target) -> None:
        info = self.confirm_held_background(state, target)
        if not info:
            return
        status = info.get("State", {})
        if status.get("Running") is not False or status.get("Restarting"):
            raise DeployError("the candidate background exit is unconfirmed")
        self.ops.run(["docker", "rm", info["Id"]], mutate=True)
        if self.ops.inspect(target.containers["background"]):
            raise DeployError("the candidate background was not removed")

    def restore_held_after_alias_failure(self, state: dict[str, Any], target: Target,
                                         held: dict[str, Any], endpoints: list[ServingEndpoint]) -> None:
        original = held["current"]
        active, disk = self.abort_routes(state, original, target.view())
        peer = target.view() if active.serving["slot"] == original["slot"] else original
        self.verify_topology(state, active, peer, disk=disk, check_health=False, wait=True)
        route = self.restore_abort_route(state, active, disk, original, target)
        self.verify_topology(state, route, target.view(), wait=True)
        self.wait_for_nginx_drain(state, route)
        identities = {role: held["containers"][role]["id"] for role in SERVING_ROLES}
        reclaimed = self.rebuild_held_isolation(target, identities, endpoints, held)
        original_endpoints = self.original_serving_endpoints(state)
        original_ids = state["in_progress"].get("original_serving_ids", {})
        self.publish_routed_aliases(state, route, target.view(), original_ids, original_endpoints)
        observed = {}
        for endpoint in original_endpoints:
            info = self.serving_info(original, endpoint.role, observed)
            network = info.get("NetworkSettings", {}).get("Networks", {}).get(endpoint.network)
            if info.get("Id") != original_ids[endpoint.role] or not endpoint.matches(network, endpoint.aliases):
                raise DeployError("the original shared alias network was not restored")
        self.remove_held_background(state, target)
        recovered = {**state, "current": original}
        self._save_progress(recovered, {**reclaimed, "upstream_fingerprint": self.upstream_fingerprint()})
        state.update(recovered)

    def _unexpected(self, err: BaseException, record: dict[str, Any], *, owned: dict[str, str] | None = None,
                    hold: dict[str, Any] | None = None) -> None:
        detail = str(err) if isinstance(err, CommandTimedOut) else type(err).__name__
        failure = FallbackFailed(f"operation interrupted or command outcome unknown ({detail}); inspect 'status', "
                                 "restore routing/background manually before clearing in_progress")
        if not self.ops.dry_run:
            # The last atomic write may have failed: the persisted state is
            # authoritative, not the in-memory transition we attempted.
            persisted = load_main_state(self.cfg)
            progress = persisted.get("in_progress") or {}
            phase = progress.get("phase")
            if phase == "deploying":
                if owned:
                    progress["owned"] = dict(owned)
                if hold is not None and progress.get("step") in ("start serving", "save held release"):
                    progress.update(step="save held release", target=hold["target"], hold=hold)
                    failure = FallbackFailed(f"could not finish recording the pause ({type(err).__name__}); "
                                             "inspect 'status', then use 'resume' or 'abort'")
                self._record_unrestored(persisted, failure)
            record.update(result="failed", error=str(failure), steps=self.timings, finished_at=now())
            self._record_history(record)
            if phase == "held":
                raise DeployError("operation interrupted while recording a pause; pause retained; "
                                  "inspect status, then resume or abort") from err
        raise failure from err

    def _finish_switch(self, state: dict[str, Any], current: dict[str, Any], target: Target,
                       record: dict[str, Any], create_background: bool, *, active: MainRoute,
                       held: dict[str, Any] | None = None, endpoints: list[ServingEndpoint] | None = None,
                       alias_plan: dict[str, list[str]] | None = None) -> None:
        if held is not None:
            self._mark(state, target, "activate held serving", held=held)
            with self.step("activate held serving"):
                identities = {role: held["containers"][role]["id"] for role in SERVING_ROLES}
                assert endpoints is not None
                self.verify_topology(state, active, target.view(), require_standby=False)
                publish = not self.alias_networks(current)
                self.activate_hold(state, held, active, target, identities, endpoints, publish=publish)
                if not publish:
                    alias_plan = {role: [ep.network for ep in endpoints if ep.role == role] for role in SERVING_ROLES}
        try:
            if create_background:
                self._mark(state, target, "background handoff")
                if held is not None and not self.ops.dry_run:
                    self.check_rollback_serving(target.view())
                route = self.handoff_and_switch(state, active, target,
                                                lambda: self.handoff_background(state, current, target, create=True))
            else:
                route = self.rollback_to(state, active, target, alias_plan=alias_plan)
            if not self.ops.dry_run:
                self._mark(state, target, "guard")
                with self.step("guard"):
                    self.guard(target)
        except (Exception, KeyboardInterrupt) as err:
            progress = state.get("in_progress") or {}
            verified = progress.get("verified_route")
            if (self.ops.dry_run or isinstance(err, (RoutingUnconfirmed, RollbackIncomplete)) or not verified
                    or verified["serving"]["slot"] != target.slot or verified["admin"]["slot"] != target.slot):
                raise
            reason = str(err) or type(err).__name__
            try:
                self.restore_after_guard(state, MainRoute(**verified), current, reason)
            except ServingCleanupFailed:
                raise
            if held is None:
                self._save_progress(state, None)
            raise DeployError(f"guard failed ({reason}); the previous release serves traffic and owns the "
                              "background again; the new serving containers are draining or stopped and kept "
                              "for inspection") from err
        self._mark(state, target, "retire previous serving")
        signalled = {}
        with self.step("retire previous serving"):
            try:
                if create_background and held is None:
                    observed = {}
                    self.verify_topology(state, route, current, observed=observed, wait=True)
                    if not self.ops.dry_run:
                        old_aliases = self.alias_networks(current, observed=observed)
                        if not old_aliases.issubset(self.alias_networks(route.serving, observed=observed)):
                            raise DeployError("new serving does not provide the shared alias on all required networks")
                    self.wait_for_nginx_drain(state, route)
                    signalled = self.retire_serving(current, state=state)
                else:
                    self.retire_and_transfer(state, route, current, alias_plan=alias_plan)
            except AliasPublicationFailed as err:
                if held is None or endpoints is None:
                    raise FallbackFailed(f"shared alias publication incomplete ({err}); inspect 'status' or retry 'abort'") from err
                try:
                    self.restore_held_after_alias_failure(state, target, held, endpoints)
                except (Exception, KeyboardInterrupt) as restore_err:
                    raise FallbackFailed(f"shared alias publication failed ({err}); pause recovery incomplete "
                                         f"({restore_err}); inspect 'status' or retry 'abort'") from restore_err
                raise DeployError(f"shared alias publication failed ({err}); original release and pause restored; "
                                  "retry 'resume' or 'abort'") from err
            except (Exception, KeyboardInterrupt) as err:
                raise FallbackFailed(f"new release is serving; previous serving retirement incomplete: {err}; "
                                     "inspect status or retry 'abort'") from err
        view = target.view()
        view["deployed_at"] = now()
        if signalled and (state.get("in_progress") or {}).get("publish_aliases_at_create"):
            # A successful ordinary release does not wait for old streams to
            # finish. Watch may still see these exact instances on the alias,
            # so keep the window until the recorded instances are observed gone.
            retiring = {"slot": current["slot"], "containers": signalled, "recorded_at": now(),
                        "names": {role: current["containers"][role] for role in signalled}}
            state["retiring_serving"] = retiring
            window = (state.get("in_progress") or {}).get("alias_overlap") or {}
            state["alias_overlap"] = {
                "reason": window.get("reason", "publish_at_create"),
                "slots": window.get("slots") or [current["slot"], target.slot],
                "stage": window.get("stage", "start serving"),
                "started_at": window.get("started_at", now()),
                "retiring": retiring,
            }
        else:
            state.pop("retiring_serving", None)
        state.update(previous=current, current=view)
        self._save_progress(state, None)
        record.update(result="ok", steps=self.timings, finished_at=now())
        self._record_history(record)
        self.log(f"done: slot {target.slot} serving {target.image}")

    def _mark(self, state: dict[str, Any], target: Target, step: str, *, held: dict[str, Any] | None = None,
              publish_aliases_at_create: bool = False, hold_creation_id: str | None = None) -> None:
        prior = state.get("in_progress") or {}
        original = prior.get("origin") or {"current": state.get("current"), "previous": state.get("previous")}
        recorded = prior.get("target") or target.view()
        progress = {"phase": "deploying", "target": target.view() if recorded["slot"] == target.slot else recorded,
                    "step": step, "since": now(), "origin": original}
        for key in ("verified_route", "pending_route", "owned", "candidate_ids", "original_endpoints",
                    "background_restart_policies", "held_background_id", "held_background_creation_id",
                    "original_serving_ids", "nginx_previous_workers", "alias_handoff", "alias_overlap"):
            if key in prior:
                progress[key] = prior[key]
        if publish_aliases_at_create or prior.get("publish_aliases_at_create"):
            progress["publish_aliases_at_create"] = True
        if publish_aliases_at_create and not prior.get("publish_aliases_at_create"):
            origin_current = original.get("current") or {}
            if origin_current.get("slot") and origin_current["slot"] != target.slot:
                # Preflight just proved no container in this slot is running,
                # so an earlier ordinary window there has ended.
                earlier = state.get("alias_overlap")
                if (isinstance(earlier, dict) and not earlier.get("ended_at")
                        and (earlier.get("retiring") or {}).get("slot") == target.slot):
                    state["alias_overlap"] = {**earlier, "ended_at": now()}
                # The target registers at create while the current slot still
                # serves; this window ends when the retired instances exit.
                progress["alias_overlap"] = alias_overlap_record(
                    "publish_at_create", [origin_current["slot"], target.slot], step)
        if hold_creation_id or prior.get("hold_creation_id"):
            progress["hold_creation_id"] = hold_creation_id or prior["hold_creation_id"]
        hold = held or prior.get("hold")
        if hold:
            progress["hold"] = hold
        self._save_progress(state, self._with_process(progress))

    def _with_process(self, progress: dict[str, Any]) -> dict[str, Any]:
        if not self.ops.dry_run:
            with contextlib.suppress(OSError):
                process = release_process(self.ops, os.getpid())
                if process:
                    return {**progress, "process": process}
        return progress

    def _save_progress(self, state: dict[str, Any], progress: dict[str, Any] | None) -> None:
        if not self.ops.dry_run:
            save_state(self.cfg, {**state, "in_progress": progress})
        state["in_progress"] = progress

    def _record_unrestored(self, state: dict[str, Any], err: DeployError) -> None:
        """Keep in_progress, with why, so the next run refuses and 'status'
        shows what a person has to do."""
        progress = state.get("in_progress")
        if progress:
            failed = {key: value for key, value in progress.items() if key != "process"}
            failed.update(phase="failed", failed_step=progress.get("step", "unknown"),
                          reason=str(err) or type(err).__name__,
                          failed=str(err) or type(err).__name__, failed_at=now())
            self._save_progress(state, failed)

    def _report_unrestored(self, state: dict[str, Any], err: DeployError) -> None:
        self._record_unrestored(state, err)
        if isinstance(err, ServingCleanupFailed):
            label = "FAILED"
        elif isinstance(err, RollbackIncomplete):
            label = "ROLLBACK INCOMPLETE"
        else:
            label = "FAILED AND NOT RESTORED"
        self.log(f"{label}: {err}")

    def _abort(self, state: dict[str, Any], target: Target, err: DeployError,
               owned: dict[str, str] | None = None, *, retry_preparation: dict[str, Any] | None = None) -> None:
        """Clear a failure only when the old release still serves or recovery has finished."""
        owned = owned or {}
        progress = state.get("in_progress") or {}
        step = progress.get("step")
        if self.ops.dry_run:
            self.log(f"FAILED: {err}")
            return
        if progress.get("phase") == "held":
            self.log(f"FAILED: {err}")
            return
        if isinstance(err, FallbackFailed) or step not in (None, "migrate", "start serving", "background handoff", "switch"):
            # Keep in_progress so the next run refuses until a person resolves
            # it, and keep why, so 'status' shows what needs doing.
            self._report_unrestored(state, err)
            return
        deadline = err.cleanup_deadline
        if deadline is not None:
            # A recovery window already bounded the background handoff. Remove
            # the unused target servings within what remains of it.
            with finish_handoff_cleanup(ignore=True):
                try:
                    for role in SERVING_ROLES:
                        name = target.containers[role]
                        network = self.sole_alias_network(name, state, extra=(target.view(),))
                        if network is not None:
                            where = f"network {network[0]}" if network[0] != "unknown" else "a network (state unconfirmed)"
                            raise SoleAliasProviderKept(f"{name} is the only serving instance advertising the "
                                                        f"shared alias on {where}; kept running.")
                        result = self.ops.run(["docker", "rm", "-f", name], mutate=True, check=False,
                                              timeout=self.recovery_timeout(deadline, 600))
                        if result.returncode:
                            timeout = self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS)
                            if not self.ops.container_missing(name, timeout=timeout):
                                raise DeployError(f"could not remove {name} (exit {result.returncode})")
                except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as cleanup_err:
                    failure = FallbackFailed(f"{err}. Incomplete serving cleanup: {cleanup_err}. "
                                             "Check the target slot before clearing in_progress.")
                    self._record_unrestored(state, failure)
                    self.log(f"FAILED: {failure}")
                    return
        elif step in ("start serving", "background handoff", "switch"):
            try:
                if progress.get("hold_creation_id"):
                    self.recover_held_creates(progress, target, owned)
                if owned:
                    route = self.current_route(state, state["current"], target.view())
                    self.verify_topology(state, route, target.view(), require_standby=False)
                for role, cid in owned.items():
                    self.remove_owned_serving(target, role, cid)
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as cleanup_err:
                failure = FallbackFailed(f"cleanup incomplete ({cleanup_err}); inspect status and remove the "
                                         "unused target containers before clearing in_progress")
                self._record_unrestored(state, failure)
                self.log(f"FAILED AND NOT RESTORED: {failure}")
                return
        if retry_preparation:
            # Cleanup succeeded, but a failed resume must remain visible and
            # the operator may retry preparing the slot or abort it.
            self._record_unrestored({**state, "in_progress": retry_preparation}, err)
        else:
            self._save_progress(state, None)
        self.log(f"FAILED: {err}")


# --------------------------------------------------------------------------
# Status, watch and adopt


def observed_container(ops: Ops, name: str) -> tuple[dict[str, Any], str]:
    try:
        info = ops.inspect(name)
    except (DeployError, OSError, subprocess.SubprocessError):
        return {}, "unknown"
    if not info:
        return {}, "missing"
    return info, info.get("State", {}).get("Status", "unknown")


def status(cfg: Config, ops: Ops) -> dict[str, Any]:
    state = load_main_state(cfg)
    deployer = Deployer(cfg, ops)
    progress = state.get("in_progress")
    report: dict[str, Any] = {"in_progress": dict(progress) if progress else None}
    if state.get("retiring_serving"):
        # The ordinary release does not wait for the retired instances to exit.
        report["retiring_serving"] = dict(state["retiring_serving"])
    if isinstance(state.get("alias_overlap"), dict):
        # Read-only: the persisted record closes on the next release command,
        # so status derives the end from the recorded instances instead.
        report["alias_overlap"] = deployer.observed_alias_overlap(dict(state["alias_overlap"]))
    if progress and progress["phase"] in ("deploying", "aborting"):
        report["in_progress"]["process_running"] = release_running(ops, progress)
    if progress and progress["phase"] in ("held", "aborting") and "held_at" in progress:
        since = dt.datetime.fromisoformat(progress["held_at"])
        age = max(0, int((dt.datetime.now(dt.timezone.utc) - since).total_seconds()))
        warning = None
        if age >= cfg.hold_warning_seconds and progress["phase"] == "held":
            warning = "held release is old; resume or abort after verification"
        report["in_progress"].update(age_seconds=age, warning=warning, live_containers={})
        for role in SERVING_ROLES:
            name = progress["target"]["containers"][role]
            info, container_status = observed_container(ops, name)
            identity = progress["containers"][role]
            runtime = None
            with contextlib.suppress(OSError):
                runtime = deployer.runtime(progress["target"]["ports"][role],
                                           deployer.token_for(progress["target"], role))
            report["in_progress"]["live_containers"][role] = {
                "status": container_status,
                "ready": (runtime or {}).get("ready"),
                "identity_matches": None if container_status == "unknown" else info.get("Id") == identity["id"],
                "image_matches": None if container_status == "unknown" else info.get("Image") == identity["image_id"]}
    for key in ("current", "previous"):
        view = state.get(key)
        if not view:
            report[key] = None
            continue
        entry = {"slot": view["slot"], "image": view.get("image"), "deployed_at": view.get("deployed_at"),
                 "containers": {}}
        for role, name in view["containers"].items():
            info, container_status = observed_container(ops, name)
            s = info.get("State", {})
            entry["containers"][role] = {"name": name, "port": view["ports"][role],
                                         "status": container_status,
                                         "health": (s.get("Health") or {}).get("Status"),
                                         "restarts": info.get("RestartCount"),
                                         # "no" on a running background: it was told to
                                         # drain and Docker will not bring it back.
                                         "restart_policy": ((info.get("HostConfig") or {})
                                                            .get("RestartPolicy") or {}).get("Name")}
        report[key] = entry
    return report


def watch_once(cfg: Config, ops: Ops, history: dict[str, list[tuple[float, int]]]) -> list[dict[str, Any]]:
    """One resident sample of the current release; returns alerts."""
    state = load_main_state(cfg)
    alerts = []
    progress = state.get("in_progress") or {}
    phase = progress.get("phase")
    active_release = phase in ("deploying", "aborting") and release_running(ops, progress) is True
    if phase == "failed":
        alerts.append({"alert": ["release_incomplete"], "at": now(), "in_progress": progress})
    elif phase in ("deploying", "aborting") and not active_release:
        # Avoid an alert if the sampled process just completed and saved a new phase.
        if load_main_state(cfg).get("in_progress") == progress:
            alert = "release_incomplete" if phase == "deploying" else "abort_incomplete"
            alerts.append({"alert": [alert], "at": now(), "in_progress": progress})
    current = state.get("current")
    if not current:
        return alerts
    deployer = Deployer(cfg, ops)
    peer = progress.get("target")
    if not peer or peer["slot"] == current["slot"]:
        peer = state.get("previous")
    # Steady state allows only the current slot. A recorded handoff also
    # allows the named peer, but only the recorded instances may keep draining.
    allowed = {current["slot"]}
    retiring_slot = None
    retiring = None
    if peer and peer["slot"] != current["slot"]:
        overlap_expected = (phase in ("deploying", "aborting") and active_release
                            and planned_alias_overlap(progress, current, peer))
        if overlap_expected:
            allowed.add(peer["slot"])
            marking = (progress.get("retiring_serving") or {}) if progress else (state.get("retiring_serving") or {})
            if marking.get("slot") == peer["slot"]:
                retiring_slot, retiring = peer["slot"], marking.get("containers")
        elif not progress:
            # A finished ordinary release may still have its retired instances
            # draining; only those exact instances are exempt.
            retired = state.get("retiring_serving") or {}
            if retired.get("slot") == peer["slot"]:
                allowed.add(peer["slot"])
                retiring_slot, retiring = peer["slot"], retired.get("containers")
    conflict = set()
    with contextlib.suppress(DeployError, OSError, subprocess.SubprocessError):
        for slot, view in alias_slot_views(cfg, state, progress).items():
            if slot == retiring_slot:
                # Once a role is marked for TERM, only that same container
                # start may continue draining on the alias.
                roles = tuple(retiring or ())
                if roles and deployer.alias_networks(view, retiring=retiring, roles=roles):
                    conflict.add(slot)
            elif slot not in allowed and deployer.alias_networks(view):
                conflict.add(slot)
    if conflict:
        alerts.append({"alert": ["shared_alias_conflict"], "at": now(), "slots": sorted(conflict)})
    t = ops.monotonic()
    for role, name in current["containers"].items():
        token = None
        with contextlib.suppress(OSError):
            token = deployer.token_for(current, role)
        info, container_status = observed_container(ops, name)
        if container_status == "unknown":
            alerts.append({"alert": ["docker_inspect_failed"], "at": now(), "role": role, "name": name})
            continue
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


def adoption_paths(cfg_path: str, work_dir: str) -> tuple[str, Path]:
    try:
        config = json.loads(Path(cfg_path).read_text())
    except FileNotFoundError:
        return DEFAULT_RELEASE_LOCK, Path(work_dir) / "state.json"
    except (OSError, ValueError) as err:
        raise DeployError("cannot read existing Main configuration for adopt") from err
    if not isinstance(config, dict) or any(
            not isinstance(config.get(key), str) or not config[key] for key in ("lock_file", "state_file")):
        raise DeployError("existing Main configuration must specify release lock and state paths before adopt")
    return config["lock_file"], Path(config["state_file"])


def adopt(cfg_path: str, ops: Ops, serving: str, standby: str, background: str, work_dir: str,
          public_health_url: str) -> None:
    lock_file, configured_state = adoption_paths(cfg_path, work_dir)
    with release_lock(lock_file, create=not ops.dry_run):
        if adoption_paths(cfg_path, work_dir) != (lock_file, configured_state):
            raise DeployError("Main configuration changed during adopt; retry after reviewing it")
        for path in dict.fromkeys((configured_state, Path(work_dir) / "state.json")):
            try:
                state = normalize_main_state(read_state_file(path))
            except (OSError, ValueError) as err:
                raise DeployError("cannot confirm existing Main release state before adopt") from err
            Deployer.require_idle(state)
        if not ops.dry_run:
            Path(work_dir).mkdir(parents=True, exist_ok=True, mode=0o700)
        _adopt(cfg_path, ops, serving, standby, background, work_dir, public_health_url, lock_file)


def _adopt(cfg_path: str, ops: Ops, serving: str, standby: str, background: str, work_dir: str,
           public_health_url: str, lock_file: str) -> None:
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
        "lock_file": lock_file,
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


def main_recovery_hint(cfg: Config | None) -> str:
    if cfg is None:
        return "Check the Main configuration before retrying."
    try:
        progress = load_main_state(cfg).get("in_progress") or {}
        phase = progress.get("phase")
    except (Exception, KeyboardInterrupt):
        return "Check access to the Main state file and run 'status' before retrying."
    if phase == "held":
        return "The release is paused. Run 'status', then 'resume' or 'abort'."
    if phase == "aborting":
        return "Cleanup is incomplete. Run 'status', then retry 'abort'."
    if progress.get("step") == "prepare held slot":
        return "Held slot preparation is incomplete. Run 'status', then retry 'resume' or 'abort'."
    if phase == "failed":
        return "Run 'status', then 'abort' to verify the original route and clean up."
    if progress:
        return "Run 'status' and verify routing and background ownership before clearing in_progress."
    return "Run 'status' and verify the current release before retrying."


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
    p_main.add_argument("--hold-before-switch", action="store_true",
                        help="exit successfully after serving readiness; resume or abort after verification")
    p_main.add_argument("--allow-public-health-failure", action="store_true",
                        help="warn instead of stopping when the public health check fails before the switch "
                             "(the guard still requires 200 after it)")
    p_main.add_argument("--allow-running-backup", action="store_true",
                        help="start a release while a scheduled backup is running")
    p_rollback = sub.add_parser("rollback", help="switch back to the previous slot")
    p_rollback.add_argument("--allow-running-backup", action="store_true",
                            help="roll back while a scheduled backup is running")
    sub.add_parser("resume", help="validate the paused release and continue switching")
    sub.add_parser("abort", help="stop and remove the paused release's serving containers")
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
    cfg = None
    try:
        ops = MainOps(dry_run=args.dry_run)
        if args.command == "adopt":
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
        with release_lock(cfg.lock_file, create=not args.dry_run):
            deployer = Deployer(cfg, ops)
            if args.command == "main":
                deployer.deploy(args.image, allow_contract=args.allow_contract,
                                allow_public_health_failure=args.allow_public_health_failure,
                                hold_before_switch=args.hold_before_switch,
                                allow_running_backup=args.allow_running_backup)
            elif args.command == "rollback":
                deployer.rollback(allow_running_backup=args.allow_running_backup)
            elif args.command == "resume":
                deployer.resume()
            elif args.command == "abort":
                deployer.abort()
        return 0
    except DeployError as err:
        print(f"deploy: {err}", file=sys.stderr)
        return 1
    except (Exception, KeyboardInterrupt) as err:
        print(f"deploy: {args.command} failed ({type(err).__name__}). {main_recovery_hint(cfg)}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
