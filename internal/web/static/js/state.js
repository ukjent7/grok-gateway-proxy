'use strict';

import { api } from './api.js';
import { $, escapeHtml, gatewayPrefixLabel } from './utils.js';

export const state = {
  activeView: 'overview',
  config: null,
  gateways: {},
  proxyURL: '',
  listenAddr: '127.0.0.1:8787',
  range: '1h',
  metrics: null,
  metricsSig: '',
  recentLogs: [],
  recentSig: '',
  logs: [],
  logsTotal: 0,
  logsOffset: 0,
  autoRefresh: Number(localStorage.getItem('grok_auto_refresh') || '15'),
  autoRefreshTimer: null,
  drawerLogId: null,
  drawerLog: null,
  drawerSection: 'request',
  drawerMode: 'diff',
  drawerSide: 'client',
  health: null,
};

export function gatewayIds() {
  return Object.keys(state.gateways || {});
}

export async function loadConfig() {
  const cfg = await api('/config');
  state.config = cfg;
  state.gateways = cfg.gateways || {};
  state.proxyURL = cfg.proxy_url || '';
  state.listenAddr = cfg.listen_addr || '127.0.0.1:8787';

  const listenEl = $('#listenAddr');
  if (listenEl) listenEl.textContent = state.listenAddr;

  const verEl = $('#railVersion');
  if (verEl && cfg.version) verEl.textContent = `v${cfg.version} · 本地模式`;

  const sel = $('#filterGateway');
  if (sel) {
    const cur = sel.value;
    sel.innerHTML = '<option value="">全部网关</option>' +
      gatewayIds().map(id => {
        const gw = state.gateways[id];
        return `<option value="${escapeHtml(id)}">${escapeHtml(gw.name || id)} (${escapeHtml(gatewayPrefixLabel(gw, id))})</option>`;
      }).join('');
    sel.value = cur;
  }
  return cfg;
}
