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
  $, $all, fmtTime, fmtMs, fmtNum,
  escapeHtml, copyText, tryPretty, buildCurlFromLog, gatewayTone
} from './utils.js';
import { showToast } from './ui.js';
import { buildDiff, renderDiffSection } from './diff.js';

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

  const idEl = $('#drawerId');
  if (idEl) idEl.textContent = id;
  const titleEl = $('#drawerTitle');
  if (titleEl) titleEl.textContent = '正在载入报文数据…';
  const metaEl = $('#drawerMeta');
  if (metaEl) metaEl.innerHTML = '<div class="drawer-loading"><span class="btn-spinner"></span> 载入请求元数据…</div>';
  const compareEl = $('#drawerCompare');
  if (compareEl) compareEl.innerHTML = '';

  updateNavButtons(id);

  $all('.log-table tbody tr').forEach(tr => tr.classList.toggle('is-active-row', tr.dataset.id === id));

  try {
    const log = await api('/logs/' + id);
    state.drawerLog = log;

    renderDrawer(log);
    const closeBtn = $('#drawerCloseBtn');
    if (closeBtn) closeBtn.focus();
  } catch (e) {
    if (titleEl) titleEl.textContent = '报文载入失败';
    if (metaEl) metaEl.innerHTML = `<div class="text-danger">无法获取详情: ${escapeHtml(e.message)}</div>`;
    showToast('载入请求详情失败: ' + e.message, 'error');
  }
}

export function closeDrawer() {
  const backdrop = $('#drawerBackdrop');
  const drawer = $('#logDrawer');
  if (backdrop) backdrop.classList.remove('is-open');
  if (drawer) {
    drawer.classList.remove('is-open');
    drawer.setAttribute('aria-hidden', 'true');
  }
  state.drawerLogId = null;
  state.drawerLog = null;
  $all('.log-table tbody tr').forEach(tr => tr.classList.remove('is-active-row'));
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
  const titleEl = $('#drawerTitle');
  if (titleEl) {
    titleEl.innerHTML = `
      <span class="drawer-title-model">${escapeHtml(log.model || '(未知模型)')}</span>
      <span class="gw-badge">
        <span class="gw-dot gw-tone-${gatewayTone(gwId)}"></span>
        ${escapeHtml(log.gateway_name || gwId)}
      </span>
    `;
  }

  // Meta
  const metaEl = $('#drawerMeta');
  if (metaEl) {
    const promptTok = usage.prompt_tokens || usage.input_tokens || 0;
    const outTok = usage.output_tokens || 0;
    const cacheTok = usage.cache_read_tokens || 0;

    metaEl.innerHTML = `
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">状态</span>
        <span class="drawer-meta-v ${log.success ? 'text-success' : 'text-danger'}">
          ${log.status_code || (log.success ? '200 OK' : 'FAIL')}
        </span>
      </div>
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">耗时</span>
        <span class="drawer-meta-v">${fmtMs(duration)}</span>
      </div>
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">传输模式</span>
        <span class="drawer-meta-v">${log.stream ? 'SSE 流式' : '全量响应'}</span>
      </div>
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">Token (输入/输出)</span>
        <span class="drawer-meta-v">${fmtNum(promptTok)} / ${fmtNum(outTok)}</span>
      </div>
      ${cacheTok > 0 ? `
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">缓存命中</span>
        <span class="drawer-meta-v text-success">${fmtNum(cacheTok)} Token</span>
      </div>
      ` : ''}
      <div class="drawer-meta-item">
        <span class="drawer-meta-k">发生时间</span>
        <span class="drawer-meta-v" title="${escapeHtml(log.started_at || '')}">${fmtTime(log.started_at)}</span>
      </div>
    `;
  }

  renderDrawerBody(log);
}

// payloads maps stored request and response fields for diff and inspection views.
function payloads(log) {
  return {
    request: {
      client: log.request_body || '',
      upstream: log.upstream_body || '',
      clientHeaders: log.request_headers || {},
      upstreamHeaders: log.upstream_headers || {},
    },
    response: {
      // Upstream is the "before" of the response direction: what arrived is
      // filtered, then what leaves is handed to the client.
      upstream: log.upstream_response_body || '',
      client: log.response_body || '',
      upstreamHeaders: log.upstream_response_headers || log.response_headers || {},
      clientHeaders: log.response_headers || {},
    },
  };
}

// 每个带复制按钮的报文块的原文，key 与 data-copy-block 对应。
const blockContents = new Map();

function payloadBlock(label, content, key) {
  blockContents.set(key, content == null ? '' : content);
  const text = tryPretty(content);
  const empty = typeof content === 'object' && content !== null && !Object.keys(content).length;
  return `
    <div class="payload-block">
      <div class="payload-block-head">
        <span class="payload-block-label">${escapeHtml(label)}</span>
        <button type="button" class="btn-ghost small" data-copy-block="${escapeHtml(key)}" ${empty ? 'disabled' : ''}>复制</button>
      </div>
      <pre class="code-block payload-code">${escapeHtml(text) || '<span class="text-muted">(空报文)</span>'}</pre>
    </div>
  `;
}

