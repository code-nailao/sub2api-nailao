#!/usr/bin/env python3
"""Fixed release entrypoint for Sources Main.

One command replaces Main serving and the unique background with one image:

    deploy.py main <image>     preflight, migrate, check, start, hand off,
                               switch, guard (auto-rollback), retire
    deploy.py resume           validate a held release, then finish switching
    deploy.py abort            discard the held serving containers
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
import hashlib
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
from dataclasses import asdict, dataclass, field
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
# One route decision is taken from one snapshot of probes. The budget bounds
# how long the snapshot may spend probing at all; the TTL bounds how long a
# snapshot may sit around before a write refuses to use it. Both are checked by
# the write entry itself (`install_from`), not by its callers.
PROBE_BUDGET_SECONDS = 12.0
# Time kept back for the last pair of a snapshot. The standby is probed
# before the primary so the primary is the freshest at the write; without a
# reserve a hung standby can spend the whole budget and leave the primary
# unchecked, which turns "no backup" into "no rollback at all".
PROBE_RESERVE_SECONDS = 8.0
SNAPSHOT_TTL_SECONDS = 30.0
# `nginx -t` and a reload are local commands; the 600s default is for image and
# container work and would turn a stuck reload into minutes of "written but
# not loaded".
NGINX_OP_TIMEOUT_SECONDS = 15.0
# Docker is consulted only to explain an application probe that failed. It
# never decides a route, so it may not hold the snapshot open for 30s.
DOCKER_NOTE_TIMEOUT_SECONDS = 0.5
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


class RouteRefused(DeployError):
    """A route was not written because the probe taken immediately before the
    write no longer held. Nothing was changed: the loaded route stays."""


class AdminRouteLost(RouteRefused):
    """The admin upstream was written and its target stopped answering. The
    tool stops on the loaded route: no second reload, no other slot."""


class RouteUnverified(RouteRefused):
    """A write was refused because its probe snapshot could not justify it: the
    snapshot ran out of probe budget, or it aged past the TTL. Nothing was
    written and no role is known to be unusable, so the candidate containers
    are kept rather than removed."""


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
    hold_warning_seconds: int = 86400

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


def render_upstreams(serving_port: int, standby_port: int | None, background_port: int) -> str:
    backup = ("" if standby_port is None
              else f"    server 127.0.0.1:{standby_port} backup max_fails=1 fail_timeout=5s;\n")
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


SERVING_BLOCK = re.compile(r"upstream sources_main_serving \{.*?\n\}", re.S)


def replace_serving(text: str, ports: list[int]) -> str:
    """Change only the serving upstream; the loaded admin block stays as it is.

    One write names one upstream block, so the roles it writes are exactly the
    roles that were probed for it. `render_upstreams` remains for the first
    write, when the file does not exist yet."""
    primary = ports[0]
    backup = ("" if len(ports) < 2
              else f"    server 127.0.0.1:{ports[1]} backup max_fails=1 fail_timeout=5s;\n")
    block = ("upstream sources_main_serving {\n    zone sources_main_serving 64k;\n"
             f"    server 127.0.0.1:{primary} max_fails=1 fail_timeout=5s;\n{backup}}}")
    replaced, count = SERVING_BLOCK.subn(lambda _match: block, text, count=1)
    if not count:
        raise DeployError("the loaded upstream file names no serving upstream to replace")
    return replaced


def replace_admin_port(text: str, port: int) -> str:
    """Change only the admin upstream; the loaded serving block stays as it is."""
    return re.sub(r"(upstream sources_main_admin \{.*?server 127\.0\.0\.1:)\d+",
                  lambda match: match.group(1) + str(port), text, count=1, flags=re.S)


def loaded_upstream_ports(text: str) -> tuple[list[int], int | None]:
    """Serving ports and admin port exactly as the loaded file names them."""
    serving = re.search(r"upstream sources_main_serving \{.*?\n\}", text, re.S)
    admin = re.search(r"upstream sources_main_admin \{.*?server 127\.0\.0\.1:(\d+)", text, re.S)
    ports = [int(p) for p in re.findall(r"server 127\.0\.0\.1:(\d+)", serving.group(0))] if serving else []
    return ports, (int(admin.group(1)) if admin else None)


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
    image_id: str | None = None  # A held release reuses the image it tested.

    def view(self) -> dict[str, Any]:
        return {"slot": self.slot, "image": self.image, "containers": self.containers, "ports": self.ports,
                "runtime_token_files": self.token_files}

    @classmethod
    def from_view(cls, view: dict[str, Any], default_token: str) -> "Target":
        tokens = {role: (view.get("runtime_token_files") or {}).get(role) or default_token for role in ROLES}
        return cls(slot=view["slot"], image=view.get("image", ""), containers=dict(view["containers"]),
                   ports=dict(view["ports"]), token_files=tokens)


@dataclass(frozen=True)
class RoleProbe:
    """One role's usability at one instant.

    Availability is not a property of a container; it is the result of a probe
    that a route write must take again immediately before writing. `outcome`
    keeps "did not answer in time", "did not answer" and "could not be checked"
    apart so reports never hide one behind another."""

    slot: str
    role: str
    name: str
    port: int
    usable: bool
    outcome: str
    detail: str
    at: str

    def record(self) -> dict[str, Any]:
        return {"slot": self.slot, "role": self.role, "name": self.name, "port": self.port,
                "usable": self.usable, "outcome": self.outcome, "detail": self.detail, "at": self.at}

    def evidence(self) -> str:
        return f"slot {self.slot} {self.role} ({self.name}, port {self.port}): {self.detail}"


@dataclass(frozen=True)
class RouteSnapshot:
    """One round of probes, taken as one unit.

    This is the only thing a route write may be built from. `probes` holds the
    roles that were actually probed; `exhausted` names the roles the budget did
    not reach, which are neither usable nor unusable. Every unusable probe is
    turned into a finding while the snapshot is built, so a failure cannot be
    overwritten by a later probe of the same decision."""

    taken_at: float
    probes: dict[tuple[str, str], RoleProbe]
    exhausted: tuple[tuple[str, str], ...]
    findings: tuple[dict[str, Any], ...]
    budget: float
    times: dict[tuple[str, str], float] = field(default_factory=dict)

    def age(self, ops: "Ops") -> float:
        return ops.monotonic() - self.taken_at

    def age_of(self, ops: "Ops", slot: str, role: str) -> float | None:
        """How long ago this pair answered, or None when it was not checked.

        Age is per pair, not per snapshot: a standby that took 30s must not
        make a primary probed a second ago look stale."""
        taken = self.times.get((slot, role))
        return None if taken is None else ops.monotonic() - taken

    def probe(self, slot: str, role: str) -> RoleProbe | None:
        return self.probes.get((slot, role))

    @property
    def incomplete(self) -> bool:
        return bool(self.exhausted)


@dataclass(frozen=True)
class Verification:
    """What the loaded route looked like when it was probed after a write.

    Route state (`serving_route` / `admin_route`, and whether `in_progress` may
    be cleared) comes from here and from nowhere else. The probes that decided
    what to write only decide what to write."""

    at: float           # ops.monotonic() when these probes started
    at_text: str        # wall clock, for reports
    snapshot: RouteSnapshot

    @property
    def findings(self) -> list[dict[str, Any]]:
        return list(self.snapshot.findings)

    @property
    def clean(self) -> bool:
        return not self.snapshot.findings and not self.snapshot.incomplete


@dataclass
class RouteOutcome:
    written: bool
    text: str
    serving_slot: str
    admin_slot: str | None
    admin_port: int | None
    admin_source: str
    probes: dict[str, RoleProbe]
    issues: list[dict[str, Any]]


def _docker_outcome(note: str) -> str:
    """Classify Docker's own view without letting it decide ownership."""
    if "unreadable" in note:
        return "state unknown"
    if "stopped" in note:
        return "stopped"
    return "missing"


def route_issue(kind: str, probe: RoleProbe) -> dict[str, Any]:
    return {**probe.record(), "kind": kind, "found_at": now()}


def issue_key(issue: dict[str, Any]) -> tuple[str, str, str]:
    """One role failing one way. The same key in a later round is a new
    observation of the same role, not a copy of the earlier one."""
    return (issue["slot"], issue["role"], issue["kind"])


def describe_route_issues(issues: list[dict[str, Any]]) -> str:
    parts = []
    for i in issues:
        where = f"slot {i['slot']} {i['role']} ({i['name']}, port {i['port']})"
        if i["kind"] == "written_role_exited":
            parts.append(f"{where} {i['detail']} was written into the route, "
                         f"then found {i['outcome']} at {i['found_at']}")
        elif i["kind"] == "probe_budget_exhausted":
            parts.append(f"{where} {i['detail']} at {i['found_at']}")
        else:
            parts.append(f"{where} {i['detail']} at {i['found_at']}")
    return "; ".join(parts)


