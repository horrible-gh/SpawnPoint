#!/usr/bin/env python3
"""Actual Caddy TLS termination -> HTTP SpawnPoint. Disposable data/CA only.

Never changes the machine trust store, installed services or production data.
Optional --download-caddy fetches a pinned official release and verifies its
SHA-256 against that release's checksum manifest before executing it.
"""
import argparse
import hashlib
import http.cookiejar
import io
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile

CHECKS = 0
VERSION = '2.10.2'


def check(condition, message):
    global CHECKS
    if not condition:
        raise AssertionError(message)
    CHECKS += 1


def download_caddy(directory):
    name = f'caddy_{VERSION}_windows_amd64.zip' if os.name == 'nt' else f'caddy_{VERSION}_linux_amd64.tar.gz'
    base = f'https://github.com/caddyserver/caddy/releases/download/v{VERSION}/'
    manifest = urllib.request.urlopen(base + f'caddy_{VERSION}_checksums.txt', timeout=60).read().decode()
    expected = next(line.split()[0] for line in manifest.splitlines() if line.split()[-1] == name)
    data = urllib.request.urlopen(base + name, timeout=90).read()
    check(hashlib.sha256(data).hexdigest() == expected, 'Caddy checksum mismatch')
    binary = 'caddy.exe' if os.name == 'nt' else 'caddy'
    if os.name == 'nt':
        content = zipfile.ZipFile(io.BytesIO(data)).read(binary)
    else:
        with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
            content = archive.extractfile(binary).read()
    destination = directory / binary
    destination.write_bytes(content)
    destination.chmod(0o755)
    return str(destination)


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class Client:
    def __init__(self, base, context=None):
        self.base, self.csrf = base, ''
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(self.jar),
            urllib.request.HTTPSHandler(context=context))

    def call(self, path, data=None, expected=200, headers=None):
        h = {'Content-Type': 'application/json'}
        if data is not None:
            h['Origin'] = self.base
            if self.csrf:
                h['X-CSRF-Token'] = self.csrf
        h.update(headers or {})
        body = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(self.base + path, data=body, headers=h)
        try:
            response = self.opener.open(req, timeout=40)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            payload = json.loads(response.read())
            check(response.status == expected, f'{req.method} {path}: {response.status}, expected {expected}')
        if payload.get('csrf'):
            self.csrf = payload['csrf']
        return payload


def ready(predicate, processes=()):
    deadline = time.monotonic() + 25
    while time.monotonic() < deadline:
        if any(p.poll() is not None for p in processes):
            raise AssertionError('test server exited before readiness')
        try:
            result = predicate()
            if result:
                return result
        except (OSError, ValueError, AssertionError):
            pass
        time.sleep(.1)
    raise AssertionError('test server readiness timed out')


