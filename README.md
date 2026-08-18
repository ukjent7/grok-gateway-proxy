# Grok Gateway Proxy

Local Go proxy for Grok Build with three fixed gateway prefixes:

| Prefix | Default upstream | Protocol |
| --- | --- | --- |
| `/oc` | `https://opencode.ai/zen/go/v1` | Responses |
| `/st` | `https://token.sensenova.cn/v1` | Chat Completions |
| `/ve` | `https://ai-gateway.vercel.sh/v1` | Responses |

The proxy does not provide a models endpoint and does not fetch models from an upstream. Configure model names directly in Grok Build and point each model at one of the local prefixes.

Each adapter preserves its native protocol. `/oc` and `/ve` accept only `POST /responses`; `/st` accepts only `POST /chat/completions`. There is no generic Responses/Chat Completions conversion layer.

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

The application stores `config.json` and `proxy.db` in the platform data directory. API keys remain in Grok Build configuration; the proxy forwards allowlisted headers and redacts credentials in logs.

The dashboard can edit the three HTTPS upstream URLs and per-gateway Header allowlists, filter logs/statistics by gateway, model, and time range, show weighted cache hit rate and coverage, inspect raw JSON/SSE bodies, and copy Grok Build configuration snippets.

## Test

```sh
go test ./...
go vet ./...
```
