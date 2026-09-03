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
  $, $all, fmtNum, fmtPct, fmtTimeShort, fmtMs,
  escapeHtml, downloadFile, csvCell, gatewayTone,
  latencyThresholds, latencyClass, latencyTitle
} from './utils.js';
import { openDrawer } from './drawer.js';
import { showToast, confirmModal } from './ui.js';
import { loadOverview } from './overview.js';

const SERVER_MAX_LIMIT = 500;
let modelDebounceTimer;

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
  if (f.limit) params.set('limit', f.limit);
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

function logRowHTML(l, thresholds) {
  const gwId = l.gateway_id || 'unknown';
  const usage = l.usage || {};
  const promptTokens = usage.prompt_tokens || usage.input_tokens || 0;
  const outputTokens = usage.output_tokens || 0;
  const reasoningTokens = usage.reasoning_tokens || 0;
  const cacheReadTokens = usage.cache_read_tokens || 0;

  let cacheHit = '—';
  let cacheHitCls = 'cache-neutral';
  if (promptTokens > 0 && cacheReadTokens > 0) {
    const rate = (cacheReadTokens / promptTokens) * 100;
    cacheHit = fmtPct(rate);
    cacheHitCls = rate >= 50 ? 'cache-high' : 'cache-med';
  } else if (promptTokens > 0) {
    cacheHit = '0%';
  }

  const latCls = latencyClass(l.duration_ms || 0, thresholds);
  const latTitle = latencyTitle(l.duration_ms || 0, thresholds);

  return `
    <tr class="log-tr ${l.success ? '' : 'is-err'}" data-id="${escapeHtml(l.id)}">
      <td class="cell-time" title="${escapeHtml(l.started_at || '')}">${fmtTimeShort(l.started_at)}</td>
      <td class="cell-gw">
        <span class="gw-badge">
          <span class="gw-dot gw-tone-${gatewayTone(gwId)}"></span>
          ${escapeHtml(l.gateway_name || gwId)}
        </span>
      </td>
      <td class="cell-model" title="${escapeHtml(l.model || '')}">${escapeHtml(l.model || '—')}</td>
      <td class="cell-status">
        <span class="status-pill ${l.success ? 'is-success' : 'is-failure'}">
          ${l.status_code || (l.success ? '200' : 'ERR')}
        </span>
      </td>
      <td class="cell-latency ${latCls}" title="${escapeHtml(latTitle)}">${fmtMs(l.duration_ms)}</td>
      <td class="cell-stream">${l.stream ? '<span class="pill-stream">SSE</span>' : '<span class="pill-json">JSON</span>'}</td>
      <td class="cell-tokens">
        <span title="输入 / 输出 / 推理">${fmtNum(promptTokens)} / ${fmtNum(outputTokens)}${reasoningTokens > 0 ? ` <small class="text-muted">(${fmtNum(reasoningTokens)})</small>` : ''}</span>
      </td>
      <td class="cell-cache ${cacheHitCls}">${cacheHit}</td>
      <td class="cell-actions">
        <button type="button" class="btn-link log-inspect-btn" data-id="${escapeHtml(l.id)}">详情</button>
      </td>
    </tr>
  `;
}

export async function loadLogs(reset = false) {
  // Auto-refresh calls loadLogs({ silent: true }): the same full reset, but
  // without the skeleton flash, without re-rendering unchanged data, and
  // without failure toasts — the table keeps its last good state.
  const silent = typeof reset === 'object' && reset !== null && reset.silent === true;
  const isReset = Boolean(reset);
  const tbody = $('#logTableBody');
  const countEl = $('#logsCount');
  const loadMoreBtn = $('#logsLoadMoreBtn');
  if (!tbody) return;

  if (isReset) {
    state.logsOffset = 0;
    if (!silent) tbody.innerHTML = renderSkeletons(10);
  }

  const query = filterQuery({ offset: String(state.logsOffset) });

  try {
    const data = await api('/logs?' + query);
    const items = data.items || [];
    state.logsTotal = data.total || items.length;

    const sig = state.logsTotal + '|' +
      items.map(l => `${l.id}:${l.success ? 1 : 0}:${l.duration_ms || 0}:${l.status_code || ''}`).join(',');
    if (silent && sig === state.logsSig) return;
    state.logsSig = sig;

    if (isReset) {
      state.logs = items;
    } else {
      state.logs = [...(state.logs || []), ...items];
    }

    if (!state.logs.length) {
      tbody.innerHTML = `
        <tr>
          <td colspan="9" class="table-empty">
            <div class="empty-state" style="padding: 42px 16px;">
              <svg viewBox="0 0 48 48" width="42" height="42" fill="none" style="opacity: 0.45; margin-bottom: 4px;">
                <circle cx="22" cy="22" r="14" stroke="currentColor" stroke-width="2"/>
                <path d="m32 32 10 10" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>
                <path d="M16 22h12M22 16v12" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
              </svg>
              <span style="font-weight: 600; font-size: 14px;">未匹配到符合条件的请求记录</span>
              <span class="muted" style="font-size: 12px;">可尝试调整模型名称搜索词、清除起止时间限制或重置网关状态</span>
              <button type="button" class="btn-ghost small" id="resetFiltersEmptyBtn" style="margin-top: 8px;">重置全部筛选条件</button>
            </div>
          </td>
        </tr>
      `;
      const resetEmpty = $('#resetFiltersEmptyBtn', tbody);
      if (resetEmpty) {
        resetEmpty.addEventListener('click', () => {
          const clearBtn = $('#filterClearBtn');
          if (clearBtn) clearBtn.click();
        });
      }
      if (countEl) countEl.textContent = '共 0 条';
      if (loadMoreBtn) loadMoreBtn.style.display = 'none';
      return;
    }

    const thresholds = latencyThresholds(state.logs);
    tbody.innerHTML = state.logs.map(l => logRowHTML(l, thresholds)).join('');

    $all('.log-tr, .log-inspect-btn', tbody).forEach(el => {
      el.addEventListener('click', (e) => {
        const id = el.dataset.id || el.closest('.log-tr')?.dataset.id;
        if (id) openDrawer(id);
      });
    });

    if (countEl) countEl.textContent = `已显示 ${state.logs.length} / 共 ${state.logsTotal} 条`;
    if (loadMoreBtn) {
      loadMoreBtn.style.display = state.logs.length < state.logsTotal ? 'inline-block' : 'none';
    }
  } catch (e) {
    if (isReset && !silent) {
      tbody.innerHTML = `<tr><td colspan="9" class="table-empty text-danger">日志载入失败: ${escapeHtml(e.message)}</td></tr>`;
    }
    if (!silent) showToast('日志载入失败: ' + e.message, 'error');
  }
}

