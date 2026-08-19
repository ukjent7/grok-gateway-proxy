'use strict';

// 本地小工具：无需鉴权，管理 API 直接开放（仅监听 127.0.0.1）

export async function api(path, opts) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, (opts && opts.headers) || {});
  const res = await fetch('/api' + path, Object.assign({}, opts, { headers }));
  let body = null;
  try { body = await res.json(); } catch (e) { /* no body */ }
  if (!res.ok) {
    const msg = (body && body.error && body.error.message) || res.statusText || ('HTTP ' + res.status);
    throw new Error(msg);
  }
  return body;
}
