'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import {
  $, $all, fmtNum, fmtPct, fmtMs, fmtTimeShort,
  escapeHtml, rangeToFrom, rangeLabel, latencyThresholds, fmtCompact,
  gatewayPrefixLabel, gatewayTone
} from './utils.js';
import { loadGatewayPulses } from './pulse.js';
import { openDrawer } from './drawer.js';
import { showToast } from './ui.js';

const WINDOW_LIMIT = 100;
const ATTENTION_ROWS = 8;
const RECENT_ROWS = 12;

function windowQuery(limit) {
  const params = new URLSearchParams({ limit: String(limit || WINDOW_LIMIT) });
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  return params.toString();
}

export async function loadOverview() {
  await Promise.all([loadMetrics(), loadWindow(), loadGatewayPulses()]);
  const label = rangeLabel(state.range);
  for (const sel of ['#pulseRange', '#attentionRange']) {
    const el = $(sel);
    if (el) el.textContent = label;
  }
}

/* ---------------- 1. 指标条 ---------------- */

async function loadMetrics() {
  try {
    const m = await api('/metrics?' + windowQuery());
    const sig = JSON.stringify([
      m.requests, m.successes, m.failures, m.input_tokens, m.output_tokens,
      m.reasoning_tokens, m.cache_hit_rate, m.cache_coverage_percent,
      m.cache_read_tokens, m.cache_write_tokens, m.by_gateway
    ]);
    if (sig === state.metricsSig && state.metrics) return;
    state.metricsSig = sig;
    state.metrics = m;
    renderMetrics(m);
  } catch (e) {
    state.metricsSig = '';
    showToast('加载指标失败: ' + e.message, 'error');
    renderMetrics(null);
  }
}

function setMetric(id, value, note, tone) {
  const el = document.getElementById(id);
  if (el) {
    el.textContent = value;
    el.classList.toggle('is-warn', tone === 'warn');
    el.classList.toggle('is-err', tone === 'err');
  }
  const sub = document.getElementById(id + 'Sub');
  if (sub) sub.textContent = note;
}

function renderMetrics(m) {
  const keys = ['Requests', 'Success', 'CacheHit', 'CacheCoverage', 'Tokens', 'Reasoning'];
  if (!m) {
    keys.forEach(k => setMetric('stat' + k, '—', '加载失败'));
    renderGatewayCacheRates(null);
    return;
  }
  setMetric('statRequests', fmtNum(m.requests),
    m.failures > 0 ? `${fmtNum(m.failures)} 次失败` : '全部成功',
    m.failures > 0 ? 'err' : null);
  setMetric('statSuccess', m.requests > 0 ? fmtPct((m.successes / m.requests) * 100) : '—',
    `${fmtNum(m.successes)} / ${fmtNum(m.requests)} 成功`);
  setMetric('statCacheHit', m.cache_hit_rate == null ? '—' : fmtPct(m.cache_hit_rate),
    `${fmtCompact(m.cache_read_tokens)} 命中 Token`);
  setMetric('statCacheCoverage', m.cache_coverage_percent == null ? '—' : fmtPct(m.cache_coverage_percent),
    `${fmtNum(m.cache_supported_calls)} / ${fmtNum(m.usage_calls)} 次支持缓存`);
  setMetric('statTokens', fmtCompact(m.prompt_tokens || m.input_tokens || 0),
    `输出 ${fmtCompact(m.output_tokens)} · 未命中 ${fmtCompact(m.input_tokens)}`);
  setMetric('statReasoning', fmtCompact(m.reasoning_tokens),
    `缓存写入 ${fmtCompact(m.cache_write_tokens)} Token`);

  renderGatewayCacheRates(m);
}

/* ---------------- 2. 异常优先 ---------------- */

async function loadWindow() {
  let items;
  try {
    const data = await api('/logs?' + windowQuery());
    items = data.items || [];
  } catch (e) {
    renderAttention(null);
    renderRecent(null);
    return;
  }
  state.recentLogs = items;
  renderAttention(items);
  renderRecent(items.slice(0, RECENT_ROWS));
}

