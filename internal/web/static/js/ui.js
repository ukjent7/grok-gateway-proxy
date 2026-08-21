'use strict';

/* ============================================================
   Grok Gateway Console · UI 交互与反馈组件
   - Toast 提示
   - 模态确认对话框
   - 快捷键帮助抽屉/模态框
   - 主题切换器 (Dark / Light)
   ============================================================ */

import { $, escapeHtml } from './utils.js';
import { state } from './state.js';

/* ---------------- 1. Toast ---------------- */

const toastIcons = {
  success: '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="rgba(16,185,129,0.2)" stroke="#10b981" stroke-width="1.2"/><path d="m5 8 2 2 4-4" stroke="#10b981" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  error: '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="rgba(244,63,94,0.2)" stroke="#f43f5e" stroke-width="1.2"/><path d="m5.5 5.5 5 5M10.5 5.5l-5 5" stroke="#f43f5e" stroke-width="1.6" stroke-linecap="round"/></svg>',
  warning: '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="rgba(245,158,11,0.2)" stroke="#f59e0b" stroke-width="1.2"/><path d="M8 5v3.5M8 11v.01" stroke="#f59e0b" stroke-width="1.6" stroke-linecap="round"/></svg>',
  info: '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="rgba(6,182,212,0.2)" stroke="#06b6d4" stroke-width="1.2"/><path d="M8 7v4M8 5v.01" stroke="#06b6d4" stroke-width="1.6" stroke-linecap="round"/></svg>'
};

export function showToast(message, kind = 'info', duration = 3000) {
  const stack = $('#toastStack');
  if (!stack) return;

  const t = document.createElement('div');
  t.className = 'toast toast-' + kind;
  t.setAttribute('role', 'alert');
  t.innerHTML = `
    <span class="toast-icon">${toastIcons[kind] || toastIcons.info}</span>
    <span class="toast-msg">${escapeHtml(message)}</span>
    <button type="button" class="toast-close" aria-label="关闭提示">
      <svg viewBox="0 0 16 16" width="11" height="11" fill="none"><path d="m4 4 8 8M12 4l-8 8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
    </button>
  `;

  const closeBtn = t.querySelector('.toast-close');
  closeBtn.addEventListener('click', () => removeToast(t));

  stack.appendChild(t);
  requestAnimationFrame(() => t.classList.add('is-show'));

  const timer = setTimeout(() => removeToast(t), duration);
  t._timer = timer;
}

function removeToast(t) {
  if (t._timer) clearTimeout(t._timer);
  t.classList.remove('is-show');
  t.classList.add('is-hiding');
  setTimeout(() => {
    if (t.parentNode) t.remove();
  }, 260);
}

/* ---------------- 2. 模态框 ---------------- */

let activeModal = null;

export function closeModal() {
  if (!activeModal) return;
  const { root, backdrop, onKey, restoreFocus } = activeModal;
  activeModal = null;
  if (onKey) document.removeEventListener('keydown', onKey, true);
  backdrop.classList.remove('is-open');
  root.classList.remove('is-open');
  setTimeout(() => {
    backdrop.remove();
    root.remove();
  }, 200);
  if (restoreFocus) {
    try { restoreFocus(); } catch (e) { /* noop */ }
  }
}

export function showModal({
  title,
  message = '',
  confirmLabel = '确定',
  cancelLabel = '取消',
  isDanger = false,
  customBodyHTML = ''
}) {
  return new Promise(resolve => {
    closeModal();
    const prevFocus = document.activeElement;

    const backdrop = document.createElement('div');
    backdrop.className = 'modal-backdrop';
    const root = document.createElement('div');
    root.className = 'modal';
    root.setAttribute('role', 'dialog');
    root.setAttribute('aria-modal', 'true');
    root.setAttribute('aria-label', title);

    root.innerHTML = `
      <div class="modal-head">
        <div class="modal-head-title">
          ${isDanger ? '<span class="modal-danger-icon"><svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M8 2.5 1.5 13.5h13L8 2.5z" stroke="#f43f5e" stroke-width="1.4"/><path d="M8 6.5v3.5M8 11.5v.01" stroke="#f43f5e" stroke-width="1.6" stroke-linecap="round"/></svg></span>' : ''}
          <strong>${escapeHtml(title)}</strong>
        </div>
        <button type="button" class="btn-icon modal-close" aria-label="关闭">
          <svg viewBox="0 0 16 16" width="13" height="13" fill="none"><path d="m4 4 8 8M12 4l-8 8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
        </button>
      </div>
      ${message ? `<p class="modal-msg">${escapeHtml(message)}</p>` : ''}
      ${customBodyHTML ? `<div class="modal-custom">${customBodyHTML}</div>` : ''}
      <div class="modal-actions">
        <button type="button" class="btn-ghost modal-cancel">${escapeHtml(cancelLabel)}</button>
        <button type="button" class="${isDanger ? 'btn-danger' : 'btn-primary'} modal-confirm">${escapeHtml(confirmLabel)}</button>
      </div>
    `;

    let finished = false;
    const finish = (value) => {
      if (finished) return;
      finished = true;
      closeModal();
      resolve(value);
    };

    const onKey = (e) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        finish(null);
      }
    };

    activeModal = {
      root, backdrop, onKey,
      restoreFocus: prevFocus && typeof prevFocus.focus === 'function' ? prevFocus.focus.bind(prevFocus) : null
    };

    document.addEventListener('keydown', onKey, true);
    backdrop.addEventListener('click', () => finish(null));
    root.querySelector('.modal-cancel').addEventListener('click', () => finish(null));
    root.querySelector('.modal-close').addEventListener('click', () => finish(null));
    root.querySelector('.modal-confirm').addEventListener('click', () => finish('ok'));

    document.body.appendChild(backdrop);
    document.body.appendChild(root);

    requestAnimationFrame(() => {
      backdrop.classList.add('is-open');
      root.classList.add('is-open');
      const confirmBtn = root.querySelector('.modal-confirm');
      if (confirmBtn) confirmBtn.focus();
    });
  });
}

