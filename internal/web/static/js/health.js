'use strict';

/* ============================================================
   Grok Gateway Console · 上游健康状态轮询与监控
   ============================================================ */

import { $ } from './utils.js';
import { state, gatewayIds } from './state.js';

export function healthOf(id) {
  return (state.health && state.health.upstreams && state.health.upstreams[id]) || null;
}

export async function pollHealth() {
  let data;
  try {
    const res = await fetch('/healthz');
    if (!res.ok) return;
    data = await res.json();
  } catch (e) {
    return;
  }
  state.health = data || { upstreams: {} };
  applyHealthUI();
}

export function applyHealthUI() {
  const upstreams = (state.health && state.health.upstreams) || {};
  const ids = gatewayIds().filter(id => upstreams[id]);

  const dot = $('#healthDot');
  const text = $('#healthText');
  const code = $('#healthCode');

  if (dot && text && code) {
    let cls = 'dot';
    let textVal = '代理监听正常';
    let codeVal = 'online';

    if (ids.length) {
      const ok = ids.filter(id => upstreams[id].reachable).length;
      if (ok === ids.length) {
        cls += ' dot-live';
        codeVal = `${ok}/${ids.length}`;
      } else if (ok === 0) {
        cls += ' dot-error';
        codeVal = `0/${ids.length}`;
      } else {
        cls += ' dot-warn';
        codeVal = `${ok}/${ids.length}`;
      }
      textVal = `上游 ${ok}/${ids.length} 正常`;
    }

    dot.className = cls;
    text.textContent = textVal;
    code.textContent = codeVal;
  }

  // Update dots in cards & pulse rows
  document.querySelectorAll('.health-dot[data-gwid]').forEach(el => {
    const h = upstreams[el.dataset.gwid];
    const isOk = !!(h && h.reachable);
    const isBad = !!(h && !h.reachable);

    el.classList.toggle('is-ok', isOk);
    el.classList.toggle('is-bad', isBad);

    let tip = '暂无探测数据';
    if (h) {
      tip = h.reachable
        ? (h.error ? h.error : `上游在线 · HTTP ${h.status || 200}`)
        : `上游异常：${h.error || `HTTP ${h.status}`}`;
    }
    el.title = tip;
  });
}

export function healthDotHTML(id) {
  return `<span class="health-dot" data-gwid="${id}" aria-hidden="true" title="网关连通性"></span>`;
}
