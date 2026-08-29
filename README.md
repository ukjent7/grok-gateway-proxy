# Grok Gateway Proxy

本地代理工具，在 Grok Build 与上游网关之间做 Responses 协议对齐与可观测性记录。


## 支持的网关

| 前缀 | 网关 | 协议 | Base URL |
|------|------|------|----------|
| `/ds` | DeepSeek（Responses API） | Responses | 留空，建议 `https://api.deepseek.com` |
| `/std` | 标准 Responses（任意标准上游） | Responses | 留空，按需填写 |
| `/st` | SenseNova | Chat Completions | 预置 |

首次使用前在控制台（`http://127.0.0.1:8787`）填好各网关的 Base URL；
Base URL 留空的网关收到请求时返回明确的 503 提示。

## Responses 协议对齐

Grok Build 的 Responses 请求携带若干 xAI 私有扩展，标准上游可能直接拒绝。
代理在 Responses 协议网关（`/ds`、`/std`）上做双向对齐。

请求方向（发往上游前剥离，其余字段——含 `reasoning.effort`——原样保留）：

- `stream_tool_calls` — xAI 后端专用选项
- `tools` 中标准词汇表之外的条目 — 如 `x_search`；`web_search` 若配置了
  `excluded_domains`（标准协议无对应能力）则整个工具被移除，而非静默放宽搜索范围
- `include` 中标准词汇表之外的值 — 如 `no_inline_citations`

DeepSeek 网关（`/ds`）在此基础上额外处理其方言差异：

- `include` 整体移除（DeepSeek 不支持任何 include 值，思维链恒以明文返回）
- 输入 `reasoning` 项剔除 `summary` / `encrypted_content`，明文 `content`
  （即 DeepSeek 的 reasoning_content 载体）完整保留回传 —— 带 tools 的请求
  必须回传思维链，否则 DeepSeek 返回 400
- `reasoning.effort` 原样透传，由 DeepSeek 自行映射（low/medium/high/xhigh →
  low/high/max，none 关闭思考模式）

响应方向（转发给客户端前清洗为 Grok Build 可解析的词汇表）：

- 旧版事件名 `response.reasoning.delta` / `.done` 重命名为标准的
  `response.reasoning_text.delta` / `.done`（载荷字段完全一致）
- ping 保活事件被丢弃
- 客户端事件词汇表之外的 SSE 事件类型被丢弃 — 一帧无法反序列化的事件会让
  整个流失败，因此丢弃而非透传
- DeepSeek 的事件序列没有 `data: [DONE]` 哨兵，客户端按 EOF 收尾，代理原样透传

无扩展的请求逐字节透传；行为由 `internal/proxy/responsesanitize.go` 与
`internal/proxy/adapter_deepseek.go` 的白名单驱动。

## 运行

```bash
go run .
# 监听 127.0.0.1:8787，数据目录 ./data
```

浏览器打开 `http://127.0.0.1:8787` 进入控制台。

## 构建

```bash
go build -ldflags "-s -w -X main.version=$(git describe --tags 2>/dev/null || echo dev)" .
```

## 配置

配置文件位于 `data/config.json`，首次运行自动生成。支持的环境变量：

- `GROK_PROXY_LISTEN` — 监听地址
- `GROK_PROXY_DATA_DIR` — 数据目录
- `GROK_PROXY_LOG_RETENTION_DAYS` — 日志保留天数

旧配置中的 `oc` / `ve` 网关条目会被自动忽略并迁移为新的 `ds` / `std` 网关。

## 开发

```bash
go test ./...      # 测试
go vet ./...       # 静态检查
```
