'use strict';

import { state } from './state.js';
import { api } from './api.js';
import { $, $all, escapeHtml, copyText, gatewayPrefixLabel, gatewayTone } from './utils.js';
import { showToast } from './ui.js';

export function setupBaseURL() {
  const host = window.location.host;
  const protocol = window.location.protocol;
  return `${protocol}//${host}`;
}

export async function loadSetup() {
  const snippetsEl = $('#setupSnippets');
  const baseEl = $('#setupBaseUrl');
  if (!snippetsEl) return;

  if (baseEl) {
    const base = setupBaseURL();
    baseEl.innerHTML = `
      <div class="panel setup-base-panel">
        <div class="panel-head" style="border:none; padding:0 0 8px 0;">
          <div>
            <h2>代理基础端点 (Base URL)</h2>
            <span class="muted">客户端工具配置基础调用地址</span>
          </div>
          <button type="button" class="btn-ghost small" id="copyBaseUrlBtn">复制</button>
        </div>
        <div class="setup-base-code">
          <code>${escapeHtml(base)}</code>
        </div>
      </div>
    `;
    const copyBtn = $('#copyBaseUrlBtn');
    if (copyBtn) {
      copyBtn.addEventListener('click', async () => {
        await copyText(base);
        showToast('已复制基础端点地址', 'success', 1200);
      });
    }
  }

  try {
    const data = await api('/setup');
    const entries = Object.entries(data || {});

    if (!entries.length) {
      snippetsEl.innerHTML = '<div class="empty-state"><span>暂无可用网关接入代码</span></div>';
      return;
    }

    snippetsEl.innerHTML = entries.map(([id, item]) => {
      const gw = state.gateways[id] || {};
      const snippet = item.snippet || '';
      const modelKey = item.model_key || id;

      return `
        <div class="panel setup-card" data-gw="${escapeHtml(id)}">
          <div class="panel-head">
            <div class="setup-card-title">
              <span class="gw-dot gw-tone-${gatewayTone(id)}"></span>
              <h3>${escapeHtml(gw.name || id)}</h3>
              <code class="gw-prefix-tag">${escapeHtml(gatewayPrefixLabel(gw, id))}</code>
            </div>
            <button type="button" class="btn-ghost small copy-snippet-btn" data-snippet="${escapeHtml(snippet)}">
              复制配置
            </button>
          </div>
          <div class="setup-card-body">
            <pre class="code-block"><code>${escapeHtml(snippet)}</code></pre>
          </div>
        </div>
      `;
    }).join('');

    $all('.copy-snippet-btn', snippetsEl).forEach(btn => {
      btn.addEventListener('click', async () => {
        const text = btn.dataset.snippet;
        await copyText(text);
        showToast('配置代码已复制到剪贴板', 'success', 1200);
      });
    });
  } catch (e) {
    snippetsEl.innerHTML = `<div class="empty-state"><span class="text-danger">接入代码载入失败: ${escapeHtml(e.message)}</span></div>`;
  }
}

export function initSetup() {
  const reloadBtn = $('#setupReloadBtn');
  if (reloadBtn) {
    reloadBtn.addEventListener('click', loadSetup);
  }
}
