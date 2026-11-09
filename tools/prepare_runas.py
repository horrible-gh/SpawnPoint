#!/usr/bin/env python3
"""One-time, anchor-checked integration of the reviewed run-as/editor change.
Executed only on the verification branch; the generated normal source is committed
before running the complete test matrix. This script removes itself afterwards.
"""
from pathlib import Path
import base64, importlib.util, json, re, shutil, zlib
ROOT=Path.cwd()
bundles=sorted((ROOT/'tools').glob('runas-overlay-*.b64'))
if bundles:
    payload=''.join(b.read_text() for b in bundles)
    files=json.loads(zlib.decompress(base64.b64decode(payload)))
    for name,content in files.items():
        path=Path(name)
        if path.is_absolute() or '..' in path.parts: raise RuntimeError('invalid overlay path')
        target=ROOT/path;target.parent.mkdir(parents=True,exist_ok=True);target.write_text(content,encoding='utf-8')

def once(s,a,b):
    if s.count(a)!=1: raise RuntimeError(f'anchor count {s.count(a)}: {a[:150]!r}')
    return s.replace(a,b,1)
def edit(path,fn):
    p=ROOT/path;s=p.read_text(encoding='utf-8');p.write_text(fn(s),encoding='utf-8')
def add_import(s,name): return once(s,'import (','import (\n\t"'+name+'"')

def manager(s):
    s=add_import(s,'spawnpoint/internal/runas')
    s=once(s,'\tkillOnExit bool','\tkillOnExit bool\n\tcredentialDir string')
    begin=s.index('func (m *Manager) register(');end=s.index('// Update replaces',begin)
    replacement='''func (m *Manager) register(label, cmd string, cwd *string, env map[string]string, start bool, autoStart ...bool) (Info, bool) {
    return m.registerConfigured(label,cmd,cwd,env,start,nil,autoStart...)
}

func (m *Manager) registerConfigured(label, cmd string, cwd *string, env map[string]string, start bool, identity *runas.Spec, autoStart ...bool) (Info, bool) {
    now:=time.Now()
    e:=&entry{id:m.newID(),label:label,cmd:cmd,cwd:normaliseCwd(cwd),env:copyEnv(env),runAs:runas.Clone(identity),createdAt:now,updatedAt:now,status:StatusStopped}
    if len(autoStart)>0 {e.autoStart=autoStart[0]}
    m.mu.Lock();m.entries[e.id]=e;m.mu.Unlock()
    persisted:=m.persist(e)
    e.mu.Lock()
    if identity!=nil && !persisted {
        e.markFailed("Execution identity was not saved; command was not started.",time.Now())
    } else if start {m.spawn(e,"")}
    info:=e.infoLocked();e.mu.Unlock()
    return info,persisted
}

'''
    s=s[:begin]+replacement+s[end:]
    s=once(s,'\tfor _, row := range rows {\n\t\te := &entry{','\tfor _, row := range rows {\n        identity,err:=runas.Parse(row.RunAsJSON)\n        if err!=nil{return 0,fmt.Errorf("restore execution identity for %s: %w",row.ID,err)}\n\t\te := &entry{\n            runAs: identity,')
    s=once(s,'\t\tAutoStart: e.autoStart,','\t\tAutoStart: e.autoStart,\n        RunAsJSON: e.runAs.JSON(),')
    s=once(s,'func (m *Manager) spawn(e *entry, marker string) {','func (m *Manager) spawn(e *entry, marker string) {\n    if e.runAs!=nil {m.spawnAs(e,marker);return}')
    a='''	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}

	e.mu.Lock()'''
    b='''	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
    m.finishRun(e,gen,code)
}

func (m *Manager) finishRun(e *entry,gen,code int) {
	e.mu.Lock()'''
    s=once(s,a,b)
    return s
edit('internal/runner/manager.go',manager)
def entry(s):
    s=add_import(s,'spawnpoint/internal/runas')
    s=once(s,'\tAutoStart bool','\tAutoStart bool\n    RunAs *runas.Summary')
    s=once(s,'\tautoStart bool','\tautoStart bool\n    runAs *runas.Spec')
    return once(s,'\t\tAutoStart: e.autoStart,','\t\tAutoStart: e.autoStart,\n        RunAs: e.runAs.Summary(),')
