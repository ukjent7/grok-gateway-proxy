'use strict';

import { api } from './api.js';
import { $ } from './utils.js';

export const state = {
  gateways: {},
  listenAddr: '',
  version: '',
  metrics: null,
  logs: [],
  logsTotal: null,
  recentLogs: [],
  sparkSeries: [],
  logsOffset: 0,
  logsLimit: 50,
  range: '1h',
  activeView: 'overview',
  drawerLog: null,
  drawerTab: 'request-compare',
  showRawHeaders: false,
  pollTimer: null,
  cmdkSelected: 0,
  cmdkItems: [],
  pulseSig: {},
  metricsSig: '',
  sparkSig: '',
  recentSig: '',
  health: { upstreams: {} }
};

// 网关显示顺序：以 /api/config 返回的键序为准（后端按 id 排序），
// 配置尚未加载时使用默认顺序。
export function gatewayIds() {
  const ids = Object.keys(state.gateways);
  return ids.length ? ids : ['oc', 'st', 've'];
}

export async function loadConfig() {
  const data = await api('/config');
  state.gateways = data.gateways || {};
  state.listenAddr = data.listen_addr || '';
  state.version = data.version || '';
  $('#listenAddr').textContent = state.listenAddr || '—';
  const verEl = $('#railVersion');
  if (verEl) verEl.textContent = 'v' + (state.version || '?') + ' · 本地模式 · Grok Build 就绪';
  const sel = $('#filterGateway');
  if (sel) {
    sel.innerHTML = '<option value="">全部网关</option>';
    gatewayIds().forEach(id => {
      const gw = state.gateways[id];
      if (!gw) return;
      const opt = document.createElement('option');
      opt.value = id;
      opt.textContent = gw.name + ' (' + gw.prefix + ')';
      sel.appendChild(opt);
    });
  }
}
