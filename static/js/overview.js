'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, $all, fmtNum, fmtPct, fmtMs, fmtTimeShort, escapeHtml, rangeToFrom, rangeLabel } from './utils.js';
import { loadGatewayPulses } from './pulse.js';
import { openDrawer } from './drawer.js';
import { showToast } from './ui.js';

/* ============================================================
   总览：指标 + 仪表 + 趋势线 + 最近请求
   ============================================================ */

function metricsQuery(extra) {
  const params = new URLSearchParams();
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  if (extra) Object.keys(extra).forEach(k => extra[k] && params.set(k, extra[k]));
  return params.toString();
}

export async function loadOverview() {
  await Promise.all([loadMetrics(), loadSparkSeries(), loadGatewayPulses(), loadRecentLogs()]);
  $('#pulseRange').textContent = rangeLabel(state.range);
}

export async function loadMetrics() {
  try {
    const m = await api('/metrics?' + metricsQuery());
    const sig = JSON.stringify([m.requests, m.successes, m.failures, m.input_tokens, m.output_tokens, m.reasoning_tokens, m.cache_hit_rate, m.cache_coverage_percent, m.cache_read_tokens, m.cache_write_tokens, m.by_gateway]);
    if (sig === state.metricsSig && state.metrics) return; // 数据未变，跳过重绘
    state.metricsSig = sig;
    state.metrics = m;
    renderMetrics(m);
  } catch (e) {
    state.metricsSig = '';
    showToast('加载指标失败: ' + e.message, 'error');
    renderMetrics(null);
  }
}

function setGauge(sel, pct) {
  const el = document.querySelector(sel);
  if (!el) return;
  const valid = pct !== null && pct !== undefined;
  const v = valid ? Math.max(0, Math.min(100, Number(pct))) : 0;
  el.style.setProperty('--p', String(v));
}

function renderMetrics(m) {
  $('#statRequests').textContent = m ? fmtNum(m.requests) : '—';
  $('#statRequestsSub').textContent = m ? (m.failures ? fmtNum(m.failures) + ' 次失败' : '无失败') : '加载失败';

  const successRate = m && m.requests > 0 ? (m.successes / m.requests) * 100 : null;
  $('#statSuccess').textContent = successRate === null ? '—' : fmtPct(successRate);
  $('#statSuccessSub').textContent = m ? fmtNum(m.successes) + ' / ' + fmtNum(m.requests) : '—';
  setGauge('#gaugeSuccess', successRate);

  $('#statCacheHit').textContent = (!m || m.cache_hit_rate === null || m.cache_hit_rate === undefined) ? '—' : fmtPct(m.cache_hit_rate);
  $('#statCacheHitSub').textContent = m ? fmtNum(m.cache_read_tokens) + ' 读取 tok' : '—';
  setGauge('#gaugeCacheHit', m ? m.cache_hit_rate : null);

  $('#statCacheCoverage').textContent = (!m || m.cache_coverage_percent === null || m.cache_coverage_percent === undefined) ? '—' : fmtPct(m.cache_coverage_percent);
  $('#statCacheCoverageSub').textContent = m ? fmtNum(m.cache_supported_calls) + ' / ' + fmtNum(m.usage_calls) + ' 次调用' : '—';
  setGauge('#gaugeCoverage', m ? m.cache_coverage_percent : null);
  renderGatewayCacheRates(m);

  $('#statTokens').textContent = m ? fmtNum(m.input_tokens) + ' / ' + fmtNum(m.output_tokens) : '—';
  $('#statTokensSub').textContent = m ? 'prompt ' + fmtNum(m.prompt_tokens) : '—';
  $('#statReasoning').textContent = m ? fmtNum(m.reasoning_tokens) : '—';
  $('#statReasoningSub').textContent = m ? 'cache write ' + fmtNum(m.cache_write_tokens) : '—';
}

function renderGatewayCacheRates(metrics) {
  const container = $('#gatewayCacheRates');
  if (!container) return;
  const byGateway = metrics && metrics.by_gateway ? metrics.by_gateway : {};
  container.innerHTML = gatewayIds().map(id => {
    const gateway = state.gateways[id] || {};
    const metric = byGateway[id];
    const valid = metric && metric.cache_hit_rate !== null && metric.cache_hit_rate !== undefined;
    const rate = valid ? Number(metric.cache_hit_rate) : 0;
    const width = Math.max(0, Math.min(100, rate));
    const value = valid ? fmtPct(rate) : '—';
    const detail = valid
      ? fmtNum(metric.cache_read_tokens) + ' 读取 tok · ' + fmtNum(metric.cache_supported_calls) + ' 次支持'
      : metric
        ? fmtNum(metric.usage_calls) + ' 次调用 · 暂无缓存字段'
        : '暂无请求数据';
    return (
      '<article class="gateway-cache-rate">' +
        '<div class="gateway-cache-rate-head">' +
          '<strong>' + escapeHtml(gateway.name || id) + '</strong>' +
          '<code>' + escapeHtml(gateway.prefix || id) + '</code>' +
        '</div>' +
        '<div class="gateway-cache-rate-value">' + value + '</div>' +
        '<div class="gateway-cache-rate-bar"><span style="width:' + width + '%"></span></div>' +
        '<div class="gateway-cache-rate-detail">' + escapeHtml(detail) + '</div>' +
      '</article>'
    );
  }).join('');
}

