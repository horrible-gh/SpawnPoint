"""Runner (Process Runner) — actual OS subprocess run/stop/restart/list + log capture.

Backend that actually performs the functions advertised by the runnerview screen (run/stop/restart/list, log tail).
Separate from `spawnpoint.service.spawn()` (instance registration) — that one records
"this instance was created" in the database; this one actually starts/kills OS subprocesses.

Per-process logs append stdout+stderr to log_dir/<id>.log.

Registration data (label/cmd/cwd/env) is persisted through an optional `store`
(`spawnpoint.storage.Registry`), so a server restart no longer wipes the command list.
Live state (pid, popen handle) is deliberately NOT restored: after a restart there is no
way to prove the recorded pid still belongs to that command, and the OS may have reused
the number. Restored entries therefore come back as `stopped` / `pid=None` and the user
resumes them with run().
"""
from __future__ import annotations

import codecs
import os
import secrets
import signal
import subprocess
import sys
import threading
from dataclasses import dataclass, field
from datetime import datetime

from .clock import KST, now_kst


@dataclass
class ProcessInfo:
    id: str
    label: str
    cmd: str
    cwd: str | None
    env: dict[str, str]
    pid: int | None
    status: str  # "running" | "exited" | "killed" | "stopped"
    exit_code: int | None
    started_at: datetime | None
    ended_at: datetime | None

    def to_public(self) -> dict:
        return {
            "id": self.id,
            "label": self.label,
            "cmd": self.cmd,
            "cwd": self.cwd,
            "env": dict(self.env),
            "pid": self.pid,
            "status": self.status,
            "exit_code": self.exit_code,
            "started_at": self.started_at.astimezone(KST).isoformat()
            if self.started_at
            else None,
            "ended_at": self.ended_at.astimezone(KST).isoformat()
            if self.ended_at
            else None,
        }


class _Entry:
    def __init__(
        self, label: str, cmd: str, cwd: str | None, env: dict | None, log_path: str
    ):
        self.label = label
        self.cmd = cmd
        self.cwd = cwd
        self.env = env or {}
        self.log_path = log_path
        self.popen: subprocess.Popen | None = None
        self.log_file = None
        self.started_at: datetime | None = None
        self.ended_at: datetime | None = None
        self.exit_code: int | None = None
        self.status: str = "stopped"
        self.stop_requested = False


_WINDOWS = sys.platform == "win32"

# --- Windows: bind children to the server process lifetime ------------------
#
# Children are spawned into their own process group, so a hard kill of the server
# (Task Manager, taskkill /F, closing the console window) leaves them running with no
# chance for shutdown() to intervene. The restarted server then lists the command as
# stopped while the old process still holds its port. A job object with
# KILL_ON_JOB_CLOSE hands that cleanup to the kernel: when the last handle to the job
# closes — i.e. when this process dies, however it dies — the children go with it.

_JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000
_JOB_OBJECT_EXTENDED_LIMIT_INFORMATION = 9

_job_lock = threading.Lock()
_job_handle = None
_job_unavailable = False


def _kernel32():
    import ctypes
    from ctypes import wintypes

    lib = ctypes.WinDLL("kernel32", use_last_error=True)
    lib.CreateJobObjectW.restype = wintypes.HANDLE
    lib.CreateJobObjectW.argtypes = [wintypes.LPVOID, wintypes.LPCWSTR]
    lib.SetInformationJobObject.restype = wintypes.BOOL
    lib.SetInformationJobObject.argtypes = [
        wintypes.HANDLE, ctypes.c_int, wintypes.LPVOID, wintypes.DWORD
    ]
    lib.AssignProcessToJobObject.restype = wintypes.BOOL
    lib.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
    lib.CloseHandle.argtypes = [wintypes.HANDLE]
    return lib


