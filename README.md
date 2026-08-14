# opencode2api

`opencode2api` 是一个使用 Go 编写的 OpenCode Zen / Zen Go 协议代理。它对外提供标准 OpenAI 与 Anthropic API，并自动添加 OpenCode 客户端请求头。

主要功能：

- 支持 OpenAI Chat Completions、Responses 和 Models API
- 支持 Anthropic Messages API
- 支持普通响应和 SSE 流式响应
- 支持文本、图片、工具定义、工具调用和工具结果转换
- 分离配置 Zen key 池与 Zen Go key 池
- 模型同时存在于两个上游时按 `prefer` 配置优先使用 Go 或 Zen（默认 Go）
- 支持直连、HTTP、HTTPS、SOCKS5 和 SOCKS5H 代理
- 将 key 自动均衡绑定到代理，保持连接亲和性
- 使用无锁轮询分配并发请求
- 代理失败后自动迁移绑定，key 失败后进行短时冷却
- 启动时及每 60 秒通过 Cloudflare trace 并行检查代理可用性
- 为不同会话生成不同的 OpenCode 会话 ID

## API 路径

| 方法 | 路径 | 协议 |
| --- | --- | --- |
| `GET` | `/v1/models` | OpenAI 模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions |
| `POST` | `/v1/responses` | OpenAI Responses |
| `POST` | `/v1/messages` | Anthropic Messages |
| `GET` | `/healthz` | 健康检查 |

## 编译

需要 Go 1.24 或更高版本。

```bash
go build -o opencode2api ./
```

## 配置

复制示例配置：

```bash
cp config.example.json config.json
```

然后编辑 `config.json`：

```json
{
  "listen": "127.0.0.1:8080",
  "server_keys": ["change-this-local-key"],
  "zen_keys": ["sk-your-zen-key"],
  "go_keys": [],
  "prefer": "go",
  "proxies": ["direct"],
  "upstream": {
    "zen": "https://opencode.ai/zen",
    "go": "https://opencode.ai/zen/go"
  },
  "retry": {
    "max_attempts": 3,
    "timeout_seconds": 300
  },
  "models": {
    "refresh_seconds": 300,
    "protocols": {}
  },
  "performance": {
    "max_idle_conns": 2048,
    "max_idle_conns_per_host": 256,
    "max_conns_per_host": 0,
    "idle_conn_timeout_seconds": 120,
    "connect_timeout_seconds": 5,
    "failure_cooldown_seconds": 15
  },
  "logging": {
    "level": "info"
  }
}
```

### 基础字段

| 字段 | 含义 |
| --- | --- |
| `listen` | 本地监听地址。默认建议使用 `127.0.0.1:8080`，避免服务直接暴露到公网。 |
| `server_keys` | 调用本代理时使用的本地 API key 列表。它们只用于本地鉴权，不会发送给 OpenCode。 |
| `zen_keys` | OpenCode Zen API key 池。允许配置多个 key。 |
| `go_keys` | OpenCode Zen Go API key 池。没有 Go key 时可以使用空数组。 |
| `prefer` | 模型同时存在于 Zen 与 Go 时优先使用的上游，值为 `go` 或 `zen`，默认 `go`。仅存在于某一池时不受影响。 |
| `proxies` | 上游代理列表。支持 `direct`、`http://`、`https://`、`socks5://` 和 `socks5h://`。URL 可以包含代理用户名和密码。 |

`server_keys` 至少需要一个值；`zen_keys` 和 `go_keys` 至少有一个池不能为空。

### key 与代理分配规则

只需要直连时使用：

```json
"proxies": ["direct"]
```

SOCKS5 代理示例：

```json
"proxies": ["socks5://127.0.0.1:1080"]
```

多个代理示例：

```json
"proxies": [
  "http://user:password@127.0.0.1:7890",
  "socks5://127.0.0.1:1080"
]
```

### `upstream`

| 字段 | 含义 |
| --- | --- |
| `upstream.zen` | Zen 上游根地址，通常保持为 `https://opencode.ai/zen`。 |
| `upstream.go` | Zen Go 上游根地址，通常保持为 `https://opencode.ai/zen/go`。 |

### `retry`

| 字段 | 含义 |
| --- | --- |
| `retry.max_attempts` | 单个请求的最大尝试次数，包含第一次请求。非 2xx 响应或网络错误会切换节点。 |
| `retry.timeout_seconds` | 单个客户端请求的总超时时间，同时用于限制上游响应头等待时间。 |

流式响应一旦已经向客户端输出数据，就不会切换节点重新生成，避免拼接两个不同的响应。

### `models`

| 字段 | 含义 |
| --- | --- |
| `models.refresh_seconds` | 重新读取 Zen 和 Go 模型列表的间隔秒数。两个列表会并发刷新。 |
| `models.protocols` | 手动指定模型的原生协议。值只能是 `chat`、`responses` 或 `anthropic`。通常保持为空。 |

模型协议覆盖示例：

```json
"protocols": {
  "custom-model": "chat"
}
```

模型同时存在于 Zen 与 Go 时按 `prefer` 配置选择：值为 `go` 时优先 Go，值为 `zen` 时优先 Zen（默认 `go`）。仅存在于某一池时才使用该池的 key。

### `performance`

| 字段 | 含义 |
| --- | --- |
| `performance.max_idle_conns` | 所有上游连接池允许保留的最大空闲连接数。 |
| `performance.max_idle_conns_per_host` | 每个上游主机允许保留的最大空闲连接数。 |
| `performance.max_conns_per_host` | 每个主机的最大并发连接数。`0` 表示不设置上限。 |
| `performance.idle_conn_timeout_seconds` | 空闲连接在连接池中保留的时间。 |
| `performance.connect_timeout_seconds` | 与上游或代理建立 TCP 连接的超时时间。 |
| `performance.failure_cooldown_seconds` | 连接失败、认证失败、限流或 5xx 后节点的基础冷却时间。连续失败会指数增加冷却时间。 |

### `logging`

| 字段 | 含义 |
| --- | --- |
| `logging.level` | 日志级别，支持 `info` 和 `debug`。生产环境建议使用 `info`。 |

日志不会输出完整上游 key、本地 key、Authorization、`x-api-key` 或请求消息正文。


## 会话 ID

代理会为上游添加 OpenCode 使用的 `User-Agent`、`x-opencode-client`、`x-opencode-session`、`x-opencode-request` 和 `x-opencode-project` 请求头。

- 每个请求使用不同的 `x-opencode-request`，同一次请求的重试保持不变。
- 优先使用客户端提供的 `x-opencode-session`、`x-session-id`、`conversation-id`、`conversation_id` 或 `metadata.session_id` 生成会话 ID。
- 没有显式会话标识时，使用第一条用户消息生成稳定会话 ID，使同一段多轮对话保持一致。
- 如果两个独立会话的第一条消息完全相同，建议由客户端发送不同的 `x-session-id`，以确保两个会话严格分离。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区一直以来的支持。
