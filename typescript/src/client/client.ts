import http from "node:http";
import { randomUUID } from "node:crypto";
import {
  MessageType,
  marshal,
  unmarshal,
  marshalText,
  unmarshalText,
  decodePayload,
  newHTTPRequest,
  newHTTPResponse,
  newHTTPResponseStart,
  newHTTPResponseChunk,
  newHTTPResponseEnd,
  newError,
  ErrorCode,
} from "../protocol/index";
import type {
  HTTPRequestPayload,
  HTTPResponsePayload,
  HTTPResponseStartPayload,
  HTTPResponseChunkPayload,
  ErrorPayload,
  Envelope,
  HOWResponse,
} from "../protocol/index";

export interface Sendable {
  sendBytes(data: Buffer | Uint8Array): void;
  sendText(data: string): void;
}

// ─── Caller ───

export interface HOWCallerOptions {
  /** Read timeout in milliseconds. Each received message resets the timer.
   *  Default: 30000 (30s). Set to 0 or negative to disable. */
  readTimeout?: number;
  /** Serialization mode. Default: 'binary'. */
  mode?: 'binary' | 'text';
}

export interface HOWCaller {
  /** Send an HTTPRequest and resolve when the peer starts responding.
   *  `resp.body` is always a `ReadableStream<Uint8Array>`:
   *  - Non-streaming peer (single HTTPResponse): the stream yields the full
   *    body, then closes.
   *  - Streaming peer (HTTPResponseStart + Chunks + End): the stream yields
   *    each chunk as it arrives, closes on End. If the transport dies
   *    mid-stream, the reader sees an error. */
  request(req: HTTPRequestPayload): Promise<HOWResponse>;
  handleBinaryMessage(data: Buffer | Uint8Array): void;
  handleTextMessage(data: string): void;
  /** Signal that the underlying transport has died. Every pending request's
   *  body stream is errored with `err`. Further `request()` calls reject
   *  immediately. Idempotent. */
  close(err?: Error): void;
}

/** Default read timeout in milliseconds. */
export const DEFAULT_READ_TIMEOUT = 30_000;

interface PendingRequest {
  resolve: (resp: HOWResponse) => void;
  reject: (err: Error) => void;
  timer?: ReturnType<typeof setTimeout>;
  // Pull-based body channel for streaming responses. Set on HTTPResponseStart.
  // We maintain our own chunk buffer (not the one inside ReadableStream) so
  // that `streamError` can deliver the terminal error AFTER draining any
  // already-enqueued bytes — ReadableStreamDefaultController.error() discards
  // the internal queue, which would silently truncate slow-reader responses.
  streamPush?: (chunk: Uint8Array) => void;
  streamClose?: () => void;
  streamError?: (err: Error) => void;
}

