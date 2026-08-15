# opencode2api

`opencode2api` 是一个使用 Go 编写的 OpenCode Zen / Zen Go 协议代理。它对外提供标准 OpenAI 与 Anthropic API，并自动添加 OpenCode 客户端请求头。

主要功能：

- 支持 OpenAI Chat Completions、Responses 和 Models API
- 支持 Anthropic Messages API
- 支持普通响应和 SSE 流式响应
- 支持文本、图片、thinking/reasoning、工具定义、工具调用和工具结果转换
- 分离配置 Zen key 池与 Zen Go key 池
- 模型同时存在于两个上游时按 `prefer` 配置优先使用 Go 或 Zen（默认 Go）
- 支持直连、HTTP、HTTPS、SOCKS5 和 SOCKS5H 代理
- 支持从文本文件读取代理池，并与配置内的代理合并、去重
- `config.json` 支持 `//` 和 `/* ... */` 注释
- 将 key 自动均衡绑定到代理，保持连接亲和性
- 使用稳定会话哈希保持同一会话的 key/proxy 亲和性，并在节点故障时自动回退
- 代理失败后自动迁移绑定，key 失败后进行短时冷却
- 根据真实上游流量识别代理故障，并每 15 分钟通过 Cloudflare trace 并行复查异常代理
- 为不同会话生成不同的 OpenCode 会话 ID，并支持 `x-opencode-session`、`x-session-id` 和 `conversation-id` 显式指定会话
- 内置独立端口 WebUI，可管理配置、查看运行指标、key/proxy 状态与实时日志
- WebUI 使用账号密码、服务端 session、HttpOnly Cookie、CSRF 与登录限速保护
- WebUI 保存后原子写入配置并热切换 Gateway；无效配置不会影响当前流量
- stdout 输出结构化 JSON 日志，最近事件与一小时滚动指标保存在有限内存中

## API 路径

| 方法 | 路径 | 协议 |
| --- | --- | --- |
| `GET` | `/v1/models` | OpenAI 模型列表 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions |
| `POST` | `/v1/responses` | OpenAI Responses |
| `POST` | `/v1/messages` | Anthropic Messages |
| `GET` | `/healthz` | 健康检查 |

`/healthz` 无需 API key，返回服务版本以及模型目录、Zen/Go key 和代理池的汇总状态，不会暴露 key 或代理地址。模型目录尚未完成首次刷新、已经过期、没有可暴露模型或没有健康代理时返回 HTTP `503`；其余情况返回 `200`。

模型目录的过期阈值为 `models.refresh_seconds` 的两倍，且不低于 60 秒。刚启动时短暂返回 `503 starting` 属于正常现象，模型列表首次刷新成功后会变为 `200 ok`。

## WebUI

示例配置会在独立的 `8081` 端口启动管理界面：

```text
http://服务器地址:8081
```

首次账号为 `admin`，密码来自 `webui.password`。服务第一次成功启动时会使用 Argon2id 将密码转换为带盐哈希，写入 `webui.password_hash`，并从配置中删除明文密码。请在首次登录后立即修改示例密码。

WebUI 可查看最近一小时的请求率、成功率、状态码与延迟分位，监控活跃请求/流、模型、key 冷却和 proxy 健康状态；实时日志通过 SSE 推送。监控和最近日志仅保存在内存，服务重启后清空，stdout 日志仍可由 Docker 或日志平台收集。

## 编译

需要 Go 1.24 或更高版本。

```bash
go build -o opencode2api ./
```

## Docker Compose 部署

服务器安装 Docker 与 Docker Compose 后，可以在克隆项目后直接启动：

```bash
git clone https://github.com/jasonxu114514/opencode2api.git
cd opencode2api
docker compose up -d
```


```bash
cp config.example.json config.json
# 编辑 server_keys、zen_keys/go_keys，并修改 webui.password
docker compose restart
```

```bash
curl http://127.0.0.1:8080/healthz
# 浏览器打开 http://127.0.0.1:8081
docker compose logs -f
```

如需修改宿主机端口：

```bash
OPENCODE2API_PORT=18080 OPENCODE2API_WEBUI_PORT=18081 docker compose up -d
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
  "proxyfile": "",
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
    "level": "info",
    "ring_size": 2000
  },
  "webui": {
    "enabled": true,
    "listen": "0.0.0.0:8081",
    "username": "admin",
    "password": "change-this-admin-password",
    "session_ttl_minutes": 720
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
| `proxyfile` | 可选代理池文件路径。相对路径以 `config.json` 所在目录为基准；内容会追加到 `proxies` 并去重。 |
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

也可以从文本文件加载代理池：

```json
{
  "proxyfile": "proxies.txt",
  "proxies": ["direct"]
}
```

`proxies.txt` 每行填写一个代理。支持空行、以 `#`、`;` 或 `//` 开头的整行注释，也支持在代理后使用空格加这些标记写行尾注释：

