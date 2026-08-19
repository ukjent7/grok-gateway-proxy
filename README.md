# Grok Gateway Proxy

本地代理工具，在 AI 客户端与上游网关之间做最小干预的协议适配与可观测性记录。

## 核心理念

尽量少干预客户端和网关之间的交互，只做必要或安全的处理。

## 支持的网关

| 前缀 | 网关 | 协议 |
|------|------|------|
| `/oc` | OpenCode Zen | Responses |
| `/st` | SenseNova | Chat Completions |
| `/ve` | Vercel AI Gateway | Responses |

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

## 开发

```bash
go test ./...      # 测试
go vet ./...       # 静态检查
```

前端为原生 ES 模块，无构建步骤，直接编辑 `static/` 下的文件即可。
