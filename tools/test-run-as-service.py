#!/usr/bin/env python3
"""CI-only: test the shipped installer and execution under its service identity.
Never run against an existing installation. GitHub-hosted disposable runners only.
"""
import ctypes
import http.cookiejar
import secrets
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.request

if os.environ.get('GITHUB_ACTIONS') != 'true':
    raise SystemExit('Service lifecycle test is restricted to disposable CI runners.')
WIN = os.name == 'nt'
USER = os.environ['SPAWNPOINT_RUNAS_TEST_USER']
PASSWORD = os.environ['SPAWNPOINT_RUNAS_TEST_PASSWORD']
EXE = str(Path(sys.argv[1]).resolve())
URL = 'http://127.0.0.1:7527'
NAME = 'SpawnPoint' if WIN else 'spawnpoint'
COOKIE_JAR=http.cookiejar.CookieJar()
OPENER=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(COOKIE_JAR))
CSRF=''
ADMIN_PASSWORD='Service!'+secrets.token_hex(20)


def run(*args):
    return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True).stdout


def api(path, data=None, method=None):
    req = urllib.request.Request(URL + path, data=None if data is None else json.dumps(data).encode(),
                                 method=method, headers={'Content-Type': 'application/json','X-CSRF-Token':CSRF})
    with OPENER.open(req, timeout=45) as response:
        return json.load(response)


def until(fn, seconds=30):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        try:
            result = fn()
            if result:
                return result
        except (OSError, ValueError):
            pass
        time.sleep(.2)
    raise AssertionError('Service test condition timed out')


def control(action):
    if WIN:
        run('sc.exe', action, NAME)
        if action == 'stop':
            until(lambda: 'STOPPED' in run('sc.exe', 'query', NAME))
    else:
        run('sudo', 'systemctl', action, NAME)
    if action == 'start':
        until(lambda: api('/healthz').get('ok'))


def process_watch(pid):
    if not WIN:
        import select
        descriptor = os.pidfd_open(pid)
        return lambda: bool(select.select([descriptor], [], [], 0)[0]), lambda: os.close(descriptor)
    kernel = ctypes.WinDLL('kernel32', use_last_error=True)
    kernel.OpenProcess.restype = ctypes.c_void_p
    kernel.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_ulong]
    kernel.CloseHandle.argtypes = [ctypes.c_void_p]
    handle = kernel.OpenProcess(0x100000, False, pid)
    if not handle:
        raise ctypes.WinError(ctypes.get_last_error())
    return lambda: kernel.WaitForSingleObject(handle, 0) == 0, lambda: kernel.CloseHandle(handle)


if WIN:
    if subprocess.run(['sc.exe', 'query', NAME], capture_output=True).returncode == 0:
        raise SystemExit('Existing service found; refusing to touch it.')
    data_dir = Path(os.environ['ProgramData']) / NAME
    install_dir = Path(os.environ['ProgramFiles']) / NAME
    if data_dir.exists() or install_dir.exists():
        raise SystemExit('Existing installation found; refusing to touch it.')
else:
    if Path('/etc/systemd/system/spawnpoint.service').exists():
        raise SystemExit('Existing service found; refusing to touch it.')
    data_dir = Path('/var/lib/spawnpoint')
    if data_dir.exists() or Path('/opt/spawnpoint').exists():
        raise SystemExit('Existing installation found; refusing to touch it.')

