'use strict';

/* ============================================================
   Grok Gateway Console · API 通信模块
   ============================================================ */

export async function api(path, opts = {}) {
  const headers = Object.assign({
    'Content-Type': 'application/json',
    'Accept': 'application/json'
  }, opts.headers || {});

  const timeoutMs = opts.timeout || 12000;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);

  let res;
  try {
    res = await fetch('/api' + path, Object.assign({}, opts, {
      headers,
      signal: opts.signal || controller.signal
    }));
  } catch (e) {
    clearTimeout(timer);
    if (e.name === 'AbortError') {
      throw new Error('请求超时（超过 ' + (timeoutMs / 1000) + ' 秒未响应）');
    }
    throw new Error('无法连接到代理服务（' + e.message + '）');
  } finally {
    clearTimeout(timer);
  }

  let body = null;
  const contentType = res.headers.get('content-type') || '';
  if (contentType.includes('application/json')) {
    try {
      body = await res.json();
    } catch (e) {
      /* parse error fallback */
    }
  } else {
    try {
      body = await res.text();
    } catch (e) {
      /* text fallback */
    }
  }

  if (!res.ok) {
    let msg = 'HTTP ' + res.status;
    if (body && typeof body === 'object') {
      msg = (body.error && body.error.message) || body.error || body.message || msg;
    } else if (typeof body === 'string' && body.trim()) {
      msg = body;
    }
    throw new Error(msg);
  }

  return body;
}
