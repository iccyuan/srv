#!/usr/bin/env python3
"""srv - run commands on a remote SSH server with persistent cwd.

Quick start:
  srv init                       configure a profile interactively
  srv config list                show profiles
  srv use <profile>              pin a profile for *this shell* (quick switch)
  srv use --clear                unpin (fall back to default)
  srv cd /opt                    set persistent remote cwd (per session+profile)
  srv pwd                        show current remote cwd
  srv ls -la                     run on remote in current cwd
  srv "ps aux | grep redis"      pipes/redirects: quote at local shell
  srv -t htop                    interactive (TTY) command
  srv -P dev rsync ...           override profile for a single call
  srv check                      probe connectivity; diagnose key/host/port issues

File transfer (uses scp):
  srv push ./local.py            upload to current cwd
  srv push ./dist /opt/app       upload (recursive auto-detected)
  srv pull logs/app.log          download to current dir
  srv pull /etc/hosts ./hosts    explicit local target

Bulk sync of changed files (tar | ssh tar; preserves relative paths):
  srv sync                       in a git repo: modified+staged+untracked
  srv sync --staged              only `git add`-ed files
  srv sync --since 2h            files mtime'd within 2 hours
  srv sync --include "src/**/*.py"   glob mode (repeatable)
  srv sync --files a.py b/c.py   explicit list
  srv sync --dry-run             show what would push, don't transfer
  srv sync /opt/app              override remote root (else cwd or sync_root)

Detached jobs (background on remote, log to ~/.srv-jobs/<id>.log):
  srv -d ./long-build.sh         kick off, return immediately, print job id
  srv jobs                       list local job records
  srv logs <id> [-f]             cat (or tail -f) the remote log
  srv kill <id>                  SIGTERM the remote process and forget it

Sessions (per-shell isolation):
  srv sessions                   list session records
  srv sessions show              show this shell's session record
  srv sessions clear             drop this shell's session record
  srv sessions prune             remove records whose pid is dead

Integrations:
  srv completion <bash|zsh|powershell>   emit shell completion script
  srv mcp                                run as a stdio MCP server

Network resilience (always on; tune via profile keys):
  - ControlMaster connection multiplexing
  - ConnectTimeout=10s, ServerAliveInterval=30s, TCPKeepAlive=yes
  - Compression=yes, retry up to 3x on handshake-only failures
  Profile keys: multiplex, compression, connect_timeout,
  keepalive_interval, keepalive_count, control_persist.

Profile resolution (highest first):
  -P/--profile flag  >  session pin (`srv use`)  >  $SRV_PROFILE  >  default

Session detection:
  Each shell gets its own session id (parent shell's PID, with the .cmd shim
  layer skipped on Windows). Override with $SRV_SESSION=<any string>.

Config: ~/.srv/config.json   Sessions: ~/.srv/sessions.json
Jobs: ~/.srv/jobs.json       Control sockets: ~/.srv/cm/
"""
from __future__ import annotations

import base64
import datetime
import fnmatch
import glob as _glob
import json
import os
import secrets
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

CONFIG_DIR = Path(os.environ.get("SRV_HOME") or (Path.home() / ".srv"))
CONFIG_FILE = CONFIG_DIR / "config.json"
SESSIONS_FILE = CONFIG_DIR / "sessions.json"
JOBS_FILE = CONFIG_DIR / "jobs.json"

RESERVED_SUBCOMMANDS = {
    "init", "config", "use", "cd", "pwd", "status", "check", "run", "exec",
    "push", "pull", "sync", "completion", "mcp", "_profiles",
    "jobs", "logs", "kill", "sessions",
    "help", "--help", "-h", "version", "--version",
}

VERSION = "0.7.2"

# Connect-time failure window (seconds): if ssh exits 255 within this window,
# the failure is presumed to be the handshake (not the user command),
# making a retry safe.
HANDSHAKE_FAILURE_WINDOW_S = 5.0

# Set to True only while the stdio MCP server is running. Used to suppress
# stderr writes (retry messages) that would otherwise leak through the
# MCP transport and confuse some clients.
_IN_MCP_MODE = False

# Process exes that are transparent layers between the user's shell and our
# python: the cmd.exe shim in srv.cmd, and the Windows Store python launcher
# (which itself spawns a real python.exe child). Walking up through these
# yields a stable id across `srv` invocations from the same shell.
_INTERMEDIATE_EXES = {
    "cmd.exe", "python.exe", "py.exe", "pythonw.exe", "python3.exe",
    "python3w.exe",
}


# --------------------------------------------------------------------------- #
# Session detection                                                           #
# --------------------------------------------------------------------------- #

def _walk_windows_processes() -> dict[int, tuple[int, str]]:
    """Return {pid: (parent_pid, exe_name_lower)} via Toolhelp snapshot."""
    import ctypes
    from ctypes import wintypes

    TH32CS_SNAPPROCESS = 0x00000002

    class PROCESSENTRY32W(ctypes.Structure):
        _fields_ = [
            ("dwSize", wintypes.DWORD),
            ("cntUsage", wintypes.DWORD),
            ("th32ProcessID", wintypes.DWORD),
            ("th32DefaultHeapID", ctypes.c_void_p),
            ("th32ModuleID", wintypes.DWORD),
            ("cntThreads", wintypes.DWORD),
            ("th32ParentProcessID", wintypes.DWORD),
            ("pcPriClassBase", wintypes.LONG),
            ("dwFlags", wintypes.DWORD),
            ("szExeFile", wintypes.WCHAR * 260),
        ]

    kernel32 = ctypes.windll.kernel32
    snap = kernel32.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
    INVALID = ctypes.c_void_p(-1).value
    if not snap or snap == INVALID:
        return {}
    try:
        entry = PROCESSENTRY32W()
        entry.dwSize = ctypes.sizeof(PROCESSENTRY32W)
        out: dict[int, tuple[int, str]] = {}
        if kernel32.Process32FirstW(snap, ctypes.byref(entry)):
            while True:
                out[int(entry.th32ProcessID)] = (
                    int(entry.th32ParentProcessID),
                    str(entry.szExeFile).lower(),
                )
                if not kernel32.Process32NextW(snap, ctypes.byref(entry)):
                    break
        return out
    finally:
        kernel32.CloseHandle(snap)


def _session_id() -> str:
    """Stable identifier for the calling shell.

    Override: $SRV_SESSION wins if set. Otherwise:
      - Unix: os.getppid().
      - Windows: walk up the process tree, skipping ancestors whose exe is in
        _INTERMEDIATE_EXES (the cmd.exe shim spawned by PowerShell to run a
        .cmd, and the Windows Store python launcher's intermediate python.exe).
        Return the first non-intermediate ancestor's pid -- typically the user
        shell (powershell.exe, bash.exe, etc.). That pid is stable for the
        lifetime of the shell, so multiple `srv` calls from one terminal share
        a session id.
    """
    explicit = os.environ.get("SRV_SESSION")
    if explicit:
        return explicit.strip()
    if sys.platform == "win32":
        try:
            tree = _walk_windows_processes()
            pid = os.getpid()
            for _ in range(20):  # bounded walk
                entry = tree.get(pid)
                if not entry:
                    return str(os.getppid())
                parent_pid, _ = entry
                if parent_pid <= 0:
                    return str(os.getppid())
                parent_entry = tree.get(parent_pid)
                if not parent_entry:
                    return str(parent_pid)
                _, parent_name = parent_entry
                if parent_name not in _INTERMEDIATE_EXES:
                    return str(parent_pid)
                pid = parent_pid
            return str(os.getppid())
        except Exception:
            return str(os.getppid())
    return str(os.getppid())


def _pid_alive(pid: Any) -> bool:
    try:
        pid = int(pid)
    except (TypeError, ValueError):
        return False
    if pid <= 0:
        return False
    if sys.platform == "win32":
        try:
            import ctypes
            kernel32 = ctypes.windll.kernel32
            PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
            h = kernel32.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, False, pid)
            if h:
                kernel32.CloseHandle(h)
                return True
            return False
        except Exception:
            return False
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False


# --------------------------------------------------------------------------- #
# JSON I/O                                                                    #
# --------------------------------------------------------------------------- #

def _read_json(path: Path, default: Any) -> Any:
    if not path.exists():
        return default
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        sys.exit(f"error: {path} is not valid JSON: {e}")


def _write_json(path: Path, data: Any) -> None:
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")


def load_config() -> dict:
    cfg = _read_json(CONFIG_FILE, {"default_profile": None, "profiles": {}})
    cfg.setdefault("default_profile", None)
    cfg.setdefault("profiles", {})
    return cfg


def save_config(cfg: dict) -> None:
    _write_json(CONFIG_FILE, cfg)


def load_sessions() -> dict:
    s = _read_json(SESSIONS_FILE, {"sessions": {}})
    s.setdefault("sessions", {})
    return s


def save_sessions(s: dict) -> None:
    _write_json(SESSIONS_FILE, s)


def load_jobs() -> dict:
    j = _read_json(JOBS_FILE, {"jobs": []})
    j.setdefault("jobs", [])
    return j


def save_jobs(j: dict) -> None:
    _write_json(JOBS_FILE, j)


# --------------------------------------------------------------------------- #
# Session record helpers                                                      #
# --------------------------------------------------------------------------- #

def _now_iso() -> str:
    return datetime.datetime.now().isoformat(timespec="seconds")


def _touch_session(sid: str | None = None) -> tuple[str, dict, dict]:
    """Get or create the current session record, update last_seen, persist.
    Returns (sid, record, all_sessions). Callers that mutate rec further
    must save_sessions(all_sessions) again."""
    sid = sid or _session_id()
    s = load_sessions()
    rec = s["sessions"].get(sid)
    if rec is None:
        rec = {
            "profile": None,
            "cwds": {},
            "started": _now_iso(),
            "last_seen": _now_iso(),
        }
        s["sessions"][sid] = rec
    else:
        rec.setdefault("cwds", {})
        rec["last_seen"] = _now_iso()
    save_sessions(s)
    return sid, rec, s


