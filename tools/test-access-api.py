#!/usr/bin/env python3
"""Runs the actual server against a disposable database. Never uses production data."""
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

exe = str(Path(sys.argv[1]).resolve())
checks = 0

def check(value, label):
    global checks
    if not value:
        raise AssertionError(label)
    checks += 1

class Client:
    def __init__(self, base):
        self.base = base
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ''
        self.bearer = ''
    def call(self, path, data=None, method=None, expected=200, headers=None, csrf=True):
        h = {'Content-Type': 'application/json'}
        if self.csrf and csrf:
            h['X-CSRF-Token'] = self.csrf
        if self.bearer:
            h['Authorization'] = 'Bearer ' + self.bearer
        if headers:
            h.update(headers)
        raw = json.dumps(data).encode() if data is not None else None
        req = urllib.request.Request(self.base + path, data=raw, method=method or ('POST' if data is not None else 'GET'), headers=h)
        try:
            response = self.opener.open(req, timeout=40)
        except urllib.error.HTTPError as e:
            response = e
        body = response.read()
        try:
            payload = json.loads(body)
        except (ValueError, UnicodeDecodeError):
            payload = {'raw': body.decode(errors='replace')}
        check(response.status == expected, f'{method or req.method} {path}: got {response.status}, expected {expected}; {payload.get("error", "")}')
        if 'csrf' in payload:
            self.csrf = payload['csrf']
        return payload
    def state(self):
        return self.call('/api/access/state')
    def admin(self, action, **kw):
        current = self.state()
        return self.call('/api/access/admin', {'action': action, 'revision': current['revision'], **kw})
    def login(self, login, password, method='local'):
        return self.call('/auth/login', {'method': method, 'login': login, 'password': password})

def wait_for(fn, seconds=15):
    last = None
    for _ in range(int(seconds * 10)):
        try:
            value = fn()
            if value:
                return value
        except Exception as e:
            last = e
        time.sleep(.1)
    raise AssertionError(f'condition timed out: {type(last).__name__ if last else "no result"}')

