'use strict';

/* ============================================================
   Grok Gateway Console · 网关配置卡片模块
   - 全局代理配置与校验
   - 动态网关卡片与实时状态
   - 在线连通性测试 (Test Connection)
   - 字段修改跟踪与内联校验
   ============================================================ */

import { state, gatewayIds, loadConfig } from './state.js';
import { api } from './api.js';
import { $, escapeHtml, escapeAttr } from './utils.js';
import { showToast } from './ui.js';
import { healthDotHTML, applyHealthUI, pollHealth } from './health.js';

function renderGlobalProxySettings() {
  const input = $('#proxyUrlInput');
  if (input && document.activeElement !== input) {
    input.value = state.proxyURL || '';
  }
  const statusEl = $('#globalProxyStatus');
  if (statusEl) {
    if (state.proxyURL) {
      statusEl.innerHTML = `<span class="proxy-status-active"><span class="dot dot-live"></span>已启用代理：<code>${escapeHtml(state.proxyURL)}</code></span>`;
    } else {
      statusEl.innerHTML = '<span class="proxy-status-direct"><span class="dot"></span>直连模式（未配置全局代理）</span>';
    }
  }
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
      error.textContent = '代理地址格式不正确，必须是以 http:// 或 https:// 开头的合法 URL';
      error.hidden = false;
    }
    return;
  }
  if (error) error.hidden = true;

  const originalText = button.innerHTML;
  button.disabled = true;
  button.innerHTML = '<span class="btn-spinner"></span> 保存中…';

  try {
    const res = await api('/proxy', {
      method: 'PATCH',
      body: JSON.stringify({ proxy_url: proxyURL })
    });
    state.proxyURL = res.proxy_url || '';
    renderGlobalProxySettings();
    showToast(state.proxyURL ? '全局代理地址已成功保存' : '已清除全局代理（恢复默认直连）', 'success');
  } catch (e) {
    if (error) {
      error.textContent = '保存失败：' + e.message;
      error.hidden = false;
    } else {
      showToast('保存代理失败: ' + e.message, 'error');
    }
  } finally {
    button.disabled = false;
    button.innerHTML = originalText;
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

    card.innerHTML = `
      <div class="gw-card-head">
        <div class="gw-card-title-group">
          <div class="gw-card-title">
            <span class="gw-dot gw-dot-${escapeHtml(id)}"></span>
            <strong>${escapeHtml(gw.name)}</strong>
            <code class="gw-card-prefix">${escapeHtml(gw.prefix || '')}</code>
            ${healthDotHTML(id)}
            ${gw.enabled ? '<span class="gw-badge is-on">已启用</span>' : '<span class="gw-badge is-off">已停用</span>'}
          </div>
          <div class="gw-card-protocol-tag">
            ${gw.protocol === 'chat_completions' ? 'Chat Completions 协议' : 'Responses 协议'}
          </div>
        </div>
        <div class="gw-card-top-actions">
          <button type="button" class="btn-ghost small test-conn-btn" data-gw="${escapeHtml(id)}" title="测试与该网关的连通性">
            <svg viewBox="0 0 16 16" width="12" height="12" fill="none"><path d="M1 8h3l2-4 4 8 2-4h3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>
            测试连通性
          </button>
          <label class="toggle toggle-large" title="启用 / 停用网关">
            <input type="checkbox" class="f-enabled" ${gw.enabled ? 'checked' : ''}>
            <span class="toggle-track"></span>
          </label>
        </div>
      </div>

      <div class="test-conn-result" id="testResult-${escapeHtml(id)}" hidden></div>

      <div class="gw-card-body">
        <div class="field-grid-2">
          <div class="field">
            <label class="field-label">网关名称</label>
            <input type="text" class="f-name input-modern" value="${escapeAttr(gw.name)}" placeholder="网关显示名称">
            <span class="field-error" hidden></span>
          </div>
          <div class="field">
            <label class="field-label">上游基地址 (Base URL)</label>
            <input type="text" class="f-baseurl input-modern" value="${escapeAttr(gw.base_url)}" placeholder="https://api.openai.com/v1">
            <span class="field-error" hidden></span>
          </div>
        </div>

        <div class="gw-card-toggles">
          <div class="toggle-row">
            <div class="toggle-info">
              <span class="switch-label">使用全局代理</span>
              <span class="field-hint">使用上方配置的 HTTP/HTTPS 代理转发请求</span>
            </div>
            <label class="toggle">
              <input type="checkbox" class="f-proxy" ${gw.use_proxy !== false ? 'checked' : ''}>
              <span class="toggle-track"></span>
            </label>
          </div>

          <div class="toggle-row">
            <div class="toggle-info">
              <span class="switch-label">User-Agent 覆盖</span>
              <span class="field-hint">向上游发起请求时伪装或重写 User-Agent 头</span>
            </div>
            <label class="toggle">
              <input type="checkbox" class="f-ua-enabled" ${gw.user_agent_override_enabled ? 'checked' : ''}>
              <span class="toggle-track"></span>
            </label>
          </div>
        </div>

        <div class="field f-ua-wrap ${gw.user_agent_override_enabled ? '' : 'is-collapsed'}">
          <label class="field-label">自定义 User-Agent 标头</label>
          <input type="text" class="f-ua input-modern" value="${escapeAttr(gw.user_agent_override || '')}" placeholder="grok-gateway-proxy/dev">
        </div>

        ${gw.id === 've' ? `
          <div class="gw-special-panel">
            <div class="toggle-row">
              <div class="toggle-info">
                <span class="switch-label font-bold">FX 免费池协议伪装 (Vercel)</span>
                <span class="field-hint">自动把 Responses 协议转为官方 fx 客户端 v3 协议发往 /v3/ai/language-model，重写 Referer/X-Title 并注入环境指纹</span>
              </div>
              <label class="toggle">
                <input type="checkbox" class="f-fx" ${gw.fx_disguise_enabled ? 'checked' : ''}>
                <span class="toggle-track"></span>
              </label>
            </div>
            <div class="field f-fx-ua-wrap ${gw.fx_disguise_enabled ? '' : 'is-collapsed'}" style="margin-top: 8px;">
              <label class="field-label">FX 伪装客户端标识</label>
              <input type="text" class="f-fx-ua input-modern" value="${escapeAttr(gw.fx_disguise_user_agent || 'fx/0.0.3')}" placeholder="fx/0.0.3">
            </div>
          </div>
        ` : ''}

        <div class="field">
          <div class="field-label-row">
            <label class="field-label">请求头白名单 (Forward Headers)</label>
            <span class="field-hint">每行一个标头名，代理将转发这些标头</span>
          </div>
          <textarea class="f-headers textarea-modern" rows="3" placeholder="Authorization&#10;X-Api-Key&#10;X-Session-Id">${escapeHtml(headers)}</textarea>
          <div class="preset-pills">
            <span class="preset-pill-label">快捷添加：</span>
            <button type="button" class="btn-preset" data-header="Authorization">+ Authorization</button>
            <button type="button" class="btn-preset" data-header="X-Api-Key">+ X-Api-Key</button>
            <button type="button" class="btn-preset" data-header="X-Session-Id">+ X-Session-Id</button>
          </div>
        </div>
      </div>

      <div class="gw-card-foot">
        <div class="gw-dirty-indicator" hidden>
          <span class="dot dot-warn"></span>
          <span>有未保存的修改</span>
        </div>
        <button type="button" class="btn-primary small save-gw-btn">
          <svg viewBox="0 0 16 16" width="12" height="12" fill="none"><path d="M4 8l3 3 5-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>
          保存配置
        </button>
      </div>
    `;

    container.appendChild(card);

    // Event listeners
    bindGatewayCardEvents(id, card);
  });

  applyHealthUI();
}

