# Grok Gateway Proxy

Local Go proxy for Grok Build with three fixed gateway prefixes:

| Prefix | Default upstream | Protocol |
| --- | --- | --- |
| `/oc` | `https://opencode.ai/zen/go/v1` | Responses |
| `/st` | `https://token.sensenova.cn/v1` | Chat Completions |
| `/ve` | `https://ai-gateway.vercel.sh/v1` | Responses |

The proxy does not provide a models endpoint and does not fetch models from an upstream. Configure model names directly in Grok Build and point each model at one of the local prefixes.

Each adapter preserves its native protocol. `/oc` and `/ve` accept only `POST /responses`; `/st` accepts only `POST /chat/completions`. There is no generic Responses/Chat Completions conversion layer.

OpenCode model compatibility is selected from the request's `model` field by **model-family prefix**, so upstream variants inherit their family's rules automatically (e.g. `muse-spark-1.2-contributo` matches the `muse-spark` family just like `muse-spark-1.2`). Convention: models prefixed `muse-spark` use the Muse profile, which removes the unsupported top-level `stream_tool_calls` option before forwarding and filters its `ping` SSE events; models prefixed `deepseek` currently pass through unchanged and will receive their own rules when needed. Other OpenCode models pass through unchanged unless they receive their own profile. Models named `grok` or prefixed with `grok-` are blocked locally and receive an empty completed Responses response without any upstream call.

The SenseNova adapter includes a narrow compatibility shim for tool-call history: it sends `messages[].tool_calls[].type` as `function_call` upstream and converts it back to `function` for the client, including SSE responses. It also removes empty `id`/function-name fields from SenseNova's streamed tool-call continuation chunks so clients do not overwrite the identity from the first chunk. `tools[].type` is left unchanged. Incomplete historical tool calls (missing `id`, function name, or arguments) and their orphaned `tool` messages are removed before forwarding because SenseNova rejects the whole request otherwise. Both forms remain visible in the request audit record.

The Vercel adapter normalizes two Responses SSE quirks of Vercel's AI Gateway that break strict clients (e.g. Grok Build's async-openai based parser): it drops the `ping` keepalive events the gateway injects, and it renames the legacy `response.reasoning.delta` / `response.reasoning.done` events to the `response.reasoning_text.delta` / `response.reasoning_text.done` variants the client's enum knows (the payloads are field-for-field identical). Everything else is forwarded byte-for-byte.

## Run

```sh
go run .
```

Open <http://127.0.0.1:8787/> for the dashboard.

### Flags and environment

| Flag / env | Purpose |
| --- | --- |
| `-listen` / `GROK_PROXY_LISTEN` | Override the saved listen address |
| `-data-dir` / `GROK_PROXY_DATA_DIR` | Directory for `config.json` and `proxy.db` |
| `-shutdown-timeout` | Graceful-shutdown grace period for in-flight requests (default `30s`) |
| `-log-level` | Log level: `debug`, `info`, `warn`, `error` (default `info`) |
| `-log-retention-days` / `GROK_PROXY_LOG_RETENTION_DAYS` | Prune logs older than N days on startup and hourly (default `30`; `0` disables pruning) |

Useful endpoints are`GET /healthz`, `GET /api/config`, `GET /api/metrics`, `GET /api/logs`,
`GET /api/logs/count`, `GET /api/logs/{id}`, `PATCH /api/proxy`,
`PUT /api/gateways`, `PATCH /api/gateways/{id}`, and `DELETE /api/logs`.


`GET /healthz` returns `{"status":"ok","upstreams":{...}}` where each entry
reports the last background probe of that gateway's `/models` endpoint
(reachability and last HTTP status); probes run at startup and every 30s and
never block the request.

Upstream request deadlines default to 5 minutes and can be raised per request
with an `X-Proxy-Timeout` header (up to 30 minutes) for long-running agentic
streams; there is no fixed client-level timeout that would truncate long
streams.

The application stores `config.json` and `proxy.db` in the `data` folder under the current working directory by default. Use `-data-dir` to override this location. API keys remain in Grok Build configuration; the proxy forwards allowlisted headers and redacts credentials in the log fields (本地小工具，管理 API 默认开放，仅监听 `127.0.0.1`，无需鉴权)。

The dashboard can edit the three HTTPS upstream URLs, the global HTTP/HTTPS proxy URL, and per-gateway Header allowlists and proxy switches; filter logs/statistics by gateway, model, and time range; show weighted overall and per-gateway cache hit rates and coverage; inspect raw JSON/SSE bodies; and copy Grok Build configuration snippets.

The request detail drawer also provides GitHub-style, change-focused side-by-side comparisons with added/modified/deleted counts and a reason for each change:

- the original client request versus the request actually sent to the upstream;
- the upstream API response versus the response actually written back to the client.

For development troubleshooting, SQLite keeps the request body, upstream request body, upstream raw response, final client response, statuses, URLs, and sanitized headers as separate fields. Sensitive header values (`Authorization`, `*Api-Key*`, `*Token*`, `*Secret*`, `Cookie`) are always stored as `[REDACTED]`; on startup the proxy also scrubs any credentials that predate write-time sanitization (旧版的 `*_actual` 字段保留但不再写入，启动时同样会被脱敏)。

Request bodies are capped at 64 MB (larger requests are rejected with `413`). Response bodies are capped at 64 MB for audit capture only: oversized responses are still forwarded to the client in full, but only the first 64 MB is stored, and the log is flagged with `response_truncated` so a pathological upstream stream cannot balloon proxy memory or the SQLite database.

The **网关配置** page includes one global HTTP/HTTPS proxy URL and an independent proxy switch for each gateway. Enabled gateways use the global URL; disabled gateways connect directly. The proxy URL can be cleared to disable proxying globally, while each gateway's switch remains available for later use. The same page also includes an independent User-Agent override switch and value for each gateway. When enabled, that gateway's configured value is applied to its upstream requests; when disabled, the client User-Agent is forwarded according to that gateway's Header allowlist.

## Test

```sh
go test ./...
go vet ./...
gofmt -l .
node --check static/js/*.js   # 或 make js-check
```

CI (`.github/workflows/build.yml`) runs `gofmt` formatting checks, `go vet`,
`go test -race`, and a `node --check` syntax pass over `static/js/*.js`.

## Development

- `make test` / `make vet` / `make fmt` / `make check` — common tasks.
- The dashboard is plain ES modules in `static/js/` (no build step); `static/index.html` loads `static/js/app.js`, which is embedded via `//go:embed static`.