def resolve_profile(cfg: dict, name: str | None) -> tuple[str, dict]:
    """Resolution order: -P flag > session pin > $SRV_PROFILE > default."""
    if not name:
        sid, rec, _ = _touch_session()
        name = rec.get("profile")
    if not name:
        name = os.environ.get("SRV_PROFILE")
    if not name:
        name = (load_config().get("default_profile")
                if cfg is None else cfg.get("default_profile"))
    if not name:
        sys.exit("error: no profile selected. Run `srv init`, then "
                 "`srv use <profile>` to pin one for this shell.")
    if name not in cfg["profiles"]:
        sys.exit(f"error: profile {name!r} not found. Run `srv config list`.")
    return name, cfg["profiles"][name]


def get_cwd(profile_name: str, profile: dict) -> str:
    sid, rec, _ = _touch_session()
    cwd = (rec.get("cwds") or {}).get(profile_name)
    if cwd:
        return cwd
    return profile.get("default_cwd") or "~"


def set_cwd(profile_name: str, cwd: str) -> None:
    sid, rec, all_s = _touch_session()
    rec.setdefault("cwds", {})[profile_name] = cwd
    save_sessions(all_s)


def set_session_profile(name: str | None) -> str:
    sid, rec, all_s = _touch_session()
    rec["profile"] = name
    save_sessions(all_s)
    return sid


# --------------------------------------------------------------------------- #
# SSH / scp builders                                                          #
# --------------------------------------------------------------------------- #

def _default_ssh_options(profile: dict) -> list[str]:
    """Resilience defaults applied to every ssh/scp call. User-supplied
    `ssh_options` come AFTER these so they always win."""
    opts: list[str] = []
    if profile.get("multiplex", True):
        cm_dir = CONFIG_DIR / "cm"
        cm_dir.mkdir(parents=True, exist_ok=True)
        control_path = (cm_dir / "%C.sock").as_posix()
        opts += [
            "ControlMaster=auto",
            f"ControlPath={control_path}",
            f"ControlPersist={profile.get('control_persist', '10m')}",
        ]
    opts += [
        f"ConnectTimeout={profile.get('connect_timeout', 10)}",
        f"ServerAliveInterval={profile.get('keepalive_interval', 30)}",
        f"ServerAliveCountMax={profile.get('keepalive_count', 3)}",
        "TCPKeepAlive=yes",
    ]
    if profile.get("compression", True):
        opts.append("Compression=yes")
    return opts


def build_ssh_cmd(profile: dict, remote_command: str, tty: bool = False,
                  capture: bool = False) -> list[str]:
    args = ["ssh"]
    if capture:
        args.append("-q")
    if tty:
        args.append("-tt")
    if profile.get("port") and int(profile["port"]) != 22:
        args += ["-p", str(profile["port"])]
    if profile.get("identity_file"):
        args += ["-i", os.path.expanduser(profile["identity_file"])]
    for opt in _default_ssh_options(profile):
        args += ["-o", opt]
    if capture:
        # Capture mode = non-interactive caller (MCP, internal probes, cd
        # validation, detach spawn). BatchMode prevents ssh from hanging
        # forever on a prompt (passphrase / host-key / fallback password)
        # while reading from a parent stdin that can't satisfy it.
        args += ["-o", "BatchMode=yes"]
    for opt in profile.get("ssh_options") or []:
        args += ["-o", opt]
    user = profile.get("user")
    target = f"{user}@{profile['host']}" if user else profile["host"]
    args.append(target)
    args.append(remote_command)
    return args


def build_scp_cmd(profile: dict, src: str, dst: str,
                  recursive: bool = False, capture: bool = False) -> list[str]:
    args = ["scp"]
    if recursive:
        args.append("-r")
    if profile.get("port") and int(profile["port"]) != 22:
        args += ["-P", str(profile["port"])]
    if profile.get("identity_file"):
        args += ["-i", os.path.expanduser(profile["identity_file"])]
    for opt in _default_ssh_options(profile):
        args += ["-o", opt]
    if capture:
        args += ["-o", "BatchMode=yes"]
    for opt in profile.get("ssh_options") or []:
        args += ["-o", opt]
    args += [src, dst]
    return args


def _ssh_call(cmd: list[str], retry: bool = True, max_attempts: int = 3) -> int:
    """Run ssh-family command, streaming stdio. Retries handshake-only failures."""
    attempts = max_attempts if retry else 1
    rc = 0
    for i in range(attempts):
        t0 = time.monotonic()
        rc = subprocess.call(cmd)
        elapsed = time.monotonic() - t0
        if rc != 255 or elapsed >= HANDSHAKE_FAILURE_WINDOW_S:
            return rc
        if i == attempts - 1:
            return rc
        sleep_s = 1.0 * (2 ** i)
        if not _IN_MCP_MODE:
            sys.stderr.write(
                f"srv: connect failed (exit 255 in {elapsed:.1f}s), "
                f"retrying in {sleep_s:.0f}s...\n"
            )
        time.sleep(sleep_s)
    return rc


def _ssh_run(cmd: list[str], retry: bool = True, max_attempts: int = 3,
             timeout: float = 60.0) -> subprocess.CompletedProcess:
    """Capture stdout/stderr. Closes child stdin (DEVNULL) so the spawned
    ssh can't inherit a parent pipe (e.g., the MCP JSON-RPC stream) and
    block forever on a prompt. Hard-caps wall time at `timeout` seconds;
    on hit, returns a synthetic CompletedProcess with rc=124."""
    attempts = max_attempts if retry else 1
    r = None
    for i in range(attempts):
        t0 = time.monotonic()
        try:
            r = subprocess.run(
                cmd,
                capture_output=True, text=True,
                stdin=subprocess.DEVNULL,
                timeout=timeout,
            )
        except subprocess.TimeoutExpired as e:
            return subprocess.CompletedProcess(
                args=cmd, returncode=124,
                stdout=(e.stdout or ""),
                stderr=(e.stderr or "") + f"\nsrv: timeout after {timeout:.0f}s",
            )
        elapsed = time.monotonic() - t0
        if r.returncode != 255 or elapsed >= HANDSHAKE_FAILURE_WINDOW_S:
            return r
        if i == attempts - 1:
            return r
        sleep_s = 1.0 * (2 ** i)
        if not _IN_MCP_MODE:
            sys.stderr.write(
                f"srv: connect failed (exit 255 in {elapsed:.1f}s), "
                f"retrying in {sleep_s:.0f}s...\n"
            )
        time.sleep(sleep_s)
    return r  # type: ignore[return-value]


# --------------------------------------------------------------------------- #
# Remote operations                                                           #
# --------------------------------------------------------------------------- #

def run_remote(profile_name: str, profile: dict, command_str: str,
               tty: bool = False) -> int:
    cwd = get_cwd(profile_name, profile)
    full = f"cd {shlex.quote(cwd)} && ({command_str})"
    ssh_cmd = build_ssh_cmd(profile, full, tty=tty)
    try:
        return _ssh_call(ssh_cmd, retry=not tty)
    except FileNotFoundError:
        sys.exit("error: `ssh` not found in PATH. Install the OpenSSH client.")


def run_remote_capture(profile_name: str, profile: dict,
                       command_str: str) -> dict:
    cwd = get_cwd(profile_name, profile)
    full = f"cd {shlex.quote(cwd)} && ({command_str})"
    ssh_cmd = build_ssh_cmd(profile, full, capture=True)
    try:
        result = _ssh_run(ssh_cmd)
    except FileNotFoundError:
        return {"stdout": "", "stderr": "ssh not found in PATH",
                "exit_code": 127, "cwd": cwd}
    return {
        "stdout": result.stdout,
        "stderr": result.stderr,
        "exit_code": result.returncode,
        "cwd": cwd,
    }


def change_remote_cwd(profile_name: str, profile: dict,
                      target: str | None) -> tuple[str | None, str]:
    target = target or "~"
    current = get_cwd(profile_name, profile)
    remote_cmd = (
        f"cd {shlex.quote(current)} 2>/dev/null || cd ~; "
        f"cd {shlex.quote(target)} && pwd"
    )
    ssh_cmd = build_ssh_cmd(profile, remote_cmd, capture=True)
    try:
        result = _ssh_run(ssh_cmd)
    except FileNotFoundError:
        return None, "ssh not found in PATH"
    if result.returncode != 0:
        return None, (result.stderr or "").strip() or f"cd failed (exit {result.returncode})"
    lines = (result.stdout or "").strip().splitlines()
    if not lines:
        return None, "remote did not return a path"
    new_cwd = lines[-1].strip()
    set_cwd(profile_name, new_cwd)
    return new_cwd, ""


def remote_target(profile: dict, remote_path: str) -> str:
    user = profile.get("user")
    host = profile["host"]
    quoted = shlex.quote(remote_path)
    return f"{user}@{host}:{quoted}" if user else f"{host}:{quoted}"


def resolve_remote_path(remote: str, cwd: str) -> str:
    if not remote:
        return cwd
    if remote.startswith("/") or remote.startswith("~"):
        return remote
    return f"{cwd.rstrip('/')}/{remote}"


# --------------------------------------------------------------------------- #
# Subcommands                                                                 #
# --------------------------------------------------------------------------- #

def cmd_cd(path: str | None, cfg: dict, profile_override: str | None) -> int:
    name, profile = resolve_profile(cfg, profile_override)
    new_cwd, err = change_remote_cwd(name, profile, path)
    if err:
        sys.stderr.write(err + "\n")
        return 1
    print(new_cwd)
    return 0


def cmd_pwd(cfg: dict, profile_override: str | None) -> int:
    name, profile = resolve_profile(cfg, profile_override)
    print(get_cwd(name, profile))
    return 0


