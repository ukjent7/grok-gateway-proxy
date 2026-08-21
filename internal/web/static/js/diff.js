'use strict';

/* ============================================================
   Grok Gateway Console · 核心 Diff 比对引擎
   - JSON 结构扁平化与字段路径级 Diff
   - 行内字符级 LCS / 前后缀高亮
   - 协议转换智能折叠 (V3/FX/SenseNova)
   - 侧边对比 (Split) 与统一对比 (Unified) 渲染
   ============================================================ */

import { escapeHtml } from './utils.js';

export function tryParseJSON(raw) {
  if (!raw || !raw.trim()) return { ok: false };
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

export function isExpectedFXChange(row) {
  const exp = row.explanation || '';
  return exp.includes('V3') || exp.includes('已转 header') || exp.includes('FX') || exp.includes('prompt 不承载') || exp.includes('已按 V3 重写');
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

  // Vercel FX V3 transformations
  if (kind === 'deleted' && path.includes('reasoning')) {
    return '删除 reasoning 历史：V3 prompt 规范不承载历史 reasoning（已保留顶层 reasoning.effort）';
  }
  if (kind === 'deleted' && (path === '$.store' || path === '$.include' || path.includes('prompt_cache_key'))) {
    return `字段重写 ${path}：已转为 X-Session-Id 或上游不接收，属预期协议转换`;
  }
  if (kind === 'deleted' && path.startsWith('$.input[')) {
    return `字段转换 ${path}：Responses input 已重写为 V3 prompt 格式`;
  }
  if (kind === 'added' && path.startsWith('$.prompt[')) {
    return `生成字段 ${path}：由 Responses input 转换生成 V3 prompt`;
  }

  if (path.includes('.tools[') && (path.includes('.parameters') || path.includes('.inputSchema'))) {
    if (kind === 'deleted' && path.includes('.parameters')) {
      return `字段重命名 ${path}：V3 将 parameters 转换为 inputSchema`;
    }
    if (kind === 'added' && path.includes('.inputSchema')) {
      return `生成字段 ${path}：由 parameters 重命名为 inputSchema`;
    }
    return `tools 结构调整：${path}`;
  }

  if (kind === 'added') return `新增字段 ${path}`;
  if (kind === 'deleted') return `删除字段 ${path}`;
  return `修改字段 ${path}`;
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
      rows.push({
        kind: 'added',
        path,
        after: afterVal,
        explanation,
        expected: explanation.includes('V3') || explanation.includes('FX') || explanation.includes('已转')
      });
    } else if (!hasAfter) {
      const beforeVal = formatDiffValue(beforeMap.get(path));
      const explanation = explainJSONChange(path, 'deleted', beforeVal, '', category);
      rows.push({
        kind: 'deleted',
        path,
        before: beforeVal,
        explanation,
        expected: explanation.includes('V3') || explanation.includes('FX') || explanation.includes('已转') || explanation.includes('prompt 不承载')
      });
    } else if (JSON.stringify(beforeMap.get(path)) !== JSON.stringify(afterMap.get(path))) {
      const beforeValue = formatDiffValue(beforeMap.get(path));
      const afterValue = formatDiffValue(afterMap.get(path));
      const explanation = explainJSONChange(path, 'modified', beforeValue, afterValue, category);
      rows.push({
        kind: 'modified',
        path,
        before: beforeValue,
        after: afterValue,
        explanation,
        expected: false
      });
    }
  });

  return mergeRenameRows(rows);
}

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

    const sameValue = delRow.before === addRow.after;
    merged.push({
      kind: sameValue ? 'moved' : 'modified',
      path: `${delPath} → ${addPath}`,
      before: delRow.before,
      after: addRow.after,
      explanation: sameValue
        ? `V3 重命名：parameters → inputSchema（值保持一致，属预期转换）`
        : `V3 重命名并修改：parameters → inputSchema`,
      expected: true
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
  return {
    rows: [{
      kind: 'modified',
      path,
      before: String(before),
      after: String(after),
      explanation: `${path} 从 ${before} 变为 ${after}`,
      expected: false
    }]
  };
}

export function inlineHighlight(before, after) {
  before = String(before || '');
  after = String(after || '');
  if (before === after) return { beforeHTML: escapeHtml(before), afterHTML: escapeHtml(after) };

  // Short strings (<800 chars): LCS diff
  if (Math.max(before.length, after.length) < 800) {
    return charLcsHighlight(before, after);
  }
  return prefixSuffixHighlight(before, after, 80);
}

function prefixSuffixHighlight(before, after, context = 64) {
  let pre = 0;
  const minLen = Math.min(before.length, after.length);
  while (pre < minLen && before[pre] === after[pre]) pre++;

  let suf = 0;
  while (suf < minLen - pre && before[before.length - 1 - suf] === after[after.length - 1 - suf]) suf++;

  if (pre === before.length && pre === after.length) {
    return { beforeHTML: escapeHtml(before), afterHTML: escapeHtml(after) };
  }

  const beforeMid = before.slice(pre, before.length - suf);
  const afterMid = after.slice(pre, after.length - suf);
  const beforePre = before.slice(0, pre);
  const beforeSuf = before.slice(before.length - suf);
  const afterPre = after.slice(0, pre);
  const afterSuf = after.slice(after.length - suf);

  let bHtml = '';
  if (beforePre.length > context) bHtml += '…' + escapeHtml(beforePre.slice(-context));
  else bHtml += escapeHtml(beforePre);
  if (beforeMid) bHtml += `<span class="diff-char-del">${escapeHtml(beforeMid)}</span>`;
  if (beforeSuf.length > context) bHtml += escapeHtml(beforeSuf.slice(0, context)) + '…';
  else bHtml += escapeHtml(beforeSuf);

  let aHtml = '';
  if (afterPre.length > context) aHtml += '…' + escapeHtml(afterPre.slice(-context));
  else aHtml += escapeHtml(afterPre);
  if (afterMid) aHtml += `<span class="diff-char-add">${escapeHtml(afterMid)}</span>`;
  if (afterSuf.length > context) aHtml += escapeHtml(afterSuf.slice(0, context)) + '…';
  else aHtml += escapeHtml(afterSuf);

  return { beforeHTML: bHtml, afterHTML: aHtml };
}

function charLcsHighlight(before, after) {
  const n = before.length, m = after.length;
  if (n * m > 1200000 || n > 2500 || m > 2500) {
    return prefixSuffixHighlight(before, after, 80);
  }

  const table = Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      table[i][j] = before[i] === after[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }

  let i = 0, j = 0;
  let bHTML = '', aHTML = '';
  while (i < n && j < m) {
    if (before[i] === after[j]) {
      bHTML += escapeHtml(before[i]);
      aHTML += escapeHtml(after[j]);
      i++; j++;
    } else if (table[i + 1][j] >= table[i][j + 1]) {
      bHTML += `<span class="diff-char-del">${escapeHtml(before[i])}</span>`;
      i++;
    } else {
      aHTML += `<span class="diff-char-add">${escapeHtml(after[j])}</span>`;
      j++;
    }
  }
  while (i < n) bHTML += `<span class="diff-char-del">${escapeHtml(before[i++])}</span>`;
  while (j < m) aHTML += `<span class="diff-char-add">${escapeHtml(after[j++])}</span>`;

  return { beforeHTML: bHTML, afterHTML: aHTML };
}

export function buildTextDiff(before, after) {
  const oldLines = before.split(/\r?\n/);
  const newLines = after.split(/\r?\n/);

  if (oldLines.length === 1 && newLines.length === 1 && Math.max(before.length, after.length) > 200) {
    return [{
      kind: 'modified',
      path: '文本',
      before,
      after,
      explanation: '单行文本字符差异（已高亮修改处）'
    }];
  }

  const maxCells = 1600000;
  if (oldLines.length * newLines.length > maxCells) {
    return [{
      kind: 'modified',
      path: '文本',
      before: before.slice(0, 4000) + '\n… (已截断)',
      after: after.slice(0, 4000) + '\n… (已截断)',
      explanation: '文本体积过大，已折叠预览'
    }];
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
    if (oldLines[i] === newLines[j]) {
      operations.push({ kind: 'same', text: oldLines[i] });
      i++; j++;
    } else if (table[i + 1][j] >= table[i][j + 1]) {
      operations.push({ kind: 'deleted', text: oldLines[i++] });
    } else {
      operations.push({ kind: 'added', text: newLines[j++] });
    }
  }
  while (i < oldLines.length) operations.push({ kind: 'deleted', text: oldLines[i++] });
  while (j < newLines.length) operations.push({ kind: 'added', text: newLines[j++] });

  const rows = [];
  for (let idx = 0; idx < operations.length; idx++) {
    const cur = operations[idx];
    const next = operations[idx + 1];
    if (cur.kind === 'deleted' && next && next.kind === 'added') {
      rows.push({
        kind: 'modified',
        path: `第 ${idx + 1} 行`,
        before: cur.text,
        after: next.text,
        explanation: `修改第 ${idx + 1} 行`,
        expected: false
      });
      idx++;
    } else if (cur.kind === 'deleted') {
      rows.push({ kind: 'deleted', path: '文本行', before: cur.text, explanation: '删除文本行', expected: false });
    } else if (cur.kind === 'added') {
      rows.push({ kind: 'added', path: '文本行', after: cur.text, explanation: '新增文本行', expected: false });
    } else {
      rows.push({ kind: 'same', path: '', before: cur.text, after: cur.text, expected: false });
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
  if (beforeJSON.ok && afterJSON.ok) {
    return { rows: buildJSONDiff(beforeJSON.value, afterJSON.value, category) };
  }
  return { rows: buildTextDiff(before, after) };
}

export function diffStats(diffs) {
  return diffs.filter(Boolean).reduce((total, diff) => {
    for (const row of diff.rows) {
      if (row.expected) continue;
      if (row.kind === 'added') total.added++;
      if (row.kind === 'modified' || row.kind === 'moved') total.modified++;
      if (row.kind === 'deleted') total.deleted++;
      if (row.kind !== 'same') total.total++;
    }
    return total;
  }, { added: 0, modified: 0, deleted: 0, total: 0 });
}

export function diffSummaryBadge(label, count, kind) {
  return count ? `<span class="diff-summary-badge diff-${kind}">${label} <strong>${count}</strong></span>` : '';
}

export function renderDiffSection(diff, title, subtitle) {
  const changedRows = diff.rows.filter(row => row.kind !== 'same');
  const unexpected = changedRows.filter(r => !r.expected);
  const expected = changedRows.filter(r => r.expected);

  if (!changedRows.length) {
    return `
      <section class="diff-section diff-section-empty">
        <div class="diff-section-head">
          <div>
            <strong>${escapeHtml(title)}</strong>
            <span class="diff-subtitle">${escapeHtml(subtitle)}</span>
          </div>
          <span class="diff-no-change-badge">✓ 无变化</span>
        </div>
      </section>
    `;
  }

  if (!unexpected.length && expected.length) {
    return `
      <section class="diff-section diff-section-expected">
        <div class="diff-section-head">
          <div>
            <strong>${escapeHtml(title)}</strong>
            <span class="diff-subtitle">${escapeHtml(subtitle)}</span>
          </div>
          <span class="diff-expected-badge">仅含 ${expected.length} 处预期协议转换</span>
        </div>
        <details class="diff-expected-details">
          <summary>展开查看 ${expected.length} 条预期改写细节</summary>
          <div class="diff-rows">${renderDiffRows(expected)}</div>
        </details>
      </section>
    `;
  }

  return `
    <section class="diff-section">
      <div class="diff-section-head">
        <div>
          <strong>${escapeHtml(title)}</strong>
          <span class="diff-subtitle">${escapeHtml(subtitle)}</span>
        </div>
        <span class="diff-change-count-badge">${unexpected.length} 处变更 ${expected.length ? `(+${expected.length} 预期折叠)` : ''}</span>
      </div>
      <div class="diff-column-head">
        <span>原始客户端报文</span>
        <span>代理上游转发报文</span>
      </div>
      <div class="diff-rows">
        ${renderDiffRows(unexpected)}
        ${expected.length ? `
          <details class="diff-expected-details">
            <summary>展开 ${expected.length} 条预期协议改写 (FX/V3)</summary>
            <div class="diff-rows">${renderDiffRows(expected)}</div>
          </details>
        ` : ''}
      </div>
    </section>
  `;
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
      if (!collapsed) output.push('<div class="diff-collapse-banner">… 未修改内容已折叠 …</div>');
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
  return `${row.path} = ${value}`;
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
    if (row.kind === 'moved') {
      const parts = row.path.split(' → ');
      oldHTML = escapeHtml(diffLineText({ path: parts[0], kind: 'modified' }, row.before || ''));
      newHTML = escapeHtml(diffLineText({ path: parts[1] || row.path, kind: 'modified' }, row.after || ''));
      if (row.before === row.after) {
        oldHTML += ' <span class="diff-note-tag">(位置重排)</span>';
        newHTML += ' <span class="diff-note-tag">(位置重排)</span>';
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

  const oldClass = (row.kind === 'deleted' || isMod) ? 'diff-line-del' : 'diff-line-empty';
  const newClass = (row.kind === 'added' || isMod) ? 'diff-line-add' : 'diff-line-empty';
  const marker = row.kind === 'added' ? '+' : row.kind === 'deleted' ? '−' : row.kind === 'moved' ? '↔' : isMod ? '±' : ' ';
  const expClass = row.expected ? ' is-expected' : '';

  return `
    <div class="diff-row diff-${row.kind}${expClass}">
      <div class="diff-line ${oldClass}">
        <span class="diff-marker">${row.kind === 'added' ? '' : marker}</span>
        <code>${isMod ? oldHTML : escapeHtml(oldLine)}</code>
      </div>
      <div class="diff-line ${newClass}">
        <span class="diff-marker">${row.kind === 'deleted' ? '' : marker}</span>
        <code>${isMod ? newHTML : escapeHtml(newLine)}</code>
      </div>
      ${row.kind === 'same' ? '' : `
        <div class="diff-explanation">
          <span class="diff-exp-dot"></span>
          <span>${escapeHtml(row.explanation || '')}</span>
          ${row.path ? `<code>${escapeHtml(row.path)}</code>` : ''}
        </div>
      `}
    </div>
  `;
}
