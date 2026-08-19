'use strict';

/* ============================================================
   Grok Gateway Console · 入口
   组装各视图模块：state / api / ui / overview / pulse / logs /
   gateways / setup / drawer / cmdk / health。
   ============================================================ */

import { state, loadConfig } from './state.js';
import { initTokenButton, updateTokenButton } from './api.js';
import { $, $all } from './utils.js';
import { showToast } from './ui.js';
import { loadOverview, initOverview } from './overview.js';
import { initLogs, loadLogs } from './logs.js';
import { initGateways, renderGatewayCards } from './gateways.js';
import { initSetup, loadSetup } from './setup.js';
import { initDrawer, closeDrawer } from './drawer.js';
import { initCmdk, openCmdk, closeCmdk } from './cmdk.js';
import { pollHealth } from './health.js';

/* ---------------- 视图切换 ---------------- */

function switchView(view) {
  state.activeView = view;
  $all('.view').forEach(el => el.classList.toggle('is-active', el.id === 'view-' + view));
  $all('.rail-nav-item').forEach(el => el.classList.toggle('is-active', el.dataset.view === view));
  if (view === 'logs') loadLogs(true);
  if (view === 'gateways') renderGatewayCards();
  if (view === 'setup') loadSetup();
  closeCmdk();
}

$all('.rail-nav-item').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
});
$all('[data-goto]').forEach(btn => {
  btn.addEventListener('click', () => switchView(btn.dataset.goto));
});

async function refreshAll() {
  await loadConfig();
  if (state.activeView === 'overview') loadOverview();
  if (state.activeView === 'logs') loadLogs(true);
  if (state.activeView === 'gateways') renderGatewayCards();
  if (state.activeView === 'setup') loadSetup();
  showToast('已刷新', 'success');
}
$('#refreshAllBtn').addEventListener('click', refreshAll);

/* ---------------- 轮询 ---------------- */
// 页面在后台时暂停轮询，回到前台立即刷新一次。
function startPolling() {
  state.pollTimer = setInterval(() => {
    if (document.hidden) return;
    if (state.activeView === 'overview') loadOverview();
  }, 15000);
  setInterval(() => {
    if (document.hidden) return;
    pollHealth();
  }, 30000);
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  if (state.activeView === 'overview') loadOverview();
  pollHealth();
});

/* ---------------- 键盘快捷键 ---------------- */
document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault();
    if ($('#cmdk').classList.contains('is-open')) closeCmdk(); else openCmdk();
    return;
  }
  if (e.key === 'Escape') {
    if ($('#cmdk').classList.contains('is-open')) { closeCmdk(); return; }
    if ($('#logDrawer').classList.contains('is-open')) { closeDrawer(); return; }
  }
  const tag = (e.target.tagName || '').toLowerCase();
  const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;
  if (inField) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === '1') switchView('overview');
  else if (e.key === '2') switchView('logs');
  else if (e.key === '3') switchView('gateways');
  else if (e.key === '4') switchView('setup');
  else if (e.key === 'r' || e.key === 'R') refreshAll();
});

/* ---------------- 启动 ---------------- */
async function boot() {
  updateTokenButton();
  try {
    await loadConfig();
    await loadOverview();
  } catch (e) {
    showToast('初始化失败: ' + e.message, 'error');
  }
  pollHealth();
  startPolling();
}

initTokenButton(refreshAll);
initOverview();
initLogs();
initGateways();
initSetup();
initDrawer();
initCmdk({ switchView, refreshAll });
boot();