function attentionItems(items, thresholds) {
  const rows = [];
  for (const l of items.filter(l => !l.success)) {
    rows.push({
      kind: 'err',
      title: `HTTP ${l.status_code || '—'} · ${l.gateway_name || l.gateway_id || '未知网关'}`,
      detail: l.error || l.model || '请求失败',
      logId: l.id
    });
  }
  if (thresholds.relative) {
    for (const l of items.filter(l => l.success && (l.duration_ms || 0) > thresholds.slow)) {
      rows.push({
        kind: 'warn',
        title: `${fmtMs(l.duration_ms)} · ${l.gateway_name || l.gateway_id || '未知网关'}`,
        detail: `超出本窗口 p90（${fmtMs(thresholds.slow)}）· ${l.model || '未知模型'}`,
        logId: l.id
      });
    }
  }
  const upstreams = (state.health && state.health.upstreams) || {};
  for (const [id, h] of Object.entries(upstreams)) {
    const gateway = state.gateways[id];
    if (h.reachable || !gateway || !gateway.enabled) continue;
    rows.push({
      kind: 'err',
      title: `不可达 · ${gateway.name || id}`,
      detail: h.error || '上游未响应',
      gatewayId: id
    });
  }
  return rows;
}

function renderAttention(items) {
  const container = $('#attentionList');
  if (!container) return;

  if (items === null) {
    container.innerHTML = '<div class="attention-empty is-err">日志窗口读取失败，指标条数字可能同样过期</div>';
    return;
  }
  const rows = attentionItems(items, latencyThresholds(items));
  if (!rows.length) {
    const scope = state.range === 'all' ? '全部历史' : `近 ${rangeLabel(state.range)}`;
    container.innerHTML = `<div class="attention-empty">${escapeHtml(scope)}无失败、无明显慢请求、上游均可达</div>`;
    return;
  }

  container.innerHTML = rows.slice(0, ATTENTION_ROWS).map(r => `
    <div class="attention-row is-${r.kind}"${r.logId ? ` data-log-id="${escapeHtml(r.logId)}"` : ''}${r.gatewayId ? ` data-gateway="${escapeHtml(r.gatewayId)}"` : ''} role="button" tabindex="0">
      <span class="attention-mark"></span>
      <span class="attention-title">${escapeHtml(r.title)}</span>
      <span class="attention-detail">${escapeHtml(r.detail)}</span>
      <span class="attention-go" aria-hidden="true">→</span>
    </div>
  `).join('') + (rows.length > ATTENTION_ROWS
    ? `<div class="attention-more">另有 ${rows.length - ATTENTION_ROWS} 条同类未列出 · <button type="button" class="btn-link" id="attentionSeeAll">在日志中查看全部</button></div>`
    : '');

  $all('.attention-row', container).forEach(row => {
    const open = () => {
      if (row.dataset.logId) {
        openDrawer(row.dataset.logId);
        return;
      }
      const select = $('#filterGateway');
      if (select && row.dataset.gateway) select.value = row.dataset.gateway;
      const nav = $('[data-view="logs"]');
      if (nav) nav.click();
    };
    row.addEventListener('click', open);
    row.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        open();
      }
    });
  });

  const seeAll = $('#attentionSeeAll', container);
  if (seeAll) {
    seeAll.addEventListener('click', () => {
      const status = $('#filterStatus');
      if (status) status.value = 'failure';
      const nav = $('[data-view="logs"]');
      if (nav) nav.click();
    });
  }
}

/* ---------------- 3. 网关缓存效率 ---------------- */

