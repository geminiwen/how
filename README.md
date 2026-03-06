# HOWS — HTTP over WebSocket

HOWS is a pure protocol library for tunneling [HTTP/1.1](https://datatracker.ietf.org/doc/html/rfc9110) requests/responses through [WebSocket](https://datatracker.ietf.org/doc/html/rfc6455) connections. It provides both TypeScript and Go implementations.

The protocol uses [MessagePack](https://msgpack.org/) serialization over WebSocket binary frames, with [UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4) `request_id` based multiplexing. See [spec.md](spec.md) for the full protocol specification.

The library does **not** include WebSocket management, HTTP servers, or connection routing — you manage your own connections, and the library handles protocol encoding/decoding and request dispatching.

## Core Concepts

Two roles, connected to your transport layer via the `Sendable` interface:

- **Caller** — sends HTTP requests, waits for responses (like an `http.Client`)
- **Handler** — receives HTTP requests, dispatches to a local handler, sends back responses

```
Your Caller Side                     Your Handler Side
     │                                    │
     │  caller.request(req)               │
     │  ──── HTTPRequest ──────────────►  │  handler.handleMessage(data)
     │                                    │  → invokes your handler
     │  caller.handleMessage(data)        │
     │  ◄──── HTTPResponse ────────────── │  → sends back response
     │                                    │
     └──── WebSocket / any transport ─────┘
```

`Sendable` is the only transport abstraction:

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

WebSocket naturally satisfies this interface (the `ws` library's WebSocket object has a `send` method directly).

## TypeScript

### Installation

```bash
npm install hows
```

### Handler — Receive and Handle Requests

Expose your HTTP handler or forward target over WebSocket:

```typescript
import { WebSocket } from "ws";
import { createHOWSHandler } from "hows";

const ws = new WebSocket("ws://your-server/ws");

// Option 1: Forward to a local HTTP service
const handler = createHOWSHandler("http://localhost:3000", ws);

// Option 2: Pass a RequestListener directly
const handler = createHOWSHandler((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("hello");
}, ws);

// Receive messages
ws.on("message", (data) => handler.handleMessage(data));
```

### Caller — Send Requests

Send HTTP requests to a remote Handler:

```typescript
import { WebSocket } from "ws";
import { createHOWSCaller } from "hows";

const ws = new WebSocket("ws://your-server/ws");
const caller = createHOWSCaller(ws);

// Receive response messages
ws.on("message", (data) => caller.handleMessage(data));

// Send a request
const resp = await caller.request({
  method: "GET",
  url: "/api/hello",
  headers: {},
});

console.log(resp.status_code); // 200
console.log(new TextDecoder().decode(resp.body)); // "hello"
```

### Streaming Responses

When the Handler uses forward mode (string target), responses automatically use the streaming protocol. The `resp.body` received by the Caller is a `ReadableStream`:

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

### Installation

```bash
go get github.com/geminiwen/how
```

### Handler — Receive and Handle Requests

The following example uses [Hertz](https://github.com/cloudwego/hertz) with [hertz-contrib/websocket](https://github.com/hertz-contrib/websocket) to set up a WebSocket server that handles HOWS requests:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "sync"

    "github.com/cloudwego/hertz/pkg/app"
    "github.com/cloudwego/hertz/pkg/app/server"
    "github.com/hertz-contrib/websocket"

    "github.com/geminiwen/how/client"
)

// wsSender implements Sendable for hertz-contrib/websocket.
type wsSender struct {
    conn *websocket.Conn
    mu   sync.Mutex
}

func (s *wsSender) Send(data []byte) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

var upgrader = websocket.HertzUpgrader{}

func main() {
    // Option 1: Forward to a local HTTP service
    handler, _ := client.ForwardTo("http://localhost:3000")

    // Option 2: Wrap an http.Handler
    mux := http.NewServeMux()
    mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprint(w, "hello")
    })
    handler = client.HTTPHandler(mux)

    h := server.Default(server.WithHostPorts("0.0.0.0:8888"))
    h.NoHijackConnPool = true

    h.GET("/ws", func(ctx context.Context, c *app.RequestContext) {
        upgrader.Upgrade(c, func(conn *websocket.Conn) {
            sender := &wsSender{conn: conn}
            howsHandler := client.NewHandler(handler, sender)

            for {
                _, data, err := conn.ReadMessage()
                if err != nil {
                    log.Println("read:", err)
                    break
                }
                howsHandler.HandleMessage(ctx, data)
            }
        })
    })

    h.Spin()
}
```

### Caller — Send Requests

```go
package main

import (
    "context"
    "fmt"
    "log"
    "sync"

    "github.com/hertz-contrib/websocket"

    "github.com/geminiwen/how/client"
    "github.com/geminiwen/how/protocol"
)

// wsSender implements Sendable for hertz-contrib/websocket.
type wsSender struct {
    conn *websocket.Conn
    mu   sync.Mutex
}

func (s *wsSender) Send(data []byte) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func main() {
    dialer := websocket.DefaultDialer
    conn, _, err := dialer.Dial("ws://localhost:8888/ws", nil)
    if err != nil {
        log.Fatal("dial:", err)
    }
    defer conn.Close()

    sender := &wsSender{conn: conn}
    caller := client.NewCaller(sender)

    // Read loop in background
    go func() {
        for {
            _, data, err := conn.ReadMessage()
            if err != nil {
                return
            }
            caller.HandleMessage(context.Background(), data)
        }
    }()

    // Send a request (blocks until response)
    resp, err := caller.Request(context.Background(), &protocol.HTTPRequestPayload{
        Method:  "GET",
        URL:     "/hello",
        Headers: map[string][]string{},
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(resp.StatusCode)    // 200
    fmt.Println(string(resp.Body))  // "hello"
}
```

The Go Caller automatically merges streaming responses (Start + Chunks + End) into a complete `HTTPResponsePayload`.

## Protocol

See [spec.md](spec.md) for details.

Message types:

| Type | Code | Direction |
|------|------|-----------|
| HTTPRequest | 0x10 | Caller → Handler |
| HTTPResponse | 0x11 | Handler → Caller |
| HTTPResponseStart | 0x12 | Handler → Caller (streaming) |
| HTTPResponseChunk | 0x13 | Handler → Caller (streaming) |
| HTTPResponseEnd | 0x14 | Handler → Caller (streaming) |
| Error | 0xFF | Bidirectional |

Serialization: [MessagePack](https://msgpack.org/) over [WebSocket](https://datatracker.ietf.org/doc/html/rfc6455) binary frames.

## Related Specifications

- [RFC 9110 — HTTP Semantics](https://datatracker.ietf.org/doc/html/rfc9110)
- [RFC 6455 — The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455)
- [RFC 9562 — UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4)
- [RFC 2119 — Requirement Levels](https://datatracker.ietf.org/doc/html/rfc2119)
- [MessagePack Specification](https://msgpack.org/)

## License

MIT
