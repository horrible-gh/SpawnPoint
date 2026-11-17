'use strict';
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const {test} = require('node:test');
const {Script, createContext, runInContext} = require('node:vm');

const html = readFileSync(join(__dirname, 'access.html'), 'utf8');
const start = html.indexOf('async function request(path,body,method){');
const end = html.indexOf('async function loadIdentity(){', start);
assert.ok(start >= 0 && end > start, 'request() must exist');
const requestSource = html.slice(start, end);

function harness(fetchImpl) {
  const timers = new Map(), failures = [];
  let nextId = 0;
  const connection = {offline: false, recovering: false};
  const context = createContext({
    AbortController,
    fetch: fetchImpl,
    setTimeout: (callback, delay) => {
      const id = ++nextId;
      timers.set(id, {callback, delay});
      return id;
    },
    clearTimeout: id => timers.delete(id),
    app: {csrf: '', ready: true},
    connection,
    noteConnectionFailure: (path, cause) => {
      connection.offline = true;
      failures.push({path, cause});
    },
    noteConnectionSuccess: () => {connection.offline = false;},
  });
  runInContext(requestSource, context);
  return {
    request: (path, body) => context.request(path, body),
    timers, failures, connection,
    timeout() {
      const next = timers.values().next().value;
      assert.ok(next, 'expected a scheduled deadline');
      assert.equal(next.delay, 10000);
      next.callback();
    },
  };
}

test('stalled process-state GET times out and a later read succeeds', async () => {
  let stalled = true;
  const h = harness((_path, options) => stalled
    ? new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted'))))
    : Promise.resolve({status: 200, ok: true, json: async () => ({ok: true})}));
  const pending = h.request('/api/access/state');
  h.timeout();
  await assert.rejects(pending, error => error.network && error.path === '/api/access/state');
  assert.equal(h.failures.length, 1);
  assert.equal(h.timers.size, 0);
  stalled = false;
  await h.request('/api/access/state');
  assert.equal(h.connection.offline, false);
  assert.equal(h.timers.size, 0);
});

test('a stalled identity GET is abortable too', async () => {
  const h = harness((_path, options) =>
    new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted')))));
  const pending = h.request('/auth/me');
  h.timeout();
  await assert.rejects(pending, error => error.network && error.path === '/auth/me');
  assert.equal(h.failures.length, 1);
});

test('a stalled response body does not hold the background refresh lock', async () => {
  const h = harness((_path, options) => Promise.resolve({
    status: 200, ok: true,
    json: () => new Promise((_resolve, reject) => options.signal.addEventListener('abort', () => reject(new Error('body aborted')))),
  }));
  const pending = h.request('/api/access/state');
  await Promise.resolve();
  h.timeout();
  await assert.rejects(pending, error => error.network && error.path === '/api/access/state');
  assert.equal(h.failures.length, 1);
  assert.equal(h.timers.size, 0);
});

test('process-changing POSTs have no timeout or retry', async () => {
  const calls = [];
  const h = harness((path, options) => {
    calls.push({path, options});
    return Promise.resolve({status: 200, ok: true, json: async () => ({ok: true})});
  });
  await h.request('/processes/p/restart', {});
  assert.equal(h.timers.size, 0);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].options.signal, undefined);
  assert.equal(calls[0].options.cache, undefined);
});
