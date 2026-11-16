'use strict';
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const {test} = require('node:test');
const vm = require('node:vm');

const html = readFileSync(join(__dirname, 'access.html'), 'utf8');
function section(start, end) {
  const from = html.indexOf(start);
  const to = html.indexOf(end, from + start.length);
  assert.ok(from >= 0 && to > from, `Missing source section: ${start}`);
  return html.slice(from, to);
}
const source = [
  section('const processFeedback=new Map();', 'function setLogDockRatio('),
  section('async function loadState(){', 'async function boot(){'),
  section('async function processAction(action,id){', 'const current=()=>'),
  section('let refreshing=false;', 'setInterval(()=>{void refreshBackground();},4000);'),
].join('\n');
function stateResponse(status = 'running') {
  return {
    revision: 2, actor: 'user',
    db: {
      users: [{id: 'user'}], userGroups: [],
      processGroups: [{id: 'group'}],
      processes: [{id: 'p', status}],
      settings: {guest: false},
    },
  };
}
function networkError() {
  return Object.assign(new Error('offline'), {network: true});
}
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => {resolve = yes; reject = no;});
  return {promise, resolve, reject};
}
function harness(handler) {
  const calls = [], errors = [], toasts = [];
  let confirmation = null, renders = 0;
  const ctx = vm.createContext({
    console,
    db: stateResponse().db,
    state: {actor: 'user', route: '/processes', policyGroup: 'group', policyTarget: 'group', effectiveUser: 'user', pgFilter: 'all'},
    app: {ready: true, revision: 1},
    connection: {offline: false, recovering: false, retries: 0, retryAt: 0},
    document: {hidden: false, activeElement: null},
    processOrderBusy: false, processDragId: null,
    $: () => ({open: false}),
    badge: (text, color) => `<span class="${color}">${text}</span>`,
    current: () => ({id: 'user'}),
    access: () => ({run: true}),
    proc: id => ctx.db.processes.find(p => p.id === id),
    render: () => {renders++;},
    toast: text => toasts.push(text),
    handleError: error => errors.push(error),
    pollLog: async () => {},
    loadIdentity: async () => ({user: {id: 'user'}}),
    noteConnectionSuccess: () => {ctx.connection.offline = false;},
    ask: (_title, _body, _label, perform) => {confirmation = perform;},
    request: async (path, body) => {
      calls.push({path, body});
      if (handler) return handler(path, body, ctx);
      if (path === '/api/access/state') return stateResponse();
      return {process: {status: 'running'}};
    },
  });
  vm.runInContext(source, ctx);
  const feedback = vm.runInContext('processFeedback', ctx);
  return {
    ctx, feedback, calls, errors, toasts,
    renders: () => renders,
    async action(action = 'restart') {
      await ctx.processAction(action, 'p');
      if (confirmation) {
        const perform = confirmation;
        confirmation = null;
        await perform();
      }
    },
  };
}

test('all inline UI scripts parse', () => {
  const scripts = [...html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)];
  assert.ok(scripts.length > 0);
  for (const [, script] of scripts) new vm.Script(script);
});

test('unknown actions show a neutral, localized state check instead of a user confirmation', () => {
  const h = harness();
  for (const label of ['실행', '중지', '재시작']) {
    h.feedback.set('p', {label, status: 'unknown'});
    const text = h.ctx.processActionFeedback({id: 'p'});
    assert.match(text, /상태 확인 중/);
    assert.match(text, /class="blue"/);
    assert.doesNotMatch(text, /확인 필요|실패/);
  }
  assert.match(html, /'상태 확인 중':\['Checking status…','状態確認中'\]/);
});

test('a fresh successful state read clears uncertain feedback and renders the current state', async () => {
  const h = harness(() => stateResponse('stopped'));
  h.feedback.set('p', {label: '중지', status: 'unknown'});
  await h.ctx.refresh();
  assert.equal(h.feedback.size, 0);
  assert.equal(h.ctx.db.processes[0].status, 'stopped');
  assert.equal(h.renders(), 1);
  assert.deepEqual(h.toasts, []); // A state read is not proof that a lost command succeeded.
});

