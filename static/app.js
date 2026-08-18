'use strict';

/* ============================================================
   State
   ============================================================ */
const state = {
  gateways: {},        // id -> GatewayConfig
  listenAddr: '',
  metrics: null,
  logs: [],
  logsOffset: 0,
  logsLimit: 50,
  range: '1h',
  activeView: 'overview',
  drawerLog: null,
  drawerTab: 'request-compare',
  pollTimer: null,
};

const GW_ORDER = ['oc', 'st', 've'];

/* ============================================================
   Helpers
   ============================================================ */
function $(sel, root) { return (root || document).querySelector(sel); }
function $all(sel, root) { return Array.from((root || document).querySelectorAll(sel)); }

function fmtNum(n) {
  if (n === null || n === undefined) return '—';
  return Number(n).toLocaleString('en-US');
}

function fmtPct(n) {
  if (n === null || n === undefined) return '—';
  return n.toFixed(1) + '%';
}

function fmtMs(ms) {
  if (ms === null || ms === undefined) return '—';
  if (ms < 1000) return ms + 'ms';
  return (ms / 1000).toFixed(2) + 's';
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

function fmtTimeShort(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

function rangeToFrom(range) {
  const now = new Date();
  if (range === '1h') return new Date(now.getTime() - 60 * 60 * 1000);
  if (range === '24h') return new Date(now.getTime() - 24 * 60 * 60 * 1000);
  if (range === '7d') return new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000);
  return null; // all
}

function showToast(message, isError) {
  const el = $('#toast');
  el.textContent = message;
  el.classList.toggle('is-error', !!isError);
  el.classList.add('is-show');
  clearTimeout(showToast._t);
  showToast._t = setTimeout(() => el.classList.remove('is-show'), 2600);
}

async function api(path, opts) {
  const res = await fetch('/api' + path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts));
  let body = null;
  try { body = await res.json(); } catch (e) { /* no body */ }
  if (!res.ok) {
    const msg = (body && body.error && body.error.message) || res.statusText;
    throw new Error(msg);
  }
  return body;
}

/* ============================================================
   View switching
   ============================================================ */
function switchView(view) {
  state.activeView = view;
  $all('.view').forEach(el => el.classList.toggle('is-active', el.id === 'view-' + view));
  $all('.rail-nav-item').forEach(el => el.classList.toggle('is-active', el.dataset.view === view));
  if (view === 'logs') loadLogs(true);
  if (view === 'gateways') renderGatewayCards();
  if (view === 'setup') loadSetup();
}

$all('.rail-nav-item').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
});

$all('[data-goto]').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.goto));
});

/* ============================================================
   Config load
   ============================================================ */
async function loadConfig() {
  const data = await api('/config');
  state.gateways = data.gateways || {};
  state.listenAddr = data.listen_addr || '';
  $('#listenAddr').textContent = state.listenAddr || '—';
  const sel = $('#filterGateway');
  sel.innerHTML = '<option value="">全部网关</option>';
  GW_ORDER.forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    const opt = document.createElement('option');
    opt.value = id;
    opt.textContent = gw.name + ' (' + gw.prefix + ')';
    sel.appendChild(opt);
  });
}

/* ============================================================
   Overview: metrics + gateway pulses + recent logs
   ============================================================ */