export function createHOWCaller(sender: Sendable, options?: HOWCallerOptions): HOWCaller {
  const pending = new Map<string, PendingRequest>();
  const readTimeout = options?.readTimeout ?? DEFAULT_READ_TIMEOUT;
  const mode = options?.mode ?? 'binary';
  let closedErr: Error | undefined;

  function startTimer(requestID: string, entry: PendingRequest): void {
    if (readTimeout <= 0) return;
    clearTimer(entry);
    entry.timer = setTimeout(() => {
      pending.delete(requestID);
      const err = new Error("read timeout");
      if (entry.streamError) {
        entry.streamError(err);
      } else {
        entry.reject(err);
      }
    }, readTimeout);
  }

  function clearTimer(entry: PendingRequest): void {
    if (entry.timer !== undefined) {
      clearTimeout(entry.timer);
      entry.timer = undefined;
    }
  }

  function sendEnvelope(env: Envelope): void {
    if (mode === 'text') {
      sender.sendText(marshalText(env));
    } else {
      sender.sendBytes(Buffer.from(marshal(env)));
    }
  }

  function processEnvelope(env: Envelope): void {
    const requestID = env.request_id ?? "";
    const entry = pending.get(requestID);
    if (!entry) return;

    // Reset read timeout on every received message.
    startTimer(requestID, entry);

    switch (env.type) {
      case MessageType.HTTPResponse: {
        // Non-streaming peer: wrap the buffered body in a single-chunk
        // ReadableStream so the caller-facing API stays uniform.
        clearTimer(entry);
        pending.delete(requestID);
        const payload = decodePayload<HTTPResponsePayload>(env);
        const body = payload.body;
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            if (body && body.length > 0) {
              controller.enqueue(new Uint8Array(body as ArrayLike<number>));
            }
            controller.close();
          },
        });
        entry.resolve({
          status_code: payload.status_code,
          headers: payload.headers,
          body: stream,
        });
        break;
      }

      case MessageType.HTTPResponseStart: {
        const start = decodePayload<HTTPResponseStartPayload>(env);

        // Pull-based stream with an external chunk buffer. ReadableStream's
        // own queue would be erased by `.error()`, so we keep chunks in our
        // own array and only surface a terminal error once the buffer has
        // been drained by the reader.
        const buffer: Uint8Array[] = [];
        let terminalErr: Error | undefined;
        let terminalClose = false;
        let notify: (() => void) | undefined;

        entry.streamPush = (chunk) => {
          buffer.push(chunk);
          notify?.();
        };
        entry.streamClose = () => {
          terminalClose = true;
          notify?.();
        };
        entry.streamError = (err) => {
          terminalErr = err;
          notify?.();
        };

        const stream = new ReadableStream<Uint8Array>({
          async pull(controller) {
            while (buffer.length === 0 && !terminalErr && !terminalClose) {
              await new Promise<void>((resolve) => { notify = resolve; });
            }
            notify = undefined;
            if (buffer.length > 0) {
              controller.enqueue(buffer.shift()!);
              return;
            }
            if (terminalErr) {
              controller.error(terminalErr);
              return;
            }
            // terminalClose
            controller.close();
          },
          // Consumer-initiated cancellation (reader.cancel() / stream.cancel()).
          // The HOW protocol has no upstream "cancel" message, so we can't tell
          // the peer to stop — but we drop our pending entry and release the
          // push callbacks so later chunks/end/error are no-ops.
          //
          // Note: `entry.resolve` fires below with this stream as `resp.body`
          // before cancel can ever run, so there is no unresolved request
          // promise here — no `entry.reject` call is needed on cancel.
          cancel: () => {
            clearTimer(entry);
            entry.streamPush = undefined;
            entry.streamClose = undefined;
            entry.streamError = undefined;
            buffer.length = 0;
            pending.delete(requestID);
          },
        });
        entry.resolve({
          status_code: start.status_code,
          headers: start.headers,
          body: stream,
        });
        break;
      }

      case MessageType.HTTPResponseChunk: {
        const chunk = decodePayload<HTTPResponseChunkPayload>(env);
        entry.streamPush?.(new Uint8Array(chunk.data as ArrayLike<number>));
        break;
      }

      case MessageType.HTTPResponseEnd: {
        clearTimer(entry);
        entry.streamClose?.();
        pending.delete(requestID);
        break;
      }

      case MessageType.Error: {
        clearTimer(entry);
        pending.delete(requestID);
        const errPayload = decodePayload<ErrorPayload>(env);
        const err = new Error(errPayload.message);
        if (entry.streamError) {
          // Streamed response: feed error into the buffer so any already-enqueued
          // chunks are delivered first.
          entry.streamError(err);
        } else {
          entry.reject(err);
        }
        break;
      }
    }
  }

  return {
    request(req: HTTPRequestPayload): Promise<HOWResponse> {
      if (closedErr) return Promise.reject(closedErr);
      return new Promise((resolve, reject) => {
        const requestID = randomUUID();
        const entry: PendingRequest = { resolve, reject };
        pending.set(requestID, entry);
        startTimer(requestID, entry);
        const env = newHTTPRequest(requestID, req);
        sendEnvelope(env);
      });
    },

    handleBinaryMessage(data: Buffer | Uint8Array): void {
      try {
        const env = unmarshal(data);
        processEnvelope(env);
      } catch (err) {
        const message = err instanceof Error ? err.message : "unknown error";
        console.error(`caller message handling error: ${message}`);
      }
    },

    handleTextMessage(data: string): void {
      try {
        const env = unmarshalText(data);
        processEnvelope(env);
      } catch (err) {
        const message = err instanceof Error ? err.message : "unknown error";
        console.error(`caller text message handling error: ${message}`);
      }
    },

    close(err?: Error): void {
      if (closedErr) return;
      closedErr = err ?? new Error("transport closed");
      for (const entry of pending.values()) {
        clearTimer(entry);
        if (entry.streamError) {
          // Already-buffered chunks will be drained by the reader before the
          // terminal error surfaces.
          entry.streamError(closedErr);
        } else {
          entry.reject(closedErr);
        }
      }
      pending.clear();
    },
  };
}

