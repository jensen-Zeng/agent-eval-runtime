(() => {
  'use strict';

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const state = {
    initialized: false, user: null, projects: [], project: null, datasets: [], experiments: [], members: [], users: [],
    usage: null, catalog: null, experiment: null, legacy: [], tab: 'projects', pendingSubmission: null,
  };

  let projectGeneration = 0;
  let submissionBusy = false;
  const roleLabels = { owner: 'Owner', executor: 'Executor', reader: 'Reader' };
  const endpointProject = () => state.project ? `/v1/manage/projects/${encodeURIComponent(state.project.id)}` : '';
  const canOwner = () => Boolean(state.project && state.project.role === 'owner');
  const canExecute = () => Boolean(state.project && (state.project.role === 'owner' || state.project.role === 'executor'));
  const isAdmin = () => Boolean(state.user && state.user.admin === true);

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }
  function button(label, className, onClick, title) {
    const node = el('button', className || 'button button-quiet', label);
    node.type = 'button';
    if (title) node.title = title;
    node.addEventListener('click', onClick);
    return node;
  }
  function formatDate(value) {
    if (!value) return '时间未知';
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? String(value) : new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(date);
  }
  function showToast(message, kind = '') {
    const host = $('#toast-region');
    const item = el('div', `toast ${kind}`.trim(), message || '操作完成');
    host.append(item);
    window.setTimeout(() => item.remove(), 5000);
  }
  function feedback(id, message, error = false) {
    const node = $(`#${id}`);
    if (!node) return;
    node.textContent = message || '';
    node.className = `form-feedback${error ? ' error' : ''}`;
  }
  function setBusy(node, busy, label) {
    if (!node) return;
    if (busy) {
      node.dataset.oldText = node.textContent;
      node.disabled = true;
      if (label) node.textContent = label;
    } else {
      node.disabled = false;
      if (node.dataset.oldText !== undefined) node.textContent = node.dataset.oldText;
      delete node.dataset.oldText;
    }
  }
  function authRoute(path) {
    return path === '/v1/manage/auth/state' || path === '/v1/manage/auth/setup' || path === '/v1/manage/auth/login' || path === '/v1/manage/auth/logout';
  }
  function expireSession() {
    projectGeneration++;
    state.user = null;
    state.project = null;
    state.projects = [];
    state.datasets = [];
    state.experiments = [];
    state.members = [];
    state.catalog = null;
    state.users = [];
    state.usage = null;
    state.experiment = null;
    state.legacy = [];
    state.pendingSubmission = null;
    for (const input of $$('input[type="password"]')) input.value = '';
    for (const id of ['experiment-detail', 'dataset-list', 'experiment-list', 'member-list', 'user-list', 'legacy-list', 'usage-grid']) {
      const node = $(`#${id}`); if (node) node.replaceChildren();
    }
    $('#app-shell').classList.add('hidden');
    $('#auth-view').classList.remove('hidden');
    $('#auth-password').value = '';
    setAuthMode(state.initialized);
  }
  async function request(path, options = {}) {
    const method = (options.method || 'GET').toUpperCase();
    const actorID = state.user && state.user.id;
    const targetProject = path.match(/^\/v1\/manage\/projects\/([a-f0-9]{24})(?:\/|$)/);
    const stillCurrent = () => authRoute(path) || (state.user && state.user.id === actorID && (!targetProject || (state.project && state.project.id === targetProject[1])));
    if (!authRoute(path) && !state.user) throw new Error('请先登录。');
    const headers = new Headers(options.headers || {});
    let body = options.body;
    if (body !== undefined && typeof body !== 'string') body = JSON.stringify(body);
    if (body !== undefined) headers.set('Content-Type', 'application/json');
    if (['POST', 'PATCH', 'PUT', 'DELETE'].includes(method)) headers.set('X-AgentEval-CSRF', '1');
    let response;
    try {
      response = await fetch(path, { ...options, method, body, headers, credentials: 'same-origin', cache: 'no-store' });
    } catch (_) {
      throw new Error('无法连接 AgentEval Runtime，请确认服务正在运行。');
    }
    if (!stillCurrent()) throw new Error('账号或项目已切换，已忽略先前请求的响应。');
    if (!response.ok) {
      let message = `请求失败（HTTP ${response.status}）`;
      try {
        const error = await response.json();
        if (typeof error.message === 'string') message = error.message;
        else if (typeof error.error === 'string') message = error.error;
      } catch (_) { /* retain the generic response */ }
      if (response.status === 401 && !authRoute(path)) expireSession();
      throw new Error(message);
    }
    if (response.status === 204) return null;
    const type = response.headers.get('content-type') || '';
    if (!type.includes('application/json')) return null;
    const result = await response.json();
    if (!stillCurrent()) throw new Error('账号或项目已切换，已忽略先前请求的响应。');
    return result;
  }

  function setAuthMode(initialized) {
    state.initialized = initialized === true;
    $('#auth-title').textContent = state.initialized ? '登录项目空间' : '创建首个管理员';
    $('#auth-description').textContent = state.initialized
      ? '登录后继续管理项目、数据集版本和 Agent 评测。'
      : '首次使用需要创建 Runtime 管理员账号。后续账号由管理员创建。';
    $('#auth-submit').textContent = state.initialized ? '登录' : '创建管理员';
    $('#auth-password').autocomplete = state.initialized ? 'current-password' : 'new-password';
    $('#auth-password').minLength = 12;
  }
  async function initialize() {
    $('#auth-view').classList.remove('hidden');
    $('#app-shell').classList.add('hidden');
    try {
      const auth = await request('/v1/manage/auth/state');
      setAuthMode(auth.initialized);
      if (auth.user && auth.user.id) {
        state.user = auth.user;
        await enterApplication();
      }
    } catch (error) {
      setAuthMode(true);
      feedback('auth-error', error.message, true);
    }
  }
  async function submitAuth(event) {
    event.preventDefault();
    const username = $('#auth-username').value.trim();
    const password = $('#auth-password').value;
    if (!username || password.length < 12 || new TextEncoder().encode(password).length > 256) {
      feedback('auth-error', '账号不能为空，密码至少需要 12 个字符且 UTF-8 编码不超过 256 字节。', true);
      return;
    }
    const route = state.initialized ? '/v1/manage/auth/login' : '/v1/manage/auth/setup';
    const submit = $('#auth-submit');
    setBusy(submit, true, state.initialized ? '登录中…' : '创建中…');
    feedback('auth-error', '');
    try {
      const result = await request(route, { method: 'POST', body: { username, password } });
      state.user = result.user;
      $('#auth-password').value = '';
      await enterApplication();
    } catch (error) {
      feedback('auth-error', error.message, true);
    } finally {
      setBusy(submit, false);
    }
  }
  async function logout() {
    try { await request('/v1/manage/auth/logout', { method: 'POST', body: {} }); }
    catch (error) { showToast(error.message, 'error'); }
    state.user = null;
    state.project = null;
    state.pendingSubmission = null;
    expireSession();
  }
  async function enterApplication() {
    if (!state.user || !state.user.id) throw new Error('Runtime 没有返回有效账号。');
    $('#auth-view').classList.add('hidden');
    $('#app-shell').classList.remove('hidden');
    $('#session-user-label').textContent = state.user.username;
    $('#session-role-label').textContent = isAdmin() ? 'Runtime 管理员' : '已认证账号';
    $('#nav-users').classList.toggle('hidden', !isAdmin());
    $('#nav-admin-settings').classList.toggle('hidden', !isAdmin());
    $('#nav-legacy').classList.toggle('hidden', !isAdmin());
    try {
      await loadProjects();
      if (isAdmin()) await loadAdminSettings();
      switchTab('projects');
    } catch (error) {
      showToast(error.message, 'error');
    }
  }

  async function loadProjects(preferredID) {
    const result = await request('/v1/manage/projects');
    state.projects = Array.isArray(result.projects) ? result.projects : [];
    const select = $('#project-select');
    select.replaceChildren();
    const placeholder = el('option', '', state.projects.length ? '选择项目…' : '没有项目');
    placeholder.value = '';
    select.append(placeholder);
    for (const project of state.projects) {
      const option = el('option', '', `${project.name} · ${roleLabels[project.role] || project.role}`);
      option.value = project.id;
      select.append(option);
    }
    let next = preferredID || (state.project && state.projects.some(p => p.id === state.project.id) ? state.project.id : '');
    if (!next && state.projects.length) next = state.projects[0].id;
    select.value = next;
    projectGeneration++;
    state.project = state.projects.find(p => p.id === next) || null;
    if (state.project) {
      restorePendingSubmission();
      await loadProjectData();
    } else {
      state.datasets = [];
      state.experiments = [];
      state.members = [];
      state.usage = null;
      renderProjectEmpty();
    }
    updateRoleUI();
  }
  function renderProjectEmpty() {
    $('#project-title').textContent = '项目管理';
    $('#project-subtitle').textContent = '创建项目后，可为其授权成员、登记数据集版本并提交实验。';
    $('#project-overview').classList.add('hidden');
    $('#empty-project-state').classList.remove('hidden');
    $('#project-dataset-count').textContent = '0';
    $('#dataset-list').replaceChildren(el('div', 'empty-state', '当前没有可访问的项目。'));
    $('#experiment-list').replaceChildren(el('div', 'empty-state', '当前没有可访问的项目。'));
    $('#member-list').replaceChildren(el('div', 'empty-state', '当前没有可访问的项目。'));
  }
  function updateRoleUI() {
    const owner = canOwner();
    const executor = canExecute();
    const role = state.project ? roleLabels[state.project.role] || state.project.role : '未选择项目';
    $('#active-role-badge').textContent = role;
    $('#session-role-label').textContent = role;
    $('#nav-members').classList.toggle('hidden', !owner);
    $('#project-quota-panel').classList.toggle('hidden', !owner);
    $('#create-dataset-panel').classList.toggle('hidden', !owner);
    $('#add-member-panel').classList.toggle('hidden', !owner);
    $('#create-experiment-panel').classList.toggle('hidden', !executor);
    if (state.project) {
      $('#project-overview').classList.remove('hidden');
      $('#empty-project-state').classList.add('hidden');
    }
  }
  async function loadProjectData() {
    if (!state.project) return;
    const generation = projectGeneration;
    const projectID = state.project.id;
    const base = endpointProject();
    const requests = [
      request(base),
      request(`${base}/datasets`),
      request(`${base}/experiments`),
    ];
    requests.push(request(`${base}/members`), request(`${base}/usage`));
    const results = await Promise.all(requests);
    if (generation !== projectGeneration || !state.project || state.project.id !== projectID) return;
    const projectResponse = results[0];
    state.project = projectResponse.project || state.project;
    state.datasets = Array.isArray(results[1].datasets) ? results[1].datasets : [];
    state.experiments = Array.isArray(results[2].experiments) ? results[2].experiments : [];
    state.members = Array.isArray(results[3].members) ? results[3].members : [];
    state.usage = results[4].usage || null;
    renderProjectData();
    updateRoleUI();
    renderExperimentDatasetSelect();
    renderRecipeSelect();
  }
  function renderProjectData() {
    if (!state.project) return renderProjectEmpty();
    $('#project-title').textContent = state.project.name;
    $('#project-subtitle').textContent = `${roleLabels[state.project.role] || state.project.role} · 创建于 ${formatDate(state.project.created_at)}`;
    $('#project-dataset-count').textContent = String(state.datasets.length);
    $('#dataset-version-count').textContent = String(state.datasets.length);
    $('#experiment-count').textContent = String(state.experiments.length);
    $('#member-count').textContent = String(state.members.length);
    $('#project-daily-limit').value = state.project.daily_limit ?? '';
    $('#project-concurrent-limit').value = state.project.concurrent_limit ?? '';
    renderUsage();
    renderDatasets();
    renderExperiments();
    renderMembers();
    renderRecentLists();
  }
  function renderUsage() {
    const host = $('#usage-grid');
    host.replaceChildren();
    if (!state.usage) {
      host.append(el('div', 'empty-state', '用量数据暂不可用。'));
      return;
    }
    const items = [
      ['已使用', state.usage.used], ['预占中', state.usage.reserved], ['已释放', state.usage.released],
      ['当前运行', state.usage.running], ['每日额度', state.usage.daily_limit], ['并发上限', state.usage.concurrent_limit],
    ];
    for (const [label, value] of items) {
      const cell = el('div', 'usage-cell');
      cell.append(el('small', '', label), el('strong', '', value ?? '—'));
      host.append(cell);
    }
  }
  function renderRecentLists() {
    const recentExperiments = $('#recent-experiments');
    recentExperiments.replaceChildren();
    const experiments = state.experiments.slice(0, 5);
    if (!experiments.length) recentExperiments.append(el('div', 'empty-state', '此项目还没有实验。'));
    for (const experiment of experiments) {
      const row = el('button', 'compact-row');
      row.type = 'button';
      row.append(el('strong', '', experiment.name), el('span', 'state-pill state-unknown', experiment.state || '查看详情'));
      row.addEventListener('click', () => openExperiment(experiment.id));
      recentExperiments.append(row);
    }
    const recentDatasets = $('#recent-datasets');
    recentDatasets.replaceChildren();
    const datasets = state.datasets.slice(0, 5);
    if (!datasets.length) recentDatasets.append(el('div', 'empty-state', '此项目还没有数据集版本。'));
    for (const dataset of datasets) {
      recentDatasets.append(el('div', 'compact-row static-row', `${dataset.name} · ${dataset.version}`));
    }
  }

  function renderDatasets() {
    const host = $('#dataset-list');
    host.replaceChildren();
    if (!state.project) return host.append(el('div', 'empty-state', '先选择项目。'));
    if (!state.datasets.length) return host.append(el('div', 'empty-state', '还没有数据集版本。项目 Owner 可以在右侧创建一个。'));
    for (const dataset of state.datasets) {
      const row = el('article', 'record-row');
      const main = el('div', 'record-main');
      main.append(el('strong', 'record-title', dataset.name), el('span', 'record-id', `v${dataset.version}`));
      const tasks = Array.isArray(dataset.tasks) ? dataset.tasks : [];
      main.append(el('div', 'record-meta', `${tasks.length} 个任务 · 创建于 ${formatDate(dataset.created_at)}`));
      row.append(main);
      const actions = el('div', 'record-actions');
      actions.append(button('用于实验', 'button button-quiet', () => {
        switchTab('experiments');
        $('#experiment-dataset').value = dataset.id;
      }));
      row.append(actions);
      host.append(row);
    }
    renderExperimentDatasetSelect();
  }
  function renderExperimentDatasetSelect() {
    const select = $('#experiment-dataset');
    if (!select) return;
    const previous = select.value;
    select.replaceChildren();
    const placeholder = el('option', '', state.datasets.length ? '选择固定的数据集版本…' : '先创建数据集版本');
    placeholder.value = '';
    select.append(placeholder);
    for (const dataset of state.datasets) {
      const option = el('option', '', `${dataset.name} · ${dataset.version}`);
      option.value = dataset.id;
      select.append(option);
    }
    if (state.datasets.some(dataset => dataset.id === previous)) select.value = previous;
  }
  async function loadCatalog() {
    if (!isAdmin()) return;
    try {
      const result = await request('/v1/manage/catalog');
      state.catalog = Array.isArray(result.tasks) ? result.tasks : [];
      renderCatalog();
    } catch (error) {
      $('#catalog-list').replaceChildren(el('div', 'empty-state', error.message));
    }
  }
  function renderCatalog() {
    const box = $('#catalog-box');
    box.classList.toggle('hidden', !isAdmin());
    const host = $('#catalog-list');
    host.replaceChildren();
    if (!state.catalog || !state.catalog.length) {
      host.append(el('div', 'muted-text', '没有已登记任务。'));
      return;
    }
    for (const task of state.catalog) {
      const row = el('div', 'catalog-row');
      const label = el('span', '', `${task.task_id} · r${task.revision}`);
      const add = button('添加', 'text-button', () => {
        const field = $('#dataset-task-ids');
        const values = parseLines(field.value);
        if (!values.includes(task.task_id)) values.push(task.task_id);
        field.value = values.join('\n');
      });
      row.append(label, add);
      host.append(row);
    }
  }
  async function createDataset(event) {
    event.preventDefault();
    if (!canOwner()) return showToast('只有项目 Owner 可以创建数据集版本。', 'error');
    const name = $('#dataset-name').value.trim();
    const version = $('#dataset-version').value.trim();
    const taskIDs = parseLines($('#dataset-task-ids').value);
    if (!name || !version || !taskIDs.length) return feedback('dataset-feedback', '名称、版本和至少一个 task_id 都是必填项。', true);
    const submit = $('#dataset-form button[type="submit"]');
    setBusy(submit, true, '创建中…');
    feedback('dataset-feedback', '');
    try {
      const result = await request(`${endpointProject()}/datasets`, { method: 'POST', body: { name, version, tasks: taskIDs } });
      $('#dataset-form').reset();
      await loadProjectData();
      showToast(`数据集版本 ${result.dataset.name} · ${result.dataset.version} 已创建。`, 'success');
    } catch (error) {
      feedback('dataset-feedback', error.message, true);
    } finally {
      setBusy(submit, false);
    }
  }
  function parseLines(value) {
    return Array.from(new Set(String(value || '').split(/[\r\n,]+/).map(item => item.trim()).filter(Boolean)));
  }

  function addVariant(variant = { agent: 'scripted', script: 'baseline' }) {
    const row = el('div', 'variant-row');
    const agent = document.createElement('select');
    agent.className = 'text-input variant-agent';
    for (const [value, label] of [['scripted', '基线 / Scripted'], ['llm', 'LLM 模型']]) {
      const option = el('option', '', label); option.value = value; agent.append(option);
    }
    agent.value = variant.agent || 'scripted';
    const value = document.createElement('input');
    value.className = 'text-input variant-value';
    value.maxLength = 256;
    value.autocomplete = 'off';
    const remove = button('移除', 'button button-quiet variant-remove', () => row.remove());
    row.append(agent, value, remove);
    $('#variant-list').append(row);
    const sync = () => {
      if (agent.value === 'scripted') {
        value.placeholder = '脚本名称，例如 baseline';
        value.value = variant.agent === 'scripted' && variant.script ? variant.script : 'baseline';
      } else {
        value.placeholder = '模型名称';
        value.value = variant.agent === 'llm' && variant.model ? variant.model : '';
      }
      value.setAttribute('aria-label', agent.value === 'llm' ? '模型名称' : 'Scripted 脚本名称');
    };
    agent.addEventListener('change', () => { variant = { agent: agent.value }; sync(); });
    sync();
    return row;
  }
  function readVariants() {
    const variants = [];
    for (const row of $$('.variant-row', $('#variant-list'))) {
      const agent = $('.variant-agent', row).value;
      const value = $('.variant-value', row).value.trim();
      if (!value) throw new Error(agent === 'llm' ? '模型名称不能为空。' : 'Scripted 脚本名称不能为空。');
      variants.push(agent === 'llm' ? { agent, model: value } : { agent, script: value });
    }
    if (!variants.length) throw new Error('请至少添加一个运行变体。');
    return variants;
  }
  function readExperimentDraft() {
    const name = $('#experiment-name').value.trim();
    const datasetVersionID = $('#experiment-dataset').value;
    const repeats = Number.parseInt($('#experiment-repeats').value, 10);
    if (!name || !datasetVersionID || !Number.isInteger(repeats) || repeats < 1 || repeats > 100) throw new Error('请填写实验名称、数据集版本和有效的重复次数。');
    const tasks = parseLines($('#experiment-tasks').value);
    return { name, dataset_version_id: datasetVersionID, tasks, variants: readVariants(), repeats };
  }
  function pendingStorageKey() {
    return state.user && state.project ? `agenteval.pending.${state.user.id}.${state.project.id}` : '';
  }
  function recipesStorageKey() {
    return state.user && state.project ? `agenteval.recipes.${state.user.id}.${state.project.id}` : '';
  }
  function randomKey() {
    if (window.crypto && crypto.randomUUID) return crypto.randomUUID().replaceAll('-', '');
    const data = new Uint8Array(24);
    crypto.getRandomValues(data);
    return Array.from(data, byte => byte.toString(16).padStart(2, '0')).join('');
  }
  function persistPending() {
    const key = pendingStorageKey();
    if (!key) return;
    try {
      if (state.pendingSubmission) sessionStorage.setItem(key, JSON.stringify(state.pendingSubmission));
      else sessionStorage.removeItem(key);
    } catch (_) { /* sessionStorage may be disabled; in-memory retry remains available */ }
    $('#retry-experiment').classList.toggle('hidden', !state.pendingSubmission);
  }
  function restorePendingSubmission() {
    state.pendingSubmission = null;
    const key = pendingStorageKey();
    if (!key) return;
    try {
      const saved = sessionStorage.getItem(key);
      if (saved) {
        const pending = JSON.parse(saved);
        if (pending && typeof pending.idempotencyKey === 'string' && pending.payload && pending.projectID === state.project.id) state.pendingSubmission = pending;
      }
    } catch (_) { state.pendingSubmission = null; }
  }
  async function submitExperiment(event, retry = false) {
    if (event) event.preventDefault();
    if (submissionBusy) return;
    if (!canExecute()) return showToast('当前角色没有提交实验权限。', 'error');
    let payload;
    try { payload = retry && state.pendingSubmission ? state.pendingSubmission.payload : readExperimentDraft(); }
    catch (error) { feedback('experiment-feedback', error.message, true); return; }
    const serialized = JSON.stringify(payload);
    if (!state.pendingSubmission || state.pendingSubmission.projectID !== state.project.id || state.pendingSubmission.serialized !== serialized) {
      state.pendingSubmission = { projectID: state.project.id, payload, serialized, idempotencyKey: randomKey() };
    }
    persistPending();
    const pending = state.pendingSubmission;
    const generation = projectGeneration;
    submissionBusy = true;
    const submit = retry ? $('#retry-experiment') : $('#submit-experiment');
    setBusy(submit, true, '提交中…');
    feedback('experiment-feedback', '');
    try {
      const result = await request(`${endpointProject()}/experiments`, {
        method: 'POST', headers: { 'Idempotency-Key': pending.idempotencyKey }, body: pending.payload,
      });
      if (generation !== projectGeneration) return;
      const experiment = result.experiment;
      state.pendingSubmission = null;
      persistPending();
      await loadProjectData();
      if (experiment && experiment.id) await openExperiment(experiment.id);
      showToast(result.reused ? '已复用同一幂等请求的实验记录。' : '实验已提交。', 'success');
    } catch (error) {
      if (generation !== projectGeneration) return;
      feedback('experiment-feedback', `${error.message} 可使用“重试上次提交”复用幂等键。`, true);
      persistPending();
    } finally { submissionBusy = false; setBusy(submit, false); }
  }
  function renderExperiments() {
    const host = $('#experiment-list');
    host.replaceChildren();
    if (!state.experiments.length) return host.append(el('div', 'empty-state', '此项目还没有实验记录。'));
    for (const experiment of state.experiments) {
      const row = el('button', 'experiment-record');
      row.type = 'button';
      const info = el('span', 'experiment-record-main');
      info.append(el('strong', '', experiment.name), el('small', '', `${formatDate(experiment.created_at)} · ${experiment.job_count ?? (experiment.jobs ? experiment.jobs.length : '—')} 个作业`));
      row.append(info, el('span', `state-pill ${stateClass(experiment.state)}`, experiment.state || '查看详情'));
      row.addEventListener('click', () => openExperiment(experiment.id));
      host.append(row);
    }
  }
  function stateClass(value) {
    const v = String(value || '').toLowerCase();
    if (v.includes('success') || v.includes('complete') || v.includes('succeed')) return 'state-complete';
    if (v.includes('fail') || v.includes('error')) return 'state-failed';
    if (v.includes('cancel')) return 'state-cancelled';
    if (v.includes('run') || v.includes('queue')) return 'state-running';
    return 'state-unknown';
  }
  async function openExperiment(id) {
    if (!state.project || !id) return;
    const generation = projectGeneration;
    const projectID = state.project.id;
    const userID = state.user && state.user.id;
    switchTab('experiments');
    const host = $('#experiment-detail');
    host.replaceChildren(el('div', 'loading-state', '正在加载实验详情…'));
    try {
      const result = await request(`${endpointProject()}/experiments/${encodeURIComponent(id)}`);
      if (generation !== projectGeneration || !state.project || state.project.id !== projectID || !state.user || state.user.id !== userID) return;
      state.experiment = result.experiment || result;
      renderExperimentDetail(state.experiment);
    } catch (error) {
      if (generation !== projectGeneration) return;
      host.replaceChildren(el('div', 'runs-error', error.message));
    }
  }
  function renderExperimentDetail(experiment) {
    const host = $('#experiment-detail');
    host.replaceChildren();
    const header = el('div', 'detail-header');
    const titleRow = el('div', 'detail-title-row');
    titleRow.append(el('h2', 'detail-title', experiment.name || '实验详情'), el('span', `state-pill ${stateClass(experiment.state)}`, experiment.state || '实验'));
    const meta = el('div', 'detail-meta', `${formatDate(experiment.created_at)} · 数据集版本 ${experiment.dataset_version_id || '未知'}`);
    const actions = el('div', 'detail-actions');
    actions.append(button('导出 JSON', 'button button-quiet', () => downloadJSON(experiment)));
    actions.append(button('导出 CSV', 'button button-quiet', () => downloadCSV(experiment)));
    actions.append(button('复制配置', 'button button-quiet', () => copyExperimentConfig(experiment)));
    if (canExecute() && !isTerminalExperiment(experiment.state)) {
      actions.append(button('取消实验', 'button button-quiet', () => cancelExperiment(experiment.id)));
    }
    header.append(titleRow, meta, actions);
    host.append(header);
    const jobs = Array.isArray(experiment.jobs) ? experiment.jobs : [];
    const summary = el('div', 'record-summary');
    summary.append(summaryCell('作业数', jobs.length), summaryCell('运行变体', uniqueVariants(jobs).length), summaryCell('任务数', uniqueJobTasks(jobs).length), summaryCell('创建者', experiment.created_by || '—'));
    host.append(summary);
    const list = el('div', 'case-list');
    if (!jobs.length) list.append(el('div', 'empty-state', '此实验尚无可显示的作业。'));
    for (const job of jobs) list.append(renderJob(job));
    host.append(list);
  }
  function summaryCell(label, value) {
    const cell = el('div', 'summary-cell');
    cell.append(el('small', '', label), el('strong', '', value));
    return cell;
  }
  function isTerminalExperiment(status) {
    return ['completed', 'succeeded', 'failed', 'canceled', 'cancelled', 'partial'].includes(String(status || '').toLowerCase());
  }
  function jobRequest(job) { return job.request || job; }
  function renderJob(job) {
    const requestData = jobRequest(job);
    const row = el('article', 'case-row');
    const top = el('div', 'case-top');
    const taskID = requestData.task_id || job.task_id || '任务';
    const jobID = job.id || job.job_id || '';
    top.append(el('strong', 'case-name', taskID), el('span', `state-pill ${stateClass(job.status || job.state)}`, job.status || job.state || '未知'));
    row.append(top);
    const model = requestData.model || requestData.script || requestData.agent || '—';
    const usage = job.result && job.result.usage ? job.result.usage : (job.usage || null);
    const usageLabel = usage ? `Token ${usage.total_tokens ?? usage.TotalTokens ?? '未知'}` : 'Token 用量待更新';
    row.append(el('div', 'case-metrics', `作业 ${jobID || '—'} · ${model} · ${usageLabel}`));
    if (job.reason || job.error) row.append(el('div', 'case-error', job.reason || job.error));
    const actions = el('div', 'trace-actions');
    if (jobID && canExecute() && !isTerminalExperiment(job.status)) actions.append(button('取消作业', 'case-detail-button', () => cancelJob(jobID)));
    if (jobID) actions.append(button('查看执行轨迹', 'case-detail-button', () => loadJobEvents(jobID, row)));
    row.append(actions);
    return row;
  }
  async function loadJobEvents(jobID, row) {
    const old = $('.job-events', row);
    if (old) { old.remove(); return; }
    const box = el('pre', 'trace-pre job-events', '正在加载执行轨迹…');
    row.append(box);
    try {
      const page = await request(`${endpointProject()}/jobs/${encodeURIComponent(jobID)}/events?after_seq=0&limit=100`);
      const events = Array.isArray(page.events) ? page.events : [];
      box.textContent = events.length ? events.map(item => typeof item === 'string' ? item : JSON.stringify(item)).join('\n') : '暂无事件记录';
      if (page.has_more) box.textContent += '\n…当前仅显示前 100 条事件';
    } catch (error) { box.textContent = `读取执行轨迹失败：${error.message}`; }
  }
  async function cancelExperiment(id) {
    if (!window.confirm('确定取消此实验中的未完成作业吗？')) return;
    try {
      await request(`${endpointProject()}/experiments/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: {} });
      await loadProjectData();
      await openExperiment(id);
      showToast('实验取消请求已提交。', 'success');
    } catch (error) { showToast(error.message, 'error'); }
  }
  async function cancelJob(id) {
    if (!window.confirm('确定取消此作业吗？')) return;
    try {
      await request(`${endpointProject()}/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: {} });
      await loadProjectData();
      if (state.experiment) await openExperiment(state.experiment.id);
      showToast('作业取消请求已提交。', 'success');
    } catch (error) { showToast(error.message, 'error'); }
  }
  function uniqueJobTasks(jobs) { return Array.from(new Set(jobs.map(job => jobRequest(job).task_id || job.task_id).filter(Boolean))); }
  function uniqueVariants(jobs) {
    return Array.from(new Set(jobs.map(job => {
      const requestData = jobRequest(job);
      return JSON.stringify({ agent: requestData.agent, script: requestData.script, model: requestData.model });
    })));
  }
  function copyExperimentConfig(experiment) {
    const jobs = Array.isArray(experiment.jobs) ? experiment.jobs : [];
    const variants = [];
    const variantKeys = new Set();
    for (const job of jobs) {
      const r = jobRequest(job);
      const variant = r.agent === 'llm' ? { agent: 'llm', model: r.model || '' } : { agent: 'scripted', script: r.script || 'baseline' };
      const key = JSON.stringify(variant);
      if (!variantKeys.has(key)) { variantKeys.add(key); variants.push(variant); }
    }
    const taskIDs = uniqueJobTasks(jobs);
    const inferredRepeats = taskIDs.length && variants.length ? Math.max(1, Math.round(jobs.length / (taskIDs.length * variants.length))) : 1;
    $('#experiment-name').value = `${experiment.name || '实验'}（副本）`;
    $('#experiment-dataset').value = experiment.dataset_version_id || '';
    $('#experiment-tasks').value = taskIDs.join('\n');
    $('#experiment-repeats').value = String(experiment.repeats || inferredRepeats);
    $('#variant-list').replaceChildren();
    for (const variant of variants.length ? variants : [{ agent: 'scripted', script: 'baseline' }]) addVariant(variant);
    showToast('已复制任务、变体和重复次数，请检查后提交。', 'success');
    $('#experiment-name').focus();
    window.scrollTo({ top: 0, behavior: 'smooth' });
  }
  function downloadBlob(name, type, text) {
    const blob = new Blob([text], { type });
    const link = document.createElement('a');
    const url = URL.createObjectURL(blob);
    link.href = url; link.download = name; link.style.display = 'none';
    document.body.append(link); link.click(); link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  function downloadJSON(experiment) {
    downloadBlob(`experiment-${experiment.id || 'detail'}.json`, 'application/json;charset=utf-8', `${JSON.stringify(experiment, null, 2)}\n`);
  }
  function downloadCSV(experiment) {
    const jobs = Array.isArray(experiment.jobs) ? experiment.jobs : [];
    const quote = value => {
      let text = String(value ?? '');
      if (/^[=+\-@]/.test(text)) text = `'${text}`;
      return `"${text.replaceAll('"', '""')}"`;
    };
    const rows = [['job_id', 'task_id', 'agent', 'script', 'model', 'status', 'reason', 'total_tokens']];
    for (const job of jobs) {
      const r = jobRequest(job);
      const usage = job.result && job.result.usage ? job.result.usage : (job.usage || {});
      rows.push([job.id || job.job_id, r.task_id || job.task_id, r.agent, r.script, r.model, job.status || job.state, job.reason || job.error, usage.total_tokens ?? usage.TotalTokens ?? '']);
    }
    downloadBlob(`experiment-${experiment.id || 'detail'}.csv`, 'text/csv;charset=utf-8', `\uFEFF${rows.map(row => row.map(quote).join(',')).join('\r\n')}\r\n`);
  }

  function recipeRead() {
    const key = recipesStorageKey();
    if (!key) return [];
    try {
      const value = JSON.parse(localStorage.getItem(key) || '[]');
      return Array.isArray(value) ? value.filter(item => item && typeof item.name === 'string' && item.payload) : [];
    } catch (_) { return []; }
  }
  function recipeWrite(recipes) {
    const key = recipesStorageKey();
    if (!key) return false;
    try { localStorage.setItem(key, JSON.stringify(recipes.slice(0, 100))); return true; }
    catch (_) { return false; }
  }
  function renderRecipeSelect() {
    const select = $('#recipe-select');
    if (!select) return;
    const current = select.value;
    select.replaceChildren();
    const placeholder = el('option', '', '选择已保存配方…'); placeholder.value = ''; select.append(placeholder);
    for (const [index, recipe] of recipeRead().entries()) {
      const option = el('option', '', recipe.name); option.value = String(index); select.append(option);
    }
    if (Number.isInteger(Number(current)) && current !== '') select.value = current;
  }
  function saveRecipe() {
    let payload;
    try { payload = readExperimentDraft(); }
    catch (error) { feedback('experiment-feedback', error.message, true); return; }
    const name = window.prompt('配方名称', payload.name);
    if (!name || !name.trim()) return;
    const recipes = recipeRead();
    recipes.unshift({ name: name.trim().slice(0, 120), payload });
    if (!recipeWrite(recipes)) return showToast('浏览器无法保存本地实验配方。', 'error');
    renderRecipeSelect();
    showToast('实验配方已保存在此账号与项目的浏览器配置中。', 'success');
  }
  function loadRecipe() {
    const index = Number.parseInt($('#recipe-select').value, 10);
    const recipe = recipeRead()[index];
    if (!recipe) return showToast('请选择一个已保存的配方。', 'error');
    applyExperimentDraft(recipe.payload);
  }
  function deleteRecipe() {
    const index = Number.parseInt($('#recipe-select').value, 10);
    const recipes = recipeRead();
    if (!Number.isInteger(index) || !recipes[index]) return showToast('请选择一个已保存的配方。', 'error');
    recipes.splice(index, 1); recipeWrite(recipes); renderRecipeSelect();
    showToast('配方已删除。', 'success');
  }
  function applyExperimentDraft(payload) {
    $('#experiment-name').value = payload.name || '';
    $('#experiment-dataset').value = payload.dataset_version_id || '';
    $('#experiment-tasks').value = Array.isArray(payload.tasks) ? payload.tasks.join('\n') : '';
    $('#experiment-repeats').value = String(payload.repeats || 1);
    $('#variant-list').replaceChildren();
    for (const variant of payload.variants || []) addVariant(variant);
    if (!payload.variants || !payload.variants.length) addVariant();
    switchTab('experiments');
    $('#experiment-name').focus();
  }

  async function loadMembers() {
    if (!canOwner()) return;
    const result = await request(`${endpointProject()}/members`);
    state.members = Array.isArray(result.members) ? result.members : [];
    renderMembers();
  }
  function renderMembers() {
    const host = $('#member-list');
    host.replaceChildren();
    if (!canOwner()) return host.append(el('div', 'empty-state', '只有项目 Owner 可以查看和管理成员。'));
    if (!state.members.length) return host.append(el('div', 'empty-state', '项目成员列表为空。'));
    for (const member of state.members) {
      const row = el('article', 'record-row member-row');
      const main = el('div', 'record-main');
      main.append(el('strong', 'record-title', member.username), el('span', 'record-id', member.user_id));
      row.append(main);
      const controls = el('div', 'member-controls');
      const select = document.createElement('select'); select.className = 'text-input member-role-select';
      for (const role of ['owner', 'executor', 'reader']) { const opt = el('option', '', roleLabels[role]); opt.value = role; select.append(opt); }
      select.value = member.role;
      controls.append(select);
      const save = button('保存角色', 'button button-quiet', async () => {
        try { await request(`${endpointProject()}/members`, { method: 'PUT', body: { username: member.username, role: select.value } }); await loadMembers(); showToast('成员角色已更新。', 'success'); }
        catch (error) { showToast(error.message, 'error'); }
      });
      controls.append(save);
      controls.append(button('移除', 'button button-quiet', async () => {
        if (!window.confirm(`从项目中移除 ${member.username}？`)) return;
        try { await request(`${endpointProject()}/members/${encodeURIComponent(member.user_id)}`, { method: 'DELETE' }); await loadMembers(); showToast('成员已移除。', 'success'); }
        catch (error) { showToast(error.message, 'error'); }
      }));
      row.append(controls); host.append(row);
    }
  }
  async function addMember(event) {
    event.preventDefault();
    if (!canOwner()) return showToast('只有项目 Owner 可以添加成员。', 'error');
    const username = $('#member-username').value.trim();
    const role = $('#member-role').value;
    const submit = $('#member-form button[type="submit"]');
    setBusy(submit, true, '添加中…'); feedback('member-feedback', '');
    try {
      await request(`${endpointProject()}/members`, { method: 'PUT', body: { username, role } });
      $('#member-form').reset(); await loadMembers(); showToast('项目成员已添加。', 'success');
    } catch (error) { feedback('member-feedback', error.message, true); }
    finally { setBusy(submit, false); }
  }

  async function loadUsers() {
    if (!isAdmin()) return;
    const result = await request('/v1/manage/users');
    state.users = Array.isArray(result.users) ? result.users : [];
    const host = $('#user-list'); host.replaceChildren();
    if (!state.users.length) return host.append(el('div', 'empty-state', '没有账号。'));
    for (const user of state.users) {
      const row = el('article', 'record-row');
      row.append(el('div', 'record-main', `${user.username}${user.admin ? ' · 管理员' : ''}`), el('span', 'record-id', user.id));
      host.append(row);
    }
  }
  async function createUser(event) {
    event.preventDefault();
    if (!isAdmin()) return;
    const username = $('#new-username').value.trim();
    const password = $('#new-user-password').value;
    if (!username || password.length < 12 || new TextEncoder().encode(password).length > 256) return feedback('user-feedback', '账号不能为空，初始密码至少需要 12 个字符且 UTF-8 编码不超过 256 字节。', true);
    const submit = $('#user-form button[type="submit"]'); setBusy(submit, true, '创建中…'); feedback('user-feedback', '');
    try {
      await request('/v1/manage/users', { method: 'POST', body: { username, password } });
      $('#new-user-password').value = ''; $('#user-form').reset(); await loadUsers(); showToast('账号已创建；请再为其添加项目成员关系。', 'success');
    } catch (error) { feedback('user-feedback', error.message, true); }
    finally { $('#new-user-password').value = ''; setBusy(submit, false); }
  }

  async function createProject(event) {
    event.preventDefault();
    const name = $('#new-project-name').value.trim();
    const dailyLimit = Number.parseInt($('#new-project-daily').value, 10);
    const concurrentLimit = Number.parseInt($('#new-project-concurrent').value, 10);
    if (!name || !Number.isInteger(dailyLimit) || dailyLimit < 1 || !Number.isInteger(concurrentLimit) || concurrentLimit < 1) return feedback('create-project-feedback', '请填写名称、正数每日额度和并发数。', true);
    const submit = $('#create-project-form button[type="submit"]'); setBusy(submit, true, '创建中…'); feedback('create-project-feedback', '');
    try {
      const result = await request('/v1/manage/projects', { method: 'POST', body: { name, daily_limit: dailyLimit, concurrent_limit: concurrentLimit } });
      $('#create-project-form').reset(); closeProjectModal(); await loadProjects(result.project && result.project.id);
      showToast('项目已创建，你是该项目的 Owner。', 'success');
    } catch (error) { feedback('create-project-feedback', error.message, true); }
    finally { setBusy(submit, false); }
  }
  function openProjectModal() { $('#create-project-modal').classList.remove('hidden'); $('#new-project-name').focus(); }
  function closeProjectModal() { $('#create-project-modal').classList.add('hidden'); }
  async function updateProjectQuota(event) {
    event.preventDefault();
    if (!canOwner()) return;
    const dailyLimit = Number.parseInt($('#project-daily-limit').value, 10);
    const concurrentLimit = Number.parseInt($('#project-concurrent-limit').value, 10);
    if (!Number.isInteger(dailyLimit) || dailyLimit < 1 || !Number.isInteger(concurrentLimit) || concurrentLimit < 1) return feedback('project-quota-feedback', '请输入有效配额。', true);
    const submit = $('#project-quota-form button[type="submit"]'); setBusy(submit, true, '保存中…'); feedback('project-quota-feedback', '');
    try {
      const result = await request(endpointProject(), { method: 'PATCH', body: { daily_limit: dailyLimit, concurrent_limit: concurrentLimit } });
      state.project = result.project || state.project; await loadProjectData(); feedback('project-quota-feedback', '配额已更新。');
    } catch (error) { feedback('project-quota-feedback', error.message, true); }
    finally { setBusy(submit, false); }
  }

  async function loadAdminSettings() {
    if (!isAdmin()) return;
    try {
      const stateResult = await request('/v1/ui/state');
      const config = stateResult.config || {};
      const provider = config.llm || {};
      $('#base-url').value = provider.base_url || '';
      $('#default-model').value = provider.model || '';
      $('#max-retries').value = String(provider.max_retries ?? 2);
      renderVaultStatus(stateResult.vault || {});
    } catch (error) { showToast(`读取管理员服务设置失败：${error.message}`, 'error'); }
  }
  function renderVaultStatus(vault) {
    const dot = $('#vault-status-dot');
    dot.className = `vault-indicator ${vault.has_key ? (vault.unlocked ? 'ok' : 'warn') : ''}`;
    $('#vault-status-title').textContent = !vault.exists ? '尚未建立密钥库' : (vault.unlocked ? '密钥库已解锁' : '密钥库已锁定');
    $('#vault-status-text').textContent = vault.has_key ? (vault.unlocked ? '模型密钥由服务进程暂存于内存。' : '已有已加密的模型密钥，需要管理员解锁。') : '尚无可用模型密钥。';
    $('#vault-lock-icon').textContent = vault.unlocked ? '◉' : '⌑';
    $('#unlock-vault').disabled = !vault.exists || vault.unlocked;
    $('#lock-vault').disabled = !vault.unlocked;
  }
  async function saveProvider(event) {
    event.preventDefault();
    if (!isAdmin()) return;
    const payload = { base_url: $('#base-url').value.trim(), model: $('#default-model').value.trim(), max_retries: Number.parseInt($('#max-retries').value, 10) };
    const submit = $('#provider-form button[type="submit"]'); setBusy(submit, true, '保存中…'); feedback('provider-feedback', '');
    try { await request('/v1/ui/settings', { method: 'POST', body: payload }); feedback('provider-feedback', '共享模型设置已保存。'); showToast('管理员模型设置已保存。', 'success'); }
    catch (error) { feedback('provider-feedback', error.message, true); }
    finally { setBusy(submit, false); }
  }
  async function vaultForm(event, route, data, successText, clearIDs) {
    event.preventDefault();
    if (!isAdmin()) return;
    const submit = event.currentTarget.querySelector('button[type="submit"]'); setBusy(submit, true, '处理中…');
    try {
      const result = await request(route, { method: 'POST', body: data });
      if (result && result.vault) renderVaultStatus(result.vault);
      showToast(successText, 'success');
      if (route.endsWith('/save')) await loadAdminSettings();
    } catch (error) { showToast(error.message, 'error'); }
    finally {
      for (const id of clearIDs) $(`#${id}`).value = '';
      setBusy(submit, false);
    }
  }
  async function lockVault() {
    if (!isAdmin()) return;
    const buttonNode = $('#lock-vault'); setBusy(buttonNode, true, '锁定中…');
    try { const result = await request('/v1/ui/vault/lock', { method: 'POST', body: {} }); if (result && result.vault) renderVaultStatus(result.vault); showToast('共享密钥库已锁定。', 'success'); }
    catch (error) { showToast(error.message, 'error'); }
    finally { setBusy(buttonNode, false); }
  }

  async function loadLegacy() {
    if (!isAdmin()) return;
    const host = $('#legacy-list'); host.replaceChildren(el('div', 'loading-state', '正在读取旧版记录…'));
    try {
      const result = await request('/v1/ui/experiments');
      state.legacy = Array.isArray(result.experiments) ? result.experiments : [];
      host.replaceChildren();
      if (!state.legacy.length) return host.append(el('div', 'empty-state', '没有旧版实验记录。'));
      for (const experiment of state.legacy) {
        const row = el('button', 'experiment-record'); row.type = 'button';
        row.append(el('span', 'experiment-record-main', `${experiment.title || experiment.id} · ${experiment.state || '未知'}`), el('span', 'record-meta', formatDate(experiment.created_at)));
        row.addEventListener('click', () => openLegacy(experiment.id)); host.append(row);
      }
    } catch (error) { host.replaceChildren(el('div', 'runs-error', error.message)); }
  }
  async function openLegacy(id) {
    if (!isAdmin()) return;
    try {
      const result = await request(`/v1/ui/experiments/${encodeURIComponent(id)}`);
      $('#legacy-detail-content').textContent = JSON.stringify(result, null, 2);
      $('#legacy-detail').classList.remove('hidden');
    } catch (error) { showToast(error.message, 'error'); }
  }

  function switchTab(tab) {
    const needsProject = ['datasets', 'experiments', 'members'].includes(tab);
    if (needsProject && !state.project) { showToast('请先创建或选择一个项目。', 'error'); tab = 'projects'; }
    if (tab === 'members' && !canOwner()) { showToast('只有项目 Owner 可以管理成员。', 'error'); tab = 'projects'; }
    if (tab === 'datasets' && !state.project) tab = 'projects';
    if (tab === 'experiments' && !state.project) tab = 'projects';
    if (['users', 'admin-settings', 'legacy'].includes(tab) && !isAdmin()) { showToast('此页面仅管理员可用。', 'error'); tab = 'projects'; }
    state.tab = tab;
    $$('.nav-item').forEach(node => node.classList.toggle('active', node.dataset.tab === tab));
    $$('.page').forEach(node => node.classList.toggle('active', node.id === `page-${tab}`));
    const label = { projects: '项目概览', datasets: '数据集版本', experiments: '实验', members: '项目成员', users: '账号管理', 'admin-settings': '管理员设置', legacy: '旧版记录' }[tab] || '项目概览';
    $('#page-crumb').textContent = label;
    if (tab === 'datasets' && isAdmin() && !state.catalog) loadCatalog();
    if (tab === 'members' && canOwner()) loadMembers().catch(error => showToast(error.message, 'error'));
    if (tab === 'users' && isAdmin()) loadUsers().catch(error => showToast(error.message, 'error'));
    if (tab === 'admin-settings' && isAdmin()) loadAdminSettings();
    if (tab === 'legacy' && isAdmin()) loadLegacy();
  }
  async function refreshCurrent() {
    if (!state.user) return initialize();
    try {
      if (state.tab === 'users') await loadUsers();
      else if (state.tab === 'admin-settings') await loadAdminSettings();
      else if (state.tab === 'legacy') await loadLegacy();
      else if (state.project) await loadProjectData();
      else await loadProjects();
      showToast('已刷新。', 'success');
    } catch (error) { showToast(error.message, 'error'); }
  }
  async function reloadDatasets() { try { await loadProjectData(); if (isAdmin()) await loadCatalog(); } catch (error) { showToast(error.message, 'error'); } }

  function bind() {
    $('#auth-form').addEventListener('submit', submitAuth);
    $('#logout-button').addEventListener('click', logout);
    $('#project-select').addEventListener('change', async event => {
      const project = state.projects.find(item => item.id === event.target.value) || null;
      const generation = ++projectGeneration;
      state.project = project;
      state.experiment = null;
      $('#experiment-detail').replaceChildren(el('div', 'empty-detail', '选择实验以查看详情。'));
      restorePendingSubmission();
      if (project) {
        try { await loadProjectData(); if (generation !== projectGeneration || !state.project || state.project.id !== project.id) return; updateRoleUI(); renderRecipeSelect(); switchTab('projects'); }
        catch (error) { showToast(error.message, 'error'); }
      } else renderProjectEmpty();
    });
    $('#open-create-project').addEventListener('click', openProjectModal);
    $('#close-create-project').addEventListener('click', closeProjectModal);
    $('#create-project-modal').addEventListener('click', event => { if (event.target === event.currentTarget) closeProjectModal(); });
    $('#create-project-form').addEventListener('submit', createProject);
    $('#refresh-button').addEventListener('click', refreshCurrent);
    $('#refresh-usage').addEventListener('click', async () => { if (!canOwner()) return; try { const result = await request(`${endpointProject()}/usage`); state.usage = result.usage; renderUsage(); } catch (error) { showToast(error.message, 'error'); } });
    $('#project-quota-form').addEventListener('submit', updateProjectQuota);
    $('#dataset-form').addEventListener('submit', createDataset);
    $('#reload-datasets').addEventListener('click', reloadDatasets);
    $('#experiment-form').addEventListener('submit', event => submitExperiment(event));
    $('#retry-experiment').addEventListener('click', () => submitExperiment(null, true));
    $('#add-variant').addEventListener('click', () => addVariant({ agent: 'llm', model: '' }));
    $('#save-recipe').addEventListener('click', saveRecipe);
    $('#load-recipe').addEventListener('click', loadRecipe);
    $('#delete-recipe').addEventListener('click', deleteRecipe);
    $('#reload-experiments').addEventListener('click', async () => { try { await loadProjectData(); } catch (error) { showToast(error.message, 'error'); } });
    $('#member-form').addEventListener('submit', addMember);
    $('#reload-members').addEventListener('click', () => loadMembers().catch(error => showToast(error.message, 'error')));
    $('#user-form').addEventListener('submit', createUser);
    $('#reload-users').addEventListener('click', () => loadUsers().catch(error => showToast(error.message, 'error')));
    $('#provider-form').addEventListener('submit', saveProvider);
    $('#vault-save-form').addEventListener('submit', event => vaultForm(event, '/v1/ui/vault/save', { password: $('#vault-password').value, api_key: $('#api-key-input').value }, 'API Key 已加密保存。', ['vault-password', 'api-key-input']));
    $('#vault-unlock-form').addEventListener('submit', event => vaultForm(event, '/v1/ui/vault/unlock', { password: $('#unlock-password').value }, '密钥库已解锁。', ['unlock-password']));
    $('#lock-vault').addEventListener('click', lockVault);
    $('#reload-legacy').addEventListener('click', loadLegacy);
    $('#close-legacy-detail').addEventListener('click', () => $('#legacy-detail').classList.add('hidden'));
    $$('.nav-item').forEach(node => node.addEventListener('click', () => switchTab(node.dataset.tab)));
    $$('[data-open-tab]').forEach(node => node.addEventListener('click', () => switchTab(node.dataset.openTab)));
  }

  bind();
  addVariant({ agent: 'scripted', script: 'baseline' });
  initialize();
})();
