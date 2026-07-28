"""Capture the exact lpCommandLine the current Python runner hands to CreateProcess.

This is the reference generator for the Go rewrite's command-line byte-identity
contract (0008-L 2.1 / 6.1). It drives the real `ProcessManager.start()` path —
not a re-implementation of it — and intercepts `_winapi.CreateProcess` to record
the command-line string and abort before any process is actually created. No
child is launched, so it is safe to run against the registered production
commands.

Output: JSON on stdout, written to internal/runner/testdata/python_reference.json.

    python tools/pyref/dump_command_lines.py > internal/runner/testdata/python_reference.json

Windows only; on other platforms the current runner takes the /bin/sh path and
there is nothing to reproduce byte for byte.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

FIXTURE = os.path.join(
    os.path.dirname(__file__),
    "..",
    "..",
    "internal",
    "runner",
    "testdata",
    "registered_commands.json",
)


class _Aborted(Exception):
    """Raised from the CreateProcess stand-in once the command line is recorded."""


def capture(manager, label: str, cmd: str) -> str:
    """Run the production spawn path for `cmd` and return the recorded command line."""
    recorded: list[str] = []
    real_create = subprocess._winapi.CreateProcess

    def spy(application_name, command_line, *rest):
        recorded.append(command_line)
        raise _Aborted()

    subprocess._winapi.CreateProcess = spy
    try:
        manager.start(label, cmd)
    except _Aborted:
        pass
    finally:
        subprocess._winapi.CreateProcess = real_create

    if not recorded:
        raise RuntimeError(f"CreateProcess was never reached for {cmd!r}")
    return recorded[0]


def main() -> int:
    if sys.platform != "win32":
        print("windows only", file=sys.stderr)
        return 2

    from spawnpoint.runner import ProcessManager

    with open(FIXTURE, encoding="utf-8") as handle:
        entries = json.load(handle)["entries"]

    with tempfile.TemporaryDirectory(prefix="pyref-") as log_dir:
        manager = ProcessManager(log_dir=log_dir, kill_children_on_exit=False)
        result = {
            "_generator": "tools/pyref/dump_command_lines.py",
            "_python": sys.version.split()[0],
            "comspec": os.environ.get("ComSpec", ""),
            "command_lines": {
                entry["id"]: capture(manager, entry["id"], entry["cmd"])
                for entry in entries
            },
        }
        manager.shutdown(kill_children=False)

    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