def cmd_status(cfg: dict, profile_override: str | None) -> int:
    name, profile = resolve_profile(cfg, profile_override)
    sid, rec, _ = _touch_session()
    user = profile.get("user", "")
    port = profile.get("port", 22)
    host = profile["host"]
    target = f"{user}@{host}" if user else host
    pinned = rec.get("profile")
    profile_label = f"{name} (pinned)" if pinned == name and not profile_override \
        else (f"{name} (-P override)" if profile_override else f"{name} (default)")
    print(f"profile : {profile_label}")
    print(f"target  : {target}:{port}")
    if profile.get("identity_file"):
        print(f"key     : {profile['identity_file']}")
    print(f"cwd     : {get_cwd(name, profile)}")
    print(f"session : {sid}")
    print(f"defaults: multiplex={profile.get('multiplex', True)}  "
          f"compression={profile.get('compression', True)}  "
          f"connect_timeout={profile.get('connect_timeout', 10)}s")
    return 0


def _ssh_check(profile: dict, timeout: float = 15.0) -> tuple[bool, int, str, str]:
    """Probe SSH connectivity with BatchMode=yes (no password / passphrase /
    host-key prompts to hang on) and a short timeout. Bypasses ControlMaster
    so a stale socket can't fool the diagnosis.

    Returns (ok, exit_code, diagnosis_tag, raw_stderr) where diagnosis_tag is
    one of: 'ok', 'no-key', 'host-key-changed', 'dns', 'refused', 'no-route',
    'tcp-timeout', 'timeout', 'ssh-not-found', 'perm-denied', 'unknown'.
    """
    cmd = ["ssh"]
    if profile.get("port") and int(profile["port"]) != 22:
        cmd += ["-p", str(profile["port"])]
    if profile.get("identity_file"):
        cmd += ["-i", os.path.expanduser(profile["identity_file"])]
    cmd += [
        "-o", "ControlMaster=no",
        "-o", "ControlPath=none",
        "-o", "BatchMode=yes",
        "-o", "StrictHostKeyChecking=accept-new",
        "-o", f"ConnectTimeout={profile.get('connect_timeout', 10)}",
    ]
    for opt in profile.get("ssh_options") or []:
        cmd += ["-o", opt]
    user = profile.get("user")
    target = f"{user}@{profile['host']}" if user else profile["host"]
    cmd += [target, "echo srv-check-ok"]

    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return (False, -1, "timeout", "")
    except FileNotFoundError:
        return (False, 127, "ssh-not-found", "")

    if r.returncode == 0 and "srv-check-ok" in (r.stdout or ""):
        return (True, 0, "ok", "")

    err = (r.stderr or "").lower()
    if "permission denied (publickey" in err:
        diag = "no-key"
    elif "host key verification failed" in err or "remote host identification has changed" in err:
        diag = "host-key-changed"
    elif "could not resolve hostname" in err or "name or service not known" in err \
            or "nodename nor servname provided" in err:
        diag = "dns"
    elif "connection refused" in err:
        diag = "refused"
    elif "no route to host" in err or "network is unreachable" in err:
        diag = "no-route"
    elif "operation timed out" in err or "connection timed out" in err:
        diag = "tcp-timeout"
    elif "permission denied" in err:
        diag = "perm-denied"
    else:
        diag = "unknown"
    return (False, r.returncode, diag, r.stderr or "")


def _check_advice(diag: str, profile: dict, profile_name: str) -> list[str]:
    """Return human-readable, actionable lines for a given diagnosis."""
    user = profile.get("user", "")
    host = profile["host"]
    port = profile.get("port", 22)
    target = f"{user}@{host}" if user else host
    identity = profile.get("identity_file") or "~/.ssh/id_rsa"
    pub = identity if identity.endswith(".pub") else identity + ".pub"

    if diag == "no-key":
        return [
            f"key authentication rejected -- your local public key is NOT in the",
            f"server's authorized_keys.",
            "",
            f"Fix it (pick one):",
            f"  ssh-copy-id -i {pub} {target}",
            f"  # PowerShell equivalent (no ssh-copy-id on Windows):",
            f"  type {pub.replace('~', '$env:USERPROFILE')} | ssh {target} \"cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys\"",
            f"",
            f"After authorizing, re-run: srv check",
        ]
    if diag == "host-key-changed":
        return [
            f"server host key doesn't match the one in known_hosts (or first connection",
            f"with strict host-key checking).",
            "",
            f"If you trust this is the same server and the key really changed:",
            f"  ssh-keygen -R {host}",
            f"  ssh-keyscan -H {host} >> ~/.ssh/known_hosts",
            f"",
            f"Otherwise verify with the server's admin first.",
        ]
    if diag == "dns":
        return [
            f"can't resolve hostname {host!r}.",
            f"  - check the host spelling: srv config show {profile_name}",
            f"  - if it's an IP, double-check digits",
        ]
    if diag == "refused":
        return [
            f"connection refused at {host}:{port}.",
            f"  - is sshd running on the server?",
            f"  - is port {port} correct? (try: srv config set {profile_name} port <N>)",
            f"  - is a firewall blocking?",
        ]
    if diag == "no-route":
        return [
            f"no route to {host}.",
            f"  network unreachable -- check VPN / firewall / interface state.",
        ]
    if diag == "tcp-timeout":
        return [
            f"connection timed out reaching {host}:{port}.",
            f"  - server may be down or unreachable",
            f"  - firewall may be silently dropping",
            f"  - try a longer timeout: srv config set {profile_name} connect_timeout 30",
        ]
    if diag == "timeout":
        return [
            f"probe took longer than the watchdog (15s). Server may be hanging on",
            f"connection setup; try interactively: ssh {target}",
        ]
    if diag == "ssh-not-found":
        return [
            "ssh not in PATH; install OpenSSH client.",
            "  Windows: Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0",
        ]
    if diag == "perm-denied":
        return [
            "auth failed -- the server didn't accept any of your keys, and",
            "password auth is either disabled or BatchMode prevented prompting.",
            "Verify which key the server expects, and ensure it's in authorized_keys.",
        ]
    return ["unknown failure mode -- see stderr above."]


def cmd_check(cfg: dict, profile_override: str | None) -> int:
    name, profile = resolve_profile(cfg, profile_override)
    user = profile.get("user", "")
    host = profile["host"]
    port = profile.get("port", 22)
    identity = profile.get("identity_file")
    target = f"{user}@{host}" if user else host

    print(f"checking {name}: {target}:{port}")
    if identity:
        print(f"  key : {identity}")
    else:
        print(f"  key : (ssh default search; commonly ~/.ssh/id_rsa or id_ed25519)")
    print()

    ok, rc, diag, stderr = _ssh_check(profile)

    if ok:
        print("OK -- connected; key authentication works.")
        return 0

    print(f"FAIL ({diag}; ssh exit {rc})")
    if stderr:
        print()
        print("ssh stderr:")
        for line in stderr.rstrip().splitlines():
            print(f"  {line}")
    print()
    for line in _check_advice(diag, profile, name):
        print(line)
    return 1


def _prompt(question: str, default: str | None = None) -> str:
    suffix = f" [{default}]" if default else ""
    try:
        ans = input(f"{question}{suffix}: ").strip()
    except (EOFError, KeyboardInterrupt):
        print()
        sys.exit(130)
    return ans or (default or "")


def cmd_init(cfg: dict) -> int:
    print("Configure a new SSH profile (Ctrl+C to abort).")
    name = _prompt("profile name", "prod")
    if not name:
        sys.exit("error: profile name required.")
    host = _prompt("host (ip or hostname)")
    if not host:
        sys.exit("error: host required.")
    user = _prompt("user", os.environ.get("USER") or os.environ.get("USERNAME") or "")
    port_in = _prompt("port", "22")
    try:
        port = int(port_in)
    except ValueError:
        sys.exit("error: port must be a number.")
    identity = _prompt("identity file (blank = ssh default)", "")
    default_cwd = _prompt("default cwd", "~")

    cfg["profiles"][name] = {
        "host": host,
        "user": user or None,
        "port": port,
        "identity_file": identity or None,
        "default_cwd": default_cwd,
    }
    if not cfg.get("default_profile"):
        cfg["default_profile"] = name
    save_config(cfg)
    print(f"saved profile {name!r} to {CONFIG_FILE}")
    print()
    print("next: verify connectivity with `srv check` (it'll tell you exactly")
    print("      what to fix if your key isn't in the server's authorized_keys).")
    return 0


def cmd_use(rest: list[str], cfg: dict) -> int:
    """Pin a profile for the current shell session."""
    if not rest:
        sid, rec, _ = _touch_session()
        pinned = rec.get("profile")
        default = cfg.get("default_profile")
        env_override = os.environ.get("SRV_PROFILE")
        print(f"session : {sid}")
        if pinned:
            print(f"pinned  : {pinned}")
        else:
            print(f"pinned  : (none)")
        if env_override:
            print(f"env     : SRV_PROFILE={env_override}")
        print(f"default : {default or '(none)'}")
        active = pinned or env_override or default or "(none)"
        print(f"active  : {active}")
        return 0
    arg = rest[0]
    if arg in ("--clear", "-", "-c"):
        sid = set_session_profile(None)
        print(f"session {sid}: unpinned")
        return 0
    if arg not in cfg["profiles"]:
        sys.exit(f"error: profile {arg!r} not found. Run `srv config list`.")
    sid = set_session_profile(arg)
    cwds = load_sessions()["sessions"][sid].get("cwds") or {}
    cwd = cwds.get(arg) or cfg["profiles"][arg].get("default_cwd") or "~"
    print(f"session {sid}: pinned to {arg!r}  (cwd: {cwd})")
    return 0


