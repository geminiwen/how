# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is this?

HOWS (HTTP over WebSocket) is a protocol library for tunneling HTTP requests through WebSocket connections. It is a **pure library** — no server, no connection management, no routing. Users manage their own WebSocket connections and wire up the two roles:

- **Caller**: sends `HTTPRequest`, waits for `HTTPResponse` (like an http.Client)
- **Handler**: receives `HTTPRequest`, dispatches to a local handler, sends back `HTTPResponse`

The protocol spec lives in `spec.md`. Two implementations exist side by side: TypeScript and Go.

## Build & Test

### TypeScript
```bash
cd typescript
npm install            # first time only
npx tsc --noEmit       # type check
npx tsx --test src/**/*.test.ts   # run all tests
npx tsx --test src/client/client.test.ts  # run a single test file
```

### Go
```bash
cd golang
go build ./...
go test ./...
go test ./client/       # run tests for a single package
```

## Architecture

Both implementations share the same structure:

### Protocol layer (`protocol/`)
MessagePack-based envelope format. Message types: `HTTPRequest` (0x10), `HTTPResponse` (0x11), `HTTPResponseStart` (0x12), `HTTPResponseChunk` (0x13), `HTTPResponseEnd` (0x14), `Error` (0xFF). Core functions: `marshal`/`unmarshal`, factory functions (`newHTTPRequest`, etc.), and `decodePayload` for typed payload extraction.

### Client layer (`client/`)
**Caller** — `createHOWSCaller(sender)` (TS) / `NewCaller(sender)` (Go):
- `request()` generates a UUID, sends `HTTPRequest` via `Sendable`, returns a Promise/blocks for response
- `handleMessage()` routes incoming messages by `request_id` to pending requests
- Streaming: `HTTPResponseStart` resolves with a `ReadableStream` body (TS) or accumulates chunks into a complete response (Go)

**Handler** — `createHOWSHandler(handler, sender)` (TS) / `NewHandler(handler, sender)` (Go):
- `handleMessage()` dispatches `HTTPRequest` to the provided handler
- Two built-in handler types: `ForwardHandler` (proxy to URL) and `HTTPHandlerAdapter` (wraps `http.RequestListener` / `http.Handler`)
- Streaming responses use `ResponseWriter` (writeHeader → write chunks → close)

**Sendable interface** — the only transport abstraction: `send(data)` / `Send(data []byte) error`. Users wrap their WebSocket (or any transport) to implement this.

### CLI (`cli/`)
Minimal CLI client that connects a WebSocket to a `HOWSHandler` forwarding to a target URL.

### Tests
Both TS and Go integration tests run over **real WebSocket connections** — they start an HTTP server with WebSocket upgrade, connect a Caller on one end and a Handler on the other, and verify end-to-end request/response flow.
