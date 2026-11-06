'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const { webcrypto } = require('node:crypto');

class Node {
  constructor() {
    this.children = []; this.value = ''; this.textContent = ''; this.className = '';
    this.dataset = {}; this.listeners = {}; this.attributes = {}; this.disabled = false;
    this.classList = { add: (...xs) => this._classes(xs, true), remove: (...xs) => this._classes(xs, false),
      toggle: (x, force) => { const yes = force === undefined ? !this.has(x) : force; this._classes([x], yes); return yes; },
      contains: x => this.has(x) };
  }
  _classes(xs, add) { const set = new Set(this.className.split(/\s+/).filter(Boolean)); for (const x of xs) add ? set.add(x) : set.delete(x); this.className = [...set].join(' '); }
  has(x) { return this.className.split(/\s+/).includes(x); }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  setAttribute(k, v) { this.attributes[k] = v; }
  querySelector(selector) { return this.children.find(n => selector.startsWith('.') && n.className.split(/\s+/).includes(selector.slice(1))) || null; }
  querySelectorAll(selector) { return this.children.filter(n => selector.startsWith('.') && n.className.split(/\s+/).includes(selector.slice(1))); }
  remove() { this.removed = true; }
}

function harness(fetchImpl = async () => response({})) {
  const nodes = new Map();
  const password = new Node();
  const variantRow = new Node(); variantRow.className = 'variant-row';
  const agent = new Node(); agent.className = 'variant-agent'; agent.value = 'scripted';
  const variant = new Node(); variant.className = 'variant-value'; variant.value = 'baseline';
  variantRow.append(agent, variant);
  variantRow.querySelector = selector => selector === '.variant-agent' ? agent : selector === '.variant-value' ? variant : null;
  const document = {
    querySelector(selector) { if (selector === '#variant-list') return this.node(selector); return this.node(selector); },
    querySelectorAll(selector) { if (selector === 'input[type="password"]') return [password]; if (selector === '.variant-row') return [variantRow]; return []; },
    createElement() { return new Node(); }, node(selector) { if (!nodes.has(selector)) nodes.set(selector, new Node()); return nodes.get(selector); },
  };
  document.node('#variant-list').append(variantRow);
  const window = { crypto: webcrypto, setTimeout, confirm: () => true };
  const sourcePath = path.join(__dirname, '..', 'internal', 'webui', 'assets', 'manage.js');
  let source = fs.readFileSync(sourcePath, 'utf8');
  const boot = '  bind();\n  addVariant({ agent: \'scripted\', script: \'baseline\' });\n  initialize();';
  assert.ok(source.includes(boot), 'manage.js boot block changed; update the test harness deliberately');
  source = source.replace(boot, `  globalThis.__ui = { state, openExperiment, loadProjectData, submitExperiment, expireSession,
    setGeneration: value => { projectGeneration = value; }, getGeneration: () => projectGeneration };`);
  const context = { document, window, fetch: fetchImpl, Headers, URL, URLSearchParams, Blob, TextEncoder, crypto: webcrypto, console,
    setTimeout, clearTimeout, sessionStorage: storage(), localStorage: storage() };
  vm.runInNewContext(source, context, { filename: sourcePath });
  return { ui: context.__ui, document, password, nodes };
}
function storage() { const data = new Map(); return { setItem: (k,v) => data.set(k,String(v)), getItem: k => data.get(k) ?? null, removeItem: k => data.delete(k), clear: () => data.clear() }; }
function response(body, status = 200) { return { ok: status >= 200 && status < 300, status, headers: { get: () => 'application/json' }, json: async () => body }; }
const project = id => ({ id, name: id, role: 'owner', daily_limit: 50, concurrent_limit: 2, created_at: '2026-01-01T00:00:00Z' });

test('openExperiment renders details through the real request and renderer', async () => {
  const h = harness(async () => response({ experiment: { id: 'e', name: 'demo', state: 'running', jobs: [] } }));
  h.ui.state.user = { id: 'user-a', username: 'a' }; h.ui.state.project = project('aaaaaaaaaaaaaaaaaaaaaaaa');
  await h.ui.openExperiment('e');
  assert.equal(h.ui.state.experiment.name, 'demo');
  assert.match(h.document.node('#experiment-detail').children[0].children[0].children[0].textContent, /demo/);
});

test('completed jobs and experiments hide cancellation controls', async () => {
  for (const status of ['QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELED', 'TIMED_OUT', 'INFRA_ERROR']) {
    const h = harness(async () => response({ experiment: { id: 'e', name: 'demo', state: status, jobs: [{ id: 'job', status }] } }));
    h.ui.state.user = { id: 'user-a', username: 'a' };
    h.ui.state.project = project('aaaaaaaaaaaaaaaaaaaaaaaa');
    await h.ui.openExperiment('e');
    const detail = h.document.node('#experiment-detail');
    const experimentActions = detail.children[0].children[2].children.map(node => node.textContent);
    const jobActions = detail.children[2].children[0].children[2].children.map(node => node.textContent);
    const canCancel = status === 'QUEUED' || status === 'RUNNING';
    assert.equal(experimentActions.includes('取消实验'), canCancel, `experiment ${status}`);
    assert.equal(jobActions.includes('取消作业'), canCancel, `job ${status}`);
    if (status === 'TIMED_OUT' || status === 'INFRA_ERROR') {
      assert.match(detail.children[0].children[0].children[1].className, /state-failed/);
      assert.match(detail.children[2].children[0].children[0].children[1].className, /state-failed/);
    }
  }
});

