'use strict';

/* ============================================================
   Grok Gateway Console · v3
   Vanilla JS，通过 /api/* 与 Go 后端通信，全部客户端渲染。
   ============================================================ */

/* ---------------- State ---------------- */
const state = {
  gateways: {},
  listenAddr: '',
  metrics: null,
  logs: [],
  recentLogs: [],
  sparkSeries: [],
  logsOffset: 0,
  logsLimit: 50,
  range: '1h',
  activeView: 'overview',
  drawerLog: null,
  drawerTab: 'request-compare',
  pollTimer: null,
  cmdkSelected: 0,
  cmdkItems: []
};

const GW_ORDER = ['oc', 'st', 've'];

/* ---------------- Helpers ---------------- */
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
function escapeHtml(str) {
  return String(str == null ? '' : str).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}
function escapeAttr(str) { return escapeHtml(str); }
function rangeToFrom(range) {
  const now = new Date();
  if (range === '1h') return new Date(now.getTime() - 60 * 60 * 1000);
  if (range === '24h') return new Date(now.getTime() - 24 * 60 * 60 * 1000);
  if (range === '7d') return new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000);
  return null;
}
function rangeLabel(range) {
  return ({ '1h': '1小时', '24h': '24小时', '7d': '7天', 'all': '全部' })[range] || range;
}

/* ---------------- API client ---------------- */
async function api(path, opts) {
  const res = await fetch('/api' + path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts));
  let body = null;
  try { body = await res.json(); } catch (e) { /* no body */ }
  if (!res.ok) {
    const msg = (body && body.error && body.error.message) || res.statusText || ('HTTP ' + res.status);
    throw new Error(msg);
  }
  return body;
}

/* ---------------- Toast ---------------- */
const toastIcons = {
  success: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M3.5 8.5l3 3 6-6" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  error: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/></svg>',
  info: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M8 5.5v6M8 3.5v.01" stroke="currentColor" stroke-width="1.8" fill="none" stroke-linecap="round"/></svg>'
};
function showToast(message, kind) {
  const stack = $('#toastStack');
  if (!stack) return;
  const t = document.createElement('div');
  t.className = 'toast' + (kind === 'error' ? ' is-error' : '');
  t.innerHTML = '<span class="toast-icon">' + (toastIcons[kind] || toastIcons.info) + '</span><span>' + escapeHtml(message) + '</span>';
  stack.appendChild(t);
  requestAnimationFrame(() => t.classList.add('is-show'));
  setTimeout(() => {
    t.classList.remove('is-show');
    setTimeout(() => { if (t.parentNode) t.remove(); }, 300);
  }, 2800);
}

/* ---------------- 视图切换 ---------------- */
function switchView(view) {
  state.activeView = view;
  $all('.view').forEach(el => el.classList.toggle('is-active', el.id === 'view-' + view));
  $all('.rail-nav-item').forEach(el => el.classList.toggle('is-active', el.dataset.view === view));
  if (view === 'logs') loadLogs(true);
  if (view === 'gateways') renderGatewayCards();
  if (view === 'setup') loadSetup();
  closeCmdk();
}
$all('.rail-nav-item').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
});
$all('[data-goto]').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.goto));
});

/* ============================================================
   Config
   ============================================================ */