// ─── Handler ───

export interface HOWHandlerOptions {
  /** Serialization mode. Default: 'binary'. */
  mode?: 'binary' | 'text';
  /** When true, an `http.RequestListener` passed as handler is driven as a
   *  streaming source: every `res.write(chunk)` is forwarded as a separate
   *  `HTTPResponseChunk`, so the caller sees `resp.body` as a `ReadableStream`.
   *  Default: false — preserves the historic buffered behavior where the
   *  whole response body arrives as a single `HTTPResponse` and `resp.body`
   *  is a complete `Uint8Array`.
   *  Has no effect when `handler` is a string (ForwardHandler is always streaming). */
  streaming?: boolean;
}

export interface HOWHandler {
  handleBinaryMessage(data: Buffer | Uint8Array): void;
  handleTextMessage(data: string): void;
}

interface Handler {
  serveHTTPOverWS(
    req: HTTPRequestPayload,
  ): Promise<HTTPResponsePayload>;
}

interface StreamingHandler {
  serveHTTPOverWSStream(
    req: HTTPRequestPayload,
    w: ResponseWriter,
  ): Promise<void>;
}

class ResponseWriter {
  private readonly requestID: string;
  private readonly sendFn: (env: Envelope) => void;
  started = false;

  constructor(requestID: string, sendFn: (env: Envelope) => void) {
    this.requestID = requestID;
    this.sendFn = sendFn;
  }

  writeHeader(statusCode: number, headers: Record<string, string[]>): void {
    if (this.started) return;
    this.started = true;
    this.sendFn(newHTTPResponseStart(this.requestID, statusCode, headers));
  }

  write(data: Uint8Array): void {
    this.sendFn(newHTTPResponseChunk(this.requestID, data));
  }

  close(): void {
    this.sendFn(newHTTPResponseEnd(this.requestID));
  }
}

class ForwardHandler implements Handler, StreamingHandler {
  private readonly target: URL;

  constructor(target: string) {
    this.target = new URL(target);
  }

  async serveHTTPOverWS(
    req: HTTPRequestPayload,
  ): Promise<HTTPResponsePayload> {
    const url = this.target.origin + req.url;

    const headers: Record<string, string> = {};
    for (const [key, values] of Object.entries(req.headers)) {
      headers[key] = values.join(", ");
    }

    const resp = await fetch(url, {
      method: req.method,
      headers,
      body:
        req.body && req.body.length > 0
          ? Buffer.from(req.body)
          : undefined,
      // @ts-expect-error - Node.js fetch supports duplex
      duplex: req.body ? "half" : undefined,
    });

    const respHeaders: Record<string, string[]> = {};
    resp.headers.forEach((value, key) => {
      respHeaders[key] = [value];
    });

    const body = new Uint8Array(await resp.arrayBuffer());

    return {
      status_code: resp.status,
      headers: respHeaders,
      body: body.length > 0 ? body : undefined,
    };
  }

  async serveHTTPOverWSStream(
    req: HTTPRequestPayload,
    w: ResponseWriter,
  ): Promise<void> {
    const url = this.target.origin + req.url;

    const headers: Record<string, string> = {};
    for (const [key, values] of Object.entries(req.headers)) {
      headers[key] = values.join(", ");
    }

    const resp = await fetch(url, {
      method: req.method,
      headers,
      body:
        req.body && req.body.length > 0
          ? Buffer.from(req.body)
          : undefined,
      // @ts-expect-error - Node.js fetch supports duplex
      duplex: req.body ? "half" : undefined,
    });

    const respHeaders: Record<string, string[]> = {};
    resp.headers.forEach((value, key) => {
      respHeaders[key] = [value];
    });

    w.writeHeader(resp.status, respHeaders);

    if (resp.body) {
      const reader = resp.body.getReader();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        w.write(value);
      }
    }

    w.close();
  }
}

