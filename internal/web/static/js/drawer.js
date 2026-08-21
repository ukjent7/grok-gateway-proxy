'use strict';

/* ============================================================
   Grok Gateway Console · 请求详情抽屉与报文查看器
   - 响应式侧滑抽屉与毛玻璃背景
   - 上一条 / 下一条快捷导航 (J/K 或 [/])
   - 智能 Diff 比对 (请求/响应)
   - 格式化 JSON 高亮与一键提取 cURL
   ============================================================ */

import { state } from './state.js';
import { api } from './api.js';
import {
  $, $all, fmtTime, fmtTimeRelative, fmtMs, fmtNum,
  escapeHtml, copyText, tryPretty
} from './utils.js';
import { showToast } from './ui.js';
import {
  buildDiff, buildValueDiff, diffStats, diffSummaryBadge,
  renderDiffSection
} from './diff.js';

const MAX_DIFF_BYTES = 1024 * 1024;
const MAX_PREVIEW_BYTES = 64 * 1024;

export async function openDrawer(id) {
  if (!id) return;
  state.drawerLogId = id;

  const backdrop = $('#drawerBackdrop');
  const drawer = $('#logDrawer');
  if (backdrop) backdrop.classList.add('is-open');
  if (drawer) {
    drawer.classList.add('is-open');
    drawer.setAttribute('aria-hidden', 'false');
  }

  $('#drawerId').textContent = id;
  $('#drawerTitle').textContent = '正在载入报文数据…';
  $('#drawerMeta').innerHTML = '<div class="drawer-loading"><span class="btn-spinner"></span> 载入请求元数据…</div>';
  $('#drawerCode').textContent = '加载中…';
  $('#drawerCompare').innerHTML = '';

  updateNavButtons(id);

  try {
    const log = await api('/logs/' + id);
    state.drawerLog = log;

    // Reset or keep active tab
    renderDrawer(log);
    const closeBtn = $('#drawerCloseBtn');
    if (closeBtn) closeBtn.focus();
  } catch (e) {
    $('#drawerTitle').textContent = '报文载入失败';
    $('#drawerCode').textContent = e.message;
    $('#drawerMeta').innerHTML = `<div class="text-danger">无法获取详情: ${escapeHtml(e.message)}</div>`;
    showToast('载入请求详情失败: ' + e.message, 'error');
  }
}

function updateNavButtons(currentId) {
  const prevBtn = $('#drawerPrevBtn');
  const nextBtn = $('#drawerNextBtn');
  if (!prevBtn || !nextBtn) return;

  const list = state.logs || [];
  const idx = list.findIndex(l => l.id === currentId);

  prevBtn.disabled = idx <= 0;
  nextBtn.disabled = idx < 0 || idx >= list.length - 1;
}

export function navigateDrawer(direction) {
  const currentId = state.drawerLogId;
  const list = state.logs || [];
  const idx = list.findIndex(l => l.id === currentId);
  if (idx === -1) return;

  const targetIdx = idx + direction;
  if (targetIdx >= 0 && targetIdx < list.length) {
    openDrawer(list[targetIdx].id);
  }
}

function renderDrawer(log) {
  const gwId = log.gateway_id || 'unknown';
  const duration = log.duration_ms || 0;
  const usage = log.usage || {};

  // Title
  $('#drawerTitle').innerHTML = `
    <span class="drawer-title-model">${escapeHtml(log.model || '(未知模型)')}</span>
    <span class="gw-badge gw-badge-${escapeHtml(gwId)}">
      <span class="gw-dot gw-dot-${escapeHtml(gwId)}"></span>
      ${escapeHtml(log.gateway_name || gwId)}
    </span>
  `;

  // Meta chips
  const chips = [
    { label: '发起时间', val: fmtTime(log.started_at) + ` (${fmtTimeRelative(log.started_at)})` },
    { label: 'HTTP 状态', val: `${log.status_code || (log.success ? 200 : 'ERR')} · ${log.success ? '成功' : '失败'}`, cls: log.success ? 'is-ok' : 'is-err' },
    { label: '总响应耗时', val: fmtMs(duration) },
    { label: '传输模式', val: log.stream ? 'SSE 流式传输' : '标准单次响应' },
    { label: '客户端路径', val: `${log.method || 'POST'} ${log.request_path || '/'}` },
    { label: '上游地址', val: log.upstream_url || '—' }
  ];

  if (usage.usage_present) {
    const prompt = usage.prompt_tokens || usage.input_tokens || 0;
    const output = usage.output_tokens || 0;
    const reasoning = usage.reasoning_tokens || 0;
    const cached = usage.cache_read_tokens || 0;
    chips.push({
      label: 'Token 统计',
      val: `Prompt ${fmtNum(prompt)} (Cached ${fmtNum(cached)}) / Output ${fmtNum(output)} / Reasoning ${fmtNum(reasoning)}`
    });
  }

  let metaHTML = chips.map(c => `
    <div class="drawer-meta-chip ${c.cls || ''}">
      <span class="chip-label">${escapeHtml(c.label)}:</span>
      <span class="chip-val">${escapeHtml(c.val)}</span>
    </div>
  `).join('');

  if (log.error) {
    metaHTML += `
      <div class="drawer-meta-chip is-err full-width">
        <span class="chip-label">异常详情:</span>
        <span class="chip-val">${escapeHtml(log.error)}</span>
      </div>
    `;
  }

  if (log.response_truncated) {
    metaHTML += `
      <div class="drawer-meta-chip is-warn full-width">
        <span class="chip-label">注意:</span>
        <span class="chip-val">响应正文超过 64MB 上限，报文内容已部分截断显示</span>
      </div>
    `;
  }

  $('#drawerMeta').innerHTML = metaHTML;
  renderDrawerTab(state.drawerTab);
}

