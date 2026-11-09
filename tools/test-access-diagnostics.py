#!/usr/bin/env python3
"""Run an integration script with narrowly redacted failure diagnostics.

Never dump frame locals, submitted credentials, commands, environment variables,
or the full process API response. Preserve the original exception and exit code.
"""
import os
import runpy
import sys


def diagnose(error):
    frames = []
    tb = error.__traceback__
    while tb is not None:
        frames.append(tb.tb_frame)
        tb = tb.tb_next
    secrets = [value for key, value in os.environ.items()
               if any(word in key.upper() for word in ('PASSWORD', 'TOKEN', 'SECRET')) and value]
    for frame in frames:
        for key in ('password', 'new_password', 'os_password', 'setup', 'token'):
            value = frame.f_locals.get(key)
            if isinstance(value, str) and value:
                secrets.append(value)
    for frame in frames:
        execution = frame.f_locals.get('execution')
        if not isinstance(execution, dict):
            continue
        process = execution.get('process')
        if not isinstance(process, dict):
            continue
        message = str(process.get('error', ''))[:2048]
        for value in secrets:
            message = message.replace(value, '<redacted>')
        print('EXECUTION DIAGNOSTIC: status=%s error=%s' %
              (process.get('status', 'unknown'), message), flush=True)
        break


if __name__ == '__main__':
    if len(sys.argv) < 2:
        raise SystemExit('usage: test-access-diagnostics.py SCRIPT [ARG ...]')
    script = sys.argv[1]
    sys.argv = sys.argv[1:]
    try:
        runpy.run_path(script, run_name='__main__')
    except Exception as error:
        diagnose(error)
        raise