test('reconciliation keeps known failures and still-running requests', async () => {
  const h = harness();
  const pending = {label: '실행', status: 'pending'};
  const error = {label: '중지', status: 'error'};
  h.feedback.set('p', {label: '재시작', status: 'unknown'});
  h.feedback.set('pending', pending);
  h.feedback.set('failed', error);
  await h.ctx.loadState();
  assert.equal(h.feedback.has('p'), false);
  assert.equal(h.feedback.get('pending'), pending);
  assert.equal(h.feedback.get('failed'), error);
});

test('failed state reads do not clear uncertainty', async () => {
  for (const error of [networkError(), Object.assign(new Error('HTTP 503'), {status: 503})]) {
    const h = harness(() => {throw error;});
    const feedback = {label: '중지', status: 'unknown'};
    h.feedback.set('p', feedback);
    await assert.rejects(h.ctx.loadState());
    assert.equal(h.feedback.get('p'), feedback);
  }
});

test('an old in-flight read cannot clear feedback created after the read began', async () => {
  const read = deferred();
  const h = harness(() => read.promise);
  const loading = h.ctx.loadState();
  const feedback = {label: '재시작', status: 'unknown'};
  h.feedback.set('p', feedback);
  read.resolve(stateResponse());
  await loading;
  assert.equal(h.feedback.get('p'), feedback);
  await h.ctx.loadState();
  assert.equal(h.feedback.has('p'), false);
});

test('a delayed read cannot erase newer feedback for the same process', async () => {
  for (const status of ['unknown', 'pending', 'error']) {
    const read = deferred();
    const h = harness(() => read.promise);
    h.feedback.set('p', {label: '중지', status: 'unknown'});
    const loading = h.ctx.loadState();
    const newer = {label: '재시작', status};
    h.feedback.set('p', newer);
    read.resolve(stateResponse());
    await loading;
    assert.equal(h.feedback.get('p'), newer);
  }
});

test('removed or no-longer-visible processes do not retain old uncertain feedback', async () => {
  const data = stateResponse();
  data.db.processes = [];
  const h = harness(() => data);
  h.feedback.set('p', {label: '중지', status: 'unknown'});
  await h.ctx.loadState();
  assert.equal(h.feedback.size, 0);
});

for (const action of ['run', 'stop', 'restart']) {
  test(`${action}: a lost command response is reconciled by GET only, without replay or a success claim`, async () => {
    const h = harness(path => {
      if (path === '/api/access/state') return stateResponse();
      throw networkError();
    });
    await h.action(action);
    assert.equal(h.feedback.get('p').status, 'unknown');
    await h.ctx.loadState();
    assert.equal(h.feedback.has('p'), false);
    assert.equal(h.calls.filter(c => c.body !== undefined).length, 1);
    assert.equal(h.calls.filter(c => c.path === '/api/access/state').length, 1);
    assert.deepEqual(h.toasts, []);
  });
}

test('successful command followed by a failed refresh is not labeled as a failed command', async () => {
  for (const error of [networkError(), Object.assign(new Error('HTTP 503'), {status: 503})]) {
    let failRead = true;
    const h = harness(path => {
      if (path !== '/api/access/state') return {process: {status: 'running'}};
      if (failRead) throw error;
      return stateResponse();
    });
    await h.action();
    assert.equal(h.feedback.get('p').status, 'unknown');
    assert.deepEqual(h.toasts, []);
    failRead = false;
    await h.ctx.loadState();
    assert.equal(h.feedback.has('p'), false);
    assert.equal(h.calls.filter(c => c.body !== undefined).length, 1);
  }
});

test('known process failure is checked before a subsequent read could hide it', async () => {
  const h = harness(path => {
    if (path === '/api/access/state') throw networkError();
    return {process: {status: 'failed', error: 'spawn failed'}};
  });
  await h.action('run');
  assert.equal(h.feedback.get('p').status, 'error');
  assert.equal(h.calls.length, 1);
  assert.equal(h.errors[0].message, 'spawn failed');
  assert.deepEqual(h.toasts, []);
});

