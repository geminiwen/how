# HTTP over WebSocket (HOWS) Protocol Specification

**Version**: 0.2.0
**Status**: Draft
**Date**: 2026-03-06

## 1. Abstract

This document specifies the HTTP over WebSocket (HOWS) protocol, which enables a server to send HTTP requests to clients through a WebSocket connection initiated by the client. This reverses the traditional HTTP request direction, allowing servers to reach clients behind firewalls or NAT without requiring the client to open any inbound ports.

The protocol defines a message format using MessagePack serialization over WebSocket binary frames, supporting concurrent request multiplexing over a single connection.

## 2. Terminology

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in [RFC 2119](https://datatracker.ietf.org/doc/html/rfc2119).

- **HOWS Server**: The component that accepts WebSocket connections from clients and dispatches HTTP requests to them.
- **HOWS Client**: The component that initiates a WebSocket connection to the server, receives HTTP requests, and returns HTTP responses.
- **Upstream Caller**: An external HTTP client that sends requests to the HOWS Server's proxy endpoint, which are then forwarded to the appropriate HOWS Client.
- **Client ID**: A unique string identifier that each HOWS Client uses to register with the server, enabling request routing.
- **Request ID**: A unique string (UUID v4) assigned to each HTTP request for multiplexing purposes.

## 3. Protocol Overview

```
+------------------+         +------------------+         +------------------+
|  Upstream Caller  | ------> |   HOWS Server    | ------> |   HOWS Client    |
|  (HTTP Client)   |  HTTP   | (Proxy + WS Hub) |   WS    | (Handler/Proxy)  |
+------------------+         +------------------+         +------------------+
                                     ^                            |
                                     |         WebSocket          |
                                     +------- (client-initiated) -+
```

1. The HOWS Client establishes a WebSocket connection to the HOWS Server.
2. The Client sends a `Register` message with its Client ID.
3. The Server acknowledges with a `RegisterAck` message.
4. An Upstream Caller sends an HTTP request to the Server's proxy endpoint, specifying the target Client ID.
5. The Server serializes the HTTP request into an `HTTPRequest` message and sends it to the Client over WebSocket.
6. The Client processes the request (either forwarding to a local service or handling internally) and returns either:
   - A single `HTTPResponse` message (non-streaming), or
   - A sequence of `HTTPResponseStart`, one or more `HTTPResponseChunk`, and `HTTPResponseEnd` messages (streaming).
7. The Server deserializes the response and returns it to the Upstream Caller as a standard HTTP response. For streaming responses, the Server flushes each chunk immediately.

## 4. Transport

### 4.1. WebSocket Connection

The HOWS protocol operates over WebSocket as defined in [RFC 6455](https://datatracker.ietf.org/doc/html/rfc6455).

- The Client MUST initiate the WebSocket connection to the Server.
- The connection SHOULD use `wss://` (WebSocket Secure) in production environments.
- The WebSocket subprotocol MUST be `hows.v1`. The Client MUST include this in the `Sec-WebSocket-Protocol` header during the handshake, and the Server MUST select it in the response.
- All HOWS messages MUST be sent as WebSocket **binary frames**.

### 4.2. Server Endpoints

The HOWS Server exposes two HTTP endpoint groups:

- **WebSocket endpoint**: `GET /ws` - Accepts WebSocket upgrade requests from HOWS Clients.
- **Proxy endpoint**: `{METHOD} /proxy/{client_id}/{path}` - Accepts HTTP requests from Upstream Callers. The `{client_id}` segment identifies the target HOWS Client. The remaining `{path}` (including query string) is forwarded as the request URL.

## 5. Message Format

### 5.1. Serialization

All messages are serialized using [MessagePack](https://msgpack.org/). MessagePack is a binary serialization format that is compact, fast, and supported across many programming languages.

### 5.2. Envelope

Every WebSocket message is an Envelope, serialized as a MessagePack map with the following fields:

| Field | MessagePack Type | Required | Description |
|-------|-----------------|----------|-------------|
| `type` | uint8 | REQUIRED | Message type identifier (see Section 6) |
| `request_id` | str | CONDITIONAL | Request identifier for multiplexing. REQUIRED for `HTTPRequest` and `HTTPResponse` messages. |
| `payload` | map or nil | CONDITIONAL | Message-specific payload. Structure depends on `type`. |

### 5.3. Field Names

MessagePack map keys MUST be strings. Implementations SHOULD use short field names as defined in this specification to minimize overhead.

## 6. Message Types

### 6.1. Type Registry

| Type Name | Value | Direction | Description |
|-----------|-------|-----------|-------------|
| `Register` | `0x01` | Client -> Server | Client registration |
| `RegisterAck` | `0x02` | Server -> Client | Registration acknowledgement |
| `HTTPRequest` | `0x10` | Server -> Client | HTTP request |
| `HTTPResponse` | `0x11` | Client -> Server | HTTP response (non-streaming) |
| `HTTPResponseStart` | `0x12` | Client -> Server | Streaming response start (headers only) |
| `HTTPResponseChunk` | `0x13` | Client -> Server | Streaming response body chunk |
| `HTTPResponseEnd` | `0x14` | Client -> Server | Streaming response end |
| `Ping` | `0x20` | Bidirectional | Heartbeat request |
| `Pong` | `0x21` | Bidirectional | Heartbeat response |
| `Error` | `0xFF` | Bidirectional | Error notification |

### 6.2. Register (0x01)

Sent by the Client immediately after WebSocket connection is established.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `client_id` | str | REQUIRED | Unique identifier for this client. MUST be non-empty. MUST match the regex `^[a-zA-Z0-9_\-\.]{1,64}$`. |

### 6.3. RegisterAck (0x02)

Sent by the Server in response to a `Register` message.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ok` | bool | REQUIRED | `true` if registration succeeded, `false` otherwise. |
| `error` | str | CONDITIONAL | Error message. REQUIRED if `ok` is `false`. |

Registration MUST fail if:
- The `client_id` is empty or does not match the required format.
- A Client with the same `client_id` is already connected.

### 6.4. HTTPRequest (0x10)

Sent by the Server to the Client to deliver an HTTP request.

**Envelope**: The `request_id` field MUST be set to a UUID v4 string.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `method` | str | REQUIRED | HTTP method (GET, POST, PUT, DELETE, etc.) |
| `url` | str | REQUIRED | Request URL (path + query string, e.g., `/api/users?page=1`) |
| `headers` | map[str][]str | REQUIRED | HTTP headers. Each key maps to an array of string values. |
| `body` | bin | OPTIONAL | Request body as raw bytes. MAY be omitted or empty for methods without a body. |

### 6.5. HTTPResponse (0x11)

Sent by the Client to the Server in response to an `HTTPRequest`.

**Envelope**: The `request_id` field MUST match the `request_id` of the corresponding `HTTPRequest`.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `status_code` | uint16 | REQUIRED | HTTP status code (e.g., 200, 404, 500). |
| `headers` | map[str][]str | REQUIRED | HTTP response headers. |
| `body` | bin | OPTIONAL | Response body as raw bytes. |

### 6.6. HTTPResponseStart (0x12)

Sent by the Client to begin a **streaming** response. This is an alternative to `HTTPResponse` for cases where the response body is generated incrementally (e.g., Server-Sent Events, large file downloads).

**Envelope**: The `request_id` field MUST match the `request_id` of the corresponding `HTTPRequest`.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `status_code` | uint16 | REQUIRED | HTTP status code. |
| `headers` | map[str][]str | REQUIRED | HTTP response headers. |

Upon receiving `HTTPResponseStart`, the Server MUST immediately write the status code and headers to the Upstream Caller's HTTP response and flush. The Server MUST NOT close the response until it receives `HTTPResponseEnd` or an error occurs.

### 6.7. HTTPResponseChunk (0x13)

Sent by the Client to deliver a chunk of response body data during a streaming response.

**Envelope**: The `request_id` field MUST match the `request_id` of the corresponding `HTTPRequest`.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `data` | bin | REQUIRED | A chunk of response body bytes. MUST be non-empty. |

Upon receiving `HTTPResponseChunk`, the Server MUST write the data to the Upstream Caller's HTTP response and flush immediately.

### 6.8. HTTPResponseEnd (0x14)

Sent by the Client to signal the end of a streaming response.

**Envelope**: The `request_id` field MUST match the `request_id` of the corresponding `HTTPRequest`.

**Payload**: nil.

Upon receiving `HTTPResponseEnd`, the Server MUST close the Upstream Caller's HTTP response.

### 6.9. Ping (0x20)

Heartbeat request. The `payload` is nil.

The receiver MUST respond with a `Pong` message. The `request_id` from the `Ping` MUST be echoed in the `Pong`.

### 6.10. Pong (0x21)

Heartbeat response. The `payload` is nil.

The `request_id` MUST match the corresponding `Ping`.

### 6.11. Error (0xFF)

Sent to indicate a protocol-level error.

**Payload**:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `code` | uint16 | REQUIRED | Error code (see Section 10). |
| `message` | str | REQUIRED | Human-readable error description. |

If the `Error` is in response to a specific request, the `request_id` SHOULD be set.

## 7. Connection Lifecycle

### 7.1. Connection Establishment

1. The Client initiates a WebSocket connection to the Server's WebSocket endpoint.
2. The Client MUST include `hows.v1` in the `Sec-WebSocket-Protocol` header.
3. Upon successful WebSocket handshake, the Client MUST send a `Register` message as its first message.
4. The Client MUST NOT send any other messages before receiving a `RegisterAck`.
5. The Server MUST respond with a `RegisterAck` within 5 seconds. If the Client does not receive a `RegisterAck` within this time, it SHOULD close the connection and retry.

### 7.2. Heartbeat

Either side MAY send `Ping` messages to verify the connection is alive.

- The sender SHOULD send a `Ping` at regular intervals (RECOMMENDED: every 30 seconds).
- The receiver MUST respond with a `Pong` within 10 seconds.
- If a `Pong` is not received within 10 seconds, the sender SHOULD consider the connection dead and close it.

Note: This is an application-level heartbeat, separate from WebSocket protocol-level ping/pong frames. Implementations MAY additionally use WebSocket-level ping/pong.

### 7.3. Graceful Disconnection

Either side MAY initiate a graceful disconnection by sending a WebSocket Close frame with status code 1000 (Normal Closure).

Before closing:
- The Server SHOULD wait for all pending `HTTPResponse` messages or cancel them with an `Error` message.
- The Client SHOULD complete processing of any in-flight requests before closing.

### 7.4. Reconnection

The Client SHOULD implement automatic reconnection with exponential backoff:
- Initial delay: 1 second
- Maximum delay: 30 seconds
- Backoff factor: 2x
- The Client SHOULD add random jitter (0-500ms) to avoid thundering herd.

## 8. Request-Response Flow

### 8.1. Proxy Request Flow

When the HOWS Server receives an HTTP request from an Upstream Caller at the proxy endpoint:

1. The Server extracts the `client_id` from the URL path.
2. The Server looks up the Client in the registry.
   - If the Client is not connected, the Server MUST respond to the Upstream Caller with HTTP `502 Bad Gateway` and a body indicating the client is not available.
3. The Server generates a UUID v4 `request_id`.
4. The Server constructs an `HTTPRequest` message from the Upstream Caller's HTTP request:
   - `method`: The HTTP method from the upstream request.
   - `url`: The path after the `client_id` segment, including query string.
   - `headers`: All headers from the upstream request. The Server SHOULD add an `X-Forwarded-For` header.
   - `body`: The request body (if any).
5. The Server sends the `HTTPRequest` over the WebSocket connection.
6. The Server waits for an `HTTPResponse` with the matching `request_id`.
   - The Server MUST enforce a timeout (RECOMMENDED: 60 seconds). On timeout, respond to the Upstream Caller with HTTP `504 Gateway Timeout`.
7. Upon receiving the `HTTPResponse`, the Server writes the HTTP response to the Upstream Caller:
   - Status code from `status_code`.
   - Headers from `headers`.
   - Body from `body`.

### 8.2. Client Request Handling (Non-Streaming)

When the HOWS Client receives an `HTTPRequest` message and the response body is fully available:

1. The Client extracts the `request_id` from the envelope.
2. The Client processes the request through its configured handler:
   - **Forward mode**: The Client forwards the request to a local HTTP service and captures the response.
   - **Embedded mode**: The Client processes the request using an in-process handler.
3. The Client constructs an `HTTPResponse` message with the same `request_id`.
4. The Client sends the `HTTPResponse` over the WebSocket connection.

If the Client encounters an error while processing:
- The Client SHOULD send an `HTTPResponse` with an appropriate HTTP error status code (e.g., 502 for forwarding failures, 500 for internal errors).
- Alternatively, the Client MAY send an `Error` message with the same `request_id`.

### 8.3. Client Request Handling (Streaming)

When the HOWS Client receives an `HTTPRequest` message and the response body is generated incrementally (e.g., SSE, large downloads):

1. The Client extracts the `request_id` from the envelope.
2. The Client begins processing the request.
3. As soon as the status code and headers are available, the Client sends an `HTTPResponseStart` message with the same `request_id`.
4. As response body data becomes available, the Client sends one or more `HTTPResponseChunk` messages with the same `request_id`.
5. When the response is complete, the Client sends an `HTTPResponseEnd` message with the same `request_id`.

The Client MUST NOT send both `HTTPResponse` and `HTTPResponseStart` for the same `request_id`. It MUST choose one response mode per request.

If an error occurs during streaming:
- If `HTTPResponseStart` has not been sent yet, the Client SHOULD send an `HTTPResponse` with an error status code.
- If `HTTPResponseStart` has already been sent, the Client SHOULD send an `HTTPResponseEnd` to terminate the stream. The Server will close the upstream connection.

### 8.4. Server Streaming Response Handling

When the Server receives streaming response messages:

1. On `HTTPResponseStart`: Write the status code and headers to the Upstream Caller, then flush.
2. On `HTTPResponseChunk`: Write the chunk data and flush immediately.
3. On `HTTPResponseEnd`: Close the response to the Upstream Caller.

The Server MUST still enforce a timeout on streaming responses. The timeout SHOULD be reset on each received `HTTPResponseChunk` (activity-based timeout rather than absolute timeout).

## 9. Multiplexing

The protocol supports multiple concurrent HTTP requests over a single WebSocket connection.

- Each request is identified by a unique `request_id` (UUID v4).
- The Server MAY send multiple `HTTPRequest` messages without waiting for responses.
- The Client MAY process requests concurrently and return responses in any order.
- Both sides MUST correctly match responses to requests using the `request_id`.
- Implementations SHOULD limit the number of concurrent in-flight requests per connection (RECOMMENDED maximum: 100).

## 10. Error Handling

### 10.1. Error Codes

| Code | Name | Description |
|------|------|-------------|
| `1000` | `UnknownError` | An unspecified error occurred. |
| `1001` | `ClientNotFound` | The specified client_id is not connected. |
| `1002` | `RequestTimeout` | The request timed out waiting for a response. |
| `1003` | `InvalidMessage` | The received message could not be decoded or is malformed. |
| `1004` | `RegistrationFailed` | Client registration was rejected. |
| `1005` | `ClientIDConflict` | A client with this ID is already connected. |
| `1006` | `InternalError` | An internal error occurred in the sender. |
| `1007` | `RequestCancelled` | The request was cancelled (e.g., Upstream Caller disconnected). |

### 10.2. Error Behavior

- Upon receiving an `Error` message with a `request_id`, the pending request with that ID MUST be resolved as failed.
- Upon receiving an `Error` message without a `request_id`, the recipient SHOULD log the error. If the error is unrecoverable, the recipient SHOULD close the connection.
- If a WebSocket connection is unexpectedly closed while requests are in-flight, all pending requests MUST be resolved as failed.

## 11. Security Considerations

### 11.1. Transport Security

- Implementations SHOULD use `wss://` (TLS) for all WebSocket connections in production.
- The Server SHOULD validate the TLS certificate chain.

### 11.2. Authentication

This specification does not define an authentication mechanism. Implementations SHOULD provide authentication through one or more of the following:

- A pre-shared token sent in the WebSocket handshake (e.g., via an `Authorization` header or query parameter).
- Mutual TLS (mTLS) for client certificate verification.

### 11.3. Authorization

- The Server SHOULD validate that the Client is authorized to register with the given `client_id`.
- The Server MAY implement access control on the proxy endpoint to restrict which Upstream Callers can access which Clients.

### 11.4. Request Limits

- Implementations SHOULD enforce maximum message sizes to prevent memory exhaustion (RECOMMENDED: 10 MB per message).
- Implementations SHOULD enforce maximum concurrent request limits per connection.
- The Server SHOULD implement rate limiting on the proxy endpoint.

### 11.5. Header Sanitization

- The Server SHOULD strip or sanitize hop-by-hop headers (e.g., `Connection`, `Transfer-Encoding`, `Upgrade`) when proxying.
- The Server SHOULD add proxy-related headers (`X-Forwarded-For`, `X-Forwarded-Proto`) to forwarded requests.