async function loadConfig() {
  const data = await api('/config');
  state.gateways = data.gateways || {};
  state.listenAddr = data.listen_addr || '';
  $('#listenAddr').textContent = state.listenAddr || '—';
  const sel = $('#filterGateway');
  if (sel) {
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
}

/* ============================================================
   Overview：指标 + 仪表 + 趋势线 + 脉冲 + 最近请求
   ============================================================ */
function metricsQuery(extra) {
  const params = new URLSearchParams();
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  if (extra) Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

async function loadOverview() {
  await Promise.all([loadMetrics(), loadSparkSeries(), loadGatewayPulses(), loadRecentLogs()]);
  $('#pulseRange').textContent = rangeLabel(state.range);
}

async function loadMetrics() {
  try {
    const m = await api('/metrics?' + metricsQuery());
    state.metrics = m;
    renderMetrics(m);
  } catch (e) {
    showToast('加载指标失败: ' + e.message, 'error');
  }
}

function setGauge(sel, pct) {
  const el = document.querySelector(sel);
  if (!el) return;
  const valid = pct !== null && pct !== undefined;
  const v = valid ? Math.max(0, Math.min(100, Number(pct))) : 0;
  el.style.setProperty('--p', String(v));
}

function renderMetrics(m) {
  $('#statRequests').textContent = fmtNum(m.requests);
  $('#statRequestsSub').textContent = m.failures ? fmtNum(m.failures) + ' 次失败' : '无失败';

  const successRate = m.requests > 0 ? (m.successes / m.requests) * 100 : null;
  $('#statSuccess').textContent = successRate === null ? '—' : fmtPct(successRate);
  $('#statSuccessSub').textContent = fmtNum(m.successes) + ' / ' + fmtNum(m.requests);
  setGauge('#gaugeSuccess', successRate);

  $('#statCacheHit').textContent = (m.cache_hit_rate === null || m.cache_hit_rate === undefined) ? '—' : fmtPct(m.cache_hit_rate);
  $('#statCacheHitSub').textContent = fmtNum(m.cache_read_tokens) + ' 读取 tok';
  setGauge('#gaugeCacheHit', m.cache_hit_rate);

  $('#statCacheCoverage').textContent = (m.cache_coverage_percent === null || m.cache_coverage_percent === undefined) ? '—' : fmtPct(m.cache_coverage_percent);
  $('#statCacheCoverageSub').textContent = fmtNum(m.cache_supported_calls) + ' / ' + fmtNum(m.usage_calls) + ' 次调用';
  setGauge('#gaugeCoverage', m.cache_coverage_percent);

  $('#statTokens').textContent = fmtNum(m.input_tokens) + ' / ' + fmtNum(m.output_tokens);
  $('#statTokensSub').textContent = 'prompt ' + fmtNum(m.prompt_tokens);
  $('#statReasoning').textContent = fmtNum(m.reasoning_tokens);
  $('#statReasoningSub').textContent = 'cache write ' + fmtNum(m.cache_write_tokens);
}

/* ---------------- Sparklines ---------------- */
async function loadSparkSeries() {
  try {
    const data = await api('/logs?limit=30');
    state.sparkSeries = (data.items || []).slice().reverse();
    renderSparklines();
  } catch (e) { /* silent */ }
}

function sparkline(svg, values) {
  const W = 100, H = 28, pad = 3;
  if (!values.length) { svg.innerHTML = ''; return; }
  const max = Math.max.apply(null, values);
  const min = Math.min.apply(null, values);
  const range = (max - min) || 1;
  const n = values.length;
  const points = values.map((v, i) => {
    const x = n === 1 ? W / 2 : (i / (n - 1)) * (W - 2 * pad) + pad;
    const y = H - pad - ((v - min) / range) * (H - 2 * pad);
    return [x, y];
  });
  const linePath = 'M' + points.map(p => p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' L');
  const areaPath = linePath + ' L' + points[points.length - 1][0].toFixed(1) + ' ' + H + ' L' + points[0][0].toFixed(1) + ' ' + H + ' Z';
  const gid = 'spark-grad-' + Math.random().toString(36).slice(2, 8);
  svg.innerHTML =
    '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0%" stop-color="#2dd4bf" stop-opacity="0.5"/>' +
    '<stop offset="100%" stop-color="#2dd4bf" stop-opacity="0"/>' +
    '</linearGradient></defs>' +
    '<path class="spark-area" d="' + areaPath + '"/>' +
    '<path class="spark-fill" d="' + areaPath + '" fill="url(#' + gid + ')" stroke="none"/>' +
    '<path class="spark-line" d="' + linePath + '"/>';
}

function renderSparklines() {
  const logs = state.sparkSeries;
  const map = {
    requests: logs.map((_, i) => i + 1),
    success: logs.map(l => l.success ? 1 : 0),
    cacheHit: logs.map(l => {
      if (!l.usage || !l.usage.cache_supported || !l.usage.prompt_tokens) return 0;
      return (l.usage.cache_read_tokens / l.usage.prompt_tokens) * 100;
    }),
    cacheCoverage: logs.map(l => (l.usage && l.usage.cache_supported) ? 1 : 0),
    tokens: logs.map(l => {
      if (!l.usage) return 0;
      return (l.usage.input_tokens || l.usage.prompt_tokens || 0) + (l.usage.output_tokens || 0);
    }),
    reasoning: logs.map(l => (l.usage && l.usage.reasoning_tokens) || 0)
  };
  $all('.stat-spark').forEach(svg => {
    const key = svg.dataset.stat;
    if (map[key]) sparkline(svg, map[key]);
  });
}

/* ---------------- 网关脉冲 ---------------- */
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
      logs = (data.items || []).slice().reverse();
    } catch (e) { /* ignore per-gateway error */ }
    const successCount = logs.filter(l => l.success).length;
    const total = logs.length;
    const totalTokens = logs.reduce((acc, l) => acc + ((l.usage && (l.usage.input_tokens || l.usage.prompt_tokens || 0)) + ((l.usage && l.usage.output_tokens) || 0)), 0);

    const row = document.createElement('div');
    row.className = 'gw-pulse-row';
    row.innerHTML =
      '<div class="gw-pulse-top">' +
        '<span><span class="gw-pulse-name">' + escapeHtml(gw.name) + '</span><span class="gw-pulse-prefix">' + escapeHtml(gw.prefix || '') + '</span></span>' +
        '<span class="gw-pulse-stat">' + (total ? successCount + '/' + total + ' 成功 · ' + fmtNum(totalTokens) + ' tok' : '暂无数据') + '</span>' +
      '</div>' +
      '<div class="gw-pulse-bar">' + renderTicks(logs, 40) + '</div>';
    container.appendChild(row);

    const chan = document.createElement('div');
    chan.className = 'rail-channel';
    chan.innerHTML =
      '<div class="rail-channel-top">' +
        '<span class="rail-channel-name">' + escapeHtml(gw.name) + '</span>' +
        '<span class="rail-channel-prefix">' + escapeHtml(gw.prefix || '') + '</span>' +
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
    return '<span class="tick ' + cls + '" style="height:' + height + '%" title="' + escapeHtml(l.model || '') + ' · ' + (l.status_code || '') + ' · ' + fmtMs(l.duration_ms) + '"></span>';
  }).join('');
}

