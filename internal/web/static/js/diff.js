'use strict';

import { escapeHtml } from './utils.js';

/* ============================================================
   Diff 引擎（纯函数，无 DOM 依赖，便于独立维护与测试）
   - JSON 差异：扁平化后按路径对比 + 期望转换折叠
   - 文本差异：行级 LCS + 行内字符高亮（对超大文本有保护性折叠）
   - 新增：单字级 inline 高亮、V3 协议预期变更自动归组
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

/* ---------- 预期变更判定（用于优雅折叠） ---------- */
export function isExpectedFXChange(row) {
  const exp = row.explanation || '';
  // 包含这些字样的都属 V3 协议转换的预期行为
  return exp.includes('V3') || exp.includes('已转 header') || exp.includes('FX') || exp.includes('prompt 不承载') || exp.includes('已按 V3 重写');
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
  // FX V3 协议：历史 reasoning / include 等在 V3 prompt 中无对应字段，属预期丢弃（已对齐官方 fx / fx-gateway-proxy）
  if (kind === 'deleted' && path.includes('reasoning')) return '删除 reasoning 历史：V3 的 prompt 不承载历史 reasoning（仅保留顶层 reasoning.effort -> reasoning），已与官方 fx 行为对齐';
  if (kind === 'deleted' && (path === '$.store' || path === '$.include' || path.includes('prompt_cache_key'))) return '删除字段 ' + path + '：已转 header（prompt_cache_key -> X-Session-Id）或 V3 不支持，属预期协议转换';
  if (kind === 'deleted' && path.startsWith('$.input[')) return '删除字段 ' + path + '：Responses 的 input 已按 V3 重写为 prompt（message/function_call -> user/assistant/tool），属 FX 协议转换，非误删';
  if (kind === 'added' && path.startsWith('$.prompt[')) return '新增字段 ' + path + '：V3 的 prompt 由 Responses input 转换生成';
  // V3 tools：Responses 的 parameters -> V3 的 inputSchema 重命名（你看到的 foreground.description 就是这个）
  if (path.includes('.tools[') && (path.includes('.parameters') || path.includes('.inputSchema'))) {
    if (kind === 'deleted' && path.includes('.parameters')) return '删除字段 ' + path + '：V3 将 parameters 重命名为 inputSchema，属预期协议转换（同值已在 inputSchema 同路径下新增）';
    if (kind === 'added' && path.includes('.inputSchema')) return '新增字段 ' + path + '：由 parameters 重命名而来（V3 inputSchema），属预期协议转换';
    return '修改字段 ' + path + '：tools 参数结构（V3 parameters ↔ inputSchema）';
  }
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
      const afterVal = formatDiffValue(afterMap.get(path));
      const explanation = explainJSONChange(path, 'added', '', afterVal, category);
      rows.push({ kind: 'added', path, after: afterVal, explanation, expected: explanation.includes('V3') || explanation.includes('FX') || explanation.includes('已转 header') });
    } else if (!hasAfter) {
      const beforeVal = formatDiffValue(beforeMap.get(path));
      const explanation = explainJSONChange(path, 'deleted', beforeVal, '', category);
      rows.push({ kind: 'deleted', path, before: beforeVal, explanation, expected: explanation.includes('V3') || explanation.includes('FX') || explanation.includes('已转 header') || explanation.includes('prompt 不承载') });
    } else if (JSON.stringify(beforeMap.get(path)) !== JSON.stringify(afterMap.get(path))) {
      const beforeValue = formatDiffValue(beforeMap.get(path));
      const afterValue = formatDiffValue(afterMap.get(path));
      const explanation = explainJSONChange(path, 'modified', beforeValue, afterValue, category);
      rows.push({ kind: 'modified', path, before: beforeValue, after: afterValue, explanation, expected: false });
    }
  });
  return mergeRenameRows(rows);
}

