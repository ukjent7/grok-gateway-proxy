'use strict';

import { state, gatewayIds, loadConfig } from './state.js';
import { api } from './api.js';
import { $, escapeHtml, escapeAttr } from './utils.js';
import { showToast } from './ui.js';
import { healthDotHTML, applyHealthUI } from './health.js';

/* ============================================================
   网关配置卡片
   ============================================================ */

function renderGlobalProxySettings() {
  const input = $('#proxyUrlInput');
  if (input && document.activeElement !== input) input.value = state.proxyURL || '';
}

function proxyURLIsValid(value) {
  if (!value) return true;
  try {
    const parsed = new URL(value);
    return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && !!parsed.host;
  } catch (e) {
    return false;
  }
}

async function saveProxyURL() {
  const input = $('#proxyUrlInput');
  const error = $('#proxyUrlError');
  const button = $('#proxyUrlSaveBtn');
  if (!input || !button) return;
  const proxyURL = input.value.trim();
  if (!proxyURLIsValid(proxyURL)) {
    if (error) {
      error.textContent = '代理地址必须是 http:// 或 https:// URL';
      error.hidden = false;
    }
    return;
  }
  if (error) error.hidden = true;
  const original = button.textContent;
  button.disabled = true;
  button.textContent = '保存中…';
  try {
    const res = await api('/proxy', { method: 'PATCH', body: JSON.stringify({ proxy_url: proxyURL }) });
    state.proxyURL = res.proxy_url || '';
    renderGlobalProxySettings();
    showToast(state.proxyURL ? '全局代理地址已更新' : '已清空全局代理地址', 'success');
  } catch (e) {
    if (error) {
      error.textContent = '保存失败：' + e.message;
      error.hidden = false;
    } else {
      showToast('保存代理地址失败: ' + e.message, 'error');
    }
  } finally {
    button.disabled = false;
    button.textContent = original;
  }
}