/* ---------------- 最近请求 ---------------- */
const EMPTY_RECENT_SVG = '<svg viewBox="0 0 60 60" width="48" height="48" fill="none"><circle cx="30" cy="30" r="26" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 4"/><path d="M20 32l5 5 15-15" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';

async function loadRecentLogs() {
  const container = $('#recentLogList');
  try {
    const data = await api('/logs?limit=12');
    const items = data.items || [];
    state.recentLogs = items;
    if (!items.length) {
      container.innerHTML = '<div class="empty-state">' + EMPTY_RECENT_SVG + '<span>暂无请求记录，等待流量接入…</span></div>';
      return;
    }
    container.innerHTML = items.map(l => {
      const gwClass = l.gateway_id || '';
      return (
        '<div class="recent-log-item" data-id="' + l.id + '">' +
          '<span class="rli-gw ' + gwClass + '">' + escapeHtml(l.gateway_id || '?') + '</span>' +
          '<span class="rli-model" title="' + escapeHtml(l.model || '') + '">' + escapeHtml(l.model || '(未知模型)') + '</span>' +
          '<span class="rli-time">' + fmtTimeShort(l.started_at) + '</span>' +
          '<span class="rli-status" style="background:' + (l.success ? 'var(--emerald)' : 'var(--rose)') + ';color:' + (l.success ? 'var(--emerald)' : 'var(--rose)') + '"></span>' +
        '</div>'
      );
    }).join('');
    $all('.recent-log-item', container).forEach(el => {
      el.addEventListener('click', () => openDrawer(el.dataset.id));
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state"><span>加载失败：' + escapeHtml(e.message) + '</span></div>';
  }
}

$('#rangePicker').addEventListener('click', (e) => {
  const btn = e.target.closest('button[data-range]');
  if (!btn) return;
  state.range = btn.dataset.range;
  $all('#rangePicker button').forEach(b => b.classList.toggle('is-active', b === btn));
  $('#pulseRange').textContent = rangeLabel(state.range);
  loadMetrics();
});

/* ============================================================
   Logs 视图
   ============================================================ */
function currentFilters() {
  return {
    gateway: $('#filterGateway').value,
    model: $('#filterModel').value.trim(),
    status: $('#filterStatus').value,
    limit: $('#filterLimit').value
  };
}

function renderSkeletons(n) {
  let html = '';
  for (let i = 0; i < n; i++) {
    html += '<tr><td colspan="8" style="padding:11px 14px;"><span class="skeleton-row" style="width:' + (50 + Math.random() * 50) + '%"></span></td></tr>';
  }
  return html;
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
  if (reset) tbody.innerHTML = renderSkeletons(8);
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

const EMPTY_LOGS_SVG = '<svg viewBox="0 0 60 60" width="44" height="44" fill="none"><rect x="10" y="10" width="40" height="40" rx="6" stroke="currentColor" stroke-width="1.4"/><path d="M16 22h28M16 30h28M16 38h18" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>';

function renderLogTable(logs) {
  const tbody = $('#logTableBody');
  if (!logs.length) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="8"><div style="display:flex;flex-direction:column;align-items:center;gap:10px;padding:8px;">' + EMPTY_LOGS_SVG + '<span>没有匹配的请求记录</span></div></td></tr>';
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
        '<td><span class="gw-tag ' + gwClass + '">' + escapeHtml(l.gateway_name || l.gateway_id || '?') + '</span></td>' +
        '<td>' + escapeHtml(l.model || '—') + '</td>' +
        '<td><span class="status-pill ' + (l.success ? 'ok' : 'err') + '">' + (l.success ? l.status_code : (l.status_code || '错误')) + '</span></td>' +
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
    showToast('已清空 ' + (res.deleted || 0) + ' 条日志', 'success');
    loadLogs(true);
    loadOverview();
  } catch (e) {
    showToast('清空失败: ' + e.message, 'error');
  }
});

/* ============================================================
   请求详情抽屉
   ============================================================ */
async function openDrawer(id) {
  $('#drawerBackdrop').classList.add('is-open');
  const drawer = $('#logDrawer');
  drawer.classList.add('is-open');
  drawer.setAttribute('aria-hidden', 'false');
  $('#drawerId').textContent = id;
  $('#drawerTitle').textContent = '加载中…';
  $('#drawerMeta').innerHTML = '';
  $('#drawerCode').textContent = '加载中…';
  $('#drawerCompare').innerHTML = '';
  try {
    const log = await api('/logs/' + id);
    state.drawerLog = log;
    state.drawerTab = 'request-compare';
    $all('.drawer-tab').forEach(t => t.classList.toggle('is-active', t.dataset.tab === 'request-compare'));
    renderDrawer(log);
  } catch (e) {
    $('#drawerTitle').textContent = '加载失败';
    $('#drawerCode').textContent = e.message;
    showToast('加载请求详情失败: ' + e.message, 'error');
  }
}

function renderDrawer(log) {
  $('#drawerTitle').textContent = (log.model || '(未知模型)') + ' · ' + (log.gateway_name || log.gateway_id);
  $('#drawerMeta').innerHTML = [
    ['时间', fmtTime(log.started_at)],
    ['状态', (log.success ? '成功 ' : '失败 ') + (log.status_code || '')],
    ['耗时', fmtMs(log.duration_ms)],
    ['流式', log.stream ? '是' : '否'],
    ['方法', (log.method || '—') + ' ' + (log.request_path || '')],
    ['上游', log.upstream_url || '—']
  ].map(([k, v]) => '<span class="meta-chip">' + escapeHtml(k) + ': <b>' + escapeHtml(String(v)) + '</b></span>').join('');
  if (log.error) {
    $('#drawerMeta').innerHTML += '<span class="meta-chip error">错误: <b>' + escapeHtml(log.error) + '</b></span>';
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
  else if (tab === 'headers') content =
    tryPretty(log.request_headers_actual || log.request_headers) +
    '\n\n--- upstream request headers ---\n\n' + tryPretty(log.upstream_headers_actual || log.upstream_headers) +
    '\n\n--- upstream response headers ---\n\n' + tryPretty(log.upstream_response_headers_actual || log.upstream_response_headers) +
    '\n\n--- client response headers ---\n\n' + tryPretty(log.response_headers_actual || log.response_headers);
  code.textContent = content || '(空)';
}

function renderComparison(tab, log) {
  const request = tab === 'request-compare';
  const left = request ? {
    label: '客户端原请求',
    url: (log.method || 'POST') + ' ' + (log.request_url || log.request_path || '—'),
    headers: log.request_headers_actual || log.request_headers,
    body: log.request_body
  } : {
    label: '上游 API 原始响应',
    url: 'HTTP ' + (log.upstream_response_status_code || log.status_code || '—'),
    headers: log.upstream_response_headers_actual || log.upstream_response_headers,
    body: log.upstream_response_body || log.response_body
  };
  const right = request ? {
    label: '代理实际发送',
    url: (log.method || 'POST') + ' ' + (log.upstream_url || '—'),
    headers: log.upstream_headers_actual || log.upstream_headers,
    body: log.upstream_body
  } : {
    label: '代理实际返回客户端',
    url: 'HTTP ' + (log.client_response_status_code || log.status_code || '—'),
    headers: log.response_headers_actual || log.response_headers,
    body: log.response_body
  };
  const headerDiff = buildDiff(left.headers, right.headers, 'headers');
  const bodyDiff = buildDiff(left.body, right.body, 'body');
  const statusDiff = request ? null : buildValueDiff('HTTP 状态码', log.upstream_response_status_code || log.status_code, log.client_response_status_code || log.status_code);
  const total = diffStats([headerDiff, bodyDiff, statusDiff]);
  $('#drawerCompare').innerHTML =
    '<div class="diff-overview">' +
      '<div class="diff-route"><span>' + escapeHtml(left.label) + '</span> <b>→</b> <span>' + escapeHtml(right.label) + '</span></div>' +
      '<div class="diff-endpoints"><code>' + escapeHtml(left.url) + '</code> <b>→</b> <code>' + escapeHtml(right.url) + '</code></div>' +
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
  if (beforeJSON.ok && afterJSON.ok) return { rows: buildJSONDiff(beforeJSON.value, afterJSON.value, category) };
  return { rows: buildTextDiff(before, after) };
}
function tryParseJSON(raw) {
  if (!raw.trim()) return { ok: false };
  try { return { ok: true, value: JSON.parse(raw) }; } catch (e) { return { ok: false }; }
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
      rows.push({ kind: 'modified', path, before: beforeValue, after: afterValue, explanation: explainJSONChange(path, 'modified', beforeValue, afterValue, category) });
    }
  });
  return rows;
}
function explainJSONChange(path, kind, before, after, category) {
  if (category === 'headers') {
    const header = headerNameFromPath(path);
    const lower = header.toLowerCase();
    if (kind === 'deleted' && hopByHopHeaderNames().includes(lower)) return '删除请求头 ' + header + '：HTTP hop-by-hop 头只属于当前连接，不应转发到下一跳';
    if (kind === 'deleted' && lower === 'content-length') return '删除请求头 Content-Length：代理会根据实际发送的正文重新管理长度';
    if (kind === 'deleted' && lower.startsWith('x-grok-')) return '删除请求头 ' + header + '：这是客户端/代理内部头，代理不会转发';
    if (kind === 'deleted') return '删除请求头 ' + header + '：未包含在当前网关的 Forward Headers 白名单中';
    if (kind === 'added' && lower === 'user-agent') return '新增请求头 User-Agent：当前网关启用了 User-Agent 覆盖或客户端头被代理补齐';
    if (kind === 'added') return '新增请求头 ' + header + '：由代理转发白名单或代理策略加入';
    return '修改请求头 ' + header + '：代理策略改变了该值';
  }
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function"' && after === '"function_call"') return '商汤协议兼容：将客户端 tool_calls 类型 function 转为上游要求的 function_call';
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function_call"' && after === '"function"') return '客户端协议兼容：将商汤返回的 function_call 转回客户端使用的 function';
  if (kind === 'added') return '新增字段 ' + path;
  if (kind === 'deleted') return '删除字段 ' + path;
  return '修改字段 ' + path;
}
function headerNameFromPath(path) {
  const dotMatch = path.match(/^\$\.([A-Za-z0-9_-]+)/);
  if (dotMatch) return dotMatch[1];
  const bracketMatch = path.match(/^\$\["([^"]+)"\]/);
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
    return '<section class="diff-section diff-section-empty"><div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-no-change">无变化</span></div></section>';
  }
  return '<section class="diff-section">' +
    '<div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-change-count">' + changedRows.length + ' 处变更</span></div>' +
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
  try { return JSON.stringify(JSON.parse(raw), null, 2); } catch (e) { return raw; }
}

