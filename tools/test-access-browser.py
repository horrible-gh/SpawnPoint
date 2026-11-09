#!/usr/bin/env python3
"""Actual server + browser UI regression. No production database is used.
The optional local bridge is only for rendering in policy-restricted environments;
CI deliberately uses native browser HTTP/cookies, never the bridge.
"""
import http.cookiejar
import json
import os
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

exe = str(Path(sys.argv[1]).resolve())
checks = 0

def check(value, message):
    global checks
    if not value:
        raise AssertionError(message)
    checks += 1

with tempfile.TemporaryDirectory(prefix='spawnpoint-browser-') as work:
    root = Path(work)
    sock = socket.socket(); sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]; sock.close()
    base = f'http://127.0.0.1:{port}'
    token = secrets.token_urlsafe(32)
    password = 'Browser!' + secrets.token_hex(16)
    env = {**os.environ, 'SPAWNPOINT_HOST':'127.0.0.1', 'SPAWNPOINT_PORT':str(port),
           'SPAWNPOINT_DB_PATH':str(root/'db'), 'SPAWNPOINT_LOG_DIR':str(root/'logs'),
           'SPAWNPOINT_SETUP_TOKEN':token, 'SPAWNPOINT_API_TOKENS':''}
    output = open(root/'server.log', 'w')
    proc = subprocess.Popen([exe], env=env, stdout=output, stderr=output)
    errors = []
    page = None
    artifacts = Path('artifacts'); artifacts.mkdir(exist_ok=True)
    try:
        for _ in range(100):
            try:
                if urllib.request.urlopen(base+'/healthz', timeout=1).status == 200:
                    break
            except Exception:
                time.sleep(.1)
        with sync_playwright() as pw:
            options = {'headless':True}
            if os.environ.get('SPAWNPOINT_CHROMIUM'):
                options['executable_path'] = os.environ['SPAWNPOINT_CHROMIUM']
            browser = pw.chromium.launch(**options)
            page = browser.new_page(viewport={'width':1440, 'height':1000})
            page.set_default_timeout(15000)
            page.on('pageerror', lambda e: errors.append(str(e)))
            if os.environ.get('SPAWNPOINT_BROWSER_BRIDGE') == '1':
                jar = http.cookiejar.CookieJar()
                opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
                def bridge(arg):
                    path, opts = arg
                    req = urllib.request.Request(base+path, data=opts.get('body','').encode() if opts.get('body') else None,
                                                 method=opts.get('method','GET'), headers=opts.get('headers',{}))
                    try:
                        response = opener.open(req, timeout=40)
                    except urllib.error.HTTPError as e:
                        response = e
                    return {'status':response.status, 'text':response.read().decode()}
                page.expose_function('httpBridge', bridge)
                page.evaluate("() => {window.fetch=async (path,opts={})=>{const r=await httpBridge([path,opts]);return {ok:r.status>=200&&r.status<300,status:r.status,json:async()=>JSON.parse(r.text)}};}")
                page.set_content(urllib.request.urlopen(base).read().decode())
            else:
                page.goto(base)
            expect(page.locator('#setupOSForm')).to_be_visible()
            check(page.locator('#setupToken').count() == 0, 'initial page no longer requires a token')
            check(page.locator('.login-card details').count() == 0, 'no long FAQ or DB-help sections underneath first-admin form')
            page.locator('[data-act="auth-help"]').click()
            expect(page.locator('#authHelpDialog')).to_be_visible()
            check(page.locator('#authHelpDialog .auth-faq-item').count() == 4, 'FAQ opens in a separate four-item dialog')
            page.locator('#authHelpDialog .auth-faq-item summary').first.click()
            expect(page.locator('#authHelpDialog .auth-faq-answer').first).to_be_visible()
            page.locator('[data-act="auth-help-close"]').click()
            expect(page.locator('#authHelpDialog')).not_to_be_visible()
            page.screenshot(path=str(artifacts/'setup-os-desktop.png'), full_page=True)
            page.locator('[data-act="setup-mode"][data-mode="local"]').click()
            expect(page.locator('.auth-command code')).to_contain_text('admin init')
            check(page.locator('.login-card details').count() == 0, 'terminal tab has no inline FAQ')
            page.screenshot(path=str(artifacts/'setup-cli-desktop.png'), full_page=True)
            page.locator('.login-card [data-act="setup-token-form"]').click()
            expect(page.locator('#setupForm')).to_be_visible()
            for name,value in {'setup_token':token,'login':'admin','name':'관리자','password':password,'repeat':password}.items():
                page.locator(f'#setupForm [name="{name}"]').fill(value)
            page.locator('#setupForm button[type=submit]').click()
            expect(page.locator('#authHelpDialog')).not_to_be_visible()
            expect(page.locator('[data-act="new-process"]')).to_be_visible()
            check(page.locator('#previewRail').count()==0, 'no role-impersonation preview rail')
            if not os.environ.get('SPAWNPOINT_BROWSER_BRIDGE'):
                cookies = page.context.cookies()
                check(any(c['name']=='sp_session' and c['httpOnly'] and c['sameSite']=='Strict' for c in cookies),'real browser HttpOnly SameSite session')
            def nav(route):
                page.locator(f'[data-nav="{route}"]').first.click()
            def save_modal():
                page.locator('#modalForm button[type=submit]').last.click()
                expect(page.locator('#modal')).not_to_be_visible()
            def api_state():
                return page.evaluate("async()=>await (await fetch('/api/access/state')).json()")
            nav('/admin/users')
            page.locator('[data-act="add-user"]').click()
            page.locator('#userName').fill('브라우저 개발자')
            page.locator('#userLogin').fill('developer')
            page.locator('#editRole').select_option('developer')
            page.locator('#userTempPassword').fill(password)
            save_modal()
            expect(page.locator('tbody')).to_contain_text('브라우저 개발자')
            check(True,'user created through form')
            uid = next(u['id'] for u in api_state()['db']['users'] if u['login']=='developer')
            nav('/admin/groups')
            page.locator('[data-act="add-user-group"]').click()
            page.locator('#groupName').fill('브라우저 팀')
            page.locator(f'[name="members"][value="{uid}"]').check()
            save_modal()
            expect(page.locator('.group-card')).to_contain_text('브라우저 팀')
            page.locator('[data-group-tab="processes"]').click()
            page.locator('[data-act="add-process-group"]').click()
            page.locator('#pgName').fill('브라우저 프로세스')
            save_modal()
            expect(page.locator('.group-card').filter(has_text='브라우저 프로세스')).to_be_visible()
            db = api_state()['db']
            gid = next(g['id'] for g in db['userGroups'] if g['name']=='브라우저 팀')
            pgid = next(g['id'] for g in db['processGroups'] if g['name']=='브라우저 프로세스')
            nav('/admin/permissions')
            page.locator(f'[data-policy-group="{pgid}"]').click()
            page.locator('[data-act="add-acl"]').click()
            page.locator('#aclSubject').select_option('g:'+gid)
            page.locator('#aclLevel').select_option('3')
            save_modal()
            page.locator(f'[data-create-grant="g:{gid}"]').check()
            page.locator('[data-act="preview-policy"]').click()
            expect(page.locator('#modal')).to_be_visible()
            save_modal()
            check(api_state()['db']['processGroups'][-1]['rules']['g:'+gid]['create'],'policy saved through server impact confirmation')
            page.screenshot(path=str(artifacts/'access-permissions-desktop.png'), full_page=True)
            nav('/processes')
            page.locator('[data-act="new-process"]').click()
            page.locator('#processName').fill('Browser Worker')
            page.locator('#processCommand').fill('echo browser-output')
            page.locator('#processGroup').select_option(pgid)
            page.locator('details.env-block summary').click()
            page.locator('[data-act="add-env"]').click()
            page.locator('[name=envKey]').fill('PUNCTUATION')
            page.locator('[name=envValue]').fill('a,b=c')
            page.screenshot(path=str(artifacts/'access-process-editor.png'), full_page=True)
            page.locator('#modalForm button[value="save"]').click()
            expect(page.locator('#modal')).not_to_be_visible()
            state = api_state(); item=next(p for p in state['db']['processes'] if p['name']=='Browser Worker'); pid=item['id']
            check(item['status']=='stopped' and item['env']['PUNCTUATION']=='a,b=c','save-only and environment punctuation preserved')
            page.locator(f'[data-process-row="{pid}"] [data-act="run"]').click()
            expect(page.locator('#liveLog')).to_contain_text('browser-output')
            # All administration views must render on real state, including empty states.
            for route in ['/admin','/admin/users','/admin/groups','/admin/permissions','/admin/security','/admin/audit','/account']:
                nav(route)
                check(page.locator('#root').inner_text().strip()!='','view renders '+route)
            nav('/admin/security')
            page.locator('[data-security-tab="guest"]').click()
            page.locator('#guestForm [name="guest"]').locator('..').click()
            page.locator(f'#guestForm [name="public:{pgid}"]').locator('..').click()
            page.locator('#guestForm button[type=submit]').click()
            expect(page.locator('#toast')).to_contain_text('공개 범위')
            page.locator('[data-act="logout"]').first.click()
            expect(page.locator('#loginForm')).to_be_visible()
            check(page.locator('.login-source-note, .login-card details').count() == 0,'login page avoids duplicated explanation blocks')
            page.locator('[data-act="auth-help"]').click()
            expect(page.locator('#authHelpDialog')).to_be_visible()
            check(page.locator('#authHelpDialog .auth-faq-item').count() == 4,'login help has its own FAQ dialog')
            page.locator('[data-act="auth-help-close"]').click()
            page.screenshot(path=str(artifacts/'access-login.png'),full_page=True)
            # OS tab is available only where the server can authenticate accounts.
            os_tab=page.locator('[data-act="login-mode"][data-mode="os"]')
            if os_tab.is_enabled():
                os_tab.click();expect(page.locator('#loginForm')).to_contain_text('OS 계정')
                page.locator('[data-act="login-mode"][data-mode="local"]').click()
            page.locator('[data-act="guest"]').click()
            check(page.locator('[data-act="new-process"]').count()==0,'guest cannot create')
            check(page.locator('[data-act="run"]').count()==0,'guest cannot run')
            check(page.locator('[data-act="go-users"]').count()==0,'guest has no admin entry')
            page.locator('[data-act="login"]').first.click()
            page.locator('#loginId').fill('developer');page.locator('#loginPassword').fill(password)
            page.locator('#loginForm button[type=submit]').click()
            expect(page.locator('[data-act="new-process"]')).to_be_visible()
            check(page.locator('[data-nav="/admin/users"]').count()==0,'ordinary user has no administration navigation')
            check(page.locator('[data-act="delete-process"]').count()==0,'developer cannot delete')
            page.locator('[data-act="new-process"]').click()
            check(page.locator('[data-act="editor-specific"]').is_disabled(),'developer cannot choose another execution account')
            page.locator('#processName').fill('Developer Worker');page.locator('#processCommand').fill('echo developer-output')
            for width,height in [(390,844),(2560,1440)]:
                page.set_viewport_size({'width':width,'height':height})
                box=page.locator('#modal').bounding_box()
                check(box is not None and box['x']>=-1 and box['x']+box['width']<=width+1,'dialog within viewport '+str(width))
                check(page.locator('#modalForm button[value="save"]').is_visible(),'save visible '+str(width))
                page.screenshot(path=str(artifacts/f'access-editor-{width}.png'),full_page=True)
            page.locator('#modalForm button[value="save"]').click()
            expect(page.locator('#modal')).not_to_be_visible()
            check(not errors,'no browser JavaScript errors: '+str(errors))
            if not os.environ.get('SPAWNPOINT_BROWSER_BRIDGE'):
                check(not page.evaluate('Object.keys(localStorage).some(k=>/token|password|session/i.test(k))'),'no stored browser credentials')
            print(f'PASS: {checks} browser checks; setup, users, groups, policy preview/apply, real process execution, guest, developer and responsive layout; bridge={os.environ.get("SPAWNPOINT_BROWSER_BRIDGE")=="1"}')
            browser.close()
    except Exception:
        if page:
            try:
                page.screenshot(path=str(artifacts/'access-browser-failure.png'),full_page=True)
                print('UI errors:',errors)
                print('Visible error:',page.locator('#modalError, #loginError').all_text_contents())
            except Exception:
                pass
        raise
    finally:
        proc.terminate()
        try:proc.wait(timeout=10)
        except subprocess.TimeoutExpired:proc.kill();proc.wait()
        output.close()