edit('internal/runner/entry.go',entry)
def main(s):
    s=add_import(s,'spawnpoint/internal/runas')
    s=once(s,'func main() {','func main() {\n    if runas.HandleChild(){return}')
    return once(s,'procs := runner.New(cfg.LogDir, log, db, cfg.KillChildrenOnExit)','procs := runner.New(cfg.LogDir, log, db, cfg.KillChildrenOnExit)\n    procs.SetCredentialDirectory(cfg.DBPath+".credentials")')
edit('cmd/spawnpoint/main.go',main)
edit('internal/store/model.go',lambda s:once(s,'\tAutoStart bool','\tAutoStart bool\n    RunAsJSON string'))
def store(s):
    s=once(s,'env, ts, ts, e.AutoStart)','env, ts, ts, e.AutoStart, e.RunAsJSON)')
    return once(s,'&updatedAt, &e.AutoStart);','&updatedAt, &e.AutoStart, &e.RunAsJSON);')
edit('internal/store/store.go',store)
edit('internal/dialect/sql/sqlite/runner_entry_list.sql',lambda s:once(s,'updated_at, auto_start','updated_at, auto_start, run_as_json'))
def upsert(s):
    s=once(s,'updated_at, auto_start)','updated_at, auto_start, run_as_json)')
    s=once(s,'VALUES (?, ?, ?, ?, ?, ?, ?, ?)','VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)')
    return once(s,'auto_start = excluded.auto_start','auto_start = excluded.auto_start,\n    run_as_json = excluded.run_as_json')
edit('internal/dialect/sql/sqlite/runner_entry_upsert.sql',upsert)
p=ROOT/'internal/dialect/sql/migration/sqlite/005_runner_run_as.sql'
p.write_text("-- Empty means service identity; legacy commands retain their original account.\nALTER TABLE runner_entry ADD COLUMN run_as_json TEXT NOT NULL DEFAULT '';\n")

def processes(s):
    s=add_import(s,'spawnpoint/internal/runas')
    s=once(s,'\tAutoStart bool `json:"auto_start,omitempty"`','\tAutoStart bool `json:"auto_start,omitempty"`\n    RunAs *runas.Summary `json:"run_as,omitempty"`')
    s=once(s,'\t\tAutoStart: info.AutoStart,','\t\tAutoStart: info.AutoStart,\n        RunAs: info.RunAs,')
    s=once(s,'\tautoStart []bool','\tautoStart []bool\n    runAs *runas.Input')
    s=once(s,'\treturn out, nil\n}\n\n// firstWord','    var identityError *apiError\n    out.runAs,identityError=parseRunAs(obj["run_as"])\n    if identityError!=nil{return out,identityError}\n\treturn out, nil\n}\n\n// firstWord')
    s=once(s,'\tif start {\n\t\tinfo, persisted = s.runner.Register(','''    if reg.runAs!=nil {
        if reg.runAs.Mode=="user" && !s.identityGuard(w,r,true){return}
        impl,ok:=s.runner.(identityRunner)
        if !ok {s.fail(w,r,invalidField("run_as","Execution identity is unavailable."));return}
        var err error
        info,persisted,err=impl.RegisterIdentity(reg.label,reg.cmd,reg.cwd,reg.env,*reg.runAs,start,reg.autoStart...)
        if err!=nil{s.fail(w,r,invalidField("run_as",err.Error()));return}
    } else if start {
		info, persisted = s.runner.Register(''')
    s=once(s,'\tinfo, persisted, ok := s.runner.Update(id, &reg.label, &reg.cmd, &cwd, reg.env, reg.autoStart...)','''    current,_:=s.runner.Get(id)
    var info runner.Info
    var persisted,ok bool
    if reg.runAs!=nil || current.RunAs!=nil {
        if (current.RunAs!=nil || (reg.runAs!=nil && reg.runAs.Mode=="user")) && !s.identityGuard(w,r,true){return}
        impl,supported:=s.runner.(identityRunner)
        if !supported{s.fail(w,r,invalidField("run_as","Execution identity is unavailable."));return}
        var err error
        info,persisted,ok,err=impl.UpdateIdentity(id,&reg.label,&reg.cmd,&cwd,reg.env,reg.runAs,reg.autoStart...)
        if err!=nil{s.fail(w,r,invalidField("run_as",err.Error()));return}
    }else{info,persisted,ok=s.runner.Update(id,&reg.label,&reg.cmd,&cwd,reg.env,reg.autoStart...)}''')
    s=once(s,'func (s *Server) serveAction(w http.ResponseWriter, r *http.Request, id, action string) {','func (s *Server) serveAction(w http.ResponseWriter, r *http.Request, id, action string) {\n    if !s.guardExistingIdentity(w,r,id,false){return}')
    s=once(s,'func (s *Server) serveDelete(w http.ResponseWriter, r *http.Request, id string) {','func (s *Server) serveDelete(w http.ResponseWriter, r *http.Request, id string) {\n    if !s.guardExistingIdentity(w,r,id,false){return}')
    return s