$('#drawerTabs').addEventListener('click', (e) => {
  const btn = e.target.closest('.drawer-tab');
  if (!btn) return;
  state.drawerTab = btn.dataset.tab;
  $all('.drawer-tab').forEach(t => t.classList.toggle('is-active', t === btn));
  renderDrawerTab(state.drawerTab);
});
$('#drawerCopyIdBtn').addEventListener('click', () => {
  const id = $('#drawerId').textContent.trim();
  if (!id || id === '—') return;
  navigator.clipboard.writeText(id).then(() => showToast('已复制请求 ID: ' + id, 'success'));
});
function closeDrawer() {
  $('#drawerBackdrop').classList.remove('is-open');
  const drawer = $('#logDrawer');
  drawer.classList.remove('is-open');
  drawer.setAttribute('aria-hidden', 'true');
}
$('#drawerCloseBtn').addEventListener('click', closeDrawer);
$('#drawerBackdrop').addEventListener('click', closeDrawer);

/* ============================================================
   网关配置
   ============================================================ */
function renderGatewayCards() {
  const container = $('#gatewayCards');
  if (!container) return;
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
          '<span class="gw-card-prefix">' + escapeHtml(gw.prefix || '') + '</span>' +
        '</div>' +
        '<div class="toggle-row compact-toggle-row">' +
          '<span class="switch-label">网关启用</span>' +
          '<label class="toggle"><input type="checkbox" class="f-enabled" ' + (gw.enabled ? 'checked' : '') + '><span class="toggle-track"></span></label>' +
        '</div>' +
      '</div>' +
      '<div class="gw-card-protocol">协议 · ' + (gw.protocol === 'chat_completions' ? 'Chat Completions' : 'Responses') + '</div>' +
      '<div class="field">' +
        '<label>显示名称</label>' +
        '<input type="text" class="f-name" value="' + escapeAttr(gw.name) + '">' +
      '</div>' +
      '<div class="field">' +
        '<label>上游地址 (base_url)</label>' +
        '<input type="text" class="f-baseurl" value="' + escapeAttr(gw.base_url) + '" placeholder="https://…">' +
        '<span class="field-hint">必须是 HTTPS 地址</span>' +
      '</div>' +
      '<div class="toggle-row ua-toggle-row">' +
        '<div><span class="switch-label">User-Agent 覆盖</span><span class="field-hint">仅作用于此网关</span></div>' +
        '<label class="toggle"><input type="checkbox" class="f-ua-enabled" ' + (gw.user_agent_override_enabled ? 'checked' : '') + '><span class="toggle-track"></span></label>' +
      '</div>' +
      '<div class="field">' +
        '<label>User-Agent 覆盖值</label>' +
        '<input type="text" class="f-ua" value="' + escapeAttr(gw.user_agent_override || '') + '" placeholder="grok-gateway-proxy/dev">' +
      '</div>' +
      '<div class="field">' +
        '<label>请求头白名单（每行一个）</label>' +
        '<textarea class="f-headers" rows="4" placeholder="Authorization&#10;X-Api-Key">' + escapeHtml(headers) + '</textarea>' +
      '</div>' +
      '<div class="gw-card-foot">' +
        '<button class="btn-primary small save-gw-btn" style="width:auto;padding:8px 18px;font-size:12px;">' +
          '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M4 8l3 3 5-6" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>' +
          '保存更改' +
        '</button>' +
      '</div>';
    container.appendChild(card);
    card.querySelector('.save-gw-btn').addEventListener('click', () => saveGateway(id, card));
  });
}

