'use strict';

import { state } from './state.js';
import { api } from './api.js';
import { $, $all, fmtNum, fmtPct, fmtTime, fmtMs, escapeHtml } from './utils.js';
import { openDrawer } from './drawer.js';
import { showToast, confirmModal } from './ui.js';
import { loadOverview } from './overview.js';

/* ============================================================
   请求日志视图
   ============================================================ */

const EMPTY_LOGS_SVG = '<svg viewBox="0 0 60 60" width="44" height="44" fill="none"><rect x="10" y="10" width="40" height="40" rx="6" stroke="currentColor" stroke-width="1.4"/><path d="M16 22h28M16 30h28M16 38h18" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>';

// 单调递增序号：慢响应回来时若已被更新查询取代，直接丢弃，避免旧数据覆盖新结果。
let logsSeq = 0;
let modelDebounce;

function currentFilters() {
  const from = $('#filterFrom').value;
  const to = $('#filterTo').value;
  return {
    gateway: $('#filterGateway').value,
    model: $('#filterModel').value.trim(),
    status: $('#filterStatus').value,
    limit: $('#filterLimit').value,
    from: from ? new Date(from).toISOString() : '',
    to: to ? new Date(to).toISOString() : ''
  };
}

function filterQuery(extra) {
  const f = currentFilters();
  const params = new URLSearchParams();
  if (f.gateway) params.set('gateway', f.gateway);
  if (f.model) params.set('model', f.model);
  if (f.status) params.set('status', f.status);
  if (f.from) params.set('from', f.from);
  if (f.to) params.set('to', f.to);
  if (extra) Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

function renderSkeletons(n) {
  let html = '';
  for (let i = 0; i < n; i++) {
    html += '<tr><td colspan="8" style="padding:11px 14px;"><span class="skeleton-row" style="width:' + (50 + Math.random() * 50) + '%"></span></td></tr>';
  }
  return html;
}

function logRowHTML(l) {
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
      '<td title="总输入=' + fmtNum(l.usage ? l.usage.prompt_tokens : 0) + ' (uncached ' + fmtNum(l.usage ? l.usage.input_tokens : 0) + ' + cached ' + fmtNum(l.usage ? l.usage.cache_read_tokens : 0) + ')">' + (l.usage ? fmtNum(l.usage.prompt_tokens || l.usage.input_tokens) + ' / ' + fmtNum(l.usage.output_tokens) : '—') + '</td>' +
      '<td>' + cacheHit + '</td>' +
    '</tr>'
  );
}

function bindRowClicks(scope) {
  $all('tr[data-id]', scope).forEach(tr => {
    tr.addEventListener('click', () => openDrawer(tr.dataset.id));
  });
}

// “加载更多”时只追加新行：保留滚动位置，且只为新行绑定事件（避免旧行重复绑定）。
function appendLogRows(items) {
  const tbody = $('#logTableBody');
  const emptyRow = tbody.querySelector('tr.empty-row');
  if (emptyRow) emptyRow.remove();
  const frag = document.createElement('tbody');
  frag.innerHTML = items.map(logRowHTML).join('');
  bindRowClicks(frag);
  while (frag.firstChild) tbody.appendChild(frag.firstChild);
}

export async function loadLogs(reset) {
  if (reset) {
    state.logsOffset = 0;
    state.logs = [];
  }
  const seq = ++logsSeq;
  const f = currentFilters();
  const tbody = $('#logTableBody');
  if (reset) tbody.innerHTML = renderSkeletons(8);
  try {
    const [data, countData] = await Promise.all([
      api('/logs?' + filterQuery({ limit: f.limit, offset: String(state.logsOffset) })),
      api('/logs/count?' + filterQuery())
    ]);
    if (seq !== logsSeq) return; // 过期响应，丢弃
    const items = data.items || [];
    if (reset) state.logs = items;
    else state.logs = state.logs.concat(items);
    state.logsTotal = (countData && typeof countData.count === 'number') ? countData.count : null;
    // reset 时整表重渲染；“加载更多”只追加新行，避免整表重建与滚动跳动。
    if (reset) {
      renderLogTable();
    } else if (items.length) {
      appendLogRows(items);
    }
    state.logsOffset += items.length;
    // 内存保护：DOM 里保留所有已加载行，state 里最多留最近 1000 条。
    if (state.logs.length > 2000) state.logs = state.logs.slice(-1000);
    $('#logsLoadMoreBtn').style.display =
      (state.logsTotal === null || items.length < Number(f.limit)) ? 'none' : 'inline-flex';
    renderLogsCount();
  } catch (e) {
    if (seq !== logsSeq) return;
    tbody.innerHTML = '<tr class="empty-row"><td colspan="8">加载失败：' + escapeHtml(e.message) + '</td></tr>';
  }
}

function renderLogsCount() {
  const el = $('#logsCount');
  if (!el) return;
  if (state.logsTotal === null) { el.textContent = ''; return; }
  el.textContent = '共 ' + fmtNum(state.logsTotal) + ' 条 · 已加载 ' + fmtNum(state.logsOffset) + ' 条';
}

function renderLogTable() {
  const tbody = $('#logTableBody');
  if (!state.logs.length) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="8"><div style="display:flex;flex-direction:column;align-items:center;gap:10px;padding:8px;">' + EMPTY_LOGS_SVG + '<span>没有匹配的请求记录</span></div></td></tr>';
    bindRowClicks(tbody);
    return;
  }
  tbody.innerHTML = state.logs.map(logRowHTML).join('');
  bindRowClicks(tbody);
}

export function initLogs() {
  $('#logsRefreshBtn').addEventListener('click', () => loadLogs(true));
  $('#logsLoadMoreBtn').addEventListener('click', () => loadLogs(false));
  $('#filterGateway').addEventListener('change', () => loadLogs(true));
  $('#filterStatus').addEventListener('change', () => loadLogs(true));
  $('#filterLimit').addEventListener('change', () => loadLogs(true));
  $('#filterFrom').addEventListener('change', () => loadLogs(true));
  $('#filterTo').addEventListener('change', () => loadLogs(true));
  $('#filterClearBtn').addEventListener('click', () => {
    ['filterGateway', 'filterStatus', 'filterFrom', 'filterTo'].forEach(id => { $('#' + id).value = ''; });
    $('#filterModel').value = '';
    loadLogs(true);
  });
  $('#filterModel').addEventListener('input', () => {
    clearTimeout(modelDebounce);
    modelDebounce = setTimeout(() => loadLogs(true), 350);
  });
  $('#logsClearBtn').addEventListener('click', async () => {
    if (!(await confirmModal('确定要清空全部请求日志吗？此操作无法撤销。', '清空日志'))) return;
    try {
      const res = await api('/logs', { method: 'DELETE' });
      showToast('已清空 ' + (res.deleted || 0) + ' 条日志', 'success');
      loadLogs(true);
      loadOverview();
    } catch (e) {
      showToast('清空失败: ' + e.message, 'error');
    }
  });
}
