'use strict';

/* ============================================================
   Grok Gateway Console · 全局响应式状态管理
   ============================================================ */

import { api } from './api.js';
import { $ } from './utils.js';

export const state = {
  gateways: {},
  listenAddr: '',
  proxyURL: '',
  version: '',
  metrics: null,
  logs: [],
  logsTotal: null,
  logsOffset: 0,
  recentLogs: [],
  sparkSeries: [],
  range: '1h',
  activeView: 'overview',
  theme: localStorage.getItem('grok_theme') || 'dark',
  autoRefresh: Number(localStorage.getItem('grok_auto_refresh') || 15),
  autoRefreshTimer: null,
  drawerLog: null,
  drawerTab: 'request-compare',
  drawerDiffMode: localStorage.getItem('grok_diff_mode') || 'split',
  drawerLogId: null,
  cmdkSelected: 0,
  cmdkItems: [],
  pulseSig: {},
  metricsSig: '',
  sparkSig: '',
  recentSig: '',
  health: { upstreams: {} }
};

export function gatewayIds() {
  const ids = Object.keys(state.gateways);
  return ids.length ? ids : ['ds', 'st', 'std'];
}

export async function loadConfig() {
  const data = await api('/config');
  state.gateways = data.gateways || {};
  state.listenAddr = data.listen_addr || '';
  state.proxyURL = data.proxy_url || '';
  state.version = data.version || '';

  const listenEl = $('#listenAddr');
  if (listenEl) listenEl.textContent = state.listenAddr || '—';

  const verEl = $('#railVersion');
  if (verEl) {
    verEl.textContent = 'v' + (state.version || '1.0.0') + ' · 本地模式 · Grok Build 就绪';
  }

  const sel = $('#filterGateway');
  if (sel) {
    const prev = sel.value;
    sel.innerHTML = '<option value="">全部网关</option>';
    gatewayIds().forEach(id => {
      const gw = state.gateways[id];
      if (!gw) return;
      const opt = document.createElement('option');
      opt.value = id;
      opt.textContent = gw.name + ' (' + (gw.prefix || id) + ')';
      sel.appendChild(opt);
    });
    if (prev && [...sel.options].some(o => o.value === prev)) {
      sel.value = prev;
    }
  }
}
