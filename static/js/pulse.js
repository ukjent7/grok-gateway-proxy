'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, fmtNum, fmtMs, escapeHtml, rangeToFrom } from './utils.js';
import { healthDotHTML, applyHealthUI } from './health.js';

/* 网关脉冲（总览面板 + 侧栏实时通道）：
   先拉数据算签名，数据没变就跳过重绘，避免入场动画反复播放导致面板闪烁 */

export async function loadGatewayPulses() {
  const container = $('#gatewayPulseList');
  const railContainer = $('#railChannels');
  if (!container || !railContainer) return;

  const sigs = {};
  const allLogs = {};
  let changed = false;
  // 与总览时间范围保持一致：范围外没有请求的网关显示空状态而非旧数据
  const from = rangeToFrom(state.range);
  for (const id of gatewayIds()) {
    const gw = state.gateways[id];
    if (!gw) continue;
    let logs = [];
    try {
      const params = new URLSearchParams({ gateway: id, limit: '40' });
      if (from) params.set('from', from.toISOString());
      const data = await api('/logs?' + params.toString());
      logs = (data.items || []).slice().reverse();
    } catch (e) { /* ignore per-gateway error */ }
    allLogs[id] = logs;
    const sig = logs.length + ':' + logs.map(l => l.id).join(',');
    sigs[id] = sig;
    if (sig !== state.pulseSig[id]) changed = true;
  }
  state.pulseSig = sigs;
  if (!changed) return;

  // 数据有变化才重建面板与侧栏实时通道
  container.innerHTML = '';
  railContainer.innerHTML = '';
  for (const id of gatewayIds()) {
    const gw = state.gateways[id];
    if (!gw) continue;
    const logs = allLogs[id] || [];
    const successCount = logs.filter(l => l.success).length;
    const total = logs.length;
    // 与总览"总输入 Token"同口径：prompt_tokens 是含缓存的总输入，
    // input_tokens 只是未命中缓存的增量，仅作兜底。
    const totalTokens = logs.reduce((acc, l) => acc + ((l.usage && (l.usage.prompt_tokens || l.usage.input_tokens || 0)) + ((l.usage && l.usage.output_tokens) || 0)), 0);

    const row = document.createElement('div');
    row.className = 'gw-pulse-row';
    row.innerHTML =
      '<div class="gw-pulse-top">' +
        '<span><span class="gw-pulse-name">' + escapeHtml(gw.name) + '</span><span class="gw-pulse-prefix">' + escapeHtml(gw.prefix || '') + '</span>' + healthDotHTML(id) + '</span>' +
        '<span class="gw-pulse-stat">' + (total ? successCount + '/' + total + ' 成功 · ' + fmtNum(totalTokens) + ' tok' : '暂无数据') + '</span>' +
      '</div>' +
      '<div class="gw-pulse-bar">' + renderTicks(logs, 40) + '</div>';
    container.appendChild(row);

    const chan = document.createElement('div');
    chan.className = 'rail-channel';
    chan.innerHTML =
      '<div class="rail-channel-top">' +
        '<span class="rail-channel-name">' + escapeHtml(gw.name) + '</span>' +
        '<span class="rail-channel-prefix">' + escapeHtml(gw.prefix || '') + '</span>' +
      '</div>' +
      '<div class="rail-channel-ticks">' + renderTicks(logs, 18) + '</div>';
    railContainer.appendChild(chan);
  }
  // 行刚渲染完时同步一次健康点，避免最多等 30s 轮询才显示状态
  applyHealthUI();
}

function renderTicks(logs, count) {
  const padded = new Array(Math.max(0, count - logs.length)).fill(null).concat(logs);
  const slice = padded.slice(-count);
  return slice.map(l => {
    if (!l) return '<span class="tick"></span>';
    const cls = l.success ? 'ok' : 'err';
    const height = 35 + Math.min(65, Math.round(((l.duration_ms || 0) / 3000) * 65));
    return '<span class="tick ' + cls + '" style="height:' + height + '%" title="' + escapeHtml(l.model || '') + ' · ' + (l.status_code || '') + ' · ' + fmtMs(l.duration_ms) + '"></span>';
  }).join('');
}
