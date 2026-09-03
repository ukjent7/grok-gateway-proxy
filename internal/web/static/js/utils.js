'use strict';

export const $ = (sel, el = document) => el.querySelector(sel);
export const $all = (sel, el = document) => Array.from(el.querySelectorAll(sel));

export function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

export function gatewayPrefixLabel(gw, id) {
  const prefix = (gw && gw.prefix) || `/${id}`;
  return prefix.startsWith('/') ? prefix : `/${prefix}`;
}

export const GATEWAY_TONE_COUNT = 8;

export function gatewayTone(id) {
  const key = String(id || '');
  let hash = 0;
  for (let i = 0; i < key.length; i++) hash = (hash * 31 + key.charCodeAt(i)) >>> 0;
  return hash % GATEWAY_TONE_COUNT;
}

export function fmtNum(n) {
  if (n === null || n === undefined || isNaN(n)) return '0';
  return Number(n).toLocaleString('en-US');
}

export function fmtPct(n) {
  if (n === null || n === undefined || isNaN(n)) return '—';
  return Number(n).toFixed(1) + '%';
}

export function fmtMs(ms) {
  if (ms === null || ms === undefined || isNaN(ms)) return '0ms';
  const num = Number(ms);
  if (num < 1000) return `${Math.round(num)}ms`;
  return `${(num / 1000).toFixed(2)}s`;
}

export function fmtCompact(n) {
  if (n === null || n === undefined || isNaN(n)) return '0';
  const num = Number(n);
  if (Math.abs(num) >= 1_000_000) return (num / 1_000_000).toFixed(1) + 'M';
  if (Math.abs(num) >= 1_000) return (num / 1_000).toFixed(1) + 'k';
  return String(num);
}

export function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime())) return String(t);
  return d.toLocaleString('zh-CN', { hour12: false });
}

export function fmtTimeShort(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime())) return String(t);
  const pad = (n) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const success = document.execCommand('copy');
    document.body.removeChild(ta);
    return success;
  }
}

export function downloadFile(filename, content, mime = 'text/plain;charset=utf-8') {
  const blob = new Blob([content], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

export function buildCurlFromLog(log) {
  if (!log) return '';
  const method = log.method || 'POST';
  const host = window.location.host;
  const raw = log.request_url || log.request_path || '';
  const path = raw.startsWith('/') ? raw : '/' + raw;
  const url = `${window.location.protocol}//${host}${path}`;

  const escapeHeader = (s) => String(s).replace(/\\/g, '\\\\').replace(/"/g, '\\"');

  let cmd = `curl -X ${method} "${url}" \\\n  -H "Content-Type: application/json"`;

  const reqHeaders = log.request_headers || {};
  for (const [k, v] of Object.entries(reqHeaders)) {
    if (k.toLowerCase() === 'content-type' || k.toLowerCase() === 'host') continue;
    const val = Array.isArray(v) ? v.join(', ') : v;
    cmd += ` \\\n  -H "${escapeHeader(k)}: ${escapeHeader(val)}"`;
  }

  const reqBody = log.request_body;
  if (reqBody) {
    const escaped = typeof reqBody === 'string' ? reqBody : JSON.stringify(reqBody);
    cmd += ` \\\n  -d '${escaped.replace(/'/g, `'\\''`)}'`;
  }
  return cmd;
}

export function csvCell(val) {
  if (val === null || val === undefined) return '""';
  const s = String(val).replace(/"/g, '""');
  return `"${s}"`;
}

export function latencyThresholds(items) {
  if (!items || !items.length) return { relative: false, slow: 2000 };
  const latencies = items
    .filter(l => l.success && l.duration_ms > 0)
    .map(l => l.duration_ms)
    .sort((a, b) => a - b);
  if (latencies.length < 5) return { relative: false, slow: 2000 };
  // 显著离群而非分位数：p90 在数学上必然把窗口内约 10% 的成功请求判为"慢"，
  // 阈值只会永远报警。中位数×5（下限 2s）只抓真正偏离正常水平的调用。
  const median = latencies[Math.floor(latencies.length / 2)];
  return { relative: true, slow: Math.max(2000, median * 5) };
}

export function latencyClass(ms, thresholds) {
  if (!thresholds || !thresholds.relative) {
    if (ms > 3000) return 'latency-slow';
    if (ms > 1000) return 'latency-medium';
    return 'latency-fast';
  }
  if (ms >= thresholds.slow) return 'latency-slow';
  if (ms >= thresholds.slow * 0.6) return 'latency-medium';
  return 'latency-fast';
}

export function latencyTitle(ms, thresholds) {
  if (!thresholds || !thresholds.relative) return fmtMs(ms);
  if (ms >= thresholds.slow) return `${fmtMs(ms)} (超过慢请求阈值 ${fmtMs(thresholds.slow)})`;
  return fmtMs(ms);
}

export function rangeToFrom(range) {
  const now = new Date();
  if (range === '15m') return new Date(now.getTime() - 15 * 60 * 1000);
  if (range === '1h') return new Date(now.getTime() - 60 * 60 * 1000);
  if (range === '24h') return new Date(now.getTime() - 24 * 60 * 60 * 1000);
  if (range === '7d') return new Date(now.getTime() - 7 * 24 * 60 * 60 * 1000);
  return null;
}

export function rangeLabel(range) {
  if (range === '15m') return '15分钟';
  if (range === '1h') return '1小时';
  if (range === '24h') return '24小时';
  if (range === '7d') return '7天';
  return '全部';
}

export function tryPretty(raw) {
  if (!raw) return '';
  if (typeof raw === 'object') {
    try {
      return JSON.stringify(raw, null, 2);
    } catch (_) {
      return String(raw);
    }
  }
  try {
    const parsed = JSON.parse(raw);
    return JSON.stringify(parsed, null, 2);
  } catch (_) {
    return String(raw);
  }
}
