'use strict';

/* ============================================================
   Grok Gateway Console · 实时网关信号脉冲
   - 总览脉冲刻度板
   - 侧边栏实时通道微指示器
   ============================================================ */

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, fmtNum, fmtMs, fmtTimeShort, escapeHtml, rangeToFrom } from './utils.js';
import { healthDotHTML, applyHealthUI } from './health.js';
import { openDrawer } from './drawer.js';

export async function loadGatewayPulses() {
  const container = $('#gatewayPulseList');
  const railContainer = $('#railChannels');
  if (!container || !railContainer) return;

  const sigs = {};
  const allLogs = {};
  const from = rangeToFrom(state.range);
  const ids = gatewayIds().filter(id => state.gateways[id]);

  const results = await Promise.all(ids.map(async id => {
    try {
      const params = new URLSearchParams({ gateway: id, limit: '40' });
      if (from) params.set('from', from.toISOString());
      const data = await api('/logs?' + params.toString());
      return (data.items || []).slice().reverse();
    } catch (e) {
      return [];
    }
  }));

  let changed = false;
  ids.forEach((id, i) => {
    const logs = results[i];
    allLogs[id] = logs;
    const sig = logs.length + ':' + logs.map(l => l.id + ':' + (l.success ? 1 : 0)).join(',');
    sigs[id] = sig;
    if (sig !== state.pulseSig[id]) changed = true;
  });

  state.pulseSig = sigs;
  if (!changed) return;

  container.innerHTML = '';
  railContainer.innerHTML = '';

  for (const id of gatewayIds()) {
    const gw = state.gateways[id];
    if (!gw) continue;
    const logs = allLogs[id] || [];
    const successCount = logs.filter(l => l.success).length;
    const total = logs.length;
    const totalTokens = logs.reduce((acc, l) => {
      const usage = l.usage || {};
      const prompt = usage.prompt_tokens || usage.input_tokens || 0;
      const output = usage.output_tokens || 0;
      return acc + prompt + output;
    }, 0);

    // 1. Overview Panel Row
    const row = document.createElement('div');
    row.className = 'gw-pulse-row';
    row.innerHTML = `
      <div class="gw-pulse-top">
        <div class="gw-pulse-header-left">
          <span class="gw-dot gw-dot-${escapeHtml(id)}"></span>
          <span class="gw-pulse-name">${escapeHtml(gw.name)}</span>
          <code class="gw-pulse-prefix">${escapeHtml(gw.prefix || '')}</code>
          ${healthDotHTML(id)}
        </div>
        <div class="gw-pulse-stat">
          ${total ? `
            <span class="pulse-stat-badge ${successCount === total ? 'is-perfect' : 'is-warn'}">
              ${successCount}/${total} 正常
            </span>
            <span class="pulse-stat-tokens">${fmtNum(totalTokens)} Tokens</span>
          ` : '<span class="muted">当前范围无调用</span>'}
        </div>
      </div>
      <div class="gw-pulse-bar" data-gw="${escapeHtml(id)}">
        ${renderTicks(logs, 40)}
      </div>
    `;
    container.appendChild(row);

    // 2. Rail Sidebar Channel Item
    const chan = document.createElement('div');
    chan.className = 'rail-channel';
    chan.innerHTML = `
      <div class="rail-channel-top">
        <div class="rail-channel-info">
          <span class="gw-dot gw-dot-${escapeHtml(id)}"></span>
          <span class="rail-channel-name">${escapeHtml(gw.name)}</span>
        </div>
        <code class="rail-channel-prefix">${escapeHtml(gw.prefix || '')}</code>
      </div>
      <div class="rail-channel-ticks">
        ${renderTicks(logs, 18)}
      </div>
    `;
    railContainer.appendChild(chan);
  }

  // Bind click on ticks to open details
  container.querySelectorAll('.tick[data-log-id]').forEach(tick => {
    tick.addEventListener('click', (e) => {
      e.stopPropagation();
      openDrawer(tick.dataset.logId);
    });
  });

  applyHealthUI();
}

function renderTicks(logs, count) {
  const padded = new Array(Math.max(0, count - logs.length)).fill(null).concat(logs);
  const slice = padded.slice(-count);

  return slice.map(l => {
    if (!l) return '<span class="tick tick-empty"></span>';
    
    let cls = l.success ? 'ok' : 'err';
    const duration = l.duration_ms || 0;
    if (l.success && duration > 1500) {
      cls += ' slow';
    }

    const heightPct = Math.min(100, Math.max(25, Math.round((duration / 2500) * 80) + 20));
    const titleText = `${escapeHtml(l.model || 'API')} · ${l.status_code || (l.success ? 200 : 'ERR')} · ${fmtMs(duration)} · ${fmtTimeShort(l.started_at)}`;

    return `
      <span class="tick ${cls}"
            data-log-id="${escapeHtml(l.id)}"
            style="height:${heightPct}%"
            title="${titleText}"
            aria-label="${titleText}">
      </span>
    `;
  }).join('');
}
