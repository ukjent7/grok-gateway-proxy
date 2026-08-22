'use strict';

/* ============================================================
   Grok Gateway Console · 总览视图
   - 核心指标卡片 & 环形进度仪表
   - 平滑曲线 Sparklines
   - 网关缓存命中率分解矩阵
   - 实时信号与最新请求流
   ============================================================ */

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import {
  $, $all, fmtNum, fmtPct, fmtMs, fmtTime, fmtTimeShort, fmtTimeRelative,
  escapeHtml, rangeToFrom, rangeLabel
} from './utils.js';
import { loadGatewayPulses } from './pulse.js';
import { openDrawer } from './drawer.js';
import { showToast } from './ui.js';

function metricsQuery(extra) {
  const params = new URLSearchParams();
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  if (extra) Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

export async function loadOverview() {
  await Promise.all([
    loadMetrics(),
    loadSparkSeries(),
    loadGatewayPulses(),
    loadRecentLogs()
  ]);
  const rangeEl = $('#pulseRange');
  if (rangeEl) rangeEl.textContent = rangeLabel(state.range);
}

export async function loadMetrics() {
  try {
    const m = await api('/metrics?' + metricsQuery());
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

function setGauge(sel, pct, colorVar = '--teal') {
  const el = document.querySelector(sel);
  if (!el) return;
  const valid = pct !== null && pct !== undefined && !Number.isNaN(Number(pct));
  const v = valid ? Math.max(0, Math.min(100, Number(pct))) : 0;
  
  // Radial SVG Gauge
  const radius = 18;
  const circumference = 2 * Math.PI * radius;
  const offset = circumference - (v / 100) * circumference;

  el.innerHTML = `
    <svg viewBox="0 0 44 44" width="44" height="44" class="gauge-svg">
      <circle cx="22" cy="22" r="${radius}" class="gauge-bg"/>
      <circle cx="22" cy="22" r="${radius}" class="gauge-val" style="stroke-dasharray:${circumference};stroke-dashoffset:${offset};stroke:${colorVar}"/>
    </svg>
  `;
}

function renderMetrics(m) {
  // 1. Requests
  $('#statRequests').textContent = m ? fmtNum(m.requests) : '—';
  const reqSub = $('#statRequestsSub');
  if (reqSub) {
    if (!m) {
      reqSub.innerHTML = '<span class="muted">加载失败</span>';
    } else if (m.failures > 0) {
      reqSub.innerHTML = `<span class="stat-badge-err">${fmtNum(m.failures)} 次异常</span> · 成功率 ${fmtPct((m.successes / Math.max(1, m.requests)) * 100)}`;
    } else {
      reqSub.innerHTML = '<span class="stat-badge-ok">全部正常</span> · 0 次异常';
    }
  }

  // 2. Success Rate
  const successRate = m && m.requests > 0 ? (m.successes / m.requests) * 100 : null;
  $('#statSuccess').textContent = successRate === null ? '—' : fmtPct(successRate);
  $('#statSuccessSub').textContent = m ? `${fmtNum(m.successes)} 成功 / ${fmtNum(m.requests)} 总量` : '—';
  setGauge('#gaugeSuccess', successRate, 'var(--emerald)');

  // 3. Cache Hit Rate
  const cacheHit = (!m || m.cache_hit_rate === null || m.cache_hit_rate === undefined) ? null : m.cache_hit_rate;
  $('#statCacheHit').textContent = cacheHit === null ? '—' : fmtPct(cacheHit);
  $('#statCacheHitSub').textContent = m ? `${fmtNum(m.cache_read_tokens)} Cached Tokens` : '—';
  setGauge('#gaugeCacheHit', cacheHit, 'var(--sky)');

  // 4. Cache Coverage
  const cacheCoverage = (!m || m.cache_coverage_percent === null || m.cache_coverage_percent === undefined) ? null : m.cache_coverage_percent;
  $('#statCacheCoverage').textContent = cacheCoverage === null ? '—' : fmtPct(cacheCoverage);
  $('#statCacheCoverageSub').textContent = m ? `${fmtNum(m.cache_supported_calls)} / ${fmtNum(m.usage_calls)} 支持调用` : '—';
  setGauge('#gaugeCoverage', cacheCoverage, 'var(--violet)');

  // 5. Tokens Throughput
  const totalPromptTokens = m ? (m.prompt_tokens || m.input_tokens || 0) : 0;
  const totalOutputTokens = m ? (m.output_tokens || 0) : 0;
  $('#statTokens').textContent = m ? `${fmtNum(totalPromptTokens)} / ${fmtNum(totalOutputTokens)}` : '—';
  $('#statTokensSub').textContent = m
    ? `未命中 ${fmtNum(m.input_tokens)} · 命中 ${fmtNum(m.cache_read_tokens)}`
    : '—';

  // 6. Reasoning Tokens
  $('#statReasoning').textContent = m ? fmtNum(m.reasoning_tokens) : '—';
  $('#statReasoningSub').textContent = m
    ? `写入缓存 ${fmtNum(m.cache_write_tokens)} tok`
    : '—';

  renderGatewayCacheRates(m);
}

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
    const value = valid ? fmtPct(rate) : '—';

    let detail = '暂无请求数据';
    if (valid) {
      detail = `${fmtNum(metric.cache_read_tokens)} 命中 Token · ${fmtNum(metric.cache_supported_calls)} 次支持`;
    } else if (metric) {
      detail = `${fmtNum(metric.usage_calls)} 次调用 · 协议不支持缓存`;
    }

    return `
      <article class="gateway-cache-rate" data-gwid="${escapeHtml(id)}">
        <div class="gateway-cache-rate-head">
          <div class="gateway-title-group">
            <span class="gw-dot gw-dot-${escapeHtml(id)}"></span>
            <strong>${escapeHtml(gateway.name || id)}</strong>
            <code>${escapeHtml(gateway.prefix || id)}</code>
          </div>
          <div class="gateway-cache-rate-value">${value}</div>
        </div>
        <div class="gateway-cache-rate-bar" role="progressbar" aria-valuenow="${width}" aria-valuemin="0" aria-valuemax="100">
          <span style="width:${width}%" class="bar-fill-${escapeHtml(id)}"></span>
        </div>
        <div class="gateway-cache-rate-foot">
          <span class="gateway-cache-rate-detail">${escapeHtml(detail)}</span>
          <button type="button" class="btn-link gw-filter-jump" data-gw="${escapeHtml(id)}" title="在日志中筛选此网关">
            查看日志 →
          </button>
        </div>
      </article>
    `;
  }).join('');

  $all('.gw-filter-jump', container).forEach(btn => {
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      const gw = btn.dataset.gw;
      const navLogs = $(`[data-view="logs"]`);
      const selGw = $('#filterGateway');
      if (selGw) selGw.value = gw;
      if (navLogs) navLogs.click();
    });
  });
}