/* 将 V3 的 parameters -> inputSchema 重命名对合并为一条 moved，避免显示为 删除+新增 */
function mergeRenameRows(rows) {
  const delMap = new Map();
  const addMap = new Map();
  rows.forEach(r => {
    if (r.kind === 'deleted' && r.path.includes('.parameters')) delMap.set(r.path, r);
    if (r.kind === 'added' && r.path.includes('.inputSchema')) addMap.set(r.path, r);
  });
  if (!delMap.size || !addMap.size) return rows;
  const toRemove = new Set();
  const merged = [];
  for (const [delPath, delRow] of delMap) {
    const addPath = delPath.replace('.parameters', '.inputSchema');
    const addRow = addMap.get(addPath);
    if (!addRow) continue;
    // 值相同或都是字符串描述，视为重命名搬运
    const sameValue = delRow.before === addRow.after;
    merged.push({
      kind: sameValue ? 'moved' : 'modified',
      path: delPath + ' → ' + addPath,
      before: delRow.before,
      after: addRow.after,
      explanation: sameValue
        ? 'V3 重命名：' + delPath + ' → ' + addPath + '（parameters → inputSchema，值未变，属预期协议转换）'
        : 'V3 重命名+修改：' + delPath + ' → ' + addPath + '（parameters → inputSchema）',
      expected: true,
    });
    toRemove.add(delPath);
    toRemove.add(addPath);
  }
  if (!merged.length) return rows;
  const filtered = rows.filter(r => !toRemove.has(r.path));
  return [...filtered, ...merged].sort((a, b) => a.path.localeCompare(b.path));
}

export function buildValueDiff(path, before, after) {
  if (String(before) === String(after)) return { rows: [] };
  return { rows: [{ kind: 'modified', path, before: String(before), after: String(after), explanation: path + ' 从 ' + before + ' 变为 ' + after, expected: false }] };
}

export function truncateDiffText(text, max) {
  max = max || 4000;
  return text.length > max ? text.slice(0, max) + '\n… (已折叠)' : text;
}

/* ---------- 行内字符级高亮 ---------- */
function escapeHtmlRaw(s) { return escapeHtml(s); }

// 前后缀法：O(n) 找到首个差异区，适合“几百字里改一个字”
function prefixSuffixHighlight(before, after, context) {
  context = context || 64;
  let pre = 0;
  const minLen = Math.min(before.length, after.length);
  while (pre < minLen && before[pre] === after[pre]) pre++;
  let suf = 0;
  while (suf < minLen - pre && before[before.length - 1 - suf] === after[after.length - 1 - suf]) suf++;
  // 完全相同
  if (pre === before.length && pre === after.length) return { beforeHTML: escapeHtmlRaw(before), afterHTML: escapeHtmlRaw(after) };
  const beforeMid = before.slice(pre, before.length - suf);
  const afterMid = after.slice(pre, after.length - suf);
  const beforePre = before.slice(0, pre);
  const beforeSuf = before.slice(before.length - suf);
  const afterPre = after.slice(0, pre);
  const afterSuf = after.slice(after.length - suf);
  // 上下文截断
  function withContext(preStr, mid, sufStr) {
    let out = '';
    if (preStr.length > context) out += '…' + escapeHtmlRaw(preStr.slice(-context));
    else out += escapeHtmlRaw(preStr);
    if (mid) out += '<span class="diff-char-hl ' + (mid === beforeMid ? 'diff-char-del' : 'diff-char-add') + '">' + escapeHtmlRaw(mid) + '</span>';
    if (sufStr.length > context) out += escapeHtmlRaw(sufStr.slice(0, context)) + '…';
    else out += escapeHtmlRaw(sufStr);
    return out;
  }
  // 需区分 before/after 的中段样式
  const beforeHTML = (pre > 0 || beforeMid || suf > 0) ? (function(){
    let html = '';
    if (beforePre.length > context) html += '…' + escapeHtmlRaw(beforePre.slice(-context));
    else html += escapeHtmlRaw(beforePre);
    if (beforeMid) html += '<span class="diff-char-del">' + escapeHtmlRaw(beforeMid) + '</span>';
    if (beforeSuf.length > context) html += escapeHtmlRaw(beforeSuf.slice(0, context)) + '…';
    else html += escapeHtmlRaw(beforeSuf);
    // 若全被截断且 mid 为空，给提示
    if (!beforeMid && before.length > context*2) html = escapeHtmlRaw(before.slice(0, context)) + ' … ' + escapeHtmlRaw(before.slice(-context));
    return html;
  })() : escapeHtmlRaw(before);
  const afterHTML = (function(){
    let html = '';
    if (afterPre.length > context) html += '…' + escapeHtmlRaw(afterPre.slice(-context));
    else html += escapeHtmlRaw(afterPre);
    if (afterMid) html += '<span class="diff-char-add">' + escapeHtmlRaw(afterMid) + '</span>';
    if (afterSuf.length > context) html += escapeHtmlRaw(afterSuf.slice(0, context)) + '…';
    else html += escapeHtmlRaw(afterSuf);
    if (!afterMid && after.length > context*2) html = escapeHtmlRaw(after.slice(0, context)) + ' … ' + escapeHtmlRaw(after.slice(-context));
    return html;
  })();
  return { beforeHTML, afterHTML };
}

