// Run with Node.js: node tools/test-autostart-ui.cjs
// Exercises the actual embedded script without installing browser dependencies.
// This checks form behaviour and request payloads, not visual browser rendering.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.join(__dirname, '../internal/webui/index.html'), 'utf8');
const match = html.match(/<script>([\s\S]*?)<\/script>/);
assert.ok(match, 'embedded script missing');
new vm.Script(match[1]);
const source = match[1].replace(/\}\)\(\);\s*$/, 'globalThis.testUI = { openForm, saveForm, renderProcesses, requestClose, state, dlg };\n})();');
assert.notEqual(source, match[1], 'test export injection failed');

class Element {
  constructor() {
    this.value = ''; this.checked = false; this.disabled = false;
    this.style = {}; this.innerHTML = ''; this.textContent = '';
    this.scrollHeight = 40; this.events = {};
    const classes = new Set();
    this.classList = {
      add: x => classes.add(x), remove: x => classes.delete(x),
      contains: x => classes.has(x),
      toggle(x, value) {
        const on = value === undefined ? !classes.has(x) : value;
        if (on) classes.add(x); else classes.delete(x);
        return on;
      },
    };
  }
  addEventListener(name, fn) { (this.events[name] ||= []).push(fn); }
  emit(name) { for (const fn of this.events[name] || []) fn({ target: this }); }
  focus() {}
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

}

function harness(persisted = true) {
  const ids = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map(m => m[1]));
  const elements = new Map([...ids].map(id => [id, new Element()]));
  elements.forEach((e,id)=>{e.id=id;e.children=[];e.all=[...elements.values()];});
  const calls = [];
  const entries = new Map();
  const ctx = vm.createContext({
    console,
    Event:class{constructor(type){this.type=type;}},
    window: { localStorage: { getItem() { return null; }, setItem() {} } },
    document: {
      getElementById(id) { assert.ok(elements.has(id), `unknown DOM id: ${id}`); return elements.get(id); },
      addEventListener() {},
      createElement(){return new Element();},
    },
    setTimeout() { return 1; }, clearTimeout() {}, setInterval() { return 1; },
    fetch: async (url, options = {}) => {
      const method = options.method || 'GET';
      const body = options.body ? JSON.parse(options.body) : undefined;
      calls.push({ url, method, body });
      let data;
      if(method === 'GET' && url === '/runtime'){data={ok:true,os:'windows',service_account:'service',run_as_supported:true,run_as_reason:''};}
      else if (method === 'GET' && url === '/processes') {
        data = { ok: true, processes: [...entries.values()] };
      } else if (method === 'GET' && url.includes('/logs')) {
        data = { ok: true, text: '', size: 0, next_offset: 0 };
      } else if (method === 'POST' && url.endsWith('/restart')) {
        const id = url.split('/')[2];
        const p = { ...entries.get(id), status: 'running' };
        entries.set(id, p);
        data = { ok: true, process: p }; // deliberately no persisted key
      } else if (method === 'PUT' || (method === 'POST' && url === '/processes')) {
        const id = method === 'PUT' ? url.split('/')[2] : 'proc_12345678';
        const p = { id, label: body.label || 'echo', cmd: body.cmd, env: body.env || {},
          auto_start: body.auto_start, status: method === 'PUT' ? 'stopped' : body.start === false ? 'stopped' : 'running' };
        entries.set(id, p);
        data = { ok: true, persisted, process: p };
      } else {
        throw new Error(`unexpected request ${method} ${url}`);
      }
      return { ok: true, status: 200, json: async () => data };
    },
  });
  vm.runInContext(source, ctx);
  return { ui: ctx.testUI, el: id => elements.get(id), calls, entries };
}

async function flush() {
  for (let i = 0; i < 4; i++) await new Promise(resolve => setImmediate(resolve));
}

async function main() {
  let checks = 0;
  for (const mode of ['new', 'edit']) {
    for (const alsoRun of [false, true]) {
      for (const flag of [false, true]) {
        const h = harness();
        await flush();
        const p = mode === 'edit' ? { id: 'proc_abcdef01', cmd: 'echo previous', auto_start: !flag } : null;
        h.ui.openForm(mode, p);
        h.el('fCmd').value = 'echo hello';
        h.el('fAutoStart').checked = flag;
        h.ui.saveForm(alsoRun);
        await flush();
        const writes = h.calls.filter(c => c.method !== 'GET');
        assert.equal(writes.length, mode === 'edit' && alsoRun ? 2 : 1);
        assert.equal(writes[0].method, mode === 'edit' ? 'PUT' : 'POST');
        assert.equal(writes[0].body.auto_start, flag, `${mode}, run=${alsoRun}, flag=${flag}`);
        if (mode === 'new') assert.equal(writes[0].body.start, alsoRun ? undefined : false);
        if (mode === 'edit' && alsoRun) assert.equal(writes[1].url, '/processes/proc_abcdef01/restart');
        assert.equal(h.el('dlgSave').disabled, false);
        checks++;
      }
    }
  }
  {
    const h = harness(); await flush();
    h.ui.openForm('edit', { id: 'proc_abcdef01', cmd: 'echo hello', auto_start: true });
    assert.equal(h.el('fAutoStart').checked, true);
    h.ui.openForm('new', null);
    assert.equal(h.el('fAutoStart').checked, false, 'new form inherited the old flag');
    h.ui.openForm('edit', { id: 'proc_abcdef01', cmd: 'echo hello' });
    assert.equal(h.el('fAutoStart').checked, false, 'legacy response must default off');
    h.el('fAutoStart').checked = true;
    h.el('fAutoStart').emit('change');
    assert.equal(h.ui.dlg.dirty, true);
    h.ui.requestClose();
    assert.equal(h.el('confirmBackdrop').classList.contains('open'), true, 'toggle-only edits must confirm discard');
    checks++;
  }
  {
    const h = harness(); await flush();
    h.ui.state.processes = [{ id: 'proc_abcdef01', label: '<unsafe>', cmd: 'echo hello', status: 'stopped', auto_start: true }];
    h.ui.renderProcesses();
    assert.ok(h.el('procBody').innerHTML.includes('auto-start-icon'));
    assert.ok(h.el('procBody').innerHTML.includes('&lt;unsafe&gt;'));
    h.ui.state.processes[0].auto_start = false;
    h.ui.renderProcesses();
    assert.ok(!h.el('procBody').innerHTML.includes('auto-start-icon'));
    checks++;
  }
  {
    const h = harness(false); await flush();
    h.ui.openForm('edit', { id: 'proc_abcdef01', cmd: 'echo hello', auto_start: true });
    h.ui.saveForm(true); await flush();
    assert.equal(h.ui.state.unpersisted.proc_abcdef01, true, 'restart hid a persistence failure');
    checks++;
  }
  console.log(`PASS: ${checks} embedded UI automatic-start checks`);
}
main().catch(err => { console.error(err); process.exitCode = 1; });
