# Grok Gateway Proxy

本地代理，在 Grok Build 与上游网关之间做 Responses 协议对齐与可观测性记录。

## 支持的网关

| 前缀 | 网关 | 协议 | Base URL |
|------|------|------|----------|
| `/ds` | DeepSeek | Responses | 留空，建议 `https://api.deepseek.com` |
| `/std` | 标准 Responses | Responses | 留空 |
| `/st` | SenseNova | Chat Completions | 预置 `https://token.sensenova.cn/v1` |
| `/oaic` | OpenAI Compatible | Chat Completions (OpenAI Compatible) | 留空，建议 `https://api.openai.com/v1` 或兼容服务 |
| `/anth` | Anthropic | Messages (Anthropic) | 留空，建议 `https://api.anthropic.com/v1` |
| 自定义 | 标准 Responses | Responses | 用户设置 |

控制台 `http://127.0.0.1:8787` 配置 Base URL，留空时请求返回 503。

## 自定义网关

控制台「网关配置」可新增，复用标准 Responses 逻辑，仅需 `前缀 / 名称 / Base URL`。上游路径固定为 `{base_url}/responses`。

- 前缀即路由：`base_url = http://127.0.0.1:8787/<前缀>`，规则 `^[a-z0-9][a-z0-9_-]{0,31}$`，`api/static/healthz/ui` 及 `ds/st/std` 保留
- 内建可停用，自定义可删除；显示名称决定 `[model.<名称>]`，重名拒绝

## 协议对齐

Grok Build 原生支持三种协议（Responses / Chat Completions / Anthropic Messages），按所选后端把请求发到对应路径。代理据此分派：

| 网关 | 客户端发来的格式 | 代理做什么 |
|------|------|------|
| `/ds`、`/std`、自定义 | Responses | 清洗正文（见下）+ 会话翻译，其余逐字节透传 |
| `/st` | Chat Completions | SenseNova 方言翻译（`type: function`↔`function_call`、`finish_reason: ""`→`null`、孤儿 tool 消息清理） |
| `/oaic` | Chat Completions | 通用清洗：剥离无效的 `tool_calls` 条目与孤儿 `tool` 消息，避免上游 400 |
| `/anth` | Anthropic Messages | 清洗：剔除缺少 `name` 的工具定义 |

### 请求头翻译

- **会话亲和**：`X-Grok-Session-Id` / `X-Grok-Conv-Id` 按协议翻译——Responses 发 `session_id` + `X-Client-Request-Id`，Chat Completions 额外发 `x-session-affinity`，Anthropic 仅发 `x-session-affinity`；`openrouter` 模式发 `X-Session-Id`，`opencode` 模式（或 Base URL 指向 opencode.ai）发 `x-opencode-session`
- **缓存路由**：会话 ID 同时作为 `prompt_cache_key` 注入 Responses 请求体（保序插入，不改动其余字节）；会话亲和头与 `prompt_cache_key` 是两套独立机制，同时发送
- **请求 ID**：审计 ID → `X-Request-Id`，`X-Grok-Req-Id` → `X-Correlation-Id`
- **标准头转发**：`Accept`、`User-Agent`、W3C `Traceparent`/`Tracestate`；其余 `X-Grok-*` 头按白名单丢弃（`x-grok-client-*` 已并入 UA）
- **强制剥离**：`Accept-Encoding`、`Content-Encoding`、`Content-Length`、`Host`——即使写进 `forward_headers` 也不转发。代理必须读取明文响应体做翻译，透传压缩协商会让 Go transport 交出未解压的字节
- **Anthropic 专属**：强制 `Accept: application/json`（Messages API 不支持 Accept 协商 SSE）、补 `Anthropic-Version: 2023-06-01` 与 `anthropic-dangerous-direct-browser-access`；`Bearer` 凭据自动补为 `X-Api-Key`。不再无条件强制 `Anthropic-Beta`（beta 开关改变上游行为，需要时经 `forward_headers` 自行携带）

### Responses 正文清洗

剥离 `stream_tool_calls`、非标准 `tools`（如 `x_search`）、非标准 `include`；`web_search.filters.excluded_domains` 重命名为 `blocked_domains`；过滤后为空则删键而非留空数组。DeepSeek 额外移除 `include`、清理 `reasoning` 的 `summary`/`encrypted_content`、`reasoning.effort: minimal` 降级为 `low`。

### 响应处理

- **Responses**：`response.reasoning.delta/done` → `response.reasoning_text.delta/done`，丢弃 `ping` 与未知事件类型（丢弃计数以 Info 级别写日志），`[DONE]` 原样透传
- **SenseNova**：响应里的 `type: function_call` 翻回 `function`，空 `finish_reason` 置为 `null`，工具调用增量中的空 `id`/`name` 字段剥离

## 运行

```bash
go run .  # 127.0.0.1:8787，数据目录 ./data
```

浏览器打开 `http://127.0.0.1:8787`。后台周期探测已配置 Base URL 的 `/models` 用于健康灯，`--health-check-interval 0` 可关闭。

## 构建

```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags 2>/dev/null || echo dev)" .
```

## 配置

文件 `data/config.json`。环境变量：`GROK_PROXY_LISTEN`、`GROK_PROXY_DATA_DIR`、`GROK_PROXY_LOG_RETENTION_DAYS`。

- `body_capture_limit_kb=0` 不截断但受 32MB 上限约束，超限标记 `response_truncated`，客户端仍收完整响应
- 并发请求体预算 64MB，超限排队 30s 后 503 + `Retry-After`

## 开发

```bash
go test ./...
go vet ./...
```