/* ---------------- Sparklines (平滑 Bezier 曲线) ---------------- */

export async function loadSparkSeries() {
  try {
    const params = new URLSearchParams({ limit: '40' });
    const from = rangeToFrom(state.range);
    if (from) params.set('from', from.toISOString());
    const data = await api('/logs?' + params.toString());
    const items = data.items || [];
    const sig = items.map(l => l.id + ':' + (l.success ? 1 : 0) + ':' + (l.duration_ms || 0)).join(',');
    if (sig === state.sparkSig && state.sparkSeries.length) return;
    state.sparkSig = sig;
    state.sparkSeries = items.slice().reverse();
    renderSparklines();
  } catch (e) {
    /* ignore sparkline errors */
  }
}

function sparklineSmooth(svg, values, color = '#06b6d4') {
  const W = 120, H = 32, pad = 3;
  if (!values || !values.length) {
    svg.innerHTML = '';
    return;
  }

  const max = Math.max(...values);
  const min = Math.min(...values);
  const range = (max - min) || 1;
  const n = values.length;

  const points = values.map((v, i) => {
    const x = n === 1 ? W / 2 : (i / (n - 1)) * (W - 2 * pad) + pad;
    const y = H - pad - ((v - min) / range) * (H - 2 * pad);
    return [x, y];
  });

  // Bezier smoothing
  let linePath = `M ${points[0][0].toFixed(1)} ${points[0][1].toFixed(1)}`;
  for (let i = 0; i < points.length - 1; i++) {
    const p0 = points[i];
    const p1 = points[i + 1];
    const midX = (p0[0] + p1[0]) / 2;
    linePath += ` C ${midX.toFixed(1)} ${p0[1].toFixed(1)}, ${midX.toFixed(1)} ${p1[1].toFixed(1)}, ${p1[0].toFixed(1)} ${p1[1].toFixed(1)}`;
  }

  const lastPoint = points[points.length - 1];
  const firstPoint = points[0];
  const areaPath = `${linePath} L ${lastPoint[0].toFixed(1)} ${H} L ${firstPoint[0].toFixed(1)} ${H} Z`;
  const gid = 'spark-grad-' + Math.random().toString(36).slice(2, 8);

  svg.innerHTML = `
    <defs>
      <linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="${color}" stop-opacity="0.32"/>
        <stop offset="100%" stop-color="${color}" stop-opacity="0.0"/>
      </linearGradient>
    </defs>
    <path class="spark-fill" d="${areaPath}" fill="url(#${gid})" stroke="none"/>
    <path class="spark-line" d="${linePath}" fill="none" stroke="${color}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>
  `;
}

function requestHistogram(items) {
  const BUCKETS = 24;
  const times = items.map(l => new Date(l.started_at).getTime()).filter(t => !Number.isNaN(t));
  if (times.length < 2) return items.map(() => 1);
  const min = Math.min(...times);
  const max = Math.max(...times);
  const span = Math.max(max - min, 1);
  const buckets = new Array(BUCKETS).fill(0);
  for (const t of times) {
    const idx = Math.min(BUCKETS - 1, Math.floor(((t - min) / span) * BUCKETS));
    buckets[idx] += 1;
  }
  return buckets;
}

