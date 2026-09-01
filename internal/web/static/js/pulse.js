'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, escapeHtml, fmtMs, latencyThresholds, gatewayPrefixLabel, gatewayTone } from './utils.js';
import { openDrawer } from './drawer.js';

export async function loadGatewayPulses() {
  const container = $('#gatewayPulseList');
  if (!container) return;

  try {
    const pulseData = await api('/pulse?limit=40');
    const byGateway = (pulseData && pulseData.gateways) || {};

    const gws = gatewayIds();
    if (!gws.length) {
      container.innerHTML = '<div class="empty-state"><span>未配置网关</span></div>';
      return;
    }

    container.innerHTML = gws.map(id => {
      const gw = state.gateways[id] || {};
      const pulses = byGateway[id] || [];
      const thresholds = latencyThresholds(pulses);

      const bars = pulses.map(p => {
        const ms = p.duration_ms || 0;
        let kind = 'ok';
        if (!p.success) kind = 'err';
        else if (thresholds.relative && ms > thresholds.slow) kind = 'warn';

        const height = Math.max(15, Math.min(100, Math.round((ms / (thresholds.slow * 1.5 || 2000)) * 100)));
        return `
          <div class="pulse-bar is-${kind}" style="height:${height}%" title="${escapeHtml(p.model || '')} · ${fmtMs(ms)}${p.success ? '' : ' (失败)'}" data-log-id="${escapeHtml(p.id)}"></div>
        `;
      }).join('');

      return `
        <div class="gw-pulse-row" data-gw="${escapeHtml(id)}">
          <div class="gw-pulse-info">
            <span class="gw-dot gw-tone-${gatewayTone(id)}"></span>
            <span class="gw-pulse-name">${escapeHtml(gw.name || id)}</span>
            <code class="gw-pulse-prefix">${escapeHtml(gatewayPrefixLabel(gw, id))}</code>
          </div>
          <div class="gw-pulse-bars">
            ${bars || '<span class="muted" style="font-size:11px;">无调用信号</span>'}
          </div>
        </div>
      `;
    }).join('');

    container.querySelectorAll('.pulse-bar[data-log-id]').forEach(bar => {
      bar.addEventListener('click', (e) => {
        e.stopPropagation();
        openDrawer(bar.dataset.logId);
      });
    });
  } catch (e) {
    container.innerHTML = `<div class="empty-state"><span class="text-danger">信号流载入失败: ${escapeHtml(e.message)}</span></div>`;
  }
}
