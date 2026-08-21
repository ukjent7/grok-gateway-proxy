'use strict';

/* ============================================================
   Grok Gateway Console · 请求日志视图
   - 高级筛选器 & 快速状态过滤
   - 响应式数据表格与微交互
   - 导出日志 (JSON / CSV)
   - 分页与骨架屏加载
   ============================================================ */

import { state } from './state.js';
import { api } from './api.js';
import {
  $, $all, fmtNum, fmtPct, fmtTime, fmtTimeRelative, fmtMs,
  escapeHtml, copyText, downloadFile
} from './utils.js';
import { openDrawer } from './drawer.js';
import { showToast, confirmModal } from './ui.js';
import { loadOverview } from './overview.js';

const EMPTY_LOGS_SVG = `
  <svg viewBox="0 0 64 64" width="48" height="48" fill="none" class="empty-icon">
    <rect x="12" y="12" width="40" height="40" rx="8" stroke="currentColor" stroke-width="1.5" stroke-dasharray="3 3" opacity="0.4"/>
    <path d="M20 26h24M20 34h18M20 42h12" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" opacity="0.6"/>
  </svg>
`;

let logsSeq = 0;
let modelDebounce;

export function currentFilters() {
  const from = $('#filterFrom') ? $('#filterFrom').value : '';
  const to = $('#filterTo') ? $('#filterTo').value : '';
  const gw = $('#filterGateway') ? $('#filterGateway').value : '';
  const model = $('#filterModel') ? $('#filterModel').value.trim() : '';
  const status = $('#filterStatus') ? $('#filterStatus').value : '';
  const limit = $('#filterLimit') ? $('#filterLimit').value : '50';

  return {
    gateway: gw,
    model: model,
    status: status,
    limit: limit || '50',
    from: from ? new Date(from).toISOString() : '',
    to: to ? new Date(to).toISOString() : ''
  };
}