```text
# HTTP 代理
http://user:password@127.0.0.1:7890
socks5://127.0.0.1:1080  # 备用代理
```

配置中的 `proxies` 会先加载，随后加载 `proxyfile`，重复项只保留第一次出现的位置。如果两个来源都为空，则仍使用 `direct`。`config.json` 本身支持 `//` 单行注释和 `/* ... */` 块注释；引号内的 `https://` 等内容不会被当作注释。

### `upstream`

| 字段 | 含义 |
| --- | --- |
| `upstream.zen` | Zen 上游根地址，通常保持为 `https://opencode.ai/zen`。 |
| `upstream.go` | Zen Go 上游根地址，通常保持为 `https://opencode.ai/zen/go`。 |

### `retry`

| 字段 | 含义 |
| --- | --- |
| `retry.max_attempts` | 单个请求的最大尝试次数，包含第一次请求。网络错误、认证失败、限流和 5xx 会切换节点；其他 4xx 属于确定性的请求错误，会直接返回而不轮换 key。 |
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

### Thinking 工具历史兼容

所有请求都会经过同一个上游请求准备流程，同协议转发和跨协议转换不再使用两套分支。通过 Chat Completions 或 Anthropic Messages API 调用 DeepSeek、Kimi/Moonshot 或 MiMo 模型时，代理会按上游的目标协议规范化 assistant 工具历史：Chat 补全缺失或空的 `reasoning_content`；Anthropic 保留有效 thinking 文本、为缺失或空的 thinking 补充兼容占位内容、将 `redacted_thinking` 转为普通 thinking，并移除这些兼容端点不接受的 `signature`。显式启用 reasoning/thinking 的别名模型也会启用该处理，普通非 reasoning 请求不会被修改。

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
| `logging.level` | 日志级别，支持 `debug`、`info`、`warn` 和 `error`，可通过 WebUI 热切换。 |
| `logging.ring_size` | WebUI 最近日志环容量，范围 100–50000，默认 2000。stdout 不受此容量限制。 |

每条 stdout 日志都是单行 JSON，包含时间、级别、组件、事件以及适用的 request ID、模型、tier、状态码、耗时和重试次数。日志不会输出完整上游 key、本地 key、Authorization、Cookie、代理认证信息或请求消息正文。

### `webui`

| 字段 | 含义 |
| --- | --- |
| `webui.enabled` | 是否在独立端口启动管理服务。旧配置未包含该段时默认关闭。 |
| `webui.listen` | 管理服务监听地址，示例为 `0.0.0.0:8081`。 |
| `webui.username` | 单一管理员账号。 |
| `webui.password` | 仅用于首次初始化的明文密码，至少 10 个字符；启动后自动删除。 |
| `webui.password_hash` | 自动生成的 Argon2id 哈希，不应手动编辑，也不会由 WebUI API 返回。 |
| `webui.session_ttl_minutes` | 登录 session 有效时间，范围 5–10080 分钟。 |

WebUI 中普通配置响应只包含 key 尾码/指纹及脱敏 proxy。需要查看完整值时必须再次输入管理密码，敏感响应禁止浏览器缓存。

### 配置保存与热重载

WebUI 保存时先解析并验证完整候选配置、创建新的连接池和 Gateway，然后写入临时文件、保留 `config.json.bak` 并替换 `config.json`，最后原子切换新请求使用的运行实例。写入或初始化失败时旧实例继续工作；切换前已开始的请求不会中断。

keys、proxy、上游、重试、模型、性能、优先 tier 和日志级别会立即生效。`listen`、`webui.listen` 与 `webui.enabled` 会保存但需要重启进程。WebUI 也提供“从磁盘重载”，外部编辑后的配置仍会经过相同的验证与回滚流程。保存后的 JSON 会被规范化，原有注释不会保留。


## 会话 ID

代理会为上游添加 OpenCode 使用的 `User-Agent`、`x-opencode-client`、`x-opencode-session`、`x-opencode-request` 和 `x-opencode-project` 请求头。

- 每个请求使用不同的 `x-opencode-request`，同一次请求的重试保持不变。
- 优先使用客户端提供的 `x-opencode-session`、`x-session-id`、`conversation-id`、`conversation_id` 或 `metadata.session_id` 生成会话 ID。
- 没有显式会话标识时，使用第一条用户消息生成稳定会话 ID，使同一段多轮对话保持一致。
- 如果两个独立会话的第一条消息完全相同，建议由客户端发送不同的 `x-session-id`，以确保两个会话严格分离。

## 致谢

感谢 [LINUX DO](https://linux.do) 社区一直以来的支持。
