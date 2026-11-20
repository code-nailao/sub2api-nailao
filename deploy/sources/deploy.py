#!/usr/bin/env python3
"""Fixed release entrypoint for Sources Main.

One command replaces Main serving and the unique background with one image:

    deploy.py main <image>     preflight, migrate, check, start, hand off,
                               switch, guard (auto-rollback), retire
    deploy.py rollback         return to the previous slot's containers
    deploy.py resume           validate a held release, then finish switching
    deploy.py abort            discard the held serving containers
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
import re
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterator

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


class DeployError(Exception):
    """A release operation failed."""


class FallbackFailed(DeployError):
    """Restoring the previous release failed too; needs a person."""


class DrainTimeout(DeployError):
    """A container still runs after its drain deadline."""


class RoutingFileNotRestored(FallbackFailed):
    """The file needs repair, but this attempt never reloaded live routing."""


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

    def inspect(self, name: str) -> dict[str, Any] | None:
        proc = self.run(["docker", "inspect", name], mutate=False, check=False, timeout=30)
        if proc.returncode != 0:
            return None
        data = json.loads(proc.stdout)
        return data[0] if data else None

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


class MainOps(Ops):
    def inspect(self, name: str) -> dict[str, Any] | None:
        proc = self.run(["docker", "inspect", name], mutate=False, check=False, timeout=30)
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

    def runtime(self, port: int, token: str) -> dict[str, Any] | None:
        status, body = self.ops.http_json(f"http://127.0.0.1:{port}/internal/sources/runtime", token=token)
        return body if status == 200 and isinstance(body, dict) else None

    def wait_ready(self, port: int, token: str, role: str, timeout: int,
                   container: str | None = None, poll: float = 2) -> dict[str, Any]:
        """Wait for a role to report ready. When the container is given, a
        container that exited or is crash-restarting fails at once instead of
        holding the background gap for the whole timeout."""
        deadline = self.ops.monotonic() + timeout
        last: dict[str, Any] | None = None
        while self.ops.monotonic() < deadline:
            if container and not self.ops.dry_run:
                info = self.ops.inspect(container) or {}
                state = info.get("State", {})
                if not state.get("Running") or state.get("Restarting"):
                    raise DeployError(f"{container} exited or is restarting before becoming ready "
                                      f"(exit {state.get('ExitCode')}); see its logs")
            last = self.runtime(port, token)
            if last and last.get("ready") and last.get("role") == role:
                if role != "background" or last.get("owns_background"):
                    return last
            self.ops.sleep(poll)
        raise DeployError(f"port {port} not ready as {role} within {timeout}s (last runtime: {json.dumps(last)[:300]})")

    def wait_exit(self, name: str, timeout: int, poll: float = 2, first_note: float = 60,
                  progress: Callable[[], str | None] | None = None) -> int:
        """Wait for a container to exit on its own. progress, when given,
        describes what it is still doing for the periodic note."""
        deadline = self.ops.monotonic() + timeout
        next_note = self.ops.monotonic() + first_note
        while True:
            info = self.ops.inspect(name)
            if info is None:
                return 0
            state = info.get("State", {})
            if not state.get("Running"):
                return int(state.get("ExitCode", 0))
            if self.ops.monotonic() >= deadline:
                raise DrainTimeout(f"{name} still draining after {timeout}s; nothing was killed")
            if self.ops.monotonic() >= next_note:
                detail = progress() if progress else None
                self.log(f"   {name} still finishing in-flight work" + (f": {detail}" if detail else " ..."))
                next_note += 60
            self.ops.sleep(poll)

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
               created: dict[str, str] | None = None) -> None:
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
        args += self.docker_env_args(role)
        args += self.mount_args(self.mounts_for(role, name))
        args.append(target.image_id or target.image)
        proc = self.ops.run(args, mutate=True)
        if created is not None and not self.ops.dry_run:
            cid = proc.stdout.strip()
            if not cid:
                raise FallbackFailed("Docker did not identify the newly created serving container; inspect status")
            # Record ownership before the remaining network connections, which can fail.
            created[role] = cid
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

    def install_upstreams(self, text: str) -> None:
        try:
            previous = self.ops.read_text(self.cfg.upstream_file)
        except FileNotFoundError:
            previous = None
        except OSError as err:
            raise DeployError("cannot read the previous upstream file; routing unchanged") from err
        try:
            self.ops.write_file_atomic(self.cfg.upstream_file, text)
            test = self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True, check=False)
            if test.returncode != 0:
                raise DeployError("nginx -t rejected the upstream file")
        except (Exception, KeyboardInterrupt) as err:
            if previous is None:
                raise RoutingFileNotRestored(f"upstream update failed before reload ({err}) and no previous upstream file is available") from err
            try:
                self.ops.write_file_atomic(self.cfg.upstream_file, previous)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise RoutingFileNotRestored("could not restore the previous upstream file after failure before reload") from restore_err
            raise DeployError(f"{err}; previous file restored, routing unchanged") from err
        try:
            self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True)
        except (Exception, KeyboardInterrupt) as err:
            if previous is None:
                raise FallbackFailed("reload failed and no previous upstream file is available") from err
            try:
                self.ops.write_file_atomic(self.cfg.upstream_file, previous)
                self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True)
                self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise FallbackFailed("reload failed; restoring previous routing failed too; inspect status") from restore_err
            raise DeployError("reload failed; previous routing restored") from err

    def switch_to(self, view: dict[str, Any]) -> None:
        ports = view["ports"]
        self.install_upstreams(render_upstreams(ports["serving"], ports["standby"], ports["background"]))

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

    def owner_runtime(self, view: dict[str, Any]) -> dict[str, Any] | None:
        """Runtime of a slot's background owner, or None when it cannot be read.
        Only informs the log: nothing here may end a handoff, so any failure
        (including one from a process exiting mid-response) is None."""
        try:
            return self.runtime(view["ports"]["background"], self.token_for(view, "background"))
        except Exception:  # noqa: BLE001 - informational read inside the handoff
            return None

    def drain_progress(self, view: dict[str, Any]) -> str | None:
        """What a draining owner still waits for, from its runtime status. It
        runs inside the wait after TERM, so it never raises."""
        try:
            runtime = self.owner_runtime(view)
            if not runtime:
                return None
            parts = []
            if runtime.get("active_admin_writes"):
                parts.append(f"{int(runtime['active_admin_writes'])} admin request(s) in flight")
            if runtime.get("stopping"):
                parts.append("still stopping " + ", ".join(str(name) for name in runtime["stopping"]))
            return "; ".join(parts) or None
        except Exception:  # noqa: BLE001 - a progress note must never end the wait
            return None

    def stuck_owner(self, current: dict[str, Any], target: Target, back: bool, timeout: int) -> FallbackFailed:
        """The old owner did not exit in time. Say which slot serves traffic,
        what is left running and which background a person has to start."""
        old = current["containers"]["background"]
        new = target.containers["background"]
        if back:
            # Traffic already went back to target's slot; old is the newer
            # release's background and must not be restarted.
            traffic, start = target.slot, new
            idle = [current["containers"][r] for r in SERVING_ROLES if current["containers"].get(r)]
        else:
            traffic, start = current["slot"], old
            idle = [target.containers[r] for r in SERVING_ROLES]
        not_this = f" (not {old})" if start != old else ""
        return FallbackFailed(
            f"{old} is still draining after {timeout}s; nothing was killed and {new} was not started. "
            f"Traffic is on slot {traffic}. {old} is set not to restart, so once it exits no background owner runs. "
            f"Serving containers left outside the Nginx route: {', '.join(idle) or 'none'}. "
            f"Wait for {old} to exit (docker ps -a), then start the background of slot {traffic}{not_this}: "
            f"'docker update --restart=unless-stopped {start}' and 'docker start {start}'; check 'status', "
            "then clear in_progress. Do not deploy again before that")

    def handoff_background(self, current: dict[str, Any], target: Target, create: bool,
                           back: bool = False) -> None:
        """Move background ownership from current to target. The old owner gets
        one SIGTERM and finishes in-flight work; the kernel lock passes when it
        exits. If the new owner does not become ready, the old one restarts.
        An old owner that reports admin_during_drain keeps answering admin
        reads until it closes its listener, just before it exits. back: the
        handoff returns ownership to target after traffic already moved there."""
        old = current["containers"]["background"]
        new = target.containers["background"]
        before = self.owner_runtime(current)  # read-only, also in a dry run
        self.ops.run(["docker", "update", "--restart=no", old], mutate=True)
        if create:
            self.create(target, "background")
        else:
            self.ops.run(["docker", "update", "--restart=unless-stopped", new], mutate=True)
        self.log(f"   TERM {old}; waiting for it to finish in-flight work (no SIGKILL)")
        if (before or {}).get("admin_during_drain") == "read-only":
            self.log(f"   while {old} drains it answers admin reads and refuses admin writes with 503; "
                     "the admin API is down only from its exit until the new background is ready")
        else:
            self.log(f"   the admin API is unavailable until the new background is ready "
                     f"({old} does not report read-only draining)")
        old_exited = False
        try:
            self.ops.run(["docker", "kill", "--signal=TERM", old], mutate=True)
            if not self.ops.dry_run:
                timeout = self.cfg.background_exit_timeout
                try:
                    code = self.wait_exit(old, timeout, poll=HANDOFF_POLL_SECONDS,
                                          first_note=HANDOFF_FIRST_NOTE_SECONDS,
                                          progress=lambda: self.drain_progress(current))
                except DrainTimeout as err:
                    raise self.stuck_owner(current, target, back, timeout) from err
                old_exited = True
                self.log(f"   {old} exited with code {code}")
            self.ops.run(["docker", "start", new], mutate=True)
            if self.ops.dry_run:
                return
            self.wait_ready(target.ports["background"], self.token_for(target.view(), "background"),
                            "background", self.cfg.background_ready_timeout, container=new,
                            poll=HANDOFF_POLL_SECONDS)
        except FallbackFailed:
            raise
        except (Exception, KeyboardInterrupt) as err:
            if self.ops.dry_run:
                raise
            self.log(f"   new background failed: {err}; restarting {old}")
            try:
                if not old_exited:
                    # TERM or the wait can fail after the old process exited.
                    # Never restart it while it may still be draining.
                    info = self.ops.inspect(old)
                    if not info or info.get("State", {}).get("Running"):
                        raise DeployError(f"exit of {old} is unconfirmed after attempting TERM")
                if self.ops.inspect(new):
                    self.ops.run(["docker", "update", "--restart=no", new], mutate=True)
                    if (self.ops.inspect(new) or {}).get("State", {}).get("Running"):
                        self.ops.run(["docker", "kill", "--signal=TERM", new], mutate=True)
                        self.wait_exit(new, 120, poll=HANDOFF_POLL_SECONDS)
                self.ops.run(["docker", "update", "--restart=unless-stopped", old], mutate=True)
                self.ops.run(["docker", "start", old], mutate=True)
                self.wait_ready(current["ports"]["background"], self.token_for(current, "background"), "background",
                                self.cfg.background_ready_timeout, container=old, poll=HANDOFF_POLL_SECONDS)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise FallbackFailed(f"background handoff failed ({err}); restoring {old} failed too "
                                     f"({restore_err}); verify and restore background ownership") from restore_err
            raise DeployError(f"new background failed ({err}); "
                              f"previous background {old} is running again") from err

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

    def restore_after_guard(self, target: Target, previous: dict[str, Any], reason: str) -> None:
        self.log(f"   guard failed: {reason}; switching traffic back to slot {previous['slot']}")
        try:
            self.switch_to(previous)
        except (Exception, KeyboardInterrupt) as err:
            raise FallbackFailed(f"guard failed ({reason}) and switching back failed ({err})") from err
        self.log("   handing the background back to the previous release")
        self.hand_back(target, previous)
        try:
            self.retire_serving(target.view())
        except (Exception, KeyboardInterrupt) as err:
            raise FallbackFailed("traffic and background restored, but target serving retirement "
                                 "is incomplete; inspect status before clearing in_progress") from err

    def hand_back(self, target: Target, previous: dict[str, Any]) -> None:
        """Return background ownership to the previous release after traffic
        is already back on it."""
        try:
            self.handoff_background(target.view(), Target.from_view(previous, self.cfg.runtime_token_file),
                                    create=False, back=True)
        except FallbackFailed:
            raise
        except (Exception, KeyboardInterrupt) as err:
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

    def retire_serving(self, view: dict[str, Any]) -> None:
        for role in SERVING_ROLES:
            name = view["containers"].get(role)
            if not name:
                continue
            token = None
            with contextlib.suppress(OSError):
                token = self.token_for(view, role)
            info = self.ops.inspect(name)
            if not info or not info.get("State", {}).get("Running"):
                continue
            self.ops.run(["docker", "update", "--restart=no", name], mutate=True)
            runtime = self.runtime(view["ports"][role], token) if token else None
            if runtime and runtime.get("instance_id") and not self.ops.dry_run:
                self.ops.http_json(f"http://127.0.0.1:{view['ports'][role]}/internal/sources/drain", token=token,
                                   method="POST", body={"instance_id": runtime["instance_id"]})
            # TERM without waiting: the process finishes requests already routed
            # to it (streams included) and exits by itself.
            self.ops.run(["docker", "kill", "--signal=TERM", name], mutate=True)

    def stop_owned_serving(self, target: Target, role: str, cid: str, *, allow_missing: bool = False) -> bool:
        """Disable restarts and let this exact serving container finish current requests."""
        name = target.containers[role]
        info = self.ops.inspect(name)
        if not info and allow_missing:
            return False
        if not info or info.get("Id") != cid:
            raise DeployError(f"{role} was removed or replaced; refusing to modify an unrelated container")
        self.ops.run(["docker", "update", "--restart=no", cid], mutate=True)
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
                self.ops.run(["docker", "kill", "--signal=TERM", cid], mutate=True)
            except DeployError:
                if (self.ops.inspect(cid) or {}).get("State", {}).get("Running"):
                    raise
            if not self.ops.dry_run:
                self.wait_exit(cid, self.cfg.ready_timeout)
        return True

    def remove_owned_serving(self, target: Target, role: str, cid: str) -> None:
        name = target.containers[role]
        if self.stop_owned_serving(target, role, cid, allow_missing=True):
            self.ops.run(["docker", "rm", cid], mutate=True)
        if not self.ops.dry_run and self.ops.inspect(name):
            raise DeployError(f"{name} still exists")

    # entrypoints --------------------------------------------------------------
    @staticmethod
    def require_idle(state: dict[str, Any]) -> None:
        progress = state.get("in_progress") or {}
        if progress.get("phase") == "held":
            raise DeployError("a release is paused; run 'resume' or 'abort' first (see 'status')")
        if progress.get("phase") == "aborting":
            raise DeployError("abort cleanup has started; retry 'abort' first (see 'status')")
        if progress:
            raise DeployError(f"a previous deploy did not finish: {json.dumps(progress)}; "
                              "inspect with 'status' and resolve before deploying again")

    def deploy(self, image: str, allow_contract: bool = False,
               allow_public_health_failure: bool = False, hold_before_switch: bool = False) -> None:
        state = load_main_state(self.cfg)
        self.require_idle(state)
        current = state.get("current") or {}
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
                pending = self.preflight(state, target, reuse=False,
                                         allow_public_health_failure=allow_public_health_failure)
                contract = [p for p in pending if "contract" in p]
                if contract and hold_before_switch:
                    raise DeployError("a held release requires additive migrations; contract migrations are not allowed")
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
                    self.create(target, role, serving_alias=not hold_before_switch, created=created)
                    self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                        self.cfg.ready_timeout)
            if hold_before_switch:
                self._hold(state, target, record, allow_public_health_failure)
            else:
                self._finish_switch(state, current, target, record, create_background=True)
            if skip_check:
                self.log("note: this dry run did NOT verify the serving and background environment files "
                         "(startup check skipped because migrations are pending)")
        except DeployError as err:
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            self._abort(state, target, err, created)
            self._record_history(record)
            raise
        except (OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            step = (state.get("in_progress") or {}).get("step")
            if not hold_before_switch and step in (None, "migrate"):
                # Keep ordinary preparation errors outside the hold and switch recovery paths.
                raise
            self._unexpected(err, record)

    def rollback(self) -> None:
        state = load_main_state(self.cfg)
        self.require_idle(state)
        current, previous = state.get("current"), state.get("previous")
        if not current or not previous:
            raise DeployError("no previous release recorded; nothing to roll back to")
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
            if not self.ops.dry_run and isinstance(err, FallbackFailed):
                self._record_unrestored(state, err)
            self._record_history(record)
            raise
        except (OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            if not state.get("in_progress"):
                raise
            self._unexpected(err, record)

    def config_fingerprint(self) -> str:
        return hashlib.sha256(json.dumps(asdict(self.cfg), sort_keys=True).encode()).hexdigest()

    def upstream_fingerprint(self) -> str:
        return hashlib.sha256(self.ops.read_text(self.cfg.upstream_file).encode()).hexdigest()

    def _hold(self, state: dict[str, Any], target: Target, record: dict[str, Any],
              allow_public_health_failure: bool) -> None:
        if not self.ops.dry_run:
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
                    "upstream_fingerprint": self.upstream_fingerprint(),
                    "allow_public_health_failure": allow_public_health_failure}
            self._save_progress(state, held)
            record.update(result="paused", steps=self.timings, finished_at=now())
            self._record_history(record)
        self.log("paused before background handoff; use 'resume' or 'abort'; no automatic expiry")
        for role in SERVING_ROLES:
            self.log(f"   {role}: http://127.0.0.1:{target.ports[role]}/ (access via your tunnel)")

    def held_release(self, state: dict[str, Any], *, aborting: bool = False) -> tuple[dict[str, Any], Target]:
        held = state.get("in_progress")
        if not held:
            raise DeployError("no paused release; nothing changed")
        if held["phase"] == "aborting" and not aborting:
            raise DeployError("abort cleanup has started; retry 'abort', not 'resume'")
        if held["phase"] not in ("held", "aborting"):
            raise DeployError("an unfinished release needs manual recovery; inspect 'status'; this is not a pause")
        return held, Target.from_view(held["target"], self.cfg.runtime_token_file)

    def has_expected_held_networks(self, info: dict[str, Any], expected: dict[str, str], *, shared: bool) -> bool:
        networks = info.get("NetworkSettings", {}).get("Networks", {})
        return networks.keys() == expected.keys() and all(
            (self.cfg.serving_alias in (endpoint.get("Aliases") or [])) == shared
            for endpoint in networks.values())

    def validate_hold(self, state: dict[str, Any], held: dict[str, Any], target: Target) -> dict[str, str]:
        if state.get("current") != held["current"] or target.slot == state["current"]["slot"]:
            raise DeployError("current release changed during pause; inspect 'status' before recovery")
        if self.config_fingerprint() != held["config_fingerprint"]:
            raise DeployError("configuration changed during pause; restore it or abort")
        if self.upstream_fingerprint() != held["upstream_fingerprint"]:
            raise DeployError("upstream file changed during pause; restore it or abort")
        if self.ops.inspect(target.containers["background"]):
            raise DeployError("unexpected target background container; inspect and resolve before resuming")
        serving = {}
        for role in SERVING_ROLES:
            info = self.ops.inspect(target.containers[role]) or {}
            identity = held["containers"][role]
            if info.get("Id") != identity["id"] or info.get("Image") != identity["image_id"]:
                raise DeployError(f"held {role} is missing or its identity/image changed; restore it or abort")
            serving[role] = info
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
        for role, info in serving.items():
            if not self.has_expected_held_networks(info, networks, shared=False):
                raise DeployError(f"held {role} network isolation changed; restore it or abort")
        return networks

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
                                expected: dict[str, str], *, shared: bool) -> None:
        if not self.cfg.serving_alias:
            return
        for role in SERVING_ROLES:
            self.stop_owned_serving(target, role, identities[role])
        # Prepare every endpoint while both containers are stopped. Docker
        # brings up the configured endpoints before it runs the process.
        for role in SERVING_ROLES:
            name = target.containers[role]
            info = self.ops.inspect(name) or {}
            if (info.get("Id") != identities[role]
                    or (not self.ops.dry_run and info.get("State", {}).get("Running"))):
                raise DeployError(f"cannot configure networks for {role}; container identity or stop is unconfirmed")
            networks = info.get("NetworkSettings", {}).get("Networks", {})
            if networks.keys() - expected.keys():
                raise DeployError(f"{role} joined an unexpected network; inspect before continuing")
            aliases = ["--alias", name]
            if shared:
                aliases += ["--alias", self.cfg.serving_alias]
            for network, network_id in expected.items():
                if network in networks:
                    actual = networks[network].get("Aliases") or []
                    if name in actual and (self.cfg.serving_alias in actual) == shared:
                        continue
                    self.ops.run(["docker", "network", "disconnect", network_id, identities[role]], mutate=True)
                self.ops.run(["docker", "network", "connect", *aliases, network_id, identities[role]], mutate=True)
        # A stopped container can store configuration for a network that no
        # longer exists. Reject that before activating either container.
        if self.inspect_held_networks() != expected:
            raise DeployError("configured Docker networks changed during activation")
        if not self.ops.dry_run:
            for role in SERVING_ROLES:
                info = self.ops.inspect(target.containers[role]) or {}
                if info.get("Id") != identities[role] or not self.has_expected_held_networks(info, expected, shared=shared):
                    raise DeployError(f"prepared networking for held {role} is unconfirmed")
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

    def activate_hold(self, state: dict[str, Any], held: dict[str, Any], target: Target,
                      identities: dict[str, str], networks: dict[str, str]) -> None:
        changed_networks = False
        try:
            if self.cfg.serving_alias:
                changed_networks = True
                self.configure_held_networks(target, identities, networks, shared=True)
            # Drain and reconnect can take time. Check again before the handoff.
            self.check_held_background_port(target)
        except (Exception, KeyboardInterrupt) as err:
            if self.ops.dry_run:
                raise
            try:
                if changed_networks:
                    self.configure_held_networks(target, identities, networks, shared=False)
                self._save_progress(state, held)
            except (Exception, KeyboardInterrupt) as restore_err:
                raise FallbackFailed(f"held serving activation failed ({err}); restoring the pause failed too "
                                     f"({restore_err}); verify target networking and serving before clearing in_progress") from restore_err
            raise DeployError(f"held serving activation failed ({err}); pause restored; retry 'resume' or 'abort'") from err

    def resume(self) -> None:
        state = load_main_state(self.cfg)
        held, target = self.held_release(state)
        # Validation never changes the pause: fix health/config and retry, or abort.
        try:
            networks = self.validate_hold(state, held, target)
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            raise DeployError(f"{err or 'resume validation interrupted'}; pause unchanged; fix the cause and retry or abort") from err
        record = {"started_at": now(), "action": "resume", "image": target.image, "slot": target.slot,
                  "paused_at": held["held_at"]}
        identities = {role: held["containers"][role]["id"] for role in SERVING_ROLES}
        try:
            self._finish_switch(state, state["current"], target, record, create_background=True,
                                held=held, networks=networks)
        except DeployError as err:
            self._abort(state, target, err, identities)
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            self._record_history(record)
            raise
        except (OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            self._unexpected(err, record)

    def abort(self) -> None:
        state = load_main_state(self.cfg)
        held, target = self.held_release(state, aborting=True)
        current = state.get("current") or {}
        if target.slot == current.get("slot") or set(target.containers.values()) & set(current.get("containers", {}).values()):
            raise DeployError("held containers overlap the current release; inspect status before manual recovery")
        if self.ops.inspect(target.containers["background"]):
            raise DeployError("unexpected target background container; inspect and resolve before aborting")
        record = {"started_at": now(), "action": "abort", "image": target.image, "slot": target.slot}
        aborting = {key: value for key, value in held.items() if key not in ("reason", "failed_at", "process")}
        aborting["phase"] = "aborting"
        try:
            # Persist intent before cleanup; after an interruption only abort may retry.
            self._save_progress(state, self._with_process(aborting))
            for role in SERVING_ROLES:
                self.remove_owned_serving(target, role, held["containers"][role]["id"])
        except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            if (state.get("in_progress") or {}).get("phase") == "aborting":
                self._save_progress(state, {**aborting, "reason": str(err) or "interrupted", "failed_at": now()})
            raise DeployError(f"abort incomplete; pause retained, retry 'abort': {err}") from err
        if not self.ops.dry_run:
            try:
                self._save_progress(state, None)
            except OSError as err:
                raise DeployError("held containers removed but state write failed. Inspect 'status' and "
                                  "retry 'abort' if the phase is still aborting") from err
            record.update(result="ok", finished_at=now())
            self._record_history(record)
        self.log("aborted held release; additive migrations and exported assets are kept")

    def _unexpected(self, err: BaseException, record: dict[str, Any]) -> None:
        failure = FallbackFailed(f"operation interrupted or command outcome unknown ({type(err).__name__}); inspect 'status', "
                                 "restore routing/background manually before clearing in_progress")
        if not self.ops.dry_run:
            # The last atomic write may have failed: the persisted state is
            # authoritative, not the in-memory transition we attempted.
            persisted = load_main_state(self.cfg)
            phase = (persisted.get("in_progress") or {}).get("phase")
            if phase == "deploying":
                self._record_unrestored(persisted, failure)
            record.update(result="failed", error=str(failure), steps=self.timings, finished_at=now())
            self._record_history(record)
            if phase == "held":
                raise DeployError("operation interrupted while recording a pause; pause retained; "
                                  "inspect status, then resume or abort") from err
        raise failure from err

    def _finish_switch(self, state: dict[str, Any], current: dict[str, Any], target: Target,
                       record: dict[str, Any], create_background: bool, *, held: dict[str, Any] | None = None,
                       networks: dict[str, str] | None = None) -> None:
        if held is not None:
            # Ordinary serving containers already got their shared aliases at create time.
            self._mark(state, target, "activate held serving")
            with self.step("activate held serving"):
                identities = {role: held["containers"][role]["id"] for role in SERVING_ROLES}
                assert networks is not None  # Supplied by resume's read-only validation.
                self.activate_hold(state, held, target, identities, networks)
        view = target.view()
        self._mark(state, target, "background handoff")
        with self.step("background handoff"):
            try:
                self.handoff_background(current, target, create=create_background)
            except FallbackFailed:
                raise
            except (Exception, KeyboardInterrupt) as err:
                if self.ops.dry_run:
                    raise
                try:
                    owner = self.owner_runtime(current) or {}
                    new = self.owner_runtime(view) or {}
                    if not owner.get("ready") or not owner.get("owns_background") or new.get("owns_background"):
                        raise DeployError("previous background ownership is unconfirmed")
                    self.ops.run(["docker", "update", "--restart=unless-stopped", current["containers"]["background"]],
                                 mutate=True)
                except (Exception, KeyboardInterrupt) as restore_err:
                    raise FallbackFailed(f"background handoff failed ({err}); recovery is unconfirmed ({restore_err}); "
                                         "inspect status and restore the previous background") from restore_err
                raise DeployError(str(err) or "background handoff interrupted; previous background is ready") from err
        self._mark(state, target, "switch")
        with self.step("switch"):
            try:
                self.switch_to(view)
            except RoutingFileNotRestored as err:
                # Reload was never attempted. The old route still needs its background.
                try:
                    self.hand_back(target, current)
                except FallbackFailed as handoff_err:
                    raise FallbackFailed(f"{err}; {handoff_err}") from handoff_err
                raise
            except FallbackFailed:
                raise
            except DeployError as err:
                self.log("   switch failed; handing the background back")
                self.hand_back(target, current)
                raise DeployError(f"{err}; the previous release owns the background again") from err
        if not self.ops.dry_run:
            self._mark(state, target, "guard")
            with self.step("guard"):
                try:
                    self.guard(target)
                except (Exception, KeyboardInterrupt) as err:
                    reason = str(err) or type(err).__name__
                    self.restore_after_guard(target, current, reason)
                    self._save_progress(state, None)
                    raise DeployError(f"guard failed ({reason}); the previous release serves traffic and owns the "
                                      "background again; the new serving containers are draining or stopped and kept "
                                      "for inspection") from err
        self._mark(state, target, "retire previous serving")
        with self.step("retire previous serving"):
            try:
                self.retire_serving(current)
            except (Exception, KeyboardInterrupt) as err:
                raise FallbackFailed(f"new release is serving; previous serving retirement incomplete: {err}; "
                                     "inspect status and finish retirement before updating current/previous") from err
        view["deployed_at"] = now()
        state.update(previous=current, current=view, in_progress=None)
        if not self.ops.dry_run:
            save_state(self.cfg, state)
        record.update(result="ok", steps=self.timings, finished_at=now())
        self._record_history(record)
        self.log(f"done: slot {target.slot} serving {target.image}")

    def _mark(self, state: dict[str, Any], target: Target, step: str) -> None:
        progress = {"phase": "deploying", "target": target.view(), "step": step, "since": now()}
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
                          reason=str(err) or type(err).__name__, failed_at=now())
            self._save_progress(state, failed)

    def _abort(self, state: dict[str, Any], target: Target, err: DeployError, owned: dict[str, str]) -> None:
        """Clear a failure only when the old release still serves or recovery has finished."""
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
            self._record_unrestored(state, err)
            self.log(f"FAILED AND NOT RESTORED: {err}")
            return
        if step in ("start serving", "background handoff", "switch"):
            try:
                for role, cid in owned.items():
                    self.remove_owned_serving(target, role, cid)
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as cleanup_err:
                failure = FallbackFailed(f"cleanup incomplete ({cleanup_err}); inspect status and remove the "
                                         "unused target containers before clearing in_progress")
                self._record_unrestored(state, failure)
                self.log(f"FAILED AND NOT RESTORED: {failure}")
                return
        if step:
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
    if progress and progress["phase"] in ("deploying", "aborting"):
        report["in_progress"]["process_running"] = release_running(ops, progress)
    if progress and progress["phase"] in ("held", "aborting"):
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
    if phase == "failed":
        alerts.append({"alert": ["release_incomplete"], "at": now(), "in_progress": progress})
    elif phase in ("deploying", "aborting") and release_running(ops, progress) is not True:
        # Avoid an alert if the sampled process just completed and saved a new phase.
        if load_main_state(cfg).get("in_progress") == progress:
            alert = "release_incomplete" if phase == "deploying" else "abort_incomplete"
            alerts.append({"alert": [alert], "at": now(), "in_progress": progress})
    current = state.get("current")
    if not current:
        return alerts
    deployer = Deployer(cfg, ops)
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
    sub.add_parser("rollback", help="switch back to the previous slot")
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
        read_only_plan = args.dry_run and args.command in ("resume", "abort")
        with release_lock(cfg.lock_file, create=not read_only_plan):
            deployer = Deployer(cfg, ops)
            if args.command == "main":
                deployer.deploy(args.image, allow_contract=args.allow_contract,
                                allow_public_health_failure=args.allow_public_health_failure,
                                hold_before_switch=args.hold_before_switch)
            elif args.command == "rollback":
                deployer.rollback()
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