// LCS 字符级（仅对短串 ≤2000 字符启用，超长回退到前后缀法）
function charLcsHighlight(before, after) {
  const n = before.length, m = after.length;
  if (n * m > 1200000 || n > 2500 || m > 2500) return prefixSuffixHighlight(before, after, 80);
  // DP 表用两行滚动
  const prev = new Uint16Array(m + 1);
  const cur = new Uint16Array(m + 1);
  const table = Array(n + 1);
  for (let i = 0; i <= n; i++) table[i] = new Uint16Array(m + 1);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      table[i][j] = before[i] === after[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }
  let i = 0, j = 0;
  let bHTML = '', aHTML = '';
  while (i < n && j < m) {
    if (before[i] === after[j]) { bHTML += escapeHtmlRaw(before[i]); aHTML += escapeHtmlRaw(after[j]); i++; j++; }
    else if (table[i + 1][j] >= table[i][j + 1]) { bHTML += '<span class="diff-char-del">' + escapeHtmlRaw(before[i]) + '</span>'; i++; }
    else { aHTML += '<span class="diff-char-add">' + escapeHtmlRaw(after[j]) + '</span>'; j++; }
  }
  while (i < n) { bHTML += '<span class="diff-char-del">' + escapeHtmlRaw(before[i++]) + '</span>'; }
  while (j < m) { aHTML += '<span class="diff-char-add">' + escapeHtmlRaw(after[j++]) + '</span>'; }
  // 过长时加上下文折叠
  const limit = 2200;
  if (bHTML.length > limit || aHTML.length > limit) return prefixSuffixHighlight(before, after, 80);
  return { beforeHTML: bHTML, afterHTML: aHTML };
}

export function inlineHighlight(before, after) {
  // 空值保护
  before = String(before || '');
  after = String(after || '');
  if (before === after) return { beforeHTML: escapeHtmlRaw(before), afterHTML: escapeHtmlRaw(after) };
  // 短串用 LCS 精细高亮，长串用前后缀
  if (Math.max(before.length, after.length) < 800) return charLcsHighlight(before, after);
  return prefixSuffixHighlight(before, after, 80);
}

