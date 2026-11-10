from pathlib import Path
p=Path('.')
f=p/'tools/test-access-api.py';s=f.read_text()
s=s.replace("user={'login': 'os-linked', 'name': 'OS user'", "user={'name': 'OS user'")
s=s.replace("check(result['user']['login'] == 'os-linked', 'native OS login maps to provisioned application identity')", """check(result['user']['auth'] == 'os' and result['user']['external'] == os_user and result['user']['id'], 'native OS login maps to the link provisioned without a separate login')
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
            admin.call('/api/access/admin', {'action': 'user.upsert', 'revision': admin.state()['revision'], 'user': {'name': 'No login', 'role': 'viewer', 'auth': 'local', 'status': 'active', 'password': password}}, expected=400)""")
s=s.replace("check(len(admin.state()['db']['processGroups']) == len(snapshot['db']['processGroups']), 'groups survive restart')", """check(len(admin.state()['db']['processGroups']) == len(snapshot['db']['processGroups']), 'groups survive restart')
        if os_user and os_password:
            check(native.call('/auth/me')['user']['id'] == native_id, 'alias-free OS identity and session survive restart')
            check(native.login(os_user, os_password, 'os')['user']['id'] == native_id, 'native login still works after restart')""")
f.write_text(s)
f=p/'tools/test-run-as-service.py';s=f.read_text()
s=s.replace("'user':{'login':'native-linked','name':'Native account'", "'user':{'name':'Native account'")
s=s.replace("assert native['user']['login']=='native-linked', 'OS login through service'", "assert native['user']['auth']=='os' and native['user']['external']==USER and native['user']['id'], 'OS link without a separate login authenticates through service'")
f.write_text(s)
f=p/'tools/test-access-browser.py';s=f.read_text()
s=s.replace('import http.cookiejar\n', 'import getpass\nimport http.cookiejar\n')
needle="            uid = next(u['id'] for u in api_state()['db']['users'] if u['login']=='developer')\n"
insert='''            # OS links use one identity field, not a second application login.
            os_user = os.environ.get('SPAWNPOINT_AUTH_TEST_USER') or getpass.getuser()
            sent_users = []
            def record_user_request(req):
                if req.url.endswith('/api/access/admin') and req.method == 'POST':
                    data = req.post_data_json
                    if data.get('action') == 'user.upsert':
                        sent_users.append(data['user'])
            page.on('request', record_user_request)
            page.locator('[data-act="add-user"]').click()
            page.locator('#userName').fill('OS 연결 사용자')
            page.locator('#userLogin').fill('discard-this-alias')
            page.locator('#userTempPassword').fill('discard-this-password')
            page.locator('#userAuth').select_option('os')
            expect(page.locator('#userLogin')).not_to_be_visible()
            expect(page.locator('#userLogin')).to_be_disabled()
            expect(page.locator('#userTempPassword')).not_to_be_visible()
            expect(page.locator('#userTempPassword')).to_be_disabled()
            check(page.locator('#externalIdentity').evaluate('(e)=>e.required'), 'OS account identity is required')
            check(not page.locator('#userLogin').evaluate('(e)=>e.required'), 'hidden application login is not required')
            page.locator('#externalIdentity').fill(os_user)
            page.locator('#userAuth').select_option('local')
            expect(page.locator('#userLogin')).to_be_visible()
            expect(page.locator('#userTempPassword')).to_be_visible()
            check(page.locator('#userLogin').evaluate('(e)=>e.required'), 'local login becomes required again')
            expect(page.locator('#externalIdentity')).to_be_disabled()
            page.locator('#userAuth').select_option('os')
            for width, height in [(1440,1000),(390,844)]:
                page.set_viewport_size({'width':width,'height':height})
                box = page.locator('#modal').bounding_box()
                check(box['x'] >= -1 and box['x'] + box['width'] <= width + 1, 'OS user dialog fits viewport '+str(width))
                page.screenshot(path=str(artifacts/f'access-os-user-{width}.png'),full_page=True)
            page.set_viewport_size({'width':1440,'height':1000})
            save_modal()
            if not os.environ.get('SPAWNPOINT_BROWSER_BRIDGE'):
                check('login' not in sent_users[-1] and sent_users[-1]['password'] == '', 'OS request omits the alias and stale local password')
            linked = next(u for u in api_state()['db']['users'] if u['auth']=='os' and u['external']==os_user)
            expect(page.locator('tbody')).to_contain_text(os_user)
            page.locator('#userSearch').fill(os_user)
            expect(page.locator('tbody')).to_contain_text('OS 연결 사용자')
            page.locator('#userSearch').fill('')
            page.locator(f'[data-act="edit-user"][data-id="{linked["id"]}"]').click()
            expect(page.locator('#userLogin')).not_to_be_visible()
            page.locator('#userAuth').select_option('local')
            expect(page.locator('#userLogin')).to_be_editable()
            check(page.locator('#userTempPassword').evaluate('(e)=>e.required'), 'OS to local conversion needs its own password')
            page.locator('#userAuth').select_option('os')
            page.locator('#userName').fill('OS 연결 사용자 수정')
            save_modal()
            after = next(u for u in api_state()['db']['users'] if u['id']==linked['id'])
            check(after['external']==linked['external'] and after['version']==linked['version'], 'OS edit keeps identity and session version')
            page.remove_listener('request',record_user_request)
'''
assert s.count(needle)==1;s=s.replace(needle,needle+insert)
f.write_text(s)
f=p/'docs/access-control.md';s=f.read_text();needle='  역할을 부여해야 한다. 사용자 ID 문자열만 같다고 자체 계정과 합치지 않는다.';assert s.count(needle)==1;s=s.replace(needle, needle+'\n  사용자 추가·편집에서 별도 사용자 ID나 자체 비밀번호를 입력하지 않는다. OS 계정\n  식별자만 받으며 내부 ID는 자동 생성한다. Windows SID/Linux UID 중복 연결은\n  거절한다. 기존 연결의 내부 ID·권한·그룹은 그대로 유지하며, 사용자 ID 입력은\n  SpawnPoint 자체 계정에서만 필수다.');f.write_text(s)