def cmd_config(rest: list[str], cfg: dict) -> int:
    if not rest:
        sys.exit("usage: srv config <list|use|remove|show|set> [args]")
    action, *params = rest
    if action == "list":
        if not cfg["profiles"]:
            print("(no profiles configured -- run `srv init`)")
            return 0
        default = cfg.get("default_profile")
        sid, rec, _ = _touch_session()
        pinned = rec.get("profile")
        for n, p in cfg["profiles"].items():
            mark = " "
            if n == pinned:
                mark = "@"  # pinned for this session
            elif n == default:
                mark = "*"  # global default
            target = f"{p.get('user') or ''}@{p['host']}".lstrip("@")
            print(f"{mark} {n:<16} {target}:{p.get('port', 22)}")
        if pinned:
            print(f"\n@ = pinned to this session ({sid})")
        return 0
    if action == "use":
        # Backward-compat: `srv config use NAME` still sets the GLOBAL default.
        if not params:
            sys.exit("usage: srv config use <name>  (sets global default; "
                     "for per-shell switching use `srv use <name>`)")
        if params[0] not in cfg["profiles"]:
            sys.exit(f"error: profile {params[0]!r} not found.")
        cfg["default_profile"] = params[0]
        save_config(cfg)
        print(f"global default profile = {params[0]}")
        return 0
    if action == "remove":
        if not params:
            sys.exit("usage: srv config remove <name>")
        if params[0] not in cfg["profiles"]:
            sys.exit(f"error: profile {params[0]!r} not found.")
        del cfg["profiles"][params[0]]
        if cfg.get("default_profile") == params[0]:
            cfg["default_profile"] = next(iter(cfg["profiles"]), None)
        save_config(cfg)
        print(f"removed {params[0]}")
        return 0
    if action == "show":
        target_name = params[0] if params else cfg.get("default_profile")
        if not target_name or target_name not in cfg["profiles"]:
            sys.exit(f"error: profile {target_name!r} not found.")
        print(json.dumps({target_name: cfg["profiles"][target_name]},
                         indent=2, ensure_ascii=False))
        return 0
    if action == "set":
        if len(params) < 3:
            sys.exit("usage: srv config set <profile> <key> <value>")
        prof, key, value = params[0], params[1], " ".join(params[2:])
        if prof not in cfg["profiles"]:
            sys.exit(f"error: profile {prof!r} not found.")
        v_lower = value.lower()
        if v_lower in ("none", "null", ""):
            cfg["profiles"][prof][key] = None
        elif v_lower in ("true", "false"):
            cfg["profiles"][prof][key] = v_lower == "true"
        elif value.isdigit():
            cfg["profiles"][prof][key] = int(value)
        else:
            cfg["profiles"][prof][key] = value
        save_config(cfg)
        print(f"{prof}.{key} = {cfg['profiles'][prof][key]!r}")
        return 0
    sys.exit(f"error: unknown config action {action!r}")


def cmd_run(command_args: list[str], cfg: dict, profile_override: str | None,
            tty: bool) -> int:
    if not command_args:
        sys.exit("error: nothing to run.")
    name, profile = resolve_profile(cfg, profile_override)
    cmd_str = " ".join(command_args)
    return run_remote(name, profile, cmd_str, tty=tty)


def _scp_args_strip(args: list[str]) -> tuple[list[str], bool]:
    out, recursive = [], False
    for a in args:
        if a in ("-r", "--recursive"):
            recursive = True
        else:
            out.append(a)
    return out, recursive


def cmd_push(args: list[str], cfg: dict, profile_override: str | None) -> int:
    args, recursive_flag = _scp_args_strip(args)
    if not args:
        sys.exit("usage: srv push <local> [<remote>] [-r]")
    local = args[0]
    remote = args[1] if len(args) > 1 else os.path.basename(local.rstrip("/\\"))
    if not os.path.exists(local):
        sys.exit(f"error: local path does not exist: {local}")
    name, profile = resolve_profile(cfg, profile_override)
    cwd = get_cwd(name, profile)
    remote_abs = resolve_remote_path(remote, cwd)
    recursive = recursive_flag or os.path.isdir(local)
    scp_cmd = build_scp_cmd(profile, local, remote_target(profile, remote_abs),
                            recursive=recursive)
    if not shutil.which("scp"):
        sys.exit("error: `scp` not found in PATH. Install the OpenSSH client.")
    return _ssh_call(scp_cmd)


def cmd_pull(args: list[str], cfg: dict, profile_override: str | None) -> int:
    args, recursive = _scp_args_strip(args)
    if not args:
        sys.exit("usage: srv pull <remote> [<local>] [-r]")
    remote = args[0]
    local = args[1] if len(args) > 1 else "."
    name, profile = resolve_profile(cfg, profile_override)
    cwd = get_cwd(name, profile)
    remote_abs = resolve_remote_path(remote, cwd)
    scp_cmd = build_scp_cmd(profile, remote_target(profile, remote_abs), local,
                            recursive=recursive)
    if not shutil.which("scp"):
        sys.exit("error: `scp` not found in PATH. Install the OpenSSH client.")
    return _ssh_call(scp_cmd)


def cmd_list_profiles_internal(cfg: dict) -> int:
    for n in cfg["profiles"]:
        print(n)
    return 0


# --------------------------------------------------------------------------- #
# Bulk sync: git-aware (default) / mtime / glob / explicit list               #
# --------------------------------------------------------------------------- #

DEFAULT_SYNC_EXCLUDES = [
    ".git", "node_modules", "__pycache__", ".venv", "venv",
    ".idea", ".vscode", ".DS_Store", "*.pyc", "*.pyo", "*.swp",
]


def _find_git_root(start: str) -> str | None:
    """Walk up from `start` until a `.git` entry is found. Returns repo root."""
    p = Path(start).resolve()
    for parent in [p] + list(p.parents):
        if (parent / ".git").exists():
            return str(parent)
    return None


def _git_changed_files(repo_root: str, scope: str = "all") -> list[str]:
    """Return relative paths of changed files. scope: all|staged|modified|untracked."""
    out: set[str] = set()
    if scope in ("all", "modified", "untracked"):
        flags = []
        if scope in ("all", "modified"):
            flags.append("--modified")
        if scope in ("all", "untracked"):
            flags += ["--others", "--exclude-standard"]
        if flags:
            r = subprocess.run(
                ["git", "-C", repo_root, "ls-files", "-z", *flags],
                capture_output=True, check=False,
            )
            if r.returncode == 0:
                for p in r.stdout.decode("utf-8", "replace").split("\x00"):
                    if p:
                        out.add(p)
    if scope in ("all", "staged"):
        r = subprocess.run(
            ["git", "-C", repo_root, "diff", "--name-only", "--cached", "-z"],
            capture_output=True, check=False,
        )
        if r.returncode == 0:
            for p in r.stdout.decode("utf-8", "replace").split("\x00"):
                if p:
                    out.add(p)
    return sorted(out)


def _parse_duration(s: str) -> float:
    """Parse '2h', '30m', '1d', '90s' into seconds. Bare digits = seconds."""
    s = s.strip().lower()
    if not s:
        sys.exit("error: empty duration")
    suffix = s[-1]
    try:
        if suffix == "s":
            return float(s[:-1])
        if suffix == "m":
            return float(s[:-1]) * 60
        if suffix == "h":
            return float(s[:-1]) * 3600
        if suffix == "d":
            return float(s[:-1]) * 86400
        return float(s)
    except ValueError:
        sys.exit(f"error: bad duration {s!r} (expected like '2h', '30m', '1d', '90s')")


def _mtime_changed_files(root: str, since_str: str,
                         excludes: list[str]) -> list[str]:
    delta = _parse_duration(since_str)
    cutoff = time.time() - delta
    out: list[str] = []
    skip_dirs = {"__pycache__", ".git", "node_modules", ".venv", "venv",
                 ".idea", ".vscode"}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in skip_dirs]
        for f in filenames:
            full = os.path.join(dirpath, f)
            try:
                if os.stat(full).st_mtime >= cutoff:
                    rel = os.path.relpath(full, root).replace("\\", "/")
                    if not _matches_any_exclude(rel, excludes):
                        out.append(rel)
            except OSError:
                pass
    return sorted(out)


def _glob_files(root: str, patterns: list[str]) -> list[str]:
    out: set[str] = set()
    for pat in patterns:
        # Glob is rooted at `root`; ** requires recursive=True
        for m in _glob.glob(os.path.join(root, pat), recursive=True):
            if os.path.isfile(m):
                rel = os.path.relpath(m, root).replace("\\", "/")
                out.add(rel)
    return sorted(out)


def _matches_any_exclude(path: str, patterns: list[str]) -> bool:
    """Path is forward-slash relative. Patterns are shell globs; a bare name
    matches any path component (so `node_modules` excludes `a/node_modules/b`)."""
    norm = path.replace("\\", "/")
    parts = norm.split("/")
    for raw in patterns:
        pat = raw.rstrip("/")
        if "/" in pat or "*" in pat:
            if fnmatch.fnmatch(norm, pat):
                return True
        if any(fnmatch.fnmatch(part, pat) for part in parts):
            return True
    return False


def _normalize_for_tar(local_root: str, path: str) -> str | None:
    """Return path relative to local_root using forward slashes, or None if
    the input falls outside the root."""
    abs_path = os.path.abspath(path)
    abs_root = os.path.abspath(local_root)
    try:
        rel = os.path.relpath(abs_path, abs_root)
    except ValueError:
        return None
    if rel.startswith("..") or os.path.isabs(rel):
        return None
    return rel.replace("\\", "/")


def _tar_pipe_upload(profile: dict, local_root: str, files: list[str],
                     remote_root: str) -> int:
    """Stream local files through `tar -cf - | ssh remote tar -xf -`."""
    if not shutil.which("tar"):
        sys.exit("error: `tar` not found in PATH (Win10+ ships tar.exe).")
    tar_args = ["tar", "-cf", "-", "-C", local_root, *files]
    remote_cmd = (
        f"mkdir -p {shlex.quote(remote_root)} && "
        f"cd {shlex.quote(remote_root)} && tar -xf -"
    )
    ssh_cmd = build_ssh_cmd(profile, remote_cmd)
    try:
        p1 = subprocess.Popen(tar_args, stdout=subprocess.PIPE)
        try:
            p2 = subprocess.Popen(ssh_cmd, stdin=p1.stdout)
        except FileNotFoundError:
            p1.kill()
            sys.exit("error: `ssh` not found in PATH.")
        if p1.stdout:
            p1.stdout.close()
        rc = p2.wait()
        p1.wait()
    except FileNotFoundError as e:
        sys.exit(f"error: {e}")
    return rc