test('explicit command rejection remains a failure after a later state read', async () => {
  const h = harness(path => {
    if (path === '/api/access/state') return stateResponse();
    throw Object.assign(new Error('Forbidden'), {status: 403});
  });
  await h.action('stop');
  const failure = h.feedback.get('p');
  assert.equal(failure.status, 'error');
  await h.ctx.loadState();
  assert.equal(h.feedback.get('p'), failure);
});

test('pending requests still prevent duplicate control commands', async () => {
  const command = deferred();
  const h = harness(path => path === '/api/access/state' ? stateResponse() : command.promise);
  const first = h.action('run');
  await h.action('run');
  assert.equal(h.calls.filter(c => c.body !== undefined).length, 1);
  command.resolve({process: {status: 'running'}});
  await first;
  assert.equal(h.feedback.size, 0);
  assert.equal(h.toasts.length, 1);
});

test('automatic reconnection clears feedback only after the process state is fetched', async () => {
  let stateAvailable = false;
  const h = harness(() => {
    if (!stateAvailable) throw networkError();
    return stateResponse('stopped');
  });
  h.ctx.connection.offline = true;
  const feedback = {label: '중지', status: 'unknown'};
  h.feedback.set('p', feedback);
  await h.ctx.refreshBackground(true);
  assert.equal(h.feedback.get('p'), feedback);
  assert.equal(h.ctx.connection.offline, true);
  stateAvailable = true;
  await h.ctx.refreshBackground(true);
  assert.equal(h.feedback.size, 0);
  assert.equal(h.ctx.connection.offline, false);
  assert.equal(h.ctx.db.processes[0].status, 'stopped');
  assert.equal(h.renders(), 1);
  assert.ok(h.calls.every(c => c.body === undefined));
});

test('offline reconnection is not blocked by a focused input', async () => {
  const h = harness(() => stateResponse('stopped'));
  h.ctx.connection.offline = true;
  h.ctx.document.activeElement = {matches: () => true};
  await h.ctx.refreshBackground();
  assert.equal(h.ctx.connection.offline, false);
  assert.equal(h.ctx.db.processes[0].status, 'stopped');
  assert.equal(h.renders(), 0, 'recovery must not discard the active editor');
  assert.equal(h.calls.length, 1);
  h.ctx.document.activeElement = null;
  await h.ctx.refreshBackground();
  assert.equal(h.renders(), 1, 'normal rendering resumes after editing');
});

test('offline reconnection is not blocked by an open modal', async () => {
  const h = harness(() => stateResponse('stopped'));
  h.ctx.connection.offline = true;
  h.ctx.$ = selector => ({open: selector === '#modal'});
  await h.ctx.refreshBackground();
  assert.equal(h.ctx.connection.offline, false);
  assert.equal(h.renders(), 0, 'the open modal must not be replaced');
  assert.equal(h.ctx.db.processes[0].status, 'stopped');
});

test('manual connection recovery ignores the retry backoff', async () => {
  const h = harness(() => stateResponse('stopped'));
  h.ctx.connection.offline = true;
  h.ctx.connection.retryAt = Date.now() + 60_000;
  await h.ctx.refreshBackground(true);
  assert.equal(h.calls.length, 1);
  assert.equal(h.ctx.connection.offline, false);
});

test('reconnect backoff stays under twelve seconds after repeated failures', async () => {
  const h = harness(() => {throw networkError();});
  h.ctx.connection.offline = true;
  h.ctx.connection.retries = 5;
  const start = Date.now();
  await h.ctx.refreshBackground(true);
  assert.equal(h.ctx.connection.offline, true);
  assert.ok(h.ctx.connection.retryAt >= start + 11_900);
  assert.ok(h.ctx.connection.retryAt <= Date.now() + 12_100);
});

test('process commands are not sent while offline', async () => {
  const h = harness();
  h.ctx.connection.offline = true;
  await h.action('restart');
  assert.equal(h.calls.length, 0);
  assert.equal(h.feedback.size, 0);
});

test('connection status offers an explicit retry without refreshing the page', () => {
  assert.match(html, /data-act="retry-connection"/);
  assert.match(html, /case 'retry-connection':connection\.retryAt=0;await refreshBackground\(true\)/);
});
