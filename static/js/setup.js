'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, escapeHtml } from './utils.js';
import { showToast } from './ui.js';

/* ============================================================
   Setup / 接入代码
   ============================================================ */

function setupBaseURL() {
  const addr = (state.listenAddr || '').trim();
  if (!addr) return 'http://127.0.0.1:8787';
  let base = addr;
  if (!/^https?:\/\//i.test(base)) base = 'http://' + base;
  return base.replace(/\/+$/, '');
}

export function buildCurl(gw) {
  const base = setupBaseURL();
  const model = (gw.name || gw.id || 'model').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') + '-model';
  const auth = '"Authorization: Bearer $GROK_PROXY_API_TOKEN"';
  if (gw.protocol === 'chat_completions') {
    const payload = JSON.stringify({ model: model, messages: [{ role: 'user', content: 'Hello' }] });
    return 'curl ' + base + gw.prefix + '/chat/completions \\\n  -H ' + JSON.stringify('Content-Type: application/json') + ' \\\n  -H ' + auth + ' \\\n  -d ' + JSON.stringify(payload);
  }
  const payload = JSON.stringify({ model: model, input: 'Hello' });
  return 'curl ' + base + gw.prefix + '/responses \\\n  -H ' + JSON.stringify('Content-Type: application/json') + ' \\\n  -H ' + auth + ' \\\n  -d ' + JSON.stringify(payload);
}

export async function loadSetup() {
  const container = $('#setupSnippets');
  if (!container) return;
  const baseInfo = $('#setupBaseUrl');
  if (baseInfo) baseInfo.innerHTML = '当前监听地址：<code>' + escapeHtml(setupBaseURL()) + '</code>，以下片段已指向该地址。';
  container.innerHTML = '<div class="empty-state">加载中…</div>';
  try {
    const data = await api('/setup');
    container.innerHTML = '';
    gatewayIds().forEach(id => {
      const snippet = data[id];
      if (!snippet) return;
      const gw = state.gateways[id] || {};
      const card = document.createElement('div');
      card.className = 'setup-card';
      card.innerHTML =
        '<div class="setup-card-head">' +
          '<strong>' + escapeHtml(gw.name || id) + ' <span class="gw-card-prefix">' + escapeHtml(gw.prefix || '') + '</span></strong>' +
          '<div class="setup-card-actions">' +
            '<button class="btn-ghost small setup-action-btn" title="复制配置文件片段">' +
              '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><rect x="5" y="5" width="8" height="8" rx="1.6" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="M3 11V3.6A1.6 1.6 0 0 1 4.6 2H11" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round"/></svg>' +
              '复制' +
            '</button>' +
            '<button class="btn-ghost small setup-action-btn copy-curl-btn" title="复制 curl 请求示例">' +
              '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M5 4l-3 4 3 4M11 4l3 4-3 4" stroke="currentColor" stroke-width="1.3" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>' +
              'cURL' +
            '</button>' +
          '</div>' +
        '</div>' +
        '<pre class="setup-pre">' + escapeHtml(snippet) + '</pre>';
      container.appendChild(card);
      card.querySelector('.copy-btn').addEventListener('click', () => {
        navigator.clipboard.writeText(snippet).then(() => showToast('已复制到剪贴板', 'success'));
      });
      card.querySelector('.copy-curl-btn').addEventListener('click', () => {
        navigator.clipboard.writeText(buildCurl(gw)).then(() => showToast('已复制 cURL 示例', 'success'));
      });
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state">加载失败：' + escapeHtml(e.message) + '</div>';
  }
}

export function initSetup() {
  $('#setupReloadBtn').addEventListener('click', () => {
    loadSetup();
    showToast('已重新加载接入代码', 'success');
  });
}