def _parse_sync_opts(args: list[str]) -> dict:
    opts: dict = {
        "remote_root": None, "mode": None, "git_scope": "all",
        "no_git": False, "since": None, "include": [], "exclude": [],
        "files": [], "root": None, "dry_run": False,
    }
    positional: list[str] = []
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--":
            opts["mode"] = opts["mode"] or "list"
            opts["files"].extend(args[i + 1:])
            break
        if a == "--git":
            opts["mode"] = "git"
            if i + 1 < len(args) and args[i + 1] in ("all", "staged", "modified", "untracked"):
                opts["git_scope"] = args[i + 1]
                i += 2
                continue
            i += 1
            continue
        if a in ("--all", "--staged", "--modified", "--untracked"):
            opts["mode"] = "git"
            opts["git_scope"] = a[2:]
            i += 1
            continue
        if a == "--no-git":
            opts["no_git"] = True
            i += 1
            continue
        if a == "--since":
            opts["mode"] = "mtime"
            opts["since"] = args[i + 1]
            i += 2
            continue
        if a.startswith("--since="):
            opts["mode"] = "mtime"
            opts["since"] = a.split("=", 1)[1]
            i += 1
            continue
        if a == "--include":
            opts["mode"] = "glob"
            opts["include"].append(args[i + 1])
            i += 2
            continue
        if a.startswith("--include="):
            opts["mode"] = "glob"
            opts["include"].append(a.split("=", 1)[1])
            i += 1
            continue
        if a == "--exclude":
            opts["exclude"].append(args[i + 1])
            i += 2
            continue
        if a.startswith("--exclude="):
            opts["exclude"].append(a.split("=", 1)[1])
            i += 1
            continue
        if a == "--files":
            opts["mode"] = "list"
            opts["files"].append(args[i + 1])
            i += 2
            continue
        if a == "--root":
            opts["root"] = args[i + 1]
            i += 2
            continue
        if a.startswith("--root="):
            opts["root"] = a.split("=", 1)[1]
            i += 1
            continue
        if a == "--dry-run":
            opts["dry_run"] = True
            i += 1
            continue
        if a.startswith("-"):
            sys.exit(f"error: unknown sync option {a!r}")
        positional.append(a)
        i += 1
    if positional:
        if len(positional) > 1:
            sys.exit(f"error: only one remote root accepted, got {positional!r}")
        opts["remote_root"] = positional[0]
    return opts


def _collect_sync_files(opts: dict, local_root: str,
                        all_excludes: list[str]) -> list[str]:
    mode = opts["mode"]
    if mode == "git":
        files = _git_changed_files(local_root, opts["git_scope"])
    elif mode == "mtime":
        files = _mtime_changed_files(local_root, opts["since"], all_excludes)
    elif mode == "glob":
        if not opts["include"]:
            sys.exit("error: --include requires at least one pattern")
        files = _glob_files(local_root, opts["include"])
    elif mode == "list":
        files = []
        for p in opts["files"]:
            rel = _normalize_for_tar(local_root, p)
            if rel is None:
                sys.stderr.write(f"warning: skipping {p!r} (outside local root)\n")
                continue
            files.append(rel)
    else:
        sys.exit("internal: no sync mode resolved")

    # Apply excludes. List mode is user-explicit, so we only honor user-supplied
    # excludes and skip the broad defaults. Other modes get the full set.
    active_excludes = opts["exclude"] if mode == "list" else all_excludes
    if active_excludes:
        files = [f for f in files if not _matches_any_exclude(f, active_excludes)]

    # Drop entries that no longer exist (deleted-in-worktree, stale globs)
    return [f for f in files if os.path.isfile(os.path.join(local_root, f))]


def cmd_sync(args: list[str], cfg: dict, profile_override: str | None) -> int:
    opts = _parse_sync_opts(args)
    name, profile = resolve_profile(cfg, profile_override)

    # local root
    local_root = opts["root"]
    if not local_root:
        if opts["mode"] == "git" or (opts["mode"] is None and not opts["no_git"]):
            local_root = _find_git_root(os.getcwd())
        if not local_root:
            local_root = os.getcwd()
    local_root = os.path.abspath(local_root)
    if not os.path.isdir(local_root):
        sys.exit(f"error: local root not a directory: {local_root}")

    # mode auto-detect
    if opts["mode"] is None:
        if not opts["no_git"] and _find_git_root(local_root):
            opts["mode"] = "git"
        else:
            reason = ("git auto-detect disabled (--no-git)" if opts["no_git"]
                      else "not in a git repo")
            sys.exit(f"error: {reason}. Specify --include / --since / --files.")

    # remote root
    remote_root_arg = opts["remote_root"]
    cwd = get_cwd(name, profile)
    if remote_root_arg:
        remote_root = resolve_remote_path(remote_root_arg, cwd)
    elif profile.get("sync_root"):
        remote_root = resolve_remote_path(profile["sync_root"], cwd)
    else:
        remote_root = cwd

    # collect files
    all_excludes = list(opts["exclude"])
    all_excludes += list(profile.get("sync_exclude") or [])
    all_excludes += DEFAULT_SYNC_EXCLUDES
    files = _collect_sync_files(opts, local_root, all_excludes)

    # report
    summary = (
        f"mode    : {opts['mode']}"
        + (f" ({opts['git_scope']})" if opts["mode"] == "git" else "")
        + (f" since {opts['since']}" if opts["mode"] == "mtime" else "")
        + "\n"
        f"local   : {local_root}\n"
        f"remote  : {profile.get('user') or ''}@{profile['host']}:{remote_root}\n"
        f"files   : {len(files)}"
    )
    sys.stderr.write(summary + "\n")
    if not files:
        sys.stderr.write("(nothing to sync)\n")
        return 0
    for f in files[:200]:
        sys.stderr.write(f"  {f}\n")
    if len(files) > 200:
        sys.stderr.write(f"  ... ({len(files) - 200} more)\n")

    if opts["dry_run"]:
        sys.stderr.write("(dry-run, not transferred)\n")
        return 0

    return _tar_pipe_upload(profile, local_root, files, remote_root)


# --------------------------------------------------------------------------- #
# Sessions subcommand                                                         #
# --------------------------------------------------------------------------- #

def cmd_sessions(rest: list[str]) -> int:
    action = rest[0] if rest else "list"
    s = load_sessions()
    sessions = s.get("sessions", {})
    current = _session_id()

    if action == "list":
        if not sessions:
            print("(no sessions)")
            return 0
        rows = []
        for sid, rec in sessions.items():
            mark = ">" if sid == current else " "
            alive = "alive" if _pid_alive(sid) else "dead"
            cwds = rec.get("cwds") or {}
            cwd_summary = ", ".join(f"{k}={v}" for k, v in cwds.items()) or "-"
            rows.append((mark, sid, alive, rec.get("profile") or "-",
                         rec.get("last_seen", "-"), cwd_summary))
        # Print aligned
        for mark, sid, alive, prof, seen, cwds in rows:
            print(f"{mark} {sid:<8} {alive:<6} profile={prof:<10} "
                  f"last_seen={seen}  cwds={cwds}")
        print(f"\n> = current session ({current})")
        return 0

    if action == "show":
        _touch_session()
        rec = load_sessions()["sessions"].get(current, {})
        print(json.dumps({current: rec}, indent=2, ensure_ascii=False))
        return 0

    if action == "clear":
        if current in sessions:
            del sessions[current]
            save_sessions(s)
            print(f"cleared session {current}")
        else:
            print(f"session {current} has no record")
        return 0

    if action == "prune":
        before = len(sessions)
        dead = [sid for sid in sessions if not _pid_alive(sid)]
        for sid in dead:
            del sessions[sid]
        save_sessions(s)
        print(f"pruned {len(dead)}/{before} sessions ({len(sessions)} remaining)")
        return 0

    sys.exit(f"error: unknown sessions action {action!r}")


# --------------------------------------------------------------------------- #
# Detached jobs                                                               #
# --------------------------------------------------------------------------- #

def _gen_job_id() -> str:
    return time.strftime("%Y%m%d-%H%M%S") + "-" + secrets.token_hex(2)


def _find_job(jid: str, jobs: dict) -> dict | None:
    matches = [j for j in jobs.get("jobs", []) if j["id"] == jid]
    if matches:
        return matches[0]
    prefix = [j for j in jobs.get("jobs", []) if j["id"].startswith(jid)]
    if len(prefix) == 1:
        return prefix[0]
    if len(prefix) > 1:
        sys.exit(f"error: ambiguous job id {jid!r} matches {len(prefix)} jobs.")
    return None


def _spawn_detached(profile_name: str, profile: dict,
                    user_cmd: str) -> tuple[dict | None, str]:
    cwd = get_cwd(profile_name, profile)
    job_id = _gen_job_id()
    log_path = f"~/.srv-jobs/{job_id}.log"
    encoded = base64.b64encode(user_cmd.encode("utf-8")).decode("ascii")
    remote_cmd = (
        f"mkdir -p ~/.srv-jobs && "
        f"cd {shlex.quote(cwd)} && "
        f"(nohup bash -c \"$(echo {encoded} | base64 -d)\" "
        f"</dev/null >{log_path} 2>&1 & echo $!)"
    )
    ssh_cmd = build_ssh_cmd(profile, remote_cmd, capture=True)
    try:
        r = _ssh_run(ssh_cmd, retry=False)
    except FileNotFoundError:
        return None, "ssh not found in PATH"
    if r.returncode != 0:
        return None, (r.stderr or "").strip() or f"spawn failed (exit {r.returncode})"
    pid_lines = [ln.strip() for ln in (r.stdout or "").splitlines() if ln.strip()]
    pid = 0
    for ln in reversed(pid_lines):
        if ln.isdigit():
            pid = int(ln)
            break
    if not pid:
        return None, "remote did not return a pid"
    record = {
        "id": job_id,
        "profile": profile_name,
        "cmd": user_cmd,
        "cwd": cwd,
        "pid": pid,
        "log": log_path,
        "started": _now_iso(),
    }
    jobs = load_jobs()
    jobs["jobs"].append(record)
    save_jobs(jobs)
    return record, ""


