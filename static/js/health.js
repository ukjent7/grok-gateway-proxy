'use strict';

import { $ } from './utils.js';
import { state, gatewayIds } from './state.js';

/* /healthz 轮询与展示。
   /healthz 不受管理接口令牌保护（liveness 探针语义），可直接 fetch。 */

export function healthOf(id) {
  return (state.health && state.health.upstreams && state.health.upstreams[id]) || null;
}

export async function pollHealth() {
  let data;
  try {
    const res = await fetch('/healthz');
    if (!res.ok) return;
    data = await res.json();
  } catch (e) { return; } // 代理本体不可达时无可展示内容
  state.health = data || { upstreams: {} };
  applyHealthUI();
}

export function applyHealthUI() {
  const upstreams = (state.health && state.health.upstreams) || {};
  const ids = gatewayIds().filter(id => upstreams[id]);
  const dot = $('#healthDot');
  const text = $('#healthText');
  const code = $('#healthCode');
  if (!dot || !text || !code) return;

  let cls = 'dot';
  let textVal = '代理监听中';
  let codeVal = 'online';
  if (ids.length) {
    const ok = ids.filter(id => upstreams[id].reachable).length;
    if (ok === ids.length) { cls += ' dot-live'; codeVal = String(ok) + '/' + ids.length; }
    else if (ok === 0) { cls += ' dot-error'; codeVal = '0/' + ids.length; }
    else { cls += ' dot-warn'; codeVal = ok + '/' + ids.length; }
    textVal = '上游 ' + ok + '/' + ids.length + ' 在线';
  }
  dot.className = cls;
  text.textContent = textVal;
  code.textContent = codeVal;

  // 页面上已渲染的网关健康点（配置卡 / 脉冲行）
  document.querySelectorAll('.health-dot[data-gwid]').forEach(el => {
    const h = upstreams[el.dataset.gwid];
    el.classList.toggle('is-ok', !!(h && h.reachable));
    el.classList.toggle('is-bad', !!(h && !h.reachable));
    el.title = h ? (h.reachable ? (h.error ? h.error : '最近状态 ' + h.status) : '不可达：' + (h.error || ('HTTP ' + h.status))) : '暂无探测数据';
  });
}

export function healthDotHTML(id) {
  return '<span class="health-dot" data-gwid="' + id + '" aria-hidden="true"></span>';
}