function renderSparklines() {
  const logs = state.sparkSeries;
  const map = {
    requests: { data: requestHistogram(logs), color: 'var(--teal)' },
    success: { data: logs.map(l => (l.success ? 1 : 0)), color: 'var(--emerald)' },
    cacheHit: {
      data: logs.map(l => {
        if (!l.usage || !l.usage.cache_supported || !l.usage.prompt_tokens) return 0;
        return (l.usage.cache_read_tokens / l.usage.prompt_tokens) * 100;
      }),
      color: 'var(--sky)'
    },
    cacheCoverage: {
      data: logs.map(l => (l.usage && l.usage.cache_supported ? 1 : 0)),
      color: 'var(--violet)'
    },
    tokens: {
      data: logs.map(l => {
        if (!l.usage) return 0;
        return (l.usage.prompt_tokens || l.usage.input_tokens || 0) + (l.usage.output_tokens || 0);
      }),
      color: 'var(--amber)'
    },
    reasoning: {
      data: logs.map(l => (l.usage && l.usage.reasoning_tokens) || 0),
      color: 'var(--rose)'
    }
  };

  $all('.stat-spark').forEach(svg => {
    const key = svg.dataset.stat;
    if (map[key]) {
      sparklineSmooth(svg, map[key].data, map[key].color);
    }
  });
}

/* ---------------- 最近请求列表 ---------------- */

const EMPTY_RECENT_SVG = `
  <svg viewBox="0 0 64 64" width="44" height="44" fill="none" class="empty-icon">
    <circle cx="32" cy="32" r="28" stroke="currentColor" stroke-width="1.5" stroke-dasharray="3 4" opacity="0.4"/>
    <path d="M22 34l7 7 15-16" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" opacity="0.6"/>
  </svg>
`;

export async function loadRecentLogs() {
  const container = $('#recentLogList');
  if (!container) return;

  try {
    const params = new URLSearchParams({ limit: '12' });
    const from = rangeToFrom(state.range);
    if (from) params.set('from', from.toISOString());
    const data = await api('/logs?' + params.toString());
    const items = data.items || [];
    const sig = items.map(l => l.id + ':' + (l.success ? 1 : 0) + ':' + (l.duration_ms || 0)).join(',');

    if (sig === state.recentSig && container.children.length) return;
    state.recentSig = sig;
    state.recentLogs = items;

    if (!items.length) {
      container.innerHTML = `
        <div class="empty-state">
          ${EMPTY_RECENT_SVG}
          <span>暂无请求记录，等待流量接入…</span>
        </div>
      `;
      return;
    }

    container.innerHTML = items.map(l => {
      const gwId = l.gateway_id || 'unknown';
      const duration = l.duration_ms || 0;
      let latencyCls = 'latency-fast';
      if (duration > 1500) latencyCls = 'latency-slow';
      else if (duration > 500) latencyCls = 'latency-mid';

      return `
        <div class="recent-log-item" data-id="${escapeHtml(l.id)}" role="button" tabindex="0" aria-label="查看请求 ${escapeHtml(l.id)} 详情">
          <span class="rli-status-dot ${l.success ? 'is-ok' : 'is-err'}"></span>
          <span class="rli-gw gw-badge-${escapeHtml(gwId)}">${escapeHtml(l.gateway_name || gwId)}</span>
          <span class="rli-model" title="${escapeHtml(l.model || '')}">${escapeHtml(l.model || '(未知模型)')}</span>
          <span class="rli-meta">
            <span class="rli-latency ${latencyCls}">${fmtMs(l.duration_ms)}</span>
            <span class="rli-time" title="${fmtTime(l.started_at)}">${fmtTimeRelative(l.started_at)}</span>
          </span>
          <span class="rli-arrow">→</span>
        </div>
      `;
    }).join('');

    $all('.recent-log-item', container).forEach(el => {
      const open = () => openDrawer(el.dataset.id);
      el.addEventListener('click', open);
      el.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          open();
        }
      });
    });
  } catch (e) {
    container.innerHTML = `
      <div class="empty-state">
        <span class="text-danger">加载失败：${escapeHtml(e.message)}</span>
      </div>
    `;
  }
}

export function initOverview() {
  const picker = $('#rangePicker');
  if (picker) {
    picker.addEventListener('click', (e) => {
      const btn = e.target.closest('button[data-range]');
      if (!btn) return;
      state.range = btn.dataset.range;
      $all('#rangePicker button').forEach(b => b.classList.toggle('is-active', b === btn));
      const rangeEl = $('#pulseRange');
      if (rangeEl) rangeEl.textContent = rangeLabel(state.range);
      loadOverview();
    });
  }
}