def _kill_on_close_job():
    """Return the process-wide job handle, creating it on first use (None if unavailable)."""
    global _job_handle, _job_unavailable
    with _job_lock:
        if _job_handle is not None or _job_unavailable:
            return _job_handle
        try:
            import ctypes
            from ctypes import wintypes

            class _IoCounters(ctypes.Structure):
                _fields_ = [
                    ("ReadOperationCount", ctypes.c_ulonglong),
                    ("WriteOperationCount", ctypes.c_ulonglong),
                    ("OtherOperationCount", ctypes.c_ulonglong),
                    ("ReadTransferCount", ctypes.c_ulonglong),
                    ("WriteTransferCount", ctypes.c_ulonglong),
                    ("OtherTransferCount", ctypes.c_ulonglong),
                ]

            class _BasicLimit(ctypes.Structure):
                _fields_ = [
                    ("PerProcessUserTimeLimit", wintypes.LARGE_INTEGER),
                    ("PerJobUserTimeLimit", wintypes.LARGE_INTEGER),
                    ("LimitFlags", wintypes.DWORD),
                    ("MinimumWorkingSetSize", ctypes.c_size_t),
                    ("MaximumWorkingSetSize", ctypes.c_size_t),
                    ("ActiveProcessLimit", wintypes.DWORD),
                    ("Affinity", ctypes.c_size_t),
                    ("PriorityClass", wintypes.DWORD),
                    ("SchedulingClass", wintypes.DWORD),
                ]

            class _ExtendedLimit(ctypes.Structure):
                _fields_ = [
                    ("BasicLimitInformation", _BasicLimit),
                    ("IoInfo", _IoCounters),
                    ("ProcessMemoryLimit", ctypes.c_size_t),
                    ("JobMemoryLimit", ctypes.c_size_t),
                    ("PeakProcessMemoryUsed", ctypes.c_size_t),
                    ("PeakJobMemoryUsed", ctypes.c_size_t),
                ]

            lib = _kernel32()
            handle = lib.CreateJobObjectW(None, None)
            if not handle:
                raise OSError(ctypes.get_last_error(), "CreateJobObjectW failed")
            info = _ExtendedLimit()
            info.BasicLimitInformation.LimitFlags = _JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
            if not lib.SetInformationJobObject(
                handle,
                _JOB_OBJECT_EXTENDED_LIMIT_INFORMATION,
                ctypes.byref(info),
                ctypes.sizeof(info),
            ):
                lib.CloseHandle(handle)
                raise OSError(ctypes.get_last_error(), "SetInformationJobObject failed")
            _job_handle = handle
        except (OSError, AttributeError, ValueError):
            # Job objects are best effort: without them shutdown() still cleans up on
            # every graceful exit, which is the common case.
            _job_unavailable = True
            _job_handle = None
        return _job_handle


def _bind_to_job(popen: subprocess.Popen) -> None:
    handle = _kill_on_close_job()
    if handle is None:
        return
    try:
        _kernel32().AssignProcessToJobObject(handle, int(popen._handle))
    except (OSError, AttributeError, ValueError):
        pass