class HTTPHandlerAdapter implements Handler {
  private readonly handler: http.RequestListener;
  /** Attached in the constructor when `streaming` is true, so that
   *  `createHOWHandler` detects the `StreamingHandler` interface and routes
   *  responses through `HTTPResponseStart`/`Chunk`/`End`. */
  serveHTTPOverWSStream?: (req: HTTPRequestPayload, w: ResponseWriter) => Promise<void>;

  constructor(handler: http.RequestListener, streaming: boolean) {
    this.handler = handler;
    if (streaming) {
      this.serveHTTPOverWSStream = this.streamImpl;
    }
  }

  private proxy(
    req: HTTPRequestPayload,
    onResponse: (httpRes: http.IncomingMessage, cleanup: () => void) => void,
    onError: (err: Error, cleanup: () => void) => void,
  ): void {
    const server = http.createServer((httpReq, httpRes) => {
      this.handler(httpReq, httpRes);
    });

    // httpReq is populated after `listen`; cleanup tears down both the
    // loopback request and the temporary server, so callers that detect a
    // transport failure mid-stream can force the wrapped handler to stop
    // instead of letting it run to completion.
    let httpReq: http.ClientRequest | undefined;
    const cleanup = () => {
      if (httpReq && !httpReq.destroyed) {
        try { httpReq.destroy(); } catch {}
      }
      server.close();
    };

    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") {
        onError(new Error("failed to get server address"), cleanup);
        return;
      }

      const url = `http://127.0.0.1:${addr.port}${req.url}`;
      const headers: Record<string, string> = {};
      for (const [key, values] of Object.entries(req.headers)) {
        headers[key] = values.join(", ");
      }

      httpReq = http.request(
        url,
        { method: req.method, headers },
        (httpRes) => onResponse(httpRes, cleanup),
      );

      httpReq.on("error", (err) => onError(err, cleanup));

      if (req.body && req.body.length > 0) {
        httpReq.write(Buffer.from(req.body));
      }
      httpReq.end();
    });
  }

  serveHTTPOverWS(
    req: HTTPRequestPayload,
  ): Promise<HTTPResponsePayload> {
    return new Promise((resolve, reject) => {
      this.proxy(
        req,
        (httpRes, cleanup) => {
          const chunks: Buffer[] = [];
          httpRes.on("data", (chunk: Buffer) => chunks.push(chunk));
          httpRes.on("end", () => {
            const body = Buffer.concat(chunks);
            const respHeaders: Record<string, string[]> = {};
            for (const [key, value] of Object.entries(httpRes.headers)) {
              if (value === undefined) continue;
              respHeaders[key] = Array.isArray(value) ? value : [value];
            }
            cleanup();
            resolve({
              status_code: httpRes.statusCode ?? 500,
              headers: respHeaders,
              body: body.length > 0 ? body : undefined,
            });
          });
          httpRes.on("error", (err) => {
            cleanup();
            reject(err);
          });
        },
        (err, cleanup) => {
          cleanup();
          reject(err);
        },
      );
    });
  }

  private streamImpl = (
    req: HTTPRequestPayload,
    w: ResponseWriter,
  ): Promise<void> => {
    return new Promise((resolve, reject) => {
      this.proxy(
        req,
        (httpRes, cleanup) => {
          let settled = false;
          // Any failure path (sender throws from a closed ws, upstream error,
          // bad response state) routes through here. Wrapping the data/end
          // handlers in try/catch is essential: these fire from EventEmitter,
          // NOT from the surrounding Promise chain, so an uncaught throw
          // otherwise becomes an unhandled error that can crash the process.
          const settle = (err?: Error) => {
            if (settled) return;
            settled = true;
            cleanup();
            if (err) reject(err); else resolve();
          };

          try {
            const respHeaders: Record<string, string[]> = {};
            for (const [key, value] of Object.entries(httpRes.headers)) {
              if (value === undefined) continue;
              respHeaders[key] = Array.isArray(value) ? value : [value];
            }
            w.writeHeader(httpRes.statusCode ?? 500, respHeaders);
          } catch (err) {
            settle(err instanceof Error ? err : new Error(String(err)));
            return;
          }

          httpRes.on("data", (chunk: Buffer) => {
            if (settled) return;
            try {
              w.write(new Uint8Array(chunk));
            } catch (err) {
              settle(err instanceof Error ? err : new Error(String(err)));
            }
          });
          httpRes.on("end", () => {
            if (settled) return;
            try {
              w.close();
            } catch (err) {
              settle(err instanceof Error ? err : new Error(String(err)));
              return;
            }
            settle();
          });
          httpRes.on("error", (err) => settle(err));
        },
        (err, cleanup) => {
          cleanup();
          reject(err);
        },
      );
    });
  };
}