function metricsQuery(extra) {
  const params = new URLSearchParams();
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  if (extra) Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

async function loadOverview() {
  await Promise.all([loadMetrics(), loadGatewayPulses(), loadRecentLogs()]);
}

async function loadMetrics() {
  try {
    const m = await api('/metrics?' + metricsQuery());
    state.metrics = m;
    renderMetrics(m);
  } catch (e) {
    showToast('加载指标失败: ' + e.message, true);
  }
}

function renderMetrics(m) {
  $('#statRequests').textContent = fmtNum(m.requests);
  $('#statRequestsSub').textContent = m.failures ? fmtNum(m.failures) + ' 次失败' : '无失败';

  const successRate = m.requests > 0 ? (m.successes / m.requests) * 100 : null;
  $('#statSuccess').textContent = successRate === null ? '—' : fmtPct(successRate);
  $('#statSuccessSub').textContent = fmtNum(m.successes) + ' / ' + fmtNum(m.requests);

  $('#statCacheHit').textContent = m.cache_hit_rate === null || m.cache_hit_rate === undefined ? '—' : fmtPct(m.cache_hit_rate);
  $('#statCacheHitSub').textContent = fmtNum(m.cache_read_tokens) + ' 读取 tok';

  $('#statCacheCoverage').textContent = m.cache_coverage_percent === null || m.cache_coverage_percent === undefined ? '—' : fmtPct(m.cache_coverage_percent);
  $('#statCacheCoverageSub').textContent = fmtNum(m.cache_supported_calls) + ' / ' + fmtNum(m.usage_calls) + ' 次调用';

  $('#statTokens').textContent = fmtNum(m.input_tokens) + ' / ' + fmtNum(m.output_tokens);
  $('#statTokensSub').textContent = 'prompt ' + fmtNum(m.prompt_tokens);

  $('#statReasoning').textContent = fmtNum(m.reasoning_tokens);
  $('#statReasoningSub').textContent = 'cache write ' + fmtNum(m.cache_write_tokens);
}

async function loadGatewayPulses() {
  const container = $('#gatewayPulseList');
  const railContainer = $('#railChannels');
  container.innerHTML = '';
  railContainer.innerHTML = '';

  for (const id of GW_ORDER) {
    const gw = state.gateways[id];
    if (!gw) continue;
    let logs = [];
    try {
      const data = await api('/logs?' + new URLSearchParams({ gateway: id, limit: '40' }).toString());
      logs = (data.items || []).slice().reverse(); // oldest -> newest for left-to-right reading
    } catch (e) { /* ignore per-gateway error */ }

    const successCount = logs.filter(l => l.success).length;
    const total = logs.length;

    // main panel row
    const row = document.createElement('div');
    row.className = 'gw-pulse-row';
    row.innerHTML =
      '<div class="gw-pulse-top">' +
        '<span><span class="gw-pulse-name">' + escapeHtml(gw.name) + '</span><span class="gw-pulse-prefix">' + gw.prefix + '</span></span>' +
        '<span class="gw-pulse-stat">' + (total ? successCount + '/' + total + ' 成功' : '暂无数据') + '</span>' +
      '</div>' +
      '<div class="gw-pulse-bar">' + renderTicks(logs, 40) + '</div>';
    container.appendChild(row);

    // rail mini strip
    const chan = document.createElement('div');
    chan.className = 'rail-channel';
    chan.innerHTML =
      '<div class="rail-channel-top">' +
        '<span class="rail-channel-name">' + escapeHtml(gw.name) + '</span>' +
        '<span class="rail-channel-prefix">' + gw.prefix + '</span>' +
      '</div>' +
      '<div class="rail-channel-ticks">' + renderTicks(logs, 18) + '</div>';
    railContainer.appendChild(chan);
  }
}

function renderTicks(logs, count) {
  const padded = new Array(Math.max(0, count - logs.length)).fill(null).concat(logs);
  const slice = padded.slice(-count);
  return slice.map(l => {
    if (!l) return '<span class="tick"></span>';
    const cls = l.success ? 'ok' : 'err';
    const height = 35 + Math.min(65, Math.round(((l.duration_ms || 0) / 3000) * 65));
    return '<span class="tick ' + cls + '" style="height:' + height + '%" title="' + (l.model || '') + ' · ' + (l.status_code || '') + '"></span>';
  }).join('');
}

async function loadRecentLogs() {
  const container = $('#recentLogList');
  try {
    const data = await api('/logs?limit=12');
    const items = data.items || [];
    if (!items.length) {
      container.innerHTML = '<div class="empty-state">暂无请求记录，等待流量接入…</div>';
      return;
    }
    container.innerHTML = items.map(l => (
      '<div class="recent-log-item" data-id="' + l.id + '">' +
        '<span class="rli-gw ' + '">' + (l.gateway_id || '?') + '</span>' +
        '<span class="rli-model" title="' + escapeHtml(l.model || '') + '">' + escapeHtml(l.model || '(未知模型)') + '</span>' +
        '<span class="rli-time">' + fmtTimeShort(l.started_at) + '</span>' +
        '<span class="rli-status" style="background:' + (l.success ? 'var(--signal)' : 'var(--coral)') + '"></span>' +
      '</div>'
    )).join('');
    $all('.recent-log-item', container).forEach(el => {
      el.addEventListener('click', () => openDrawer(el.dataset.id));
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

$('#rangePicker').addEventListener('click', (e) => {
  const btn = e.target.closest('button[data-range]');
  if (!btn) return;
  state.range = btn.dataset.range;
  $all('#rangePicker button').forEach(b => b.classList.toggle('is-active', b === btn));
  loadMetrics();
});

/* ============================================================
   Logs view
   ============================================================ */
function currentFilters() {
  return {
    gateway: $('#filterGateway').value,
    model: $('#filterModel').value.trim(),
    status: $('#filterStatus').value,
    limit: $('#filterLimit').value,
  };
}

async function loadLogs(reset) {
  if (reset) state.logsOffset = 0;
  const f = currentFilters();
  const params = new URLSearchParams();
  if (f.gateway) params.set('gateway', f.gateway);
  if (f.model) params.set('model', f.model);
  if (f.status) params.set('status', f.status);
  params.set('limit', f.limit);
  params.set('offset', String(state.logsOffset));

  const tbody = $('#logTableBody');
  if (reset) tbody.innerHTML = '<tr class="empty-row"><td colspan="8">加载中…</td></tr>';

  try {
    const data = await api('/logs?' + params.toString());
    const items = data.items || [];
    if (reset) state.logs = items;
    else state.logs = state.logs.concat(items);
    renderLogTable(state.logs);
    state.logsOffset += items.length;
    $('#logsLoadMoreBtn').style.display = items.length < Number(f.limit) ? 'none' : 'inline-flex';
  } catch (e) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="8">加载失败：' + escapeHtml(e.message) + '</td></tr>';
  }
}

function renderLogTable(logs) {
  const tbody = $('#logTableBody');
  if (!logs.length) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="8">没有匹配的请求记录</td></tr>';
    return;
  }
  tbody.innerHTML = logs.map(l => {
    const gwClass = l.gateway_id || '';
    const cacheHit = (l.usage && l.usage.usage_present && l.usage.cache_supported && l.usage.prompt_tokens > 0)
      ? fmtPct((l.usage.cache_read_tokens / l.usage.prompt_tokens) * 100)
      : '—';
    return (
      '<tr data-id="' + l.id + '">' +
        '<td>' + fmtTime(l.started_at) + '</td>' +
        '<td><span class="gw-tag ' + gwClass + '">' + (l.gateway_name || l.gateway_id || '?') + '</span></td>' +
        '<td>' + escapeHtml(l.model || '—') + '</td>' +
        '<td><span class="status-pill ' + (l.success ? 'ok' : 'err') + '">' + (l.success ? '● ' + l.status_code : '● ' + (l.status_code || '错误')) + '</span></td>' +
        '<td>' + fmtMs(l.duration_ms) + '</td>' +
        '<td>' + (l.stream ? '是' : '否') + '</td>' +
        '<td>' + (l.usage ? fmtNum(l.usage.input_tokens || l.usage.prompt_tokens) + ' / ' + fmtNum(l.usage.output_tokens) : '—') + '</td>' +
        '<td>' + cacheHit + '</td>' +
      '</tr>'
    );
  }).join('');
  $all('tr[data-id]', tbody).forEach(tr => {
    tr.addEventListener('click', () => openDrawer(tr.dataset.id));
  });
}

$('#logsRefreshBtn').addEventListener('click', () => loadLogs(true));
$('#logsLoadMoreBtn').addEventListener('click', () => loadLogs(false));
$('#filterGateway').addEventListener('change', () => loadLogs(true));
$('#filterStatus').addEventListener('change', () => loadLogs(true));
$('#filterLimit').addEventListener('change', () => loadLogs(true));
let modelDebounce;
$('#filterModel').addEventListener('input', () => {
  clearTimeout(modelDebounce);
  modelDebounce = setTimeout(() => loadLogs(true), 350);
});

$('#logsClearBtn').addEventListener('click', async () => {
  if (!confirm('确定要清空全部请求日志吗？此操作无法撤销。')) return;
  try {
    const res = await api('/logs', { method: 'DELETE' });
    showToast('已清空 ' + (res.deleted || 0) + ' 条日志');
    loadLogs(true);
    loadOverview();
  } catch (e) {
    showToast('清空失败: ' + e.message, true);
  }
});

/* ============================================================
   Log detail drawer
   ============================================================ */
async function openDrawer(id) {
  const backdrop = $('#drawerBackdrop');
  const drawer = $('#logDrawer');
  backdrop.classList.add('is-open');
  drawer.classList.add('is-open');
  $('#drawerId').textContent = id;
  $('#drawerTitle').textContent = '加载中…';
  $('#drawerMeta').innerHTML = '';
  $('#drawerCode').textContent = '加载中…';

  try {
    const log = await api('/logs/' + id);
    state.drawerLog = log;
    state.drawerTab = 'request-compare';
    $all('.drawer-tab').forEach(t => t.classList.toggle('is-active', t.dataset.tab === 'request-compare'));
    renderDrawer(log);
  } catch (e) {
    $('#drawerTitle').textContent = '加载失败';
    $('#drawerCode').textContent = e.message;
  }
}

function renderDrawer(log) {
  $('#drawerTitle').textContent = (log.model || '(未知模型)') + '  ·  ' + (log.gateway_name || log.gateway_id);
  $('#drawerMeta').innerHTML = [
    ['时间', fmtTime(log.started_at)],
    ['状态', (log.success ? '成功 ' : '失败 ') + (log.status_code || '')],
    ['耗时', fmtMs(log.duration_ms)],
    ['流式', log.stream ? '是' : '否'],
    ['方法', log.method + ' ' + log.request_path],
    ['上游', log.upstream_url || '—'],
  ].map(([k, v]) => '<span class="meta-chip">' + k + ': <b>' + escapeHtml(String(v)) + '</b></span>').join('');
  if (log.error) {
    $('#drawerMeta').innerHTML += '<span class="meta-chip" style="color:var(--coral)">错误: <b>' + escapeHtml(log.error) + '</b></span>';
  }
  renderDrawerTab(state.drawerTab);
}

function renderDrawerTab(tab) {
  const log = state.drawerLog;
  if (!log) return;
  const compare = $('#drawerCompare');
  const code = $('#drawerCode');
  if (tab === 'request-compare' || tab === 'response-compare') {
    compare.style.display = 'grid';
    code.style.display = 'none';
    renderComparison(tab, log);
    return;
  }
  compare.style.display = 'none';
  code.style.display = 'block';
  let content = '';
  if (tab === 'request') content = tryPretty(log.request_body);
  else if (tab === 'upstream') content = tryPretty(log.upstream_body);
  else if (tab === 'upstream-response') content = tryPretty(log.upstream_response_body || log.response_body);
  else if (tab === 'response') content = tryPretty(log.response_body);
  else if (tab === 'headers') content = tryPretty(log.request_headers_actual || log.request_headers) + '\n\n--- upstream request headers ---\n\n' + tryPretty(log.upstream_headers_actual || log.upstream_headers) + '\n\n--- upstream response headers ---\n\n' + tryPretty(log.upstream_response_headers_actual || log.upstream_response_headers) + '\n\n--- client response headers ---\n\n' + tryPretty(log.response_headers_actual || log.response_headers);
  code.textContent = content || '(空)';
}

function renderComparison(tab, log) {
  const request = tab === 'request-compare';
  const left = request ? {
    label: '客户端原请求',
    url: (log.method || 'POST') + ' ' + (log.request_url || log.request_path || '—'),
    headers: log.request_headers_actual || log.request_headers,
    body: log.request_body,
  } : {
    label: '上游 API 原始响应',
    url: 'HTTP ' + (log.upstream_response_status_code || log.status_code || '—'),
    headers: log.upstream_response_headers_actual || log.upstream_response_headers,
    body: log.upstream_response_body || log.response_body,
  };
  const right = request ? {
    label: '代理实际发送',
    url: (log.method || 'POST') + ' ' + (log.upstream_url || '—'),
    headers: log.upstream_headers_actual || log.upstream_headers,
    body: log.upstream_body,
  } : {
    label: '代理实际返回客户端',
    url: 'HTTP ' + (log.client_response_status_code || log.status_code || '—'),
    headers: log.response_headers_actual || log.response_headers,
    body: log.response_body,
  };
  const headerDiff = buildDiff(left.headers, right.headers, 'headers');
  const bodyDiff = buildDiff(left.body, right.body, 'body');
  const statusDiff = request ? null : buildValueDiff('HTTP 状态码', log.upstream_response_status_code || log.status_code, log.client_response_status_code || log.status_code);
  const total = diffStats([headerDiff, bodyDiff, statusDiff]);
  $('#drawerCompare').innerHTML =
    '<div class="diff-overview">' +
      '<div class="diff-route"><span>' + escapeHtml(left.label) + '</span><b>→</b><span>' + escapeHtml(right.label) + '</span></div>' +
      '<div class="diff-endpoints"><code>' + escapeHtml(left.url) + '</code><b>→</b><code>' + escapeHtml(right.url) + '</code></div>' +
      '<div class="diff-summary">' +
        diffSummaryBadge('新增', total.added, 'added') +
        diffSummaryBadge('修改', total.modified, 'modified') +
        diffSummaryBadge('删除', total.deleted, 'deleted') +
        (total.total === 0 ? '<span class="diff-summary-badge same">没有检测到变更</span>' : '') +
      '</div>' +
    '</div>' +
    (statusDiff ? renderDiffSection(statusDiff, '状态码', '上游 API 返回 → 客户端实际收到') : '') +
    renderDiffSection(headerDiff, 'Headers', '请求头字段变更') +
    renderDiffSection(bodyDiff, 'Body', '正文变更');
}

function diffSummaryBadge(label, count, kind) {
  return count ? '<span class="diff-summary-badge ' + kind + '">' + label + ' ' + count + '</span>' : '';
}

function diffStats(diffs) {
  return diffs.filter(Boolean).reduce((total, diff) => {
    for (const row of diff.rows) {
      if (row.kind === 'added') total.added++;
      if (row.kind === 'modified') total.modified++;
      if (row.kind === 'deleted') total.deleted++;
      if (row.kind !== 'same') total.total++;
    }
    return total;
  }, { added: 0, modified: 0, deleted: 0, total: 0 });
}

function buildValueDiff(path, before, after) {
  if (String(before) === String(after)) return { rows: [] };
  return { rows: [{ kind: 'modified', path, before: String(before), after: String(after), explanation: path + ' 从 ' + before + ' 变为 ' + after }] };
}

function buildDiff(beforeRaw, afterRaw, category) {
  const before = String(beforeRaw || '');
  const after = String(afterRaw || '');
  if (before === after) return { rows: [] };

  const beforeJSON = tryParseJSON(before);
  const afterJSON = tryParseJSON(after);
  if (beforeJSON.ok && afterJSON.ok) {
    return { rows: buildJSONDiff(beforeJSON.value, afterJSON.value, category) };
  }
  return { rows: buildTextDiff(before, after) };
}

function tryParseJSON(raw) {
  if (!raw.trim()) return { ok: false };
  try { return { ok: true, value: JSON.parse(raw) }; }
  catch (e) { return { ok: false }; }
}

function flattenJSON(value, path, result) {
  result = result || [];
  path = path || '$';
  if (Array.isArray(value)) {
    if (!value.length) result.push({ path, value });
    value.forEach((item, index) => flattenJSON(item, path + '[' + index + ']', result));
    return result;
  }
  if (value && typeof value === 'object') {
    const keys = Object.keys(value).sort();
    if (!keys.length) result.push({ path, value });
    keys.forEach(key => {
      const childPath = /^[A-Za-z_][A-Za-z0-9_]*$/.test(key) ? path + '.' + key : path + '[' + JSON.stringify(key) + ']';
      flattenJSON(value[key], childPath, result);
    });
    return result;
  }
  result.push({ path, value });
  return result;
}

function buildJSONDiff(before, after, category) {
  const beforeMap = new Map(flattenJSON(before).map(item => [item.path, item.value]));
  const afterMap = new Map(flattenJSON(after).map(item => [item.path, item.value]));
  const paths = Array.from(new Set([...beforeMap.keys(), ...afterMap.keys()])).sort();
  const rows = [];
  paths.forEach(path => {
    const hasBefore = beforeMap.has(path);
    const hasAfter = afterMap.has(path);
    if (!hasBefore) {
      rows.push({ kind: 'added', path, after: formatDiffValue(afterMap.get(path)), explanation: explainJSONChange(path, 'added', '', formatDiffValue(afterMap.get(path)), category) });
    } else if (!hasAfter) {
      rows.push({ kind: 'deleted', path, before: formatDiffValue(beforeMap.get(path)), explanation: explainJSONChange(path, 'deleted', formatDiffValue(beforeMap.get(path)), '', category) });
    } else if (JSON.stringify(beforeMap.get(path)) !== JSON.stringify(afterMap.get(path))) {
      const beforeValue = formatDiffValue(beforeMap.get(path));
      const afterValue = formatDiffValue(afterMap.get(path));
      rows.push({
        kind: 'modified',
        path,
        before: beforeValue,
        after: afterValue,
        explanation: explainJSONChange(path, 'modified', beforeValue, afterValue, category),
      });
    }
  });
  return rows;
}

function explainJSONChange(path, kind, before, after, category) {
  if (category === 'headers') {
    const header = headerNameFromPath(path);
    const lower = header.toLowerCase();
    if (kind === 'deleted' && hopByHopHeaderNames().includes(lower)) {
      return '删除请求头 ' + header + '：HTTP hop-by-hop 头只属于当前连接，不应转发到下一跳';
    }
    if (kind === 'deleted' && lower === 'content-length') {
      return '删除请求头 Content-Length：代理会根据实际发送的正文重新管理长度';
    }
    if (kind === 'deleted' && lower.startsWith('x-grok-')) {
      return '删除请求头 ' + header + '：这是客户端/代理内部头，代理不会转发';
    }
    if (kind === 'deleted') {
      return '删除请求头 ' + header + '：未包含在当前网关的 Forward Headers 白名单中';
    }
    if (kind === 'added' && lower === 'user-agent') {
      return '新增请求头 User-Agent：当前网关启用了 User-Agent 覆盖或客户端头被代理补齐';
    }
    if (kind === 'added') return '新增请求头 ' + header + '：由代理转发白名单或代理策略加入';
    return '修改请求头 ' + header + '：代理策略改变了该值';
  }
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function"' && after === '"function_call"') {
    return '商汤协议兼容：将客户端 tool_calls 类型 function 转为上游要求的 function_call';
  }
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function_call"' && after === '"function"') {
    return '客户端协议兼容：将商汤返回的 function_call 转回客户端使用的 function';
  }
  if (kind === 'added') return '新增字段 ' + path;
  if (kind === 'deleted') return '删除字段 ' + path;
  return '修改字段 ' + path;
}

function headerNameFromPath(path) {
  const dotMatch = path.match(/^\$\.([A-Za-z0-9_-]+)/);
  if (dotMatch) return dotMatch[1];
  const bracketMatch = path.match(/^\$\[\"([^\"]+)\"\]/);
  return bracketMatch ? bracketMatch[1] : path;
}

function hopByHopHeaderNames() {
  return ['connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer', 'transfer-encoding', 'upgrade', 'host'];
}

function formatDiffValue(value) {
  if (typeof value === 'string') return JSON.stringify(value);
  const encoded = JSON.stringify(value);
  return encoded === undefined ? String(value) : encoded;
}

function buildTextDiff(before, after) {
  const oldLines = before.split(/\r?\n/);
  const newLines = after.split(/\r?\n/);
  const maxCells = 1600000;
  if (oldLines.length * newLines.length > maxCells) {
    return [{ kind: 'modified', path: '文本', before: truncateDiffText(before), after: truncateDiffText(after), explanation: '文本内容发生变化，内容过大，已折叠显示' }];
  }
  const table = Array.from({ length: oldLines.length + 1 }, () => new Uint32Array(newLines.length + 1));
  for (let i = oldLines.length - 1; i >= 0; i--) {
    for (let j = newLines.length - 1; j >= 0; j--) {
      table[i][j] = oldLines[i] === newLines[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }
  const operations = [];
  let i = 0, j = 0;
  while (i < oldLines.length && j < newLines.length) {
    if (oldLines[i] === newLines[j]) { operations.push({ kind: 'same', text: oldLines[i] }); i++; j++; }
    else if (table[i + 1][j] >= table[i][j + 1]) { operations.push({ kind: 'deleted', text: oldLines[i++] }); }
    else { operations.push({ kind: 'added', text: newLines[j++] }); }
  }
  while (i < oldLines.length) operations.push({ kind: 'deleted', text: oldLines[i++] });
  while (j < newLines.length) operations.push({ kind: 'added', text: newLines[j++] });

  const rows = [];
  for (let index = 0; index < operations.length; index++) {
    const current = operations[index];
    const next = operations[index + 1];
    if (current.kind === 'deleted' && next && next.kind === 'added') {
      rows.push({ kind: 'modified', path: '第 ' + (index + 1) + ' 行', before: current.text, after: next.text, explanation: '修改第 ' + (index + 1) + ' 行' });
      index++;
    } else if (current.kind === 'deleted') {
      rows.push({ kind: 'deleted', path: '文本行', before: current.text, explanation: '删除文本行' });
    } else if (current.kind === 'added') {
      rows.push({ kind: 'added', path: '文本行', after: current.text, explanation: '新增文本行' });
    } else {
      rows.push({ kind: 'same', path: '', before: current.text, after: current.text });
    }
  }
  return rows;
}

function truncateDiffText(text) {
  return text.length > 4000 ? text.slice(0, 4000) + '\n… (已折叠)' : text;
}

function renderDiffSection(diff, title, subtitle) {
  const changedRows = diff.rows.filter(row => row.kind !== 'same');
  if (!changedRows.length) {
    return '<section class="diff-section diff-section-empty"><div class="diff-section-head"><div><strong>' + title + '</strong><span>' + subtitle + '</span></div><span class="diff-no-change">无变化</span></div></section>';
  }
  return '<section class="diff-section">' +
    '<div class="diff-section-head"><div><strong>' + title + '</strong><span>' + subtitle + '</span></div><span class="diff-change-count">' + changedRows.length + ' 处变更</span></div>' +
    '<div class="diff-column-head"><span>原始</span><span>代理实际</span></div>' +
    '<div class="diff-rows">' + renderDiffRows(diff.rows) + '</div>' +
  '</section>';
}

function renderDiffRows(rows) {
  const output = [];
  const changedIndexes = rows.map((row, index) => row.kind !== 'same' ? index : -1).filter(index => index >= 0);
  const visibleContext = new Set();
  changedIndexes.forEach(index => {
    for (let offset = -2; offset <= 2; offset++) {
      if (index + offset >= 0 && index + offset < rows.length) visibleContext.add(index + offset);
    }
  });
  let collapsed = false;
  rows.forEach((row, index) => {
    if (row.kind === 'same' && !visibleContext.has(index)) {
      if (!collapsed) output.push('<div class="diff-collapse">… 其余未变化内容已折叠 …</div>');
      collapsed = true;
      return;
    }
    collapsed = false;
    output.push(renderDiffRow(row));
  });
  return output.join('');
}

function diffLineText(row, value) {
  if (row.kind === 'same' || !row.path) return value;
  return row.path + ' = ' + value;
}

function renderDiffRow(row) {
  const oldLine = row.kind === 'added' ? '' : diffLineText(row, row.before || '');
  const newLine = row.kind === 'deleted' ? '' : diffLineText(row, row.after || '');
  const oldClass = row.kind === 'deleted' || row.kind === 'modified' ? 'diff-line-old' : 'diff-line-empty';
  const newClass = row.kind === 'added' || row.kind === 'modified' ? 'diff-line-new' : 'diff-line-empty';
  const marker = row.kind === 'added' ? '+' : row.kind === 'deleted' ? '−' : row.kind === 'modified' ? '±' : ' ';
  return '<div class="diff-row ' + row.kind + '">' +
    '<div class="diff-line ' + oldClass + '"><i>' + (row.kind === 'added' ? ' ' : marker) + '</i><code>' + escapeHtml(oldLine) + '</code></div>' +
    '<div class="diff-line ' + newClass + '"><i>' + (row.kind === 'deleted' ? ' ' : marker) + '</i><code>' + escapeHtml(newLine) + '</code></div>' +
    (row.kind === 'same' ? '' : '<div class="diff-explanation">' + escapeHtml(row.explanation || '') + (row.path ? ' <code>' + escapeHtml(row.path) + '</code>' : '') + '</div>') +
  '</div>';
}

function tryPretty(raw) {
  if (!raw) return '';
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch (e) {
    return raw;
  }
}

$('#drawerTabs').addEventListener('click', (e) => {
  const btn = e.target.closest('.drawer-tab');
  if (!btn) return;
  state.drawerTab = btn.dataset.tab;
  $all('.drawer-tab').forEach(t => t.classList.toggle('is-active', t === btn));
  renderDrawerTab(state.drawerTab);
});

function closeDrawer() {
  $('#drawerBackdrop').classList.remove('is-open');
  $('#logDrawer').classList.remove('is-open');
}
$('#drawerCloseBtn').addEventListener('click', closeDrawer);
$('#drawerBackdrop').addEventListener('click', closeDrawer);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeDrawer(); });

/* ============================================================
   Gateway config view
   ============================================================ */
function renderGatewayCards() {
  const container = $('#gatewayCards');
  container.innerHTML = '';
  GW_ORDER.forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    const card = document.createElement('div');
    card.className = 'gw-card';
    card.dataset.id = id;
    const headers = (gw.forward_headers || []).join('\n');
    card.innerHTML =
      '<div class="gw-card-head">' +
        '<div class="gw-card-title">' +
          '<strong>' + escapeHtml(gw.name) + '</strong>' +
          '<span class="gw-card-prefix">' + gw.prefix + '</span>' +
        '</div>' +
        '<div class="toggle-row compact-toggle-row"><span class="switch-label">网关启用</span>' +
          '<label class="toggle">' +
            '<input type="checkbox" class="f-enabled" ' + (gw.enabled ? 'checked' : '') + ' />' +
            '<span class="toggle-track"></span>' +
          '</label>' +
        '</div>' +
      '</div>' +
      '<div class="gw-card-protocol">协议: ' + (gw.protocol === 'chat_completions' ? 'Chat Completions' : 'Responses') + '</div>' +
      '<div class="field">' +
        '<label>显示名称</label>' +
        '<input type="text" class="f-name" value="' + escapeAttr(gw.name) + '" />' +
      '</div>' +
      '<div class="field">' +
        '<label>上游地址 (base_url)</label>' +
        '<input type="text" class="f-baseurl" value="' + escapeAttr(gw.base_url) + '" placeholder="https://…" />' +
        '<span class="field-hint">必须是 HTTPS 地址</span>' +
      '</div>' +
      '<div class="toggle-row ua-toggle-row">' +
        '<div><span class="switch-label">User-Agent 覆盖</span><span class="field-hint">仅作用于此网关</span></div>' +
        '<label class="toggle">' +
          '<input type="checkbox" class="f-ua-enabled" ' + (gw.user_agent_override_enabled ? 'checked' : '') + ' />' +
          '<span class="toggle-track"></span>' +
        '</label>' +
      '</div>' +
      '<div class="field">' +
        '<label>User-Agent 覆盖值</label>' +
        '<input type="text" class="f-ua" value="' + escapeAttr(gw.user_agent_override || '') + '" placeholder="grok-gateway-proxy/dev" />' +
      '</div>' +
      '<div class="field">' +
        '<label>请求头白名单（每行一个）</label>' +
        '<textarea class="f-headers" rows="4" placeholder="Authorization&#10;X-Api-Key">' + escapeHtml(headers) + '</textarea>' +
      '</div>' +
      '<div class="gw-card-foot">' +
        '<button class="btn-ghost small save-gw-btn" style="width:auto;padding:8px 18px;">保存更改</button>' +
      '</div>';
    container.appendChild(card);

    card.querySelector('.save-gw-btn').addEventListener('click', () => saveGateway(id, card));
  });
}

async function saveGateway(id, card) {
  const btn = card.querySelector('.save-gw-btn');
  const payload = {
    name: card.querySelector('.f-name').value.trim(),
    base_url: card.querySelector('.f-baseurl').value.trim(),
    enabled: card.querySelector('.f-enabled').checked,
    user_agent_override_enabled: card.querySelector('.f-ua-enabled').checked,
    user_agent_override: card.querySelector('.f-ua').value.trim(),
    forward_headers: card.querySelector('.f-headers').value.split('\n').map(s => s.trim()).filter(Boolean),
  };
  btn.textContent = '保存中…';
  btn.disabled = true;
  try {
    const gateways = {};
    gateways[id] = Object.assign({}, state.gateways[id], payload);
    const res = await api('/gateways', { method: 'PUT', body: JSON.stringify({ gateways }) });
    state.gateways = res.gateways;
    showToast('网关 ' + payload.name + ' 已更新');
    renderGatewayCards();
  } catch (e) {
    showToast('保存失败: ' + e.message, true);
  } finally {
    btn.textContent = '保存更改';
    btn.disabled = false;
  }
}

/* ============================================================
   Setup snippets view
   ============================================================ */
async function loadSetup() {
  const container = $('#setupSnippets');
  container.innerHTML = '<div class="empty-state">加载中…</div>';
  try {
    const data = await api('/setup');
    container.innerHTML = '';
    GW_ORDER.forEach(id => {
      const snippet = data[id];
      if (!snippet) return;
      const gw = state.gateways[id] || {};
      const card = document.createElement('div');
      card.className = 'setup-card';
      card.innerHTML =
        '<div class="setup-card-head">' +
          '<strong>' + escapeHtml(gw.name || id) + ' <span class="gw-card-prefix" style="font-size:10.5px;">' + (gw.prefix || '') + '</span></strong>' +
          '<button class="btn-ghost small copy-btn" style="width:auto;padding:6px 12px;">复制</button>' +
        '</div>' +
        '<pre class="setup-pre">' + escapeHtml(snippet) + '</pre>';
      container.appendChild(card);
      card.querySelector('.copy-btn').addEventListener('click', () => {
        navigator.clipboard.writeText(snippet).then(() => showToast('已复制到剪贴板'));
      });
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

/* ============================================================
   Escaping utils
   ============================================================ */
function escapeHtml(str) {
  return String(str).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function escapeAttr(str) { return escapeHtml(str); }

/* ============================================================
   Boot
   ============================================================ */
$('#refreshAllBtn').addEventListener('click', () => {
  loadConfig().then(() => {
    if (state.activeView === 'overview') loadOverview();
    if (state.activeView === 'logs') loadLogs(true);
    if (state.activeView === 'gateways') renderGatewayCards();
  });
  showToast('已刷新');
});

async function boot() {
  try {
    await loadConfig();
    await loadOverview();
  } catch (e) {
    showToast('初始化失败: ' + e.message, true);
  }
  // light polling for overview while visible
  state.pollTimer = setInterval(() => {
    if (state.activeView === 'overview') loadOverview();
    if (state.activeView === 'logs') { /* don't auto-reset user's scroll; skip */ }
  }, 15000);
}

boot();
