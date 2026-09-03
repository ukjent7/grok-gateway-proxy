'use strict';

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import {
  $, $all, fmtNum, fmtPct,
  escapeHtml, rangeToFrom, rangeLabel, fmtCompact,
  gatewayPrefixLabel, gatewayTone
} from './utils.js';
import { showToast } from './ui.js';

const WINDOW_LIMIT = 100;

function windowQuery(limit) {
  const params = new URLSearchParams({ limit: String(limit || WINDOW_LIMIT) });
  const from = rangeToFrom(state.range);
  if (from) params.set('from', from.toISOString());
  return params.toString();
}

export async function loadOverview() {
  await Promise.all([loadMetrics(), loadActivity()]);
  const label = rangeLabel(state.range);
  const el = $('#activityRange');
  if (el) el.textContent = label;
}

/* ---------------- 1. 指标区 ---------------- */

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

// 记录每个指标上一次的原始数值，用于值变化时做 300ms 的滚动动画。
const metricPrev = new Map();

function easeOut(t) {
  return 1 - Math.pow(1 - t, 3);
}

function renderMetricValue(el, target, fmt) {
  if (!el) return;
  const from = metricPrev.get(el.id);
  metricPrev.set(el.id, target);
  const reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (from === undefined || !Number.isFinite(from) || !Number.isFinite(target) || reduced) {
    el.textContent = fmt(target);
    return;
  }
  const start = performance.now();
  const tick = (now) => {
    const p = Math.min(1, (now - start) / 300);
    el.textContent = fmt(from + (target - from) * easeOut(p));
    if (p < 1) requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

function setMetric(id, value, fmt, note, tone) {
  const el = document.getElementById(id);
  if (el) {
    if (value === null || value === undefined) {
      el.textContent = '—';
      metricPrev.delete(id);
    } else {
      renderMetricValue(el, value, fmt || String);
    }
    el.classList.toggle('is-warn', tone === 'warn');
    el.classList.toggle('is-err', tone === 'err');
  }
  const sub = document.getElementById(id + 'Sub');
  if (sub) sub.textContent = note;
}

function renderMetrics(m) {
  const keys = ['Requests', 'Success', 'CacheHit', 'Tokens'];
  if (!m) {
    keys.forEach(k => {
      const el = document.getElementById('stat' + k);
      if (el) el.textContent = '—';
      const sub = document.getElementById('stat' + k + 'Sub');
      if (sub) sub.textContent = '加载失败';
    });
    renderGatewayCacheRates(null);
    return;
  }
  const coverage = m.cache_coverage_percent == null ? '—' : fmtPct(m.cache_coverage_percent);
  setMetric('statRequests', m.requests, fmtNum,
    m.failures > 0 ? `${fmtNum(m.failures)} 次失败` : '全部成功',
    m.failures > 0 ? 'err' : null);
  setMetric('statSuccess', m.requests > 0 ? (m.successes / m.requests) * 100 : null, fmtPct,
    `${fmtNum(m.successes)} / ${fmtNum(m.requests)} 成功`);
  setMetric('statCacheHit', m.cache_hit_rate == null ? null : m.cache_hit_rate, fmtPct,
    `覆盖率 ${coverage} · ${fmtCompact(m.cache_read_tokens)} 命中 Token`);
  setMetric('statTokens', m.prompt_tokens || m.input_tokens || 0, fmtCompact,
    `输出 ${fmtCompact(m.output_tokens)} · 推理 ${fmtCompact(m.reasoning_tokens)}`);

  renderGatewayCacheRates(m);
}

/* ---------------- 2. 网关缓存效率 ---------------- */

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
        <span class="gateway-rate-bar">${valid ? `<span style="width:${width}%"></span>` : ''}</span>
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

/* ---------------- 3. 主图表 ---------------- */

async function loadActivity() {
  const container = $('#activityChart');
  if (!container) return;
  try {
    const params = new URLSearchParams({ buckets: '48' });
    const from = rangeToFrom(state.range);
    if (from) params.set('from', from.toISOString());
    const series = await api('/metrics/timeseries?' + params.toString());
    state.activity = series;
    renderActivity();
  } catch (e) {
    state.activity = null;
    state.activitySig = '';
    chartGeom = null;
    container.innerHTML = '<div class="empty-state"><span class="text-danger">活动数据载入失败</span></div>';
  }
}

const CHART_H = 212;
const CHART_PAD = { top: 26, right: 14, bottom: 26, left: 44 };
const MIN_TICKS = 2;
const MAX_TICKS = 6;
// 0.75 而不是 1：张力越大越"滑"，但遇到孤立的尖峰时曲线会在峰两侧甩出明显的 S 形。
const CURVE_TENSION = 0.75;

// 最近一次绘制的几何信息：悬浮时要把指针的像素坐标换算回第几个时间桶。
let chartGeom = null;
// 只有首次绘制（含切换时间范围）才描线；实时推送每来一条请求都会重画，
// 每次都重放一遍动画会闪得没法看。
let chartPainted = false;

function clampNum(v, lo, hi) {
  return v < lo ? lo : (v > hi ? hi : v);
}

// 纵轴刻度取整到 1/2/2.5/5/10 × 10ⁿ 的档位，并在所有可行档位里挑浪费垂直空间
// 最少的那个：既让刻度是能读的整数（不是"峰值 37 → 刻度 0 / 9.25 / 18.5"），
// 又不至于把峰值压在图表底部。2.5 档只在步长 ≥10 时启用 —— 请求数是整数，
// 小数量级下出现 0 / 2.5 / 5 这种半格读数反而更难读。
function niceScale(maxValue) {
  const max = Math.max(1, Number(maxValue) || 0);
  const niceSteps = [1, 2, 2.5, 5, 10];
  const exp = Math.floor(Math.log10(max));
  let best = null;
  for (const e of [exp - 1, exp, exp + 1]) {
    for (const n of niceSteps) {
      const step = n * Math.pow(10, e);
      if (step < 1 || (n === 2.5 && step < 10)) continue;
      const top = Math.ceil(max / step) * step;
      const ticks = Math.round(top / step);
      if (ticks < MIN_TICKS || ticks > MAX_TICKS) continue;
      const waste = top / max;
      if (!best || waste < best.waste - 1e-9) best = { step: step, max: top, waste: waste };
    }
  }
  return best || { step: 1, max: Math.ceil(max) };
}

// Catmull-Rom 转三次贝塞尔，把折线拉成平滑曲线。控制点的 y 被夹在绘图区内，
// 否则相邻两桶高低悬殊时曲线会甩到坐标轴外面去。
function smoothPath(points, lo, hi) {
  if (!points.length) return '';
  if (points.length === 1) return `M ${points[0].x.toFixed(2)} ${points[0].y.toFixed(2)}`;
  let d = `M ${points[0].x.toFixed(2)} ${points[0].y.toFixed(2)}`;
  for (let i = 0; i < points.length - 1; i++) {
    const p0 = points[i - 1] || points[i];
    const p1 = points[i];
    const p2 = points[i + 1];
    const p3 = points[i + 2] || p2;
    const c1x = p1.x + ((p2.x - p0.x) / 6) * CURVE_TENSION;
    const c1y = clampNum(p1.y + ((p2.y - p0.y) / 6) * CURVE_TENSION, lo, hi);
    const c2x = p2.x - ((p3.x - p1.x) / 6) * CURVE_TENSION;
    const c2y = clampNum(p2.y - ((p3.y - p1.y) / 6) * CURVE_TENSION, lo, hi);
    d += ` C ${c1x.toFixed(2)} ${c1y.toFixed(2)} ${c2x.toFixed(2)} ${c2y.toFixed(2)} ${p2.x.toFixed(2)} ${p2.y.toFixed(2)}`;
  }
  return d;
}

// 把曲线闭合到基线，得到面积图的可填充区域。
function areaPath(points, baseY, lo) {
  if (!points.length) return '';
  const line = smoothPath(points, lo, baseY);
  const first = points[0];
  if (points.length === 1) return `${line} L ${(first.x + 1).toFixed(2)} ${baseY} Z`;
  const last = points[points.length - 1];
  return `${line} L ${last.x.toFixed(2)} ${baseY} L ${first.x.toFixed(2)} ${baseY} Z`;
}

// 时间精度跟着整个窗口跨度走，而不是跟着桶宽走：1 小时视图里桶宽 75 秒，
// 但轴上打一串 HH:MM:SS 只会让标签互相挤；7 天视图里则连时刻都没有意义。
function timeFormatFor(series) {
  const span = series && series.from && series.to
    ? (new Date(series.to).getTime() - new Date(series.from).getTime()) / 1000
    : 0;
  if (span > 3 * 86400) return 'date';
  if (span > 1200) return 'hm';
  return 'hms';
}

function axisTimeLabel(t, mode) {
  const d = new Date(t);
  if (isNaN(d.getTime())) return '';
  const pad = (v) => String(v).padStart(2, '0');
  if (mode === 'date') return `${d.getMonth() + 1}/${d.getDate()}`;
  if (mode === 'hm') return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function axisValueLabel(v) {
  return v >= 10000 ? fmtCompact(v) : fmtNum(v);
}

function renderActivity() {
  const container = $('#activityChart');
  if (!container) return;
  const series = state.activity;
  const sig = JSON.stringify(series && [series.bucket_seconds, series.buckets]);
  if (sig === state.activitySig) return;
  state.activitySig = sig;

  const buckets = (series && series.buckets) || [];
  if (!buckets.length || !buckets.some(b => (b.requests || 0) > 0)) {
    chartGeom = null;
    container.innerHTML = `
      <div class="empty-state chart-empty-state">
        <svg viewBox="0 0 48 48" width="40" height="40" fill="none" style="opacity: 0.45; margin-bottom: 4px;">
          <path d="M6 38h36M10 32l8-10 8 6 12-14" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
          <circle cx="10" cy="32" r="2.5" fill="currentColor"/>
          <circle cx="18" cy="22" r="2.5" fill="currentColor"/>
          <circle cx="26" cy="28" r="2.5" fill="currentColor"/>
          <circle cx="38" cy="14" r="2.5" fill="currentColor"/>
        </svg>
        <span style="font-weight: 500;">所选时间范围内暂无请求流量</span>
        <span class="muted" style="font-size: 11.5px;">向上游网关发起调用后，吞吐曲线与 Token 消耗将在此实时呈现</span>
      </div>
    `;
    return;
  }

  const animate = !chartPainted;
  chartPainted = true;

  const timeMode = timeFormatFor(series);
  const width = Math.max(Math.round(container.clientWidth || 0), 280);
  const height = CHART_H;
  const plotL = CHART_PAD.left;
  const plotR = width - CHART_PAD.right;
  const plotT = CHART_PAD.top;
  const plotB = height - CHART_PAD.bottom;
  const innerW = Math.max(plotR - plotL, 1);
  const innerH = Math.max(plotB - plotT, 1);
  const n = buckets.length;
  const step = n > 1 ? innerW / (n - 1) : 0;
  const xAt = (i) => (n > 1 ? plotL + i * step : plotL + innerW / 2);

  const reqScale = niceScale(Math.max(...buckets.map(b => b.requests || 0), 1));
  const tokMax = Math.max(...buckets.map(b => b.output_tokens || 0), 0);
  const yReq = (v) => plotB - (v / reqScale.max) * innerH;
  const yTok = (v) => plotB - (v / tokMax) * innerH;

  const totalPts = buckets.map((b, i) => ({ x: xAt(i), y: yReq(b.requests || 0) }));
  const hasFailures = buckets.some(b => (b.failures || 0) > 0);
  const failPts = hasFailures ? buckets.map((b, i) => ({ x: xAt(i), y: yReq(b.failures || 0) })) : [];
  // Token 量纲是万级、请求量是个级，各用自己的上限缩放才不会互相压平。
  const tokenPts = tokMax > 0 ? buckets.map((b, i) => ({ x: xAt(i), y: yTok(b.output_tokens || 0) })) : [];

  const lo = plotT - 4;
  const grid = [];
  const tickCount = Math.round(reqScale.max / reqScale.step);
  for (let k = 0; k <= tickCount; k++) {
    const value = reqScale.step * k;
    const y = yReq(value).toFixed(1);
    grid.push(
      `<line class="activity-grid${k === 0 ? ' is-base' : ''}" x1="${plotL}" y1="${y}" x2="${plotR}" y2="${y}"/>` +
      `<text class="activity-grid-label" x="${plotL - 8}" y="${(Number(y) + 3.5).toFixed(1)}" text-anchor="end">${axisValueLabel(value)}</text>`
    );
  }

  const labelCount = n === 1 ? 1 : clampNum(Math.round(innerW / 130), 2, 6);
  const axis = [];
  for (let k = 0; k < labelCount; k++) {
    const i = labelCount === 1 ? 0 : Math.round((k / (labelCount - 1)) * (n - 1));
    const anchor = labelCount === 1 ? 'middle' : (i === 0 ? 'start' : (i === n - 1 ? 'end' : 'middle'));
    axis.push(`<text class="activity-axis-text" x="${xAt(i).toFixed(1)}" y="${(plotB + 16).toFixed(1)}" text-anchor="${anchor}">${axisTimeLabel(buckets[i].t, timeMode)}</text>`);
  }

  // 失败量常常只占请求量的百分之几，按同一纵轴画出来就是贴着基线的一条细缝，
  // 等于看不见。给每个"有失败"的桶点一个红点，事故才不会被面积比例吞掉。
  const failMarks = hasFailures
    ? buckets.map((b, i) => (b.failures || 0) > 0
      ? `<circle class="activity-fail-mark" cx="${failPts[i].x.toFixed(1)}" cy="${failPts[i].y.toFixed(1)}" r="2.4"/>`
      : '').join('')
    : '';

  const tokenNote = tokMax > 0
    ? `<text class="activity-token-note" x="${plotR}" y="12" text-anchor="end">Token 峰值 ${fmtCompact(tokMax)}</text>`
    : '';

  container.innerHTML = `
    <svg class="activity-svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="请求吞吐与 Token 输出趋势图">
      <defs>
        <linearGradient id="activityOkFill" x1="0" y1="0" x2="0" y2="1">
          <stop class="activity-grad is-ok-top" offset="0%"/>
          <stop class="activity-grad is-ok-bottom" offset="100%"/>
        </linearGradient>
        <linearGradient id="activityErrFill" x1="0" y1="0" x2="0" y2="1">
          <stop class="activity-grad is-err-top" offset="0%"/>
          <stop class="activity-grad is-err-bottom" offset="100%"/>
        </linearGradient>
      </defs>
      ${grid.join('')}
      <path class="activity-area is-ok" d="${areaPath(totalPts, plotB, lo)}" fill="url(#activityOkFill)"/>
      ${hasFailures ? `<path class="activity-area is-err" d="${areaPath(failPts, plotB, lo)}" fill="url(#activityErrFill)"/>` : ''}
      <path class="activity-line is-ok" d="${smoothPath(totalPts, lo, plotB)}" fill="none"/>
      ${hasFailures ? `<path class="activity-line is-err" d="${smoothPath(failPts, lo, plotB)}" fill="none"/>` : ''}
      ${tokenPts.length ? `<path class="activity-line is-token" d="${smoothPath(tokenPts, lo, plotB)}" fill="none"/>` : ''}
      ${failMarks}
      ${axis.join('')}
      ${tokenNote}
      <g class="activity-hover">
        <line class="activity-crosshair" x1="0" y1="${plotT}" x2="0" y2="${plotB}"/>
        <circle class="activity-hover-dot is-req" cx="0" cy="0" r="4.5"/>
        ${tokenPts.length ? '<circle class="activity-hover-dot is-token" cx="0" cy="0" r="3"/>' : ''}
      </g>
    </svg>
    <div class="activity-tip"></div>
  `;

  chartGeom = { n, width, plotL, step, buckets, totalPts, tokenPts, tokMax };

  requestAnimationFrame(() => {
    // 描线动画靠 dashoffset，得先量出路径长度；量完再挂动画类，否则
    // 第一帧会先整条画出来再被擦掉重画。
    $all('.activity-line', container).forEach(el => {
      if (typeof el.getTotalLength !== 'function') return;
      el.style.setProperty('--dash-len', Math.ceil(el.getTotalLength()));
    });
    if (animate) {
      const svg = $('.activity-svg', container);
      if (svg) svg.classList.add('is-animated');
    }
  });
}

function hideChartHover(container) {
  const tip = $('.activity-tip', container);
  const layer = $('.activity-hover', container);
  if (tip) tip.classList.remove('is-visible');
  if (layer) layer.classList.remove('is-visible');
}

// 指针在图上移动时，把最近的那个桶的数值摊开显示：曲线只负责看趋势，
// 精确读数交给随指针走的提示卡。
function bindChartHover(container) {
  container.addEventListener('pointermove', (e) => {
    const svg = $('.activity-svg', container);
    const tip = $('.activity-tip', container);
    const layer = $('.activity-hover', container);
    if (!svg || !tip || !layer || !chartGeom) return;

    const rect = svg.getBoundingClientRect();
    if (!rect.width) return;
    const scale = rect.width / chartGeom.width || 1;
    const px = (e.clientX - rect.left) / scale;

    const { n, plotL, step, buckets, totalPts, tokenPts, tokMax } = chartGeom;
    const i = n > 1 ? clampNum(Math.round((px - plotL) / step), 0, n - 1) : 0;
    const b = buckets[i];
    const total = totalPts[i];
    if (!b || !total) return;

    layer.classList.add('is-visible');
    const cross = $('.activity-crosshair', container);
    if (cross) {
      cross.setAttribute('x1', total.x);
      cross.setAttribute('x2', total.x);
    }
    const dotReq = $('.activity-hover-dot.is-req', container);
    if (dotReq) {
      dotReq.setAttribute('cx', total.x);
      dotReq.setAttribute('cy', total.y);
    }
    const dotTok = $('.activity-hover-dot.is-token', container);
    if (dotTok && tokenPts.length) {
      dotTok.setAttribute('cx', tokenPts[i].x);
      dotTok.setAttribute('cy', tokenPts[i].y);
    }

    tip.innerHTML = `
      <span class="activity-tip-time">${escapeHtml(axisTimeLabel(b.t, timeFormatFor(state.activity)))}</span>
      <span class="activity-tip-row"><i class="legend-dot legend-ok"></i>请求<b>${fmtNum(b.requests || 0)}</b></span>
      <span class="activity-tip-row"><i class="legend-dot legend-err"></i>失败<b>${fmtNum(b.failures || 0)}</b></span>
      ${tokMax > 0 ? `<span class="activity-tip-row"><i class="legend-dot legend-token"></i>输出<b>${fmtCompact(b.output_tokens || 0)}</b></span>` : ''}
    `;
    tip.classList.add('is-visible');
    // 先显示再量宽度，否则 offsetWidth 是 0，贴右边缘时卡片会被挤出图外。
    const left = clampNum(e.clientX - rect.left + 14, 4, Math.max(rect.width - tip.offsetWidth - 4, 4));
    tip.style.left = `${left}px`;
  });

  container.addEventListener('pointerleave', () => hideChartHover(container));
}

/* ---------------- 4. 初始化 ---------------- */

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
      chartPainted = false;
      loadOverview();
    });
  }

  const chart = $('#activityChart');
  if (chart) {
    bindChartHover(chart);
    // 曲线坐标是按容器宽度算的，宽度一变就得按新坐标重画，否则会被拉变形。
    if ('ResizeObserver' in window) {
      new ResizeObserver(() => {
        if (!state.activity) return;
        state.activitySig = '';
        renderActivity();
      }).observe(chart);
    }
  }
}
