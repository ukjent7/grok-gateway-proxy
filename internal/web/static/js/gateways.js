'use strict';

/* ============================================================
   Grok Gateway Console · 网关配置卡片模块
   - 全局代理配置与实时状态
   - 结构化网关卡片设计 (无文本换行溢出)
   - 在线连通性测试 (Test Connection)
   - 精确脏值状态追踪 (Dirty State Tracking)
   ============================================================ */

import { state, gatewayIds, loadConfig } from './state.js';
import { api } from './api.js';
import { $, $all, escapeHtml, gatewayPrefixLabel, gatewayTone } from './utils.js';
import { showToast, confirmModal } from './ui.js';
import { healthDotHTML, applyHealthUI, recordProbe, pollHealth } from './health.js';
import { setupBaseURL } from './setup.js';

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
    state.proxyURL = (res && res.proxy_url) || '';
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

function getPrefixPattern() {
  const pat = state.config && state.config.gateway_rules && state.config.gateway_rules.prefix_pattern;
  if (pat) {
    try {
      return new RegExp(pat);
    } catch (_) {}
  }
  return /^[a-z0-9][a-z0-9_-]{0,31}$/;
}

function getReservedPrefixes() {
  return (state.config && state.config.gateway_rules && state.config.gateway_rules.reserved_prefixes) || ['api', 'static', 'healthz', 'ui'];
}

function normalizePrefix(raw) {
  return String(raw || '').trim().replace(/^\/+/, '');
}

function validateNewGateway(prefix, baseURL) {
  if (!prefix) return '请填写调用前缀';
  const pattern = getPrefixPattern();
  if (!pattern.test(prefix)) {
    return '前缀只能由小写字母、数字、- 与 _ 组成，且以字母或数字开头';
  }
  const reserved = getReservedPrefixes();
  if (reserved.includes(prefix)) {
    return `前缀 /${prefix} 为系统保留端点，不可使用`;
  }
  const exists = Object.values(state.gateways || {}).some(g => (g.prefix || '').toLowerCase() === ('/' + prefix).toLowerCase() || (g.prefix || '').toLowerCase() === prefix.toLowerCase());
  if (exists) {
    return `前缀 /${prefix} 已被其他网关占用`;
  }
  if (baseURL) {
    if (!baseURL.startsWith('https://')) {
      return '上游 Base URL 必须以 https:// 开头';
    }
    try {
      new URL(baseURL);
    } catch (_) {
      return '上游 Base URL 格式无效';
    }
  }
  return '';
}

async function createNewGateway() {
  const prefixInput = $('#newGwPrefix');
  const nameInput = $('#newGwName');
  const baseInput = $('#newGwBaseURL');
  const errorEl = $('#newGwError');
  const btn = $('#newGwCreateBtn');

  const prefix = normalizePrefix(prefixInput ? prefixInput.value : '');
  const name = nameInput ? nameInput.value.trim() : '';
  const baseURL = baseInput ? baseInput.value.trim() : '';

  const err = validateNewGateway(prefix, baseURL);
  if (err) {
    if (errorEl) {
      errorEl.textContent = err;
      errorEl.hidden = false;
    }
    return;
  }
  if (errorEl) errorEl.hidden = true;

  btn.disabled = true;
  try {
    await api('/gateways', {
      method: 'POST',
      body: JSON.stringify({
        prefix,
        name: name || prefix,
        base_url: baseURL,
      })
    });
    if (prefixInput) prefixInput.value = '';
    if (nameInput) nameInput.value = '';
    if (baseInput) baseInput.value = '';
    updateNewGwPreview();
    await loadConfig();
    renderGatewayCards();
    pollHealth();
    showToast('自定义网关已成功创建', 'success');
  } catch (e) {
    if (errorEl) {
      errorEl.textContent = '创建失败: ' + e.message;
      errorEl.hidden = false;
    } else {
      showToast('创建失败: ' + e.message, 'error');
    }
  } finally {
    btn.disabled = false;
  }
}

function updateNewGwPreview() {
  const prefixInput = $('#newGwPrefix');
  const preview = $('#newGwPreview');
  if (!preview) return;
  const prefix = normalizePrefix(prefixInput ? prefixInput.value : '');
  const base = setupBaseURL();
  preview.textContent = prefix ? `${base}/${prefix}` : `${base}/<前缀>`;
}