async function saveGateway(id, card) {
  const btn = card.querySelector('.save-gw-btn');
  const original = btn.innerHTML;
  const payload = {
    name: card.querySelector('.f-name').value.trim(),
    base_url: card.querySelector('.f-baseurl').value.trim(),
    enabled: card.querySelector('.f-enabled').checked,
    user_agent_override_enabled: card.querySelector('.f-ua-enabled').checked,
    user_agent_override: card.querySelector('.f-ua').value.trim(),
    forward_headers: card.querySelector('.f-headers').value.split('\n').map(s => s.trim()).filter(Boolean)
  };
  btn.disabled = true;
  btn.innerHTML = '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M8 2v3M8 11v3M2 8h3M11 8h3" stroke="currentColor" stroke-width="1.6" fill="none" stroke-linecap="round"/></svg>保存中…';
  try {
    const gateways = {};
    gateways[id] = Object.assign({}, state.gateways[id], payload);
    const res = await api('/gateways', { method: 'PUT', body: JSON.stringify({ gateways }) });
    state.gateways = res.gateways;
    showToast('网关 ' + payload.name + ' 已更新', 'success');
    renderGatewayCards();
  } catch (e) {
    showToast('保存失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
    btn.innerHTML = original;
  }
}

$('#gatewaysReloadBtn').addEventListener('click', async () => {
  await loadConfig();
  renderGatewayCards();
  showToast('已重新加载网关配置', 'success');
});

/* ============================================================
   接入代码
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
          '<strong>' + escapeHtml(gw.name || id) + ' <span class="gw-card-prefix" style="font-size:10.5px;">' + escapeHtml(gw.prefix || '') + '</span></strong>' +
          '<button class="btn-ghost small copy-btn" style="width:auto;padding:6px 12px;">' +
            '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><rect x="5" y="5" width="8" height="8" rx="1.6" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="M3 11V3.6A1.6 1.6 0 0 1 4.6 2H11" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round"/></svg>' +
            '复制' +
          '</button>' +
        '</div>' +
        '<pre class="setup-pre">' + escapeHtml(snippet) + '</pre>';
      container.appendChild(card);
      card.querySelector('.copy-btn').addEventListener('click', () => {
        navigator.clipboard.writeText(snippet).then(() => showToast('已复制到剪贴板', 'success'));
      });
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}
$('#setupReloadBtn').addEventListener('click', () => {
  loadSetup();
  showToast('已重新加载接入代码', 'success');
});

/* ============================================================
   命令面板 (Cmd+K)
   ============================================================ */
function buildCmdkItems() {
  const items = [
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><rect x="2" y="2" width="5" height="5" rx="1" fill="currentColor"/><rect x="9" y="2" width="5" height="5" rx="1" fill="currentColor" opacity=".55"/><rect x="2" y="9" width="5" height="5" rx="1" fill="currentColor" opacity=".55"/><rect x="9" y="9" width="5" height="5" rx="1" fill="currentColor"/></svg>', label: '前往 总览', hint: '1', run: () => switchView('overview') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M3 3.5h10M3 6h10M3 8.5h7M3 11h5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" fill="none"/></svg>', label: '前往 请求日志', hint: '2', run: () => switchView('logs') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><circle cx="8" cy="8" r="2" fill="currentColor"/><path d="M8 1.5v2.5M8 12v2.5M1.5 8h2.5M12 8h2.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" fill="none"/></svg>', label: '前往 网关配置', hint: '3', run: () => switchView('gateways') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M5 4l-3 4 3 4M11 4l3 4-3 4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" fill="none"/></svg>', label: '前往 接入代码', hint: '4', run: () => switchView('setup') },
    { group: '操作', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M2.5 8a5.5 5.5 0 0 1 9.4-3.9M13.5 8a5.5 5.5 0 0 1-9.4 3.9" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round"/><path d="M11 1.5v3h-3M5 14.5v-3h3" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>', label: '刷新全部数据', hint: 'r', run: () => { refreshAll(); } },
    { group: '操作', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M3 4h10M5.5 4V2.5h5V4M5 4l.5 9h5l.5-9" stroke="currentColor" stroke-width="1.3" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>', label: '清空请求日志', hint: '', run: async () => {
      if (!confirm('确定要清空全部请求日志吗？此操作无法撤销。')) return;
      try {
        const res = await api('/logs', { method: 'DELETE' });
        showToast('已清空 ' + (res.deleted || 0) + ' 条日志', 'success');
        if (state.activeView === 'logs') loadLogs(true);
        loadOverview();
      } catch (e) { showToast('清空失败: ' + e.message, 'error'); }
    } }
  ];
  GW_ORDER.forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    items.push({
      group: '复制接入片段',
      icon: '<svg viewBox="0 0 16 16" width="13" height="13"><rect x="5" y="5" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.3" fill="none"/><path d="M3 11V3.5A1.5 1.5 0 0 1 4.5 2H11" stroke="currentColor" stroke-width="1.3" fill="none" stroke-linecap="round"/></svg>',
      label: '复制 ' + gw.name + ' 接入片段 (' + gw.prefix + ')',
      hint: '',
      run: async () => {
        try {
          const data = await api('/setup');
          if (data[id]) {
            navigator.clipboard.writeText(data[id]).then(() => showToast('已复制 ' + gw.name + ' 接入片段', 'success'));
          }
        } catch (e) { showToast('复制失败: ' + e.message, 'error'); }
      }
    });
  });
  return items;
}

