# HOW — HTTP over WebSocket

HOW is a pure protocol library for tunneling [HTTP/1.1](https://datatracker.ietf.org/doc/html/rfc9110) requests/responses through [WebSocket](https://datatracker.ietf.org/doc/html/rfc6455) connections. It provides both TypeScript and Go implementations.

The protocol supports two serialization modes:
- **Binary mode** — [MessagePack](https://msgpack.org/) over WebSocket binary frames, prefixed with `0x69` discriminator byte
- **Text mode** — JSON over WebSocket text frames

Both modes use [UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4) `request_id` based multiplexing. See [spec.md](spec.md) for the full protocol specification.

The library does **not** include WebSocket management, HTTP servers, or connection routing — you manage your own connections, and the library handles protocol encoding/decoding and request dispatching.

## Core Concepts

Two roles, connected to your transport layer via the `Sendable` interface:

- **Caller** — sends HTTP requests, waits for responses (like an `http.Client`)
- **Handler** — receives HTTP requests, dispatches to a local handler, sends back responses

```
Your Caller Side                      Your Handler Side
     │                                      │
     │  caller.request(req)                 │
     │  ──── HTTPRequest ────────────────►  │  → invokes your handler
     │                                      │
     │  Non-streaming peer:                 │
     │  ◄──── HTTPResponse ──────────────── │  → full response in one frame
     │                                      │
     │  Streaming peer:                     │
     │  ◄──── HTTPResponseStart ─────────── │  → status + headers
     │  ◄──── HTTPResponseChunk ─────────── │  → body chunk (0..N times)
     │  ◄──── HTTPResponseEnd ───────────── │  → clean EOF
     │                                      │
     └───── WebSocket / any transport ──────┘
```

Either response shape is fed in through `caller.handleBinaryMessage` / `handleTextMessage`; on the caller side the two collapse into the same `ReadableStream` / `io.ReadCloser` body.

`Sendable` is the only transport abstraction:

```typescript
// TypeScript
interface Sendable {
  sendBytes(data: Buffer | Uint8Array): void;
  sendText(data: string): void;
}
```

```go
// Go
type Sendable interface {
    SendBytes(data []byte) error
    SendText(data string) error
}
```

## TypeScript

### Handler — Receive and Handle Requests

Expose your HTTP handler or forward target over WebSocket:

```typescript
import { WebSocket } from "ws";
import { createHOWHandler } from "@byted/how";

const ws = new WebSocket("ws://your-server/ws");
const sendable = {
  sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
  sendText: (data: string) => ws.send(data),
};

// Option 1: Forward to a local HTTP service
// (ForwardHandler always streams chunks as they arrive from upstream)
const handler = createHOWHandler("http://localhost:3000", sendable);

// Option 2: Pass a RequestListener directly (buffered by default — the whole
// response arrives as one HTTPResponse). Add `{ streaming: true }` to forward
// every `res.write(chunk)` as a separate HTTPResponseChunk instead.
const handler = createHOWHandler((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("hello");
}, sendable);

// Option 3: Text mode (JSON over WebSocket text frames)
const handler = createHOWHandler("http://localhost:3000", sendable, { mode: "text" });

// Receive messages
ws.on("message", (data, isBinary) => {
  if (isBinary) {
    handler.handleBinaryMessage(data);
  } else {
    handler.handleTextMessage(data.toString());
  }
});
```

### Caller — Send Requests

Send HTTP requests to a remote Handler:

```typescript
import { WebSocket } from "ws";
import { createHOWCaller } from "@byted/how";

const ws = new WebSocket("ws://your-server/ws");
const sendable = {
  sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
  sendText: (data: string) => ws.send(data),
};

// Binary mode (default)
const caller = createHOWCaller(sendable);

// Or text mode
const caller = createHOWCaller(sendable, { mode: "text" });

// Receive response messages
ws.on("message", (data, isBinary) => {
  if (isBinary) {
    caller.handleBinaryMessage(data);
  } else {
    caller.handleTextMessage(data.toString());
  }
});

// Send a request. `resp.body` is always a ReadableStream<Uint8Array>.
const resp = await caller.request({
  method: "GET",
  url: "/api/hello",
  headers: {},
});

console.log(resp.status_code);                         // 200
console.log(await new Response(resp.body).text());     // "hello"
```

### Streaming Responses

`resp.body` is always a `ReadableStream<Uint8Array>`. When the peer is non-streaming (single `HTTPResponse`), the stream yields the full body and closes; when the peer streams (`HTTPResponseStart` + chunks + `End`), each chunk arrives as it is produced. The reading code is identical either way:

```typescript
const resp = await caller.request({ method: "GET", url: "/stream", headers: {} });

const reader = resp.body.getReader();
while (true) {
  const { done, value } = await reader.read();
  if (done) break;
  process.stdout.write(value);
}
```

A Handler emits chunks when either:
- the handler is a string target (ForwardHandler is always streaming), or
- the handler is a `RequestListener` and `createHOWHandler` was called with `{ streaming: true }`.

### Transport Lifecycle

When the underlying WebSocket dies, call `caller.close(err)` — every pending request's body stream is errored with `err` (already-buffered chunks are delivered first), and further `request()` calls reject immediately. The call is idempotent.

Read timeout defaults to 30s and resets on every received message; pass `{ readTimeout: ms }` to `createHOWCaller` to override, or `0` / a negative value to disable.

## Go

### Installation

```bash
go get github.com/geminiwen/how/golang
```

### Handler — Receive and Handle Requests

The following example uses [Hertz](https://github.com/cloudwego/hertz) with [hertz-contrib/websocket](https://github.com/hertz-contrib/websocket) to set up a WebSocket server that handles HOW requests:

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

    "github.com/geminiwen/how/golang/client"
)

// wsSender implements Sendable for hertz-contrib/websocket.
type wsSender struct {
    conn *websocket.Conn
    mu   sync.Mutex
}

func (s *wsSender) SendBytes(data []byte) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (s *wsSender) SendText(data string) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.TextMessage, []byte(data))
}

