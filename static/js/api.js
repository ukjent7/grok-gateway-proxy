'use strict';

import { $ } from './utils.js';
import { showToast, promptToken } from './ui.js';

const TOKEN_KEY = 'grok_gateway_token';

export function savedToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ''; } catch (e) { return ''; }
}
export function saveToken(token) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch (e) { /* private mode */ }
}

export async function api(path, opts, _retried) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, (opts && opts.headers) || {});
  const token = savedToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const res = await fetch('/api' + path, Object.assign({}, opts, { headers }));
  let body = null;
  try { body = await res.json(); } catch (e) { /* no body */ }
  if (res.status === 401 && !_retried && !(opts && opts.retry401)) {
    const entered = await promptToken(
      '此代理的管理接口需要访问令牌。',
      '请输入管理接口访问令牌（留空则不再提示）',
      token || ''
    );
    if (entered && entered.trim()) {
      saveToken(entered.trim());
      updateTokenButton();
      return api(path, opts, true);
    }
    if (entered === null) {
      throw new Error('未提供访问令牌');
    }
  }
  if (!res.ok) {
    const msg = (body && body.error && body.error.message) || res.statusText || ('HTTP ' + res.status);
    throw new Error(msg);
  }
  return body;
}

export function updateTokenButton() {
  const btn = $('#tokenBtn');
  if (!btn) return;
  const has = savedToken();
  btn.classList.toggle('is-set', !!has);
  btn.title = has ? '已设置访问令牌，点击清除' : '设置管理接口访问令牌';
  const label = btn.querySelector('.token-label');
  if (label) label.textContent = has ? '令牌已设' : '设置令牌';
}

export function initTokenButton(refreshAll) {
  $('#tokenBtn').addEventListener('click', async () => {
    if (savedToken()) {
      saveToken('');
      updateTokenButton();
      showToast('已清除访问令牌', 'success');
      return;
    }
    const entered = await promptToken(
      '管理接口访问令牌',
      '输入管理接口访问令牌（需与 config.json 中的 api_token 一致）',
      ''
    );
    if (entered && entered.trim()) {
      saveToken(entered.trim());
      updateTokenButton();
      showToast('访问令牌已保存', 'success');
      refreshAll();
    }
  });
}