export function confirmModal(message, confirmLabel, isDanger = true) {
  return showModal({
    title: '确认操作',
    message: message || '',
    confirmLabel: confirmLabel || '确定',
    isDanger: isDanger
  }).then(v => v === 'ok');
}

/* ---------------- 3. 快捷键指南 ---------------- */

export function showShortcutsModal() {
  const shortcuts = [
    { key: '1', desc: '切换到 总览视图' },
    { key: '2', desc: '切换到 请求日志' },
    { key: '3', desc: '切换到 网关配置' },
    { key: '4', desc: '切换到 接入代码' },
    { key: '⌘ K / Ctrl K', desc: '打开全局命令面板' },
    { key: 'R', desc: '刷新当前页面数据' },
    { key: 'J / K 或 [ / ]', desc: '详情抽屉中浏览 上一条 / 下一条 日志' },
    { key: 'Esc', desc: '关闭当前弹窗 / 抽屉 / 面板' },
    { key: '?', desc: '查看键盘快捷键帮助' }
  ];

  const rows = shortcuts.map(s => `
    <div class="shortcut-row">
      <span class="shortcut-desc">${escapeHtml(s.desc)}</span>
      <kbd class="shortcut-kbd">${escapeHtml(s.key)}</kbd>
    </div>
  `).join('');

  showModal({
    title: '键盘快捷键指南',
    confirmLabel: '知道了',
    cancelLabel: '关闭',
    customBodyHTML: `<div class="shortcuts-grid">${rows}</div>`
  });
}

/* ---------------- 4. 主题切换 ---------------- */

export function applyTheme(theme) {
  state.theme = theme;
  localStorage.setItem('grok_theme', theme);
  document.documentElement.setAttribute('data-theme', theme);

  const themeBtn = $('#themeToggleBtn');
  if (themeBtn) {
    const isDark = theme === 'dark';
    themeBtn.setAttribute('title', isDark ? '切换至亮色模式' : '切换至暗色模式');
    themeBtn.setAttribute('aria-label', isDark ? '切换至亮色模式' : '切换至暗色模式');
    themeBtn.innerHTML = isDark
      ? '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><circle cx="8" cy="8" r="3.2" stroke="currentColor" stroke-width="1.4"/><path d="M8 1.5v1.8M8 12.7v1.8M1.5 8h1.8M12.7 8h1.8M3.4 3.4l1.3 1.3M11.3 11.3l1.3 1.3M3.4 12.6l1.3-1.3M11.3 4.7l1.3-1.3" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>'
      : '<svg viewBox="0 0 16 16" width="14" height="14" fill="none"><path d="M13.5 9.5A6 6 0 1 1 6.5 2.5a5 5 0 0 0 7 7z" stroke="currentColor" stroke-width="1.4" fill="currentColor" fill-opacity="0.12"/></svg>';
  }
}

export function toggleTheme() {
  const next = state.theme === 'dark' ? 'light' : 'dark';
  applyTheme(next);
  showToast(next === 'dark' ? '已切换至暗色模式' : '已切换至亮色模式', 'info', 1800);
}

export function initTheme() {
  applyTheme(state.theme);
  const themeBtn = $('#themeToggleBtn');
  if (themeBtn) {
    themeBtn.addEventListener('click', toggleTheme);
  }
}
