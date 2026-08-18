# Grok Gateway Proxy

Local Go proxy for Grok Build with three fixed gateway prefixes:

| Prefix | Default upstream | Protocol |
| --- | --- | --- |
| `/oc` | `https://opencode.ai/zen/go/v1` | Responses |
| `/st` | `https://token.sensenova.cn/v1` | Chat Completions |
| `/ve` | `https://ai-gateway.vercel.sh/v1` | Responses |

The proxy does not provide a models endpoint and does not fetch models from an upstream. Configure model names directly in Grok Build and point each model at one of the local prefixes.

Each adapter preserves its native protocol. `/oc` and `/ve` accept only `POST /responses`; `/st` accepts only `POST /chat/completions`. There is no generic Responses/Chat Completions conversion layer.

The SenseNova adapter includes a narrow compatibility shim for tool-call history: it sends `messages[].tool_calls[].type` as `function_call` upstream and converts it back to `function` for the client, including SSE responses. It also removes empty `id`/function-name fields from SenseNova's streamed tool-call continuation chunks so clients do not overwrite the identity from the first chunk. `tools[].type` is left unchanged. Incomplete historical tool calls (missing `id`, function name, or arguments) and their orphaned `tool` messages are removed before forwarding because SenseNova rejects the whole request otherwise. Both forms remain visible in the request audit record.

The Vercel adapter normalizes two Responses SSE quirks of Vercel's AI Gateway that break strict clients (e.g. Grok Build's async-openai based parser): it drops the `ping` keepalive events the gateway injects, and it renames the legacy `response.reasoning.delta` / `response.reasoning.done` events to the `response.reasoning_text.delta` / `response.reasoning_text.done` variants the client's enum knows (the payloads are field-for-field identical). Everything else is forwarded byte-for-byte.

## Run

```sh
go run .
```

Open <http://127.0.0.1:8787/> for the dashboard.

Build a single executable with the embedded dashboard:

```sh
go build -o grok-gateway-proxy .
```

Useful endpoints are `GET /healthz`, `GET /api/config`, `GET /api/metrics`, `GET /api/logs`, `GET /api/logs/{id}`, and `DELETE /api/logs`. Use `-listen` to override the saved listen address and `-data-dir` to choose where `config.json` and `proxy.db` are stored.

The application stores `config.json` and `proxy.db` in the `data` folder under the current working directory by default. Use `-data-dir` to override this location. API keys remain in Grok Build configuration; the proxy forwards allowlisted headers and redacts credentials in the normal log fields.

Set an optional `api_token` in `config.json` to require `Authorization: Bearer <token>` on the management API (`/api/*`). When the token is empty (default) the management API stays open to localhost. The dashboard remembers the token in the browser and prompts for it on `401`; the request/response headers drawer also defaults to the sanitized view with a button to reveal the raw header snapshots.

The dashboard can edit the three HTTPS upstream URLs and per-gateway Header allowlists, filter logs/statistics by gateway, model, and time range, show weighted cache hit rate and coverage, inspect raw JSON/SSE bodies, and copy Grok Build configuration snippets.

The request detail drawer also provides GitHub-style, change-focused side-by-side comparisons with added/modified/deleted counts and a reason for each change:

- the original client request versus the request actually sent to the upstream;
- the upstream API response versus the response actually written back to the client.

For development troubleshooting, SQLite keeps the request body, upstream request body, upstream raw response, final client response, statuses, URLs, sanitized headers, and actual header snapshots as separate fields. Existing databases are migrated automatically on startup.

Request bodies are capped at 64 MB (larger requests are rejected with `413`). Response bodies are capped at 64 MB for audit capture only: oversized responses are still forwarded to the client in full, but only the first 64 MB is stored, and the log is flagged with `response_truncated` so a pathological upstream stream cannot balloon proxy memory or the SQLite database.

The **网关配置** page includes an independent User-Agent override switch and value for each gateway. When enabled, that gateway's configured value is applied to its upstream requests; when disabled, the client User-Agent is forwarded according to that gateway's Header allowlist.

## Test

```sh
go test ./...
go vet ./...
```