class ProcessManager:
    """Track running processes and provide run/stop/restart/list operations.

    `store` is optional and duck-typed (save_runner_entry / delete_runner_entry /
    list_runner_entries — see `spawnpoint.storage.Registry`). Without it the manager
    behaves exactly as before: registrations live only in this process's memory.

    `kill_children_on_exit` decides whether children are tied to this process's
    lifetime (Windows job object) and is the default for shutdown(). Turn it off to
    let started processes outlive the server — at the cost of the restarted server no
    longer being able to stop or restart them.
    """

    def __init__(self, log_dir: str, store=None, kill_children_on_exit: bool = True):
        self._log_dir = log_dir
        os.makedirs(log_dir, exist_ok=True)
        self._lock = threading.Lock()
        self._store = store
        self._kill_children_on_exit = kill_children_on_exit
        self._entries: dict[str, _Entry] = {}
        self._restore()

    def _log_path(self, ident: str) -> str:
        return os.path.join(self._log_dir, f"{ident}.log")

    def _restore(self) -> None:
        """Load persisted registrations as stopped entries (no live process attached)."""
        if self._store is None:
            return
        for row in self._store.list_runner_entries():
            ident = row["id"]
            self._entries[ident] = _Entry(
                row["label"], row["cmd"], row["cwd"], row["env"], self._log_path(ident)
            )

    def _persist(self, ident: str, entry: _Entry) -> None:
        if self._store is None:
            return
        self._store.save_runner_entry(
            ident, entry.label, entry.cmd, entry.cwd, entry.env
        )

    def start(
        self, label: str, cmd: str, cwd: str | None = None, env: dict | None = None
    ) -> ProcessInfo:
        ident = "proc_" + secrets.token_hex(4)
        entry = _Entry(label, cmd, cwd or None, env, self._log_path(ident))
        with self._lock:
            self._entries[ident] = entry
        # Persist before spawning: a command that fails to launch is still a
        # registration the user typed and expects to find after a restart.
        self._persist(ident, entry)
        self._spawn(entry)
        return self._info(ident, entry)

    def restart(self, ident: str) -> ProcessInfo | None:
        with self._lock:
            entry = self._entries.get(ident)
        if entry is None:
            return None
        if entry.popen is not None and entry.popen.poll() is None:
            self._terminate(entry)
        self._spawn(entry, marker="--- restart ---")
        return self._info(ident, entry)

    def run(self, ident: str) -> ProcessInfo | None:
        """Resume a stopped/exited registered entry."""
        with self._lock:
            entry = self._entries.get(ident)
        if entry is None:
            return None
        self._refresh(entry)
        if entry.popen is None or entry.popen.poll() is not None:
            self._spawn(entry, marker="--- run ---")
        return self._info(ident, entry)

    def update(
        self,
        ident: str,
        label: str,
        cmd: str,
        cwd: str | None = None,
        env: dict | None = None,
    ) -> ProcessInfo | None:
        """Update entry settings. Applies on next restart if running."""
        with self._lock:
            entry = self._entries.get(ident)
            if entry is None:
                return None
            entry.label = label
            entry.cmd = cmd
            entry.cwd = cwd or None
            entry.env = dict(env or {})
        self._persist(ident, entry)
        self._refresh(entry)
        return self._info(ident, entry)

    def delete(self, ident: str) -> bool:
        """Remove entry. Also cleans up running process and logs."""
        with self._lock:
            entry = self._entries.get(ident)
        if entry is None:
            return False
        self._refresh(entry)
        if entry.popen is not None and entry.popen.poll() is None:
            entry.stop_requested = True
            self._terminate(entry)
            self._refresh(entry)
        with self._lock:
            self._entries.pop(ident, None)
        if self._store is not None:
            self._store.delete_runner_entry(ident)
        try:
            os.remove(entry.log_path)
        except FileNotFoundError:
            pass
        return True

    def stop(self, ident: str) -> ProcessInfo | None:
        with self._lock:
            entry = self._entries.get(ident)
        if entry is None:
            return None
        if entry.popen is None or entry.popen.poll() is not None:
            self._refresh(entry)
            return self._info(ident, entry)
        entry.stop_requested = True
        self._terminate(entry)
        self._refresh(entry)
        return self._info(ident, entry)

    def get(self, ident: str) -> ProcessInfo | None:
        with self._lock:
            entry = self._entries.get(ident)
        if entry is None:
            return None
        self._refresh(entry)
        return self._info(ident, entry)

    def list(self) -> list[ProcessInfo]:
        with self._lock:
            items = list(self._entries.items())
        result = []
        for ident, entry in items:
            self._refresh(entry)
            result.append(self._info(ident, entry))
        return result

    def log_path(self, ident: str) -> str | None:
        with self._lock:
            entry = self._entries.get(ident)
        return entry.log_path if entry else None

    def read_log(self, ident: str, offset: int) -> tuple[str, int] | None:
        """Return new log text after offset and next offset."""
        path = self.log_path(ident)
        if path is None:
            return None
        if not os.path.exists(path):
            return "", offset
        with open(path, "rb") as f:
            f.seek(offset)
            data = f.read()
        if not data:
            return "", offset

        # Windows commands output via both UTF-8 and system code page (cp949, etc.).
        # If the last character is incomplete, keep those bytes for the next poll
        # to avoid splitting multi-byte characters.
        encodings = ["utf-8"]
        if _WINDOWS:
            encodings.extend(["mbcs", "cp949"])
        for encoding in encodings:
            decoder = codecs.getincrementaldecoder(encoding)(errors="strict")
            try:
                text = decoder.decode(data, final=False)
            except UnicodeDecodeError:
                continue
            buffered, _ = decoder.getstate()
            consumed = len(data) - len(buffered)
            return text, offset + consumed

        return data.decode("utf-8", errors="replace"), offset + len(data)

    def shutdown(self, kill_children: bool | None = None) -> None:
        """Release runner resources on server exit. Safe to call more than once.

        Children are spawned into their own process group/session, so they outlive the
        server unless terminated here. When killing them (the default, see
        `kill_children_on_exit`) a restart starts from a clean slate: registrations come
        back as `stopped` and are resumed with run(). Otherwise the processes keep
        running, but the restarted server can no longer stop or restart them from the UI.
        """
        if kill_children is None:
            kill_children = self._kill_children_on_exit
        with self._lock:
            entries = list(self._entries.values())
        for entry in entries:
            alive = entry.popen is not None and entry.popen.poll() is None
            if alive and kill_children:
                entry.stop_requested = True
                self._terminate(entry)
                self._refresh(entry)
            if entry.log_file is not None:
                try:
                    entry.log_file.close()
                except OSError:
                    pass
                entry.log_file = None

    # --- Internal -------------------------------------------------------

    def _spawn(self, entry: _Entry, marker: str | None = None) -> None:
        entry.log_file = open(entry.log_path, "ab", buffering=0)
        if marker:
            entry.log_file.write(f"\n{marker}\n".encode("ascii"))
        full_env = {
            **os.environ,
            "PYTHONIOENCODING": "utf-8",
            "PYTHONUTF8": "1",
            **entry.env,
        }
        kwargs: dict = {
            "shell": True,
            "cwd": entry.cwd,
            "stdout": entry.log_file,
            "stderr": subprocess.STDOUT,
            "stdin": subprocess.DEVNULL,
            "env": full_env,
        }
        if _WINDOWS:
            kwargs["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
        else:
            kwargs["preexec_fn"] = os.setsid
        command = entry.cmd
        if _WINDOWS:
            command = "chcp 65001 >nul & " + command
        entry.popen = subprocess.Popen(command, **kwargs)
        if _WINDOWS and self._kill_children_on_exit:
            _bind_to_job(entry.popen)
        entry.started_at = now_kst()
        entry.ended_at = None
        entry.exit_code = None
        entry.stop_requested = False
        entry.status = "running"

    def _terminate(self, entry: _Entry, timeout: float = 5.0) -> None:
        if entry.popen is None:
            return
        pid = entry.popen.pid
        try:
            if _WINDOWS:
                subprocess.run(
                    ["taskkill", "/PID", str(pid), "/T", "/F"],
                    capture_output=True,
                    check=False,
                )
            else:
                os.killpg(os.getpgid(pid), signal.SIGTERM)
        except (ProcessLookupError, OSError):
            pass
        try:
            entry.popen.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                if _WINDOWS:
                    subprocess.run(
                        ["taskkill", "/PID", str(pid), "/T", "/F"],
                        capture_output=True,
                        check=False,
                    )
                else:
                    os.killpg(os.getpgid(pid), signal.SIGKILL)
                entry.popen.wait(timeout=timeout)
            except (ProcessLookupError, OSError, subprocess.TimeoutExpired):
                pass
        if entry.log_file is not None:
            try:
                entry.log_file.close()
            except OSError:
                pass
            entry.log_file = None

    def _refresh(self, entry: _Entry) -> None:
        if entry.popen is None or entry.status != "running":
            return
        rc = entry.popen.poll()
        if rc is None:
            return
        entry.ended_at = now_kst()
        entry.exit_code = rc
        if entry.stop_requested:
            entry.status = "stopped"
        else:
            entry.status = "exited" if rc == 0 else "killed"
        if entry.log_file is not None:
            try:
                entry.log_file.close()
            except OSError:
                pass
            entry.log_file = None

    def _info(self, ident: str, entry: _Entry) -> ProcessInfo:
        return ProcessInfo(
            id=ident,
            label=entry.label,
            cmd=entry.cmd,
            cwd=entry.cwd,
            env=dict(entry.env),
            pid=entry.popen.pid if entry.popen is not None else None,
            status=entry.status,
            exit_code=entry.exit_code,
            started_at=entry.started_at,
            ended_at=entry.ended_at,
        )