def cmd_detach(command_args: list[str], cfg: dict,
               profile_override: str | None) -> int:
    if not command_args:
        sys.exit("error: srv -d needs a command.")
    name, profile = resolve_profile(cfg, profile_override)
    user_cmd = " ".join(command_args)
    rec, err = _spawn_detached(name, profile, user_cmd)
    if err:
        sys.stderr.write(err + "\n")
        return 1
    assert rec is not None
    print(f"job   {rec['id']}  pid={rec['pid']}  profile={rec['profile']}")
    print(f"log   {rec['log']}")
    print(f"tail  srv logs {rec['id']} -f")
    print(f"kill  srv kill {rec['id']}")
    return 0


def cmd_jobs(cfg: dict, profile_override: str | None) -> int:
    jobs = load_jobs().get("jobs", [])
    if profile_override:
        jobs = [j for j in jobs if j["profile"] == profile_override]
    if not jobs:
        print("(no jobs)")
        return 0
    for j in jobs:
        cmd = j["cmd"] if len(j["cmd"]) <= 60 else j["cmd"][:57] + "..."
        print(f"{j['id']}  pid={j['pid']:<7} profile={j['profile']:<10} "
              f"started={j['started']}  cmd={cmd}")
    return 0


def cmd_logs(args: list[str], cfg: dict, profile_override: str | None) -> int:
    if not args or args[0] in ("-f", "--follow"):
        sys.exit("usage: srv logs <id> [-f]")
    jid = args[0]
    follow = ("-f" in args[1:]) or ("--follow" in args[1:])
    jobs = load_jobs()
    j = _find_job(jid, jobs)
    if not j:
        sys.exit(f"error: no such job {jid!r}")
    if j["profile"] not in cfg["profiles"]:
        sys.exit(f"error: profile {j['profile']!r} (from job) not found.")
    profile = cfg["profiles"][j["profile"]]
    cmd = ("tail -f " if follow else "cat ") + j["log"]
    return run_remote(j["profile"], profile, cmd, tty=follow)


def cmd_kill(args: list[str], cfg: dict, profile_override: str | None) -> int:
    if not args:
        sys.exit("usage: srv kill <id>")
    jid = args[0]
    sig = "TERM"
    for a in args[1:]:
        if a.startswith("--signal="):
            sig = a.split("=", 1)[1]
        elif a == "-9":
            sig = "KILL"
    jobs = load_jobs()
    j = _find_job(jid, jobs)
    if not j:
        sys.exit(f"error: no such job {jid!r}")
    if j["profile"] not in cfg["profiles"]:
        sys.exit(f"error: profile {j['profile']!r} (from job) not found.")
    profile = cfg["profiles"][j["profile"]]
    pid = j["pid"]
    rc = run_remote(
        j["profile"], profile,
        f"kill -{sig} {pid} 2>/dev/null && echo killed || echo 'no such pid (already exited?)'",
    )
    jobs["jobs"] = [x for x in jobs["jobs"] if x["id"] != j["id"]]
    save_jobs(jobs)
    return rc


# --------------------------------------------------------------------------- #
# Shell completion                                                            #
# --------------------------------------------------------------------------- #

_BASH_COMPLETION = r"""# srv bash completion
_srv() {
    local cur prev words cword
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]:-}"
    local subs="init config use cd pwd status check run exec push pull sync jobs logs kill sessions completion mcp help version"
    local sub=""
    local i
    for ((i=1; i<COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            -P|--profile) i=$((i+1)) ;;
            --profile=*|-t|--tty|-d|--detach) ;;
            *) sub="${COMP_WORDS[i]}"; break ;;
        esac
    done
    if [[ -z "$sub" ]]; then
        COMPREPLY=( $(compgen -W "$subs" -- "$cur") )
        return 0
    fi
    case "$sub" in
        config)
            local action=""
            for ((i=1; i<COMP_CWORD; i++)); do
                case "${COMP_WORDS[i]}" in
                    -P|--profile) i=$((i+1)) ;;
                    config) ;;
                    *) action="${COMP_WORDS[i]}"; break ;;
                esac
            done
            if [[ "$action" == "config" || -z "$action" ]]; then
                COMPREPLY=( $(compgen -W "list use remove show set" -- "$cur") )
            elif [[ "$action" == "use" || "$action" == "remove" || "$action" == "show" ]]; then
                local profs
                profs=$(srv _profiles 2>/dev/null)
                COMPREPLY=( $(compgen -W "$profs" -- "$cur") )
            fi
            ;;
        use)
            local profs
            profs=$(srv _profiles 2>/dev/null)
            COMPREPLY=( $(compgen -W "$profs --clear" -- "$cur") )
            ;;
        sessions)
            COMPREPLY=( $(compgen -W "list show clear prune" -- "$cur") )
            ;;
        completion)
            COMPREPLY=( $(compgen -W "bash zsh powershell" -- "$cur") )
            ;;
        push)
            COMPREPLY=( $(compgen -f -- "$cur") )
            ;;
    esac
    if [[ "$prev" == "-P" || "$prev" == "--profile" ]]; then
        local profs
        profs=$(srv _profiles 2>/dev/null)
        COMPREPLY=( $(compgen -W "$profs" -- "$cur") )
    fi
}
complete -F _srv srv
"""

_ZSH_COMPLETION = r"""#compdef srv
_srv() {
    local -a subs
    subs=(
        'init:configure a profile'
        'config:manage profiles'
        'use:pin a profile for this shell'
        'cd:change persistent remote cwd'
        'pwd:show remote cwd'
        'status:show profile and cwd'
        'check:probe SSH connectivity and diagnose failures'
        'run:run a command on remote'
        'push:upload via scp'
        'pull:download via scp'
        'sync:bulk-sync changed files (git/mtime/glob/list)'
        'jobs:list detached jobs'
        'logs:tail a detached job log'
        'kill:terminate a detached job'
        'sessions:list/manage shell sessions'
        'completion:emit shell completion script'
        'mcp:run as a stdio MCP server'
        'help:show help'
        'version:show version'
    )
    if (( CURRENT == 2 )); then
        _describe 'subcommand' subs
        return
    fi
    case "$words[2]" in
        config)
            if (( CURRENT == 3 )); then
                _values 'action' list use remove show set
            elif [[ "$words[3]" == (use|remove|show) ]]; then
                local profs
                profs=("${(@f)$(srv _profiles 2>/dev/null)}")
                _values 'profile' $profs
            fi
            ;;
        use)
            local profs
            profs=("${(@f)$(srv _profiles 2>/dev/null)}")
            _values 'profile' $profs --clear
            ;;
        sessions) _values 'action' list show clear prune ;;
        completion) _values 'shell' bash zsh powershell ;;
        push|pull) _files ;;
    esac
}
_srv "$@"
"""

_POWERSHELL_COMPLETION = r"""# srv PowerShell completion
Register-ArgumentCompleter -Native -CommandName srv -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $tokens = $commandAst.CommandElements |
        ForEach-Object { $_.ToString() } |
        Where-Object { $_ -ne 'srv' -and $_ -ne 'srv.cmd' }
    $skip = $false
    $sub = $null
    foreach ($t in $tokens) {
        if ($skip) { $skip = $false; continue }
        if ($t -in '-P', '--profile') { $skip = $true; continue }
        if ($t -like '--profile=*' -or $t -in '-t', '--tty', '-d', '--detach') { continue }
        $sub = $t
        break
    }
    $subs = 'init','config','use','cd','pwd','status','check','run','exec','push','pull','sync','jobs','logs','kill','sessions','completion','mcp','help','version'
    if (-not $sub -or $sub -eq $wordToComplete) {
        $subs | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        return
    }
    switch ($sub) {
        'config' {
            'list','use','remove','show','set' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'use' {
            $profs = (& srv _profiles 2>$null) -split "`n" | Where-Object { $_ }
            ($profs + '--clear') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'sessions' {
            'list','show','clear','prune' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'completion' {
            'bash','zsh','powershell' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
    }
}
"""


def cmd_completion(rest: list[str]) -> int:
    if not rest:
        sys.exit("usage: srv completion <bash|zsh|powershell>")
    shell = rest[0].lower()
    if shell == "bash":
        sys.stdout.write(_BASH_COMPLETION)
    elif shell == "zsh":
        sys.stdout.write(_ZSH_COMPLETION)
    elif shell in ("powershell", "pwsh", "ps"):
        sys.stdout.write(_POWERSHELL_COMPLETION)
    else:
        sys.exit(f"error: unknown shell {shell!r} (expected bash/zsh/powershell)")
    return 0


# --------------------------------------------------------------------------- #
# MCP server (stdio JSON-RPC 2.0)                                             #
# --------------------------------------------------------------------------- #

MCP_PROTOCOL_VERSION = "2024-11-05"