async function exportLogs(format) {
  try {
    const query = filterQuery({ limit: String(SERVER_MAX_LIMIT) });
    const data = await api('/logs?' + query);
    const items = data.items || [];

    if (!items.length) {
      showToast('当前筛选条件下无日志可导出', 'warning');
      return;
    }

    const timestamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-');
    if (format === 'json') {
      downloadFile(`grok-logs-${timestamp}.json`, JSON.stringify(items, null, 2), 'application/json');
    } else {
      const headers = ['ID', 'Time', 'Gateway', 'Model', 'Status', 'DurationMS', 'Stream', 'InputTokens', 'OutputTokens', 'ReasoningTokens', 'CacheReadTokens', 'Error'];
      const rows = items.map(l => {
        const u = l.usage || {};
        return [
          csvCell(l.id),
          csvCell(l.started_at),
          csvCell(l.gateway_name || l.gateway_id),
          csvCell(l.model),
          csvCell(l.status_code || (l.success ? '200' : '500')),
          csvCell(l.duration_ms),
          csvCell(l.stream ? 'true' : 'false'),
          csvCell(u.prompt_tokens || u.input_tokens || 0),
          csvCell(u.output_tokens || 0),
          csvCell(u.reasoning_tokens || 0),
          csvCell(u.cache_read_tokens || 0),
          csvCell(l.error || '')
        ].join(',');
      });
      const csv = '\uFEFF' + headers.join(',') + '\n' + rows.join('\n');
      downloadFile(`grok-logs-${timestamp}.csv`, csv, 'text/csv;charset=utf-8');
    }
    showToast(
      items.length < (data.total || items.length)
        ? `已导出 ${items.length} / 共 ${data.total} 条（单次导出上限 ${SERVER_MAX_LIMIT} 条）`
        : `已成功导出 ${items.length} 条日志`,
      'success'
    );
  } catch (e) {
    showToast('导出日志失败: ' + e.message, 'error');
  }
}

function clearAllLogs() {
  confirmModal('清空全部日志', '确定要清空数据库中的所有历史请求日志吗？此操作无法撤销。', async () => {
    try {
      await api('/logs', { method: 'DELETE' });
      await loadLogs(true);
      await loadOverview();
      showToast('历史日志已全部清空', 'success');
    } catch (e) {
      showToast('清空日志失败: ' + e.message, 'error');
    }
  });
}

export function initLogs() {
  const refreshBtn = $('#logsRefreshBtn');
  if (refreshBtn) refreshBtn.addEventListener('click', () => loadLogs(true));

  const clearBtn = $('#logsClearBtn');
  if (clearBtn) clearBtn.addEventListener('click', clearAllLogs);

  const exportJsonBtn = $('#logsExportJsonBtn');
  if (exportJsonBtn) exportJsonBtn.addEventListener('click', () => exportLogs('json'));

  const exportCsvBtn = $('#logsExportCsvBtn');
  if (exportCsvBtn) exportCsvBtn.addEventListener('click', () => exportLogs('csv'));

  const loadMoreBtn = $('#logsLoadMoreBtn');
  if (loadMoreBtn) {
    loadMoreBtn.addEventListener('click', () => {
      state.logsOffset = (state.logs || []).length;
      loadLogs(false);
    });
  }

  // Filter triggers
  ['#filterGateway', '#filterStatus', '#filterLimit', '#filterFrom', '#filterTo'].forEach(sel => {
    const el = $(sel);
    if (el) el.addEventListener('change', () => loadLogs(true));
  });

  const modelInput = $('#filterModel');
  if (modelInput) {
    modelInput.addEventListener('input', () => {
      clearTimeout(modelDebounceTimer);
      modelDebounceTimer = setTimeout(() => loadLogs(true), 300);
    });
  }

  const clearFilterBtn = $('#filterClearBtn');
  if (clearFilterBtn) {
    clearFilterBtn.addEventListener('click', () => {
      if ($('#filterFrom')) $('#filterFrom').value = '';
      if ($('#filterTo')) $('#filterTo').value = '';
      if ($('#filterGateway')) $('#filterGateway').value = '';
      if ($('#filterModel')) $('#filterModel').value = '';
      if ($('#filterStatus')) $('#filterStatus').value = '';
      loadLogs(true);
    });
  }
}
