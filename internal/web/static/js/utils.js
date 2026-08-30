'use strict';

/* ============================================================
   Grok Gateway Console · 通用工具与格式化模块
   ============================================================ */

export function $(sel, root) { return (root || document).querySelector(sel); }
export function $all(sel, root) { return Array.from((root || document).querySelectorAll(sel)); }

export function fmtNum(n) {
  if (n === null || n === undefined || Number.isNaN(Number(n))) return '—';
  return Number(n).toLocaleString('en-US');
}

export function fmtPct(n) {
  if (n === null || n === undefined || Number.isNaN(Number(n))) return '—';
  return Number(n).toFixed(1) + '%';
}

export function fmtMs(ms) {
  if (ms === null || ms === undefined || Number.isNaN(Number(ms))) return '—';
  const val = Number(ms);
  if (val < 1) return '<1ms';
  if (val < 1000) return val + 'ms';
  return (val / 1000).toFixed(2) + 's';
}

export function fmtBytes(bytes) {
  if (!bytes || bytes <= 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return (bytes / Math.pow(k, i)).toFixed(i === 0 ? 0 : 1) + ' ' + sizes[i];
}

export function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  });
}

export function fmtTimeShort(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  });
}

export function fmtTimeRelative(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  const now = new Date();
  const diffSec = Math.floor((now.getTime() - d.getTime()) / 1000);
  if (diffSec < 5) return '刚刚';
  if (diffSec < 60) return diffSec + ' 秒前';
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return diffMin + ' 分钟前';
  const diffHours = Math.floor(diffMin / 60);
  if (diffHours < 24) return diffHours + ' 小时前';
  const diffDays = Math.floor(diffHours / 24);
  return diffDays + ' 天前';
}

export function escapeHtml(str) {
  return String(str == null ? '' : str).replace(/[&<>"']/g, c => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;'
  }[c]));
}

export function escapeAttr(str) {
  return escapeHtml(str);
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
  return ({
    '15m': '15分钟',
    '1h': '1小时',
    '24h': '24小时',
    '7d': '7天',
    'all': '全部'
  })[range] || range;
}

export function tryPretty(raw) {
  if (!raw) return '';
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch (e) {
    return String(raw);
  }
}

export function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(text);
  }
  return new Promise((resolve, reject) => {
    const textArea = document.createElement('textarea');
    textArea.value = text;
    textArea.style.position = 'fixed';
    textArea.style.opacity = '0';
    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();
    try {
      document.execCommand('copy');
      textArea.remove();
      resolve();
    } catch (err) {
      textArea.remove();
      reject(err);
    }
  });
}

// 折叠展示/复制时单个体量上限，超过则截断并用注释说明，避免把整段报文塞进
// 剪贴板或 DOM。
export const MAX_PREVIEW_BYTES = 64 * 1024;

// 把一条日志还原成可复现的 curl 命令。请求体超过 MAX_PREVIEW_BYTES 时不内联，
// 只留一行注释：完整报文在抽屉里看，命令本身仍可执行。
export function buildCurlFromLog(log) {
  const url = log.request_url || log.request_path || '';
  let cmd = `curl ${JSON.stringify(url)} \\\n  -X ${log.method || 'POST'}`;
  const headers = log.request_headers;
  if (headers) {
    try {
      const obj = JSON.parse(headers);
      for (const [k, v] of Object.entries(obj)) {
        cmd += ` \\\n  -H ${JSON.stringify(k + ': ' + v)}`;
      }
    } catch (e) {}
  }
  const body = log.request_body || '';
  if (body) {
    if (body.length > MAX_PREVIEW_BYTES) {
      cmd += ` \\\n  # 请求体过大（${body.length} 字符），已折叠`;
    } else {
      cmd += ` \\\n  -d ${JSON.stringify(body)}`;
    }
  }
  return cmd;
}

export function downloadFile(filename, content, type = 'text/plain;charset=utf-8') {
  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  setTimeout(() => {
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }, 100);
}