function filterQuery(extra = {}) {
  const f = currentFilters();
  const params = new URLSearchParams();
  if (f.gateway) params.set('gateway', f.gateway);
  if (f.model) params.set('model', f.model);
  if (f.status) params.set('status', f.status);
  if (f.from) params.set('from', f.from);
  if (f.to) params.set('to', f.to);
  Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

function renderSkeletons(n) {
  let html = '';
  for (let i = 0; i < n; i++) {
    html += `
      <tr class="skeleton-tr">
        <td><div class="skeleton-bar" style="width: 80px;"></div></td>
        <td><div class="skeleton-bar" style="width: 70px;"></div></td>
        <td><div class="skeleton-bar" style="width: 140px;"></div></td>
        <td><div class="skeleton-bar" style="width: 50px;"></div></td>
        <td><div class="skeleton-bar" style="width: 60px;"></div></td>
        <td><div class="skeleton-bar" style="width: 30px;"></div></td>
        <td><div class="skeleton-bar" style="width: 90px;"></div></td>
        <td><div class="skeleton-bar" style="width: 50px;"></div></td>
        <td><div class="skeleton-bar" style="width: 60px;"></div></td>
      </tr>
    `;
  }
  return html;
}

function logRowHTML(l) {
  const gwId = l.gateway_id || 'unknown';
  const usage = l.usage || {};
  const promptTokens = usage.prompt_tokens || usage.input_tokens || 0;
  const outputTokens = usage.output_tokens || 0;
  const reasoningTokens = usage.reasoning_tokens || 0;
  const cacheReadTokens = usage.cache_read_tokens || 0;
  const cacheWriteTokens = usage.cache_write_tokens || 0;

  let cacheHit = '—';
  let cacheHitCls = 'cache-neutral';
  if (usage.usage_present && usage.cache_supported && promptTokens > 0) {
    const rate = (cacheReadTokens / promptTokens) * 100;
    cacheHit = fmtPct(rate);
    if (rate > 50) cacheHitCls = 'cache-high';
    else if (rate > 0) cacheHitCls = 'cache-mid';
    else cacheHitCls = 'cache-zero';
  }

  const duration = l.duration_ms || 0;
  let latencyCls = 'latency-fast';
  if (duration > 1500) latencyCls = 'latency-slow';
  else if (duration > 500) latencyCls = 'latency-mid';

  const tokenTooltip = `总输入: ${fmtNum(promptTokens)} (未缓存: ${fmtNum(usage.input_tokens || 0)}, 缓存读取: ${fmtNum(cacheReadTokens)}) | 输出: ${fmtNum(outputTokens)} | 推理: ${fmtNum(reasoningTokens)} | 缓存写入: ${fmtNum(cacheWriteTokens)}`;

  return `
    <tr data-id="${escapeHtml(l.id)}" tabindex="0" role="row">
      <td class="cell-time">
        <div class="time-main" title="${fmtTime(l.started_at)}">${fmtTimeShort(l.started_at)}</div>
        <div class="time-sub">${fmtTimeRelative(l.started_at)}</div>
      </td>
      <td class="cell-gw">
        <span class="gw-badge gw-badge-${escapeHtml(gwId)}">
          <span class="gw-dot gw-dot-${escapeHtml(gwId)}"></span>
          ${escapeHtml(l.gateway_name || gwId)}
        </span>
      </td>
      <td class="cell-model">
        <div class="model-name" title="${escapeHtml(l.model || '')}">
          ${escapeHtml(l.model || '—')}
        </div>
      </td>
      <td class="cell-status">
        <span class="status-badge ${l.success ? 'is-success' : 'is-error'}">
          <span class="status-dot"></span>
          ${l.status_code || (l.success ? '200' : 'ERR')}
        </span>
      </td>
      <td class="cell-duration">
        <span class="latency-chip ${latencyCls}">
          ${fmtMs(duration)}
        </span>
      </td>
      <td class="cell-stream">
        ${l.stream
          ? '<span class="stream-pill is-stream" title="流式响应 (SSE)"><svg viewBox="0 0 16 16" width="10" height="10" fill="currentColor"><path d="M1 8a7 7 0 0 1 14 0A7 7 0 0 1 1 8zm7-5a5 5 0 0 0-5 5h1a4 4 0 0 1 4-4V3zm0 2a3 3 0 0 0-3 3h1a2 2 0 0 1 2-2V5z"/></svg>SSE</span>'
          : '<span class="stream-pill is-sync" title="标准单次响应">Sync</span>'}
      </td>
      <td class="cell-tokens" title="${escapeHtml(tokenTooltip)}">
        ${usage.usage_present
          ? `<span class="tokens-val"><b>${fmtNum(promptTokens)}</b> / ${fmtNum(outputTokens)}</span>`
          : '<span class="muted">—</span>'}
      </td>
      <td class="cell-cache">
        <span class="cache-rate-badge ${cacheHitCls}">
          ${cacheHit}
        </span>
      </td>
      <td class="cell-actions">
        <div class="row-actions">
          <button type="button" class="btn-row-action btn-copy-curl" title="复制 cURL 命令" aria-label="复制 cURL 命令">
            <svg viewBox="0 0 16 16" width="11" height="11" fill="none"><path d="M5 4l-3 4 3 4M11 4l3 4-3 4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>
          </button>
          <button type="button" class="btn-row-action btn-view-detail" title="查看请求与响应详情" aria-label="查看详情">
            <svg viewBox="0 0 16 16" width="11" height="11" fill="none"><path d="M2 8s3-5 6-5 6 5 6 5-3 5-6 5-6-5-6-5z" stroke="currentColor" stroke-width="1.3"/><circle cx="8" cy="8" r="2" stroke="currentColor" stroke-width="1.3"/></svg>
          </button>
        </div>
      </td>
    </tr>
  `;
}

function bindRowClicks(scope) {
  $all('tr[data-id]', scope).forEach(tr => {
    const id = tr.dataset.id;
    
    // Clicking row opens drawer
    tr.addEventListener('click', (e) => {
      if (e.target.closest('.btn-row-action')) return;
      openDrawer(id);
    });

    tr.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        openDrawer(id);
      }
    });

    // Detail button
    const detailBtn = tr.querySelector('.btn-view-detail');
    if (detailBtn) {
      detailBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        openDrawer(id);
      });
    }

    // Copy cURL button
    const curlBtn = tr.querySelector('.btn-copy-curl');
    if (curlBtn) {
      curlBtn.addEventListener('click', async (e) => {
        e.stopPropagation();
        try {
          const log = await api('/logs/' + id);
          const curl = buildCurlFromLog(log);
          await copyText(curl);
          showToast('已复制 cURL 请求命令', 'success');
        } catch (err) {
          showToast('复制失败: ' + err.message, 'error');
        }
      });
    }
  });
}

function buildCurlFromLog(log) {
  const url = log.request_url || log.request_path || '';
  let cmd = `curl ${JSON.stringify(url)} \\\n  -X ${log.method || 'POST'}`;
  const headers = log.request_headers;
  if (headers) {
    try {
      const obj = JSON.parse(headers);
      for (const [k, v] of Object.entries(obj)) {
        cmd += ` \\\n  -H ${JSON.stringify(k + ': ' + v)}`;
      }
    } catch (e) {}
  }
  const body = log.request_body || '';
  if (body) {
    if (body.length > 50000) {
      cmd += ` \\\n  # 请求体过大（${body.length} 字符），已折叠`;
    } else {
      cmd += ` \\\n  -d ${JSON.stringify(body)}`;
    }
  }
  return cmd;
}

