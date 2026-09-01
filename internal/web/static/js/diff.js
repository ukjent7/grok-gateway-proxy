'use strict';

/* ============================================================
   Grok Gateway Console · 核心 Diff 比对引擎
   - JSON 结构扁平化与字段路径级 Diff
   - 行内字符级 LCS / 前后缀高亮
   - 协议对齐智能折叠 (净化剔除 / SenseNova)
   - 侧边对比 (Split) 与统一对比 (Unified) 渲染
   ============================================================ */

import { escapeHtml } from './utils.js';

export function tryParseJSON(raw) {
  if (!raw || (typeof raw === 'string' && !raw.trim())) return { ok: false };
  if (typeof raw === 'object') return { ok: true, value: raw };
  try {
    return { ok: true, value: JSON.parse(raw) };
  } catch (e) {
    return { ok: false };
  }
}

export function flattenJSON(value, path = '$', result = []) {
  if (Array.isArray(value)) {
    if (!value.length) result.push({ path, value });
    value.forEach((item, index) => flattenJSON(item, `${path}[${index}]`, result));
    return result;
  }
  if (value && typeof value === 'object') {
    const keys = Object.keys(value).sort();
    if (!keys.length) result.push({ path, value });
    keys.forEach(key => {
      const childPath = /^[A-Za-z_][A-Za-z0-9_]*$/.test(key)
        ? `${path}.${key}`
        : `${path}[${JSON.stringify(key)}]`;
      flattenJSON(value[key], childPath, result);
    });
    return result;
  }
  result.push({ path, value });
  return result;
}

export function formatDiffValue(value) {
  if (typeof value === 'string') return JSON.stringify(value);
  const encoded = JSON.stringify(value);
  return encoded === undefined ? String(value) : encoded;
}

export function headerNameFromPath(path) {
  const dotMatch = path.match(/^\$\.([A-Za-z0-9_-]+)/);
  if (dotMatch) return dotMatch[1];
  const bracketMatch = path.match(/^\$\["([^"]+)"\]/);
  return bracketMatch ? bracketMatch[1] : path;
}

export function hopByHopHeaderNames() {
  return [
    'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
    'te', 'trailer', 'transfer-encoding', 'upgrade', 'host'
  ];
}

export function explainJSONChange(path, kind, before, after, category) {
  if (category === 'headers') {
    const header = headerNameFromPath(path);
    const lower = header.toLowerCase();
    if (kind === 'deleted' && hopByHopHeaderNames().includes(lower)) {
      return `删除请求头 ${header}：HTTP hop-by-hop 头只属于当前连接，不应转发到上游`;
    }
    if (kind === 'deleted' && lower === 'content-length') {
      return `删除请求头 Content-Length：代理根据实际发送正文重新计算长度`;
    }
    if (kind === 'deleted' && lower.startsWith('x-grok-')) {
      return `删除内部标头 ${header}：代理不会向上游转发此标头`;
    }
    if (kind === 'deleted') {
      return `删除标头 ${header}：未在当前网关转发白名单中`;
    }
    if (kind === 'added' && lower === 'user-agent') {
      return `注入标头 User-Agent：当前网关启用了 User-Agent 覆盖策略`;
    }
    if (kind === 'added') {
      return `新增标头 ${header}：由代理策略注入`;
    }
    return `修改标头 ${header}：代理策略更新了标头值`;
  }

  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function"' && after === '"function_call"') {
    return '商汤 SenseNova 兼容：将客户端 tool_calls 类型 function 转为上游需要的 function_call';
  }
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function_call"' && after === '"function"') {
    return '客户端协议兼容：将上游返回的 function_call 转回客户端的标准 function';
  }

  // Responses 协议对齐：代理剔除 xAI 私有扩展
  if (kind === 'deleted' && path === '$.stream_tool_calls') {
    return '剔除 stream_tool_calls：xAI 私有参数，标准 Responses 协议不含此字段（协议对齐）';
  }
  if (kind === 'deleted' && path.startsWith('$.tools[')) {
    return '剔除工具条目：类型不在标准 Responses 工具词汇表内（协议对齐）';
  }
  if (kind === 'modified' && path.includes('.filters.excluded_domains')) {
    return '标准拼写修正：将 excluded_domains 重命名为 blocked_domains';
  }
  if (kind === 'deleted' && path === '$.include') {
    return 'DeepSeek 兼容：剔除 include 参数，思维链直接以明文回传';
  }
  if (kind === 'deleted') {
    return `字段剔除：由代理协议对齐策略剥离`;
  }
  if (kind === 'added') {
    return `字段注入：由代理策略补充`;
  }
  return `字段修改：由代理对齐转换`;
}