function bindGatewayCardEvents(id, card) {
  const urlInput = card.querySelector('.f-baseurl');
  const nameInput = card.querySelector('.f-name');
  const uaToggle = card.querySelector('.f-ua-enabled');
  const uaWrap = card.querySelector('.f-ua-wrap');
  const fxToggle = card.querySelector('.f-fx');
  const fxWrap = card.querySelector('.f-fx-ua-wrap');
  const dirtyIndicator = card.querySelector('.gw-dirty-indicator');
  const saveBtn = card.querySelector('.save-gw-btn');
  const testBtn = card.querySelector('.test-conn-btn');
  const testResult = card.querySelector(`#testResult-${id}`);

  const markDirty = () => {
    if (dirtyIndicator) dirtyIndicator.hidden = false;
  };

  card.querySelectorAll('input, textarea').forEach(el => {
    el.addEventListener('input', markDirty);
    el.addEventListener('change', markDirty);
  });

  if (uaToggle && uaWrap) {
    uaToggle.addEventListener('change', () => {
      uaWrap.classList.toggle('is-collapsed', !uaToggle.checked);
    });
  }

  if (fxToggle && fxWrap) {
    fxToggle.addEventListener('change', () => {
      fxWrap.classList.toggle('is-collapsed', !fxToggle.checked);
    });
  }

  // Preset pills
  card.querySelectorAll('.btn-preset').forEach(btn => {
    btn.addEventListener('click', () => {
      const header = btn.dataset.header;
      const textarea = card.querySelector('.f-headers');
      if (!textarea) return;
      const current = textarea.value.split('\n').map(s => s.trim()).filter(Boolean);
      if (!current.includes(header)) {
        current.push(header);
        textarea.value = current.join('\n');
        markDirty();
        showToast(`已添加标头 ${header}`, 'info', 1200);
      }
    });
  });

  // Validation on input
  const urlErr = urlInput.closest('.field').querySelector('.field-error');
  const nameErr = nameInput.closest('.field').querySelector('.field-error');

  urlInput.addEventListener('input', () => {
    const v = urlInput.value.trim();
    const bad = v !== '' && !v.startsWith('https://');
    urlErr.textContent = bad ? 'Base URL 必须以 https:// 开头' : '';
    urlErr.hidden = !bad;
    urlInput.classList.toggle('is-invalid', bad);
  });

  nameInput.addEventListener('input', () => {
    const bad = nameInput.value.trim() === '';
    nameErr.textContent = bad ? '网关名称不能为空' : '';
    nameErr.hidden = !bad;
    nameInput.classList.toggle('is-invalid', bad);
  });

  // Test Connection
  if (testBtn && testResult) {
    testBtn.addEventListener('click', async () => {
      testBtn.disabled = true;
      testResult.hidden = false;
      testResult.className = 'test-conn-result is-probing';
      testResult.innerHTML = '<span class="btn-spinner"></span> 正在测试网关连接…';

      const t0 = performance.now();
      try {
        await pollHealth();
        const latency = Math.round(performance.now() - t0);
        const upstreams = (state.health && state.health.upstreams) || {};
        const h = upstreams[id];

        if (h && h.reachable) {
          testResult.className = 'test-conn-result is-ok';
          testResult.innerHTML = `
            <span class="dot dot-live"></span>
            <span>连接正常 · 状态码 ${h.status || 200} · 探测耗时 ${latency}ms</span>
          `;
        } else {
          testResult.className = 'test-conn-result is-err';
          testResult.innerHTML = `
            <span class="dot dot-error"></span>
            <span>连接失败：${escapeHtml((h && h.error) || '上游返回异常')} · ${latency}ms</span>
          `;
        }
      } catch (e) {
        testResult.className = 'test-conn-result is-err';
        testResult.innerHTML = `<span class="dot dot-error"></span> 探测异常: ${escapeHtml(e.message)}`;
      } finally {
        testBtn.disabled = false;
        setTimeout(() => {
          if (testResult && testResult.classList.contains('is-ok')) {
            testResult.hidden = true;
          }
        }, 5000);
      }
    });
  }

  // Save
  saveBtn.addEventListener('click', () => saveGateway(id, card));
}