/* ---------------- Sparklines ---------------- */

export async function loadSparkSeries() {
  try {
    const data = await api('/logs?limit=30');
    const items = data.items || [];
    const sig = items.map(l => l.id + ':' + (l.success ? 1 : 0)).join(',');
    if (sig === state.sparkSig && state.sparkSeries.length) return; // 无新请求，跳过重绘
    state.sparkSig = sig;
    state.sparkSeries = items.slice().reverse();
    renderSparklines();
  } catch (e) { /* silent */ }
}

function sparkline(svg, values) {
  const W = 100, H = 28, pad = 3;
  if (!values.length) { svg.innerHTML = ''; return; }
  const max = Math.max.apply(null, values);
  const min = Math.min.apply(null, values);
  const range = (max - min) || 1;
  const n = values.length;
  const points = values.map((v, i) => {
    const x = n === 1 ? W / 2 : (i / (n - 1)) * (W - 2 * pad) + pad;
    const y = H - pad - ((v - min) / range) * (H - 2 * pad);
    return [x, y];
  });
  const linePath = 'M' + points.map(p => p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' L');
  const areaPath = linePath + ' L' + points[points.length - 1][0].toFixed(1) + ' ' + H + ' L' + points[0][0].toFixed(1) + ' ' + H + ' Z';
  const gid = 'spark-grad-' + Math.random().toString(36).slice(2, 8);
  svg.innerHTML =
    '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
    '<stop offset="0%" stop-color="#2dd4bf" stop-opacity="0.5"/>' +
    '<stop offset="100%" stop-color="#2dd4bf" stop-opacity="0"/>' +
    '</linearGradient></defs>' +
    '<path class="spark-area" d="' + areaPath + '"/>' +
    '<path class="spark-fill" d="' + areaPath + '" fill="url(#' + gid + ')" stroke="none"/>' +
    '<path class="spark-line" d="' + linePath + '"/>';
}

function renderSparklines() {
  const logs = state.sparkSeries;
  const map = {
    requests: logs.map((_, i) => i + 1),
    success: logs.map(l => l.success ? 1 : 0),
    cacheHit: logs.map(l => {
      if (!l.usage || !l.usage.cache_supported || !l.usage.prompt_tokens) return 0;
      return (l.usage.cache_read_tokens / l.usage.prompt_tokens) * 100;
    }),
    cacheCoverage: logs.map(l => (l.usage && l.usage.cache_supported) ? 1 : 0),
    tokens: logs.map(l => {
      if (!l.usage) return 0;
      return (l.usage.input_tokens || l.usage.prompt_tokens || 0) + (l.usage.output_tokens || 0);
    }),
    reasoning: logs.map(l => (l.usage && l.usage.reasoning_tokens) || 0)
  };
  $all('.stat-spark').forEach(svg => {
    const key = svg.dataset.stat;
    if (map[key]) sparkline(svg, map[key]);
  });
}

/* ---------------- 最近请求 ---------------- */
const EMPTY_RECENT_SVG = '<svg viewBox="0 0 60 60" width="48" height="48" fill="none"><circle cx="30" cy="30" r="26" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 4"/><path d="M20 32l5 5 15-15" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';

export async function loadRecentLogs() {
  const container = $('#recentLogList');
  if (!container) return;
  try {
    const data = await api('/logs?limit=12');
    const items = data.items || [];
    const sig = items.map(l => l.id + ':' + (l.success ? 1 : 0)).join(',');
    if (sig === state.recentSig && container.children.length) return; // 无新请求，跳过重绘
    state.recentSig = sig;
    state.recentLogs = items;
    if (!items.length) {
      container.innerHTML = '<div class="empty-state">' + EMPTY_RECENT_SVG + '<span>暂无请求记录，等待流量接入…</span></div>';
      return;
    }
    container.innerHTML = items.map(l => {
      const gwClass = l.gateway_id || '';
      return (
        '<div class="recent-log-item" data-id="' + l.id + '">' +
          '<span class="rli-gw ' + gwClass + '">' + escapeHtml(l.gateway_id || '?') + '</span>' +
          '<span class="rli-model" title="' + escapeHtml(l.model || '') + '">' + escapeHtml(l.model || '(未知模型)') + '</span>' +
          '<span class="rli-time">' + fmtTimeShort(l.started_at) + '</span>' +
          '<span class="rli-status ' + (l.success ? 'ok' : 'err') + '"></span>' +
        '</div>'
      );
    }).join('');
    $all('.recent-log-item', container).forEach(el => {
      el.addEventListener('click', () => openDrawer(el.dataset.id));
    });
  } catch (e) {
    container.innerHTML = '<div class="empty-state"><span>加载失败：' + escapeHtml(e.message) + '</span></div>';
  }
}

export function initOverview() {
  $('#rangePicker').addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-range]');
    if (!btn) return;
    state.range = btn.dataset.range;
    $all('#rangePicker button').forEach(b => b.classList.toggle('is-active', b === btn));
    $('#pulseRange').textContent = rangeLabel(state.range);
    loadMetrics();
  });
}