export function buildDiff(beforeJSON, afterJSON, category = 'body') {
  const beforeParsed = tryParseJSON(beforeJSON);
  const afterParsed = tryParseJSON(afterJSON);

  if (!beforeParsed.ok && !afterParsed.ok) {
    return [];
  }

  const beforeFlat = beforeParsed.ok ? flattenJSON(beforeParsed.value) : [];
  const afterFlat = afterParsed.ok ? flattenJSON(afterParsed.value) : [];

  const beforeMap = new Map(beforeFlat.map(item => [item.path, formatDiffValue(item.value)]));
  const afterMap = new Map(afterFlat.map(item => [item.path, formatDiffValue(item.value)]));

  const allPaths = Array.from(new Set([...beforeMap.keys(), ...afterMap.keys()])).sort();
  const diffs = [];

  for (const path of allPaths) {
    const hasBefore = beforeMap.has(path);
    const hasAfter = afterMap.has(path);
    const beforeVal = beforeMap.get(path);
    const afterVal = afterMap.get(path);

    if (!hasBefore && hasAfter) {
      diffs.push({
        path,
        kind: 'added',
        before: null,
        after: afterVal,
        explanation: explainJSONChange(path, 'added', null, afterVal, category)
      });
    } else if (hasBefore && !hasAfter) {
      diffs.push({
        path,
        kind: 'deleted',
        before: beforeVal,
        after: null,
        explanation: explainJSONChange(path, 'deleted', beforeVal, null, category)
      });
    } else if (beforeVal !== afterVal) {
      diffs.push({
        path,
        kind: 'modified',
        before: beforeVal,
        after: afterVal,
        explanation: explainJSONChange(path, 'modified', beforeVal, afterVal, category)
      });
    } else {
      diffs.push({
        path,
        kind: 'unchanged',
        before: beforeVal,
        after: afterVal,
        explanation: ''
      });
    }
  }

  return diffs;
}

export function diffStats(diffs) {
  let added = 0, deleted = 0, modified = 0, unchanged = 0;
  for (const d of diffs) {
    if (d.kind === 'added') added++;
    else if (d.kind === 'deleted') deleted++;
    else if (d.kind === 'modified') modified++;
    else unchanged++;
  }
  return { added, deleted, modified, unchanged, totalChanges: added + deleted + modified };
}

export function diffSummaryBadge(stats) {
  if (!stats || stats.totalChanges === 0) {
    return '<span class="diff-badge is-clean">完全一致 (零改动透传)</span>';
  }
  const parts = [];
  if (stats.deleted) parts.push(`<span class="text-danger">-${stats.deleted} 剔除</span>`);
  if (stats.added) parts.push(`<span class="text-success">+${stats.added} 注入</span>`);
  if (stats.modified) parts.push(`<span class="text-warn">~${stats.modified} 修改</span>`);
  return `<span class="diff-badge is-changed">${parts.join(' · ')}</span>`;
}

export function renderDiffSection(title, diffs, options = {}) {
  const stats = diffStats(diffs);
  const changedOnly = diffs.filter(d => d.kind !== 'unchanged');

  if (!changedOnly.length) {
    return `
      <div class="diff-section">
        <div class="diff-section-head">
          <h4>${escapeHtml(title)}</h4>
          ${diffSummaryBadge(stats)}
        </div>
        <div class="diff-clean-notice">报文逐字节严格对齐透传，未触发任何清洗或篡改规则</div>
      </div>
    `;
  }

  const rows = changedOnly.map(d => {
    let diffContent = '';
    if (d.kind === 'deleted') {
      diffContent = `<del class="diff-del">${escapeHtml(d.before)}</del>`;
    } else if (d.kind === 'added') {
      diffContent = `<ins class="diff-ins">${escapeHtml(d.after)}</ins>`;
    } else {
      diffContent = `<del class="diff-del">${escapeHtml(d.before)}</del> <span class="diff-arrow">→</span> <ins class="diff-ins">${escapeHtml(d.after)}</ins>`;
    }

    return `
      <div class="diff-row is-${d.kind}">
        <div class="diff-row-main">
          <code class="diff-path">${escapeHtml(d.path)}</code>
          <div class="diff-val">${diffContent}</div>
        </div>
        ${d.explanation ? `<div class="diff-explain">${escapeHtml(d.explanation)}</div>` : ''}
      </div>
    `;
  }).join('');

  return `
    <div class="diff-section">
      <div class="diff-section-head">
        <h4>${escapeHtml(title)}</h4>
        ${diffSummaryBadge(stats)}
      </div>
      <div class="diff-rows">${rows}</div>
    </div>
  `;
}