with tempfile.TemporaryDirectory(prefix='spawnpoint-access-') as work:
    root = Path(work)
    sock = socket.socket(); sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]; sock.close()
    base = f'http://127.0.0.1:{port}'
    password = 'Access!' + secrets.token_hex(20)
    setup = secrets.token_urlsafe(32)
    env = {**os.environ, 'SPAWNPOINT_HOST': '127.0.0.1', 'SPAWNPOINT_PORT': str(port),
           'SPAWNPOINT_DB_PATH': str(root / 'test.db'), 'SPAWNPOINT_LOG_DIR': str(root / 'logs'),
           'SPAWNPOINT_SETUP_TOKEN': setup, 'SPAWNPOINT_API_TOKENS': 'legacy-token-must-not-bypass'}
    output = open(root / 'server.log', 'wb')
    proc = subprocess.Popen([exe], env=env, stdout=output, stderr=output)
    try:
        admin = Client(base); anon = Client(base)
        wait_for(lambda: anon.call('/healthz')['ok'])
        check(anon.call('/auth/me')['setup_required'], 'initial setup is required')
        anon.call('/processes', expected=401)
        anon.call('/processes', expected=401, headers={'Authorization': 'Bearer legacy-token-must-not-bypass'})
        anon.call('/auth/setup', {'login': 'admin', 'name': '관리자', 'password': password, 'setup_token': 'wrong'}, expected=403)
        data = admin.call('/auth/setup', {'login': 'admin', 'name': '관리자', 'password': password, 'setup_token': setup})
        check(data['user']['role'] == 'admin' and data['csrf'], 'setup creates an administrator session')
        admin_id = data['user']['id']
        anon.call('/auth/setup', {'login': 'other', 'password': password, 'setup_token': setup}, expected=403)
        check(not anon.call('/auth/me')['setup_required'], 'setup cannot be repeated')
        check(all(c.has_nonstandard_attr('HttpOnly') for c in admin.jar), 'HttpOnly session cookie')
        check(all(c._rest.get('SameSite') == 'Strict' for c in admin.jar), 'SameSite Strict cookie')
        html = admin.call('/')['raw']
        check('const seed=' not in html and '18000+' not in html, 'production screen has no demo data or fake execution')
        admin.call('/processes', {'cmd': 'echo bad', 'start': False}, expected=403, csrf=False)
        admin.call('/processes', {'cmd': 'echo bad', 'start': False}, expected=403, headers={'Origin': 'https://evil.invalid'})
        anon.call('/auth/login', {'method': 'local', 'login': 'admin', 'password': password}, expected=403, headers={'Host': 'untrusted.invalid'})
        anon.call('/auth/login', {'method': 'local', 'login': 'admin', 'password': password}, expected=403, headers={'Host': 'untrusted.invalid', 'X-Forwarded-Proto': 'https', 'X-Remote-User': 'admin'})
        users = {}
        for role in ['viewer', 'reference', 'developer', 'manager']:
            admin.admin('user.upsert', user={'login': role, 'name': role, 'role': role, 'auth': 'local', 'status': 'active', 'groups': [], 'password': password})
            users[role] = next(u for u in admin.state()['db']['users'] if u['login'] == role)['id']
        admin.admin('user-group.upsert', name='team', desc='test group', members=list(users.values()))
        ug = next(g['id'] for g in admin.state()['db']['userGroups'] if g['name'] == 'team')
        admin.admin('process-group.upsert', name='allowed', desc='test processes', guest=False)
        pg = next(g['id'] for g in admin.state()['db']['processGroups'] if g['name'] == 'allowed')
        admin.admin('process-group.upsert', name='hidden', desc='private', guest=False)
        hidden = next(g['id'] for g in admin.state()['db']['processGroups'] if g['name'] == 'hidden')
        admin.admin('policy', target='group', id=pg, rules={'g:' + ug: {'level': 3, 'create': True}})
        clients = {}
        for role in users:
            c = Client(base); c.login(role, password); clients[role] = c
        viewer, ref, dev, manager = clients['viewer'], clients['reference'], clients['developer'], clients['manager']
        cmd = 'echo access-test & ping -n 40 127.0.0.1 >nul' if os.name == 'nt' else 'printf access-test; sleep 40'
        public = admin.call('/processes', {'cmd': cmd, 'label': 'allowed-process', 'group_id': pg, 'start': False, 'env': {'SECRET_TEST': 'hidden-value'}})['process']['id']
        private = admin.call('/processes', {'cmd': 'echo private', 'label': 'private-process', 'group_id': hidden, 'start': False})['process']['id']
        visible = viewer.call('/processes')['processes']
        check(len(visible) == 1 and visible[0]['id'] == public, 'list authorization filters hidden processes')
        check('env' not in visible[0], 'non-editor does not receive environment secrets')
        for c in clients.values():
            c.call('/processes/' + private, expected=404)
            c.call('/processes/' + private + '/run', {}, expected=404)
            c.call('/processes/' + private + '/logs', expected=404)
            c.call('/api/access/admin', {'action': 'settings', 'revision': 1}, expected=403)
        viewer.call('/processes/' + public + '/run', {}, expected=403)
        viewer.call('/processes/' + public, {'cmd': 'echo mutate'}, 'PUT', expected=403)
        ref.call('/processes', {'cmd': 'echo created', 'start': False, 'group_id': pg}, expected=403)
        dev.call('/processes', {'cmd': 'echo created', 'start': False, 'group_id': hidden}, expected=403)
        created = dev.call('/processes', {'cmd': 'echo developer', 'start': False, 'group_id': pg})['process']['id']
        dev.call('/processes/' + created, {}, 'DELETE', expected=403)
        ref.call('/processes/' + public, {'cmd': cmd, 'label': 'edited', 'env': {}, 'group_id': pg}, 'PUT')
        ref.call('/processes/' + public, {'cmd': cmd, 'run_as': {'mode': 'service'}}, 'PUT', expected=403)
        admin.admin('process.meta', id=public, protected=True)
        ref.call('/processes/' + public, {'cmd': 'echo changed'}, 'PUT', expected=403)
        ref.call('/processes/' + public + '/run', {})
        wait_for(lambda: 'access-test' in ref.call('/processes/' + public + '/logs').get('text', ''))
        ref.call('/processes/' + public + '/stop', {})
        previous = ref.call('/processes/' + public + '/logs')
        check(previous.get('generation'), 'initial run has a console generation')
        # Run after stop must discard earlier output in the console, but
        # retain the earlier log lines on disk for diagnostics.
        ref.call('/processes/' + public + '/run', {})
        second = wait_for(lambda: (data if data.get('generation') != previous['generation'] and 'access-test' in data.get('text', '') else None)
                          if (data := ref.call('/processes/' + public + '/logs')) else None)
        check(second['text'].count('access-test') == 1, 'Run shows only the new invocation')
        from_start = ref.call('/processes/' + public + '/logs?offset=0')
        check(from_start['reset'] and from_start['generation'] == second['generation'],
              'Run invalidates an old console byte offset')
        check(from_start['text'].count('access-test') == 1, 'Run cursor cannot re-read previous output')
        # Restart while running must also clear the console. An on-disk
        # marker is intentionally not visible to the UI.
        ref.call('/processes/' + public + '/restart', {})
        third = wait_for(lambda: (data if data.get('generation') != second['generation'] and 'access-test' in data.get('text', '') else None)
                         if (data := ref.call('/processes/' + public + '/logs')) else None)
        check(third['text'].count('access-test') == 1 and '--- restart ' not in third['text'],
              'Restart shows only current output and no historical marker')
        original_file = (root / 'logs' / 'edited.log').read_text(errors='replace')
        check(original_file.count('access-test') >= 3 and '--- restart ' in original_file,
              'On-disk diagnostic history is not destroyed')
        ref.call('/processes/' + public + '/stop', {})
        admin.admin('process.meta', id=public, protected=False)
        # A process override denies only that process; a role never grows above its cap.
        admin.admin('policy', target='process', id=public, rules={'u:' + users['reference']: {'level': 0, 'create': False}})
        ref.call('/processes/' + public, expected=404)
        admin.admin('policy', target='process', id=public, rules={})
        ref.call('/processes/' + public)
        check(not viewer.call('/processes/' + public)['process']['capabilities']['run'], 'viewer role remains a ceiling')
        before = admin.state()
        preview = admin.call('/api/access/preview', {'action': 'bulk', 'revision': before['revision'], 'ids': [public], 'destination': hidden, 'clear': False})
        check(preview['impact']['lost'] > 0, 'server-calculated batch impact')
        admin.call('/api/access/admin', {'action': 'bulk', 'revision': before['revision'], 'ids': [public], 'destination': hidden, 'clear': False})
        ref.call('/processes/' + public, expected=404)
        admin.call('/api/access/admin', {'action': 'policy', 'revision': before['revision'], 'target': 'group', 'id': pg, 'rules': {}}, expected=409)
        admin.admin('bulk', ids=[public], destination=pg, clear=True)
        ref.call('/processes/' + public)
        # Manager lifecycle permissions are independent and limited to granted groups.
        manager.call('/processes', {'cmd':'echo outside','start':False,'group_id':hidden}, expected=403)
        check(not manager.call('/processes/'+public)['process']['capabilities']['remove'], 'legacy group grant does not grant deletion')
        manager.call('/processes/'+public, {}, 'DELETE', expected=403)
        manager_worker=manager.call('/processes', {'cmd':cmd,'label':'manager-worker','start':False,'group_id':pg})['process']['id']
        for action, payload in [('policy',{'target':'group','id':pg,'rules':{}}), ('user.upsert',{'user':{'role':'admin'}}), ('process.meta',{'id':manager_worker,'protected':False}), ('bulk',{'ids':[manager_worker],'destination':hidden})]:
            manager.call('/api/access/admin', {'action':action,'revision':admin.state()['revision'],**payload}, expected=403)
        manager.call('/processes/'+manager_worker, {'cmd':'echo move','group_id':hidden,'revision':admin.state()['revision']}, 'PUT', expected=403)
        manager.call('/processes', {'cmd':'echo privilege','start':False,'group_id':pg,'run_as':{'mode':'service'}}, expected=403)
        allowed_rules={'g:'+ug:{'level':3,'create':True,'remove':True}}
        preview=admin.call('/api/access/preview',{'action':'policy','target':'group','id':pg,'revision':admin.state()['revision'],'rules':allowed_rules})
        check(any('삭제' in row['gained'] for row in preview['impact']['rows']), 'delete grants appear in policy impact')
        admin.admin('policy',target='group',id=pg,rules=allowed_rules)
        check(manager.call('/processes/'+manager_worker)['process']['capabilities']['remove'], 'manager deletion requires group opt-in')
        for c in [viewer, ref, dev]:
            check(not c.call('/processes/'+manager_worker)['process']['capabilities']['remove'], 'lower role cannot gain deletion from group')
            c.call('/processes/'+manager_worker, {}, 'DELETE', expected=403)
        manager.call('/processes/'+manager_worker, {}, 'DELETE', expected=403, csrf=False)
        manager.call('/processes/'+manager_worker, {}, 'DELETE', expected=403, headers={'Origin':'https://evil.invalid'})
        admin.admin('policy',target='process',id=manager_worker,rules={'u:'+users['manager']:{'level':3,'remove':False}})
        manager.call('/processes/'+manager_worker, {}, 'DELETE', expected=403)
        admin.admin('policy',target='process',id=manager_worker,rules={})
        admin.admin('process.meta',id=manager_worker,protected=True)
        manager.call('/processes/'+manager_worker, {}, 'DELETE', expected=403)
        manager.call('/processes/'+manager_worker, {'cmd':cmd,'protected':False}, 'PUT', expected=403)
        admin.admin('process.meta',id=manager_worker,protected=False)
        admin.admin('bulk',ids=[manager_worker],destination=hidden,clear=False)
        manager.call('/processes/'+manager_worker, {}, 'DELETE', expected=404)
        admin.admin('bulk',ids=[manager_worker],destination=pg,clear=False)
        expiry_manager=time.strftime('%Y-%m-%d',time.gmtime(time.time()+86400))
        manager_token=admin.admin('token.create',name='manager integration',owner=users['manager'],scope=pg,level=3,create=True,expiry=expiry_manager)['secret']
        manager_api=Client(base);manager_api.bearer=manager_token
        manager_api.call('/processes/'+manager_worker, {}, 'DELETE', expected=403)
        manager_created=manager_api.call('/processes',{'cmd':'echo scoped','start':False,'group_id':pg})['process']['id']
        manager.call('/processes/'+manager_created, {}, 'DELETE')
        manager.call('/processes/'+manager_worker+'/run',{})
        wait_for(lambda:'access-test' in manager.call('/processes/'+manager_worker+'/logs').get('text',''))
        manager.call('/processes/'+manager_worker, {}, 'DELETE')
        admin.call('/processes/'+manager_worker,expected=404)
        check(any(a['action']=='프로세스 삭제' and a['actor']=='manager' for a in admin.state()['db']['audit']), 'manager deletion is audited')
        # Removing a grant changes authorization immediately, without needing logout.
        admin.admin('policy',target='group',id=pg,rules={'g:'+ug:{'level':3,'create':True}})
        manager.call('/processes/'+public,{},'DELETE',expected=403)
        manager_input=next(u for u in admin.state()['db']['users'] if u['id']==users['manager'])
        manager_input={k:manager_input[k] for k in ['id','login','name','role','auth','status','external','groups']}
        admin.admin('user.upsert',user={**manager_input,'role':'developer'})
        manager.call('/processes',expected=401)
        manager_api.call('/processes',expected=401)
        admin.admin('user.upsert',user=manager_input)
        manager.login('manager',password)
        # A single process may move directly from the editor with a reviewed
        # revision; all policy and ownership boundaries are still server-side.
        moved = admin.call('/processes', {'cmd': 'echo before-move', 'label': 'editor-move', 'group_id': pg, 'start': False, 'env': {'KEEP': 'a,b=c'}, 'auto_start': False})['process']['id']
        overrides = {'u:' + users['viewer']: {'level': 1, 'create': False}}
        admin.admin('policy', target='process', id=moved, rules=overrides)
        admin.admin('process.meta', id=moved, protected=False, public=False)
        move = {'cmd': 'echo after-move', 'label': 'editor-moved', 'group_id': hidden, 'env': {'KEEP': 'a,b=c'}, 'auto_start': False}
        rev = admin.state()['revision']
        for c in [viewer, ref, dev, manager]:
            c.call('/processes/' + moved, {**move, 'revision': rev}, 'PUT', expected=403)
        admin.call('/processes/' + moved, move, 'PUT', expected=409)
        admin.call('/processes/' + moved, {**move, 'revision': rev-1}, 'PUT', expected=409)
        admin.call('/processes/' + moved, {**move, 'revision': rev, 'group_id': 'missing'}, 'PUT', expected=400)
        check(admin.call('/processes/' + moved)['process']['cmd']=='echo before-move', 'invalid moves never update the command')
        impact = admin.call('/api/access/preview', {'action':'bulk', 'revision':rev, 'ids':[moved], 'destination':hidden, 'clear':False})
        check(impact['impact']['lost']>0 and admin.call('/processes/' + moved)['process']['group_id']==pg, 'editor group preview is read-only')
        admin.admin('process-group.upsert', name='concurrent-policy-change', guest=False)
        admin.call('/processes/' + moved, {**move, 'revision': impact['revision']}, 'PUT', expected=409)
        check(admin.call('/processes/' + moved)['process']['cmd']=='echo before-move', 'stale preview rejected before command mutation')
        rev = admin.state()['revision']
        impact = admin.call('/api/access/preview', {'action':'bulk', 'revision':rev, 'ids':[moved], 'destination':hidden, 'clear':False})
        result = admin.call('/processes/' + moved, {**move, 'revision': impact['revision']}, 'PUT')
        check(result['persisted'] and result['process']['group_id']==hidden and result['process']['status']=='stopped', 'editor saves target group without starting')
        row = next(p for p in admin.state()['db']['processes'] if p['id']==moved)
        check(row['overrides']==overrides and row['guest'] is False and row['env']=={'KEEP':'a,b=c'}, 'editor move preserves exceptions, public override and environment')
        ref.call('/processes/' + moved, expected=404)
        viewer.call('/processes/' + moved)  # Explicit exception survives the move.
        check(any(a['action']=='프로세스 그룹 이동' and a['object']==moved for a in admin.state()['db']['audit']), 'editor moves have an audit record')
        # Even a token owned by an administrator cannot change process groups.
        expiry_move = time.strftime('%Y-%m-%d', time.gmtime(time.time()+86400))
        token_move = admin.admin('token.create', name='move-denial', owner=admin_id, scope=hidden, level=3, create=True, expiry=expiry_move)['secret']
        token_client = Client(base); token_client.bearer=token_move
        token_client.call('/processes/' + moved, {**move, 'group_id':pg, 'revision':admin.state()['revision']}, 'PUT', expected=403)
        # Anonymous status-only access is opt-in and never execution authority.
        admin.admin('guest', guest=True, guest_logs=False, public_groups=[pg])
        guest_rows = anon.call('/processes')['processes']
        check(all(set(row) <= {'id', 'label', 'status', 'group_id', 'capabilities'} for row in guest_rows), 'guest status redaction')
        check(all(private != row['id'] for row in guest_rows), 'guest cannot see private process')
        anon.call('/processes/' + public + '/logs', expected=403)
        anon.call('/processes/' + public + '/run', {}, expected=403)
        anon.call('/api/access/admin', {'action': 'guest', 'revision': 1}, expected=403)
        # Scoped tokens use the owner's current role and the group's current grant.
        expiry = time.strftime('%Y-%m-%d', time.gmtime(time.time() + 86400))
        token = admin.admin('token.create', name='operator integration', owner=users['reference'], scope=pg, level=2, create=False, expiry=expiry)['secret']
        api = Client(base); api.bearer = token
        api.call('/processes/' + public)
        api.call('/processes/' + private, expected=404)
        api.call('/processes/' + public, {'cmd': 'echo changed'}, 'PUT', expected=403)
        api.call('/api/access/admin', {'action': 'guest', 'revision': 1}, expected=403)
        api.call('/api/access/state', expected=403)
        api.call('/spawn', {'requester': 'x', 'kind': 'x'}, expected=403)
        token_id = next(t['id'] for t in admin.state()['db']['tokens'] if t['name'] == 'operator integration')
        admin.admin('token.revoke', id=token_id)
        api.call('/processes', expected=401)
        # Role changes immediately invalidate sessions and do not preserve prior privilege.
        u = next(u for u in admin.state()['db']['users'] if u['id'] == users['reference'])
        user_input = {k: u[k] for k in ['id', 'login', 'name', 'role', 'auth', 'status', 'external', 'groups']}
        user_input['role'] = 'viewer'
        admin.admin('user.upsert', user=user_input)
        ref.call('/processes', expected=401)
        ref.login('reference', password)
        ref.call('/processes/' + public + '/run', {}, expected=403)
        # Last administrator and API schema guards.
        root_user = next(u for u in admin.state()['db']['users'] if u['id'] == admin_id)
        root_input = {k: root_user[k] for k in ['id', 'login', 'name', 'role', 'auth', 'status', 'external', 'groups']}
        root_input['status'] = 'paused'
        revision = admin.state()['revision']
        admin.call('/api/access/admin', {'action': 'user.upsert', 'revision': revision, 'user': root_input}, expected=400)
        admin.call('/api/access/admin', {'action': 'user.upsert', 'revision': revision, 'user': {'login': 'x', 'hash': 'injected'}}, expected=400)
        # New local password rotates the session and makes old sessions unusable.
        other = Client(base); other.login('developer', password)
        new_password = password + '-new'
        dev.call('/auth/password', {'password': password, 'new_password': new_password})
        other.call('/processes', expected=401)
        check(dev.call('/auth/me')['user']['login'] == 'developer', 'password change issues a new session')
        dev.call('/auth/logout', {})
        dev.login('developer', new_password)
        # Native OS auth and OS execution are exercised when CI supplies an isolated account.
        os_user = os.environ.get('SPAWNPOINT_AUTH_TEST_USER')
        os_password = os.environ.get('SPAWNPOINT_AUTH_TEST_PASSWORD')
        if os_user and os_password:
            admin.admin('user.upsert', user={'name': 'OS user', 'role': 'viewer', 'auth': 'os', 'status': 'active', 'external': os_user, 'groups': []})
            native = Client(base); result = native.login(os_user, os_password, 'os')
            check(result['user']['auth'] == 'os' and result['user']['external'] == os_user and result['user']['id'], 'native OS login maps to the link provisioned without a separate login')
            native_id = result['user']['id']
            linked = next(u for u in admin.state()['db']['users'] if u['id'] == native_id)
            edit = {k: linked[k] for k in ['id', 'name', 'role', 'auth', 'status', 'external', 'groups']}
            edit['name'] = 'Renamed OS user'
            admin.admin('user.upsert', user=edit)
            after = next(u for u in admin.state()['db']['users'] if u['id'] == native_id)
            check(after['id'] == linked['id'] and after['external'] == linked['external'] and after['version'] == linked['version'], 'alias-free OS edit preserves identity and sessions')
            check(native.call('/auth/me')['user']['id'] == native_id, 'native session survives metadata-only edit')
            duplicate = {**edit, 'id': '', 'login': 'must-not-create-another-identity'}
            admin.call('/api/access/admin', {'action': 'user.upsert', 'revision': admin.state()['revision'], 'user': duplicate}, expected=400)
            admin.call('/api/access/admin', {'action': 'user.upsert', 'revision': admin.state()['revision'], 'user': {'name': 'No login', 'role': 'viewer', 'auth': 'local', 'status': 'active', 'password': password}}, expected=400)
            native.call('/auth/login', {'method': 'os', 'login': os_user, 'password': os_password + '-wrong'}, expected=401)
            execution = admin.call('/processes', {'cmd': 'whoami' if os.name == 'nt' else 'id -un', 'start': True, 'group_id': pg, 'run_as': {'mode': 'user', 'username': os_user, 'password': os_password}})
            check(execution['process']['status'] != 'failed', 'native run-as still launches behind the access gateway')
            eid = execution['process']['id']
            wait_for(lambda: os_user.lower() in admin.call('/processes/' + eid + '/logs').get('text', '').lower())
            admin.admin('policy',target='group',id=pg,rules=allowed_rules)
            manager.call('/processes/'+eid,{},'DELETE',expected=403)
            check(not manager.call('/processes/'+eid)['process']['capabilities']['remove'],'saved OS identity is protected from manager deletion')
            check(os_password.encode() not in (root / 'test.db').read_bytes(), 'OS password not persisted as plaintext')
        # Audit/state must never expose local hashes, raw tokens or submitted passwords.
        snapshot = admin.state()
        public_json = json.dumps(snapshot)
        check(password not in public_json and token not in public_json and 'pbkdf2-sha256$' not in public_json, 'admin snapshot redacts authentication secrets')
        check(password.encode() not in (root / 'test.db').read_bytes(), 'local password absent from SQLite')
        check(token.encode() not in (root / 'test.db').read_bytes(), 'raw API token absent from SQLite')
        check(any(a['action'] == '권한 거부' for a in snapshot['db']['audit']), 'denied actions are audited')
        check(any(a['action'] == '권한 변경' for a in snapshot['db']['audit']), 'policy changes are audited')
        # Persisted rate limits reject the sixth failed attempt.
        for _ in range(5):
            anon.call('/auth/login', {'method': 'local', 'login': 'rate-limit-test', 'password': 'wrong'}, expected=401)
        anon.call('/auth/login', {'method': 'local', 'login': 'rate-limit-test', 'password': 'wrong'}, expected=429)
        admin.call('/processes/' + created, {}, 'DELETE')
        # Reopen the real database: policies and sessions survive an orderly server restart.
        proc.terminate(); proc.wait(timeout=15)
        proc = subprocess.Popen([exe], env=env, stdout=output, stderr=output)
        wait_for(lambda: anon.call('/healthz')['ok'])
        check(not anon.call('/auth/me')['setup_required'], 'bootstrap stays disabled after restart')
        admin.state()
        check(len(admin.state()['db']['processGroups']) == len(snapshot['db']['processGroups']), 'groups survive restart')
        check(admin.call('/processes/' + moved)['process']['group_id']==hidden, 'editor group move survives server restart')
        if os_user and os_password:
            check(native.call('/auth/me')['user']['id'] == native_id, 'alias-free OS identity and session survive restart')
            check(native.login(os_user, os_password, 'os')['user']['id'] == native_id, 'native login still works after restart')
        print(f'PASS: {checks} real-server access-control checks; roles, groups, exceptions, batching, sessions, tokens, CSRF, redaction, audit and persistence')
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill(); proc.wait(timeout=5)
        output.close()