installed = False
handles = []
id = None
try:
    if WIN:
        run('powershell', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File',
            'tools/install-service.ps1', '-SourceExe', EXE)
    else:
        run('sudo', 'sh', 'tools/install-service.sh', '--user', os.environ['USER'], '--source', EXE)
    installed = True
    control('start')
    setup=(data_dir/'spawnpoint.db.setup-token').read_text().strip()
    cli_prefix=[] if WIN else ['sudo','-n','--']
    status=subprocess.check_output([EXE,'setup','status','--url',URL],text=True,encoding='utf-8')
    assert str(data_dir/'spawnpoint.db') in status and setup not in status
    shown=subprocess.check_output(cli_prefix+[EXE,'setup','token','--url',URL],text=True,encoding='utf-8').strip()
    assert shown == setup, 'CLI discovers protected service token without a guessed DB path'
    os_admin=os.environ.get('SPAWNPOINT_ADMIN_TEST_USER')
    os_password=os.environ.get('SPAWNPOINT_ADMIN_TEST_PASSWORD')
    if os_admin and os_password:
        result=api('/auth/setup-os',{'login':os_admin,'password':os_password})
        assert result['user']['auth']=='os' and result['user']['role']=='admin', 'OS bootstrap under real service identity'
        CSRF=result['csrf']
        assert not (data_dir/'spawnpoint.db.setup-token').exists()
        state=api('/api/access/state')
        api('/api/access/admin',{'action':'user.upsert','revision':state['revision'],'user':{'login':'service-admin','name':'Service administrator','auth':'local','password':ADMIN_PASSWORD,'role':'admin','status':'active','groups':[]}})
        result=api('/auth/login',{'method':'local','login':'service-admin','password':ADMIN_PASSWORD})
    else:
        result=api('/auth/setup',{'login':'service-admin','name':'Service administrator','password':ADMIN_PASSWORD,'setup_token':setup})
    CSRF=result['csrf']
    # Provision and authenticate the OS account while the server actually runs
    # as LocalService/systemd, without changing the process execution identity.
    state=api('/api/access/state')
    # The GUI explicitly documents .\username for Windows local accounts.
    # Exercise it while the API really runs as LocalService, not runneradmin.
    linked_identity = '.\\' + USER if WIN else USER
    api('/api/access/admin',{'action':'user.upsert','revision':state['revision'],'user':{'name':'Native account','auth':'os','external':linked_identity,'role':'viewer','status':'active','groups':[]}})
    native=api('/auth/login',{'method':'os','login':linked_identity,'password':PASSWORD})
    assert native['user']['auth']=='os' and native['user']['external']==linked_identity and native['user']['id'], 'OS link without a separate login authenticates through service'
    result=api('/auth/login',{'method':'local','login':'service-admin','password':ADMIN_PASSWORD})
    CSRF=result['csrf']
    runtime = api('/runtime')
    assert runtime['run_as_supported'], runtime
    if WIN:
        assert runtime['service_account'].lower().endswith('local service'), runtime
        command = 'whoami & echo PROFILE=%USERPROFILE% & powershell -NoProfile -NonInteractive -Command "Write-Output (\'CHILD_PID=\' + $PID); Start-Sleep -Seconds 90"'
    else:
        assert runtime['service_account'] == os.environ['USER'], runtime
        command = 'id -un; printf "PROFILE=%s\\n" "$HOME"; sleep 90 & echo CHILD_PID=$!; wait'
    body = {'cmd': command, 'label': 'service-account-integration', 'auto_start': True,'group_id':'pg-default',
            'run_as': {'mode': 'user', 'username': USER, 'password': PASSWORD}}
    out = api('/processes', body)
    assert out['persisted'] and out['process']['status'] == 'running', out
    id = out['process']['id']

    def output():
        text = api('/processes/' + id + '/logs').get('text', '')
        return text if 'CHILD_PID=' in text else None

    text = until(output)
    first_generation = api('/processes/' + id + '/logs').get('generation')
    assert first_generation, 'first service run must have a console generation'
    assert USER.lower() in text.lower() and 'PROFILE=' in text, text
    assert PASSWORD not in text
    pid = int(re.findall(r'CHILD_PID=(\d+)', text)[-1])
    gone, close = process_watch(pid)
    handles.append(close)
    assert api('/processes/' + id + '/stop', {}, 'POST')['process']['status'] == 'stopped'
    until(gone)
    control('stop')
    control('start')
    until(lambda: api('/processes/' + id)['process']['status'] == 'running')
    def new_run_output():
        result = api('/processes/' + id + '/logs')
        return result if result.get('generation') != first_generation and 'CHILD_PID=' in result.get('text', '') else None
    second = until(new_run_output)
    text = second['text']
    assert len(re.findall(r'CHILD_PID=(\d+)', text)) == 1, 'console should show only the current auto-start run'
    assert USER.lower() in text.lower() and 'PROFILE=' in text, text
    log_dir = data_dir / 'logs' if WIN else Path('/var/log/spawnpoint')
    historical = (log_dir / 'service-account-integration.log').read_text(errors='replace')
    assert len(re.findall(r'CHILD_PID=(\d+)', historical)) >= 2, 'previous service run must remain in on-disk logs'
    pid = int(re.findall(r'CHILD_PID=(\d+)', text)[-1])
    gone, close = process_watch(pid)
    handles.append(close)
    control('stop')
    until(gone)
    assert ADMIN_PASSWORD.encode() not in (data_dir / 'spawnpoint.db').read_bytes()
    assert PASSWORD.encode() not in (data_dir / 'spawnpoint.db').read_bytes()
    print('PASS: service OS-admin bootstrap, CLI path/token discovery, access gateway local/OS login, protected setup token, shipped service installer, real service identity, authenticated target user, descendant termination, encrypted restart/auto-start and service shutdown')
except Exception:
    # Preserve evidence before cleanup; never print the submitted credentials.
    if installed:
        try:
            print('SERVICE SNAPSHOT:', json.dumps(api('/processes')).replace(PASSWORD, '<redacted>'), flush=True)
            if id:
                print('COMMAND OUTPUT:', json.dumps(api('/processes/' + id + '/logs')).replace(PASSWORD, '<redacted>'), flush=True)
            logs = data_dir / 'logs' if WIN else Path('/var/log/spawnpoint')
            for f in logs.glob('spawnpoint.log*'):
                print('OPERATIONS LOG:', f.read_text(errors='replace')[-12000:].replace(PASSWORD, '<redacted>'), flush=True)
        except Exception as diagnostic_error:
            print('Diagnostic unavailable:', str(diagnostic_error).replace(PASSWORD, '<redacted>'), flush=True)
    raise
finally:
    for close in handles:
        close()
    if installed:
        try:
            control('stop')
        except (subprocess.CalledProcessError, AssertionError):
            pass
        if WIN:
            run('sc.exe', 'delete', NAME)
            shutil.rmtree(data_dir, ignore_errors=True)
            shutil.rmtree(install_dir, ignore_errors=True)
        else:
            run('sudo', 'systemctl', 'disable', NAME)
            run('sudo', 'rm', '/etc/systemd/system/spawnpoint.service')
            run('sudo', 'systemctl', 'daemon-reload')
            run('sudo', 'rm', '-rf', '/var/lib/spawnpoint', '/var/log/spawnpoint', '/opt/spawnpoint')