function previewOrFull(raw) {
  const text = raw || '';
  if (text.length > MAX_PREVIEW_BYTES) {
    return text.slice(0, MAX_PREVIEW_BYTES) + `\n\n… [正文体积过大（${text.length} 字符），已折叠展示前 64KB]`;
  }
  return text;
}

function renderDrawerTab(tab) {
  const log = state.drawerLog;
  if (!log) return;

  const compare = $('#drawerCompare');
  const codeWrap = $('#drawerCodeWrap');
  const code = $('#drawerCode');
  if (!compare || !code) return;

  if (tab === 'request-compare' || tab === 'response-compare') {
    compare.style.display = 'block';
    if (codeWrap) codeWrap.style.display = 'none';
    renderComparison(tab, log);
    return;
  }

  compare.style.display = 'none';
  if (codeWrap) codeWrap.style.display = 'block';

  let content = '';
  if (tab === 'request') {
    content = previewOrFull(tryPretty(log.request_body));
  } else if (tab === 'upstream') {
    content = previewOrFull(tryPretty(log.upstream_body));
  } else if (tab === 'upstream-response') {
    content = previewOrFull(tryPretty(log.upstream_response_body || log.response_body));
  } else if (tab === 'response') {
    content = previewOrFull(tryPretty(log.response_body));
  } else if (tab === 'headers') {
    content = `=== [1] 客户端原始请求头 (Client Request Headers) ===\n${tryPretty(log.request_headers) || '(空)'}\n\n` +
      `=== [2] 代理发往上游标头 (Upstream Request Headers) ===\n${tryPretty(log.upstream_headers) || '(空)'}\n\n` +
      `=== [3] 上游返回原始响应头 (Upstream Response Headers) ===\n${tryPretty(log.upstream_response_headers) || '(空)'}\n\n` +
      `=== [4] 代理返回客户端标头 (Client Response Headers) ===\n${tryPretty(log.response_headers) || '(空)'}`;
  }

  code.textContent = content || '(空报文)';
}