export function renderGatewayCards() {
  renderGlobalProxySettings();
  const container = $('#gatewayCards');
  if (!container) return;
  container.innerHTML = '';
  gatewayIds().forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    const card = document.createElement('div');
    card.className = 'gw-card' + (gw.enabled ? '' : ' is-disabled');
    card.dataset.id = id;
    const headers = (gw.forward_headers || []).join('\n');
    card.innerHTML =
      '<div class="gw-card-head">' +
        '<div class="gw-card-title">' +
          '<strong>' + escapeHtml(gw.name) + '</strong>' +
          '<span class="gw-card-prefix">' + escapeHtml(gw.prefix || '') + '</span>' +
          healthDotHTML(id) +
          (gw.enabled ? '' : '<span class="gw-badge is-off">已停用</span>') +
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
        '<span class="field-error" hidden></span>' +
      '</div>' +
      '<div class="field">' +
        '<label>上游地址 (base_url)</label>' +
        '<input type="text" class="f-baseurl" value="' + escapeAttr(gw.base_url) + '" placeholder="https://…">' +
        '<span class="field-error" hidden></span>' +
      '</div>' +
      '<div class="toggle-row proxy-toggle-row">' +
        '<div><span class="switch-label">使用全局代理</span><span class="field-hint">使用上方配置的 HTTP / HTTPS 地址</span></div>' +
        '<label class="toggle"><input type="checkbox" class="f-proxy" ' + (gw.use_proxy !== false ? 'checked' : '') + '><span class="toggle-track"></span></label>' +
      '</div>' +
      '<div class="toggle-row ua-toggle-row">' +
        '<div><span class="switch-label">User-Agent 覆盖</span><span class="field-hint">仅作用于此网关</span></div>' +
        '<label class="toggle"><input type="checkbox" class="f-ua-enabled" ' + (gw.user_agent_override_enabled ? 'checked' : '') + '><span class="toggle-track"></span></label>' +
      '</div>' +
      '<div class="field">' +
        '<label>User-Agent 覆盖值</label>' +
        '<input type="text" class="f-ua" value="' + escapeAttr(gw.user_agent_override || '') + '" placeholder="grok-gateway-proxy/dev">' +
      '</div>' +
      (gw.id === 've' ?
      '<div class="toggle-row fx-toggle-row">' +
        '<div><span class="switch-label">FX 免费池伪装（默认关闭）</span><span class="field-hint">开启后把 Vercel 请求改写为官方 fx 客户端协议：覆盖 User-Agent / HTTP-Referer / X-Title 头，并在请求体注入 user-agent 与 x-title，同时把 Responses 协议转换为 v3 language-model 协议发往 /v3/ai/language-model</span></div>' +
        '<label class="toggle"><input type="checkbox" class="f-fx" ' + (gw.fx_disguise_enabled ? 'checked' : '') + '><span class="toggle-track"></span></label>' +
      '</div>' +
      '<div class="field">' +
        '<label>FX 伪装 User-Agent</label>' +
        '<input type="text" class="f-fx-ua" value="' + escapeAttr(gw.fx_disguise_user_agent || 'fx/0.0.3') + '" placeholder="fx/0.0.3">' +
      '</div>' : '') +
      '<div class="field">' +
        '<label>请求头白名单（每行一个）</label>' +
        '<textarea class="f-headers" rows="4" placeholder="Authorization&#10;X-Api-Key">' + escapeHtml(headers) + '</textarea>' +
      '</div>' +
      '<div class="gw-card-foot">' +
        '<button class="btn-primary small save-gw-btn">' +
          '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M4 8l3 3 5-6" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>' +
          '保存更改' +
        '</button>' +
      '</div>';
    container.appendChild(card);

    // 输入时实时内联校验。error 提示框与输入框同属一个 .field，直接反查，
    // 避免依赖 nth-child 序号（卡片结构一变就错位）。
    const urlInput = card.querySelector('.f-baseurl');
    const nameInput = card.querySelector('.f-name');
    const urlErr = urlInput.closest('.field').querySelector('.field-error');
    const nameErr = nameInput.closest('.field').querySelector('.field-error');
    urlInput.addEventListener('input', () => {
      const v = urlInput.value.trim();
      const bad = v !== '' && !v.startsWith('https://');
      urlErr.textContent = bad ? 'base_url 必须以 https:// 开头' : '';
      urlErr.hidden = !bad;
      urlInput.classList.toggle('is-invalid', bad);
    });
    nameInput.addEventListener('input', () => {
      const bad = nameInput.value.trim() === '';
      nameErr.textContent = bad ? '显示名称不能为空' : '';
      nameErr.hidden = !bad;
      nameInput.classList.toggle('is-invalid', bad);
    });
    card.querySelector('.save-gw-btn').addEventListener('click', () => saveGateway(id, card));
  });
  applyHealthUI();
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
    use_proxy: card.querySelector('.f-proxy').checked,
    forward_headers: card.querySelector('.f-headers').value.split('\n').map(s => s.trim()).filter(Boolean)
  };
  const fxToggle = card.querySelector('.f-fx');
  const fxUA = card.querySelector('.f-fx-ua');
  if (fxToggle) payload.fx_disguise_enabled = fxToggle.checked;
  if (fxUA) payload.fx_disguise_user_agent = fxUA.value.trim();
  // 发送前校验；把出错的字段就地标出。
  let ok = true;
  const urlInput = card.querySelector('.f-baseurl');
  const nameInput = card.querySelector('.f-name');
  const urlErr = urlInput.closest('.field').querySelector('.field-error');
  const nameErr = nameInput.closest('.field').querySelector('.field-error');
  if (!payload.base_url.startsWith('https://')) {
    ok = false;
    urlErr.textContent = 'base_url 必须以 https:// 开头';
    urlErr.hidden = false;
    urlInput.classList.add('is-invalid');
  }
  if (!payload.name) {
    ok = false;
    nameErr.textContent = '显示名称不能为空';
    nameErr.hidden = false;
    nameInput.classList.add('is-invalid');
  }
  if (!ok) { showToast('请修正高亮字段', 'error'); return; }

  btn.disabled = true;
  btn.innerHTML = '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M8 2v3M8 11v3M2 8h3M11 8h3" stroke="currentColor" stroke-width="1.6" fill="none" stroke-linecap="round"/></svg>保存中…';
  try {
    const res = await api('/gateways/' + id, { method: 'PATCH', body: JSON.stringify(payload) });
    if (res.gateway) state.gateways[id] = res.gateway;
    showToast('网关 ' + payload.name + ' 已更新', 'success');
    renderGatewayCards();
  } catch (e) {
    showToast('保存失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
    btn.innerHTML = original;
  }
}

export function initGateways() {
  const reloadButton = $('#gatewaysReloadBtn');
  if (reloadButton) reloadButton.addEventListener('click', async () => {
    await loadConfig();
    renderGatewayCards();
    showToast('已重新加载网关配置', 'success');
  });
  const proxyButton = $('#proxyUrlSaveBtn');
  if (proxyButton) proxyButton.addEventListener('click', saveProxyURL);
}
