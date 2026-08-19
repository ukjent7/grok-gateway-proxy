'use strict';

import { state } from './state.js';
import { api } from './api.js';
import { $, $all, fmtTime, fmtMs, escapeHtml, tryPretty } from './utils.js';
import { showToast } from './ui.js';
import {
  buildDiff, buildValueDiff, diffStats, diffSummaryBadge,
  renderDiffSection
} from './diff.js';

/* ============================================================
   请求详情抽屉（本地小工具：仅展示脱敏后的 headers）
   ============================================================ */

const MAX_DIFF_BYTES = 1024 * 1024;   // 超过则跳过 diff，直接显示截断预览
const MAX_PREVIEW_BYTES = 50 * 1024;  // 大 body 的预览上限

export async function openDrawer(id) {
  const prevFocus = document.activeElement;
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
    $all('.drawer-tab').forEach(t => {
      const active = t.dataset.tab === 'request-compare';
      t.classList.toggle('is-active', active);
      t.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    renderDrawer(log);
    const closeBtn = $('#drawerCloseBtn');
    if (closeBtn) closeBtn.focus();
  } catch (e) {
    $('#drawerTitle').textContent = '加载失败';
    $('#drawerCode').textContent = e.message;
    showToast('加载请求详情失败: ' + e.message, 'error');
    if (prevFocus && typeof prevFocus.focus === 'function') prevFocus.focus();
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
  if (log.response_truncated) {
    $('#drawerMeta').innerHTML += '<span class="meta-chip error" title="响应超过记录上限，客户端仍收到完整内容，日志仅保留前 64MB">响应体已截断</span>';
  }
  renderDrawerTab(state.drawerTab);
}

// 超大 body 不整段展示，截断后提示
function previewOrFull(raw) {
  const text = raw || '';
  if (text.length > MAX_PREVIEW_BYTES) {
    return text.slice(0, MAX_PREVIEW_BYTES) + '\n…（内容过大，已截断显示，完整长度 ' + text.length + ' 字符）';
  }
  return text;
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
  if (tab === 'request') content = previewOrFull(tryPretty(log.request_body));
  else if (tab === 'upstream') content = previewOrFull(tryPretty(log.upstream_body));
  else if (tab === 'upstream-response') content = previewOrFull(tryPretty(log.upstream_response_body || log.response_body));
  else if (tab === 'response') content = previewOrFull(tryPretty(log.response_body));
  else if (tab === 'headers') content =
    tryPretty(log.request_headers) +
    '\n\n--- upstream request headers ---\n\n' + tryPretty(log.upstream_headers) +
    '\n\n--- upstream response headers ---\n\n' + tryPretty(log.upstream_response_headers) +
    '\n\n--- client response headers ---\n\n' + tryPretty(log.response_headers);
  code.textContent = content || '(空)';
}

function renderComparison(tab, log) {
  const request = tab === 'request-compare';
  const left = request ? {
    label: '客户端原请求',
    url: (log.method || 'POST') + ' ' + (log.request_url || log.request_path || '—'),
    headers: log.request_headers,
    body: log.request_body
  } : {
    label: '上游 API 原始响应',
    url: 'HTTP ' + (log.upstream_response_status_code || log.status_code || '—'),
    headers: log.upstream_response_headers,
    body: log.upstream_response_body || log.response_body
  };
  const right = request ? {
    label: '代理实际发送',
    url: (log.method || 'POST') + ' ' + (log.upstream_url || '—'),
    headers: log.upstream_headers,
    body: log.upstream_body
  } : {
    label: '代理实际返回客户端',
    url: 'HTTP ' + (log.client_response_status_code || log.status_code || '—'),
    headers: log.response_headers,
    body: log.response_body
  };
  const oversized = String(left.body || '').length + String(right.body || '').length > MAX_DIFF_BYTES;
  let headerDiff, bodyDiff;
  if (oversized) {
    headerDiff = buildDiff(left.headers, right.headers, 'headers');
    bodyDiff = { rows: [{ kind: 'modified', path: 'Body', before: previewOrFull(left.body), after: previewOrFull(right.body), explanation: '内容过大，已跳过 diff 并折叠展示' }] };
  } else {
    headerDiff = buildDiff(left.headers, right.headers, 'headers');
    bodyDiff = buildDiff(left.body, right.body, 'body');
  }
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

export function closeDrawer() {
  $('#drawerBackdrop').classList.remove('is-open');
  const drawer = $('#logDrawer');
  drawer.classList.remove('is-open');
  drawer.setAttribute('aria-hidden', 'true');
}

function buildCurlFromLog(log) {
  const url = log.request_url || log.request_path || '';
  let cmd = 'curl ' + JSON.stringify(url) + ' \\\n  -X ' + (log.method || 'POST');
  const headers = log.request_headers;
  if (headers) {
    try {
      const obj = JSON.parse(headers);
      for (const [k, v] of Object.entries(obj)) {
        cmd += ' \\\n  -H ' + JSON.stringify(k + ': ' + v);
      }
    } catch (e) { /* 非 JSON 头，跳过 */ }
  }
  const body = log.request_body || '';
  if (body) {
    if (body.length > MAX_PREVIEW_BYTES) {
      cmd += ' \\\n  # 请求体过大（' + body.length + ' 字符），已省略';
    } else {
      cmd += ' \\\n  -d ' + JSON.stringify(body);
    }
  }
  return cmd;
}

export function initDrawer() {
  $('#drawerTabs').addEventListener('click', (e) => {
    const btn = e.target.closest('.drawer-tab');
    if (!btn) return;
    state.drawerTab = btn.dataset.tab;
    $all('.drawer-tab').forEach(t => {
      const active = t === btn;
      t.classList.toggle('is-active', active);
      t.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    renderDrawerTab(state.drawerTab);
  });
  $('#drawerCopyIdBtn').addEventListener('click', () => {
    const id = $('#drawerId').textContent.trim();
    if (!id || id === '—') return;
    navigator.clipboard.writeText(id).then(() => showToast('已复制请求 ID: ' + id, 'success'));
  });
  $('#drawerCopyCurlBtn').addEventListener('click', () => {
    const log = state.drawerLog;
    if (!log) return;
    navigator.clipboard.writeText(buildCurlFromLog(log)).then(() => showToast('已复制 cURL 请求', 'success'));
  });
  $('#drawerCloseBtn').addEventListener('click', closeDrawer);
  $('#drawerBackdrop').addEventListener('click', closeDrawer);
}