function openCmdk() {
  state.cmdkItems = buildCmdkItems();
  state.cmdkSelected = 0;
  $('#cmdkInput').value = '';
  renderCmdk('');
  $('#cmdkBackdrop').classList.add('is-open');
  const cmdk = $('#cmdk');
  cmdk.classList.add('is-open');
  cmdk.setAttribute('aria-hidden', 'false');
  setTimeout(() => $('#cmdkInput').focus(), 80);
}
function closeCmdk() {
  $('#cmdkBackdrop').classList.remove('is-open');
  const cmdk = $('#cmdk');
  cmdk.classList.remove('is-open');
  cmdk.setAttribute('aria-hidden', 'true');
}
function renderCmdk(query) {
  const list = $('#cmdkList');
  const q = query.trim().toLowerCase();
  const filtered = q ? state.cmdkItems.filter(it => it.label.toLowerCase().includes(q) || it.group.toLowerCase().includes(q)) : state.cmdkItems;
  if (!filtered.length) {
    list.innerHTML = '<div class="cmdk-empty">没有匹配的操作</div>';
    return;
  }
  let lastGroup = '';
  let html = '';
  filtered.forEach((it, i) => {
    if (it.group !== lastGroup) {
      html += '<div class="cmdk-group">' + escapeHtml(it.group) + '</div>';
      lastGroup = it.group;
    }
    const isActive = i === state.cmdkSelected ? ' is-active' : '';
    html += '<div class="cmdk-item' + isActive + '" data-index="' + i + '"><span class="cmdk-item-icon">' + it.icon + '</span><span class="cmdk-item-label">' + escapeHtml(it.label) + '</span>' + (it.hint ? '<kbd>' + escapeHtml(it.hint) + '</kbd>' : '') + '</div>';
  });
  list.innerHTML = html;
  $all('.cmdk-item', list).forEach(el => {
    el.addEventListener('click', () => {
      const i = Number(el.dataset.index);
      const item = filtered[i];
      if (item) { closeCmdk(); item.run(); }
    });
    el.addEventListener('mouseenter', () => {
      state.cmdkSelected = Number(el.dataset.index);
      $all('.cmdk-item', list).forEach(x => x.classList.toggle('is-active', x === el));
    });
  });
}
$('#cmdKBtn').addEventListener('click', openCmdk);
$('#cmdkBackdrop').addEventListener('click', closeCmdk);
$('#cmdkInput').addEventListener('input', (e) => {
  state.cmdkSelected = 0;
  renderCmdk(e.target.value);
});
$('#cmdkInput').addEventListener('keydown', (e) => {
  const q = e.target.value.trim().toLowerCase();
  const filtered = q ? state.cmdkItems.filter(it => it.label.toLowerCase().includes(q) || it.group.toLowerCase().includes(q)) : state.cmdkItems;
  if (e.key === 'ArrowDown') { e.preventDefault(); state.cmdkSelected = (state.cmdkSelected + 1) % filtered.length; renderCmdk(e.target.value); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); state.cmdkSelected = (state.cmdkSelected - 1 + filtered.length) % filtered.length; renderCmdk(e.target.value); }
  else if (e.key === 'Enter') {
    e.preventDefault();
    const item = filtered[state.cmdkSelected];
    if (item) { closeCmdk(); item.run(); }
  }
});

