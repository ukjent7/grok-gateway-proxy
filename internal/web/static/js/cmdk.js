'use strict';

/* ============================================================
   Grok Gateway Console · 全局命令面板 (Cmd+K)
   - 模糊检索与快速操作
   - 键盘导航 (↑/↓/↵/esc)
   - 分类群组展示 (导航 / 操作 / 导出 / 代码)
   ============================================================ */

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, $all, escapeHtml, copyText } from './utils.js';
import { showToast, confirmModal, showShortcutsModal, toggleTheme } from './ui.js';
import { loadLogs } from './logs.js';
import { loadOverview } from './overview.js';

let viewSwitcher = () => {};
let refresher = () => {};

function buildCmdkItems() {
  const items = [
    // Navigation
    {
      group: '视图导航',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><rect x="2" y="2" width="5" height="5" rx="1.5" fill="currentColor"/><rect x="9" y="2" width="5" height="5" rx="1.5" fill="currentColor" opacity=".5"/><rect x="2" y="9" width="5" height="5" rx="1.5" fill="currentColor" opacity=".5"/><rect x="9" y="9" width="5" height="5" rx="1.5" fill="currentColor"/></svg>',
      label: '前往 总览视图 (Overview)',
      hint: '1',
      keywords: 'overview dashboard 总览 仪表盘 概览',
      run: () => viewSwitcher('overview')
    },
    {
      group: '视图导航',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M3 3.5h10M3 6.2h10M3 8.9h7M3 11.6h5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>',
      label: '前往 请求日志 (Logs)',
      hint: '2',
      keywords: 'logs request requests 请求 日志 记录',
      run: () => viewSwitcher('logs')
    },
    {
      group: '视图导航',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="2.2" fill="currentColor"/><path d="M8 1.6v2.4M8 12v2.4M1.6 8h2.4M12 8h2.4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>',
      label: '前往 网关配置 (Gateways)',
      hint: '3',
      keywords: 'gateways config proxy 配置 网关 代理',
      run: () => viewSwitcher('gateways')
    },
    {
      group: '视图导航',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M5.5 4 2.5 8l3 4M10.5 4l3 4-3 4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
      label: '前往 接入代码 (Setup & Snippets)',
      hint: '4',
      keywords: 'setup code snippets toml curl python 接入 代码',
      run: () => viewSwitcher('setup')
    },

    // Quick Actions
    {
      group: '快捷操作',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M2.7 8a5.3 5.3 0 0 1 9-3.8M13.3 8a5.3 5.3 0 0 1-9 3.8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/><path d="M11.9 1.7v2.7h-2.7M4.1 14.3v-2.7h2.7" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>',
      label: '刷新全部数据 (Refresh All)',
      hint: 'R',
      keywords: 'refresh reload 刷新 重载',
      run: () => refresher()
    },
    {
      group: '快捷操作',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M13.5 9.5A6 6 0 1 1 6.5 2.5a5 5 0 0 0 7 7z" stroke="currentColor" stroke-width="1.4"/></svg>',
      label: '切换 暗色 / 亮色 主题 (Toggle Theme)',
      hint: '',
      keywords: 'theme dark light mode 主题 切换 皮肤 亮色 暗色',
      run: () => toggleTheme()
    },
    {
      group: '快捷操作',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.4"/><path d="M8 7v4M8 5v.01" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
      label: '查看键盘快捷键帮助 (Shortcuts)',
      hint: '?',
      keywords: 'shortcuts help cheatsheet 帮助 快捷键',
      run: () => showShortcutsModal()
    },
    {
      group: '快捷操作',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M3 4h10M5.5 4V2.6h5V4M5 4l.4 9.4h5.2L11 4" stroke="#f43f5e" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>',
      label: '清空全部请求日志 (Clear Logs)',
      hint: '',
      keywords: 'clear delete truncate logs 清空 删除 清除',
      run: async () => {
        const ok = await confirmModal('确定要清空全部请求日志吗？此操作无法撤销。', '清空日志', true);
        if (!ok) return;
        try {
          const res = await api('/logs', { method: 'DELETE' });
          showToast(`已清空 ${res.deleted || 0} 条日志`, 'success');
          if (state.activeView === 'logs') loadLogs(true);
          if (state.activeView === 'overview') loadOverview();
        } catch (e) {
          showToast('清空失败: ' + e.message, 'error');
        }
      }
    }
  ];

  // Gateway Code Snippets
  gatewayIds().forEach(id => {
    const gw = state.gateways[id];
    if (!gw) return;
    items.push({
      group: '复制接入代码',
      icon: '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><rect x="5" y="5" width="8" height="8" rx="1.6" stroke="currentColor" stroke-width="1.3"/><path d="M3 11V3.6A1.6 1.6 0 0 1 4.6 2H11" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
      label: `复制 ${gw.name} 配置文件片段 (${gw.prefix})`,
      hint: '',
      keywords: `${gw.name} ${gw.prefix} toml snippet config`,
      run: async () => {
        try {
          const data = await api('/setup');
          if (data[id]) {
            await copyText(data[id]);
            showToast(`已复制 ${gw.name} 的 TOML 配置`, 'success');
          }
        } catch (e) {
          showToast('获取配置失败: ' + e.message, 'error');
        }
      }
    });
  });

  return items;
}

