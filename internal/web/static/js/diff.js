'use strict';

import { escapeHtml } from './utils.js';

/* ============================================================
   Diff 引擎（纯函数，无 DOM 依赖，便于独立维护与测试）
   - JSON 差异：扁平化后按路径对比
   - 文本差异：行级 LCS（对超大文本有保护性折叠）
   ============================================================ */

export function tryParseJSON(raw) {
  if (!raw || !raw.trim()) return { ok: false };
  try { return { ok: true, value: JSON.parse(raw) }; } catch (e) { return { ok: false }; }
}

export function flattenJSON(value, path, result) {
  result = result || [];
  path = path || '$';
  if (Array.isArray(value)) {
    if (!value.length) result.push({ path, value });
    value.forEach((item, index) => flattenJSON(item, path + '[' + index + ']', result));
    return result;
  }
  if (value && typeof value === 'object') {
    const keys = Object.keys(value).sort();
    if (!keys.length) result.push({ path, value });
    keys.forEach(key => {
      const childPath = /^[A-Za-z_][A-Za-z0-9_]*$/.test(key) ? path + '.' + key : path + '[' + JSON.stringify(key) + ']';
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
  return ['connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization', 'te', 'trailer', 'transfer-encoding', 'upgrade', 'host'];
}

export function explainJSONChange(path, kind, before, after, category) {
  if (category === 'headers') {
    const header = headerNameFromPath(path);
    const lower = header.toLowerCase();
    if (kind === 'deleted' && hopByHopHeaderNames().includes(lower)) return '删除请求头 ' + header + '：HTTP hop-by-hop 头只属于当前连接，不应转发到下一跳';
    if (kind === 'deleted' && lower === 'content-length') return '删除请求头 Content-Length：代理会根据实际发送的正文重新管理长度';
    if (kind === 'deleted' && lower.startsWith('x-grok-')) return '删除请求头 ' + header + '：这是客户端/代理内部头，代理不会转发';
    if (kind === 'deleted') return '删除请求头 ' + header + '：未包含在当前网关的 Forward Headers 白名单中';
    if (kind === 'added' && lower === 'user-agent') return '新增请求头 User-Agent：当前网关启用了 User-Agent 覆盖或客户端头被代理补齐';
    if (kind === 'added') return '新增请求头 ' + header + '：由代理转发白名单或代理策略加入';
    return '修改请求头 ' + header + '：代理策略改变了该值';
  }
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function"' && after === '"function_call"') return '商汤协议兼容：将客户端 tool_calls 类型 function 转为上游要求的 function_call';
  if (path.endsWith('.type') && path.includes('.tool_calls[') && before === '"function_call"' && after === '"function"') return '客户端协议兼容：将商汤返回的 function_call 转回客户端使用的 function';
  if (kind === 'added') return '新增字段 ' + path;
  if (kind === 'deleted') return '删除字段 ' + path;
  return '修改字段 ' + path;
}

export function buildJSONDiff(before, after, category) {
  const beforeMap = new Map(flattenJSON(before).map(item => [item.path, item.value]));
  const afterMap = new Map(flattenJSON(after).map(item => [item.path, item.value]));
  const paths = Array.from(new Set([...beforeMap.keys(), ...afterMap.keys()])).sort();
  const rows = [];
  paths.forEach(path => {
    const hasBefore = beforeMap.has(path);
    const hasAfter = afterMap.has(path);
    if (!hasBefore) {
      rows.push({ kind: 'added', path, after: formatDiffValue(afterMap.get(path)), explanation: explainJSONChange(path, 'added', '', formatDiffValue(afterMap.get(path)), category) });
    } else if (!hasAfter) {
      rows.push({ kind: 'deleted', path, before: formatDiffValue(beforeMap.get(path)), explanation: explainJSONChange(path, 'deleted', formatDiffValue(beforeMap.get(path)), '', category) });
    } else if (JSON.stringify(beforeMap.get(path)) !== JSON.stringify(afterMap.get(path))) {
      const beforeValue = formatDiffValue(beforeMap.get(path));
      const afterValue = formatDiffValue(afterMap.get(path));
      rows.push({ kind: 'modified', path, before: beforeValue, after: afterValue, explanation: explainJSONChange(path, 'modified', beforeValue, afterValue, category) });
    }
  });
  return rows;
}

export function buildValueDiff(path, before, after) {
  if (String(before) === String(after)) return { rows: [] };
  return { rows: [{ kind: 'modified', path, before: String(before), after: String(after), explanation: path + ' 从 ' + before + ' 变为 ' + after }] };
}

export function truncateDiffText(text, max) {
  max = max || 4000;
  return text.length > max ? text.slice(0, max) + '\n… (已折叠)' : text;
}

export function buildTextDiff(before, after) {
  const oldLines = before.split(/\r?\n/);
  const newLines = after.split(/\r?\n/);
  const maxCells = 1600000;
  if (oldLines.length * newLines.length > maxCells) {
    return [{ kind: 'modified', path: '文本', before: truncateDiffText(before), after: truncateDiffText(after), explanation: '文本内容发生变化，内容过大，已折叠显示' }];
  }
  const table = Array.from({ length: oldLines.length + 1 }, () => new Uint32Array(newLines.length + 1));
  for (let i = oldLines.length - 1; i >= 0; i--) {
    for (let j = newLines.length - 1; j >= 0; j--) {
      table[i][j] = oldLines[i] === newLines[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }
  const operations = [];
  let i = 0, j = 0;
  while (i < oldLines.length && j < newLines.length) {
    if (oldLines[i] === newLines[j]) { operations.push({ kind: 'same', text: oldLines[i] }); i++; j++; }
    else if (table[i + 1][j] >= table[i][j + 1]) { operations.push({ kind: 'deleted', text: oldLines[i++] }); }
    else { operations.push({ kind: 'added', text: newLines[j++] }); }
  }
  while (i < oldLines.length) operations.push({ kind: 'deleted', text: oldLines[i++] });
  while (j < newLines.length) operations.push({ kind: 'added', text: newLines[j++] });
  const rows = [];
  for (let index = 0; index < operations.length; index++) {
    const current = operations[index];
    const next = operations[index + 1];
    if (current.kind === 'deleted' && next && next.kind === 'added') {
      rows.push({ kind: 'modified', path: '第 ' + (index + 1) + ' 行', before: current.text, after: next.text, explanation: '修改第 ' + (index + 1) + ' 行' });
      index++;
    } else if (current.kind === 'deleted') {
      rows.push({ kind: 'deleted', path: '文本行', before: current.text, explanation: '删除文本行' });
    } else if (current.kind === 'added') {
      rows.push({ kind: 'added', path: '文本行', after: current.text, explanation: '新增文本行' });
    } else {
      rows.push({ kind: 'same', path: '', before: current.text, after: current.text });
    }
  }
  return rows;
}

export function buildDiff(beforeRaw, afterRaw, category) {
  const before = String(beforeRaw || '');
  const after = String(afterRaw || '');
  if (before === after) return { rows: [] };
  const beforeJSON = tryParseJSON(before);
  const afterJSON = tryParseJSON(after);
  if (beforeJSON.ok && afterJSON.ok) return { rows: buildJSONDiff(beforeJSON.value, afterJSON.value, category) };
  return { rows: buildTextDiff(before, after) };
}

export function diffStats(diffs) {
  return diffs.filter(Boolean).reduce((total, diff) => {
    for (const row of diff.rows) {
      if (row.kind === 'added') total.added++;
      if (row.kind === 'modified') total.modified++;
      if (row.kind === 'deleted') total.deleted++;
      if (row.kind !== 'same') total.total++;
    }
    return total;
  }, { added: 0, modified: 0, deleted: 0, total: 0 });
}

export function diffSummaryBadge(label, count, kind) {
  return count ? '<span class="diff-summary-badge ' + kind + '">' + label + ' ' + count + '</span>' : '';
}

export function renderDiffSection(diff, title, subtitle) {
  const changedRows = diff.rows.filter(row => row.kind !== 'same');
  if (!changedRows.length) {
    return '<section class="diff-section diff-section-empty"><div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-no-change">无变化</span></div></section>';
  }
  return '<section class="diff-section">' +
    '<div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-change-count">' + changedRows.length + ' 处变更</span></div>' +
    '<div class="diff-column-head"><span>原始</span><span>代理实际</span></div>' +
    '<div class="diff-rows">' + renderDiffRows(diff.rows) + '</div>' +
  '</section>';
}

export function renderDiffRows(rows) {
  const output = [];
  const changedIndexes = rows.map((row, index) => row.kind !== 'same' ? index : -1).filter(index => index >= 0);
  const visibleContext = new Set();
  changedIndexes.forEach(index => {
    for (let offset = -2; offset <= 2; offset++) {
      if (index + offset >= 0 && index + offset < rows.length) visibleContext.add(index + offset);
    }
  });
  let collapsed = false;
  rows.forEach((row, index) => {
    if (row.kind === 'same' && !visibleContext.has(index)) {
      if (!collapsed) output.push('<div class="diff-collapse">… 其余未变化内容已折叠 …</div>');
      collapsed = true;
      return;
    }
    collapsed = false;
    output.push(renderDiffRow(row));
  });
  return output.join('');
}

function diffLineText(row, value) {
  if (row.kind === 'same' || !row.path) return value;
  return row.path + ' = ' + value;
}

function renderDiffRow(row) {
  const oldLine = row.kind === 'added' ? '' : diffLineText(row, row.before || '');
  const newLine = row.kind === 'deleted' ? '' : diffLineText(row, row.after || '');
  const oldClass = row.kind === 'deleted' || row.kind === 'modified' ? 'diff-line-old' : 'diff-line-empty';
  const newClass = row.kind === 'added' || row.kind === 'modified' ? 'diff-line-new' : 'diff-line-empty';
  const marker = row.kind === 'added' ? '+' : row.kind === 'deleted' ? '−' : row.kind === 'modified' ? '±' : ' ';
  return '<div class="diff-row ' + row.kind + '">' +
    '<div class="diff-line ' + oldClass + '"><i>' + (row.kind === 'added' ? ' ' : marker) + '</i><code>' + escapeHtml(oldLine) + '</code></div>' +
    '<div class="diff-line ' + newClass + '"><i>' + (row.kind === 'deleted' ? ' ' : marker) + '</i><code>' + escapeHtml(newLine) + '</code></div>' +
    (row.kind === 'same' ? '' : '<div class="diff-explanation">' + escapeHtml(row.explanation || '') + (row.path ? ' <code>' + escapeHtml(row.path) + '</code>' : '') + '</div>') +
  '</div>';
}