async function saveGateway(id, card) {
  const btn = card.querySelector('.save-gw-btn');
  const originalHTML = btn.innerHTML;

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

  let ok = true;
  const urlInput = card.querySelector('.f-baseurl');
  const nameInput = card.querySelector('.f-name');
  const urlErr = urlInput.closest('.field').querySelector('.field-error');
  const nameErr = nameInput.closest('.field').querySelector('.field-error');

  if (!payload.base_url.startsWith('https://')) {
    ok = false;
    urlErr.textContent = 'Base URL 必须以 https:// 开头';
    urlErr.hidden = false;
    urlInput.classList.add('is-invalid');
  }
  if (!payload.name) {
    ok = false;
    nameErr.textContent = '网关名称不能为空';
    nameErr.hidden = false;
    nameInput.classList.add('is-invalid');
  }

  if (!ok) {
    showToast('请修正表单中的错误项', 'error');
    return;
  }

  btn.disabled = true;
  btn.innerHTML = '<span class="btn-spinner"></span> 保存中…';

  try {
    const res = await api('/gateways/' + id, {
      method: 'PATCH',
      body: JSON.stringify(payload)
    });
    if (res.gateway) state.gateways[id] = res.gateway;
    showToast(`网关 ${payload.name} 配置已更新`, 'success');
    renderGatewayCards();
  } catch (e) {
    showToast('保存失败: ' + e.message, 'error');
  } finally {
    btn.disabled = false;
    btn.innerHTML = originalHTML;
  }
}

export function initGateways() {
  const reloadButton = $('#gatewaysReloadBtn');
  if (reloadButton) {
    reloadButton.addEventListener('click', async () => {
      await loadConfig();
      renderGatewayCards();
      showToast('已重新载入网关配置', 'success');
    });
  }

  const proxyButton = $('#proxyUrlSaveBtn');
  if (proxyButton) {
    proxyButton.addEventListener('click', saveProxyURL);
  }
}
