'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, $all, escapeHtml } from './utils.js';
import { showToast, confirmModal } from './ui.js';
import { loadLogs } from './logs.js';
import { loadOverview } from './overview.js';

/* ============================================================
   命令面板 (Cmd+K)
   ============================================================ */

let viewSwitcher = () => {};
let refresher = () => {};

function buildCmdkItems() {
  const items = [
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><rect x="2" y="2" width="5" height="5" rx="1" fill="currentColor"/><rect x="9" y="2" width="5" height="5" rx="1" fill="currentColor" opacity=".55"/><rect x="2" y="9" width="5" height="5" rx="1" fill="currentColor" opacity=".55"/><rect x="9" y="9" width="5" height="5" rx="1" fill="currentColor"/></svg>', label: '前往 总览', hint: '1', run: () => viewSwitcher('overview') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M3 3.5h10M3 6h10M3 8.5h7M3 11h5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" fill="none"/></svg>', label: '前往 请求日志', hint: '2', run: () => viewSwitcher('logs') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><circle cx="8" cy="8" r="2" fill="currentColor"/><path d="M8 1.5v2.5M8 12v2.5M1.5 8h2.5M12.5 8h2.5" stroke="currentColor" stroke-width="1.2" fill="none"/></svg>', label: '前往 网关配置', hint: '3', run: () => viewSwitcher('gateways') },
    { group: '导航', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M5 4l-3 4 3 4M11 4l3 4-3 4" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>', label: '前往 接入代码', hint: '4', run: () => viewSwitcher('setup') },
    { group: '操作', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M2.5 8a5.5 5.5 0 0 1 9.4-3.9M13.5 8a5.5 5.5 0 0 1-9.4 3.9" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>', label: '刷新全部数据', hint: 'r', run: () => refresher() },
    { group: '操作', icon: '<svg viewBox="0 0 16 16" width="13" height="13"><path d="M3 4h10M5.5 4V2.5h5V4M5 4l.5 9h5l.5-9" stroke="currentColor" stroke-width="1.3" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>', label: '清空请求日志', hint: '', run: async () => {
      if (!(await confirmModal('确定要清空全部请求日志吗？此操作无法撤销。', '清空日志'))) return;
      try {
        const res = await api('/logs', { method: 'DELETE' });
        showToast('已清空 ' + (res.deleted || 0) + ' 条日志', 'success');
        if (state.activeView === 'logs') loadLogs(true);
        if (state.activeView === 'overview') loadOverview();
      } catch (e) { showToast('清空失败: ' + e.message, 'error'); }
    } }
  ];
  gatewayIds().forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    items.push({
      group: '复制接入片段',
      icon: '<svg viewBox="0 0 16 16" width="13" height="13"><rect x="5" y="5" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.3" fill="none"/><path d="M3 11V3.5A1.5 1.5 0 0 1 4.5 2H11" stroke="currentColor" stroke-width="1.3" fill="none" stroke-linecap="round"/></svg>',
      label: '复制 ' + gw.name + ' 接入片段 (' + gw.prefix + ')',
      hint: '',
      run: async () => {
        try {
          const data = await api('/setup');
          if (data[id]) {
            navigator.clipboard.writeText(data[id]).then(() => showToast('已复制 ' + gw.name + ' 接入片段', 'success'));
          }
        } catch (e) { showToast('复制失败: ' + e.message, 'error'); }
      }
    });
  });
  return items;
}

export function openCmdk() {
  const prevFocus = document.activeElement;
  state.cmdkItems = buildCmdkItems();
  state.cmdkSelected = 0;
  $('#cmdkInput').value = '';
  renderCmdk('');
  $('#cmdkBackdrop').classList.add('is-open');
  const cmdk = $('#cmdk');
  cmdk.classList.add('is-open');
  cmdk.setAttribute('aria-hidden', 'false');
  setTimeout(() => $('#cmdkInput').focus(), 80);
}

export function closeCmdk() {
  $('#cmdkBackdrop').classList.remove('is-open');
  const cmdk = $('#cmdk');
  cmdk.classList.remove('is-open');
  cmdk.setAttribute('aria-hidden', 'true');
}

function renderCmdk(query) {
  const list = $('#cmdkList');
  const q = query.trim().toLowerCase();
  const filtered = q ? state.cmdkItems.filter(it => it.label.toLowerCase().includes(q) || it.group.toLowerCase().includes(q)) : state.cmdkItems;
  if (!filtered.length) {
    list.innerHTML = '<div class="cmdk-empty">没有匹配的操作</div>';
    return;
  }
  let lastGroup = '';
  let html = '';
  filtered.forEach((it, i) => {
    if (it.group !== lastGroup) {
      html += '<div class="cmdk-group">' + escapeHtml(it.group) + '</div>';
      lastGroup = it.group;
    }
    const isActive = i === state.cmdkSelected ? ' is-active' : '';
    html += '<div class="cmdk-item' + isActive + '" data-index="' + i + '"><span class="cmdk-item-icon">' + it.icon + '</span><span class="cmdk-item-label">' + escapeHtml(it.label) + '</span>' + (it.hint ? '<kbd>' + escapeHtml(it.hint) + '</kbd>' : '') + '</div>';
  });
  list.innerHTML = html;
  $all('.cmdk-item', list).forEach(el => {
    el.addEventListener('click', () => {
      const i = Number(el.dataset.index);
      const item = filtered[i];
      if (item) { closeCmdk(); item.run(); }
    });
    el.addEventListener('mouseenter', () => {
      state.cmdkSelected = Number(el.dataset.index);
      $all('.cmdk-item', list).forEach(x => x.classList.toggle('is-active', x === el));
    });
  });
}

export function initCmdk({ switchView, refreshAll }) {
  viewSwitcher = switchView;
  refresher = refreshAll;
  $('#cmdKBtn').addEventListener('click', openCmdk);
  $('#cmdkBackdrop').addEventListener('click', closeCmdk);
  $('#cmdkInput').addEventListener('input', (e) => {
    state.cmdkSelected = 0;
    renderCmdk(e.target.value);
  });
  $('#cmdkInput').addEventListener('keydown', (e) => {
    const q = e.target.value.trim().toLowerCase();
    const filtered = q ? state.cmdkItems.filter(it => it.label.toLowerCase().includes(q) || it.group.toLowerCase().includes(q)) : state.cmdkItems;
    if (e.key === 'ArrowDown') { e.preventDefault(); state.cmdkSelected = (state.cmdkSelected + 1) % filtered.length; renderCmdk(e.target.value); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); state.cmdkSelected = (state.cmdkSelected - 1 + filtered.length) % filtered.length; renderCmdk(e.target.value); }
    else if (e.key === 'Enter') {
      e.preventDefault();
      const item = filtered[state.cmdkSelected];
      if (item) { closeCmdk(); item.run(); }
    }
  });
}
