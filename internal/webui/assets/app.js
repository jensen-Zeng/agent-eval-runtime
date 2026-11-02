(() => {
  'use strict';
  const $ = (s, root = document) => root.querySelector(s);
  const $$ = (s, root = document) => Array.from(root.querySelectorAll(s));
  const state = { tasks: [], presets: [], experiments: [], config: null, vault: null, selectedTasks: new Set(), currentExperiment: null, detailRenderedFor: null, detailFingerprint: '', tab: 'workspace', busy: new Set(), timer: null, detailSeq: 0, modelInitialized: false };

  async function request(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    let response;
    try { response = await fetch(path, { ...options, headers, credentials: 'same-origin', cache: 'no-store' }); }
    catch (_) { throw new Error('无法连接本机服务，请确认 AgentEval Runtime 正在运行。'); }
    if (!response.ok) {
      if (options.secretSafe) throw new Error(options.safeMessage || '操作失败，请检查输入后重试。');
      let message = `请求失败（HTTP ${response.status}）`;
      try { const data = await response.json(); if (typeof data.error === 'string') message = data.error.slice(0, 500); else if (typeof data.message === 'string') message = data.message.slice(0, 500); }
      catch (_) { /* Keep the generic status message. */ }
      throw new Error(message);
    }
    if (options.raw) return response;
    if (response.status === 204) return null;
    const type = response.headers.get('content-type') || '';
    if (!type.includes('application/json')) return null;
    return response.json();
  }
  function jsonOptions(method, value, extra = {}) { return { method, body: JSON.stringify(value), ...extra }; }
  function toast(message, kind = '', duration = 4200) {
    const node = document.createElement('div'); node.className = `toast ${kind}`.trim(); node.textContent = String(message || '操作完成'); $('#toast-region').append(node);
    window.setTimeout(() => node.remove(), duration);
  }
  function setBusy(button, key, busy, label) {
    const labelNode = $('[data-button-label]', button) || button;
    if (busy) { state.busy.add(key); button.dataset.originalText = labelNode.textContent; button.disabled = true; if (label) labelNode.textContent = label; }
    else { state.busy.delete(key); button.disabled = false; if (button.dataset.originalText !== undefined) labelNode.textContent = button.dataset.originalText; delete button.dataset.originalText; }
  }
  function safeText(value, fallback = '—') { return value === undefined || value === null || value === '' ? fallback : String(value); }
  function fmtDate(value) { if (!value) return '时间未知'; const d = new Date(value); return Number.isNaN(d.valueOf()) ? String(value) : new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(d); }
  function fmtDuration(v) { const n = Number(v); if (!Number.isFinite(n)) return '—'; if (n < 1000) return `${Math.max(0, Math.round(n))} ms`; const sec = n / 1000; return sec < 60 ? `${sec.toFixed(1)} 秒` : `${Math.floor(sec / 60)} 分 ${Math.round(sec % 60)} 秒`; }
  function fmtBytes(v) { let n = Number(v); if (!Number.isFinite(n) || n < 0) return '—'; if (n < 1024) return `${Math.round(n)} B`; n /= 1024; if (n < 1024) return `${n.toFixed(1)} KiB`; n /= 1024; if (n < 1024) return `${n.toFixed(1)} MiB`; return `${(n / 1024).toFixed(1)} GiB`; }
  function displayBudget(task) {
    const b = task.budget || {}; const l = task.limits || {};
    const timeout = b.run_seconds ?? b.timeout_seconds ?? b.timeout ?? l.timeout_seconds ?? l.timeout;
    const memory = l.memory_mib ?? b.memory_mb ?? b.memory_mb_limit ?? l.memory_mb ?? l.memory_mb_limit;
    return [timeout ? `${timeout}s` : '', memory ? `${memory} MiB` : ''].filter(Boolean).join(' · ') || '使用任务默认预算';
  }
  function sourceOf(task) { const m = task.metadata || {}; return m.dataset || m.source || m.package || m.source_type || '本地任务'; }
  function taskTitle(task) { return task.title || task.name || task.task_id || '未命名任务'; }
  function taskSearchText(task) { return [taskTitle(task), task.task_id, task.backend || 'go', sourceOf(task), task.metadata && Object.values(task.metadata).join(' ')].join(' ').toLowerCase(); }

  function renderTasks() {
    const host = $('#task-list'); host.replaceChildren(); const query = $('#task-search').value.trim().toLowerCase();
    const visible = state.tasks.filter(t => !query || taskSearchText(t).includes(query));
    $('#task-total').textContent = String(state.tasks.length);
    $('#selection-count').textContent = `已选 ${state.selectedTasks.size} 项`;
    if (!state.tasks.length) { emptyNode(host, '当前没有已登记任务', '先使用数据集导入流程登记任务，然后刷新列表。'); return; }
    if (!visible.length) { emptyNode(host, '没有匹配任务', '试试缩短搜索词，或清空搜索条件。'); return; }
    visible.forEach(task => {
      const row = document.createElement('article'); row.className = `task-card${state.selectedTasks.has(task.task_id) ? ' selected' : ''}`;
      const check = document.createElement('input'); check.type = 'checkbox'; check.className = 'task-check'; check.checked = state.selectedTasks.has(task.task_id); check.setAttribute('aria-label', `选择 ${taskTitle(task)}`);
      check.addEventListener('change', () => { check.checked ? state.selectedTasks.add(task.task_id) : state.selectedTasks.delete(task.task_id); renderTasks(); });
      const main = document.createElement('div'); main.className = 'task-main';
      const line = document.createElement('div'); line.className = 'task-name-line';
      const name = document.createElement('span'); name.className = 'task-name'; name.textContent = taskTitle(task); line.append(name);
      const id = document.createElement('span'); id.className = 'task-id'; id.textContent = safeText(task.task_id); line.append(id); main.append(line);
      const meta = document.createElement('div'); meta.className = 'task-meta';
      const src = document.createElement('span'); src.className = 'source-chip'; src.textContent = sourceOf(task); meta.append(src);
      const backend = document.createElement('span'); backend.className = 'language-chip'; backend.textContent = task.backend === 'harbor' ? 'Harbor · 文件交付' : 'Go · 代码修复'; meta.append(backend);
      const language = (task.metadata && (task.metadata.language || task.metadata.lang)) || task.language;
      if (language) { const lang = document.createElement('span'); lang.className = 'language-chip'; lang.textContent = String(language); meta.append(lang); }
      if (task.revision) { const rev = document.createElement('span'); rev.textContent = `版本 ${String(task.revision).slice(0, 16)}`; meta.append(rev); }
      main.append(meta);
      if (task.prompt) {
        const open = document.createElement('button'); open.className = 'task-open'; open.type = 'button'; open.textContent = 'ⓘ'; open.title = '查看任务说明'; open.setAttribute('aria-expanded', 'false');
        const prompt = document.createElement('p'); prompt.className = 'task-prompt hidden'; prompt.textContent = task.prompt;
        open.addEventListener('click', () => { const hidden = prompt.classList.toggle('hidden'); open.setAttribute('aria-expanded', String(!hidden)); });
        row.append(check, main, open, prompt);
      } else row.append(check, main);
      const budget = document.createElement('div'); budget.className = 'task-budget'; const budgetLabel = document.createElement('strong'); budgetLabel.textContent = '预算'; const budgetValue = document.createElement('span'); budgetValue.textContent = displayBudget(task); budget.append(budgetLabel, budgetValue);
      main.append(budget); host.append(row);
    });
  }
  function emptyNode(host, title, detail) { const box = document.createElement('div'); box.className = 'empty-state'; const wrap = document.createElement('div'); const h = document.createElement('strong'); h.textContent = title; h.style.display = 'block'; const p = document.createElement('p'); p.textContent = detail; p.style.margin = '5px 0 0'; wrap.append(h, p); box.append(wrap); host.append(box); }

  function modelInputs() { return $$('.model-row input').map(i => i.value.trim()).filter(Boolean); }
  function addModel(value = '') {
    const host = $('#model-list'); const row = document.createElement('div'); row.className = 'model-row'; const input = document.createElement('input'); input.type = 'text'; input.maxLength = 200; input.placeholder = '模型名称，例如 gpt-4.1-mini'; input.value = value; input.autocomplete = 'off'; input.setAttribute('aria-label', '评测模型名称');
    const remove = document.createElement('button'); remove.type = 'button'; remove.className = 'model-remove'; remove.textContent = '×'; remove.title = '移除此模型'; remove.setAttribute('aria-label', '移除此模型'); remove.addEventListener('click', () => { if ($$('.model-row').length <= 1) { input.value = ''; input.focus(); return; } row.remove(); }); row.append(input, remove); host.append(row);
  }
  function getPlan() {
    const title = $('#experiment-title').value.trim(); const tasks = Array.from(state.selectedTasks); const models = modelInputs(); const repeats = Math.max(1, Math.min(100, Number.parseInt($('#repeat-count').value, 10) || 1));
    if (!title) throw new Error('请填写实验方案名称。'); if (!tasks.length) throw new Error('请至少选择一个任务。'); if (models.some(m => !m)) throw new Error('模型名称不能为空。'); if (models.length > 32) throw new Error('一次最多配置 32 个模型。'); if (!$('#baseline-toggle').checked && !models.length) throw new Error('请添加模型，或开启基线运行。');
    const unique = new Set(models.map(m => m.toLowerCase())); if (unique.size !== models.length) throw new Error('模型列表中有重复名称，请检查后再保存。');
    return { title, tasks, models, repeats, baseline: $('#baseline-toggle').checked };
  }
  async function loadState() {
    try {
      const data = await request('/v1/ui/state'); state.tasks = Array.isArray(data.tasks) ? data.tasks : []; state.config = data.config || {}; state.vault = data.vault || {};
      if (!state.modelInitialized) { const initial = $('.model-row input'); if (initial && !initial.value.trim() && state.config.llm && state.config.llm.model) initial.value = state.config.llm.model; state.modelInitialized = true; }
      const selectedExisting = new Set(state.tasks.map(t => t.task_id)); state.selectedTasks = new Set(Array.from(state.selectedTasks).filter(id => selectedExisting.has(id)));
      renderTasks(); renderService(data); renderVault(); renderSettings();
    } catch (e) { $('#task-list').replaceChildren(); emptyNode($('#task-list'), '无法读取 Runtime 状态', e.message); setService(false, '服务未连接', '请启动 AgentEval Runtime 后重试。'); }
  }
  function renderService(data) {
    const s = data.summary || {}; const running = Number(s.running || 0), queued = Number(s.queued || 0), workers = Number(s.worker_limit || 0);
    setService(true, '服务已连接', `${running} 运行中 · ${queued} 排队${workers ? ` · ${workers} workers` : ''}`);
  }
  function setService(ok, label, sub) {
    for (const id of ['service-dot', 'top-dot']) { const el = $(`#${id}`); el.className = `status-dot ${ok ? 'ok' : 'error'}`; }
    $('#service-label').textContent = label; $('#service-sub').textContent = sub; $('#top-status').textContent = ok ? 'Runtime 在线' : 'Runtime 离线';
  }
  function renderVault() {
    const v = state.vault || {}; const exists = Boolean(v.exists), unlocked = Boolean(v.unlocked), hasKey = Boolean(v.has_key);
    const stateText = !exists ? '尚未保存 API Key' : unlocked ? (hasKey ? '密钥库已解锁 · Key 可用' : '密钥库已解锁 · 尚无 Key') : '密钥库已锁定';
    const dotClass = unlocked && hasKey ? 'ok' : exists ? 'warn' : '';
    $('#vault-summary').textContent = stateText; $('#vault-dot').className = `vault-indicator ${dotClass}`;
    $('#vault-status-title').textContent = stateText; $('#vault-status-text').textContent = exists ? '密钥只在 Runtime 解锁后暂存在进程内存中。' : '设置主密码并保存 API Key，即可启用真实模型运行。';
    $('#vault-status-dot').className = `vault-indicator ${dotClass}`; $('#vault-lock-icon').textContent = unlocked ? '⌑' : '⊙';
    $('#vault-password-label').textContent = exists ? '当前主密码' : '设置主密码';
    $('#vault-password').placeholder = exists ? '输入当前主密码以更新密钥' : '至少 12 个字符';
    $('#unlock-vault').disabled = !exists || unlocked; $('#lock-vault').disabled = !unlocked;
  }
  function renderSettings() {
    const llm = state.config && state.config.llm || {}; if (document.activeElement !== $('#base-url')) $('#base-url').value = llm.base_url || '';
    if (document.activeElement !== $('#default-model')) $('#default-model').value = llm.model || '';
    if (document.activeElement !== $('#max-retries')) $('#max-retries').value = Number.isFinite(Number(llm.max_retries)) ? llm.max_retries : 2;
  }
  async function loadPresets() {
    try { const data = await request('/v1/ui/presets'); state.presets = Array.isArray(data.presets) ? data.presets : []; renderPresets(); }
    catch (e) { toast(`读取已保存方案失败：${e.message}`, 'error'); }
  }
  function renderPresets() {
    const select = $('#preset-select'); select.replaceChildren(); const placeholder = document.createElement('option'); placeholder.value = ''; placeholder.textContent = '选择已保存方案…'; select.append(placeholder);
    state.presets.forEach(p => { const o = document.createElement('option'); o.value = p.id; o.textContent = p.title; select.append(o); });
  }
  async function savePreset() {
    const button = $('#save-preset'); if (state.busy.has('preset')) return; let plan; try { plan = getPlan(); } catch (e) { toast(e.message, 'error'); return; }
    setBusy(button, 'preset', true, '保存中…');
    try { const data = await request('/v1/ui/presets', jsonOptions('POST', plan)); await loadPresets(); if (data && data.preset) $('#preset-select').value = data.preset.id; toast('实验方案已保存。', 'success'); }
    catch (e) { toast(`保存方案失败：${e.message}`, 'error'); }
    finally { setBusy(button, 'preset', false); }
  }
  function loadPreset() {
    const preset = state.presets.find(p => p.id === $('#preset-select').value); if (!preset) { toast('请先选择一个已保存方案。', 'error'); return; }
    $('#experiment-title').value = preset.title || ''; state.selectedTasks = new Set(Array.isArray(preset.tasks) ? preset.tasks : []); $('#model-list').replaceChildren(); (preset.models || []).forEach(m => addModel(m)); if (!preset.models || !preset.models.length) addModel();
    $('#repeat-count').value = Math.max(1, Math.min(100, Number(preset.repeats) || 1)); $('#baseline-toggle').checked = Boolean(preset.baseline); renderTasks(); toast(`已载入方案“${preset.title}”。`, 'success');
  }
  async function deletePreset() {
    const id = $('#preset-select').value; if (!id) { toast('请先选择要删除的方案。', 'error'); return; }
    const p = state.presets.find(x => x.id === id); if (!window.confirm(`删除方案“${p ? p.title : id}”？此操作无法撤销。`)) return;
    const button = $('#delete-preset'); if (state.busy.has('preset')) return; setBusy(button, 'preset', true, '…');
    try { await request(`/v1/ui/presets/${encodeURIComponent(id)}`, { method: 'DELETE' }); await loadPresets(); toast('方案已删除。', 'success'); }
    catch (e) { toast(`删除方案失败：${e.message}`, 'error'); }
    finally { setBusy(button, 'preset', false); }
  }
  function cryptoRequestID() {
    if (!globalThis.crypto || !crypto.randomUUID) throw new Error('当前浏览器无法安全生成请求编号，请更新浏览器后重试。');
    return crypto.randomUUID().replaceAll('-', '').slice(0, 24);
  }
  async function startExperiment() {
    const button = $('#start-experiment'); if (state.busy.has('start')) return; let plan; try { plan = getPlan(); } catch (e) { toast(e.message, 'error'); return; }
    if (plan.models.length && (!state.vault || !state.vault.unlocked || !state.vault.has_key)) { toast('要运行模型，请先在“连接设置”保存并解锁 API Key。模型调用可能产生服务商费用。', 'error', 6500); switchTab('settings'); return; }
    setBusy(button, 'start', true, '正在创建实验…');
    try {
      const selectedPreset = state.presets.find(p => p.id === $('#preset-select').value);
      const sameSet = (a, b) => { if (!Array.isArray(a) || a.length !== b.length) return false; const left = [...a].sort(), right = [...b].sort(); return left.every((v, i) => v === right[i]); };
      const canReuse = selectedPreset && selectedPreset.title === plan.title && sameSet(selectedPreset.tasks, plan.tasks) && Array.isArray(selectedPreset.models) && selectedPreset.models.length === plan.models.length && selectedPreset.models.every((v, i) => v === plan.models[i]) && Number(selectedPreset.repeats) === plan.repeats && Boolean(selectedPreset.baseline) === plan.baseline;
      let preset = canReuse ? selectedPreset : null;
      if (!preset) { const presetResp = await request('/v1/ui/presets', jsonOptions('POST', plan)); preset = presetResp && presetResp.preset; }
      if (!preset || !preset.id) throw new Error('服务未返回保存的实验方案编号。');
      if (!canReuse) await loadPresets(); $('#preset-select').value = preset.id;
      const data = await request('/v1/ui/experiments', jsonOptions('POST', { preset_id: preset.id, request_id: cryptoRequestID() }));
      if (!data || !data.experiment) throw new Error('服务未返回实验记录。');
      toast(`实验“${safeText(data.experiment.title, plan.title)}”已提交。`, 'success'); await loadExperiments(); switchTab('runs'); await openExperiment(data.experiment.id);
    } catch (e) { toast(`启动实验失败：${e.message}`, 'error', 6500); }
    finally { setBusy(button, 'start', false); }
  }

  function normalizedStatus(s) {
    const v = String(s || '').toLowerCase(); if (['queued', 'pending', 'created'].includes(v)) return ['排队中', 'state-queued']; if (['running', 'active'].includes(v)) return ['运行中', 'state-running'];
    if (['completed', 'complete', 'succeeded', 'success', 'done'].includes(v)) return ['已完成', 'state-complete']; if (['failed', 'error', 'timed_out', 'infra_error'].includes(v)) return ['失败', 'state-failed']; if (['partial', 'incomplete', 'needs_attention'].includes(v)) return [v === 'needs_attention' ? '需要处理' : '部分完成', 'state-partial']; if (['cancelled', 'canceled'].includes(v)) return ['已取消', 'state-cancelled']; if (['paused', 'interrupted'].includes(v)) return [v === 'interrupted' ? '已中断' : '已暂停', 'state-paused']; return [safeText(s, '状态未知'), 'state-unknown'];
  }
  function isTerminalJobStatus(status) { return ['SUCCEEDED', 'FAILED', 'TIMED_OUT', 'CANCELED', 'CANCELLED', 'INFRA_ERROR'].includes(String(status || '').toUpperCase()); }
  function badge(status) { const [label, css] = normalizedStatus(status); const n = document.createElement('span'); n.className = `state-pill ${css}`; n.textContent = label; return n; }
  async function loadExperiments() {
    try { const data = await request('/v1/ui/experiments'); state.experiments = Array.isArray(data.experiments) ? data.experiments : []; renderExperiments(); }
    catch (e) { $('#experiment-list').replaceChildren(); errorNode($('#experiment-list'), `读取实验记录失败：${e.message}`); }
  }
  function renderExperiments() {
    const host = $('#experiment-list'); host.replaceChildren(); $('#history-count').textContent = `${state.experiments.length} 项`; $('#run-count').textContent = String(state.experiments.length);
    if (!state.experiments.length) { emptyNode(host, '还没有实验记录', '回到实验台选择任务并启动一次运行。'); return; }
    state.experiments.forEach(exp => {
      const item = document.createElement('article'); item.className = `experiment-item${state.currentExperiment === exp.id ? ' active' : ''}`; item.tabIndex = 0; item.setAttribute('role', 'button'); item.setAttribute('aria-label', `打开实验 ${safeText(exp.title)}`);
      const title = document.createElement('div'); title.className = 'experiment-title'; title.textContent = safeText(exp.title, '未命名实验'); const meta = document.createElement('div'); meta.className = 'experiment-meta'; meta.append(badge(exp.state)); const time = document.createElement('span'); time.textContent = fmtDate(exp.created_at); meta.append(time); if (exp.error) { const err = document.createElement('span'); err.textContent = '有错误信息'; meta.append(err); } item.append(title, meta);
      item.addEventListener('click', () => openExperiment(exp.id)); item.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openExperiment(exp.id); } }); host.append(item);
    });
  }
  function errorNode(host, text) { const e = document.createElement('div'); e.className = 'runs-error'; e.textContent = text; host.append(e); }
  function appendButton(parent, text, className, fn) { const b = document.createElement('button'); b.type = 'button'; b.className = `button ${className}`; b.textContent = text; b.addEventListener('click', fn); parent.append(b); return b; }
  async function openExperiment(id) {
    if (!id) return; const preserveDetail = state.currentExperiment === id && state.detailRenderedFor === id; state.currentExperiment = id; renderExperiments(); const seq = ++state.detailSeq; const host = $('#experiment-detail');
    if (!preserveDetail) { host.replaceChildren(); const loading = document.createElement('div'); loading.className = 'loading-state'; const spin = document.createElement('span'); spin.className = 'spinner'; loading.append(spin, document.createTextNode('正在读取实验明细…')); host.append(loading); }
    try { const data = await request(`/v1/ui/experiments/${encodeURIComponent(id)}`); if (seq !== state.detailSeq) return; const warning = $('.detail-refresh-error', host); if (warning) warning.remove(); const fingerprint = JSON.stringify(data); if (preserveDetail && fingerprint === state.detailFingerprint) return; renderExperimentDetail(data); state.detailRenderedFor = id; state.detailFingerprint = fingerprint; }
    catch (e) { if (seq !== state.detailSeq) return; if (preserveDetail) { if (!$('.detail-refresh-error', host)) { const warning = document.createElement('div'); warning.className = 'runs-error detail-refresh-error'; warning.textContent = `刷新明细失败：${e.message}`; host.prepend(warning); } return; } host.replaceChildren(); errorNode(host, `读取实验明细失败：${e.message}`); }
  }
  function entriesOf(manifest) { return Array.isArray(manifest && manifest.entries) ? manifest.entries : []; }
  function summaryValue(summary, keys, fallback = 0) { for (const key of keys) if (summary && summary[key] !== undefined && summary[key] !== null) return summary[key]; return fallback; }
  function renderExperimentDetail(payload) {
    const host = $('#experiment-detail'); host.replaceChildren(); const exp = payload.experiment || {}; const manifest = payload.manifest || {}; const entries = entriesOf(manifest); const header = document.createElement('div'); header.className = 'detail-header';
    const titleRow = document.createElement('div'); titleRow.className = 'detail-title-row'; const title = document.createElement('h2'); title.className = 'detail-title'; title.textContent = safeText(exp.title, '实验明细'); titleRow.append(title, badge(exp.state)); header.append(titleRow);
    const meta = document.createElement('div'); meta.className = 'detail-meta'; const created = document.createElement('span'); created.textContent = fmtDate(exp.created_at); meta.append(created); if (exp.id) { const eid = document.createElement('span'); eid.textContent = `ID ${String(exp.id).slice(0, 32)}`; meta.append(eid); } header.append(meta);
    const actions = document.createElement('div'); actions.className = 'detail-actions';
    if (['failed', 'partial', 'paused', 'interrupted', 'needs_attention'].includes(String(exp.state || '').toLowerCase()) || (exp.state === 'completed' && !manifest.complete)) { let b; b = appendButton(actions, '恢复未完成', 'button-quiet', () => experimentAction(exp.id, 'resume', b)); }
    if (['queued', 'running'].includes(String(exp.state || '').toLowerCase())) { let b; b = appendButton(actions, '取消实验', 'button-quiet', () => experimentAction(exp.id, 'cancel', b)); }
    appendLink(actions, '下载 JSON', `/v1/ui/experiments/${encodeURIComponent(exp.id)}/json`); appendLink(actions, '下载 CSV', `/v1/ui/experiments/${encodeURIComponent(exp.id)}/csv`);
    const exportButton = appendButton(actions, '导出证据', 'button-quiet', () => exportExperiment(exp.id, actions)); exportButton.disabled = !manifest.complete; if (!manifest.complete) exportButton.title = '完成实验后可导出'; header.append(actions); host.append(header);
    const summary = manifest.summary || {}; const total = entries.length; const finished = entries.filter(e => isTerminalJobStatus(e.job && e.job.status) || e.stats != null || e.error).length; const meanScore = entries.map(e => e.stats && e.stats.score).filter(v => v !== null && v !== undefined && Number.isFinite(Number(v))).map(Number); const avg = meanScore.length ? (meanScore.reduce((a,b)=>a+b,0)/meanScore.length).toFixed(2) : '—';
    const cells = [['任务运行', `${finished} / ${total}`], ['平均得分', avg], ['执行失败', summaryValue(summary, ['failed', 'failures', 'infra_errors'])], ['实验状态', String(exp.state || '').toLowerCase() === 'needs_attention' ? '需检查' : String(exp.state || '').toLowerCase() === 'interrupted' ? '已中断' : ['canceled', 'cancelled'].includes(String(exp.state || '').toLowerCase()) ? '已取消' : String(exp.state || '').toLowerCase() === 'running' ? '进行中' : manifest.complete ? '完成' : '等待中']]; const sumNode = document.createElement('div'); sumNode.className = 'record-summary'; cells.forEach(([label,value]) => { const c=document.createElement('div');c.className='summary-cell';const s=document.createElement('small');s.textContent=label;const v=document.createElement('strong');v.textContent=String(value);c.append(s,v);sumNode.append(c); }); host.append(sumNode);
    const list = document.createElement('div'); list.className = 'case-list';
    if (!entries.length) emptyNode(list, '实验尚无明细', '任务开始后，评分与执行结果会出现在这里。');
    entries.forEach(entry => list.append(renderCase(entry)));
    host.append(list); if (exp.error) { const error = document.createElement('div'); error.className = 'runs-error'; error.textContent = exp.error; host.append(error); }
  }
  function appendLink(parent, text, href) { const a = document.createElement('a'); a.className = 'button button-quiet'; a.textContent = text; a.href = href; a.download = ''; parent.append(a); return a; }
  function renderCase(entry) {
    const row = document.createElement('div'); row.className = 'case-row'; const top = document.createElement('div'); top.className = 'case-top'; const name = document.createElement('span'); name.className = 'case-name'; const c = entry.case && typeof entry.case === 'object' ? entry.case : {}; const isBaseline = c.agent === 'scripted' && c.script === 'baseline'; const actor = isBaseline ? '基线' : (c.model || c.agent || ''); const repeat = Number(c.repeat) > 0 ? ` · 第 ${c.repeat} 次` : ''; name.textContent = c.task_id ? `${c.task_id}${actor ? ` · ${actor}` : ''}${repeat}` : safeText(entry.case, '未命名任务'); const jobStatus = entry.job && entry.job.status; const status = entry.error ? 'failed' : jobStatus || (entry.stats != null ? 'completed' : 'queued'); top.append(name, badge(status)); row.append(top);
    const metrics = document.createElement('div'); metrics.className = 'case-metrics'; const stats = entry.stats || {}; const resources = stats.resources || {}; const evalResources = resources.evaluation || {}; const evalSamplePeakBytes = evalResources.memory_peak_sampled_bytes; for (const [label, value] of [['得分', stats.score === undefined || stats.score === null ? '—' : `${(Number(stats.score) * 100).toFixed(Number(stats.score) <= 1 ? 0 : 1)}%`], ['耗时', fmtDuration(stats.wall_ms)], ['评测内存采样峰值', fmtBytes(evalSamplePeakBytes)], ['基线', isBaseline ? '是' : '']]) { if (value === '') continue; const item = document.createElement('span'); item.textContent = `${label} ${value}`; if (label === '评测内存采样峰值') item.title = '来源：stats.resources.evaluation.memory_peak_sampled_bytes。该值是评测容器采样数据，不代表整个任务的真实总内存峰值。'; metrics.append(item); } row.append(metrics);
    if (entry.error) { const e = document.createElement('div'); e.className = 'case-error'; e.textContent = entry.error; row.append(e); }
    const jobID = typeof entry.job === 'string' ? entry.job : (entry.job && (entry.job.id || entry.job.job_id));
    if (jobID) { const jid = document.createElement('div'); jid.className = 'case-job'; jid.textContent = `Job ${jobID}`; row.append(jid); const btn = document.createElement('button'); btn.className = 'case-detail-button'; btn.type = 'button'; btn.textContent = '查看报告与执行轨迹'; btn.addEventListener('click', () => loadRunEvidence(jobID, row, btn)); row.append(btn); }
    return row;
  }
  async function loadRunEvidence(jobID, parent, button) {
    if (button.disabled) return;
    const existing = $('.trace-box', parent);
    if (existing) { existing.remove(); button.textContent = '查看报告与执行轨迹'; return; }
    button.disabled = true; button.textContent = '读取中…'; const box = document.createElement('div'); box.className = 'trace-box'; parent.append(box);
    try {
      const report = await request(`/v1/runs/${encodeURIComponent(jobID)}`);
      const actions = document.createElement('div'); actions.className = 'trace-actions'; const reportLink = appendLink(actions, '下载运行报告', `/v1/runs/${encodeURIComponent(jobID)}`); reportLink.removeAttribute('download'); reportLink.target = '_blank'; reportLink.rel = 'noopener noreferrer'; box.append(actions);
      const reportHeading = document.createElement('div'); reportHeading.className = 'case-name'; reportHeading.textContent = '运行报告'; box.append(reportHeading);
      const reportPre = document.createElement('pre'); reportPre.textContent = JSON.stringify(report, null, 2); box.append(reportPre);
      const eventHeading = document.createElement('div'); eventHeading.className = 'case-name'; eventHeading.textContent = '执行轨迹（每页最多100条）'; box.append(eventHeading);
      const pager = document.createElement('div'); pager.className = 'trace-actions';
      const previous = document.createElement('button'); previous.type = 'button'; previous.className = 'button button-quiet'; previous.textContent = '上一页';
      const pageLabel = document.createElement('span'); pageLabel.className = 'case-job';
      const next = document.createElement('button'); next.type = 'button'; next.className = 'button button-quiet'; next.textContent = '下一页';
      const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'button button-quiet'; retry.textContent = '重试';
      pager.append(previous, pageLabel, next, retry); box.append(pager);
      const eventsPre = document.createElement('pre'); eventsPre.textContent = '正在读取执行轨迹…'; box.append(eventsPre);
      const pages = { cursors: [0], data: [], index: 0, pending: 0, loading: false, failed: false };
      const updatePager = () => {
        previous.disabled = pages.loading || pages.index <= 0;
        next.disabled = pages.loading || pages.failed || pages.index >= 99 || !pages.data[pages.index] || !pages.data[pages.index].has_more || pages.cursors[pages.index + 1] === undefined && !(Number.isFinite(Number(pages.data[pages.index].next_sequence)) && Number(pages.data[pages.index].next_sequence) > pages.cursors[pages.index]);
        retry.disabled = pages.loading || !pages.failed;
        pageLabel.textContent = pages.failed ? `第 ${pages.pending + 1} 页读取失败（最多100页）` : `第 ${pages.index + 1} 页 / 最多100页`;
      };
      const loadPage = async index => {
        if (pages.loading || index < 0 || index > 99) return;
        let cursor = pages.cursors[index];
        if (cursor === undefined && index === pages.index + 1) {
          const current = pages.data[pages.index]; const seq = Number(current && current.next_sequence);
          if (!current || !current.has_more || !Number.isFinite(seq) || seq <= pages.cursors[pages.index]) return;
          cursor = seq; pages.cursors[index] = seq;
        }
        if (cursor === undefined) return;
        pages.loading = true; pages.failed = false; pages.pending = index; previous.disabled = true; next.disabled = true; retry.disabled = true; pageLabel.textContent = `正在读取第 ${index + 1} 页…`;
        if (index !== pages.index || !pages.data[index]) eventsPre.textContent = '正在读取执行轨迹…';
        try {
          const data = await request(`/v1/runs/${encodeURIComponent(jobID)}/events?after_seq=${encodeURIComponent(cursor)}&limit=100`);
          if (!data || typeof data !== 'object') throw new Error('invalid page');
          const rows = Array.isArray(data) ? data : (Array.isArray(data.events) ? data.events : Array.isArray(data.items) ? data.items : []);
          pages.data[index] = Array.isArray(data) ? { events: rows, has_more: false } : data; pages.index = index; pages.failed = false;
          eventsPre.textContent = rows.length ? rows.slice(0, 100).map(event => JSON.stringify(event)).join('\n') : '暂无事件记录';
        } catch (_) {
          pages.failed = true; eventsPre.textContent = '读取执行轨迹失败，请点击“重试”。'; toast('读取执行轨迹失败，请重试。', 'error');
        } finally { pages.loading = false; updatePager(); }
      };
      previous.addEventListener('click', () => loadPage(pages.index - 1));
      next.addEventListener('click', () => loadPage(pages.index + 1));
      retry.addEventListener('click', () => loadPage(pages.pending));
      updatePager(); await loadPage(0);
    } catch (_) { box.textContent = '读取运行报告失败，请重试。'; toast('读取运行报告失败，请重试。', 'error'); }
    finally { button.disabled = false; button.textContent = '收起执行轨迹'; }
  }
  async function experimentAction(id, action, button) {
    const busyKey = `experiment-${action}-${id}`; if (state.busy.has(busyKey)) return;
    const title = action === 'resume' ? '恢复实验' : '取消实验'; if (action === 'cancel' && !window.confirm('确定取消此实验？正在运行的任务可能会被中止。')) return;
    if (button) setBusy(button, busyKey, true, action === 'resume' ? '恢复中…' : '取消中…'); else state.busy.add(busyKey);
    try { await request(`/v1/ui/experiments/${encodeURIComponent(id)}/${action}`, jsonOptions('POST', {})); toast(`${title}请求已提交。`, 'success'); await loadExperiments(); await openExperiment(id); }
    catch (e) { toast(`${title}失败：${e.message}`, 'error'); }
    finally { if (button) setBusy(button, busyKey, false); else state.busy.delete(busyKey); }
  }
  async function exportExperiment(id, parent) {
    const button = $$('.button', parent).find(b => b.textContent === '导出证据'); if (!button || button.disabled) return; setBusy(button, 'export', true, '导出中…');
    try { const data = await request(`/v1/ui/experiments/${encodeURIComponent(id)}/export`, jsonOptions('POST', {})); const path = data && data.path ? `\n保存位置：${data.path}` : ''; const omitted = data && Number(data.omitted) > 0 ? `，跳过 ${data.omitted} 项` : ''; toast(`证据导出完成：${data && data.exported !== undefined ? data.exported : 0} 项${omitted}${path}`, 'success', 8500); }
    catch (e) { toast(`导出证据失败：${e.message}`, 'error', 6500); }
    finally { setBusy(button, 'export', false); }
  }

  async function saveProvider(event) {
    event.preventDefault(); if (state.busy.has('provider')) return; const button = $('#save-provider'); const feedback = $('#provider-feedback'); const payload = { base_url: $('#base-url').value.trim(), model: $('#default-model').value.trim(), max_retries: Number.parseInt($('#max-retries').value, 10) };
    if (!payload.base_url || !payload.model || !Number.isInteger(payload.max_retries) || payload.max_retries < 0 || payload.max_retries > 2) { feedback.textContent = '请检查地址、模型名称和重试次数（0–2）。'; feedback.className = 'form-feedback error'; return; }
    setBusy(button, 'provider', true, '保存中…'); feedback.textContent = ''; feedback.className = 'form-feedback';
    try { await request('/v1/ui/settings', jsonOptions('POST', payload)); await loadState(); feedback.textContent = '设置已保存'; toast('模型服务设置已保存。', 'success'); }
    catch (e) { feedback.textContent = e.message; feedback.className = 'form-feedback error'; }
    finally { setBusy(button, 'provider', false); }
  }
  async function vaultSubmit(event, route, payloadFn, buttonID, clearIDs, successText, errorText) {
    event.preventDefault(); if (state.busy.has('vault')) return; const button = $(`#${buttonID}`); setBusy(button, 'vault', true, '处理中…');
    try { const payload = payloadFn(); const data = await request(route, jsonOptions('POST', payload, { secretSafe: true, safeMessage: errorText })); if (data && data.vault) state.vault = data.vault; renderVault(); toast(successText, 'success'); }
    catch (_) { toast(errorText, 'error'); }
    finally { clearIDs.forEach(id => { $(`#${id}`).value = ''; }); setBusy(button, 'vault', false); renderVault(); }
  }
  function bindEvents() {
    $$('.nav-item').forEach(b => b.addEventListener('click', () => switchTab(b.dataset.tab)));
    $$('[data-open-settings]').forEach(b => b.addEventListener('click', () => switchTab('settings')));
    $('#refresh-button').addEventListener('click', refreshAll); $('#reload-tasks').addEventListener('click', loadState); $('#refresh-runs').addEventListener('click', loadExperiments);
    $('#task-search').addEventListener('input', renderTasks); $('#select-all').addEventListener('click', () => { const q = $('#task-search').value.trim().toLowerCase(); state.tasks.filter(t => !q || taskSearchText(t).includes(q)).forEach(t => state.selectedTasks.add(t.task_id)); renderTasks(); }); $('#clear-tasks').addEventListener('click', () => { state.selectedTasks.clear(); renderTasks(); });
    $('#add-model').addEventListener('click', () => addModel()); $('#model-list').addEventListener('keydown', e => { if (e.key === 'Enter' && e.target.matches('input')) { e.preventDefault(); addModel(); const inputs = $$('.model-row input'); inputs[inputs.length - 1].focus(); } });
    $$('[data-number]').forEach(b => b.addEventListener('click', () => { const input = $('#repeat-count'); const val = Number(input.value) || 1; input.value = String(Math.max(1, Math.min(100, val + (b.dataset.number === 'up' ? 1 : -1)))); })); $('#repeat-count').addEventListener('change', e => { e.target.value = String(Math.max(1, Math.min(100, Number.parseInt(e.target.value, 10) || 1))); });
    $('#save-preset').addEventListener('click', savePreset); $('#load-preset').addEventListener('click', loadPreset); $('#delete-preset').addEventListener('click', deletePreset); $('#start-experiment').addEventListener('click', startExperiment);
    $('#provider-form').addEventListener('submit', saveProvider);
    $('#vault-save-form').addEventListener('submit', e => vaultSubmit(e, '/v1/ui/vault/save', () => ({ password: $('#vault-password').value, api_key: $('#api-key-input').value }), 'save-vault', ['vault-password', 'api-key-input'], 'API Key 已加密保存，密钥库已解锁。', '保存失败。请检查主密码和 API Key，确认主密码不少于 12 个字符。'));
    $('#vault-unlock-form').addEventListener('submit', e => vaultSubmit(e, '/v1/ui/vault/unlock', () => ({ password: $('#unlock-password').value }), 'unlock-vault', ['unlock-password'], '密钥库已解锁。', '解锁失败。请确认主密码正确。'));
    $('#lock-vault').addEventListener('click', async e => { const b = e.currentTarget; if (b.disabled) return; setBusy(b, 'vault', true, '锁定中…'); try { const data = await request('/v1/ui/vault/lock', jsonOptions('POST', {}, { secretSafe: true, safeMessage: '锁定密钥库失败。' })); if (data && data.vault) state.vault = data.vault; renderVault(); toast('密钥库已锁定。', 'success'); } catch (_) { toast('锁定密钥库失败。', 'error'); } finally { setBusy(b, 'vault', false); renderVault(); } });
    document.addEventListener('keydown', e => { if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); if (state.tab !== 'workspace') switchTab('workspace'); $('#task-search').focus(); } });
    document.addEventListener('visibilitychange', () => { if (document.hidden) { if (state.timer) window.clearInterval(state.timer); state.timer = null; } else { refreshVisible(); schedulePolling(); } });
  }
  function switchTab(tab) {
    if (!['workspace', 'runs', 'settings'].includes(tab)) return; state.tab = tab; $$('.nav-item').forEach(b => b.classList.toggle('active', b.dataset.tab === tab)); $$('.page').forEach(p => p.classList.toggle('active', p.id === `page-${tab}`)); $('#page-crumb').textContent = tab === 'workspace' ? '实验台' : tab === 'runs' ? '运行记录' : '连接设置'; refreshVisible(); schedulePolling();
  }
  async function refreshVisible() { if (document.hidden) return; if (state.tab === 'workspace') { await loadState(); } else if (state.tab === 'runs') { await Promise.all([loadExperiments(), loadState()]); if (state.currentExperiment) await openExperiment(state.currentExperiment); } else await loadState(); }
  function schedulePolling() {
    if (state.timer) window.clearInterval(state.timer); state.timer = null; if (document.hidden) return;
    state.timer = window.setInterval(async () => { if (document.hidden || state.busy.size) return; if (state.tab === 'runs') { if (state.refreshing) return; state.refreshing = true; try { await loadExperiments(); if (state.currentExperiment) await openExperiment(state.currentExperiment); } finally { state.refreshing = false; } } else if (state.tab === 'workspace') { if (state.refreshing) return; state.refreshing = true; try { await loadState(); } finally { state.refreshing = false; } } }, 5000);
  }
  async function refreshAll() { if (state.refreshing) return; state.refreshing = true; try { await Promise.all([loadState(), loadPresets(), loadExperiments()]); if (state.currentExperiment && state.tab === 'runs') await openExperiment(state.currentExperiment); } finally { state.refreshing = false; } }
  async function init() { addModel(); bindEvents(); await Promise.all([loadState(), loadPresets(), loadExperiments()]); schedulePolling(); }
  init();
})();
