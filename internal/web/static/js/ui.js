'use strict';

import { $, escapeHtml } from './utils.js';

/* ---------------- Toast 提示栈 ---------------- */

const TOAST_ICONS = {
  success: `<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.5"/><path d="m5 8 2 2 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>`,
  error: `<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.5"/><path d="m5.5 5.5 5 5M10.5 5.5l-5 5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>`,
  warning: `<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M8 2.2 1.5 13.5h13L8 2.2z" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/><path d="M8 6.5v3M8 11.5v.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>`,
  info: `<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.5"/><path d="M8 7v4M8 5v.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>`
};

export function showToast(message, type = 'info', duration = 3000) {
  const stack = $('#toastStack');
  if (!stack) return;

  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  const icon = TOAST_ICONS[type] || TOAST_ICONS.info;
  toast.innerHTML = `
    <span class="toast-icon">${icon}</span>
    <span class="toast-msg">${escapeHtml(message)}</span>
    <button type="button" class="toast-close" aria-label="关闭">&times;</button>
  `;

  const close = () => {
    toast.classList.add('is-closing');
    setTimeout(() => {
      if (toast.parentNode) toast.parentNode.removeChild(toast);
    }, 200);
  };

  const closeBtn = toast.querySelector('.toast-close');
  if (closeBtn) closeBtn.addEventListener('click', close);

  stack.appendChild(toast);
  if (duration > 0) {
    setTimeout(close, duration);
  }
}

/* ---------------- 确认对话框 ---------------- */

export function confirmModal(title, message, onConfirm) {
  let modal = $('#confirmModal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'confirmModal';
    modal.className = 'modal-backdrop';
    modal.innerHTML = `
      <div class="modal" role="dialog" aria-modal="true">
        <h3 class="modal-title" id="confirmModalTitle"></h3>
        <p class="modal-body" id="confirmModalMsg"></p>
        <div class="modal-actions">
          <button type="button" id="confirmCancelBtn" class="btn-ghost">取消</button>
          <button type="button" id="confirmOkBtn" class="btn-danger">确认</button>
        </div>
      </div>
    `;
    document.body.appendChild(modal);
  }

  $('#confirmModalTitle').textContent = title;
  $('#confirmModalMsg').textContent = message;
  modal.classList.add('is-open');

  const close = () => {
    modal.classList.remove('is-open');
  };

  const okBtn = $('#confirmOkBtn');
  const cancelBtn = $('#confirmCancelBtn');

  const handleOk = async () => {
    close();
    if (onConfirm) await onConfirm();
  };

  okBtn.onclick = handleOk;
  cancelBtn.onclick = close;
  modal.onclick = (e) => {
    if (e.target === modal) close();
  };
}

/* ---------------- 主题切换 ---------------- */

export function initTheme() {
  const saved = localStorage.getItem('grok_theme') || 'dark';
  document.documentElement.setAttribute('data-theme', saved);

  const toggleBtn = $('#themeToggleBtn');
  if (toggleBtn) {
    toggleBtn.addEventListener('click', () => {
      const current = document.documentElement.getAttribute('data-theme') || 'dark';
      const next = current === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      localStorage.setItem('grok_theme', next);
      showToast(`已切换至${next === 'dark' ? '深色' : '浅色'}模式`, 'info', 1200);
    });
  }
}

/* ---------------- 快捷键帮助弹窗 ---------------- */

export function showShortcutsModal() {
  let modal = $('#shortcutsModal');
  if (!modal) {
    modal = document.createElement('div');
    modal.id = 'shortcutsModal';
    modal.className = 'modal-backdrop';
    modal.innerHTML = `
      <div class="modal shortcuts-modal" role="dialog" aria-modal="true">
        <div class="modal-head">
          <h3>键盘快捷键速查</h3>
          <button type="button" class="btn-icon" id="shortcutsCloseBtn">&times;</button>
        </div>
        <div class="shortcuts-list">
          <div class="shortcut-row"><span>切换至 运行总览</span><kbd>1</kbd></div>
          <div class="shortcut-row"><span>切换至 请求日志</span><kbd>2</kbd></div>
          <div class="shortcut-row"><span>切换至 网关配置</span><kbd>3</kbd></div>
          <div class="shortcut-row"><span>切换至 接入代码</span><kbd>4</kbd></div>
          <div class="shortcut-row"><span>刷新全部数据与连通状态</span><kbd>R</kbd></div>
          <div class="shortcut-row"><span>抽屉 上一条 / 下一条请求</span><kbd>J / K 或 [ / ]</kbd></div>
          <div class="shortcut-row"><span>关闭详情抽屉或弹窗</span><kbd>Esc</kbd></div>
          <div class="shortcut-row"><span>查看本快捷键速查</span><kbd>?</kbd></div>
        </div>
      </div>
    `;
    document.body.appendChild(modal);

    const closeBtn = $('#shortcutsCloseBtn', modal);
    if (closeBtn) closeBtn.onclick = () => modal.classList.remove('is-open');
    modal.onclick = (e) => {
      if (e.target === modal) modal.classList.remove('is-open');
    };
  }

  modal.classList.add('is-open');
}