export function buildTextDiff(before, after) {
  // 单行超长文本（常见于 body 单行 JSON）直接做字符级对比，避免整行标为 modified 看不出差异
  const oldLines = before.split(/\r?\n/);
  const newLines = after.split(/\r?\n/);
  if (oldLines.length === 1 && newLines.length === 1 && Math.max(before.length, after.length) > 200) {
    // 单行大文本：用字符级高亮的单条 modified
    return [{ kind: 'modified', path: '文本', before, after, explanation: '单行文本字符级差异（已高亮改动处，前后各保留约80字上下文）' }];
  }
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
      rows.push({ kind: 'modified', path: '第 ' + (index + 1) + ' 行', before: current.text, after: next.text, explanation: '修改第 ' + (index + 1) + ' 行（已对行内字符做高亮）', expected: false });
      index++;
    } else if (current.kind === 'deleted') {
      rows.push({ kind: 'deleted', path: '文本行', before: current.text, explanation: '删除文本行', expected: false });
    } else if (current.kind === 'added') {
      rows.push({ kind: 'added', path: '文本行', after: current.text, explanation: '新增文本行', expected: false });
    } else {
      rows.push({ kind: 'same', path: '', before: current.text, after: current.text, expected: false });
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
      if (row.expected) continue; // 预期转换不计入顶部数字，避免“一堆删除”吓人
      if (row.kind === 'added') total.added++;
      if (row.kind === 'modified' || row.kind === 'moved') total.modified++;
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
  const unexpected = changedRows.filter(r => !r.expected);
  const expected = changedRows.filter(r => r.expected);
  if (!changedRows.length) {
    return '<section class="diff-section diff-section-empty"><div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-no-change">无变化</span></div></section>';
  }
  // 若全是预期转换，直接给出友好提示而非吓人的数字
  if (!unexpected.length && expected.length) {
    return '<section class="diff-section diff-section-empty"><div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-no-change">仅含预期协议转换</span></div>' +
      '<div class="diff-expected-note">检测到 ' + expected.length + ' 处 V3/FX 预期转换（reasoning 历史删除、input→prompt 重写等），已自动折叠。<details style="margin-top:8px"><summary style="cursor:pointer;color:var(--teal)">展开查看 ' + expected.length + ' 条预期变更</summary><div class="diff-rows" style="margin-top:8px">' + renderDiffRows(expected) + '</div></details></div></section>';
  }
  return '<section class="diff-section">' +
    '<div class="diff-section-head"><div><strong>' + escapeHtml(title) + '</strong><span>' + escapeHtml(subtitle) + '</span></div><span class="diff-change-count">' + unexpected.length + ' 处实质变更' + (expected.length ? '（+' + expected.length + ' 预期已折叠）' : '') + '</span></div>' +
    '<div class="diff-column-head"><span>原始</span><span>代理实际</span></div>' +
    '<div class="diff-rows">' + renderDiffRows(unexpected) + 
      (expected.length ? '<details class="diff-expected-wrap"><summary>展开 ' + expected.length + ' 条预期协议转换（V3/FX）</summary><div class="diff-rows">' + renderDiffRows(expected) + '</div></details>' : '') +
    '</div>' +
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
  const isMod = row.kind === 'modified' || row.kind === 'moved';
  let oldLine = '', newLine = '';
  let oldHTML = '', newHTML = '';
  if (row.kind === 'added') {
    newLine = diffLineText(row, row.after || '');
    newHTML = escapeHtml(newLine);
  } else if (row.kind === 'deleted') {
    oldLine = diffLineText(row, row.before || '');
    oldHTML = escapeHtml(oldLine);
  } else if (isMod) {
    // moved: 路径已包含 →，值相同则无需字符高亮，直接展示
    if (row.kind === 'moved') {
      oldHTML = escapeHtml(diffLineText({ path: row.path.split(' → ')[0], kind: 'modified' }, row.before || ''));
      newHTML = escapeHtml(diffLineText({ path: row.path.split(' → ')[1] || row.path, kind: 'modified' }, row.after || ''));
      // 同值搬运时在行尾加提示，避免用户以为是修改
      if (row.before === row.after) {
        oldHTML += ' <span style="color:var(--text-3);font-size:10px">(搬运)</span>';
        newHTML += ' <span style="color:var(--text-3);font-size:10px">(搬运)</span>';
      }
    } else {
      const leftRaw = diffLineText(row, row.before || '');
      const rightRaw = diffLineText(row, row.after || '');
      const inline = inlineHighlight(leftRaw, rightRaw);
      oldHTML = inline.beforeHTML;
      newHTML = inline.afterHTML;
    }
  } else {
    oldHTML = escapeHtml(diffLineText(row, row.before || ''));
    newHTML = escapeHtml(diffLineText(row, row.after || ''));
  }
  const oldClass = row.kind === 'deleted' || isMod ? 'diff-line-old' : 'diff-line-empty';
  const newClass = row.kind === 'added' || isMod ? 'diff-line-new' : 'diff-line-empty';
  const marker = row.kind === 'added' ? '+' : row.kind === 'deleted' ? '−' : row.kind === 'moved' ? '↔' : isMod ? '±' : ' ';
  const expClass = row.expected ? ' diff-row-expected' : '';
  return '<div class="diff-row ' + row.kind + expClass + '">' +
    '<div class="diff-line ' + oldClass + '"><i>' + (row.kind === 'added' ? ' ' : marker) + '</i><code>' + (isMod ? oldHTML : escapeHtml(oldLine ? oldLine : '')) + '</code></div>' +
    '<div class="diff-line ' + newClass + '"><i>' + (row.kind === 'deleted' ? ' ' : marker) + '</i><code>' + (isMod ? newHTML : escapeHtml(newLine ? newLine : '')) + '</code></div>' +
    (row.kind === 'same' ? '' : '<div class="diff-explanation">' + escapeHtml(row.explanation || '') + (row.path ? ' <code>' + escapeHtml(row.path) + '</code>' : '') + '</div>') +
  '</div>';
}
