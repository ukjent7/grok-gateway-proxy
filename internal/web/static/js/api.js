'use strict';

export async function api(endpoint, options = {}) {
  const url = endpoint.startsWith('/api') || endpoint.startsWith('/healthz') ? endpoint : '/api' + (endpoint.startsWith('/') ? endpoint : '/' + endpoint);
  const opts = { ...options };
  opts.headers = {
    'Accept': 'application/json',
    ...(opts.body ? { 'Content-Type': 'application/json' } : {}),
    ...opts.headers,
  };

  const res = await fetch(url, opts);
  if (!res.ok) {
    let errMsg = `HTTP ${res.status}`;
    try {
      const errJson = await res.json();
      if (errJson && errJson.error) {
        errMsg = errJson.error;
      }
    } catch (_) {
      const text = await res.text().catch(() => '');
      if (text) errMsg = text;
    }
    throw new Error(errMsg);
  }

  if (res.status === 204) {
    return null;
  }

  const contentType = res.headers.get('content-type') || '';
  if (contentType.includes('application/json')) {
    return await res.json();
  }
  return await res.text();
}
