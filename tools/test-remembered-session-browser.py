#!/usr/bin/env python3
"""Real browser cookie persistence against a disposable SpawnPoint database.
No bridge, real credentials, or production database is used. Time boundaries
are simulated only by changing rows in this test's temporary SQLite database.
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request

from playwright.sync_api import sync_playwright, expect

EXE = str(Path(sys.argv[1]).resolve())
checks = 0


def check(condition, message):
    global checks
    if not condition:
        raise AssertionError(message)
    checks += 1


with tempfile.TemporaryDirectory(prefix='spawnpoint-remember-test-') as work:
    root = Path(work)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    base = f'http://127.0.0.1:{port}'
    password = 'Remember!' + secrets.token_hex(20)
    setup = secrets.token_urlsafe(32)
    env = {**os.environ, 'SPAWNPOINT_HOST': '127.0.0.1', 'SPAWNPOINT_PORT': str(port),
           'SPAWNPOINT_DB_PATH': str(root/'db'), 'SPAWNPOINT_LOG_DIR': str(root/'logs'),
           'SPAWNPOINT_SETUP_TOKEN': setup, 'SPAWNPOINT_API_TOKENS': ''}
    output = open(root/'server.log', 'w')
    proc = None
    context = None

    def start():
        global proc
        proc = subprocess.Popen([EXE], env=env, stdout=output, stderr=output)
        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError('Test server exited: '+(root/'server.log').read_text())
            try:
                if urllib.request.urlopen(base+'/healthz', timeout=1).status == 200:
                    return
            except (OSError, urllib.error.URLError):
                time.sleep(.1)
        raise RuntimeError('Test server did not start')

    def stop():
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)

    def sql(statement, args=()):
        with sqlite3.connect(root/'db', timeout=10) as db:
            return db.execute(statement, args).fetchall()

    def logged_in(page):
        # DOM assertions work under the product's strict CSP; string eval does not.
        expect(page.locator('[data-act="new-process"]')).to_be_visible()
        expect(page.locator('#loginForm')).to_have_count(0)
        check(page.locator('#loginForm').count() == 0, 'login form unexpectedly visible')

    def login(page, remember):
        expect(page.locator('#loginForm')).to_be_visible()
        page.locator('[data-act="login-mode"][data-mode="local"]').click()
        page.locator('#loginId').fill('remember-admin')
        page.locator('#loginPassword').fill(password)
        page.locator('input[name="remember"]').set_checked(remember)
        page.locator('#loginForm button[type="submit"]').click()
        logged_in(page)

    def cookie():
        return next(c for c in context.cookies() if c['name'] == 'sp_session')

    try:
        start()
        body = json.dumps({'login': 'remember-admin', 'password': password,
                           'setup_token': setup}).encode()
        urllib.request.urlopen(urllib.request.Request(base+'/auth/setup', data=body,
                               headers={'Content-Type': 'application/json'}), timeout=15).close()
        with sync_playwright() as pw:
            options = {'headless': True}
            if os.environ.get('SPAWNPOINT_CHROMIUM'):
                options['executable_path'] = os.environ['SPAWNPOINT_CHROMIUM']

            def browser():
                # A real persistent browser profile; cookies are NOT injected via
                # storage_state. Closing/reopening must preserve the actual cookie.
                return pw.chromium.launch_persistent_context(str(root/'profile'), **options)

            context = browser()
            page = context.pages[0]
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            page.goto(base)
            login(page, True)
            first = cookie()
            check(first['expires'] > time.time()+29*86400, 'remember cookie is not 30 days')
            check(first['httpOnly'] and first['sameSite'] == 'Strict', 'cookie flags lost')
            deadline = sql('SELECT expires_at FROM access_session WHERE remembered=1')[0][0]
            # A return after a long idle gap must stay logged in and rotate.
            sql('UPDATE access_session SET last_seen=?,rotated_at=? WHERE remembered=1',
                (int(time.time())-7*86400, int(time.time())-13*3600))
            page.reload()
            logged_in(page)
            rotated = cookie()
            check(first['value'] != rotated['value'], 'return did not rotate token')
            check(sql('SELECT expires_at FROM access_session WHERE remembered=1')[0][0] == deadline,
                  'rotation extended absolute deadline')
            check(sql('SELECT auth_at=created_at FROM access_session WHERE remembered=1')[0][0] == 1,
                  'rotation renewed sensitive-action password verification')
            # Closing the entire browser must not require credentials on return.
            context.close()
            context = browser()
            page = context.pages[0]
            page.goto(base)
            logged_in(page)
            check(cookie()['value'] == rotated['value'], 'browser restart lost persistent cookie')
            stop()
            start()
            page.reload()
            logged_in(page)
            check(cookie()['value'] == rotated['value'], 'server restart lost session')
            # Two tabs share the newly rotated cookie and unchanged CSRF token.
            other = context.new_page()
            other.goto(base)
            logged_in(other)
            sql('UPDATE access_session SET rotated_at=? WHERE remembered=1', (int(time.time())-13*3600,))
            other.evaluate("async()=>{await request('/auth/heartbeat',{});}")
            result = page.evaluate("async()=>await request('/auth/heartbeat',{})")
            check(result['ok'], 'other tab lost CSRF/session after rotation')
            other.close()
            page.locator('[data-act="logout"]').click()
            expect(page.locator('#loginForm')).to_be_visible()
            check(not any(c['name']=='sp_session' for c in context.cookies()), 'logout retained cookie')
            check(sql('SELECT COUNT(*) FROM access_session WHERE remembered=1')[0][0] == 0,
                  'logout left remembered session in database')
            login(page, False)
            check(cookie()['expires'] == -1, 'unchecked login created persistent cookie')
            sql('UPDATE access_session SET last_seen=? WHERE remembered=0', (int(time.time())-2*3600,))
            page.reload()
            expect(page.locator('#loginForm')).to_be_visible()
            check(True, 'ordinary idle timeout enforced')
            login(page, True)
            sql('UPDATE access_session SET expires_at=? WHERE remembered=1', (int(time.time())-1,))
            page.reload()
            expect(page.locator('#loginForm')).to_be_visible()
            check(True, 'remembered absolute timeout enforced')
            check(not errors, 'browser JavaScript error: '+repr(errors))
            context.close()
            context = None
        print(f'PASS: {checks} real-browser remembered-session checks')
    finally:
        try:
            if context is not None:
                with contextlib.suppress(Exception):
                    context.close()
        finally:
            stop()
            output.close()