edit('internal/httpapi/processes.go',processes)
def api(s):
    s=once(s,'\trouteHelp\n','\trouteHelp\n    routeRuntime\n')
    s=once(s,'\tcase "/healthz":','    case "/runtime":\n        return route{kind:routeRuntime, methods:[]string{http.MethodGet,http.MethodHead}}\n\tcase "/healthz":')
    s=once(s,'\tswitch rt.kind {','    if (r.Method==http.MethodPost || r.Method==http.MethodPut || r.Method==http.MethodDelete) && !sameOrigin(r) {s.identityRefuse(w,r,"Cross-origin requests are not allowed.");return}\n\tswitch rt.kind {\n    case routeRuntime:\n        s.serveRuntime(w,r)')
    return s
edit('internal/httpapi/api.go',api)
def contract(s):
    s=once(s,'routes := []ContractRoute{','routes := []ContractRoute{\n        contractRoute(s,"runtime.info","/runtime","/runtime","runtime_response",http.MethodGet,http.MethodHead),')
    s=once(s,'PathMethods: map[string][]string{','PathMethods: map[string][]string{\n            "/runtime":append([]string(nil),s.resolve("/runtime").methods...),')
    s=once(s,'{Name: "auto_start", Type: "boolean", Required: false, Nullable: false, Default: false, ValidationOrder: 5},','{Name: "auto_start", Type: "boolean", Required: false, Nullable: false, Default: false, ValidationOrder: 5},\n            {Name:"run_as",Type:"object",Required:false,Nullable:false,Default:nil,ValidationOrder:6},')
    s=once(s,'{"cmd", "label", "cwd", "env", "auto_start"}','{"cmd", "label", "cwd", "env", "auto_start", "run_as"}')
    s=once(s,'SchemaKeys: map[string][]string{','SchemaKeys: map[string][]string{\n            "runtime_response":{"ok","os","service_account","run_as_supported","run_as_reason"},')
    return s
