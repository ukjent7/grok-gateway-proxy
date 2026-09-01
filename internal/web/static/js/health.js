'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, escapeHtml, gatewayPrefixLabel } from './utils.js';

export function healthDotHTML(reachable) {
  if (reachable === true) return '<span class="dot dot-live" title="上游连通正常"></span>';
  if (reachable === false) return '<span class="dot dot-dead" title="上游不可达或连接异常"></span>';
  return '<span class="dot dot-idle" title="未配置或未探测"></span>';
}

export function applyHealthUI(health) {
  state.health = health;
  const upstreams = (health && health.upstreams) || {};

  const dot = $('#healthDot');
  const txt = $('#healthText');
  const code = $('#healthCode');

  let allOk = true;
  let hasChecked = false;
  for (const [id, u] of Object.entries(upstreams)) {
    const gw = state.gateways[id];
    if (gw && gw.enabled && gw.base_url) {
      hasChecked = true;
      if (!u.reachable) allOk = false;
    }
  }

  if (dot && txt && code) {
    if (!hasChecked) {
      dot.className = 'dot dot-idle';
      txt.textContent = '代理监听中 (未配置上游)';
      code.textContent = 'ready';
    } else if (allOk) {
      dot.className = 'dot dot-live';
      txt.textContent = '网关上游均可达';
      code.textContent = 'online';
    } else {
      dot.className = 'dot dot-dead';
      txt.textContent = '部分网关连接异常';
      code.textContent = 'warning';
    }
  }

  // Update rail channels
  const channels = $('#railChannels');
  if (channels) {
    channels.innerHTML = gatewayIds().map(id => {
      const gw = state.gateways[id] || {};
      const u = upstreams[id];
      const reachable = u ? u.reachable : null;
      let statusCls = 'is-idle';
      if (reachable === true) statusCls = 'is-live';
      if (reachable === false) statusCls = 'is-dead';

      return `
        <div class="rail-channel-item ${statusCls}" title="${escapeHtml(gw.name || id)}: ${u && u.error ? escapeHtml(u.error) : (reachable ? '连通' : '未连接')}">
          <span class="rail-channel-dot"></span>
          <span class="rail-channel-name">${escapeHtml(gw.name || id)}</span>
          <code class="rail-channel-prefix">${escapeHtml(gatewayPrefixLabel(gw, id))}</code>
        </div>
      `;
    }).join('');
  }
}

export function recordProbe(gatewayID, entry) {
  if (!state.health || !state.health.upstreams) {
    state.health = { ...(state.health || {}), upstreams: {} };
  }
  state.health.upstreams[gatewayID] = entry;
}

export async function pollHealth() {
  try {
    const data = await api('/healthz');
    applyHealthUI(data);
    return data;
  } catch (e) {
    return null;
  }
}