var upgrader = websocket.HertzUpgrader{}

func main() {
    // Option 1: Forward to a local HTTP service
    // (ForwardHandler always streams chunks as they arrive from upstream)
    handler, _ := client.ForwardTo("http://localhost:3000")

    // Option 2: Wrap an http.Handler — buffered by default. Add
    // client.WithStreaming() to forward every w.Write as a separate
    // HTTPResponseChunk, which matters for SSE and other long-lived responses.
    mux := http.NewServeMux()
    mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprint(w, "hello")
    })
    handler = client.HTTPHandler(mux)
    // handler = client.HTTPHandler(mux, client.WithStreaming())

    h := server.Default(server.WithHostPorts("0.0.0.0:8888"))
    h.NoHijackConnPool = true

    h.GET("/ws", func(ctx context.Context, c *app.RequestContext) {
        upgrader.Upgrade(c, func(conn *websocket.Conn) {
            sender := &wsSender{conn: conn}
            // Binary mode (default)
            howHandler := client.NewHandler(handler, sender)
            // Or text mode:
            // howHandler := client.NewHandler(handler, sender, client.WithHandlerTextMode())

            for {
                msgType, data, err := conn.ReadMessage()
                if err != nil {
                    log.Println("read:", err)
                    break
                }
                if msgType == websocket.TextMessage {
                    howHandler.HandleTextMessage(ctx, string(data))
                } else {
                    howHandler.HandleBinaryMessage(ctx, data)
                }
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
    "io"
    "log"
    "sync"

    "github.com/hertz-contrib/websocket"

    "github.com/geminiwen/how/golang/client"
    "github.com/geminiwen/how/golang/protocol"
)

// wsSender implements Sendable for hertz-contrib/websocket.
type wsSender struct {
    conn *websocket.Conn
    mu   sync.Mutex
}

func (s *wsSender) SendBytes(data []byte) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (s *wsSender) SendText(data string) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.conn.WriteMessage(websocket.TextMessage, []byte(data))
}

func main() {
    dialer := websocket.DefaultDialer
    conn, _, err := dialer.Dial("ws://localhost:8888/ws", nil)
    if err != nil {
        log.Fatal("dial:", err)
    }
    defer conn.Close()

    sender := &wsSender{conn: conn}
    // Binary mode (default)
    caller := client.NewCaller(sender)
    // Or text mode:
    // caller := client.NewCaller(sender, client.WithTextMode())

    // Read loop in background
    go func() {
        for {
            msgType, data, err := conn.ReadMessage()
            if err != nil {
                return
            }
            if msgType == websocket.TextMessage {
                caller.HandleTextMessage(context.Background(), string(data))
            } else {
                caller.HandleBinaryMessage(context.Background(), data)
            }
        }
    }()

    // Send a request. resp.Body is an io.ReadCloser that you MUST close —
    // otherwise the pending request leaks until the transport dies, and a
    // finalizer will log a warning.
    resp, err := caller.Request(context.Background(), &protocol.HTTPRequestPayload{
        Method:  "GET",
        URL:     "/hello",
        Headers: map[string][]string{},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(resp.StatusCode)  // 200
    fmt.Println(string(body))     // "hello"
}
```

`resp.Body` yields bytes as they arrive: a non-streaming peer's buffered body reads out and then EOFs; a streaming peer's chunks read incrementally. For the common "just give me the whole body" case, use `io.ReadAll(resp.Body)` as above.

When the underlying WebSocket dies, call `caller.Close(err)` — every pending `Body.Read` returns `err` (or `ErrTransportClosed` when `err` is nil), and further `Request` calls return the same error immediately. `Caller.ReadTimeout` defaults to 30s and resets on every received frame; set it to a negative duration to disable.

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

Serialization modes:
- **Binary** — `0x69` + [MessagePack](https://msgpack.org/) over WebSocket binary frames (default)
- **Text** — JSON over WebSocket text frames

## Related Specifications

- [RFC 9110 — HTTP Semantics](https://datatracker.ietf.org/doc/html/rfc9110)
- [RFC 6455 — The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455)
- [RFC 9562 — UUID v4](https://datatracker.ietf.org/doc/html/rfc9562#section-5.4)
- [RFC 2119 — Requirement Levels](https://datatracker.ietf.org/doc/html/rfc2119)
- [MessagePack Specification](https://msgpack.org/)

## License

MIT