function appendLogRows(items) {
  const tbody = $('#logTableBody');
  if (!tbody) return;
  const emptyRow = tbody.querySelector('tr.empty-row');
  if (emptyRow) emptyRow.remove();

  const frag = document.createElement('tbody');
  frag.innerHTML = items.map(logRowHTML).join('');
  bindRowClicks(frag);

  while (frag.firstChild) {
    tbody.appendChild(frag.firstChild);
  }
}

export async function loadLogs(reset = false) {
  if (reset) {
    state.logsOffset = 0;
    state.logs = [];
  }

  const seq = ++logsSeq;
  const f = currentFilters();
  const tbody = $('#logTableBody');
  if (!tbody) return;

  if (reset) tbody.innerHTML = renderSkeletons(8);

  try {
    const [data, countData] = await Promise.all([
      api('/logs?' + filterQuery({ limit: f.limit, offset: String(state.logsOffset) })),
      api('/logs/count?' + filterQuery())
    ]);

    if (seq !== logsSeq) return;

    const items = data.items || [];
    if (reset) {
      state.logs = items;
    } else {
      state.logs = state.logs.concat(items);
    }
    state.logsTotal = (countData && typeof countData.count === 'number') ? countData.count : null;

    if (reset) {
      renderLogTable();
    } else if (items.length) {
      appendLogRows(items);
    }

    state.logsOffset += items.length;
    if (state.logs.length > 2000) {
      state.logs = state.logs.slice(-1000);
    }

    const loadMoreBtn = $('#logsLoadMoreBtn');
    if (loadMoreBtn) {
      loadMoreBtn.style.display =
        (state.logsTotal === null || items.length < Number(f.limit) || state.logsOffset >= state.logsTotal)
          ? 'none'
          : 'inline-flex';
    }

    renderLogsCount();
  } catch (e) {
    if (seq !== logsSeq) return;
    tbody.innerHTML = `
      <tr class="empty-row">
        <td colspan="9">
          <div class="empty-message-wrap">
            <span class="text-danger">加载失败：${escapeHtml(e.message)}</span>
            <button type="button" class="btn-ghost small" onclick="window.grokConsole.loadLogs(true)">重试</button>
          </div>
        </td>
      </tr>
    `;
  }
}

function renderLogsCount() {
  const el = $('#logsCount');
  if (!el) return;
  if (state.logsTotal === null) {
    el.textContent = '';
    return;
  }
  el.innerHTML = `共 <strong>${fmtNum(state.logsTotal)}</strong> 条记录 · 已载入 <strong>${fmtNum(state.logsOffset)}</strong> 条`;
}

function renderLogTable() {
  const tbody = $('#logTableBody');
  if (!tbody) return;

  if (!state.logs.length) {
    tbody.innerHTML = `
      <tr class="empty-row">
        <td colspan="9">
          <div class="empty-state">
            ${EMPTY_LOGS_SVG}
            <span>没有匹配的请求记录</span>
            <button type="button" class="btn-ghost small" id="emptyResetFilterBtn">重置筛选条件</button>
          </div>
        </td>
      </tr>
    `;
    const resetBtn = $('#emptyResetFilterBtn');
    if (resetBtn) {
      resetBtn.addEventListener('click', () => {
        resetFilters();
        loadLogs(true);
      });
    }
    return;
  }

  tbody.innerHTML = state.logs.map(logRowHTML).join('');
  bindRowClicks(tbody);
}

function resetFilters() {
  ['filterGateway', 'filterStatus', 'filterFrom', 'filterTo'].forEach(id => {
    const el = $('#' + id);
    if (el) el.value = '';
  });
  const modelInput = $('#filterModel');
  if (modelInput) modelInput.value = '';
  $all('.quick-filter-pill').forEach(p => p.classList.toggle('is-active', p.dataset.status === ''));
}

/* ---------------- 日志导出 (JSON & CSV) ---------------- */

async function exportLogsJSON() {
  showToast('正在导出日志数据…', 'info');
  try {
    const data = await api('/logs?' + filterQuery({ limit: '500' }));
    const items = data.items || [];
    if (!items.length) {
      showToast('当前筛选条件下无日志可导出', 'warning');
      return;
    }
    const jsonStr = JSON.stringify(items, null, 2);
    const dateTag = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    downloadFile(`grok-gateway-logs-${dateTag}.json`, jsonStr, 'application/json');
    showToast(`成功导出 ${items.length} 条 JSON 日志`, 'success');
  } catch (e) {
    showToast('导出失败: ' + e.message, 'error');
  }
}