function renderGatewayCacheRates(metrics) {
  const container = $('#gatewayCacheRates');
  if (!container) return;
  const byGateway = (metrics && metrics.by_gateway) || {};

  container.innerHTML = gatewayIds().map(id => {
    const gateway = state.gateways[id] || {};
    const metric = byGateway[id];
    const valid = metric && metric.cache_hit_rate !== null && metric.cache_hit_rate !== undefined;
    const rate = valid ? Number(metric.cache_hit_rate) : 0;
    const width = Math.max(0, Math.min(100, rate));

    let detail = '暂无请求数据';
    if (valid) {
      detail = `${fmtNum(metric.cache_read_tokens)} 命中 Token · ${fmtNum(metric.cache_supported_calls)} 次支持`;
    } else if (metric) {
      detail = `${fmtNum(metric.usage_calls)} 次调用 · 协议不支持缓存`;
    }

    return `
      <div class="gateway-rate-row" data-gwid="${escapeHtml(id)}">
        <span class="gw-dot gw-tone-${gatewayTone(id)}"></span>
        <span class="gateway-rate-name">${escapeHtml(gateway.name || id)}</span>
        <code class="gateway-rate-prefix">${escapeHtml(gatewayPrefixLabel(gateway, id))}</code>
        <span class="gateway-rate-bar"><span style="width:${width}%"></span></span>
        <span class="gateway-rate-value">${valid ? fmtPct(rate) : '—'}</span>
        <span class="gateway-rate-detail">${escapeHtml(detail)}</span>
        <button type="button" class="btn-link gw-filter-jump" data-gw="${escapeHtml(id)}">日志</button>
      </div>
    `;
  }).join('');

  $all('.gw-filter-jump', container).forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation();
      const select = $('#filterGateway');
      if (select) select.value = btn.dataset.gw;
      const nav = $('[data-view="logs"]');
      if (nav) nav.click();
    });
  });
}

/* ---------------- 4. 最近请求 ---------------- */

function renderRecent(items) {
  const container = $('#recentLogList');
  if (!container) return;

  if (items === null) {
    container.innerHTML = '<div class="empty-state"><span class="text-danger">加载失败</span></div>';
    return;
  }
  const sig = items.map(l => l.id + ':' + (l.success ? 1 : 0) + ':' + (l.duration_ms || 0)).join(',');
  if (sig === state.recentSig && container.children.length) return;
  state.recentSig = sig;

  if (!items.length) {
    container.innerHTML = '<div class="empty-state"><span>暂无请求记录，等待流量接入…</span></div>';
    return;
  }

  container.innerHTML = items.map(l => `
    <div class="recent-log-item" data-id="${escapeHtml(l.id)}" role="button" tabindex="0" aria-label="查看请求 ${escapeHtml(l.id)} 详情">
      <span class="rli-status-dot ${l.success ? 'is-ok' : 'is-err'}"></span>
      <span class="rli-gw">${escapeHtml(l.gateway_name || l.gateway_id || '未知')}</span>
      <span class="rli-model" title="${escapeHtml(l.model || '')}">${escapeHtml(l.model || '(未知模型)')}</span>
      <span class="rli-meta">
        <span class="rli-latency">${fmtMs(l.duration_ms)}</span>
        <span class="rli-time">${l.status_code || ''}${l.stream ? ' · SSE' : ''}</span>
        <span class="rli-time" title="${escapeHtml(l.started_at || '')}">${fmtTimeShort(l.started_at)}</span>
      </span>
      <span class="rli-arrow" aria-hidden="true">→</span>
    </div>
  `).join('');

  $all('.recent-log-item', container).forEach(el => {
    const open = () => openDrawer(el.dataset.id);
    el.addEventListener('click', open);
    el.addEventListener('keydown', e => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        open();
      }
    });
  });
}

/* ---------------- 5. 初始化 ---------------- */

export function initOverview() {
  const picker = $('#rangePicker');
  if (picker) {
    picker.addEventListener('click', e => {
      const btn = e.target.closest('button[data-range]');
      if (!btn) return;
      state.range = btn.dataset.range;
      $all('#rangePicker button').forEach(b => {
        const active = b === btn;
        b.classList.toggle('is-active', active);
        b.setAttribute('aria-pressed', String(active));
      });
      loadOverview();
    });
  }
}
