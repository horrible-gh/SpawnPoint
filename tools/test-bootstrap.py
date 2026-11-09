#!/usr/bin/env python3
"""Real server, OS-admin bootstrap and elevated local CLI. Disposable DB only."""
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

EXE=str(Path(sys.argv[1]).resolve())
PASSWORD='Bootstrap!'+secrets.token_hex(20)
CHECKS=0

def check(value,message):
    global CHECKS
    if not value: raise AssertionError(message)
    CHECKS+=1

class Server:
    def __init__(self,root):
        self.root=root; root.mkdir()
        self.db=root/'custom database.db'
        with socket.socket() as s:
            s.bind(('127.0.0.1',0));self.port=s.getsockname()[1]
        self.base=f'http://127.0.0.1:{self.port}'
        self.env={**os.environ,'SPAWNPOINT_HOST':'127.0.0.1','SPAWNPOINT_PORT':str(self.port),
                  'SPAWNPOINT_DB_PATH':str(self.db),'SPAWNPOINT_LOG_DIR':str(root/'logs'),
                  'SPAWNPOINT_SETUP_TOKEN':'','SPAWNPOINT_API_TOKENS':''}
        self.log=open(root/'server.log','wb')
        self.jar=http.cookiejar.CookieJar()
        self.http=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPCookieProcessor(self.jar))
        self.proc=None; self.start()
    def start(self):
        self.proc=subprocess.Popen([EXE],cwd=self.root,env=self.env,stdout=self.log,stderr=self.log)
        for _ in range(150):
            try:
                if self.call('/healthz')[0]==200:return
            except (OSError,ValueError):pass
            if self.proc.poll() is not None: raise AssertionError('server exited before readiness')
            time.sleep(.1)
        raise AssertionError('server startup timed out')
    def stop(self):
        if self.proc is not None and self.proc.poll() is None:
            self.proc.terminate();self.proc.wait(timeout=15)
    def close(self): self.stop();self.log.close()
    def call(self,path,data=None,headers=None):
        req=urllib.request.Request(self.base+path,data=None if data is None else json.dumps(data).encode(),headers={'Content-Type':'application/json',**(headers or {})})
        try:r=self.http.open(req,timeout=35)
        except urllib.error.HTTPError as e:r=e
        with r:return r.status,json.loads(r.read())
    def cli(self,*args,stdin=None,elevated=False):
        prefix=['sudo','-n','--'] if elevated and os.name!='nt' and os.geteuid()!=0 else []
        # A different cwd/env must not make the CLI guess a different database.
        env={**os.environ,'SPAWNPOINT_DB_PATH':str(self.root/'WRONG-do-not-create.db')}
        return subprocess.run(prefix+[EXE,*args,'--url',self.base],input=stdin,text=True,encoding="utf-8",capture_output=True,cwd=self.root.parent,env=env,timeout=30)

with tempfile.TemporaryDirectory(prefix='sp-bootstrap-') as work:
    root=Path(work)
    s=Server(root/'CLI instance')
    try:
        r=s.cli('setup','status')
        check(r.returncode==0 and str(s.db) in r.stdout,'CLI locates actual running database, not cwd/env guess')
        check(not (s.root/'WRONG-do-not-create.db').exists(),'diagnostics never create a new DB')
        token=Path(str(s.db)+'.setup-token').read_text().strip()
        check(token not in r.stdout+r.stderr,'status hides token')
        code,me=s.call('/auth/me');check(code==200 and str(s.db) not in json.dumps(me) and token not in json.dumps(me),'public identity response hides paths and token')
        check(me['runtime']['local_setup_url']==s.base,'UI CLI command has real listener port')
        for headers in [{'Host':'evil.invalid'},{'X-Forwarded-For':'203.0.113.5'},{'Origin':'https://evil.invalid'}]:
            code,data=s.call('/auth/setup-info',headers=headers)
            check(code==403 and str(s.db) not in json.dumps(data),'diagnostic paths reject remote/proxy/cross-origin')
        r=s.cli('setup','token',elevated=True)
        check(r.returncode==0 and r.stdout.strip()==token,'explicit elevated token command works')
        r=s.cli('admin','init','--login','cli-admin','--name','CLI Administrator','--password-stdin',stdin=PASSWORD+'\n',elevated=True)
        check(r.returncode==0,'elevated CLI creates initial admin: '+r.stderr)
        check(PASSWORD not in r.stdout+r.stderr and token not in r.stdout+r.stderr,'CLI creation does not print secrets')
        check(not Path(str(s.db)+'.setup-token').exists(),'initialization removes token')
        check(s.call('/auth/login',{'method':'local','login':'cli-admin','password':PASSWORD})[0]==200,'created local admin can log in')
        r=s.cli('admin','init','--login','takeover','--password-stdin',stdin=PASSWORD+'\n',elevated=True)
        check(r.returncode!=0,'CLI cannot reinitialize existing admin')
        check(s.cli('setup','token',elevated=True).returncode!=0,'no bootstrap token after enrollment')
        s.stop();s.start()
        check(not s.call('/auth/me')[1]['setup_required'],'administrator persists across restart')
        check(s.call('/auth/login',{'method':'local','login':'cli-admin','password':PASSWORD})[0]==200,'local login after restart')
    finally:s.close()
    admin=os.environ.get('SPAWNPOINT_ADMIN_TEST_USER')
    pw=os.environ.get('SPAWNPOINT_ADMIN_TEST_PASSWORD')
    if admin and pw:
        s=Server(root/'OS instance')
        try:
            standard=os.environ['SPAWNPOINT_AUTH_TEST_USER'];standard_pw=os.environ['SPAWNPOINT_AUTH_TEST_PASSWORD']
            for login,password in [(standard,standard_pw),(admin,pw+'wrong')]:
                check(s.call('/auth/setup-os',{'login':login,'password':password})[0]==403,'non-admin or wrong password cannot bootstrap')
            check(s.call('/auth/setup-os',{'login':admin,'password':pw},headers={'Host':'remote.invalid'})[0]==403,'remote HTTP cannot submit OS password')
            code,data=s.call('/auth/setup-os',{'login':admin,'password':pw})
            check(code==200 and data['user']['role']=='admin' and data['user']['auth']=='os','OS admin authenticates and enrolls directly')
            check(pw.encode() not in s.db.read_bytes(),'OS password is not persisted')
            check(not Path(str(s.db)+'.setup-token').exists(),'OS bootstrap also removes token')
            check(s.call('/auth/setup-os',{'login':admin,'password':pw})[0]==403,'OS bootstrap cannot bypass existing users')
            s.stop();s.start()
            check(s.call('/auth/login',{'method':'os','login':admin,'password':pw})[0]==200,'OS administrator can log in after restart')
        finally:s.close()
    else:
        print('SKIP native OS-admin scenario: no isolated account fixture')
print(f'PASS: {CHECKS} bootstrap/CLI/identity/transport/restart checks')
