'use strict';

/* ============================================================
   Grok Gateway Console · 核心应用入口
   - 视图路由与状态切换
   - 自动轮询与能效优化
   - 全局键盘快捷键调度
   ============================================================ */

import { state, loadConfig } from './state.js';
import { $, $all, copyText } from './utils.js';
import { showToast, initTheme, showShortcutsModal } from './ui.js';
import { loadOverview, initOverview } from './overview.js';
import { initLogs, loadLogs } from './logs.js';
import { initGateways, renderGatewayCards } from './gateways.js';
import { initSetup, loadSetup } from './setup.js';
import { initDrawer, closeDrawer, navigateDrawer } from './drawer.js';
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

  closeMobileSidebar();
}

$all('.rail-nav-item').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
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

const addrChip = $('.addr-chip');
if (addrChip) {
  addrChip.addEventListener('click', async () => {
    const raw = state.listenAddr || '127.0.0.1:8787';
    const text = raw.startsWith('http://') || raw.startsWith('https://') ? raw : `http://${raw}`;
    await copyText(text);
    showToast(`已复制监听端点: ${text}`, 'success', 1500);
  });
}

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

/* ---------------- 3. 实时刷新（SSE 推送） ---------------- */

// 数据/配置一变，服务端就从 /api/events 推一个 change 事件过来；
// 这里做 250ms 收尾防抖，把一次请求风暴压成一次刷新。
// EventSource 断线自动重连，后台标签页也照常收到事件。
let changeDebounce = null;

function onServerChange() {
  clearTimeout(changeDebounce);
  changeDebounce = setTimeout(async () => {
    try {
      await loadConfig();
    } catch (_) { /* 配置读取失败不影响其余刷新 */ }
    if (state.activeView === 'overview') loadOverview();
    if (state.activeView === 'logs') loadLogs({ silent: true });
    if (state.activeView === 'gateways' && !editingGatewayCards()) renderGatewayCards();
    if (state.activeView === 'setup') loadSetup();
    pollHealth();
  }, 250);
}

// 用户正在网关卡片里输入时不要重绘，避免输入被清掉。
function editingGatewayCards() {
  const grid = document.getElementById('gatewayCards');
  const active = document.activeElement;
  return Boolean(grid && active && grid.contains(active));
}

function connectRealtime() {
  const source = new EventSource('/api/events');
  source.addEventListener('change', onServerChange);
  // 网关健康状态由服务端后台探测，变化不一定伴随日志写入，
  // 保留一个轻量健康轮询兜底。
  setInterval(() => {
    if (!document.hidden) pollHealth();
  }, 30000);
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  pollHealth();
});

/* ---------------- 4. 键盘全局快捷键 ---------------- */

document.addEventListener('keydown', (e) => {
  // Escape -> Close drawer / modals
  if (e.key === 'Escape') {
    const drawer = $('#logDrawer');
    if (drawer && drawer.classList.contains('is-open')) {
      closeDrawer();
    }
    return;
  }

  const tag = (e.target.tagName || '').toLowerCase();
  const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;
  if (inField) return;

  if (e.metaKey || e.ctrlKey || e.altKey) return;

  // Views switching
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

  try {
    await loadConfig();
    await loadOverview();
  } catch (e) {
    showToast('代理控制台初始化异常: ' + e.message, 'error');
  }

  pollHealth();
  connectRealtime();
}

// Initialize modules
initOverview();
initLogs();
initGateways();
initSetup();
initDrawer();

boot();