/* ============================================================
   键盘快捷键
   ============================================================ */
document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault();
    if ($('#cmdk').classList.contains('is-open')) closeCmdk(); else openCmdk();
    return;
  }
  if (e.key === 'Escape') {
    if ($('#cmdk').classList.contains('is-open')) { closeCmdk(); return; }
    if ($('#logDrawer').classList.contains('is-open')) { closeDrawer(); return; }
  }
  const tag = (e.target.tagName || '').toLowerCase();
  const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;
  if (inField) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === '1') switchView('overview');
  else if (e.key === '2') switchView('logs');
  else if (e.key === '3') switchView('gateways');
  else if (e.key === '4') switchView('setup');
  else if (e.key === 'r' || e.key === 'R') refreshAll();
});

/* ============================================================
   刷新 + 启动
   ============================================================ */
async function refreshAll() {
  await loadConfig();
  if (state.activeView === 'overview') loadOverview();
  if (state.activeView === 'logs') loadLogs(true);
  if (state.activeView === 'gateways') renderGatewayCards();
  if (state.activeView === 'setup') loadSetup();
  showToast('已刷新', 'success');
}
$('#refreshAllBtn').addEventListener('click', refreshAll);

async function boot() {
  try {
    await loadConfig();
    await loadOverview();
  } catch (e) {
    showToast('初始化失败: ' + e.message, 'error');
  }
  state.pollTimer = setInterval(() => {
    if (state.activeView === 'overview') loadOverview();
  }, 15000);
}
boot();