test('experiment details keep the latest selection when responses arrive out of order', async t => {
  for (const scenario of [
    { name: 'late success', oldID: 'old', newID: 'new', status: 200 },
    { name: 'late error', oldID: 'old', newID: 'new', status: 503 },
    { name: 'overlapping refresh', oldID: 'same', newID: 'same', status: 200 },
  ]) {
    await t.test(scenario.name, async () => {
      let releaseOld;
      const oldWait = new Promise(resolve => { releaseOld = resolve; });
      let requests = 0;
      const h = harness(async () => {
        if (++requests === 1) {
          await oldWait;
          return response({ experiment: { id: scenario.oldID, name: 'old result', jobs: [] }, message: 'outdated error' }, scenario.status);
        }
        return response({ experiment: { id: scenario.newID, name: 'new result', jobs: [] } });
      });
      h.ui.state.user = { id: 'user-a', username: 'a' };
      h.ui.state.project = project('aaaaaaaaaaaaaaaaaaaaaaaa');
      const oldLoad = h.ui.openExperiment(scenario.oldID);
      await h.ui.openExperiment(scenario.newID);
      releaseOld();
      await oldLoad;
      assert.equal(h.ui.state.experiment.name, 'new result');
      assert.equal(h.document.node('#experiment-detail').children[0].children[0].children[0].textContent, 'new result');
    });
  }
});

test('late response from an old project cannot replace current data', async () => {
  let releaseOld;
  const oldWait = new Promise(resolve => { releaseOld = resolve; });
  const h = harness(async url => {
    if (url.startsWith('/v1/manage/projects/aaaaaaaa')) { await oldWait; return response(url.endsWith('/datasets') ? { datasets: [{ name: 'old' }] } : url.endsWith('/experiments') ? { experiments: [] } : url.endsWith('/members') ? { members: [] } : url.endsWith('/usage') ? { usage: {} } : { project: project('aaaaaaaaaaaaaaaaaaaaaaaa') }); }
    return response(url.endsWith('/datasets') ? { datasets: [{ name: 'new' }] } : url.endsWith('/experiments') ? { experiments: [] } : url.endsWith('/members') ? { members: [] } : url.endsWith('/usage') ? { usage: {} } : { project: project('bbbbbbbbbbbbbbbbbbbbbbbb') });
  });
  h.ui.state.user = { id: 'user-a', username: 'a' }; h.ui.state.project = project('aaaaaaaaaaaaaaaaaaaaaaaa');
  const oldLoad = h.ui.loadProjectData();
  h.ui.setGeneration(h.ui.getGeneration() + 1); h.ui.state.project = project('bbbbbbbbbbbbbbbbbbbbbbbb');
  await h.ui.loadProjectData(); releaseOld(); await oldLoad.catch(() => {});
  assert.equal(h.ui.state.project.id, 'bbbbbbbbbbbbbbbbbbbbbbbb');
  assert.equal(h.ui.state.datasets[0].name, 'new');
});

test('expireSession clears sensitive inputs and account-scoped state', () => {
  const h = harness(); const s = h.ui.state;
  Object.assign(s, { initialized: true, user: { id: 'u' }, project: project('aaaaaaaaaaaaaaaaaaaaaaaa'), projects: [project('aaaaaaaaaaaaaaaaaaaaaaaa')],
    datasets: [{}], experiments: [{}], members: [{}], users: [{}], usage: {}, catalog: {}, experiment: {}, legacy: [{}], pendingSubmission: { idempotencyKey: 'secret-free' } });
  h.password.value = 'do-not-retain';
  h.ui.expireSession();
  assert.equal(s.user, null); assert.equal(s.project, null); assert.deepEqual(Array.from(s.projects), []);
  assert.equal(s.catalog, null); assert.equal(s.pendingSubmission, null); assert.equal(h.password.value, '');
  assert.ok(h.document.node('#app-shell').classList.contains('hidden'));
});

test('duplicate submit is suppressed and retry reuses the original idempotency key', async () => {
  const calls = []; let release;
  const h = harness(async (url, options) => { calls.push({ url, options }); await new Promise(resolve => { release = resolve; }); return response({ message: 'temporary outage' }, 503); });
  const s = h.ui.state; s.user = { id: 'user-a', username: 'a' }; s.project = project('aaaaaaaaaaaaaaaaaaaaaaaa');
  h.document.node('#experiment-name').value = 'demo'; h.document.node('#experiment-dataset').value = 'dataset'; h.document.node('#experiment-repeats').value = '1'; h.document.node('#experiment-tasks').value = '';
  const first = h.ui.submitExperiment(null, false); await new Promise(resolve => setImmediate(resolve));
  await h.ui.submitExperiment(null, false); assert.equal(calls.length, 1);
  const key = calls[0].options.headers.get('Idempotency-Key'); release(); await first;
  assert.ok(s.pendingSubmission); assert.equal(s.pendingSubmission.idempotencyKey, key);
  const retry = h.ui.submitExperiment(null, true); await new Promise(resolve => setImmediate(resolve));
  assert.equal(calls.length, 2); assert.equal(calls[1].options.headers.get('Idempotency-Key'), key); release(); await retry;
  assert.ok(s.pendingSubmission);
});
