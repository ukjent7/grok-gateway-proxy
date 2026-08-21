'use strict';

/* ============================================================
   Grok Gateway Console · 接入代码模块
   - 多客户端代码生成 (Grok Build TOML / cURL / Python / Node.js)
   - 代码高亮框与一键复制反馈
   ============================================================ */

import { state, gatewayIds } from './state.js';
import { api } from './api.js';
import { $, escapeHtml, copyText } from './utils.js';
import { showToast } from './ui.js';

function setupBaseURL() {
  const addr = (state.listenAddr || '').trim();
  if (!addr) return 'http://127.0.0.1:8787';
  let base = addr;
  if (!/^https?:\/\//i.test(base)) base = 'http://' + base;
  return base.replace(/\/+$/, '');
}

export function buildCurl(gw) {
  const base = setupBaseURL();
  const model = (gw.name || gw.id || 'model').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') + '-model';
  const auth = '"Authorization: Bearer $YOUR_API_TOKEN"';

  if (gw.protocol === 'chat_completions') {
    const payload = JSON.stringify({
      model: model,
      messages: [{ role: 'user', content: 'Hello, how can you help me?' }],
      stream: true
    }, null, 2);

    return `curl ${base}${gw.prefix}/chat/completions \\
  -H "Content-Type: application/json" \\
  -H ${auth} \\
  -d '${payload.replace(/'/g, "\\'")}'`;
  }

  const payload = JSON.stringify({
    model: model,
    input: 'Hello, how can you help me?'
  }, null, 2);

  return `curl ${base}${gw.prefix}/responses \\
  -H "Content-Type: application/json" \\
  -H ${auth} \\
  -d '${payload.replace(/'/g, "\\'")}'`;
}

export function buildPythonSDK(gw) {
  const base = setupBaseURL();
  const model = (gw.name || gw.id || 'model').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') + '-model';

  return `from openai import OpenAI

# 初始化 OpenAI 客户端，指向 Grok Gateway 本地代理
client = OpenAI(
    base_url="${base}${gw.prefix}",
    api_key="your_api_token_here"  # 填入上游所需的 API Key
)

response = client.chat.completions.create(
    model="${model}",
    messages=[
        {"role": "system", "content": "You are a helpful coding assistant."},
        {"role": "user", "content": "Hello, write a quicksort in Python."}
    ],
    stream=True
)

for chunk in response:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
`;
}

export function buildNodeSDK(gw) {
  const base = setupBaseURL();
  const model = (gw.name || gw.id || 'model').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') + '-model';

  return `import OpenAI from 'openai';

const client = new OpenAI({
  baseURL: '${base}${gw.prefix}',
  apiKey: process.env.API_KEY || 'your_api_token_here'
});

async function main() {
  const stream = await client.chat.completions.create({
    model: '${model}',
    messages: [{ role: 'user', content: 'Hello!' }],
    stream: true,
  });

  for await (const chunk of stream) {
    process.stdout.write(chunk.choices[0]?.delta?.content || '');
  }
}

main().catch(console.error);
`;
}

export async function loadSetup() {
  const container = $('#setupSnippets');
  if (!container) return;

  const base = setupBaseURL();
  const baseInfo = $('#setupBaseUrl');
  if (baseInfo) {
    baseInfo.innerHTML = `
      <div class="setup-base-card">
        <div class="setup-base-icon">
          <svg viewBox="0 0 16 16" width="16" height="16" fill="none"><circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.3"/><path d="M8 4v4l3 3" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>
        </div>
        <div class="setup-base-content">
          <span class="setup-base-label">当前代理监听基地址</span>
          <code class="setup-base-val">${escapeHtml(base)}</code>
        </div>
        <button type="button" class="btn-ghost small" id="copyBaseUrlBtn">
          <svg viewBox="0 0 16 16" width="12" height="12" fill="none"><rect x="5" y="5" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.4"/><path d="M3 11V3.5A1.5 1.5 0 0 1 4.5 2H11" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>
          复制基地址
        </button>
      </div>
    `;

    const copyBtn = $('#copyBaseUrlBtn');
    if (copyBtn) {
      copyBtn.addEventListener('click', async () => {
        await copyText(base);
        showToast('已复制基地址到剪贴板', 'success');
      });
    }
  }

  container.innerHTML = '<div class="empty-state">正在载入配置片段…</div>';

  try {
    const data = await api('/setup');
    container.innerHTML = '';

    gatewayIds().forEach(id => {
      const snippet = data[id];
      if (!snippet) return;
      const gw = state.gateways[id] || {};

      const card = document.createElement('div');
      card.className = 'setup-card';

      const curlSnippet = buildCurl(gw);
      const pythonSnippet = buildPythonSDK(gw);
      const nodeSnippet = buildNodeSDK(gw);

      card.innerHTML = `
        <div class="setup-card-head">
          <div class="setup-card-title">
            <span class="gw-dot gw-dot-${escapeHtml(id)}"></span>
            <strong>${escapeHtml(gw.name || id)}</strong>
            <code class="gw-card-prefix">${escapeHtml(gw.prefix || '')}</code>
          </div>
          <div class="setup-card-tabs" role="tablist">
            <button type="button" class="setup-tab is-active" data-lang="toml">Grok Build (TOML)</button>
            <button type="button" class="setup-tab" data-lang="curl">cURL</button>
            <button type="button" class="setup-tab" data-lang="python">Python</button>
            <button type="button" class="setup-tab" data-lang="node">Node.js</button>
          </div>
          <button type="button" class="btn-primary small setup-copy-current-btn">
            <svg viewBox="0 0 16 16" width="12" height="12" fill="none"><rect x="5" y="5" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.4"/><path d="M3 11V3.5A1.5 1.5 0 0 1 4.5 2H11" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>
            复制此代码
          </button>
        </div>

        <div class="setup-code-container">
          <pre class="setup-pre code-toml is-visible"><code>${escapeHtml(snippet)}</code></pre>
          <pre class="setup-pre code-curl" style="display:none"><code>${escapeHtml(curlSnippet)}</code></pre>
          <pre class="setup-pre code-python" style="display:none"><code>${escapeHtml(pythonSnippet)}</code></pre>
          <pre class="setup-pre code-node" style="display:none"><code>${escapeHtml(nodeSnippet)}</code></pre>
        </div>
      `;

      container.appendChild(card);

      // Tab switching
      const tabs = card.querySelectorAll('.setup-tab');
      const pres = {
        toml: card.querySelector('.code-toml'),
        curl: card.querySelector('.code-curl'),
        python: card.querySelector('.code-python'),
        node: card.querySelector('.code-node')
      };
      const snippets = {
        toml: snippet,
        curl: curlSnippet,
        python: pythonSnippet,
        node: nodeSnippet
      };

      let currentLang = 'toml';

      tabs.forEach(tab => {
        tab.addEventListener('click', () => {
          tabs.forEach(t => t.classList.remove('is-active'));
          tab.classList.add('is-active');
          currentLang = tab.dataset.lang;

          Object.keys(pres).forEach(lang => {
            if (pres[lang]) {
              pres[lang].style.display = lang === currentLang ? 'block' : 'none';
            }
          });
        });
      });

      // Copy current snippet
      const copyCurrentBtn = card.querySelector('.setup-copy-current-btn');
      if (copyCurrentBtn) {
        copyCurrentBtn.addEventListener('click', async () => {
          const text = snippets[currentLang];
          if (text) {
            await copyText(text);
            const orig = copyCurrentBtn.innerHTML;
            copyCurrentBtn.innerHTML = '<svg viewBox="0 0 16 16" width="12" height="12" fill="none"><path d="M3.5 8.5l3 3 6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg> 已复制 ✓';
            showToast(`已复制 ${gw.name} 的 ${currentLang.toUpperCase()} 接入代码`, 'success');
            setTimeout(() => {
              copyCurrentBtn.innerHTML = orig;
            }, 2000);
          }
        });
      }
    });
  } catch (e) {
    container.innerHTML = `<div class="empty-state text-danger">加载失败：${escapeHtml(e.message)}</div>`;
  }
}

export function initSetup() {
  const reloadBtn = $('#setupReloadBtn');
  if (reloadBtn) {
    reloadBtn.addEventListener('click', () => {
      loadSetup();
      showToast('已刷新接入代码', 'success');
    });
  }
}
