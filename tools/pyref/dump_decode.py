"""Capture how the current Python runner reads a child log back as text.

This is the reference generator for the Go rewrite's log decoding contract
(0008-L 2.7 / 0007-P [로그 조회 — 여러 표기 혼재]). It drives the real
`ProcessManager.read_log()` — the decoder loop under test is the deployed one,
not a re-implementation of it — by placing an entry in the manager's table and
pointing it at a file this script writes. No process is started and no
registered command is touched.

What is captured per case is the pair the rewrite has to reproduce: the text and
how many bytes of the input it accounted for. The `encoding` field of the
response is new in the rewrite (0007-P) and has no counterpart here.

The candidate list is machine-dependent — `mbcs` is whatever the ANSI code page
is — so the code page is recorded alongside and the Go test skips itself on a
machine that would answer differently.

Output: JSON on stdout.

    python tools/pyref/dump_decode.py > internal/textdec/testdata/python_reference.json

Windows only; elsewhere the current runner tries UTF-8 alone and there is
nothing multi-byte to reproduce.
"""
from __future__ import annotations

import ctypes
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))

from spawnpoint.runner import ProcessManager, _Entry  # noqa: E402

IDENT = "proc_00000000"


def cases() -> list[tuple[str, bytes]]:
    """The inputs. Each name says what the case is there to pin down."""
    korean = "서버를 시작합니다\n포트 8100 대기 중\n"
    japanese = "サーバーを起動します\n"
    return [
        ("empty", b""),
        ("ascii", b"hello\n"),
        ("ascii crlf", b"line one\r\nline two\r\n"),
        ("ascii with nul", b"before\x00after\n"),
        ("utf-8 korean", korean.encode("utf-8")),
        ("utf-8 korean cut 1 of 3", korean.encode("utf-8")[:-1] + "다".encode("utf-8")[:1]),
        ("utf-8 korean cut 2 of 3", korean.encode("utf-8")[:-1] + "다".encode("utf-8")[:2]),
        ("utf-8 emoji cut 1 of 4", b"ok " + "\U0001F600".encode("utf-8")[:1]),
        ("utf-8 emoji cut 2 of 4", b"ok " + "\U0001F600".encode("utf-8")[:2]),
        ("utf-8 emoji cut 3 of 4", b"ok " + "\U0001F600".encode("utf-8")[:3]),
        ("cp949 korean", korean.encode("cp949")),
        ("cp949 korean cut lead", korean.encode("cp949")[:-1] + b"\xbc"),
        ("cp932 japanese", japanese.encode("cp932")),
        ("cp932 japanese cut lead", japanese.encode("cp932")[:-1] + b"\x83"),
        ("lone continuation byte", b"abc\x80"),
        ("byte that leads nothing", b"abc\xff\n"),
        ("bad continuation", b"abc\xc3\x28\n"),
        ("truncated lead that can never complete", b"abc\xe0\x28"),
        ("overlong lead", b"abc\xc0\xaf"),
        ("surrogate half", b"abc\xed\xa0\x80"),
        ("utf-8 then cp949", "정상\n".encode("utf-8") + "정상\n".encode("cp949")),
        ("long ascii", b"x" * 5000 + b"\n"),
    ]


def main() -> None:
    if sys.platform != "win32":
        raise SystemExit("windows only: the current runner tries UTF-8 alone elsewhere")

    with tempfile.TemporaryDirectory() as work:
        log_path = os.path.join(work, IDENT + ".log")
        manager = ProcessManager(work)
        # read_log() only needs the entry to resolve a log path. Registering
        # through the public route would start a process.
        manager._entries[IDENT] = _Entry("ref", "noop", None, None, log_path)

        out = []
        for name, data in cases():
            with open(log_path, "wb") as f:
                f.write(data)
            text, consumed = manager.read_log(IDENT, 0)
            out.append(
                {
                    "name": name,
                    "data_hex": data.hex(),
                    "text": text,
                    "consumed": consumed,
                    # True when every candidate failed and the bytes came back
                    # with substitutions. The rewrite consumes the same number
                    # of bytes on this path but is not held to the same number
                    # of replacement characters.
                    "replaced": "�" in text,
                }
            )

    json.dump(
        {
            "ansi_code_page": ctypes.windll.kernel32.GetACP(),
            "python": sys.version.split()[0],
            "cases": out,
        },
        sys.stdout,
        ensure_ascii=False,
        indent=2,
    )
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
