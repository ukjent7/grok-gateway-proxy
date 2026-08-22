'use strict';

/* ============================================================
   Grok Gateway Console · 核心应用入口
   - 视图路由与状态切换
   - 自动轮询与能效优化
   - 全局键盘快捷键调度
   ============================================================ */

import { state, loadConfig } from './state.js';
import { $, $all } from './utils.js';
import { showToast, initTheme, showShortcutsModal } from './ui.js';
import { loadOverview, initOverview } from './overview.js';
import { initLogs, loadLogs } from './logs.js';
import { initGateways, renderGatewayCards } from './gateways.js';
import { initSetup, loadSetup } from './setup.js';
import { initDrawer, closeDrawer, navigateDrawer } from './drawer.js';
import { initCmdk, openCmdk, closeCmdk } from './cmdk.js';
import { pollHealth } from './health.js';

/* ---------------- 1. 视图切换 ---------------- */

export function switchView(view) {
  state.activeView = view;
  $all('.view').forEach(el => el.classList.toggle('is-active', el.id === 'view-' + view));
  $all('.rail-nav-item').forEach(el => {
    const act = el.dataset.view === view;
    el.classList.toggle('is-active', act);
    el.setAttribute('aria-selected', act ? 'true' : 'false');
  });

  if (view === 'overview') loadOverview();
  if (view === 'logs') loadLogs(true);
  if (view === 'gateways') renderGatewayCards();
  if (view === 'setup') loadSetup();

  closeCmdk();
  closeMobileSidebar();
}

$all('.rail-nav-item').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
});

$all('[data-goto]').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.goto));
});

export async function refreshAll() {
  const refreshIcon = $('#refreshAllBtn svg');
  if (refreshIcon) refreshIcon.classList.add('is-spinning');

  try {
    await loadConfig();
    if (state.activeView === 'overview') await loadOverview();
    if (state.activeView === 'logs') await loadLogs(true);
    if (state.activeView === 'gateways') renderGatewayCards();
    if (state.activeView === 'setup') await loadSetup();
    await pollHealth();
    showToast('数据已全面刷新', 'success', 1600);
  } catch (e) {
    showToast('刷新出错: ' + e.message, 'error');
  } finally {
    if (refreshIcon) {
      setTimeout(() => refreshIcon.classList.remove('is-spinning'), 400);
    }
  }
}

const refreshBtn = $('#refreshAllBtn');
if (refreshBtn) refreshBtn.addEventListener('click', refreshAll);

/* ---------------- 2. 移动端侧栏 ---------------- */

function toggleMobileSidebar() {
  const rail = $('.rail');
  const overlay = $('#mobileRailOverlay');
  if (rail && overlay) {
    const isOpen = rail.classList.toggle('is-open');
    overlay.classList.toggle('is-open', isOpen);
  }
}

function closeMobileSidebar() {
  const rail = $('.rail');
  const overlay = $('#mobileRailOverlay');
  if (rail) rail.classList.remove('is-open');
  if (overlay) overlay.classList.remove('is-open');
}

const mobileMenuBtn = $('#mobileMenuBtn');
if (mobileMenuBtn) mobileMenuBtn.addEventListener('click', toggleMobileSidebar);

const mobileOverlay = $('#mobileRailOverlay');
if (mobileOverlay) mobileOverlay.addEventListener('click', closeMobileSidebar);

/* ---------------- 3. 自动轮询调度 ---------------- */

function restartAutoRefresh() {
  if (state.autoRefreshTimer) {
    clearInterval(state.autoRefreshTimer);
    state.autoRefreshTimer = null;
  }

  const intervalSec = state.autoRefresh;
  if (intervalSec <= 0) return;

  state.autoRefreshTimer = setInterval(() => {
    if (document.hidden) return;
    if (state.activeView === 'overview') loadOverview();
    if (state.activeView === 'logs') loadLogs({ silent: true });
    pollHealth();
  }, intervalSec * 1000);
}

function initAutoRefreshSelector() {
  const sel = $('#autoRefreshSelect');
  if (sel) {
    sel.value = String(state.autoRefresh);
    sel.addEventListener('change', () => {
      state.autoRefresh = Number(sel.value);
      localStorage.setItem('grok_auto_refresh', String(state.autoRefresh));
      restartAutoRefresh();
      showToast(state.autoRefresh > 0 ? `已开启自动刷新 (${state.autoRefresh}s)` : '已关闭自动刷新', 'info', 1600);
    });
  }
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  if (state.activeView === 'overview') loadOverview();
  pollHealth();
});

/* ---------------- 4. 键盘全局快捷键 ---------------- */

document.addEventListener('keydown', (e) => {
  // 1. Cmd/Ctrl + K -> Command Palette
  if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault();
    const cmdk = $('#cmdk');
    if (cmdk && cmdk.classList.contains('is-open')) closeCmdk();
    else openCmdk();
    return;
  }

  // 2. Escape -> Close drawer / modals / cmdk
  if (e.key === 'Escape') {
    const cmdk = $('#cmdk');
    if (cmdk && cmdk.classList.contains('is-open')) {
      closeCmdk();
      return;
    }
    const drawer = $('#logDrawer');
    if (drawer && drawer.classList.contains('is-open')) {
      closeDrawer();
      return;
    }
    return;
  }

  const tag = (e.target.tagName || '').toLowerCase();
  const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;
  if (inField) return;

  if (e.metaKey || e.ctrlKey || e.altKey) return;

  // 3. Views switching
  if (e.key === '1') switchView('overview');
  else if (e.key === '2') switchView('logs');
  else if (e.key === '3') switchView('gateways');
  else if (e.key === '4') switchView('setup');
  else if (e.key === 'r' || e.key === 'R') refreshAll();
  else if (e.key === '?' || (e.shiftKey && e.key === '/')) {
    e.preventDefault();
    showShortcutsModal();
  }
  else if (e.key === 'j' || e.key === 'J' || e.key === ']') {
    const drawer = $('#logDrawer');
    if (drawer && drawer.classList.contains('is-open')) {
      navigateDrawer(1);
    }
  }
  else if (e.key === 'k' || e.key === 'K' || e.key === '[') {
    const drawer = $('#logDrawer');
    if (drawer && drawer.classList.contains('is-open')) {
      navigateDrawer(-1);
    }
  }
});

// Shortcuts cheatsheet button in sidebar
const shortcutsHelpBtn = $('#shortcutsHelpBtn');
if (shortcutsHelpBtn) {
  shortcutsHelpBtn.addEventListener('click', showShortcutsModal);
}

/* ---------------- 5. 启动自检 ---------------- */

async function boot() {
  initTheme();
  initAutoRefreshSelector();

  try {
    await loadConfig();
    await loadOverview();
  } catch (e) {
    showToast('代理控制台初始化异常: ' + e.message, 'error');
  }

  pollHealth();
  restartAutoRefresh();
}

// Global debug access
window.grokConsole = {
  state,
  switchView,
  refreshAll,
  loadLogs,
  loadOverview
};

// Initialize modules
initOverview();
initLogs();
initGateways();
initSetup();
initDrawer();
initCmdk({ switchView, refreshAll });

boot();
