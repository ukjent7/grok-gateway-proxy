# Grok Gateway Proxy

本地 AI 网关代理，将多个上游 AI 服务统一为统一的 OpenAI 兼容接口，并提供请求日志与可视化仪表盘。

## 支持的网关

| 路径前缀 | 网关 | 协议 |
|---|---|---|
| `/oc` | OpenCode Zen | Responses API |
| `/st` | SenseNova | Chat Completions |
| `/ve` | Vercel AI Gateway | Responses API |

每个网关支持启用/禁用、自定义 Base URL、请求头转发、UA 覆盖等配置。Vercel 网关还支持 FX 伪装模式。

## 快速开始

```bash
# 直接运行（默认监听 127.0.0.1:8787）
./grok-gateway-proxy.exe

# 自定义监听地址和数据目录
./grok-gateway-proxy.exe -listen 127.0.0.1:9000 -data-dir ./data
```

启动后访问 `http://127.0.0.1:8787` 即可打开 Web 仪表盘，查看请求日志、缓存命中率、上游健康状态等。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | 读取配置文件 | HTTP 监听地址 |
| `-data-dir` | `./data` | 配置与日志数据库目录 |
| `-log-level` | `info` | 日志级别：debug / info / warn / error |
| `-log-retention-days` | `30` | 日志保留天数，0 为永久保留 |
| `-shutdown-timeout` | `30s` | 优雅关闭超时 |

也可通过环境变量 `GROK_PROXY_LISTEN`、`GROK_PROXY_DATA_DIR`、`GROK_PROXY_LOG_RETENTION_DAYS` 覆盖。

## 配置

配置文件位于 `data/config.json`，也可通过 Web 仪表盘在线修改。主要字段：

- `listen_addr` — 监听地址
- `proxy_url` — 全局 HTTP/HTTPS 代理
- `log_retention_days` — 日志保留天数
- `gateways` — 各网关的配置（启用状态、Base URL、转发头、UA 覆盖、FX 伪装等）

## 构建

```bash
go build -o grok-gateway-proxy.exe .
```

需要 Go 1.23+，依赖纯 Go SQLite 驱动，无需 CGO 编译器。