def _mcp_tool_defs() -> list[dict]:
    return [
        {
            "name": "run",
            "description": "Run a shell command on the configured remote SSH server in the persisted cwd. Returns stdout, stderr, and exit code.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "command": {"type": "string"},
                    "profile": {"type": "string", "description": "Optional profile override."},
                },
                "required": ["command"],
            },
        },
        {
            "name": "cd",
            "description": "Change the persisted remote working directory for THIS MCP session. Validated by `cd <path> && pwd` on the remote.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "profile": {"type": "string"},
                },
                "required": ["path"],
            },
        },
        {
            "name": "pwd",
            "description": "Get the persisted remote working directory for this session.",
            "inputSchema": {"type": "object", "properties": {"profile": {"type": "string"}}},
        },
        {
            "name": "use",
            "description": "Pin a profile for this MCP session (subsequent calls without `profile` will use it). Pass `clear: true` to unpin.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "profile": {"type": "string"},
                    "clear": {"type": "boolean", "default": False},
                },
            },
        },
        {
            "name": "status",
            "description": "Get the active profile, target host, current cwd, and session id.",
            "inputSchema": {"type": "object", "properties": {"profile": {"type": "string"}}},
        },
        {
            "name": "list_profiles",
            "description": "List configured SSH profiles, the global default, and what's pinned for this session.",
            "inputSchema": {"type": "object", "properties": {}},
        },
        {
            "name": "check",
            "description": "Probe SSH connectivity (BatchMode=yes, no hangs). Returns a diagnosis tag (ok / no-key / host-key-changed / dns / refused / no-route / tcp-timeout / timeout / perm-denied / unknown) and human-readable fix steps for failures.",
            "inputSchema": {
                "type": "object",
                "properties": {"profile": {"type": "string"}},
            },
        },
        {
            "name": "push",
            "description": "Upload a local file or directory to the remote server via scp.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "local": {"type": "string"},
                    "remote": {"type": "string"},
                    "profile": {"type": "string"},
                },
                "required": ["local"],
            },
        },
        {
            "name": "pull",
            "description": "Download a remote file or directory to the local machine via scp.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "remote": {"type": "string"},
                    "local": {"type": "string"},
                    "recursive": {"type": "boolean", "default": False},
                    "profile": {"type": "string"},
                },
                "required": ["remote"],
            },
        },
        {
            "name": "sync",
            "description": "Bulk-sync changed files from local to remote, preserving relative paths via tar pipe. Auto-detects git repo. Returns the file list and exit code.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "remote_root": {"type": "string", "description": "Remote target root; defaults to profile sync_root or current cwd."},
                    "mode": {"type": "string", "enum": ["git", "mtime", "glob", "list"], "description": "If omitted, defaults to git when in a repo."},
                    "git_scope": {"type": "string", "enum": ["all", "staged", "modified", "untracked"], "default": "all"},
                    "since": {"type": "string", "description": "Duration like '2h', '30m', '1d' (mtime mode)."},
                    "include": {"type": "array", "items": {"type": "string"}, "description": "Glob patterns (glob mode)."},
                    "exclude": {"type": "array", "items": {"type": "string"}},
                    "files": {"type": "array", "items": {"type": "string"}, "description": "Explicit relative paths (list mode)."},
                    "root": {"type": "string", "description": "Local root; defaults to git toplevel or cwd."},
                    "dry_run": {"type": "boolean", "default": False},
                    "profile": {"type": "string"},
                },
            },
        },
        {
            "name": "detach",
            "description": "Spawn a long-running command on the remote server, detached via nohup. Returns immediately with a job id and pid.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "command": {"type": "string"},
                    "profile": {"type": "string"},
                },
                "required": ["command"],
            },
        },
        {
            "name": "list_jobs",
            "description": "List local records of detached jobs.",
            "inputSchema": {"type": "object", "properties": {"profile": {"type": "string"}}},
        },
        {
            "name": "tail_log",
            "description": "Read the last N lines of a detached job's remote log file.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "lines": {"type": "integer", "default": 200},
                },
                "required": ["id"],
            },
        },
        {
            "name": "kill_job",
            "description": "Send a signal (TERM by default) to a detached job's pid and forget it.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "signal": {"type": "string", "default": "TERM"},
                },
                "required": ["id"],
            },
        },
    ]


def _mcp_handle_tool(name: str, args: dict, cfg: dict) -> dict:
    profile_override = args.get("profile")

    if name == "run":
        command = args.get("command", "")
        if not command:
            return {"isError": True, "content": [{"type": "text", "text": "error: command is required"}]}
        prof_name, profile = resolve_profile(cfg, profile_override)
        result = run_remote_capture(prof_name, profile, command)
        text = ""
        if result["stdout"]:
            text += result["stdout"]
        if result["stderr"]:
            text += ("\n--- stderr ---\n" if text else "") + result["stderr"]
        text += f"\n[exit {result['exit_code']} cwd {result['cwd']}]"
        return {
            "content": [{"type": "text", "text": text}],
            "isError": result["exit_code"] != 0,
            "structuredContent": result,
        }

    if name == "cd":
        path = args.get("path")
        prof_name, profile = resolve_profile(cfg, profile_override)
        new_cwd, err = change_remote_cwd(prof_name, profile, path)
        if err:
            return {"isError": True, "content": [{"type": "text", "text": err}]}
        return {
            "content": [{"type": "text", "text": new_cwd or ""}],
            "structuredContent": {"cwd": new_cwd, "profile": prof_name},
        }

    if name == "pwd":
        prof_name, profile = resolve_profile(cfg, profile_override)
        cwd = get_cwd(prof_name, profile)
        return {
            "content": [{"type": "text", "text": cwd}],
            "structuredContent": {"cwd": cwd, "profile": prof_name},
        }

    if name == "use":
        if args.get("clear"):
            sid = set_session_profile(None)
            return {
                "content": [{"type": "text", "text": f"session {sid}: unpinned"}],
                "structuredContent": {"session": sid, "profile": None},
            }
        target = args.get("profile")
        if not target:
            sid, rec, _ = _touch_session()
            return {
                "content": [{"type": "text", "text": json.dumps({
                    "session": sid,
                    "pinned": rec.get("profile"),
                    "default": cfg.get("default_profile"),
                }, indent=2)}],
                "structuredContent": {
                    "session": sid,
                    "pinned": rec.get("profile"),
                    "default": cfg.get("default_profile"),
                },
            }
        if target not in cfg["profiles"]:
            return {"isError": True, "content": [{"type": "text", "text": f"profile {target!r} not found"}]}
        sid = set_session_profile(target)
        return {
            "content": [{"type": "text", "text": f"session {sid}: pinned to {target!r}"}],
            "structuredContent": {"session": sid, "profile": target},
        }

    if name == "status":
        prof_name, profile = resolve_profile(cfg, profile_override)
        sid, rec, _ = _touch_session()
        info = {
            "profile": prof_name,
            "pinned": rec.get("profile"),
            "host": profile["host"],
            "user": profile.get("user"),
            "port": profile.get("port", 22),
            "identity_file": profile.get("identity_file"),
            "cwd": get_cwd(prof_name, profile),
            "session": sid,
            "multiplex": profile.get("multiplex", True),
            "compression": profile.get("compression", True),
        }
        return {
            "content": [{"type": "text", "text": json.dumps(info, indent=2)}],
            "structuredContent": info,
        }

    if name == "list_profiles":
        sid, rec, _ = _touch_session()
        info = {
            "default": cfg.get("default_profile"),
            "pinned": rec.get("profile"),
            "session": sid,
            "profiles": list(cfg["profiles"].keys()),
        }
        return {
            "content": [{"type": "text", "text": json.dumps(info, indent=2)}],
            "structuredContent": info,
        }

    if name == "check":
        prof_name, profile = resolve_profile(cfg, profile_override)
        ok, rc, diag, stderr = _ssh_check(profile)
        advice = _check_advice(diag, profile, prof_name) if not ok else []
        info = {
            "profile": prof_name,
            "host": profile["host"],
            "user": profile.get("user"),
            "port": profile.get("port", 22),
            "ok": ok,
            "diagnosis": diag,
            "exit_code": rc,
            "stderr": stderr,
            "advice": advice,
        }
        if ok:
            text = f"OK -- {prof_name}: {profile.get('user') or ''}@{profile['host']} key auth works."
        else:
            text = f"FAIL ({diag}): {stderr.strip() or '(no stderr)'}\n\n" + "\n".join(advice)
        return {
            "content": [{"type": "text", "text": text}],
            "isError": not ok,
            "structuredContent": info,
        }

    if name == "push":
        local = args.get("local")
        if not local or not os.path.exists(local):
            return {"isError": True, "content": [{"type": "text", "text": f"local path missing: {local!r}"}]}
        prof_name, profile = resolve_profile(cfg, profile_override)
        cwd = get_cwd(prof_name, profile)
        remote = args.get("remote") or os.path.basename(local.rstrip("/\\"))
        remote_abs = resolve_remote_path(remote, cwd)
        recursive = bool(args.get("recursive")) or os.path.isdir(local)
        scp_cmd = build_scp_cmd(profile, local, remote_target(profile, remote_abs),
                                recursive=recursive, capture=True)
        try:
            r = _ssh_run(scp_cmd)
        except FileNotFoundError:
            return {"isError": True, "content": [{"type": "text", "text": "scp not found in PATH"}]}
        ok = r.returncode == 0
        text = (r.stdout + r.stderr) or ("uploaded" if ok else "upload failed")
        return {
            "content": [{"type": "text", "text": text.strip() + f"\n[exit {r.returncode}]"}],
            "isError": not ok,
            "structuredContent": {"exit_code": r.returncode, "remote": remote_abs, "local": local},
        }

    if name == "pull":
        remote = args.get("remote")
        if not remote:
            return {"isError": True, "content": [{"type": "text", "text": "remote is required"}]}
        prof_name, profile = resolve_profile(cfg, profile_override)
        cwd = get_cwd(prof_name, profile)
        local = args.get("local") or "."
        remote_abs = resolve_remote_path(remote, cwd)
        recursive = bool(args.get("recursive"))
        scp_cmd = build_scp_cmd(profile, remote_target(profile, remote_abs), local,
                                recursive=recursive, capture=True)
        try:
            r = _ssh_run(scp_cmd)
        except FileNotFoundError:
            return {"isError": True, "content": [{"type": "text", "text": "scp not found in PATH"}]}
        ok = r.returncode == 0
        text = (r.stdout + r.stderr) or ("downloaded" if ok else "download failed")
        return {
            "content": [{"type": "text", "text": text.strip() + f"\n[exit {r.returncode}]"}],
            "isError": not ok,
            "structuredContent": {"exit_code": r.returncode, "remote": remote_abs, "local": local},
        }

    if name == "sync":
        prof_name, profile = resolve_profile(cfg, profile_override)
        opts = {
            "remote_root": args.get("remote_root"),
            "mode": args.get("mode"),
            "git_scope": args.get("git_scope") or "all",
            "no_git": False,
            "since": args.get("since"),
            "include": list(args.get("include") or []),
            "exclude": list(args.get("exclude") or []),
            "files": list(args.get("files") or []),
            "root": args.get("root"),
            "dry_run": bool(args.get("dry_run")),
        }
        local_root = opts["root"] or _find_git_root(os.getcwd()) or os.getcwd()
        local_root = os.path.abspath(local_root)
        if opts["mode"] is None:
            if _find_git_root(local_root):
                opts["mode"] = "git"
            elif opts["include"]:
                opts["mode"] = "glob"
            elif opts["since"]:
                opts["mode"] = "mtime"
            elif opts["files"]:
                opts["mode"] = "list"
            else:
                return {"isError": True, "content": [{"type": "text", "text":
                    "no mode resolved (not a git repo and no include/since/files)"}]}
        cwd = get_cwd(prof_name, profile)
        if opts["remote_root"]:
            remote_root = resolve_remote_path(opts["remote_root"], cwd)
        elif profile.get("sync_root"):
            remote_root = resolve_remote_path(profile["sync_root"], cwd)
        else:
            remote_root = cwd
        all_excludes = list(opts["exclude"]) + list(profile.get("sync_exclude") or []) + DEFAULT_SYNC_EXCLUDES
        try:
            files = _collect_sync_files(opts, local_root, all_excludes)
        except SystemExit as e:
            return {"isError": True, "content": [{"type": "text", "text": str(e)}]}
        if not files:
            return {
                "content": [{"type": "text", "text": "(nothing to sync)"}],
                "structuredContent": {"files": [], "remote_root": remote_root, "exit_code": 0},
            }
        if opts["dry_run"]:
            return {
                "content": [{"type": "text", "text":
                    f"would sync {len(files)} files to {remote_root}\n"
                    + "\n".join(files[:200])
                    + (f"\n... ({len(files)-200} more)" if len(files) > 200 else "")}],
                "structuredContent": {"files": files, "remote_root": remote_root, "dry_run": True},
            }
        rc = _tar_pipe_upload(profile, local_root, files, remote_root)
        return {
            "content": [{"type": "text", "text":
                f"synced {len(files)} files to {remote_root} [exit {rc}]"}],
            "isError": rc != 0,
            "structuredContent": {"files": files, "remote_root": remote_root, "exit_code": rc},
        }

    if name == "detach":
        command = args.get("command", "")
        if not command:
            return {"isError": True, "content": [{"type": "text", "text": "command is required"}]}
        prof_name, profile = resolve_profile(cfg, profile_override)
        rec, err = _spawn_detached(prof_name, profile, command)
        if err:
            return {"isError": True, "content": [{"type": "text", "text": err}]}
        return {
            "content": [{"type": "text", "text": json.dumps(rec, indent=2)}],
            "structuredContent": rec,
        }

    if name == "list_jobs":
        jobs = load_jobs().get("jobs", [])
        if profile_override:
            jobs = [j for j in jobs if j["profile"] == profile_override]
        return {
            "content": [{"type": "text", "text": json.dumps(jobs, indent=2)}],
            "structuredContent": {"jobs": jobs},
        }

    if name == "tail_log":
        jid = args.get("id", "")
        lines = int(args.get("lines") or 200)
        jobs = load_jobs()
        j = _find_job(jid, jobs)
        if not j:
            return {"isError": True, "content": [{"type": "text", "text": f"no such job {jid!r}"}]}
        if j["profile"] not in cfg["profiles"]:
            return {"isError": True, "content": [{"type": "text", "text": f"profile {j['profile']!r} not found"}]}
        profile = cfg["profiles"][j["profile"]]
        result = run_remote_capture(j["profile"], profile, f"tail -n {lines} {j['log']}")
        return {
            "content": [{"type": "text", "text": result["stdout"] or result["stderr"]}],
            "isError": result["exit_code"] != 0,
            "structuredContent": {"job": j, "tail": result["stdout"], "exit_code": result["exit_code"]},
        }

    if name == "kill_job":
        jid = args.get("id", "")
        sig = args.get("signal") or "TERM"
        jobs = load_jobs()
        j = _find_job(jid, jobs)
        if not j:
            return {"isError": True, "content": [{"type": "text", "text": f"no such job {jid!r}"}]}
        if j["profile"] not in cfg["profiles"]:
            return {"isError": True, "content": [{"type": "text", "text": f"profile {j['profile']!r} not found"}]}
        profile = cfg["profiles"][j["profile"]]
        result = run_remote_capture(
            j["profile"], profile,
            f"kill -{sig} {j['pid']} 2>/dev/null && echo killed || echo 'no such pid'",
        )
        jobs["jobs"] = [x for x in jobs["jobs"] if x["id"] != j["id"]]
        save_jobs(jobs)
        return {
            "content": [{"type": "text", "text": result["stdout"].strip() or result["stderr"].strip()}],
            "isError": result["exit_code"] != 0,
            "structuredContent": {"job_id": j["id"], "signal": sig, "exit_code": result["exit_code"]},
        }

    return {"isError": True, "content": [{"type": "text", "text": f"unknown tool {name!r}"}]}


