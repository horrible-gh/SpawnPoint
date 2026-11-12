#!/usr/bin/env python3
"""Real server, real OS account, SQLite restore and process lifecycle regression."""
from contextlib import closing
import http.cookiejar, json, os, pathlib, secrets, socket, sqlite3, subprocess, sys, tempfile, time, urllib.error, urllib.request
USER=os.environ['SPAWNPOINT_RUNAS_TEST_USER']; PASSWORD=os.environ['SPAWNPOINT_RUNAS_TEST_PASSWORD']
EXE=str(pathlib.Path(sys.argv[1]).resolve()); WIN=os.name=='nt'
with socket.socket() as s:s.bind(('127.0.0.1',0)); PORT=s.getsockname()[1]
URL=f'http://127.0.0.1:{PORT}'
SETUP_TOKEN=secrets.token_urlsafe(32); ADMIN_PASSWORD='Test!'+secrets.token_hex(20)
OPENER=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
CSRF=''

def call(path, data=None, method=None, origin=None):
    headers={'Content-Type':'application/json'}
    if CSRF:headers['X-CSRF-Token']=CSRF
    if origin:headers['Origin']=origin
    req=urllib.request.Request(URL+path,headers=headers,data=None if data is None else json.dumps(data).encode(),method=method)
    try:
        with OPENER.open(req,timeout=45) as r:return r.status,json.load(r)
    except urllib.error.HTTPError as e:return e.code,json.load(e)

def until(fn,seconds=20):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        try:
            value=fn()
            if value:return value
        except (OSError,ValueError):pass
        time.sleep(.15)
    raise AssertionError('Condition did not become true')

with tempfile.TemporaryDirectory(prefix='spawnpoint-account-') as tmp:
    root=pathlib.Path(tmp); db=root/'runner.db'; logdir=root/'logs'
    env={**os.environ,'SPAWNPOINT_HOST':'127.0.0.1','SPAWNPOINT_PORT':str(PORT),'SPAWNPOINT_DB_PATH':str(db),'SPAWNPOINT_LOG_DIR':str(logdir),'SPAWNPOINT_API_TOKENS':'','SPAWNPOINT_SETUP_TOKEN':SETUP_TOKEN,'SPAWNPOINT_KILL_CHILDREN_ON_EXIT':'1'}
    env.pop('SPAWNPOINT_RUNAS_TEST_PASSWORD',None)
    stdout=(root/'server-output.log').open('ab');proc=None
    def start():
        global proc
        proc=subprocess.Popen([EXE],env=env,stdout=stdout,stderr=subprocess.STDOUT)
        until(lambda: call('/healthz')[0]==200)
    def stop():
        if proc and proc.poll() is None:
            proc.terminate()
            try:proc.wait(timeout=20)
            except subprocess.TimeoutExpired:proc.kill();proc.wait();raise
    try:
        start()
        status,out=call('/auth/setup',{'login':'test-admin','name':'Test administrator','password':ADMIN_PASSWORD,'setup_token':SETUP_TOKEN})
        assert status==200 and out.get('csrf'),('admin setup failed',status,out)
        CSRF=out['csrf']
        status,runtime=call('/runtime')
        assert status==200 and runtime.get('run_as_supported'),('runtime unavailable',status,runtime)
        command=('whoami & echo HOME=%USERPROFILE% & echo ENV=%SPAWNPOINT_RUNAS_CHECK% & ping -n 60 127.0.0.1 >nul' if WIN else 'id -un; printf "HOME=%s\nENV=%s\n" "$HOME" "$SPAWNPOINT_RUNAS_CHECK"; echo CHILD_PID=$$; sleep 60')
        body={'label':'identity-e2e','cmd':command,'env':{'SPAWNPOINT_RUNAS_CHECK':'value,with=punctuation'},'start':False,'auto_start':True,'run_as':{'mode':'user','username':USER,'password':PASSWORD}}
        status,out=call('/processes',body); assert status==200 and out['persisted'] and out['process']['status']=='stopped',out
        item=out['process'];id=item['id'];assert item['run_as']['has_password']
        assert PASSWORD not in json.dumps(out) and 'sealed' not in json.dumps(out)
        status,out=call('/processes',body,origin='https://attacker.invalid');assert status==403
        status,out=call('/processes/'+id+'/run',{},'POST');assert status==200 and out['process']['status']=='running',out
        def output_ready():
            text=call('/processes/'+id+'/logs')[1].get('text','')
            return text if 'ENV=value,with=punctuation' in text else None
        text=until(output_ready);assert USER.lower() in text.lower(),text;assert PASSWORD not in text
        status,out=call('/processes/'+id+'/stop',{},'POST');assert out['process']['status']=='stopped',out
        # Empty password retains only this same account. An identity edit alone cannot run it.
        body['label']='edited';body['run_as'].pop('password')
        status,out=call('/processes/'+id,body,'PUT');assert status==200 and out['persisted'] and out['process']['status']=='stopped',out
        with closing(sqlite3.connect(db)) as conn:
            stored=conn.execute('select run_as_json from runner_entry where id=?',(id,)).fetchone()[0]
        assert 'sealed' in stored and PASSWORD not in stored
        assert PASSWORD.encode() not in db.read_bytes()
        stop();start()
        status,out=call('/auth/login',{'method':'local','login':'test-admin','password':ADMIN_PASSWORD})
        assert status==200 and out.get('csrf'),('admin login after restart failed',status,out)
        CSRF=out['csrf']
        until(lambda:call('/processes/'+id)[1]['process']['status']=='running')
        text=until(output_ready);assert USER.lower() in text.lower()
        call('/processes/'+id+'/stop',{},'POST')
        # Wrong password must not execute under any fallback identity.
        bad={'cmd':'echo FORBIDDEN-FALLBACK','start':True,'run_as':{'mode':'user','username':USER,'password':PASSWORD+'incorrect'}}
        status,out=call('/processes',bad);assert status==200 and out['process']['status']=='failed',out
        badid=out['process']['id'];badlog=call('/processes/'+badid+'/logs')[1].get('text','');assert 'FORBIDDEN-FALLBACK' not in badlog
        assert PASSWORD not in json.dumps(out)+badlog
        body['run_as']={'mode':'service'};body['auto_start']=False
        status,out=call('/processes/'+id,body,'PUT');assert status==200 and out['persisted'] and 'run_as' not in out['process'],out
        with closing(sqlite3.connect(db)) as conn:assert conn.execute('select run_as_json from runner_entry where id=?',(id,)).fetchone()[0]==''
        for entry in (id,badid):assert call('/processes/'+entry,method='DELETE')[0]==200
        print('PASS: real account identity, clean environment, output, stop, edit/retain, encrypted SQLite, restart/auto-start, wrong-password/no-fallback, clear identity and delete')
    finally:
        stop();stdout.close()
        output=(root/'server-output.log').read_bytes()
        assert PASSWORD.encode() not in output and ADMIN_PASSWORD.encode() not in output and SETUP_TOKEN.encode() not in output, 'credential leaked to server output'