edit('internal/httpapi/contract.go',contract)
p=ROOT/'internal/apispec/help.json';doc=json.loads(p.read_text())
doc['schemas']['registration']['keys'].append('run_as')
doc['schemas']['registration']['fields'].append(dict(name='run_as',type='object',required=False,nullable=False,default=None,validation_order=6,max_runes=None,description='Optional execution identity: {mode: service} removes credentials; {mode: user, username, domain?, password?} uses an authenticated OS account. Omission preserves the current identity on update. An omitted password only preserves a credential for the same account.'))
doc['schemas']['process']['keys'].append('run_as')
doc['schemas']['process']['conditional_keys']['run_as']='Included for a specified account; contains mode, username, optional domain, has_password; never password or sealed credential.'
doc['schemas']['runtime_response']={'keys':['ok','os','service_account','run_as_supported','run_as_reason']}
ep=json.loads(json.dumps(next(x for x in doc['endpoints'] if x['id']=='api.help')))
ep['id']='runtime.info';ep['path']='/runtime';ep['success']['response_schema']='runtime_response';doc['endpoints'].append(ep)
p.write_text(json.dumps(doc,ensure_ascii=False,indent=2)+'\n')
# Preserve the historical schema fixture, updating only the known feature suffix.
p=ROOT/'internal/store/autostart_test.go';s=p.read_text().replace('len(scripts) != 4','len(scripts) != 5').replace('already != 3 || applied != 1','already != 3 || applied != 2').replace('already != 4 || applied != 0','already != 5 || applied != 0').replace('already != 0 || applied != 4','already != 0 || applied != 5');p.write_text(s)
p=ROOT/'internal/dialect/assets_test.go';s=p.read_text();s,n=re.subn(r'("004_runner_auto_start.sql"\s*:\s*1,)',r'\1\n        "005_runner_run_as.sql": 1,',s);assert n==1;p.write_text(s)
# Reuse the approved editor conversion, then wire the real identity controls.
src=ROOT/'tools/editor-source'
module_spec=importlib.util.spec_from_file_location('editor_base',src/'base_apply.py');m=importlib.util.module_from_spec(module_spec);module_spec.loader.exec_module(m)
m.ROOT=src
(src/'fragments').mkdir(exist_ok=True)
for name in ['editor.css','editor.html','editor.js','environment.js']:shutil.copy(src/name,src/'fragments'/name)
p=ROOT/'internal/webui/index.html'
assert m.blob_sha(p.read_bytes())==m.BASE_BLOB,'The inspected editor source changed; refusing replacement.'
s=m.transform(p.read_text())
s=once(s,'    syncEditor();','    syncEditor(p);')
s=once(s,'  var state = {','  var apiToken = ""; // Kept only in this tab, never persisted.\n  var state = {')
s=once(s,'    var headers = opts.headers || {};','    var headers = opts.headers || {};\n    if(apiToken) headers["Authorization"]="Bearer "+apiToken;')
s=s.replace('Authentication required — this server is running with SPAWNPOINT_API_TOKENS set.','Authentication required · click to enter API token')
s=once(s,'    var parsedEnv;','    var runAsInput;\n    try{runAsInput=executionInput();}catch(error){setNote(error.message,true);return;}\n    var parsedEnv;')
s=once(s,'      auto_start: el.fAutoStart.checked,','      auto_start: el.fAutoStart.checked,\n      run_as:runAsInput,')
s=once(s,'          auto_start: payload.auto_start,','          auto_start: payload.auto_start,\n          run_as:payload.run_as,')
s=once(s,'    editorRows = []; el.fEnv.value = "[]"; editorElement("envList").replaceChildren();','    editorRows = []; el.fEnv.value = "[]"; editorElement("envList").replaceChildren();\n    editorElement("passwordInput").value="";editorElement("usernameInput").value="";originalIdentity=null;')
s=once(s,'          if (!alsoRun) return putData;','          if (!alsoRun) return putData;\n          if(putData.persisted===false){notePersistence(putData);throw new Error("Save failed; command was not restarted.");}')
p.write_text(s)
# Extend the lightweight DOM harness without replacing its existing coverage.
p=ROOT/'tools/test-autostart-ui.cjs';s=p.read_text();s=once(s,'  focus() {}','''  focus() {}
  setAttribute(k,v){(this.attrs ||= {})[k]=String(v);}
  getAttribute(k){return (this.attrs || {})[k];}
  replaceChildren(...nodes){this.children=nodes;}
  append(...nodes){(this.children ||= []).push(...nodes);}
  querySelectorAll(){return this.all || [];}
  dispatchEvent(event){this.emit(event.type);return true;}
  getClientRects(){return [1];}
  get lastElementChild(){return (this.children || []).at(-1);}
  querySelector(){return (this.children || [])[0];}
  contains(){return false;}
''')
s=once(s,'const calls = [];','elements.forEach((e,id)=>{e.id=id;e.children=[];e.all=[...elements.values()];});\n  const calls = [];')
s=once(s,'    console,','    console,\n    Event:class{constructor(type){this.type=type;}},')
s=once(s,'      addEventListener() {},','      addEventListener() {},\n      createElement(){return new Element();},')
s=once(s,"if (method === 'GET' && url === '/processes') {","if(method === 'GET' && url === '/runtime'){data={ok:true,os:'windows',service_account:'service',run_as_supported:true,run_as_reason:''};}\n      else if (method === 'GET' && url === '/processes') {")
p.write_text(s)
shutil.rmtree(src)
for bundle in bundles:bundle.unlink(missing_ok=True)
(ROOT/'tools/prepare_runas.py').unlink(missing_ok=True)
print('Integrated execution identities and approved editor. Normal source files are ready for verification.')
