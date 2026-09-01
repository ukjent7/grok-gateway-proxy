# Grok Gateway Proxy

本地代理，在 Grok Build 与上游网关之间做 Responses 协议对齐与可观测性记录。

## 支持的网关

| 前缀 | 网关 | 协议 | Base URL |
|------|------|------|----------|
| `/ds` | DeepSeek | Responses | 留空，建议 `https://api.deepseek.com` |
| `/std` | 标准 Responses | Responses | 留空 |
| `/st` | SenseNova | Chat Completions | 预置 `https://token.sensenova.cn/v1` |
| 自定义 | 标准 Responses | Responses | 用户设置 |

控制台 `http://127.0.0.1:8787` 配置 Base URL，留空时请求返回 503。

## 自定义网关

控制台「网关配置」可新增，复用标准 Responses 逻辑，仅需 `前缀 / 名称 / Base URL`。上游路径固定为 `{base_url}/responses`。

- 前缀即路由：`base_url = http://127.0.0.1:8787/<前缀>`，规则 `^[a-z0-9][a-z0-9_-]{0,31}$`，`api/static/healthz/ui` 及 `ds/st/std` 保留
- 内建可停用，自定义可删除；显示名称决定 `[model.<名称>]`，重名拒绝

## 协议对齐

仅对 Responses 网关（`/ds`、`/std`、自定义）生效，无扩展请求逐字节透传。

- **请求**：剥离 `stream_tool_calls`、非标准 `tools`（如 `x_search`）、非标准 `include`；`web_search.filters.excluded_domains` 重命名为 `blocked_domains`；DeepSeek 额外移除 `include`、清理 `reasoning` 的 `summary/encrypted_content`
- **响应**：`response.reasoning.delta/done` → `response.reasoning_text.delta/done`，丢弃 `ping` 与未知事件类型，`[DONE]` 按上游原样透传

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