class Deployer:
    def __init__(self, cfg: Config, ops: Ops):
        self.cfg = cfg
        self.ops = ops
        self.timings: list[dict[str, Any]] = []
        # Backgrounds already started in this guard rollback. A failed start is
        # not retried in the same round; it is reported instead.
        self._rollback_starts: set[str] | None = None
        # Route findings recorded in this run, one per (slot, role, kind). A
        # finding that a previous run wrote into the state file belongs to
        # that run, so the same role failing again is recorded here anew.
        self._findings: dict[tuple[str, str, str], dict[str, Any]] = {}
        # The text this process last told Nginx to load, recorded only after the
        # reload command was accepted, and when that write finished in
        # monotonic time. `loaded_route_ports` reads this instead of the file,
        # which may not be loaded yet and is restored when `nginx -t` rejects.
        self._loaded: str | None = None
        self._last_write_at: float | None = None
        # The most recent post-write verification: the only evidence that may
        # end a run in success.
        self._verification: Verification | None = None
        # The probe that justified the last admin move, kept apart from the
        # state so a later exit does not erase why admin was moved.
        self._admin_move: dict[str, Any] | None = None
        # Every slot view this run has probed. The loaded file can name a role
        # whose slot the current write was not given (the admin entry usually
        # belongs to the other slot), and it still has to be verified.
        self._view_index: dict[str, dict[str, Any]] = {}

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

    def health(self, port: int) -> bool:
        return self.ops.http_json(f"http://127.0.0.1:{port}/health")[0] == 200

    def health_within(self, port: int, deadline: float | None) -> bool:
        """`/health` with what is left of the snapshot's budget as its timeout."""
        timeout = RUNTIME_STATUS_TIMEOUT_SECONDS
        if deadline is not None:
            timeout = min(timeout, deadline - self.ops.monotonic())
            if timeout <= 0:
                return False
        return self.ops.http_json(f"http://127.0.0.1:{port}/health", timeout=timeout)[0] == 200

    # availability probes ------------------------------------------------------
    def probe_role(self, view: dict[str, Any], role: str, *,
                   deadline: float | None = None) -> RoleProbe:
        """Probe one role now. Never cached, never inferred from an earlier
        answer: a route write must call this immediately before it writes.
        Only the application probe decides; Docker is consulted to explain a
        missing answer, never to decide one. That keeps the successful release
        command sequence free of extra `docker inspect` calls."""
        if role == "background":
            return self._probe_background(view, deadline)
        return self._probe_serving(view, role, deadline)

    def docker_note(self, name: str) -> str | None:
        """Why the role gave no application answer, when Docker can say.

        Bounded: Docker explains a failed probe, it never decides a route, so
        it may not hold the snapshot open for its own 30s query timeout."""
        try:
            info = self.ops.inspect(name, timeout=DOCKER_NOTE_TIMEOUT_SECONDS)
        except Exception as err:  # noqa: BLE001 - unreadable state is its own outcome
            return f"docker state unreadable ({err})"
        if info is None:
            return "container is missing"
        if not (info.get("State") or {}).get("Running"):
            return "container is stopped"
        return None

    def _probe_serving(self, view: dict[str, Any], role: str, deadline: float | None = None) -> RoleProbe:
        name, port = view["containers"][role], view["ports"][role]

        def result(usable: bool, outcome: str, detail: str) -> RoleProbe:
            return RoleProbe(view["slot"], role, name, port, usable, outcome, detail, now())

        try:
            token = self.token_for(view, role)
        except OSError as err:
            return result(False, "unreadable", f"runtime token unreadable ({err})")
        started = self.ops.monotonic()
        runtime = self.runtime_request_before(
            lambda timeout: self.runtime(port, token, timeout=timeout), deadline)
        elapsed = self.ops.monotonic() - started
        if not runtime:
            note = self.docker_note(name)
            if note:
                return result(False, _docker_outcome(note), note)
            if elapsed >= RUNTIME_STATUS_TIMEOUT_SECONDS:
                return result(False, "timeout",
                              f"no runtime answer within the {RUNTIME_STATUS_TIMEOUT_SECONDS:.0f}s probe timeout")
            if deadline is not None and self.ops.monotonic() >= deadline:
                return result(False, "timeout", "no runtime answer within the probe budget")
            return result(False, "no answer", "runtime request was refused or reset")
        if runtime.get("role") not in ("serving", "standby"):
            return result(False, "wrong role", f"runtime reports role {runtime.get('role')!r}")
        if not runtime.get("ready"):
            return result(False, "not ready", "runtime reports ready=false")
        try:
            healthy = self.health_within(port, deadline)
        except Exception as err:  # noqa: BLE001
            return result(False, "no answer", f"/health probe failed ({err})")
        if not healthy:
            return result(False, "unhealthy", "/health did not return 200")
        return result(True, "answers", "runtime ready and /health returned 200")

    def _probe_background(self, view: dict[str, Any], deadline: float | None) -> RoleProbe:
        name, port = view["containers"]["background"], view["ports"]["background"]

        def result(usable: bool, outcome: str, detail: str) -> RoleProbe:
            return RoleProbe(view["slot"], "background", name, port, usable, outcome, detail, now())

        started = self.ops.monotonic()
        try:
            runtime = self.owner_runtime(view, deadline=deadline)
        except Exception as err:  # noqa: BLE001
            return result(False, "unreadable", f"runtime probe failed ({err})")
        elapsed = self.ops.monotonic() - started
        if runtime is None:
            note = self.docker_note(name)
            if note:
                return result(False, _docker_outcome(note), note)
            if elapsed >= RUNTIME_STATUS_TIMEOUT_SECONDS:
                return result(False, "timeout",
                              f"no runtime answer within the {RUNTIME_STATUS_TIMEOUT_SECONDS:.0f}s probe timeout")
            if deadline is not None and self.ops.monotonic() >= deadline:
                return result(False, "timeout", "no runtime answer within the probe budget")
            return result(False, "no answer", "runtime request was refused or reset")
        if runtime.get("role") != "background":
            return result(False, "wrong role", f"runtime reports role {runtime.get('role')!r}")
        if not runtime.get("owns_background"):
            return result(False, "not the owner", "runtime does not hold the background lock")
        if not (runtime.get("ready")
                or (runtime.get("draining") and runtime.get("admin_during_drain") == "read-only")):
            return result(False, "not ready", "runtime is not ready for admin reads")
        try:
            healthy = self.health_within(port, deadline)
        except Exception as err:  # noqa: BLE001
            return result(False, "no answer", f"/health probe failed ({err})")
        if not healthy:
            return result(False, "unhealthy", "/health did not return 200")
        if runtime.get("draining"):
            return result(True, "answers", "holds the background lock and answers admin reads while draining")
        return result(True, "answers", "holds the background lock and answers /health 200")

    def recovery_timeout(self, deadline: float | None, maximum: float) -> float:
        if deadline is None:
            return maximum
        remaining = deadline - self.ops.monotonic()
        if remaining <= 0:
            raise DeployError("background recovery deadline reached")
        return min(maximum, remaining)

    def wait_ready(self, port: int, token: str, role: str, timeout: int,
                   container: str | None = None, poll: float = 2, *, deadline: float | None = None,
                   check_health: bool = False) -> dict[str, Any]:
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
                    # Routing that a person depends on must not move before
                    # the new owner answers its health probe too.
                    if not check_health or self.health(port):
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

    def create(self, target: Target, role: str, *, include_serving_alias: bool = True) -> None:
        name = target.containers[role]
        shared_alias = self.cfg.serving_alias if include_serving_alias and role in SERVING_ROLES else ""
        self.prepare_per_container_copies(role, name)
        args = ["docker", "create", "--name", name, "--hostname", name,
                "--user", self.cfg.user, "--entrypoint", "/app/sub2api",
                "--restart", "unless-stopped",
                "--memory", self.cfg.memory, "--cpus", self.cfg.cpus,
                "--pids-limit", str(self.cfg.pids_limit),
                "--log-opt", f"max-size={self.cfg.log_max_size}", "--log-opt", f"max-file={self.cfg.log_max_file}",
                "--publish", f"127.0.0.1:{target.ports[role]}:8080",
                "--network", self.cfg.networks[0], "--network-alias", name,
                *(["--network-alias", shared_alias] if shared_alias else []),
                "--label", f"com.sources.deploy.slot={target.slot}",
                "--label", f"com.sources.deploy.role={role}",
                "--label", f"com.sources.deploy.created={now()}"]
        args += self.docker_env_args(role)
        args += self.mount_args(self.mounts_for(role, name))
        args.append(target.image_id or target.image)
        self.ops.run(args, mutate=True)
        aliases = ["--alias", name]
        if shared_alias:
            aliases += ["--alias", shared_alias]
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

    def snapshot_now(self, pairs: list[tuple[dict[str, Any], str]], *,
                     budget: float | None = None, deadline: float | None = None,
                     note: str = "", kind: str = "probe_not_usable") -> RouteSnapshot:
        """Probe every (view, role) pair now, serially, inside one budget.

        This is the only entry that produces probe results. Serial on purpose:
        the order is part of what a write can claim, and the tests inject by
        order. Each probe is capped by what is left of the budget, and a role
        the budget does not reach is recorded as "not checked" - neither usable
        nor unusable. Every failed probe records its own finding here, so a
        first failure cannot be overwritten by a later probe of the same
        decision."""
        budget = PROBE_BUDGET_SECONDS if budget is None else budget
        started = self.ops.monotonic()
        until = started + budget
        if deadline is not None:
            until = min(until, deadline)
        probes: dict[tuple[str, str], RoleProbe] = {}
        times: dict[tuple[str, str], float] = {}
        exhausted: list[tuple[str, str]] = []
        findings: list[dict[str, Any]] = []
        # Every pair but the last is capped short of the budget, so the final
        # probe - the one the write rests on - always gets its own time.
        reserve = PROBE_RESERVE_SECONDS if len(pairs) > 1 else 0.0
        last = len(pairs) - 1
        for index, (view, role) in enumerate(pairs):
            self._view_index.setdefault(view["slot"], view)
            key = (view["slot"], role)
            if key in probes or key in exhausted:
                continue
            # The final pair gets its own window starting now: an earlier probe
            # that ignores its deadline must not be able to starve the role the
            # write rests on. Its age still stays inside the snapshot TTL.
            cap = (self.ops.monotonic() + reserve) if (reserve and index == last) else (
                until if index == last else min(until, until - reserve))
            if self.ops.monotonic() >= cap:
                exhausted.append(key)
                findings.append({
                    "slot": view["slot"], "role": role,
                    "name": view["containers"][role], "port": view["ports"][role],
                    "kind": "probe_budget_exhausted",
                    "detail": f"was not checked: the {budget:.0f}s probe budget ran out",
                    "found_at": now(),
                })
                continue
            probe = self.probe_role(view, role, deadline=cap)
            probes[key] = probe
            times[key] = self.ops.monotonic()
            if not probe.usable:
                findings.append(route_issue(kind, probe))
        snapshot = RouteSnapshot(started, probes, tuple(exhausted), tuple(findings), budget, times)
        self._record_route_issues(findings, note or "probe snapshot")
        return snapshot

    def install_from(self, snapshot: RouteSnapshot, build: Callable[[str], str], *,
                     note: str = "", required: set[tuple[str, str]] | None = None) -> str:
        """The only way the upstream file changes.

        A snapshot that is incomplete or past its TTL cannot write: the caller
        gets `RouteUnverified`, the file is not touched, and - the difference
        from an unusable role - no role is known to be broken, so the
        candidate containers are kept. `nginx -t` and the reload run after the
        rename with their own short timeout, so they cannot extend the TTL."""
        # A role the write does not depend on may go unchecked: a standby the
        # budget did not reach simply stays out of the route. Only a role the
        # caller needs can block the write.
        blocking = ([pair for pair in snapshot.exhausted if pair in required]
                    if required is not None else list(snapshot.exhausted))
        if blocking:
            self.log(f"   route not written ({note}): the probe budget ran out before "
                     f"{len(blocking)} role(s) were checked")
            detail = ", ".join(f"slot {slot} {role}" for slot, role in blocking)
            raise RouteUnverified(
                f"not writing the route ({note}): the {snapshot.budget:.0f}s probe budget ran out before "
                f"{detail} could be checked; the loaded route is unchanged and the candidate containers "
                "are kept")
        # The TTL applies per pair, not to the moment the round started: a
        # standby that took 30s answering must not make a primary probed a
        # second ago look stale. Only the pairs this write rests on can expire
        # it; an optional pair that went stale is dropped by its caller.
        needed = required if required is not None else set(snapshot.probes)
        stale: list[tuple[str, str, float | None]] = []
        for pair in needed:
            pair_age = snapshot.age_of(self.ops, *pair)
            if pair_age is None or pair_age > SNAPSHOT_TTL_SECONDS:
                stale.append((pair[0], pair[1], pair_age))
        if stale:
            worst = max((age for _, _, age in stale if age is not None), default=SNAPSHOT_TTL_SECONDS)
            detail = ", ".join(
                f"slot {slot} {role}" + (" was not checked" if age is None else f" answered {age:.1f}s ago")
                for slot, role, age in stale)
            self.log(f"   route not written ({note}): {detail}")
            raise RouteUnverified(
                f"not writing the route ({note}): {detail}, past the {SNAPSHOT_TTL_SECONDS:.0f}s snapshot "
                "TTL; the loaded route is unchanged and the candidate containers are kept")
        text = build(self.current_upstreams() or "")
        self._install(text)
        return text

    def _install(self, text: str) -> None:
        """Write the file and reload Nginx. Private: every Main route write
        goes through `install_from`, which is where the snapshot is checked
        (Portal reuses this for its own single upstream)."""
        # Read through the Ops boundary, not the filesystem: the file may not
        # exist yet (first deploy) and the restore below depends on knowing
        # exactly what was there before this write.
        try:
            previous = self.ops.read_text(self.cfg.upstream_file)
        except OSError:
            previous = None
        self.ops.write_file_atomic(self.cfg.upstream_file, text)

        def restore(why: str) -> DeployError:
            # Every way `nginx -t` can fail - a non-zero exit, a hang, a binary
            # that cannot be run - means Nginx never loaded this text. The disk
            # must not keep it either, or `status` would report a route nobody
            # serves from.
            if previous is not None:
                self.ops.write_file_atomic(self.cfg.upstream_file, previous)
            return DeployError(f"{why}; previous file restored, routing unchanged")

        try:
            test = self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True, check=False,
                                timeout=NGINX_OP_TIMEOUT_SECONDS)
        except (OSError, subprocess.SubprocessError) as err:
            raise restore(f"nginx -t could not finish ({err})") from err
        if test.returncode != 0:
            raise restore(f"nginx -t rejected the upstream file (exit {test.returncode})")
        # A zero exit means the signal was accepted, not that Nginx loaded the
        # file. `_loaded` records only what this process told it to load.
        try:
            self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True, timeout=NGINX_OP_TIMEOUT_SECONDS)
        except (OSError, subprocess.SubprocessError) as err:
            # A reload that does not report back is not a validation failure:
            # the signal may already have been delivered and the new route may
            # be the one serving. Restoring the file here would make `status`
            # and the cleanup below believe the candidate is gone, and the
            # cleanup would then remove containers that answer requests. The
            # file stays as written, the route counts as this process's route,
            # and the caller puts the previous route back with a fresh write.
            self._loaded = text
            self._last_write_at = self.ops.monotonic()
            raise DeployError(f"nginx reload did not report back ({err}); the loaded route is unknown "
                              "until it is verified again") from err
        self._loaded = text
        self._last_write_at = self.ops.monotonic()

    def current_upstreams(self) -> str | None:
        try:
            return self.ops.read_text(self.cfg.upstream_file)
        except OSError:
            return None

    def loaded_text(self) -> str:
        """The route this process last told Nginx to load, else the file."""
        if self._loaded is not None:
            return self._loaded
        return self.current_upstreams() or ""

    def loaded_route_ports(self) -> tuple[list[int], int | None]:
        return loaded_upstream_ports(self.loaded_text())

    @staticmethod
    def admin_port_slot(port: int | None, views: tuple[dict[str, Any], ...]) -> str | None:
        for view in views:
            if port is not None and view["ports"]["background"] == port:
                return view["slot"]
        return None

    @staticmethod
    def distinct_views(*views: dict[str, Any]) -> tuple[dict[str, Any], ...]:
        """One view per slot, first occurrence wins."""
        chosen: dict[str, dict[str, Any]] = {}
        for view in views:
            chosen.setdefault(view["slot"], view)
        return tuple(chosen.values())

    def _route_progress(self) -> dict[str, Any]:
        state = load_state(self.cfg)
        progress = state.get("in_progress")
        if not progress:
            progress = state["in_progress"] = {"since": now(), "phase": "deploying"}
        progress.setdefault("phase", "deploying")
        return progress, state

    def _record_route_issues(self, issues: list[dict[str, Any]], note: str) -> None:
        """Record what the probes of this round actually found.

        The unresolved list is recomputed from this round's findings: a role
        that fails again after an earlier round, or after a run that ended
        without resolving it, is down now and is recorded again with the time
        it was found. Only a role this run has already reported is collapsed,
        so one run names a failing role once."""
        if not issues or self.ops.dry_run:
            return
        try:
            progress, state = self._route_progress()
            kept = {issue_key(i): i for i in progress.get("route_issues") or []}
            fresh: list[dict[str, Any]] = []
            for issue in issues:
                key = issue_key(issue)
                if key in self._findings:
                    continue
                self._findings[key] = issue
                kept[key] = issue
                fresh.append(issue)
            if not fresh:
                return
            progress["route_issues"] = list(kept.values())
            # Durable: a release that succeeds clears in_progress, but a role
            # that was written and then found gone must stay visible in status
            # and in the watch until a verification proves it answers again.
            kept_state = {issue_key(i): i for i in state.get("route_findings") or []}
            kept_state.update({issue_key(i): i for i in fresh})
            state["route_findings"] = list(kept_state.values())
            save_state(self.cfg, state)
            self.log(f"   route issue ({note}): {describe_route_issues(fresh)}")
        except Exception as err:  # noqa: BLE001 - never mask the routing failure
            self.log(f"   WARNING: could not record a route issue: {err}")

    def record_verification(self, verification: Verification, *, note: str,
                            views: tuple[dict[str, Any], ...]) -> None:
        """Route state from the post-write probes and from nothing else.

        `admin_route.source` has exactly three values: `verified`, `no
        background answers` and `unverified (probe budget)`. There is no
        `probed`: having probed before a write is not a state of the loaded
        route."""
        if self.ops.dry_run:
            return
        snapshot = verification.snapshot
        serving_ports, admin_port = self.loaded_route_ports()
        by_port = {probe.port: probe for probe in snapshot.probes.values()}
        serving_probes = [by_port[p] for p in serving_ports if p in by_port]
        primary = next((p for p in serving_probes if p.role == "serving"),
                       serving_probes[0] if serving_probes else None)
        backup = next((p for p in serving_probes if p.role == "standby"), None)
        serving_slot = primary.slot if primary else self.admin_port_slot(admin_port, views)
        admin_probe = by_port.get(admin_port) if admin_port is not None else None
        budget_missed = any(f["kind"] == "probe_budget_exhausted" and f["role"] == "background"
                            for f in snapshot.findings)
        if admin_port is None or (admin_probe is not None and not admin_probe.usable):
            admin_source = "no background answers"
        elif admin_probe is not None:
            admin_source = "verified"
        elif budget_missed:
            admin_source = "unverified (probe budget)"
        else:
            admin_source = "no background answers"
        restore = None
        if admin_source == "no background answers" and admin_port is not None:
            restore = self.admin_port_slot(admin_port, views) or serving_slot
        serving_route = {
            "at": verification.at_text, "note": note, "slot": serving_slot,
            "primary": primary.record() if primary else None,
            "backup": backup.record() if backup else None,
            "written": [p.port for p in serving_probes if p.usable],
            "excluded": [p.record() for p in serving_probes if not p.usable],
        }
        admin_route = {
            "at": verification.at_text, "note": note,
            "slot": admin_probe.slot if admin_source == "verified" and admin_probe else None,
            "port": admin_port, "source": admin_source,
            "probe": admin_probe.record() if admin_probe else None,
            "restore": restore,
        }
        try:
            progress, state = self._route_progress()
            progress["serving_route"] = serving_route
            progress["admin_route"] = admin_route
            healthy = {(probe.slot, probe.role) for probe in snapshot.probes.values() if probe.usable}
            if healthy:
                state["route_findings"] = [i for i in state.get("route_findings") or []
                                           if (i["slot"], i["role"]) not in healthy]
            admin_move = self._admin_move
            if admin_move is None and isinstance(state.get("last_route"), dict):
                admin_move = state["last_route"].get("admin_move")
            if admin_move is not None:
                progress["admin_move"] = admin_move
            # Durable copy: a successful release clears in_progress, and the
            # evidence for the last write must survive that.
            state["last_route"] = {"at": verification.at_text, "note": note,
                                   "serving": serving_route, "admin": admin_route,
                                   "admin_move": admin_move}
            save_state(self.cfg, state)
        except Exception as err:  # noqa: BLE001
            self.log(f"   WARNING: could not record the route verification: {err}")

    def known_views(self, views: tuple[dict[str, Any], ...] | list[dict[str, Any]]
                    ) -> tuple[dict[str, Any], ...]:
        """The views given now plus every slot this run has already probed."""
        merged = self.distinct_views(*views, *self._view_index.values())
        for view in merged:
            self._view_index.setdefault(view["slot"], view)
        return merged

    def verification_pairs(self, views: tuple[dict[str, Any], ...]) -> list[tuple[dict[str, Any], str]]:
        """The (view, role) pairs the loaded upstream file names."""
        serving_ports, admin_port = self.loaded_route_ports()
        pairs: list[tuple[dict[str, Any], str]] = []
        for view in self.known_views(views):
            for role in SERVING_ROLES:
                if view["ports"][role] in serving_ports:
                    pairs.append((view, role))
            if admin_port is not None and view["ports"]["background"] == admin_port:
                pairs.append((view, "background"))
        return pairs

    def verify_loaded_route(self, views: tuple[dict[str, Any], ...] | list[dict[str, Any]], *,
                            note: str = "", deadline: float | None = None) -> Verification:
        """Probe exactly the roles the loaded file names, after the write.

        This is the only source of route state: it records the verification and
        keeps it as the run's last one."""
        views = self.distinct_views(*views)
        if self.ops.dry_run:
            empty = RouteSnapshot(self.ops.monotonic(), {}, (), (), PROBE_BUDGET_SECONDS)
            return Verification(empty.taken_at, now(), empty)
        snapshot = self.snapshot_now(self.verification_pairs(views), deadline=deadline,
                                     note=note or "verification", kind="written_role_exited")
        verification = Verification(snapshot.taken_at, now(), snapshot)
        self._verification = verification
        self.record_verification(verification, note=note or "verification", views=views)
        return verification

    def publish_serving(self, serving: dict[str, Any], *, deadline: float | None = None,
                        note: str = "", views: tuple[dict[str, Any], ...] = ()) -> RouteOutcome:
        """Write only the serving upstream block.

        The standby is probed first and the primary last: the primary is the
        role the file is worthless without, so it is the freshest probe at the
        write. The standby only decides whether it is written. The admin block
        is not touched here - it has its own write and its own probe."""
        serving_slot = serving["slot"]
        related = self.distinct_views(serving, *views)
        if self.ops.dry_run:
            text = render_upstreams(serving["ports"]["serving"], serving["ports"]["standby"],
                                    serving["ports"]["background"])
            self.ops.write_file_atomic(self.cfg.upstream_file, text)
            self.ops.run([self.cfg.nginx_bin, "-t"], mutate=True, check=False)
            self.ops.run([self.cfg.nginx_bin, "-s", "reload"], mutate=True)
            synthetic = RoleProbe(serving_slot, "serving", serving["containers"]["serving"],
                                  serving["ports"]["serving"], True, "dry-run", "not probed in dry-run", now())
            return RouteOutcome(True, text, serving_slot, None, None, "dry-run", {"serving": synthetic}, [])
        existing = self.current_upstreams()
        first_write = existing is None
        pairs = ([(serving, "background")] if first_write else []) + [(serving, "standby"), (serving, "serving")]
        snapshot = self.snapshot_now(pairs, deadline=deadline, note=note or "route write")
        primary = snapshot.probe(serving_slot, "serving")
        backup = snapshot.probe(serving_slot, "standby")
        if primary is None or not primary.usable:
            self.settle_loaded_route(related, note=f"{note or 'route write'}: refused write",
                                     deadline=deadline)
            why = primary.evidence() if primary else (
                f"the probe budget ran out before slot {serving_slot} serving could be checked")
            raise RouteRefused(f"not writing the service route: {why}; the loaded route is unchanged")
        # The standby only decides whether it is written, so a standby whose own
        # probe went stale is left out instead of refusing the write.
        backup_age = snapshot.age_of(self.ops, serving_slot, "standby")
        backup_stale = backup_age is not None and backup_age > SNAPSHOT_TTL_SECONDS
        backup_port = (backup.port if backup is not None and backup.usable and not backup_stale
                       else None)
        if backup is None:
            self.log(f"   slot {serving_slot} standby was not checked within the probe budget; "
                     "it stays out of the route")
        elif not backup.usable:
            self.log(f"   slot {serving_slot} standby stays out of the route: {backup.evidence()}")
        ports = [primary.port] + ([backup_port] if backup_port is not None else [])
        if first_write:
            admin_probe = snapshot.probe(serving_slot, "background")
            if admin_probe is None or not admin_probe.usable:
                self.settle_loaded_route(related, note=f"{note or 'route write'}: refused write",
                                         deadline=deadline)
                why = admin_probe.evidence() if admin_probe else (
                    "the probe budget ran out before it was checked")
                raise RouteRefused(
                    f"not writing the upstream file (it does not exist yet): slot {serving_slot} "
                    f"background {why}; the loaded route is unchanged")

            # Creating the file also names the admin upstream: that is a move
            # and needs the probe that authorized it, like any other.
            self._admin_move = {"at": now(), "note": f"{note or 'route write'}: first file",
                                "slot": serving_slot, "port": admin_probe.port,
                                "probe": admin_probe.record()}

            def build(_current: str) -> str:
                return render_upstreams(primary.port, backup_port, admin_probe.port)
        else:
            def build(current: str) -> str:
                return replace_serving(current, ports)
        text = build(existing or "")
        written = text != existing
        if written:
            verification = self.write_route(snapshot, build, related,
                                            note=note or "route write", deadline=deadline,
                                            required={(serving_slot, "serving")})
            backup_note = "" if backup_port is not None else "; no backup"
            self.log(f"   route written: serving slot {serving_slot} port {primary.port}{backup_note} "
                     f"({primary.evidence()}); admin block unchanged")
        else:
            self.log(f"   service route already points at slot {serving_slot} port {primary.port}; no reload")
            verification = self.settle_loaded_route(related, note=f"{note or 'route write'}: after writing",
                                                    deadline=deadline)
        return RouteOutcome(written, text, serving_slot, None, None, "",
                            {"serving": primary, "standby": backup}, verification.findings)

    def publish_admin_route(self, view: dict[str, Any], *, deadline: float | None = None,
                            note: str = "", views: tuple[dict[str, Any], ...] = ()) -> RoleProbe:
        """Move only the admin upstream, leaving the serving block alone.

        One probe decides the move; the move is then verified against the file
        that Nginx was told to load. A background that stopped answering is
        reported as such, with the slot a person must restore."""
        related = self.distinct_views(view, *views)
        if self.ops.dry_run:
            return RoleProbe(view["slot"], "background", view["containers"]["background"],
                             view["ports"]["background"], True, "dry-run", "not probed in dry-run", now())
        text = self.current_upstreams()
        if text is None:
            raise RouteRefused("the loaded upstream file is missing; there is no admin upstream to move")
        snapshot = self.snapshot_now([(view, "background")], deadline=deadline, note=note or "admin route")
        probe = snapshot.probe(view["slot"], "background")
        if probe is None or not probe.usable:
            self.settle_loaded_route(related, note=f"{note or 'admin route'}: refused write",
                                     deadline=deadline)
            why = probe.evidence() if probe else (
                "the probe budget ran out before it could be checked")
            raise RouteRefused(f"not writing the admin route: {why}")
        if loaded_upstream_ports(text)[1] is None:
            self.settle_loaded_route(related, note=f"{note or 'admin route'}: refused write",
                                     deadline=deadline)
            raise RouteRefused(
                f"the loaded upstream file names no admin upstream to move to slot {view['slot']} "
                f"port {probe.port}; {probe.evidence()}")
        if loaded_upstream_ports(text)[1] == probe.port:
            self.log(f"   admin route already points at slot {view['slot']} port {probe.port}; no reload")
            verification = self.settle_loaded_route(related, note=f"{note or 'admin route'}: after writing",
                                                    deadline=deadline)
        else:
            self._admin_move = {"at": now(), "note": note or "admin route", "slot": view["slot"],
                                "port": probe.port, "probe": probe.record()}
            self.log(f"   admin route written: slot {view['slot']} port {probe.port} ({probe.evidence()})")
            verification = self.write_route(snapshot, lambda current: replace_admin_port(current, probe.port),
                                            related, note=note or "admin route", deadline=deadline)
        loaded_admin = self.loaded_route_ports()[1]
        owner = next((p for p in verification.snapshot.probes.values()
                      if loaded_admin is not None and p.port == loaded_admin), None)
        if owner is None or not owner.usable:
            probes: dict[str, RoleProbe] = {}
            for other in related:
                snap = self.snapshot_now([(other, "background")], deadline=deadline,
                                         note=f"{note or 'admin route'}: admin owner")
                other_probe = snap.probe(other["slot"], "background")
                if other_probe is not None:
                    probes[other["slot"]] = other_probe
            budget_missed = len(probes) < len(related)
            self._record_admin_none(
                probes, loaded_admin, view["slot"], note or "admin route",
                owner.evidence() if owner else "no background answers the loaded admin port",
                source="unverified (probe budget)" if budget_missed else "no background answers")
            if budget_missed:
                raise AdminRouteLost(
                    f"admin was written to slot {view['slot']} port {probe.port}, but the probe budget ran "
                    "out before the loaded admin port could be checked; nothing further was written")
            raise AdminRouteLost(
                f"admin was written to slot {view['slot']} port {probe.port}, but no background answers "
                f"admin now ({owner.evidence() if owner else 'none answers'}); the loaded pointer is "
                "unchanged and no further reload was made")
        if owner.slot != view["slot"]:
            raise AdminRouteLost(
                f"admin was written to slot {view['slot']} port {probe.port}, but that background stopped "
                f"answering; admin now points at slot {owner.slot} port {owner.port}, which answers "
                f"({owner.detail}); no further reload was made beyond that correction")
        return owner

    def write_route(self, snapshot: RouteSnapshot, build: Callable[[str], str],
                    views: tuple[dict[str, Any], ...] | list[dict[str, Any]],
                    *, note: str, deadline: float | None = None,
                    required: set[tuple[str, str]] | None = None) -> Verification:
        """One write, then one verification of what the file now names."""
        self.install_from(snapshot, build, note=note, required=required)
        return self.settle_loaded_route(views, note=note, deadline=deadline)

    def settle_loaded_route(self, views: tuple[dict[str, Any], ...] | list[dict[str, Any]],
                            *, note: str = "", deadline: float | None = None) -> Verification:
        """Verify the loaded route and correct it at most once.

        A role the file names and which no longer answers is removed from the
        file once, from a fresh snapshot, and the result is verified again. A
        route that is still wrong after that correction is reported, never
        rewritten again."""
        if self.ops.dry_run:
            return self.verify_loaded_route(views, note=note, deadline=deadline)
        views = self.distinct_views(*views)
        verification = self.verify_loaded_route(views, note=note or "verification", deadline=deadline)
        if verification.clean:
            return verification
        if not self.correct_loaded_route(views, verification.findings, note=note or "verification",
                                         deadline=deadline):
            return verification
        return self.verify_loaded_route(views, note=f"{note or 'verification'}: after the correction",
                                        deadline=deadline)

    def correct_loaded_route(self, views: tuple[dict[str, Any], ...] | list[dict[str, Any]],
                             findings: list[dict[str, Any]], *, note: str,
                             deadline: float | None = None) -> bool:
        """Rewrite the loaded route once without the roles that just failed.

        The rewrite is justified by a snapshot of the roles the corrected file
        will name, so it passes the same TTL gate as any other write. Returns
        whether the file was rewritten."""
        text = self.loaded_text()
        if not text:
            return False
        serving_ports, admin_port = loaded_upstream_ports(text)
        dead = {i["port"] for i in findings}
        surviving = [p for p in serving_ports if p not in dead]
        if not surviving:
            # An upstream with no server is rejected by `nginx -t`; there is
            # nothing to write. The finding is what the caller reports.
            return False
        views = self.known_views(views)
        keep_admin = admin_port is not None and admin_port not in dead
        pairs = [(view, role) for view in views for role in SERVING_ROLES
                 if view["ports"][role] in surviving]
        if keep_admin:
            pairs += [(view, "background") for view in views if view["ports"]["background"] == admin_port]
        else:
            pairs += [(view, "background") for view in views if view["ports"]["background"] != admin_port]
        snapshot = self.snapshot_now(pairs, deadline=deadline, note=f"{note}: correction")
        reachable = {probe.port for probe in snapshot.probes.values() if probe.usable}
        surviving = [p for p in surviving if p in reachable]
        if not surviving:
            return False
        # Whatever the earlier verification said, the admin this write is about
        # to name must come out of *this* snapshot: a role the correction has
        # just found stopped cannot be carried into the file because it was
        # fine a round ago. "Not checked" is not usable either.
        admin_probe = next((probe for probe in snapshot.probes.values() if probe.port == admin_port), None)
        if keep_admin and admin_probe is not None and admin_probe.usable:
            new_admin = admin_port
        else:
            new_admin = next((probe.port for probe in snapshot.probes.values()
                              if probe.role == "background" and probe.usable), None)
        if new_admin is None:
            return False

        def build(current: str) -> str:
            return replace_admin_port(replace_serving(current, surviving), new_admin)

        if build(text) == text:
            return False
        if new_admin != admin_port:
            # A4: this correction moved the admin, so the evidence recorded for
            # the move must be this snapshot's probe - not whatever the last
            # admin move left behind.
            moved = next(probe for probe in snapshot.probes.values() if probe.port == new_admin)
            self._admin_move = {"at": now(), "note": f"{note}: correction", "slot": moved.slot,
                                "port": new_admin, "probe": moved.record()}
        self.install_from(snapshot, build, note=f"{note}: correction")
        dropped = sorted(set(serving_ports) - set(surviving))
        self.log(f"   route corrected ({note}): removed serving ports {dropped or 'none'}, "
                 f"admin port {admin_port} -> {new_admin}")
        return True

    def pending_route_issues(self) -> list[dict[str, Any]]:
        """What still has to keep in_progress.

        A role a write found unusable before writing it is a candidate that
        was left out (a standby) or a reason the write was refused; only a
        finding about a role the loaded route names, or a role that was
        written and has since dropped out, is unfinished recovery. What an
        earlier run left in the state file counts while the loaded route still
        names that port."""
        serving_ports, admin_port = self.loaded_route_ports()

        def in_loaded_route(issue: dict[str, Any]) -> bool:
            if issue["role"] in SERVING_ROLES:
                return issue["port"] in serving_ports
            return issue["port"] == admin_port

        pending = {key: issue for key, issue in self._findings.items()
                   if issue["kind"] == "written_role_exited" or in_loaded_route(issue)}
        for issue in self.unresolved_route_issues():
            pending.setdefault(issue_key(issue), issue)
        return list(pending.values())

    def unresolved_route_issues(self) -> list[dict[str, Any]]:
        """Findings that still matter: a recorded role failure whose port the
        loaded route still names. A role that was retired out of the route is
        no longer unresolved."""
        progress = load_state(self.cfg).get("in_progress") or {}
        recorded = progress.get("route_issues") or []
        if not recorded:
            return []
        serving_ports, admin_port = self.loaded_route_ports()
        return [i for i in recorded
                if (i["role"] in SERVING_ROLES and i["port"] in serving_ports)
                or (i["role"] == "background" and i["port"] == admin_port)]

    def switch_to(self, view: dict[str, Any], *, admin: dict[str, Any] | None = None,
                  admin_required: bool = True, note: str = "") -> RouteOutcome:
        """Point the serving block at view and, unless the caller keeps admin
        where it is, move the admin block to the background of `admin` (view
        itself by default). Each block is one write with its own probes."""
        outcome = self.publish_serving(view, note=note)
        if not admin_required:
            return outcome
        admin_view = view if admin is None else admin
        owner = self.publish_admin_route(admin_view, note=f"{note} admin" if note else "admin route",
                                         views=self.distinct_views(view))
        outcome.admin_slot, outcome.admin_port = owner.slot, owner.port
        outcome.admin_source = "verified"
        return outcome

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
        pending = self.result_line(self.one_off(target.image_id or target.image, "background",
                                                ["-list-pending-migrations"]), PENDING_PREFIX)
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

    def background_answering(self, view: dict[str, Any], *, allow_draining: bool = False) -> bool:
        """Whether the slot's background owns the lock and answers admin requests.
        A read-only owner that is still draining can answer reads until it exits."""
        runtime = self.owner_runtime(view) or {}
        responding = runtime.get("ready") or (allow_draining and runtime.get("draining")
                                              and runtime.get("admin_during_drain") == "read-only")
        return bool(runtime.get("role") == "background" and runtime.get("owns_background") and responding
                    and self.health(view["ports"]["background"]))

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
                         deadline: float | None = None, check_health: bool = False) -> None:
        name = view["containers"]["background"]
        if self._rollback_starts is not None:
            self._rollback_starts.add(name)
        # Once ownership is vacant, Ctrl-C must not stop the Docker clients
        # that put an owner back in service. They inherit the ignored signal.
        with finish_handoff_cleanup(ignore=True):
            self.ops.run(["docker", "update", f"--restart={restart}", name], mutate=True,
                         timeout=self.recovery_timeout(deadline, 600))
            self.ops.run(["docker", "start", name], mutate=True, timeout=self.recovery_timeout(deadline, 600))
            if not self.ops.dry_run:
                self.wait_ready(view["ports"]["background"], self.token_for(view, "background"),
                                "background", self.cfg.background_ready_timeout, container=name,
                                poll=HANDOFF_POLL_SECONDS, deadline=deadline, check_health=check_health)

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
                           back: bool = False, *, check_health: bool = False) -> float | None:
        """Move background ownership from current to target. The old owner gets
        one SIGTERM and finishes in-flight work; the kernel lock passes when it
        exits. If the new owner does not become ready, the old one restarts.
        An old owner that reports admin_during_drain keeps answering admin
        reads until it closes its listener, just before it exits. back: the
        handoff returns ownership to target after traffic already moved there.
        check_health also requires /health before the new owner counts as ready.
        Return the active recovery deadline, if any, for subsequent cleanup."""
        old = current["containers"]["background"]
        new = target.containers["background"]
        before = self.owner_runtime(current)  # read-only, also in a dry run
        old_info, old_removed = self.inspect_or_missing(old)
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
                self.start_background(view, deadline=recovery.work_until if recovery else None,
                                      check_health=check_health)
            except (DeployError, OSError, subprocess.SubprocessError) as err:
                if recovery is not None:
                    raise FallbackFailed(f"could not make {new} ready during background recovery: {err}") from err
                self.log(f"   new background failed: {err}. Restoring {old} once {new} exits.")
                new_info, new_removed = self.inspect_or_missing(new)
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
                raise self.fail_back_to_previous(target, previous, reason)

            if self.ops.monotonic() >= deadline:
                return
            self.ops.sleep(self.cfg.guard_interval)

    def refused_service_route(self, target: Target, previous: dict[str, Any], reason: str,
                              err: BaseException) -> FallbackFailed:
        """Report a rollback that could not write serving, including whether
        any background still answers admin. No route is written here."""
        views = {"previous": previous, "candidate": target.view()}
        probes = {side: self.probe_role(view, "background") for side, view in views.items()}
        owner = next((side for side, probe in probes.items() if probe.usable), None)
        loaded_admin = self.loaded_route_ports()[1]
        pointed = self.admin_port_slot(loaded_admin, tuple(views.values()))
        if owner is None:
            summary = "; ".join(probe.evidence() for probe in probes.values())
            self._record_admin_none(probes, loaded_admin, previous["slot"], reason, err)
            return FallbackFailed(
                f"guard failed ({reason}); the rollback service route was not written ({err}). "
                f"No background answers admin now ({summary}); the admin entry is unavailable and needs a "
                f"person to restore one background (slot {previous['slot']} for the rollback) and verify "
                f"live routing. Traffic stays on slot {target.slot} until slot {previous['slot']} serves")
        where = f"admin still points at slot {pointed}" if pointed else f"admin points at port {loaded_admin}"
        return FallbackFailed(
            f"guard failed ({reason}); the rollback service route was not written ({err}). "
            f"Slot {views[owner]['slot']}'s background answers admin and {where}; verify slot "
            f"{previous['slot']}'s serving containers and restore the service route before clearing "
            "in_progress")

    def fail_back_to_previous(self, target: Target, previous: dict[str, Any], reason: str) -> DeployError:
        """Switch serving back, hand the background over and retire the target.
        Returns the error the caller reports; an unsettled route raises."""
        self.log(f"   guard failed: {reason}; switching traffic back to slot {previous['slot']}")
        self._rollback_starts = set()
        try:
            # Serving goes back first. Admin stays on the background that
            # still answers (the candidate's while it drains) until the
            # previous background is ready to take over. The primary and the
            # standby are probed here, immediately before the write.
            self.switch_to(previous, admin=target.view(), admin_required=False,
                           note=f"guard rollback serving after: {reason}")
        except RouteRefused as err:
            # Nothing was written, so admin still points where it did. Say who,
            # if anyone, answers admin: a refused serving write must not hide
            # that both backgrounds are gone.
            failure = self.refused_service_route(target, previous, reason, err)
            self.record_guard_failure(target, failure)
            raise failure from err
        except BaseException as err:
            failure = FallbackFailed(
                f"guard failed ({reason}) and switching serving back failed ({err}); admin still "
                f"points at slot {target.slot}. The admin entry needs a person to check live routing")
            self.record_guard_failure(target, failure)
            raise failure from err
        self.log(f"   handing the background back to the previous release; admin stays on slot "
                 f"{target.slot} until slot {previous['slot']}'s background answers")
        try:
            recovery_deadline = self.hand_back(target, previous, check_health=True)
            # The owner can exit or lose the lock between readiness and the
            # reload. Only a probe taken now may receive admin; the write is
            # followed by another probe, and a failed one stops here.
            self.publish_admin_route(previous, deadline=recovery_deadline,
                                     note=f"guard rollback admin after: {reason}",
                                     views=(target.view(),))
        except AdminRouteLost as err:
            # The admin route was written and the target stopped answering.
            # The route itself was settled before this raised (serving roles
            # included), so all that is left is to say where admin points and
            # what to restore.
            failure = FallbackFailed(
                f"guard failed ({reason}); handing the background back to slot {previous['slot']} did not "
                f"finish ({err}). Restore slot {previous['slot']}'s background (or point admin routing at "
                "an answering one) and verify live routing before clearing in_progress")
            self.record_guard_failure(target, failure)
            raise failure from err
        except BaseException as err:
            # An interrupt belongs here too: whatever ended the step,
            # the failure is recorded and admin keeps an owner.
            raise self.rollback_admin_owner(target, previous, reason, err) from err
        restored = f"guard failed ({reason}); the previous release serves traffic and owns the background again"
        protect = finish_handoff_cleanup(ignore=True) if recovery_deadline is not None else contextlib.nullcontext()
        with protect:
            try:
                self.retire_serving(target.view(), deadline=recovery_deadline, require_known_state=True)
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                # Retirement failing must not skip the check of the roles that
                # were written: the reason to look is exactly that a step
                # failed while the route was live.
                self.settle_loaded_route((previous, target.view()),
                                         note="guard rollback: retirement failed")
                # Name what this run saw while the route was live, including a
                # role the settlement corrected away: the retirement failed, so
                # a person has to look at the route anyway.
                found = self.pending_route_issues()
                detail = f" {describe_route_issues(found)}." if found else ""
                raise ServingCleanupFailed(f"{restored}. Incomplete serving cleanup: {err}.{detail} "
                                           "Check the target slot and the loaded route before clearing "
                                           "in_progress.") from err
        # Retiring the candidate is the last write-shaped step. Probe the route
        # that is loaded now: a role that exited between probes is reported in
        # the same round instead of surfacing at the next release. A finding
        # stays unresolved even if the role came back, because nobody has
        # verified the route since.
        verification = self.settle_loaded_route(
            (previous, target.view()), note="guard rollback: after retiring the candidate serving")
        try:
            self.finish_ok(verification, action="guard rollback")
        except FallbackFailed as failure:
            self.record_guard_failure(target, failure)
            raise failure from None
        return DeployError(f"{restored}; the new serving containers were stopped and kept for inspection")

    def hand_back(self, target: Target, previous: dict[str, Any], *, check_health: bool = False) -> float | None:
        """Return background ownership to the release that still serves traffic."""
        try:
            return self.handoff_background(target.view(), Target.from_view(previous, self.cfg.runtime_token_file),
                                           create=False, back=True, check_health=check_health)
        except FallbackFailed:
            raise
        except DeployError as err:
            raise FallbackFailed(f"handing the background back failed: {err}") from err

    def background_running(self, view: dict[str, Any]) -> bool:
        """Whether the slot's background container exists and is not stopped."""
        info = self.ops.inspect(view["containers"]["background"]) or {}
        state = info.get("State") or {}
        return bool(state.get("Running") or state.get("Restarting"))

    def admin_owner_status(self, view: dict[str, Any]) -> tuple[bool, str]:
        """Probe only: is this background the admin owner right now, and if
        not, why. The answer comes from the runtime lock and its health, never
        from Docker visibility: a container whose state cannot be read may
        still be the one answering admin."""
        name = view["containers"]["background"]
        try:
            runtime = self.owner_runtime(view) or {}
        except Exception as err:  # noqa: BLE001 - a probe failure disqualifies the candidate
            return False, f"{name}: runtime probe failed ({err}); {self.docker_state_hint(view)}"
        if runtime.get("role") != "background" or not runtime.get("owns_background"):
            return False, f"{name}: does not hold the background lock; {self.docker_state_hint(view)}"
        if not (runtime.get("ready") or (runtime.get("draining")
                                         and runtime.get("admin_during_drain") == "read-only")):
            return False, f"{name}: not ready for admin reads"
        try:
            if not self.health(view["ports"]["background"]):
                return False, f"{name}: /health did not return 200"
        except Exception as err:  # noqa: BLE001
            return False, f"{name}: health probe failed ({err})"
        return True, f"{name}: holds the background lock and answers"

    def docker_state_hint(self, view: dict[str, Any]) -> str:
        """Wording for a rejection reason only. Docker's view never decides
        ownership: an unreadable state is reported, not treated as absence."""
        name = view["containers"]["background"]
        try:
            info = self.ops.inspect(name)
        except Exception as err:  # noqa: BLE001
            return f"docker state unknown ({err})"
        if not info:
            return "container is missing"
        if not (info.get("State") or {}).get("Running"):
            return "container is stopped"
        return "container is running"

    def answering_admin_owner(self, views: dict[str, dict[str, Any]]) -> tuple[dict[str, Any] | None, dict[str, str]]:
        """Return the first background the probes confirm as admin owner,
        with why each candidate was rejected."""
        detail: dict[str, str] = {}
        for side, view in views.items():
            owner, why = self.admin_owner_status(view)
            detail[side] = why
            if owner:
                return view, detail
        return None, detail

    def revive_background(self, view: dict[str, Any]) -> bool:
        """Restart a background that merely exited and wait for the lock and
        health, so the admin upstream keeps an owner that answers."""
        name = view["containers"]["background"]
        try:
            if self.background_running(view):
                return False
            if self._rollback_starts is not None and name in self._rollback_starts:
                # One attempt per background per rollback round: a second start
                # of the same container is not a new fact, it is a repeat.
                self.log(f"   {name} was already started once in this rollback; reporting instead of retrying")
                return False
            self.log(f"   {name} is stopped; restarting it so admin keeps an owner")
            self.start_background(view, check_health=True)
            return self.background_answering(view)
        except (Exception, KeyboardInterrupt) as err:  # noqa: BLE001 - the caller probes again
            self.log(f"   {name} did not come back: {err}")
            return False

    def record_guard_failure(self, target: Target, failure: FallbackFailed) -> None:
        """Persist step=guard, the failure and its reason before leaving."""
        try:
            state = load_state(self.cfg)
            progress = state.get("in_progress")
            if not progress:
                progress = state["in_progress"] = {"target": target.view(), "since": now()}
            progress["step"] = "guard"
            self._record_unrestored(state, failure)
        except (Exception, KeyboardInterrupt) as record_err:  # noqa: BLE001 - never mask the failure
            self.log(f"   WARNING: could not record the incomplete recovery: {record_err}")

    def rollback_admin_owner(self, target: Target, previous: dict[str, Any], reason: str,
                             err: BaseException) -> FallbackFailed:
        """Settle the admin entry from probes only, and always record why the
        recovery is incomplete, whatever the cleanup itself raises."""
        try:
            failure = self.settle_admin_owner(target, previous, reason, err)
        except (Exception, KeyboardInterrupt) as cleanup_err:  # noqa: BLE001 - record before leaving
            failure = FallbackFailed(
                f"guard failed ({reason}); handing the background back to slot {previous['slot']} did not finish "
                f"({err}); settling the admin entry failed too ({cleanup_err}). No background is confirmed to "
                f"answer admin; check slot {previous['slot']} and slot {target.slot} and restore one background "
                "that holds the lock and answers /health, then verify live routing before clearing in_progress")
        self.record_guard_failure(target, failure)
        self._guard_settled = True
        return failure

    def current_admin_port(self) -> str | None:
        """Where the admin upstream points right now, read from the rendered
        file rather than assumed from which step ran last."""
        try:
            text = self.ops.read_text(self.cfg.upstream_file)
        except OSError:
            return None
        match = re.search(r"upstream sources_main_admin \{.*?server 127\.0\.0\.1:(\d+)", text, re.S)
        return match.group(1) if match else None

    def settle_admin_owner(self, target: Target, previous: dict[str, Any], reason: str,
                           err: BaseException) -> FallbackFailed:
        """Point admin at an owner probed now, or leave the loaded pointer
        alone when nothing answers and say which background a person must
        restore. Never moves admin to a target that was not just probed."""
        views = {"previous": previous, "candidate": target.view()}
        probes = {side: self.probe_role(view, "background") for side, view in views.items()}
        restarted = False
        owner_side = next((side for side, probe in probes.items() if probe.usable), None)
        if owner_side is None:
            self.log(f"   no background answers admin; trying to restore slot "
                     f"{views['candidate']['slot']}'s background once")
            if self.revive_background(views["candidate"]):
                restarted = True
            # Never assume the restart made it the owner: probe again.
            probes = {side: self.probe_role(view, "background") for side, view in views.items()}
            owner_side = next((side for side, probe in probes.items() if probe.usable), None)
        loaded_admin = self.loaded_route_ports()[1]
        pointed = self.admin_port_slot(loaded_admin, tuple(views.values()))
        if owner_side is None:
            summary = "; ".join(probe.evidence() for probe in probes.values())
            where = (f"admin still points at slot {pointed}" if pointed
                     else f"admin points at port {loaded_admin}" if loaded_admin is not None
                     else "admin points at an unknown port")
            self._record_admin_none(probes, loaded_admin, previous["slot"], reason, err)
            return FallbackFailed(
                f"guard failed ({reason}); handing the background back to slot {previous['slot']} did not finish "
                f"({err}). No background answers admin now ({summary}); {where}, so the admin entry is "
                f"unavailable and needs a person to restore one background (slot {previous['slot']} for the "
                "rollback) and point admin routing at it; verify the route and background ownership "
                "before clearing in_progress")
        owner = views[owner_side]
        admin_port = owner["ports"]["background"]
        if loaded_admin != admin_port:
            try:
                self.publish_admin_route(owner, note=f"guard rollback: settle admin on slot {owner['slot']}",
                                         views=tuple(views.values()))
            except (Exception, KeyboardInterrupt) as route_err:  # noqa: BLE001
                return FallbackFailed(
                    f"guard failed ({reason}); handing the background back to slot {previous['slot']} did not "
                    f"finish ({err}). Slot {owner['slot']}'s background answered when probed, but moving admin "
                    f"routing there did not hold ({route_err}); the admin entry is unavailable and needs a "
                    "person to check where it points and restore one background; verify the route and "
                    "background ownership before clearing in_progress")
            moved = f"admin was moved to slot {owner['slot']}, whose background "
        elif owner_side == "previous":
            moved = f"serving and admin serve from slot {previous['slot']}, whose background "
        else:
            moved = f"serving serves from slot {previous['slot']} but admin still uses slot {owner['slot']}, whose background "
        state = "was restarted and answers" if restarted and owner_side != "previous" else "answers"
        return FallbackFailed(
            f"guard failed ({reason}); handing the background back to slot {previous['slot']} did not finish "
            f"({err}). {moved}{state}. verify the background, its lock and live routing "
            "before clearing in_progress")

    def _record_admin_none(self, probes: dict[str, RoleProbe], loaded_port: int | None,
                           restore_slot: str, reason: str, err: BaseException, *,
                           source: str = "no background answers") -> None:
        """Persist that no background answers (or that the budget did not let
        the tool check) with the slot a person must restore. The backgrounds
        that did not answer are also kept as route findings, so `status` and
        the watch name them like any other role that dropped out."""
        if self.ops.dry_run:
            return
        self._record_route_issues([route_issue("written_role_exited", probe)
                                   for probe in probes.values() if not probe.usable],
                                  f"{reason}: no background answers")
        try:
            progress, state = self._route_progress()
            progress["admin_route"] = {
                "at": now(), "note": f"guard rollback after: {reason}", "slot": None,
                "port": loaded_port, "source": source,
                "probes": {side: probe.record() for side, probe in probes.items()},
                "restore": restore_slot, "failed": str(err),
            }
            save_state(self.cfg, state)
        except Exception as record_err:  # noqa: BLE001 - never mask the failure
            self.log(f"   WARNING: could not record the missing admin owner: {record_err}")

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

    def retire_serving(self, view: dict[str, Any], *, deadline: float | None = None,
                       require_known_state: bool = False) -> None:
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
               allow_running_backup: bool = False, *, hold_before_switch: bool = False) -> None:
        state = load_state(self.cfg)
        self.require_no_hold(state)
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
                if hold_before_switch:
                    target.image_id = self.ops.run(["docker", "image", "inspect", "--format", "{{.Id}}", image],
                                                   mutate=False, timeout=30).stdout.strip()
                    if not target.image_id:
                        raise DeployError("could not resolve the image for a held release")
            run_image = target.image_id or image
            with self.step("preflight"):
                pending = self.preflight(state, target, reuse=False,
                                         allow_public_health_failure=allow_public_health_failure)
                contract = [p for p in pending if "contract" in p]
                if contract and hold_before_switch:
                    raise DeployError("a held release requires additive migrations; contract migrations are not allowed")
                if contract and not allow_contract:
                    raise DeployError(f"contract migrations need --allow-contract: {contract}; nothing changed")
                self.log(f"   pending migrations: {pending or 'none'}")
                if hold_before_switch:
                    if not self.background_answering(current):
                        raise DeployError("cannot prepare a hold without a healthy current background owner")
                    fingerprints = self.hold_fingerprints()
            self._mark(state, target, "migrate")
            if pending:
                with self.step("migrate"):
                    self.one_off(run_image, "background", ["-migrate-only", ",".join(pending)], timeout=900, mutate=True)
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
                        out = self.one_off(run_image, role, ["-check-startup"])
                        self.log(f"   {role}: " + self.result_line(out, CHECK_PREFIX)[:120])
            with self.step("export assets"):
                self.export_assets(run_image)
            self._mark(state, target, "start serving")
            with self.step("start serving"):
                for role in ROLES:
                    self.ops.run(["docker", "rm", "-f", target.containers[role]], mutate=True, check=False)
                for role in SERVING_ROLES:
                    self.create(target, role, include_serving_alias=not hold_before_switch)
                    self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                        self.cfg.ready_timeout,
                                        container=target.containers[role] if hold_before_switch else None,
                                        check_health=hold_before_switch)
            if hold_before_switch:
                self.record_hold(state, target, record, fingerprints, allow_public_health_failure, allow_running_backup)
            else:
                self._finish_switch(state, current, target, record, create_background=True)
            if skip_check:
                self.log("note: this dry run did NOT verify the serving and background environment files "
                         "(startup check skipped because migrations are pending)")
        except DeployError as err:
            record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
            failure: FallbackFailed | None = None
            if not hold_before_switch or state.get("in_progress"):
                failure = self._abort(state, target, err)
            # A failed dry run changed nothing; a record would read like a failed release.
            if not self.ops.dry_run:
                append_history(self.cfg, record)
            if failure is not None:
                raise failure from err
            raise

    @staticmethod
    def require_no_hold(state: dict[str, Any]) -> None:
        phase = (state.get("in_progress") or {}).get("phase")
        if phase == "held":
            raise DeployError("a release is paused. Run 'resume' or 'abort' first")
        if phase == "aborting":
            raise DeployError("paused release cleanup is incomplete. Retry 'abort' first")

    def hold_fingerprints(self) -> dict[str, str]:
        configuration = asdict(self.cfg)
        configuration.pop("hold_warning_seconds")  # This setting only affects status output.
        try:
            files = {path: self.ops.read_text(path) for path in
                     {*self.cfg.env_files.values(), self.cfg.runtime_token_file}}
        except OSError as err:
            raise DeployError("could not read the env files or runtime token file") from err
        payload = json.dumps([configuration, files], sort_keys=True).encode()
        return {"config_fingerprint": hashlib.sha256(payload).hexdigest(),
                "upstream_fingerprint": self.upstream_fingerprint()}

    def upstream_fingerprint(self) -> str:
        try:
            upstream = self.ops.read_text(self.cfg.upstream_file)
        except OSError as err:
            raise DeployError("could not read the upstream file") from err
        return hashlib.sha256(upstream.encode()).hexdigest()

    def check_hold_fingerprints(self, expected: dict[str, Any]) -> None:
        actual = self.hold_fingerprints()
        if actual["config_fingerprint"] != expected["config_fingerprint"]:
            raise DeployError("deployment configuration or its input files changed since preparation began")
        if actual["upstream_fingerprint"] != expected["upstream_fingerprint"]:
            raise DeployError("upstream file changed since preparation began")

    def held_serving_readiness(self, view: dict[str, Any], role: str) -> tuple[int, bool]:
        port = view["ports"][role]
        code, _ = self.ops.http_json(f"http://127.0.0.1:{port}/health")
        if code != 200:
            return code, False
        try:
            runtime = self.runtime(port, self.token_for(view, role)) or {}
        except OSError:
            return code, False
        ready = (runtime.get("role") == "serving" and bool(runtime.get("ready"))
                 and not runtime.get("owns_background") and not runtime.get("draining"))
        return code, ready

    def check_held_serving(self, target: Target, role: str, *, shared: bool = False,
                           expected_id: str | None = None) -> str:
        info = self.ops.inspect(target.containers[role]) or {}
        cid = info.get("Id")
        if not cid or (expected_id is not None and cid != expected_id) or info.get("Image") != target.image_id:
            raise DeployError(f"held {role} is missing or its container identity or image changed")
        state = info.get("State", {})
        if not state.get("Running") or state.get("Restarting") or state.get("OOMKilled"):
            raise DeployError(f"held {role} is not ready (stopped, restarting or OOM killed)")
        networks = info.get("NetworkSettings", {}).get("Networks", {})
        alias = self.cfg.serving_alias
        needs_alias = shared and bool(alias)
        if set(networks) != set(self.cfg.networks) or any(
            bool(alias and alias in (ep.get("Aliases") or [])) != needs_alias
            for ep in networks.values()
        ):
            raise DeployError(f"held {role} has unexpected networks or shared aliases")
        _, ready = self.held_serving_readiness(target.view(), role)
        if not ready:
            raise DeployError(f"held {role} is unhealthy or is not ready as ordinary serving")
        return cid

    def record_hold(self, state: dict[str, Any], target: Target, record: dict[str, Any],
                    fingerprints: dict[str, str], allow_public_health_failure: bool,
                    allow_running_backup: bool) -> None:
        if not self.ops.dry_run:
            containers = {role: self.check_held_serving(target, role) for role in SERVING_ROLES}
            if not self.background_answering(state["current"]):
                raise DeployError("current background stopped answering before the release could pause")
            self.check_hold_fingerprints(fingerprints)
            held_at = now()
            held = {"phase": "held", "held_at": held_at, "target": target.view(),
                    "containers": containers, "image_id": target.image_id, "current": state["current"],
                    "allow_public_health_failure": allow_public_health_failure,
                    "allow_running_backup": allow_running_backup, **fingerprints}
            save_state(self.cfg, {**state, "in_progress": held})
            record.update(result="held", steps=self.timings, finished_at=held_at)
            self.record_hold_history(record)
        prefix = "DRY-RUN would pause" if self.ops.dry_run else "paused"
        self.log(f"{prefix} before background handoff. Use 'resume' or 'abort' after verification.")
        for role in SERVING_ROLES:
            self.log(f"   {role}: http://127.0.0.1:{target.ports[role]}/ (access via your tunnel)")

    def record_hold_history(self, record: dict[str, Any]) -> None:
        # The state file already reflects the result. A history write error must
        # not send a successfully paused or discarded release through cleanup.
        try:
            append_history(self.cfg, record)
        except OSError:
            self.log("   WARNING: could not append release history. See 'status' for the saved state.")

    def held_target(self, state: dict[str, Any], *, aborting: bool = False) -> tuple[dict[str, Any], Target]:
        held = state.get("in_progress") or {}
        phase = held.get("phase")
        if phase == "aborting" and not aborting:
            raise DeployError("paused release cleanup is incomplete. Retry 'abort'. "
                              "If the retry fails, inspect the remaining containers with 'status'")
        if phase not in ("held", "aborting"):
            if held:
                raise DeployError("unfinished release requires manual recovery. Inspect 'status'")
            raise DeployError("no paused release")
        target = Target.from_view(held["target"], self.cfg.runtime_token_file)
        target.image_id = held["image_id"]
        current = state.get("current") or {}
        if (current != held["current"] or target.slot == current.get("slot")
                or set(target.containers.values()) & set(current.get("containers", {}).values())):
            raise DeployError("current release changed during pause. Inspect 'status' before recovery")
        return held, target

    def stop_held_serving(self, name: str, info: dict[str, Any]) -> None:
        cid = info["Id"]
        deadline = self.ops.monotonic() + self.cfg.ready_timeout
        self.ops.run(["docker", "update", "--restart=no", cid], mutate=True,
                     timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
        if info["State"].get("Running"):
            # The existing SIGTERM handler drains serving requests. Never force
            # removal and do not signal a replacement that took the same name.
            self.ops.run(["docker", "kill", "--signal=TERM", cid], mutate=True,
                         timeout=self.recovery_timeout(deadline, DOCKER_STATUS_TIMEOUT_SECONDS))
        if not self.ops.dry_run:
            self.wait_exit(name, deadline, allow_backup_wait=False)

    def activate_held_serving(self, target: Target, held: dict[str, Any]) -> None:
        # Reattach while stopped so no preview request is broken by a live
        # network change. The old release still serves during this preparation.
        for role in SERVING_ROLES:
            name = target.containers[role]
            cid = held["containers"][role]
            info = self.ops.inspect(name) or {}
            if info.get("Id") != cid:
                raise DeployError(f"held {role} was replaced before activation")
            self.stop_held_serving(name, info)
            for network in self.cfg.networks:
                self.ops.run(["docker", "network", "disconnect", network, cid], mutate=True)
                self.ops.run(["docker", "network", "connect", "--alias", name, "--alias", self.cfg.serving_alias,
                              network, cid], mutate=True)
            self.ops.run(["docker", "update", "--restart=unless-stopped", cid], mutate=True)
            self.ops.run(["docker", "start", cid], mutate=True)
        if not self.ops.dry_run:
            for role in SERVING_ROLES:
                self.wait_ready(target.ports[role], self.token_for(target.view(), role), "serving",
                                self.cfg.ready_timeout, container=target.containers[role], check_health=True)
                self.check_held_serving(target, role, shared=True, expected_id=held["containers"][role])
            self.check_hold_fingerprints(held)

    def resume(self) -> None:
        state = load_state(self.cfg)
        held, target = self.held_target(state)
        current = state["current"]
        try:
            self.check_hold_fingerprints(held)
            _, missing = self.inspect_or_missing(target.containers["background"])
            if not missing:
                raise DeployError("could not confirm the target background is absent")
            problems = port_problems(self.ops, [target.ports["background"]])
            if problems:
                raise DeployError("target background port is unavailable: " + ", ".join(problems))
            self.check_running_backup(current, held["allow_running_backup"])
            if not self.background_answering(current):
                raise DeployError("current background is not healthy and holding the lock")
            code, _ = self.ops.http_json(self.cfg.public_health_url, timeout=10)
            if code != 200:
                if not held["allow_public_health_failure"]:
                    raise DeployError(f"public health check returned status {code}")
                self.log(f"   WARNING: public health check returned status {code}. The guard still requires 200.")
            for role in SERVING_ROLES:
                self.check_held_serving(target, role, expected_id=held["containers"][role])
        except (DeployError, OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            raise DeployError(f"resume validation failed. Pause retained. Fix the cause and retry or run 'abort': {err}") from err

        record = {"started_at": now(), "action": "resume", "image": target.image, "slot": target.slot,
                  "held_at": held["held_at"]}
        try:
            if self.cfg.serving_alias:
                self._mark(state, target, "activate held serving")
                with self.step("activate held serving"):
                    try:
                        self.activate_held_serving(target, held)
                    except (DeployError, OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                        raise FallbackFailed(f"held serving activation incomplete: {err}. "
                                             "Inspect the target containers and their network aliases") from err
            self._finish_switch(state, current, target, record, create_background=True)
        except DeployError as err:
            self._abort(state, target, err)
            if not self.ops.dry_run:
                record.update(result="failed", error=str(err), steps=self.timings, finished_at=now())
                self.record_hold_history(record)
            raise
        except (OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            raise DeployError(f"resume interrupted ({type(err).__name__}). Inspect 'status' before continuing") from err

    def abort(self) -> None:
        state = load_state(self.cfg)
        held, target = self.held_target(state, aborting=True)
        if self.upstream_fingerprint() != held["upstream_fingerprint"]:
            raise DeployError("upstream file changed during pause. Confirm the current routing before aborting")
        _, missing = self.inspect_or_missing(target.containers["background"])
        if not missing:
            raise DeployError("could not confirm the target background is absent. Inspect 'status' before aborting")
        record = {"started_at": now(), "action": "abort", "image": target.image, "slot": target.slot,
                  "held_at": held["held_at"]}
        progress = {**held, "phase": "aborting"}
        phase = held["phase"]
        try:
            if not self.ops.dry_run:
                save_state(self.cfg, {**state, "in_progress": progress})
                phase = "aborting"
            for role in SERVING_ROLES:
                name = target.containers[role]
                cid = held["containers"][role]
                info, missing = self.inspect_or_missing(name)
                if missing:
                    continue
                if not info or info.get("Id") != cid:
                    raise DeployError(f"could not confirm identity of held {role}. Container was left in place")
                self.stop_held_serving(name, info)
                self.ops.run(["docker", "rm", cid], mutate=True, timeout=DOCKER_STATUS_TIMEOUT_SECONDS)
                if not self.ops.dry_run:
                    _, missing = self.inspect_or_missing(name)
                    if not missing:
                        raise DeployError(f"could not confirm removal of held {role}")
            if not self.ops.dry_run:
                save_state(self.cfg, {**state, "in_progress": None})
        except (DeployError, OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt) as err:
            if not self.ops.dry_run and phase == "aborting":
                progress.update(reason=str(err) or "interrupted", failed_at=now())
                try:
                    save_state(self.cfg, {**state, "in_progress": progress})
                except OSError:
                    self.log("   WARNING: could not save the cleanup error. The abort record was retained.")
            remaining = held_containers(self.cfg, self.ops, held, probe_health=False)
            detail = ", ".join(f"{role}={entry['status']}" for role, entry in remaining.items())
            raise DeployError(f"abort incomplete (phase={phase}, {detail}). Inspect 'status' and retry 'abort': {err}") from err
        if not self.ops.dry_run:
            record.update(result="ok", finished_at=now())
            self.record_hold_history(record)
        self.log("DRY-RUN would discard held containers" if self.ops.dry_run else "aborted held release")
        self.log("Applied migrations and exported assets are retained.")

    def rollback(self, allow_running_backup: bool = False) -> None:
        state = load_state(self.cfg)
        self.require_no_hold(state)
        current, previous = state.get("current"), state.get("previous")
        if not current or not previous:
            raise DeployError("no previous release recorded; nothing to roll back to")
        self.check_running_backup(current, allow_running_backup)
        for role in ("serving", "background"):
            if not self.ops.inspect(previous["containers"][role]):
                raise DeployError(f"previous container {previous['containers'][role]} no longer exists")
        # The previous standby only decides whether it is written as a backup.
        # A container that is missing, will not start or never becomes ready
        # must not keep a healthy previous primary from serving; the route
        # write probes it again and leaves it out while it does not answer.
        # "Cannot read its state" is treated like "not there": an unreadable
        # standby must not stop a healthy previous primary from serving. The
        # route write probes it again and leaves it out while it does not answer.
        try:
            standby_skipped = not self.ops.inspect(previous["containers"]["standby"])
        except (DeployError, OSError, subprocess.SubprocessError) as err:
            standby_skipped = True
            self.log(f"   previous standby {previous['containers']['standby']} could not be inspected "
                     f"({err}); the rollback will serve from the primary only")
        target = Target.from_view(previous, self.cfg.runtime_token_file)
        record: dict[str, Any] = {"started_at": now(), "action": "rollback", "image": target.image, "slot": target.slot}
        try:
            with self.step("start previous serving"):
                for role in SERVING_ROLES:
                    if role == "standby" and standby_skipped:
                        self.log(f"   previous standby {target.containers[role]} is missing; "
                                 "the rollback will serve from the primary only")
                        continue
                    # Both calls belong to "start this role". A standby whose
                    # restart policy cannot be set is as absent as one that
                    # will not start: it must not keep a healthy previous
                    # primary from taking traffic.
                    try:
                        self.ops.run(["docker", "update", "--restart=unless-stopped",
                                      target.containers[role]], mutate=True)
                        self.ops.run(["docker", "start", target.containers[role]], mutate=True)
                    except (DeployError, OSError, subprocess.SubprocessError) as err:
                        if role != "standby":
                            raise
                        standby_skipped = True
                        self.log(f"   previous standby {target.containers[role]} did not start ({err}); "
                                 "the rollback will serve from the primary only")
                if not self.ops.dry_run:
                    for role in SERVING_ROLES:
                        if role == "standby" and standby_skipped:
                            continue
                        try:
                            self.wait_ready(target.ports[role], self.token_for(previous, role), "serving",
                                            self.cfg.ready_timeout)
                        except (DeployError, OSError) as err:
                            if role != "standby":
                                raise
                            standby_skipped = True
                            self.log(f"   previous standby {target.containers[role]} did not become ready "
                                     f"({err}); the rollback serves from the primary only")
            self._finish_switch(state, current, target, record, create_background=False)
        except DeployError as err:
            failure: DeployError = err
            if not self.ops.dry_run:
                if isinstance(err, FallbackFailed):
                    self._record_unrestored(state, err)
                elif isinstance(err, BackgroundRestored) and self.pending_route_issues():
                    failure = FallbackFailed(str(err))
                    self._record_unrestored(state, failure)
                elif isinstance(err, BackgroundRestored):
                    # The handoff failed and the previous background runs
                    # again; nothing was written in this run, so nothing needs
                    # verifying, but only the gate may clear in_progress.
                    try:
                        self.finish_ok(self._verification, action="rollback")
                    except FallbackFailed as gate:
                        failure = gate
                        self._record_unrestored(state, gate)
                record.update(result="failed", error=str(failure), steps=self.timings, finished_at=now())
                append_history(self.cfg, record)
            raise failure from err

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
            except RouteRefused as err:
                # Serving and admin are separate writes now, so a refusal can
                # arrive with the service block already on the new slot. Hand
                # the background back, put the route back on the previous
                # slot, and only then report. The candidate containers are
                # kept by `_abort`: one of them may be serving.
                self.log("   switch refused; handing the background back and restoring the previous route")
                self.hand_back(target, current)
                try:
                    self.switch_to(current, note=f"restoring slot {current['slot']} after: {err}")
                except DeployError as restore_err:
                    raise FallbackFailed(
                        f"{err}; the previous release owns the background again, but returning the route "
                        f"to slot {current['slot']} failed ({restore_err}); traffic may still use slot "
                        f"{target.slot}. Check the loaded route before clearing in_progress") from err
                raise type(err)(f"{err}; the previous release serves traffic and owns the background "
                                "again") from err
            except FallbackFailed:
                raise
            except (DeployError, OSError, subprocess.SubprocessError) as err:
                # The serving block may already have been written and reloaded
                # when the admin write failed. Only then is there a route to put
                # back: when the first write itself failed, the file was restored
                # and nothing of the candidate is loaded.
                already_loaded = bool(set(self.loaded_route_ports()[0])
                                      & {target.ports[role] for role in SERVING_ROLES})
                self.log("   switch failed; handing the background back")
                recovery_deadline = self.hand_back(target, current)
                if already_loaded:
                    self.log("   the candidate is still serving; restoring the previous route")
                    try:
                        self.switch_to(current, note=f"restoring slot {current['slot']} after: {err}")
                    except DeployError as restore_err:
                        raise FallbackFailed(
                            f"{err}; the previous release owns the background again, but returning the route "
                            f"to slot {current['slot']} failed ({restore_err}); traffic may still use slot "
                            f"{target.slot}. Check the loaded route before clearing in_progress",
                            cleanup_deadline=recovery_deadline) from err
                raise DeployError(f"{err}; the previous release owns the background again",
                                  cleanup_deadline=recovery_deadline) from err
        if not self.ops.dry_run:
            self._mark(state, target, "guard")
            with self.step("guard"):
                self.guard(target, current)
        retire_error: BaseException | None = None
        with self.step("retire previous serving"):
            try:
                self.retire_serving(current)
            except (DeployError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as err:
                # Retirement failing must not skip the check of the roles that
                # were written while the route was live.
                retire_error = err
        if not self.ops.dry_run:
            # The last step that can invalidate the loaded route is retirement.
            # Verify it before promising the release: this verification, with
            # nothing after it, is the only thing that may clear in_progress.
            verification = self.settle_loaded_route(
                (view, current), note="release: after retiring the previous serving")
            if retire_error is not None:
                detail = f" {describe_route_issues(verification.findings)}." if verification.findings else ""
                failure = FallbackFailed(
                    f"release finished but the previous serving was not fully retired: {retire_error}."
                    f"{detail} Restore or remove the affected role and verify live routing before "
                    "clearing in_progress")
                self._record_unrestored(state, failure)
                raise failure
            self.finish_ok(verification, action="release")
        view["deployed_at"] = now()
        # Re-read so the durable route evidence written during the switch is
        # kept; saving the in-memory copy would drop it.
        fresh = load_state(self.cfg) if not self.ops.dry_run else state
        slots = fresh.get("slots") or {}
        slots[target.slot] = view
        if current:
            slots[current["slot"]] = current
        fresh.update(previous=current, current=view, in_progress=None, slots=slots)
        if not self.ops.dry_run:
            save_state(self.cfg, fresh)
        record.update(result="ok", steps=self.timings, finished_at=now())
        if not self.ops.dry_run:
            append_history(self.cfg, record)
        self.log(f"done: slot {target.slot} serving {target.image}")

    def _mark(self, state: dict[str, Any], target: Target, step: str) -> None:
        # A step change must not erase route evidence an earlier probe wrote,
        # nor the durable copy that outlives a successful release.
        recorded = state.get("in_progress") or {}
        fresh = state
        if not self.ops.dry_run:
            fresh = load_state(self.cfg)
            recorded = fresh.get("in_progress") or recorded
            # The file is the source of truth for what earlier probes wrote:
            # this run's in-memory copy predates them.
            for key in ("last_route", "route_findings"):
                if key in fresh:
                    state[key] = fresh[key]
        progress = {"target": target.view(), "step": step, "since": now(), "phase": "deploying"}
        for key in ("serving_route", "admin_route", "admin_move", "route_issues"):
            if key in recorded:
                progress[key] = recorded[key]
        state["in_progress"] = progress
        if not self.ops.dry_run:
            fresh["in_progress"] = progress
            save_state(self.cfg, fresh)

    def _record_unrestored(self, state: dict[str, Any], err: FallbackFailed) -> None:
        """Keep in_progress, with why, so the next run refuses and 'status'
        shows what a person has to do. Route evidence recorded by an earlier
        probe is merged in, never overwritten by a later step."""
        if self.ops.dry_run:
            return
        fresh = load_state(self.cfg)
        recorded = fresh.get("in_progress") or {}
        progress = fresh.get("in_progress") or state.get("in_progress")
        if not progress:
            return
        # The file can already have been cleared by a verification that held;
        # its durable last_route still carries the newest route evidence, and
        # that is newer than this run's in-memory copy.
        last = fresh.get("last_route") or {}
        for key, durable in (("serving_route", "serving"), ("admin_route", "admin"),
                             ("admin_move", "admin_move")):
            value = recorded.get(key) or last.get(durable)
            if value is not None:
                progress[key] = value
        # One entry per failing role, the most recent observation winning: a
        # person reads this list, not a history of every probe. This run's
        # findings are merged in too: they may have been cleared from the file
        # by a verification that came after them.
        merged = {issue_key(i): i for i in progress.get("route_issues") or []}
        for issue in self.pending_route_issues() + list(recorded.get("route_issues") or []):
            merged[issue_key(issue)] = issue
        if merged:
            progress["route_issues"] = list(merged.values())
        progress["phase"] = "failed"
        progress["failed"] = str(err)
        progress["failed_at"] = now()
        fresh["in_progress"] = progress
        save_state(self.cfg, fresh)

    def _abort(self, state: dict[str, Any], target: Target,
               err: DeployError) -> FallbackFailed | None:
        """Record failed recovery or clean up after the previous release is ready.

        Returns the failure the caller should report in place of `err`, when
        the run was not really cleaned up."""
        step = (state.get("in_progress") or {}).get("step")
        if self.ops.dry_run:
            self.log(f"FAILED: {err}")
            return None
        if isinstance(err, FallbackFailed):
            # Keep in_progress so the next run refuses until a person resolves
            # it, and keep why, so 'status' shows what needs doing.
            self._record_unrestored(state, err)
            status = "FAILED" if isinstance(err, ServingCleanupFailed) else "FAILED AND NOT RESTORED"
            self.log(f"{status}: {err}")
            return None
        if isinstance(err, RouteRefused):
            # A refused write is not a broken role: the snapshot was incomplete
            # or past its TTL, or a probe no longer held, but nothing was
            # written. The service block may already point at the candidate
            # (the admin block is a second write), so removing its containers
            # as if the slot were broken could destroy a working release.
            failure = FallbackFailed(f"{err}; the candidate containers in slot {target.slot} were kept")
            self._record_unrestored(state, failure)
            self.log(f"FAILED AND NOT CLEANED UP: {failure}")
            return failure
        if step in (None, "migrate", "start serving", "background handoff", "switch"):
            # A failure after a second write leaves the first one loaded, so the
            # route can already be serving from the candidate. Removing that
            # container would point Nginx at a port nobody answers on: whatever
            # the loaded route names is kept and reported instead.
            serving_now, admin_now = self.loaded_route_ports()
            in_route = set(serving_now) | ({admin_now} if admin_now is not None else set())
            kept: list[str] = []
            deadline = err.cleanup_deadline
            protect = finish_handoff_cleanup(ignore=True) if deadline is not None else contextlib.nullcontext()
            with protect:
                try:
                    for role in SERVING_ROLES:
                        name = target.containers[role]
                        if target.ports[role] in in_route:
                            kept.append(f"{name} (port {target.ports[role]})")
                            continue
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
            if kept:
                failure = FallbackFailed(
                    f"{err}. The loaded route still names {', '.join(kept)}, so those containers were kept; "
                    "check live routing and the background lock before clearing in_progress.")
                self._record_unrestored(state, failure)
                self.log(f"FAILED AND NOT CLEANED UP: {failure}")
                return failure
        issues = self.pending_route_issues()
        if issues:
            # A probe wrote down a role that failed while the route was being
            # written or after it. Clearing now would hide that from the next
            # person and from 'status'; a transient step failure alone would not
            # survive, because the step that recorded it also ended it.
            failure = FallbackFailed(
                f"{err}. An earlier route probe still records an unresolved finding "
                f"({describe_route_issues(issues)}); "
                "verify live routing and the background lock before clearing in_progress")
            self._record_unrestored(state, failure)
            self.log(f"FAILED AND NOT RESTORED: {failure}")
            return failure
        # Clearing in_progress is a success claim about the loaded route, so it
        # goes through the same gate as a successful release.
        try:
            self.finish_ok(self._verification, action=str(err) or "release")
        except FallbackFailed as failure:
            self._record_unrestored(state, failure)
            self.log(f"FAILED AND NOT RESTORED: {failure}")
            return failure
        self.log(f"FAILED: {err}")
        return None

    def finish_ok(self, verification: Verification | None, *, action: str) -> None:
        """The one gate before `in_progress` is cleared after a write.

        It requires that the last thing that happened to the route was a
        verification, that the verification started after the last write, and
        that it found nothing. `self._findings` is deliberately not consulted:
        it records what was seen during the run, it is not proof that the
        loaded route holds now."""
        if verification is None:
            if self._last_write_at is not None:
                raise FallbackFailed(
                    f"{action}: the loaded route was not verified after its last write; "
                    "verify live routing before clearing in_progress")
        else:
            if self._last_write_at is not None and verification.at <= self._last_write_at:
                raise FallbackFailed(
                    f"{action}: the last route verification is older than the last write; "
                    "verify live routing before clearing in_progress")
            if not verification.clean:
                if verification.findings:
                    detail = describe_route_issues(verification.findings)
                else:
                    unchecked = ", ".join(f"slot {slot} {role}"
                                          for slot, role in verification.snapshot.exhausted)
                    detail = f"the probe budget ran out before {unchecked} could be checked"
                raise FallbackFailed(
                    f"{action}: the loaded route does not hold ({detail}); "
                    "verify live routing before clearing in_progress")
        if not self.ops.dry_run:
            fresh = load_state(self.cfg)
            fresh["in_progress"] = None
            save_state(self.cfg, fresh)


# --------------------------------------------------------------------------
# Status, watch and adopt


def observe_release(cfg: Config) -> tuple[dict[str, Any], bool]:
    """Read release state and confirm an unfinished step has no lock owner."""
    state = load_state(cfg)
    progress = state.get("in_progress") or {}
    if not progress or progress.get("phase") == "held" or progress.get("failed") or progress.get("reason"):
        return state, False
    try:
        lock = Path(cfg.lock_file).open("rb")
    except FileNotFoundError:
        return state, True
    except OSError:
        return state, False
    with lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_SH | fcntl.LOCK_NB)
        except OSError:
            return state, False
        # The writer may have completed between the first read and the lock
        # probe. Take a new snapshot while no writer can change the state.
        return load_state(cfg), True


RELEASE_INTERRUPTED = "The release lock is free but a release step remains. Check live routing and containers before recovery."
ABORT_INTERRUPTED = "The release lock is free but paused release cleanup remains. Check the containers and retry 'abort'."


def held_containers(cfg: Config, ops: Ops, held: dict[str, Any], *, probe_health: bool = True) -> dict[str, Any]:
    deployer = Deployer(cfg, ops)
    containers = {}
    view = held["target"]
    for role in SERVING_ROLES:
        name = view["containers"][role]
        info, missing = deployer.inspect_or_missing(name)
        entry: dict[str, Any] = {"name": name, "health": None, "ready": False}
        if info:
            matches = info.get("Id") == held["containers"][role]
            state = info.get("State", {})
            live_status = state.get("Status", "unknown")
            image_matches = info.get("Image") == held["image_id"]
            entry.update(status=live_status if matches else "replaced", identity_matches=matches,
                         image_matches=image_matches)
            if (probe_health and matches and image_matches and state.get("Running")
                    and not state.get("Restarting") and not state.get("OOMKilled")):
                entry["health"], entry["ready"] = deployer.held_serving_readiness(view, role)
        else:
            entry["status"] = "missing" if missing else "unknown"
        containers[role] = entry
    return containers


def status(cfg: Config, ops: Ops) -> dict[str, Any]:
    state, lock_free = observe_release(cfg)
    progress = state.get("in_progress")
    deployer = Deployer(cfg, ops)
    serving_ports, admin_port = deployer.loaded_route_ports()
    report: dict[str, Any] = {
        "in_progress": progress,
        # What Nginx was last told, read from the file rather than assumed.
        "loaded_route": {"serving_ports": serving_ports, "admin_port": admin_port},
        # The probe that justified the last route write; survives a successful
        # release, which clears in_progress.
        "last_route": state.get("last_route"),
        # Findings that outlive a successful release: a role that was written
        # into the route and then found gone stays here until a verification
        # proves it answers again.
        "route_findings": state.get("route_findings") or [],
        "slots": sorted((state.get("slots") or {}).keys()),
    }
    if progress and progress.get("phase") in ("held", "aborting"):
        held_at = dt.datetime.fromisoformat(progress["held_at"])
        age = max(0, int((dt.datetime.now(dt.timezone.utc) - held_at).total_seconds()))
        live = held_containers(cfg, ops, progress)
        warnings = []
        if progress["phase"] == "aborting":
            warnings.append(ABORT_INTERRUPTED if lock_free and not progress.get("reason") else
                            "paused release cleanup is incomplete. Inspect the containers and retry 'abort'")
        else:
            not_ready = [role for role, entry in live.items() if not entry["ready"]]
            if not_ready:
                warnings.append(f"Held candidates are not ready ({', '.join(not_ready)}). "
                                "Investigate or run 'abort' before resuming.")
            if age >= cfg.hold_warning_seconds:
                warnings.append("release has been paused for a long time. Verify it, then resume or abort")
        identities = {role: {"id": cid, "image_id": progress["image_id"]}
                      for role, cid in progress["containers"].items()}
        report["in_progress"] = {**progress, "age_seconds": age, "warning": " ".join(warnings) or None,
                                 "containers": identities, "live_containers": live}
    elif progress and lock_free and not progress.get("failed"):
        report["in_progress"] = {**progress, "phase": "interrupted", "warning": RELEASE_INTERRUPTED}
    for key in ("current", "previous"):
        view = state.get(key)
        if not view:
            report[key] = None
            continue
        entry = {"slot": view["slot"], "image": view.get("image"), "deployed_at": view.get("deployed_at"),
                 "containers": {}}
        for role, name in view["containers"].items():
            info, missing = deployer.inspect_or_missing(name)
            s = (info or {}).get("State", {})
            entry["containers"][role] = {"name": name, "port": view["ports"][role],
                                         "status": s.get("Status", "unknown") if info else
                                                   ("missing" if missing else "unknown"),
                                         "health": (s.get("Health") or {}).get("Status"),
                                         "restarts": (info or {}).get("RestartCount"),
                                         # "no" on a running background: it was told to
                                         # drain and Docker will not bring it back.
                                         "restart_policy": (((info or {}).get("HostConfig") or {})
                                                            .get("RestartPolicy") or {}).get("Name")}
        report[key] = entry
    return report


def watch_once(cfg: Config, ops: Ops, history: dict[str, list[tuple[float, int]]]) -> list[dict[str, Any]]:
    """One resident sample of every slot the state still knows about.

    Coverage does not depend on which slot a release retired: both slots are
    kept in `slots`, and a slot that cannot be inspected is named in an alert
    instead of being skipped."""
    state, lock_free = observe_release(cfg)
    progress = state.get("in_progress") or {}
    alerts: list[dict[str, Any]] = []
    if progress.get("phase") == "aborting":
        reason = progress.get("reason") or (ABORT_INTERRUPTED if lock_free else None)
        if reason:
            alerts.append({"alert": ["abort_incomplete"], "at": now(), "phase": "aborting",
                           "slot": progress["target"]["slot"], "reason": reason})
    elif progress.get("failed"):
        alerts.append({"alert": ["release_incomplete"], "at": now(), "step": progress.get("step"),
                       "reason": progress["failed"]})
    elif lock_free and progress.get("phase") != "held":
        alerts.append({"alert": ["release_incomplete"], "at": now(), "phase": "interrupted",
                       "step": progress.get("step"), "reason": RELEASE_INTERRUPTED})
    views: dict[str, dict[str, Any]] = {}
    for key in ("current", "previous"):
        view = state.get(key)
        if view:
            views[view["slot"]] = view
    for slot, view in (state.get("slots") or {}).items():
        views.setdefault(slot, view)
    target = (state.get("in_progress") or {}).get("target")
    if target:
        # A paused release has no background in the candidate slot by design.
        # Sampling that role would report "unhealthy" for a container that was
        # never supposed to exist, so only the roles the release recorded count.
        recorded = progress.get("containers") or {}
        if progress.get("phase") in ("held", "aborting") and recorded:
            target = {**target, "containers": {role: name for role, name in target["containers"].items()
                                               if role in recorded}}
        views.setdefault(target["slot"], target)
    deployer = Deployer(cfg, ops)
    t = ops.monotonic()
    for slot, view in sorted(views.items()):
        for role, name in view["containers"].items():
            info, missing = deployer.inspect_or_missing(name)
            if info is None and not missing:
                # "Cannot check" and "confirmed absent" are different facts. A
                # retired slot whose containers Docker confirms are gone is
                # skipped below; a slot Docker could not check is named.
                alerts.append({"slot": slot, "role": role, "name": name, "alert": ["docker_inspect_failed"],
                               "detail": f"could not inspect {name}; this slot was not checked",
                               "at": now()})
                continue
            if info is None and missing and view["slot"] == (state.get("current") or {}).get("slot"):
                # Docker confirmed it is gone. In a retired slot that is
                # expected; in the live slot it is the outage itself. Naming it
                # is the only useful sample - there is no container to sample.
                alerts.append({"slot": slot, "role": role, "name": name, "alert": ["missing"],
                               "detail": f"{name} does not exist; slot {slot} cannot serve {role}",
                               "at": now()})
                continue
            if not info and view["slot"] != (state.get("current") or {}).get("slot"):
                # A retired slot's containers are gone on purpose. Reporting them
                # as unhealthy would keep a permanent red dot for something nobody
                # needs to act on; only the live slot's absence is a problem.
                continue
            token = None
            with contextlib.suppress(OSError):
                token = deployer.token_for(view, role)
            s = info.get("State", {})
            port = view["ports"][role]
            try:
                status_code, _ = ops.http_json(f"http://127.0.0.1:{port}/health")
            except Exception:  # noqa: BLE001 - no answer is unhealthy, not a crash
                status_code = 0
            try:
                runtime = deployer.runtime(port, token) if token else None
            except Exception:  # noqa: BLE001
                runtime = None
            memory = deployer.container_memory(info)
            sample = {"slot": slot, "role": role, "name": name, "running": s.get("Running"),
                      "health": status_code, "restarts": info.get("RestartCount"), "oom": s.get("OOMKilled"),
                      "memory_gib": round(memory / GIB, 2) if memory else None,
                      "active_http": (runtime or {}).get("active_http")}
            reasons = []
            if not s.get("Running") or status_code != 200:
                reasons.append("unhealthy")
            if runtime is None and s.get("Running") and status_code == 200:
                # The container answers Docker and /health but the tool cannot
                # read its runtime state (no token file, or no answer). That is
                # "not checked", not "fine", so it is an alert either way.
                reasons.append("runtime_unavailable")
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
    progress = state.get("in_progress") or {}
    if progress.get("route_issues") or progress.get("failed"):
        alerts.append({"alert": ["in_progress"], "step": progress.get("step"),
                       "phase": progress.get("phase"), "failed": progress.get("failed"),
                       "route_issues": progress.get("route_issues"), "since": progress.get("since"),
                       "at": now()})
    if state.get("route_findings"):
        alerts.append({"alert": ["route_findings"], "route_findings": state["route_findings"],
                       "at": now()})
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
        save_state(Config.load(cfg_path), {"current": legacy, "previous": None, "in_progress": None,
                                           "slots": {"legacy": legacy}})
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
    p_main.add_argument("--hold-before-switch", action="store_true",
                        help="pause after the new serving instances are ready")
    p_main.add_argument("--allow-contract", action="store_true", help="allow pending migrations marked 'contract'")
    p_main.add_argument("--allow-public-health-failure", action="store_true",
                        help="warn instead of stopping when the public health check fails before the switch "
                             "(the guard still requires 200 after it)")
    p_main.add_argument("--allow-running-backup", action="store_true",
                        help="start a release while a scheduled backup is running")
    p_rollback = sub.add_parser("rollback", help="switch back to the previous slot")
    p_rollback.add_argument("--allow-running-backup", action="store_true",
                            help="roll back while a scheduled backup is running")
    sub.add_parser("resume", help="verify a paused release and continue switching")
    sub.add_parser("abort", help="stop and remove the paused release's serving instances")
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
                                allow_running_backup=args.allow_running_backup,
                                hold_before_switch=args.hold_before_switch)
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


if __name__ == "__main__":
    sys.exit(main())