function renderDrawerBody(log) {
  const tab = state.drawerTab || 'request-diff';
  const compareWrap = $('#drawerCompare');
  if (!compareWrap) return;
  const hops = payloads(log);

  if (tab === 'request-diff') {
    const bodyDiffs = buildDiff(hops.request.client, hops.request.upstream, 'body');
    const headerDiffs = buildDiff(hops.request.clientHeaders, hops.request.upstreamHeaders, 'headers');
    compareWrap.innerHTML = `
      ${renderDiffSection('请求正文 (Payload 对齐)', bodyDiffs)}
      ${renderDiffSection('请求标头 (HTTP Headers 清洗)', headerDiffs)}
    `;
    return;
  }
  if (tab === 'response-diff') {
    const bodyDiffs = buildDiff(hops.response.upstream, hops.response.client, 'body');
    compareWrap.innerHTML = `
      ${renderDiffSection('响应正文 (SSE 事件清洗与标准化)', bodyDiffs)}
    `;
    return;
  }
  if (tab === 'request-raw') {
    compareWrap.innerHTML = `
      ${payloadBlock('客户端发送', hops.request.client, 'req-client')}
      ${payloadBlock('代理转发上游', hops.request.upstream, 'req-upstream')}
    `;
    return;
  }
  if (tab === 'response-raw') {
    compareWrap.innerHTML = `
      ${payloadBlock('上游返回', hops.response.upstream, 'resp-upstream')}
      ${payloadBlock('代理回传客户端', hops.response.client, 'resp-client')}
    `;
    return;
  }
  compareWrap.innerHTML = `
    ${payloadBlock('客户端请求标头', hops.request.clientHeaders, 'h-req-client')}
    ${payloadBlock('上游请求标头', hops.request.upstreamHeaders, 'h-req-upstream')}
    ${payloadBlock('上游响应标头', hops.response.upstreamHeaders, 'h-resp-upstream')}
    ${payloadBlock('客户端响应标头', hops.response.clientHeaders, 'h-resp-client')}
  `;
}

export function initDrawer() {
  const closeBtn = $('#drawerCloseBtn');
  if (closeBtn) closeBtn.addEventListener('click', closeDrawer);

  const backdrop = $('#drawerBackdrop');
  if (backdrop) backdrop.addEventListener('click', closeDrawer);

  const prevBtn = $('#drawerPrevBtn');
  if (prevBtn) prevBtn.addEventListener('click', () => navigateDrawer(-1));

  const nextBtn = $('#drawerNextBtn');
  if (nextBtn) nextBtn.addEventListener('click', () => navigateDrawer(1));

  const copyIdBtn = $('#drawerCopyIdBtn');
  if (copyIdBtn) {
    copyIdBtn.addEventListener('click', async () => {
      if (state.drawerLogId) {
        await copyText(state.drawerLogId);
        const orig = copyIdBtn.textContent;
        copyIdBtn.textContent = '已复制 ✓';
        setTimeout(() => copyIdBtn.textContent = orig, 1500);
        showToast('已复制请求 ID', 'success', 1200);
      }
    });
  }

  const copyCurlBtn = $('#drawerCopyCurlBtn');
  if (copyCurlBtn) {
    copyCurlBtn.addEventListener('click', async () => {
      if (state.drawerLog) {
        const curl = buildCurlFromLog(state.drawerLog);
        await copyText(curl);
        const orig = copyCurlBtn.textContent;
        copyCurlBtn.textContent = '已复制 ✓';
        setTimeout(() => copyCurlBtn.textContent = orig, 1500);
        showToast('已生成并复制 cURL 调试命令', 'success', 1200);
      }
    });
  }

  // 报文块复制按钮（请求/响应原文与标头页签）
  const compareWrap = $('#drawerCompare');
  if (compareWrap) {
    compareWrap.addEventListener('click', async (e) => {
      const btn = e.target.closest('[data-copy-block]');
      if (!btn || btn.disabled) return;
      const content = blockContents.get(btn.dataset.copyBlock);
      if (content === undefined) return;
      const text = typeof content === 'string' ? content : JSON.stringify(content, null, 2);
      if (!text) return;
      await copyText(text);
      const orig = btn.textContent;
      btn.textContent = '已复制 ✓';
      btn.classList.add('text-success');
      setTimeout(() => {
        btn.textContent = orig;
        btn.classList.remove('text-success');
      }, 1500);
      showToast('已复制内容', 'success', 1200);
    });
  }

  // Segment buttons
  $all('.drawer-nav .seg').forEach(seg => {
    seg.addEventListener('click', () => {
      $all('.drawer-nav .seg').forEach(s => {
        const act = s === seg;
        s.classList.toggle('is-active', act);
        s.setAttribute('aria-selected', act ? 'true' : 'false');
      });

      state.drawerTab = seg.dataset.tab;

      if (state.drawerLog) {
        renderDrawerBody(state.drawerLog);
      }
    });
  });
}
