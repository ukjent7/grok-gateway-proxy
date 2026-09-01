'use strict';

import { $, escapeHtml } from './utils.js';

/* ---------------- Toast 提示栈 ---------------- */

export function showToast(message, type = 'info', duration = 3000) {
  const stack = $('#toastStack');
  if (!stack) return;

  const toast = document.createElement('div');
  toast.className = `toast toast-${type}`;
  toast.innerHTML = `
    <span class="toast-icon"></span>
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
          <h3>键盘快捷键帮助</h3>
          <button type="button" class="btn-icon" id="shortcutsCloseBtn">&times;</button>
        </div>
        <div class="shortcuts-list">
          <div class="shortcut-row"><span>切换至运行总览</span><kbd>1</kbd></div>
          <div class="shortcut-row"><span>切换至请求日志</span><kbd>2</kbd></div>
          <div class="shortcut-row"><span>切换至网关配置</span><kbd>3</kbd></div>
          <div class="shortcut-row"><span>切换至接入代码</span><kbd>4</kbd></div>
          <div class="shortcut-row"><span>打开命令面板</span><kbd>⌘ K / Ctrl+K</kbd></div>
          <div class="shortcut-row"><span>刷新全部数据</span><kbd>R</kbd></div>
          <div class="shortcut-row"><span>抽屉上一条/下一条</span><kbd>J / K 或 [ / ]</kbd></div>
          <div class="shortcut-row"><span>关闭抽屉 / 弹窗</span><kbd>Esc</kbd></div>
          <div class="shortcut-row"><span>查看本快捷键帮助</span><kbd>?</kbd></div>
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