function resolveHandler(handler: http.RequestListener | string, streaming: boolean): Handler {
  if (typeof handler === "string") {
    return new ForwardHandler(handler);
  }
  return new HTTPHandlerAdapter(handler, streaming);
}

export function createHOWHandler(
  handler: http.RequestListener | string,
  sender: Sendable,
  options?: HOWHandlerOptions,
): HOWHandler {
  const resolved = resolveHandler(handler, options?.streaming ?? false);
  const mode = options?.mode ?? 'binary';

  function sendEnv(env: Envelope): void {
    if (mode === 'text') {
      sender.sendText(marshalText(env));
    } else {
      sender.sendBytes(Buffer.from(marshal(env)));
    }
  }

  async function handleRequest(env: Envelope): Promise<void> {
    const requestID = env.request_id ?? "";
    const reqPayload = decodePayload<HTTPRequestPayload>(env);

    const streamingHandler = resolved as Partial<StreamingHandler>;
    if (streamingHandler.serveHTTPOverWSStream) {
      const rw = new ResponseWriter(requestID, sendEnv);
      try {
        await streamingHandler.serveHTTPOverWSStream(reqPayload, rw);
      } catch (err) {
        const message = err instanceof Error ? err.message : "unknown error";
        console.error(`streaming handler error: ${message}`);
        // sendEnv itself can throw if the transport died (that's often WHY
        // the handler just failed). Don't let a second throw escape the
        // Promise and become an unhandled rejection.
        try {
          if (!rw.started) {
            const errResp: HTTPResponsePayload = {
              status_code: 502,
              headers: { "content-type": ["text/plain"] },
              body: new TextEncoder().encode(message),
            };
            sendEnv(newHTTPResponse(requestID, errResp));
          } else {
            // Headers already on the wire — send an Error envelope, not End,
            // so the peer caller surfaces the failure through the body stream
            // instead of seeing a clean EOF on a truncated stream.
            sendEnv(newError(requestID, ErrorCode.Internal, message));
          }
        } catch (sendErr) {
          const m = sendErr instanceof Error ? sendErr.message : String(sendErr);
          console.error(`streaming handler: failed to signal error to peer: ${m}`);
        }
      }
      return;
    }

    try {
      const resp = await resolved.serveHTTPOverWS(reqPayload);
      sendEnv(newHTTPResponse(requestID, resp));
    } catch (err) {
      const message = err instanceof Error ? err.message : "unknown error";
      console.error(`handler error: ${message}`);
      try {
        const errResp: HTTPResponsePayload = {
          status_code: 502,
          headers: { "content-type": ["text/plain"] },
          body: new TextEncoder().encode(message),
        };
        sendEnv(newHTTPResponse(requestID, errResp));
      } catch (sendErr) {
        const m = sendErr instanceof Error ? sendErr.message : String(sendErr);
        console.error(`handler: failed to signal error to peer: ${m}`);
      }
    }
  }

  function processEnvelope(env: Envelope): void {
    switch (env.type) {
      case MessageType.HTTPRequest:
        // handleRequest is async and fire-and-forget; catch any rejection to
        // avoid unhandled-rejection process termination.
        handleRequest(env).catch((err) => {
          const m = err instanceof Error ? err.message : String(err);
          console.error(`handleRequest unhandled error: ${m}`);
        });
        break;

      default:
        console.log(`unexpected message type 0x${env.type.toString(16)}`);
    }
  }

  return {
    handleBinaryMessage(data: Buffer | Uint8Array): void {
      try {
        const env = unmarshal(data);
        processEnvelope(env);
      } catch (err) {
        const message = err instanceof Error ? err.message : "unknown error";
        console.error(`message handling error: ${message}`);
      }
    },

    handleTextMessage(data: string): void {
      try {
        const env = unmarshalText(data);
        processEnvelope(env);
      } catch (err) {
        const message = err instanceof Error ? err.message : "unknown error";
        console.error(`text message handling error: ${message}`);
      }
    },
  };
}