export function renderGatewayCards() {
  renderGlobalProxySettings();
  const container = $('#gatewayCards');
  if (!container) return;

  const ids = gatewayIds();
  if (!ids.length) {
    container.innerHTML = '<div class="empty-state"><span>暂无网关配置</span></div>';
    return;
  }

  const upstreams = (state.health && state.health.upstreams) || {};
  const affinityModes = (state.config && state.config.session_affinity_modes) || ['openai', 'openrouter', 'opencode', 'off'];

  container.innerHTML = ids.map(id => {
    const gw = state.gateways[id] || {};
    const u = upstreams[id];
    const uaEnabled = !!gw.user_agent_override_enabled;
    const forwardHeadersStr = (gw.forward_headers || []).join(', ');
    const currentAffinity = gw.session_affinity || 'openai';

    return `
      <div class="panel gw-card" data-gw-id="${escapeHtml(id)}">
        <div class="panel-head">
          <div class="gw-card-title">
            <span class="gw-dot gw-tone-${gatewayTone(id)}"></span>
            <h3>${escapeHtml(gw.name || id)}</h3>
            <code class="gw-prefix-tag">${escapeHtml(gatewayPrefixLabel(gw, id))}</code>
          </div>
          <div class="gw-card-head-actions">
            ${healthDotHTML(u ? u.reachable : null)}
            <label class="toggle-wrap" title="${gw.enabled ? '已启用此网关' : '已停用此网关'}">
              <input type="checkbox" class="gw-enabled-toggle" ${gw.enabled ? 'checked' : ''}>
              <span class="toggle-slider"></span>
            </label>
          </div>
        </div>

        <div class="gw-card-form">
          <label class="field">
            <span class="field-label">上游 Base URL</span>
            <input type="text" class="input-modern gw-base-url" value="${escapeHtml(gw.base_url || '')}" placeholder="https://api.example.com" spellcheck="false">
          </label>

          <label class="field">
            <span class="field-label">请求头透传 (Forward Headers)</span>
            <input type="text" class="input-modern gw-forward-headers" value="${escapeHtml(forwardHeadersStr)}" placeholder="例如: Authorization, X-Custom-Header (逗号分隔)" spellcheck="false">
          </label>

          <label class="field">
            <span class="field-label">会话粘性模式 (Session Affinity)</span>
            <select class="input-modern gw-session-affinity">
              ${affinityModes.map(mode => `
                <option value="${escapeHtml(mode)}" ${currentAffinity === mode ? 'selected' : ''}>${escapeHtml(mode)}</option>
              `).join('')}
            </select>
          </label>

          <label class="field">
            <span class="field-label">User-Agent 伪装</span>
            <div class="gw-user-agent-row">
              <input type="text" class="input-modern gw-user-agent" value="${escapeHtml(gw.user_agent_override || '')}" placeholder="默认由网关策略决定" spellcheck="false" ${uaEnabled ? '' : 'disabled'}>
              <label class="toggle-wrap" title="${uaEnabled ? '上游请求已使用此 User-Agent' : '未启用：上游收到的是客户端原始 User-Agent'}">
                <input type="checkbox" class="gw-user-agent-toggle" ${uaEnabled ? 'checked' : ''}>
                <span class="toggle-slider"></span>
              </label>
            </div>
          </label>

          <div class="gw-card-foot">
            <div class="gw-card-left-actions">
              <button type="button" class="btn-ghost small gw-test-btn" title="立即向该网关发起一次连通探测">测试连通</button>
              ${gw.custom ? `<button type="button" class="btn-danger small gw-delete-btn" title="删除自定义网关">删除</button>` : ''}
              <span class="gw-test-result"></span>
            </div>
            <button type="button" class="btn-primary small gw-save-btn" disabled>保存</button>
          </div>
          <div class="field-error gw-card-error" hidden></div>
        </div>
      </div>
    `;
  }).join('');

  $all('.gw-card', container).forEach(card => {
    const id = card.dataset.gwId;
    const baseInput = card.querySelector('.gw-base-url');
    const headersInput = card.querySelector('.gw-forward-headers');
    const affinitySelect = card.querySelector('.gw-session-affinity');
    const uaInput = card.querySelector('.gw-user-agent');
    const uaToggle = card.querySelector('.gw-user-agent-toggle');
    const enabledToggle = card.querySelector('.gw-enabled-toggle');
    const saveBtn = card.querySelector('.gw-save-btn');
    const testBtn = card.querySelector('.gw-test-btn');
    const delBtn = card.querySelector('.gw-delete-btn');
    const errEl = card.querySelector('.gw-card-error');

    const markDirty = () => {
      if (saveBtn) {
        saveBtn.disabled = false;
        saveBtn.classList.add('is-dirty');
      }
    };

    const runProbe = async () => {
      const probe = await api(`/gateways/${encodeURIComponent(id)}/test`, { method: 'POST' });
      recordProbe(id, probe);
      const dotSlot = card.querySelector('.gw-card-head-actions .dot');
      if (dotSlot) dotSlot.outerHTML = healthDotHTML(probe ? probe.reachable : null);
      applyHealthUI(state.health);
      return probe;
    };

    if (baseInput) baseInput.addEventListener('input', markDirty);
    if (headersInput) headersInput.addEventListener('input', markDirty);
    if (affinitySelect) affinitySelect.addEventListener('change', markDirty);
    if (uaInput) uaInput.addEventListener('input', markDirty);
    if (enabledToggle) enabledToggle.addEventListener('change', markDirty);
    if (uaToggle) {
      uaToggle.addEventListener('change', () => {
        markDirty();
        if (uaInput) uaInput.disabled = !uaToggle.checked;
      });
    }

    if (saveBtn) {
      saveBtn.addEventListener('click', async () => {
        saveBtn.disabled = true;
        saveBtn.classList.remove('is-dirty');
        if (errEl) errEl.hidden = true;
        try {
          const forwardHeaders = (headersInput ? headersInput.value : '')
            .split(',')
            .map(s => s.trim())
            .filter(Boolean);

          await api(`/gateways/${encodeURIComponent(id)}`, {
            method: 'PATCH',
            body: JSON.stringify({
              base_url: baseInput ? baseInput.value.trim() : '',
              forward_headers: forwardHeaders,
              session_affinity: affinitySelect ? affinitySelect.value : 'openai',
              user_agent_override: uaInput ? uaInput.value.trim() : '',
              user_agent_override_enabled: uaToggle ? uaToggle.checked : false,
              enabled: enabledToggle ? enabledToggle.checked : true,
            })
          });
          await loadConfig();
          await runProbe().catch(() => {});
          renderGatewayCards();
          showToast(`网关 ${id} 配置已保存`, 'success');
        } catch (e) {
          saveBtn.disabled = false;
          saveBtn.classList.add('is-dirty');
          if (errEl) {
            errEl.textContent = '保存失败: ' + e.message;
            errEl.hidden = false;
          } else {
            showToast('保存失败: ' + e.message, 'error');
          }
        }
      });
    }

    if (testBtn) {
      testBtn.addEventListener('click', async () => {
        testBtn.disabled = true;
        const orig = testBtn.textContent;
        testBtn.textContent = '探测中…';
        const resSlot = card.querySelector('.gw-test-result');
        if (resSlot) resSlot.innerHTML = '';
        try {
          const probe = await runProbe();
          if (probe && probe.reachable) {
            showToast(`网关 ${id} 连通正常${probe.status ? ` (HTTP ${probe.status})` : ''}`, 'success', 2000);
            if (resSlot) resSlot.innerHTML = `<span class="text-success">${probe.status ? 'HTTP ' + probe.status : '连通正常'}</span>`;
          } else {
            showToast(`网关 ${id} 探测失败: ${(probe && probe.error) || '上游不可达'}`, 'error', 4000);
            if (resSlot) resSlot.innerHTML = `<span class="text-danger" title="${escapeHtml((probe && probe.error) || '不可达')}">${probe && probe.status ? 'HTTP ' + probe.status : '探测失败'}</span>`;
          }
        } catch (e) {
          showToast(`网关 ${id} 探测失败: ` + e.message, 'error');
          if (resSlot) resSlot.innerHTML = `<span class="text-danger">异常</span>`;
        } finally {
          testBtn.disabled = false;
          testBtn.textContent = orig;
        }
      });
    }

    if (delBtn) {
      delBtn.addEventListener('click', () => {
        confirmModal('删除网关', `确定要删除自定义网关 /${id} 吗？此操作无法撤销。`, async () => {
          try {
            await api(`/gateways/${encodeURIComponent(id)}`, { method: 'DELETE' });
            await loadConfig();
            renderGatewayCards();
            showToast(`网关 /${id} 已删除`, 'success');
          } catch (e) {
            showToast('删除网关失败: ' + e.message, 'error');
          }
        });
      });
    }
  });
}

export function initGateways() {
  const saveProxyBtn = $('#proxyUrlSaveBtn');
  if (saveProxyBtn) saveProxyBtn.addEventListener('click', saveProxyURL);

  const createGwBtn = $('#newGwCreateBtn');
  if (createGwBtn) createGwBtn.addEventListener('click', createNewGateway);

  const newGwPrefix = $('#newGwPrefix');
  if (newGwPrefix) newGwPrefix.addEventListener('input', updateNewGwPreview);

  const reloadBtn = $('#gatewaysReloadBtn');
  if (reloadBtn) {
    reloadBtn.addEventListener('click', async () => {
      await loadConfig();
      renderGatewayCards();
      pollHealth();
      showToast('网关配置已重新载入', 'success');
    });
  }
}