async function exportLogsCSV() {
  showToast('正在导出 CSV…', 'info');
  try {
    const data = await api('/logs?' + filterQuery({ limit: '500' }));
    const items = data.items || [];
    if (!items.length) {
      showToast('当前筛选条件下无日志可导出', 'warning');
      return;
    }

    const headers = ['ID', 'Started At', 'Gateway', 'Model', 'Status', 'Duration (ms)', 'Stream', 'Prompt Tokens', 'Output Tokens', 'Reasoning Tokens', 'Cache Read Tokens'];
    const rows = items.map(l => [
      l.id,
      l.started_at,
      l.gateway_name || l.gateway_id,
      l.model || '',
      l.status_code,
      l.duration_ms,
      l.stream ? 'true' : 'false',
      (l.usage && (l.usage.prompt_tokens || l.usage.input_tokens)) || 0,
      (l.usage && l.usage.output_tokens) || 0,
      (l.usage && l.usage.reasoning_tokens) || 0,
      (l.usage && l.usage.cache_read_tokens) || 0
    ]);

    const csvContent = [headers.join(','), ...rows.map(r => r.map(cell => `"${String(cell).replace(/"/g, '""')}"`).join(','))].join('\n');
    const dateTag = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    downloadFile(`grok-gateway-logs-${dateTag}.csv`, csvContent, 'text/csv;charset=utf-8');
    showToast(`成功导出 ${items.length} 条 CSV 日志`, 'success');
  } catch (e) {
    showToast('导出失败: ' + e.message, 'error');
  }
}

export function initLogs() {
  const refreshBtn = $('#logsRefreshBtn');
  if (refreshBtn) refreshBtn.addEventListener('click', () => loadLogs(true));

  const loadMoreBtn = $('#logsLoadMoreBtn');
  if (loadMoreBtn) loadMoreBtn.addEventListener('click', () => loadLogs(false));

  const gwSelect = $('#filterGateway');
  if (gwSelect) gwSelect.addEventListener('change', () => loadLogs(true));

  const statusSelect = $('#filterStatus');
  if (statusSelect) statusSelect.addEventListener('change', () => loadLogs(true));

  const limitSelect = $('#filterLimit');
  if (limitSelect) limitSelect.addEventListener('change', () => loadLogs(true));

  const fromInput = $('#filterFrom');
  if (fromInput) fromInput.addEventListener('change', () => loadLogs(true));

  const toInput = $('#filterTo');
  if (toInput) toInput.addEventListener('change', () => loadLogs(true));

  const clearBtn = $('#filterClearBtn');
  if (clearBtn) {
    clearBtn.addEventListener('click', () => {
      resetFilters();
      loadLogs(true);
      showToast('已重置全部筛选条件', 'info', 1800);
    });
  }

  const modelInput = $('#filterModel');
  if (modelInput) {
    modelInput.addEventListener('input', () => {
      clearTimeout(modelDebounce);
      modelDebounce = setTimeout(() => loadLogs(true), 300);
    });
  }

  // Quick Status Filter Pills
  $all('.quick-filter-pill').forEach(pill => {
    pill.addEventListener('click', () => {
      $all('.quick-filter-pill').forEach(p => p.classList.toggle('is-active', p === pill));
      const status = pill.dataset.status;
      if (statusSelect) statusSelect.value = status;
      loadLogs(true);
    });
  });

  // Export Buttons
  const exportJsonBtn = $('#logsExportJsonBtn');
  if (exportJsonBtn) exportJsonBtn.addEventListener('click', exportLogsJSON);

  const exportCsvBtn = $('#logsExportCsvBtn');
  if (exportCsvBtn) exportCsvBtn.addEventListener('click', exportLogsCSV);

  // Clear Logs Button
  const clearLogsBtn = $('#logsClearBtn');
  if (clearLogsBtn) {
    clearLogsBtn.addEventListener('click', async () => {
      const ok = await confirmModal(
        '确定要清空全部请求日志吗？数据库中的所有历史记录都将被永久删除，此操作不可撤销。',
        '确认清空',
        true
      );
      if (!ok) return;

      try {
        const res = await api('/logs', { method: 'DELETE' });
        showToast(`已成功清空 ${res.deleted || 0} 条日志`, 'success');
        loadLogs(true);
        loadOverview();
      } catch (e) {
        showToast('清空日志失败: ' + e.message, 'error');
      }
    });
  }
}
