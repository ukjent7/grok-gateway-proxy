'use strict';

import { $, escapeHtml } from './utils.js';

/* ---------------- Toast ---------------- */

const toastIcons = {
  success: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M3.5 8.5l3 3 6-6" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  error: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/></svg>',
  info: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M8 5.5v6M8 3.5v.01" stroke="currentColor" stroke-width="1.8" fill="none" stroke-linecap="round"/></svg>'
};

export function showToast(message, kind) {
  const stack = $('#toastStack');
  if (!stack) return;
  const t = document.createElement('div');
  t.className = 'toast' + (kind === 'error' ? ' is-error' : '');
  t.innerHTML = '<span class="toast-icon">' + (toastIcons[kind] || toastIcons.info) + '</span><span>' + escapeHtml(message) + '</span>';
  stack.appendChild(t);
  requestAnimationFrame(() => t.classList.add('is-show'));
  setTimeout(() => {
    t.classList.remove('is-show');
    setTimeout(() => { if (t.parentNode) t.remove(); }, 300);
  }, 2800);
}

/* ---------------- 模态框 ----------------
   通用 Promise 式确认模态框：resolve 'ok'（确定）或 null（取消）。
   打开时焦点移入对话框，Esc / 背景点击 / 取消均 resolve(null)，关闭后还原焦点。 */

let activeModal = null;

function closeModal() {
  if (!activeModal) return;
  const { root, backdrop, onKey, restoreFocus } = activeModal;
  activeModal = null;
  if (onKey) document.removeEventListener('keydown', onKey, true);
  backdrop.remove();
  root.remove();
  if (restoreFocus) {
    try { restoreFocus(); } catch (e) { /* noop */ }
  }
}

function showModal({ title, message = '', confirmLabel = '确定', cancelLabel = '取消' }) {
  return new Promise(resolve => {
    closeModal();
    const prevFocus = document.activeElement;

    const backdrop = document.createElement('div');
    backdrop.className = 'modal-backdrop is-open';
    const root = document.createElement('div');
    root.className = 'modal';
    root.setAttribute('role', 'dialog');
    root.setAttribute('aria-modal', 'true');
    root.setAttribute('aria-label', title);
    root.innerHTML =
      '<div class="modal-head"><strong>' + escapeHtml(title) + '</strong>' +
      '<button type="button" class="btn-icon modal-close" aria-label="关闭">' +
        '<svg viewBox="0 0 16 16" width="13" height="13" fill="none" aria-hidden="true"><path d="m4 4 8 8M12 4l-8 8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
      '</button></div>' +
      (message ? '<p class="modal-msg">' + escapeHtml(message) + '</p>' : '') +
      '<div class="modal-actions">' +
        '<button type="button" class="btn-ghost modal-cancel">' + escapeHtml(cancelLabel) + '</button>' +
        '<button type="button" class="btn-primary modal-confirm">' + escapeHtml(confirmLabel) + '</button>' +
      '</div>';

    let finished = false;
    const finish = (value) => {
      if (finished) return;
      finished = true;
      closeModal();
      resolve(value);
    };
    // capture 阶段拦截 Esc，避免穿透到 app 层的全局 Esc 快捷键
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); finish(null); } };

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

    root.querySelector('.modal-confirm').focus();
  });
}

// 确认框：resolve Promise<boolean>
export function confirmModal(message, confirmLabel) {
  return showModal({
    title: '确认操作',
    message: message || '',
    confirmLabel: confirmLabel || '确定'
  }).then(v => v === 'ok');
}