function renderComparison(tab, log) {
  const request = tab === 'request-compare';
  const left = request ? {
    label: '客户端原始请求',
    url: (log.method || 'POST') + ' ' + (log.request_url || log.request_path || '—'),
    headers: log.request_headers,
    body: log.request_body
  } : {
    label: '上游服务原始响应',
    url: 'HTTP ' + (log.upstream_response_status_code || log.status_code || '—'),
    headers: log.upstream_response_headers,
    body: log.upstream_response_body || log.response_body
  };

  const right = request ? {
    label: '代理上游转发请求',
    url: (log.method || 'POST') + ' ' + (log.upstream_url || '—'),
    headers: log.upstream_headers,
    body: log.upstream_body
  } : {
    label: '代理返回客户端最终响应',
    url: 'HTTP ' + (log.client_response_status_code || log.status_code || '—'),
    headers: log.response_headers,
    body: log.response_body
  };

  const oversized = String(left.body || '').length + String(right.body || '').length > MAX_DIFF_BYTES;
  let headerDiff, bodyDiff;

  if (oversized) {
    headerDiff = buildDiff(left.headers, right.headers, 'headers');
    bodyDiff = {
      rows: [{
        kind: 'modified',
        path: 'Body',
        before: previewOrFull(left.body),
        after: previewOrFull(right.body),
        explanation: '报文过大，跳过精细比对'
      }]
    };
  } else {
    headerDiff = buildDiff(left.headers, right.headers, 'headers');
    bodyDiff = buildDiff(left.body, right.body, 'body');
  }

  const statusDiff = request
    ? null
    : buildValueDiff('HTTP Status', log.upstream_response_status_code || log.status_code, log.client_response_status_code || log.status_code);

  const total = diffStats([headerDiff, bodyDiff, statusDiff]);

  $('#drawerCompare').innerHTML = `
    <div class="diff-overview-card">
      <div class="diff-route-bar">
        <div class="diff-endpoint">
          <span class="endpoint-tag">${escapeHtml(left.label)}</span>
          <code>${escapeHtml(left.url)}</code>
        </div>
        <div class="diff-arrow">
          <svg viewBox="0 0 16 16" width="16" height="16" fill="none"><path d="M3 8h10M9 4l4 4-4 4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        </div>
        <div class="diff-endpoint">
          <span class="endpoint-tag">${escapeHtml(right.label)}</span>
          <code>${escapeHtml(right.url)}</code>
        </div>
      </div>
      <div class="diff-summary-badges">
        ${diffSummaryBadge('新增', total.added, 'added')}
        ${diffSummaryBadge('修改', total.modified, 'modified')}
        ${diffSummaryBadge('删除', total.deleted, 'deleted')}
        ${total.total === 0 ? '<span class="diff-summary-badge diff-same">✓ 双端报文完全一致（无重写变更）</span>' : ''}
      </div>
    </div>
    ${statusDiff ? renderDiffSection(statusDiff, '状态码 (Status)', '上游返回 ➔ 客户端接收') : ''}
    ${renderDiffSection(headerDiff, '标头差异 (Headers)', '请求/响应头变更对比')}
    ${renderDiffSection(bodyDiff, '正文差异 (Body Payload)', 'JSON 载荷改写对比')}
  `;
}

export function closeDrawer() {
  const backdrop = $('#drawerBackdrop');
  const drawer = $('#logDrawer');
  if (backdrop) backdrop.classList.remove('is-open');
  if (drawer) {
    drawer.classList.remove('is-open');
    drawer.setAttribute('aria-hidden', 'true');
  }
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
    if (body.length > MAX_PREVIEW_BYTES) {
      cmd += ` \\\n  # 请求体过大（${body.length} 字符），已折叠`;
    } else {
      cmd += ` \\\n  -d ${JSON.stringify(body)}`;
    }
  }
  return cmd;
}

export function initDrawer() {
  // Tab switching
  const tabsContainer = $('#drawerTabs');
  if (tabsContainer) {
    tabsContainer.addEventListener('click', (e) => {
      const btn = e.target.closest('.drawer-tab');
      if (!btn) return;
      state.drawerTab = btn.dataset.tab;
      $all('.drawer-tab', tabsContainer).forEach(t => {
        const active = t === btn;
        t.classList.toggle('is-active', active);
        t.setAttribute('aria-selected', active ? 'true' : 'false');
      });
      renderDrawerTab(state.drawerTab);
    });
  }

  // Copy ID
  const copyIdBtn = $('#drawerCopyIdBtn');
  if (copyIdBtn) {
    copyIdBtn.addEventListener('click', async () => {
      const id = $('#drawerId').textContent.trim();
      if (!id || id === '—') return;
      await copyText(id);
      showToast('已复制请求 ID: ' + id, 'success');
    });
  }

  // Copy cURL
  const copyCurlBtn = $('#drawerCopyCurlBtn');
  if (copyCurlBtn) {
    copyCurlBtn.addEventListener('click', async () => {
      const log = state.drawerLog;
      if (!log) return;
      await copyText(buildCurlFromLog(log));
      showToast('已复制 cURL 调试命令', 'success');
    });
  }

  // Copy Raw JSON Body
  const copyBodyBtn = $('#drawerCopyBodyBtn');
  if (copyBodyBtn) {
    copyBodyBtn.addEventListener('click', async () => {
      const log = state.drawerLog;
      if (!log) return;
      const codeText = $('#drawerCode').textContent;
      if (codeText) {
        await copyText(codeText);
        showToast('已复制当前报文内容', 'success');
      }
    });
  }

  // Prev / Next Navigation
  const prevBtn = $('#drawerPrevBtn');
  if (prevBtn) prevBtn.addEventListener('click', () => navigateDrawer(-1));

  const nextBtn = $('#drawerNextBtn');
  if (nextBtn) nextBtn.addEventListener('click', () => navigateDrawer(1));

  // Close
  const closeBtn = $('#drawerCloseBtn');
  if (closeBtn) closeBtn.addEventListener('click', closeDrawer);

  const backdrop = $('#drawerBackdrop');
  if (backdrop) backdrop.addEventListener('click', closeDrawer);
}