def scenario(exe, caddy, root, method, browser=False):
    backend, tls_port, http_port = free_port(), free_port(), free_port()
    check(len({backend, tls_port, http_port}) == 3, 'unique fixture ports')
    setup_token = secrets.token_urlsafe(32)
    password = 'Proxy!' + secrets.token_hex(24)
    env = {**os.environ, 'SPAWNPOINT_HOST': '127.0.0.1', 'SPAWNPOINT_PORT': str(backend),
           'SPAWNPOINT_DB_PATH': str(root / 'test.db'), 'SPAWNPOINT_LOG_DIR': str(root / 'logs'),
           'SPAWNPOINT_SETUP_TOKEN': setup_token, 'SPAWNPOINT_API_TOKENS': ''}
    # The reported failure is tested with NO opt-in setting at all.
    env.pop('SPAWNPOINT_TRUST_LOOPBACK_PROXY', None)
    ca_env = dict(os.environ)
    for variable, subdir in [('HOME', 'home'), ('USERPROFILE', 'home'), ('APPDATA', 'appdata'),
                             ('LOCALAPPDATA', 'localappdata'), ('XDG_DATA_HOME', 'data'), ('XDG_CONFIG_HOME', 'config')]:
        directory = root / 'caddy-state' / subdir
        directory.mkdir(parents=True, exist_ok=True)
        ca_env[variable] = str(directory)
    conf = root / 'Caddyfile'
    conf.write_text(f'''{{
    admin off
    auto_https disable_redirects
    skip_install_trust
}}
https://127.0.0.1:{tls_port} {{
    bind 127.0.0.1
    tls internal
    reverse_proxy 127.0.0.1:{backend}
}}
http://127.0.0.1:{http_port} {{
    bind 127.0.0.1
    reverse_proxy 127.0.0.1:{backend}
}}
''', encoding='utf-8')
    processes = []
    with open(root / 'server.log', 'wb') as log, open(root / 'caddy.log', 'wb') as ca_log:
        try:
            processes.append(subprocess.Popen([exe], cwd=root, env=env, stdout=log, stderr=log))
            local = Client(f'http://127.0.0.1:{backend}')
            ready(lambda: local.call('/healthz').get('ok'), processes)
            processes.append(subprocess.Popen([caddy, 'run', '--config', str(conf), '--adapter', 'caddyfile'],
                                              cwd=root, env=ca_env, stdout=ca_log, stderr=ca_log))
            root_ca = ready(lambda: next((root / 'caddy-state').rglob('root.crt'), None), processes)
            # Trust only this disposable CA for this client. No OS trust edits.
            context = ssl.create_default_context(cafile=str(root_ca))
            https = Client(f'https://127.0.0.1:{tls_port}', context)
            plain = Client(f'http://127.0.0.1:{http_port}')
            me = ready(lambda: https.call('/auth/me'), processes)
            check(me['runtime']['secure'] and me['runtime']['https'], 'Caddy HTTPS recognized without configuration')
            check(me['setup_required'], 'first-admin screen available through TLS proxy')
            https.call('/processes', expected=401)
            https.call('/auth/setup-info', expected=403)
            for forged in [None, {'X-Forwarded-Proto': 'https', 'X-Forwarded-Host': f'127.0.0.1:{http_port}',
                                 'X-Forwarded-For': '127.0.0.1'}]:
                http_me = plain.call('/auth/me', headers=forged)
                check(not http_me['runtime']['secure'] and not http_me['runtime']['https'], 'HTTP frontend stays blocked even with forged forwarding headers')
                plain.call('/auth/login', {'method': 'local', 'login': 'nobody', 'password': 'unused'}, expected=403, headers=forged)
            if browser:
                from playwright.sync_api import sync_playwright, expect
                with sync_playwright() as p:
                    chromium = p.chromium.launch()
                    try:
                        page = chromium.new_page(ignore_https_errors=True, viewport={'width': 1440, 'height': 1000})
                        errors = []
                        page.on('pageerror', lambda e: errors.append(str(e)))
                        page.goto(https.base)
                        expect(page.locator('#setupOSForm')).to_be_visible()
                        if me['runtime']['os_admin_setup_available']:
                            expect(page.locator('#setupOSLogin')).to_be_enabled()
                        check('현재는 원격 HTTP' not in page.locator('.login-card').inner_text(), 'HTTPS setup screen has no false HTTP warning')
                        check(page.locator('.login-card details').count() == 0, 'compact help layout preserved')
                        Path('artifacts').mkdir(exist_ok=True)
                        page.screenshot(path='artifacts/https-caddy-setup.png', full_page=True)
                        page.locator('[data-act="setup-mode"][data-mode="local"]').click()
                        page.locator('.login-card [data-act="setup-token-form"]').click()
                        for name, value in {'setup_token': setup_token, 'login': 'proxy-admin', 'name': '관리자',
                                            'password': password, 'repeat': password}.items():
                            page.locator(f'#setupForm [name="{name}"]').fill(value)
                        page.locator('#setupForm button[type=submit]').click()
                        expect(page.locator('[data-act="new-process"]')).to_be_visible()
                        cookies = page.context.cookies()
                        check(any(c['name'] == 'sp_session' and c['secure'] and c['httpOnly'] and c['sameSite'] == 'Strict' for c in cookies), 'real browser has a secure HTTPS session')
                        check(not errors, 'no browser JavaScript errors')
                        page.screenshot(path='artifacts/https-caddy-session.png', full_page=True)
                    finally:
                        chromium.close()
                method, username = 'local', 'proxy-admin'
            elif method == 'os':
                username = os.environ['SPAWNPOINT_ADMIN_TEST_USER']
                password = os.environ['SPAWNPOINT_ADMIN_TEST_PASSWORD']
                https.call('/auth/setup-os', {'login': username, 'password': password}, expected=403,
                           headers={'Origin': 'https://attacker.invalid'})
                https.call('/auth/setup-os', {'login': username, 'password': password})
            else:
                username = 'proxy-admin'
                https.call('/auth/setup', {'login': username, 'password': password, 'setup_token': setup_token})
            me = https.call('/auth/login', {'method': method, 'login': username, 'password': password})
            check(me['user']['role'] == 'admin', 'login through Caddy returns administrator')
            check(any(c.name == 'sp_session' and c.secure and c.has_nonstandard_attr('HttpOnly') for c in https.jar), 'Secure/HttpOnly cookie issued')
            check(https.call('/auth/me')['user'] is not None, 'session survives subsequent HTTPS request')
            https.call('/auth/heartbeat', {}, expected=403, headers={'Origin': plain.base})
            https.call('/auth/heartbeat', {}, expected=403, headers={'X-CSRF-Token': 'wrong'})
            https.call('/auth/heartbeat', {})
            target = os.environ.get('SPAWNPOINT_RUNAS_TEST_USER')
            if target:
                check(https.call('/runtime')['run_as_supported'], 'run-as shares HTTPS recognition after login')
                command = 'whoami' if os.name == 'nt' else 'id -un'
                process = https.call('/processes', {'label': 'TLS-proxy-native-identity', 'cmd': command, 'start': True,
                    'run_as': {'mode': 'user', 'username': target, 'password': os.environ['SPAWNPOINT_RUNAS_TEST_PASSWORD']}})['process']
                check(process['status'] != 'failed', 'native run-as launched through HTTPS proxy')
                ready(lambda: target.lower() in https.call('/processes/' + process['id'] + '/logs').get('text', '').lower(), processes)
                check(True, 'native execution output confirms specified OS account')
            https.call('/auth/logout', {})
            check(https.call('/auth/me')['user'] is None, 'logout invalidates proxied session')
            print(f'PASS: actual Caddy HTTPS/{method} setup, login, cookies, CSRF, HTTP rejection and execution')
        finally:
            for proc in reversed(processes):
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        proc.kill(); proc.wait(timeout=10)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('executable')
    parser.add_argument('--caddy')
    parser.add_argument('--download-caddy', action='store_true')
    parser.add_argument('--browser', action='store_true')
    args = parser.parse_args()
    exe = str(Path(args.executable).resolve())
    with tempfile.TemporaryDirectory(prefix='spawnpoint-caddy-') as directory:
        root = Path(directory)
        caddy = str(Path(args.caddy).resolve()) if args.caddy else None
        if args.download_caddy:
            caddy = download_caddy(root)
        if not caddy:
            parser.error('provide --caddy or --download-caddy')
        for method in (['local'] if args.browser else ['local'] + (['os'] if os.environ.get('SPAWNPOINT_ADMIN_TEST_USER') else [])):
            work = root / method
            work.mkdir()
            scenario(exe, caddy, work, method, args.browser)
    print(f'PASS: {CHECKS} Caddy transport and authentication checks')


if __name__ == '__main__':
    main()