def _mcp_send(obj: dict) -> None:
    try:
        sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
        sys.stdout.flush()
    except (BrokenPipeError, OSError):
        # Client closed the read end. Don't crash the server -- the stdin
        # readline loop will see EOF and exit cleanly on the next iteration.
        pass


def _mcp_response(req_id: Any, result: Any = None, error: dict | None = None) -> dict:
    msg: dict = {"jsonrpc": "2.0", "id": req_id}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    return msg


def cmd_mcp(cfg: dict) -> int:
    global _IN_MCP_MODE
    _IN_MCP_MODE = True
    # Force UTF-8 on stdio so non-ASCII payloads (Chinese profile names,
    # filenames in stderr, etc.) don't crash on Windows cp1252/cp936 stdout.
    for stream in (sys.stdin, sys.stdout):
        if hasattr(stream, "reconfigure"):
            try:
                stream.reconfigure(encoding="utf-8", errors="replace")
            except Exception:
                pass

    while True:
        try:
            line = sys.stdin.readline()
        except (KeyboardInterrupt, OSError, UnicodeDecodeError):
            return 0
        if not line:
            return 0
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as e:
            _mcp_send({"jsonrpc": "2.0", "id": None,
                       "error": {"code": -32700, "message": f"parse error: {e}"}})
            continue

        method = req.get("method")
        req_id = req.get("id")
        params = req.get("params") or {}

        if method == "initialize":
            _mcp_send(_mcp_response(req_id, {
                "protocolVersion": MCP_PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "srv", "version": VERSION},
            }))
            continue
        if method == "notifications/initialized":
            continue
        if method == "ping":
            _mcp_send(_mcp_response(req_id, {}))
            continue
        if method == "tools/list":
            _mcp_send(_mcp_response(req_id, {"tools": _mcp_tool_defs()}))
            continue
        if method == "tools/call":
            tool_name = params.get("name", "")
            tool_args = params.get("arguments") or {}
            cfg = load_config()
            try:
                result = _mcp_handle_tool(tool_name, tool_args, cfg)
            except SystemExit as e:
                result = {"isError": True, "content": [{"type": "text", "text": str(e)}]}
            except Exception as e:
                result = {"isError": True,
                          "content": [{"type": "text", "text": f"{type(e).__name__}: {e}"}]}
            _mcp_send(_mcp_response(req_id, result))
            continue
        if req_id is None:
            continue
        _mcp_send(_mcp_response(req_id, error={
            "code": -32601, "message": f"method not found: {method}"}))


# --------------------------------------------------------------------------- #
# CLI dispatch                                                                #
# --------------------------------------------------------------------------- #

def parse_global_flags(argv: list[str]) -> tuple[dict, list[str]]:
    opts = {"profile": None, "tty": False, "detach": False}
    i = 0
    while i < len(argv):
        a = argv[i]
        if a in ("-P", "--profile"):
            if i + 1 >= len(argv):
                sys.exit(f"error: {a} requires a value.")
            opts["profile"] = argv[i + 1]
            i += 2
            continue
        if a.startswith("--profile="):
            opts["profile"] = a.split("=", 1)[1]
            i += 1
            continue
        if a in ("-t", "--tty"):
            opts["tty"] = True
            i += 1
            continue
        if a in ("-d", "--detach"):
            opts["detach"] = True
            i += 1
            continue
        break
    return opts, argv[i:]


def main(argv: list[str] | None = None) -> int:
    argv = list(argv if argv is not None else sys.argv[1:])
    if not argv:
        print(__doc__)
        return 0

    opts, rest = parse_global_flags(argv)
    if not rest:
        print(__doc__)
        return 0

    sub = rest[0]
    cfg = load_config()

    if sub in ("help", "--help", "-h"):
        print(__doc__)
        return 0
    if sub in ("version", "--version"):
        print(f"srv {VERSION}")
        return 0
    if sub == "init":
        return cmd_init(cfg)
    if sub == "config":
        return cmd_config(rest[1:], cfg)
    if sub == "use":
        return cmd_use(rest[1:], cfg)
    if sub == "cd":
        path = rest[1] if len(rest) > 1 else None
        return cmd_cd(path, cfg, opts["profile"])
    if sub == "pwd":
        return cmd_pwd(cfg, opts["profile"])
    if sub == "status":
        return cmd_status(cfg, opts["profile"])
    if sub == "check":
        return cmd_check(cfg, opts["profile"])
    if sub == "push":
        return cmd_push(rest[1:], cfg, opts["profile"])
    if sub == "pull":
        return cmd_pull(rest[1:], cfg, opts["profile"])
    if sub == "sync":
        return cmd_sync(rest[1:], cfg, opts["profile"])
    if sub == "jobs":
        return cmd_jobs(cfg, opts["profile"])
    if sub == "logs":
        return cmd_logs(rest[1:], cfg, opts["profile"])
    if sub == "kill":
        return cmd_kill(rest[1:], cfg, opts["profile"])
    if sub == "sessions":
        return cmd_sessions(rest[1:])
    if sub == "completion":
        return cmd_completion(rest[1:])
    if sub == "mcp":
        return cmd_mcp(cfg)
    if sub == "_profiles":
        return cmd_list_profiles_internal(cfg)
    if sub in ("run", "exec"):
        if opts["detach"]:
            return cmd_detach(rest[1:], cfg, opts["profile"])
        return cmd_run(rest[1:], cfg, opts["profile"], opts["tty"])

    if opts["detach"]:
        return cmd_detach(rest, cfg, opts["profile"])
    return cmd_run(rest, cfg, opts["profile"], opts["tty"])


if __name__ == "__main__":
    try:
        sys.exit(main() or 0)
    except KeyboardInterrupt:
        sys.exit(130)