export function openCmdk() {
  state.cmdkItems = buildCmdkItems();
  state.cmdkSelected = 0;

  const input = $('#cmdkInput');
  if (input) input.value = '';

  renderCmdk('');

  const backdrop = $('#cmdkBackdrop');
  const cmdk = $('#cmdk');
  if (backdrop) backdrop.classList.add('is-open');
  if (cmdk) {
    cmdk.classList.add('is-open');
    cmdk.setAttribute('aria-hidden', 'false');
  }

  setTimeout(() => {
    if (input) input.focus();
  }, 60);
}

export function closeCmdk() {
  const backdrop = $('#cmdkBackdrop');
  const cmdk = $('#cmdk');
  if (backdrop) backdrop.classList.remove('is-open');
  if (cmdk) {
    cmdk.classList.remove('is-open');
    cmdk.setAttribute('aria-hidden', 'true');
  }
}

function renderCmdk(query) {
  const list = $('#cmdkList');
  if (!list) return;

  const q = query.trim().toLowerCase();
  const filtered = q
    ? state.cmdkItems.filter(it =>
        it.label.toLowerCase().includes(q) ||
        it.group.toLowerCase().includes(q) ||
        (it.keywords && it.keywords.toLowerCase().includes(q))
      )
    : state.cmdkItems;

  if (!filtered.length) {
    list.innerHTML = `
      <div class="cmdk-empty">
        <svg viewBox="0 0 16 16" width="24" height="24" fill="none" opacity=".4"><circle cx="7" cy="7" r="4.5" stroke="currentColor" stroke-width="1.5"/><path d="m10.5 10.5 3.5 3.5" stroke="currentColor" stroke-width="1.5"/></svg>
        <span>未找到与 “<strong>${escapeHtml(query)}</strong>” 相关的操作</span>
      </div>
    `;
    return;
  }

  if (state.cmdkSelected >= filtered.length) {
    state.cmdkSelected = filtered.length - 1;
  }

  let lastGroup = '';
  let html = '';

  filtered.forEach((it, i) => {
    if (it.group !== lastGroup) {
      html += `<div class="cmdk-group">${escapeHtml(it.group)}</div>`;
      lastGroup = it.group;
    }
    const isActive = i === state.cmdkSelected ? ' is-active' : '';
    html += `
      <div class="cmdk-item${isActive}" data-index="${i}" role="option" aria-selected="${i === state.cmdkSelected}">
        <span class="cmdk-item-icon">${it.icon}</span>
        <span class="cmdk-item-label">${escapeHtml(it.label)}</span>
        ${it.hint ? `<kbd class="cmdk-kbd">${escapeHtml(it.hint)}</kbd>` : ''}
      </div>
    `;
  });

  list.innerHTML = html;

  $all('.cmdk-item', list).forEach(el => {
    el.addEventListener('click', () => {
      const idx = Number(el.dataset.index);
      const item = filtered[idx];
      if (item) {
        closeCmdk();
        item.run();
      }
    });

    el.addEventListener('mouseenter', () => {
      state.cmdkSelected = Number(el.dataset.index);
      $all('.cmdk-item', list).forEach(x => {
        const act = x === el;
        x.classList.toggle('is-active', act);
        x.setAttribute('aria-selected', act);
      });
    });
  });

  // Ensure active element is scrolled into view
  const activeEl = list.querySelector('.cmdk-item.is-active');
  if (activeEl) {
    activeEl.scrollIntoView({ block: 'nearest' });
  }
}

export function initCmdk({ switchView, refreshAll }) {
  viewSwitcher = switchView;
  refresher = refreshAll;

  const cmdBtn = $('#cmdKBtn');
  if (cmdBtn) cmdBtn.addEventListener('click', openCmdk);

  const backdrop = $('#cmdkBackdrop');
  if (backdrop) backdrop.addEventListener('click', closeCmdk);

  const input = $('#cmdkInput');
  if (input) {
    input.addEventListener('input', (e) => {
      state.cmdkSelected = 0;
      renderCmdk(e.target.value);
    });

    input.addEventListener('keydown', (e) => {
      const q = e.target.value.trim().toLowerCase();
      const filtered = q
        ? state.cmdkItems.filter(it =>
            it.label.toLowerCase().includes(q) ||
            it.group.toLowerCase().includes(q) ||
            (it.keywords && it.keywords.toLowerCase().includes(q))
          )
        : state.cmdkItems;

      if (!filtered.length) return;

      if (e.key === 'ArrowDown') {
        e.preventDefault();
        state.cmdkSelected = (state.cmdkSelected + 1) % filtered.length;
        renderCmdk(e.target.value);
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        state.cmdkSelected = (state.cmdkSelected - 1 + filtered.length) % filtered.length;
        renderCmdk(e.target.value);
      } else if (e.key === 'Enter') {
        e.preventDefault();
        const item = filtered[state.cmdkSelected];
        if (item) {
          closeCmdk();
          item.run();
        }
      }
    });
  }
}
