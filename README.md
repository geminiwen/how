# HOWS — HTTP over WebSocket

HOWS 是一个纯协议库，通过 [WebSocket](https://datatracker.ietf.org/doc/html/rfc6455) 隧道传输 [HTTP/1.1](https://datatracker.ietf.org/doc/html/rfc9110) 请求/响应。提供 TypeScript 和 Go 两套实现。

协议使用 [MessagePack](https://msgpack.org/) 序列化，通过 WebSocket binary frames 传输，支持基于 [UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4) request_id 的多路复用。完整协议规范见 [spec.md](spec.md)。

库本身**不包含** WebSocket 管理、HTTP server、连接路由——你自己管连接，库只负责协议编解码和请求分发。

## 核心概念

两个角色，通过 `Sendable` 接口与你的传输层对接：

- **Caller** — 发送 HTTP 请求，等待响应（类似 `http.Client`）
- **Handler** — 接收 HTTP 请求，调用本地 handler 处理，返回响应

```
你的 Caller 端                      你的 Handler 端
     │                                    │
     │  caller.request(req)               │
     │  ──── HTTPRequest ──────────────►  │  handler.handleMessage(data)
     │                                    │  → 调用你的 handler
     │  caller.handleMessage(data)        │
     │  ◄──── HTTPResponse ────────────── │  → 自动发回响应
     │                                    │
     └──── WebSocket / 任意传输 ──────────┘
```

`Sendable` 是唯一的传输抽象：

```typescript
// TypeScript
interface Sendable {
  send(data: Buffer | Uint8Array): void;
}
```

```go
// Go
type Sendable interface {
    Send(data []byte) error
}
```

WebSocket 天然满足这个接口（`ws` 库的 WebSocket 对象直接有 `send` 方法）。

## TypeScript

### 安装

```bash
npm install hows
```

### Handler — 接收请求并处理

把你的 HTTP handler 或转发目标通过 WebSocket 暴露出去：

```typescript
import { WebSocket } from "ws";
import { createHOWSHandler } from "hows";

const ws = new WebSocket("ws://your-server/ws");

// 方式 1: 转发到本地 HTTP 服务
const handler = createHOWSHandler("http://localhost:3000", ws);

// 方式 2: 直接传入 RequestListener
const handler = createHOWSHandler((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("hello");
}, ws);

// 接收消息
ws.on("message", (data) => handler.handleMessage(data));
```

### Caller — 发送请求

向远端 Handler 发起 HTTP 请求：

```typescript
import { WebSocket } from "ws";
import { createHOWSCaller } from "hows";

const ws = new WebSocket("ws://your-server/ws");
const caller = createHOWSCaller(ws);

// 接收响应消息
ws.on("message", (data) => caller.handleMessage(data));

// 发请求
const resp = await caller.request({
  method: "GET",
  url: "/api/hello",
  headers: {},
});

console.log(resp.status_code); // 200
console.log(new TextDecoder().decode(resp.body)); // "hello"
```

### 流式响应

当 Handler 使用转发模式（string target），响应自动走流式协议。Caller 收到的 `resp.body` 是一个 `ReadableStream`：

```typescript
const resp = await caller.request({ method: "GET", url: "/stream", headers: {} });

const stream = resp.body as unknown as ReadableStream<Uint8Array>;
const reader = stream.getReader();
while (true) {
  const { done, value } = await reader.read();
  if (done) break;
  process.stdout.write(value);
}
```

## Go

### 安装

```bash
go get github.com/geminiwen/http-over-ws
```

### Handler — 接收请求并处理

```go
import (
    "github.com/geminiwen/http-over-ws/client"
    "github.com/geminiwen/http-over-ws/protocol"
)

// 方式 1: 转发到本地 HTTP 服务
handler, _ := client.ForwardTo("http://localhost:3000")

// 方式 2: 包装 http.Handler
mux := http.NewServeMux()
mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
    fmt.Fprint(w, "hello")
})
handler := client.HTTPHandler(mux)

// 方式 3: 实现 Handler 接口
handler := client.HandlerFunc(func(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
    return &protocol.HTTPResponsePayload{
        StatusCode: 200,
        Headers:    map[string][]string{"Content-Type": {"text/plain"}},
        Body:       []byte("hello"),
    }, nil
})

// 创建 HOWSHandler，接到 WebSocket
sender := &wsSender{conn: wsConn, ctx: ctx}  // 你自己实现 Sendable
h := client.NewHandler(handler, sender)

// 读消息循环
for {
    _, data, err := wsConn.Read(ctx)
    if err != nil {
        break
    }
    h.HandleMessage(ctx, data)
}
```

### Caller — 发送请求

```go
sender := &wsSender{conn: wsConn, ctx: ctx}
caller := client.NewCaller(sender)

// 后台读消息循环
go func() {
    for {
        _, data, err := wsConn.Read(ctx)
        if err != nil {
            return
        }
        caller.HandleMessage(ctx, data)
    }
}()

// 发请求（阻塞等待响应）
resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
    Method:  "GET",
    URL:     "/api/hello",
    Headers: map[string][]string{},
})

fmt.Println(resp.StatusCode)    // 200
fmt.Println(string(resp.Body))  // "hello"
```

Go 的 Caller 会自动把流式响应（Start + Chunks + End）合并成完整的 `HTTPResponsePayload` 返回。

## 协议

详见 [spec.md](spec.md)。

消息类型：

| Type | Code | 方向 |
|------|------|------|
| HTTPRequest | 0x10 | Caller → Handler |
| HTTPResponse | 0x11 | Handler → Caller |
| HTTPResponseStart | 0x12 | Handler → Caller（流式） |
| HTTPResponseChunk | 0x13 | Handler → Caller（流式） |
| HTTPResponseEnd | 0x14 | Handler → Caller（流式） |
| Error | 0xFF | 双向 |

序列化：[MessagePack](https://msgpack.org/) over [WebSocket](https://datatracker.ietf.org/doc/html/rfc6455) binary frames。

## 相关规范

- [RFC 9110 — HTTP Semantics](https://datatracker.ietf.org/doc/html/rfc9110)
- [RFC 6455 — The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455)
- [RFC 9562 — UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4)
- [RFC 2119 — Requirement Levels](https://datatracker.ietf.org/doc/html/rfc2119)
- [MessagePack Specification](https://msgpack.org/)

## License

MIT
