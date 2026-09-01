'use strict';

import { state, gatewayIds } from './state.js';
import { $, $all, escapeHtml, gatewayPrefixLabel } from './utils.js';
import { showShortcutsModal } from './ui.js';

let cmdkHandlers = {};
let selectedIndex = 0;
let currentItems = [];

function buildCommands() {
  const list = [
    { id: 'view-overview', title: '切换视图：运行总览', cat: '导航', shortcut: '1', run: () => cmdkHandlers.switchView && cmdkHandlers.switchView('overview') },
    { id: 'view-logs', title: '切换视图：请求日志', cat: '导航', shortcut: '2', run: () => cmdkHandlers.switchView && cmdkHandlers.switchView('logs') },
    { id: 'view-gateways', title: '切换视图：网关配置', cat: '导航', shortcut: '3', run: () => cmdkHandlers.switchView && cmdkHandlers.switchView('gateways') },
    { id: 'view-setup', title: '切换视图：接入代码', cat: '导航', shortcut: '4', run: () => cmdkHandlers.switchView && cmdkHandlers.switchView('setup') },
    { id: 'act-refresh', title: '全局刷新数据与状态', cat: '操作', shortcut: 'R', run: () => cmdkHandlers.refreshAll && cmdkHandlers.refreshAll() },
    { id: 'act-theme', title: '切换界面深色 / 浅色模式', cat: '外观', shortcut: 'T', run: () => {
      const toggle = $('#themeToggleBtn');
      if (toggle) toggle.click();
    }},
    { id: 'act-shortcuts', title: '键盘快捷键参考帮助', cat: '帮助', shortcut: '?', run: () => showShortcutsModal() },
    { id: 'filter-err', title: '日志筛选：仅失败请求 (Errors)', cat: '日志', shortcut: '', run: () => {
      const sel = $('#filterStatus');
      if (sel) sel.value = 'failure';
      if (cmdkHandlers.switchView) cmdkHandlers.switchView('logs');
    }},
    { id: 'filter-ok', title: '日志筛选：仅成功请求 (2xx)', cat: '日志', shortcut: '', run: () => {
      const sel = $('#filterStatus');
      if (sel) sel.value = 'success';
      if (cmdkHandlers.switchView) cmdkHandlers.switchView('logs');
    }},
  ];

  gatewayIds().forEach(id => {
    const gw = state.gateways[id] || {};
    list.push({
      id: `gw-${id}`,
      title: `查看网关日志：${escapeHtml(gw.name || id)} (${escapeHtml(gatewayPrefixLabel(gw, id))})`,
      cat: '网关',
      shortcut: '',
      run: () => {
        const sel = $('#filterGateway');
        if (sel) sel.value = id;
        if (cmdkHandlers.switchView) cmdkHandlers.switchView('logs');
      }
    });
  });

  return list;
}

function renderList(items) {
  const container = $('#cmdkList');
  if (!container) return;

  currentItems = items;
  if (selectedIndex >= items.length) selectedIndex = Math.max(0, items.length - 1);

  if (!items.length) {
    container.innerHTML = '<div class="cmdk-empty">无匹配命令或网关</div>';
    return;
  }

  container.innerHTML = items.map((item, idx) => `
    <div class="cmdk-item ${idx === selectedIndex ? 'is-selected' : ''}" data-idx="${idx}" role="option" aria-selected="${idx === selectedIndex}">
      <span class="cmdk-item-cat">${escapeHtml(item.cat)}</span>
      <span class="cmdk-item-title">${escapeHtml(item.title)}</span>
      ${item.shortcut ? `<kbd class="cmdk-item-kbd">${escapeHtml(item.shortcut)}</kbd>` : ''}
    </div>
  `).join('');

  $all('.cmdk-item', container).forEach(el => {
    const idx = Number(el.dataset.idx);
    el.addEventListener('click', () => {
      executeItem(idx);
    });
    el.addEventListener('mouseenter', () => {
      selectedIndex = idx;
      updateSelection();
    });
  });
}

function updateSelection() {
  const container = $('#cmdkList');
  if (!container) return;
  $all('.cmdk-item', container).forEach((el, idx) => {
    const act = idx === selectedIndex;
    el.classList.toggle('is-selected', act);
    el.setAttribute('aria-selected', act ? 'true' : 'false');
    if (act) el.scrollIntoView({ block: 'nearest' });
  });
}

function executeItem(idx) {
  const item = currentItems[idx];
  if (item && item.run) {
    closeCmdk();
    item.run();
  }
}

export function openCmdk() {
  const modal = $('#cmdk');
  const backdrop = $('#cmdkBackdrop');
  const input = $('#cmdkInput');
  if (modal && backdrop) {
    modal.classList.add('is-open');
    modal.setAttribute('aria-hidden', 'false');
    backdrop.classList.add('is-open');
  }
  if (input) {
    input.value = '';
    input.focus();
  }
  selectedIndex = 0;
  renderList(buildCommands());
}

export function closeCmdk() {
  const modal = $('#cmdk');
  const backdrop = $('#cmdkBackdrop');
  if (modal && backdrop) {
    modal.classList.remove('is-open');
    modal.setAttribute('aria-hidden', 'true');
    backdrop.classList.remove('is-open');
  }
}

export function initCmdk(handlers = {}) {
  cmdkHandlers = handlers;

  const btn = $('#cmdKBtn');
  if (btn) btn.addEventListener('click', openCmdk);

  const backdrop = $('#cmdkBackdrop');
  if (backdrop) backdrop.addEventListener('click', closeCmdk);

  const input = $('#cmdkInput');
  if (input) {
    input.addEventListener('input', () => {
      const q = input.value.trim().toLowerCase();
      const all = buildCommands();
      const filtered = q ? all.filter(c => c.title.toLowerCase().includes(q) || c.cat.toLowerCase().includes(q)) : all;
      selectedIndex = 0;
      renderList(filtered);
    });

    input.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (currentItems.length) {
          selectedIndex = (selectedIndex + 1) % currentItems.length;
          updateSelection();
        }
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (currentItems.length) {
          selectedIndex = (selectedIndex - 1 + currentItems.length) % currentItems.length;
          updateSelection();
        }
      } else if (e.key === 'Enter') {
        e.preventDefault();
        executeItem(selectedIndex);
      }
    });
  }
}
