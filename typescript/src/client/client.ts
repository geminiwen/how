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
  request(req: HTTPRequestPayload): Promise<HTTPResponsePayload>;
  handleBinaryMessage(data: Buffer | Uint8Array): void;
  handleTextMessage(data: string): void;
}

/** Default read timeout in milliseconds. */
export const DEFAULT_READ_TIMEOUT = 30_000;

interface PendingRequest {
  resolve: (resp: HTTPResponsePayload) => void;
  reject: (err: Error) => void;
  timer?: ReturnType<typeof setTimeout>;
  // Streaming state
  streamController?: ReadableStreamDefaultController<Uint8Array>;
  streamHeaders?: Record<string, string[]>;
  streamStatusCode?: number;
}

export function createHOWCaller(sender: Sendable, options?: HOWCallerOptions): HOWCaller {
  const pending = new Map<string, PendingRequest>();
  const readTimeout = options?.readTimeout ?? DEFAULT_READ_TIMEOUT;
  const mode = options?.mode ?? 'binary';

  function startTimer(requestID: string, entry: PendingRequest): void {
    if (readTimeout <= 0) return;
    clearTimer(entry);
    entry.timer = setTimeout(() => {
      pending.delete(requestID);
      if (entry.streamController) {
        try { entry.streamController.error(new Error("read timeout")); } catch {}
      }
      entry.reject(new Error("read timeout"));
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
        clearTimer(entry);
        pending.delete(requestID);
        const payload = decodePayload<HTTPResponsePayload>(env);
        entry.resolve(payload);
        break;
      }

      case MessageType.HTTPResponseStart: {
        const start = decodePayload<HTTPResponseStartPayload>(env);
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            entry.streamController = controller;
          },
        });
        entry.streamStatusCode = start.status_code;
        entry.streamHeaders = start.headers;
        entry.resolve({
          status_code: start.status_code,
          headers: start.headers,
          body: stream as unknown as Uint8Array,
        });
        break;
      }

      case MessageType.HTTPResponseChunk: {
        const chunk = decodePayload<HTTPResponseChunkPayload>(env);
        if (entry.streamController) {
          entry.streamController.enqueue(new Uint8Array(chunk.data as ArrayLike<number>));
        }
        break;
      }

      case MessageType.HTTPResponseEnd: {
        clearTimer(entry);
        if (entry.streamController) {
          entry.streamController.close();
        }
        pending.delete(requestID);
        break;
      }

      case MessageType.Error: {
        clearTimer(entry);
        pending.delete(requestID);
        const errPayload = decodePayload<ErrorPayload>(env);
        entry.reject(new Error(errPayload.message));
        break;
      }
    }
  }

  return {
    request(req: HTTPRequestPayload): Promise<HTTPResponsePayload> {
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
  };
}

// ─── Handler ───

export interface HOWHandlerOptions {
  /** Serialization mode. Default: 'binary'. */
  mode?: 'binary' | 'text';
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

  constructor(handler: http.RequestListener) {
    this.handler = handler;
  }

  serveHTTPOverWS(
    req: HTTPRequestPayload,
  ): Promise<HTTPResponsePayload> {
    return new Promise((resolve, reject) => {
      const server = http.createServer((httpReq, httpRes) => {
        this.handler(httpReq, httpRes);
      });

      server.listen(0, "127.0.0.1", () => {
        const addr = server.address();
        if (!addr || typeof addr === "string") {
          server.close();
          reject(new Error("failed to get server address"));
          return;
        }

        const url = `http://127.0.0.1:${addr.port}${req.url}`;
        const headers: Record<string, string> = {};
        for (const [key, values] of Object.entries(req.headers)) {
          headers[key] = values.join(", ");
        }

        const httpReq = http.request(
          url,
          { method: req.method, headers },
          (httpRes) => {
            const chunks: Buffer[] = [];
            httpRes.on("data", (chunk: Buffer) => chunks.push(chunk));
            httpRes.on("end", () => {
              const body = Buffer.concat(chunks);
              const respHeaders: Record<string, string[]> = {};
              for (const [key, value] of Object.entries(httpRes.headers)) {
                if (value === undefined) continue;
                respHeaders[key] = Array.isArray(value) ? value : [value];
              }

              server.close();
              resolve({
                status_code: httpRes.statusCode ?? 500,
                headers: respHeaders,
                body: body.length > 0 ? body : undefined,
              });
            });
            httpRes.on("error", (err) => {
              server.close();
              reject(err);
            });
          },
        );

        httpReq.on("error", (err) => {
          server.close();
          reject(err);
        });

        if (req.body && req.body.length > 0) {
          httpReq.write(Buffer.from(req.body));
        }
        httpReq.end();
      });
    });
  }
}

function resolveHandler(handler: http.RequestListener | string): Handler {
  if (typeof handler === "string") {
    return new ForwardHandler(handler);
  }
  return new HTTPHandlerAdapter(handler);
}

export function createHOWHandler(
  handler: http.RequestListener | string,
  sender: Sendable,
  options?: HOWHandlerOptions,
): HOWHandler {
  const resolved = resolveHandler(handler);
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
        if (!rw.started) {
          const errResp: HTTPResponsePayload = {
            status_code: 502,
            headers: { "content-type": ["text/plain"] },
            body: new TextEncoder().encode(message),
          };
          sendEnv(newHTTPResponse(requestID, errResp));
        } else {
          rw.close();
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
      const errResp: HTTPResponsePayload = {
        status_code: 502,
        headers: { "content-type": ["text/plain"] },
        body: new TextEncoder().encode(message),
      };
      sendEnv(newHTTPResponse(requestID, errResp));
    }
  }

  function processEnvelope(env: Envelope): void {
    switch (env.type) {
      case MessageType.HTTPRequest:
        handleRequest(env);
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
