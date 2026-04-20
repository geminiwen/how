import { describe, it, afterEach } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { WebSocketServer, WebSocket } from "ws";
import { createHOWCaller, createHOWHandler } from "./client";
import {
  unmarshal,
  marshal,
  MessageType,
  newHTTPRequest,
  newHTTPResponseStart,
  newHTTPResponseChunk,
  newHTTPResponseEnd,
  newError,
} from "../protocol/index";
import type { Envelope } from "../protocol/index";

/**
 * Starts an HTTP server with a WebSocket upgrade endpoint.
 * Returns the WS URL and a cleanup function.
 * The onConnection callback receives each WebSocket that connects.
 */
function startWSServer(onConnection: (ws: WebSocket) => void): Promise<{
  url: string;
  close: () => void;
}> {
  return new Promise((resolve) => {
    const server = http.createServer();
    const wss = new WebSocketServer({ server });
    wss.on("connection", onConnection);
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") throw new Error("bad address");
      resolve({
        url: `ws://127.0.0.1:${addr.port}`,
        close: () => {
          wss.close();
          server.close();
        },
      });
    });
  });
}

/** Connect a WebSocket client and wait for open. */
function connectWS(url: string): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    ws.on("open", () => resolve(ws));
    ws.on("error", reject);
  });
}

/** Wrap a WebSocket as a Sendable. */
function wrapSendable(ws: WebSocket) {
  return {
    sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
    sendText: (data: string) => ws.send(data),
  };
}

describe("Caller + Handler over WebSocket", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("simple request/response with RequestListener handler", async () => {
    // Handler side: WS server + createHOWHandler
    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(
        ((req: http.IncomingMessage, res: http.ServerResponse) => {
          res.writeHead(200, { "Content-Type": "text/plain" });
          res.end("hello from handler");
        }) as http.RequestListener,
        wrapSendable(serverWs),
      );
      serverWs.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    // Caller side: WS client + createHOWCaller
    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({
      method: "GET",
      url: "/test",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    const body = await readStreamBody(resp);
    assert.equal(body, "hello from handler");
  });

  it("request with body", async () => {
    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(
        ((req: http.IncomingMessage, res: http.ServerResponse) => {
          const chunks: Buffer[] = [];
          req.on("data", (chunk: Buffer) => chunks.push(chunk));
          req.on("end", () => {
            const body = Buffer.concat(chunks).toString();
            res.writeHead(200, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ echo: body }));
          });
        }) as http.RequestListener,
        wrapSendable(serverWs),
      );
      serverWs.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({
      method: "POST",
      url: "/echo",
      headers: { "Content-Type": ["application/json"] },
      body: new TextEncoder().encode('{"msg":"hi"}'),
    });

    assert.equal(resp.status_code, 200);
    const body = JSON.parse(await readStreamBody(resp));
    assert.equal(body.echo, '{"msg":"hi"}');
  });

  it("forward handler (string target)", async () => {
    // Start a local HTTP server to forward to
    const targetServer = http.createServer((req, res) => {
      res.writeHead(200, { "Content-Type": "text/plain" });
      res.end(`forwarded: ${req.method} ${req.url}`);
    });
    await new Promise<void>((resolve) => targetServer.listen(0, "127.0.0.1", resolve));
    cleanups.push(() => targetServer.close());
    const targetAddr = targetServer.address();
    if (!targetAddr || typeof targetAddr === "string") throw new Error("bad address");
    const target = `http://127.0.0.1:${targetAddr.port}`;

    // WS server with ForwardHandler
    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(target, wrapSendable(serverWs));
      serverWs.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({
      method: "GET",
      url: "/hello",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    // Forward handler uses streaming, so body is a ReadableStream
    const stream = resp.body;
    const reader = stream.getReader();
    const chunks: Uint8Array[] = [];
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
    }
    const totalLen = chunks.reduce((sum, c) => sum + c.length, 0);
    const bodyBytes = new Uint8Array(totalLen);
    let offset = 0;
    for (const c of chunks) {
      bodyBytes.set(c, offset);
      offset += c.length;
    }
    const body = new TextDecoder().decode(bodyBytes);
    assert.equal(body, "forwarded: GET /hello");
  });
});

/** Helper to collect a ReadableStream body into a string. */
async function readStreamBody(resp: { body?: Uint8Array }): Promise<string> {
  const stream = resp.body;
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
  }
  const totalLen = chunks.reduce((sum, c) => sum + c.length, 0);
  const merged = new Uint8Array(totalLen);
  let off = 0;
  for (const c of chunks) {
    merged.set(c, off);
    off += c.length;
  }
  return new TextDecoder().decode(merged);
}

describe("ForwardHandler", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  async function setup() {
    // Target HTTP server that exercises various HTTP features.
    const targetServer = http.createServer((req, res) => {
      if (req.url?.startsWith("/echo")) {
        const chunks: Buffer[] = [];
        req.on("data", (chunk: Buffer) => chunks.push(chunk));
        req.on("end", () => {
          const body = Buffer.concat(chunks);
          res.setHeader("Content-Type", "application/json");
          res.setHeader("X-Echo-Method", req.method ?? "");
          res.setHeader("X-Echo-Query", (req.url?.split("?")[1]) ?? "");
          res.setHeader("X-Custom-Header", req.headers["x-custom-header"] ?? "");
          res.writeHead(200);
          res.end(body);
        });
        return;
      }
      if (req.url === "/status/404") {
        res.writeHead(404);
        res.end("not found");
        return;
      }
      if (req.url === "/status/500") {
        res.writeHead(500);
        res.end("internal error");
        return;
      }
      res.writeHead(200);
      res.end(`ok: ${req.method} ${req.url}`);
    });
    await new Promise<void>((resolve) => targetServer.listen(0, "127.0.0.1", resolve));
    cleanups.push(() => targetServer.close());
    const targetAddr = targetServer.address();
    if (!targetAddr || typeof targetAddr === "string") throw new Error("bad address");
    const target = `http://127.0.0.1:${targetAddr.port}`;

    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(target, wrapSendable(serverWs));
      serverWs.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    return caller;
  }

  it("POST with body and headers", async () => {
    const caller = await setup();
    const resp = await caller.request({
      method: "POST",
      url: "/echo?foo=bar",
      headers: {
        "Content-Type": ["application/json"],
        "X-Custom-Header": ["test-value"],
      },
      body: new TextEncoder().encode('{"hello":"world"}'),
    });
    assert.equal(resp.status_code, 200);
    const body = await readStreamBody(resp);
    assert.equal(body, '{"hello":"world"}');
    assert.equal(resp.headers["x-echo-method"]?.[0], "POST");
    assert.equal(resp.headers["x-echo-query"]?.[0], "foo=bar");
    assert.equal(resp.headers["x-custom-header"]?.[0], "test-value");
  });

  it("PUT request", async () => {
    const caller = await setup();
    const resp = await caller.request({
      method: "PUT",
      url: "/echo",
      headers: { "Content-Type": ["text/plain"] },
      body: new TextEncoder().encode("updated"),
    });
    assert.equal(resp.status_code, 200);
    const body = await readStreamBody(resp);
    assert.equal(body, "updated");
    assert.equal(resp.headers["x-echo-method"]?.[0], "PUT");
  });

  it("DELETE request", async () => {
    const caller = await setup();
    const resp = await caller.request({
      method: "DELETE",
      url: "/echo",
      headers: {},
    });
    assert.equal(resp.status_code, 200);
    assert.equal(resp.headers["x-echo-method"]?.[0], "DELETE");
  });

  it("404 status code", async () => {
    const caller = await setup();
    const resp = await caller.request({
      method: "GET",
      url: "/status/404",
      headers: {},
    });
    assert.equal(resp.status_code, 404);
    const body = await readStreamBody(resp);
    assert.equal(body, "not found");
  });

  it("500 status code", async () => {
    const caller = await setup();
    const resp = await caller.request({
      method: "GET",
      url: "/status/500",
      headers: {},
    });
    assert.equal(resp.status_code, 500);
    const body = await readStreamBody(resp);
    assert.equal(body, "internal error");
  });
});

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

describe("Caller read timeout", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("times out when no response is received", async () => {
    // Server receives request but never responds.
    const { url, close } = await startWSServer((_serverWs) => {
      // intentionally do nothing
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { readTimeout: 100 });
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const start = Date.now();
    await assert.rejects(
      caller.request({ method: "GET", url: "/hello", headers: {} }),
      { message: "read timeout" },
    );
    const elapsed = Date.now() - start;
    assert.ok(elapsed < 1000, `timeout took too long: ${elapsed}ms`);
  });

  it("streaming chunks reset the timeout", async () => {
    // Server sends Start + 3 chunks (each within timeout), then stops.
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", async (data: Buffer) => {
        const env = unmarshal(data) as Envelope;
        if (env.type !== MessageType.HTTPRequest) return;
        const requestID = env.request_id!;

        // Send ResponseStart
        serverWs.send(
          Buffer.from(marshal(newHTTPResponseStart(requestID, 200, { "Content-Type": ["text/plain"] }))),
        );

        // Send 3 chunks, each within the timeout window
        for (let i = 0; i < 3; i++) {
          await sleep(50);
          serverWs.send(
            Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode(`chunk${i}`)))),
          );
        }
        // Then stop — no End message. Caller should time out.
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { readTimeout: 100 });
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const start = Date.now();
    // request() resolves on HTTPResponseStart with a streaming body.
    // The timeout should fire after chunks stop arriving.
    const resp = await caller.request({ method: "GET", url: "/stream", headers: {} });
    assert.equal(resp.status_code, 200);

    // The body stream should error with timeout
    const stream = resp.body;
    const reader = stream.getReader();
    const chunks: string[] = [];
    await assert.rejects(async () => {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(new TextDecoder().decode(value));
      }
    }, { message: "read timeout" });

    const elapsed = Date.now() - start;
    // Should have received the 3 chunks before timing out
    assert.equal(chunks.length, 3);
    // 3 chunks at 50ms + 100ms timeout ≈ 250ms. Should be > 150ms (proving reset works).
    assert.ok(elapsed >= 150, `timed out too early (chunks didn't reset timer): ${elapsed}ms`);
    assert.ok(elapsed < 2000, `timeout took too long: ${elapsed}ms`);
  });
});

describe("HTTPHandlerAdapter streaming", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("SSE-style RequestListener streams each write as a separate chunk", async () => {
    // Handler writes header, then 3 chunks with a gap, then ends.
    const handler: http.RequestListener = async (_req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("event: one\ndata: 1\n\n");
      await sleep(30);
      res.write("event: two\ndata: 2\n\n");
      await sleep(30);
      res.write("event: three\ndata: 3\n\n");
      res.end();
    };

    const { url, close } = await startWSServer((serverWs) => {
      const howHandler = createHOWHandler(handler, wrapSendable(serverWs), { streaming: true });
      serverWs.on("message", (data: Buffer) => howHandler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({ method: "GET", url: "/sse", headers: {} });
    assert.equal(resp.status_code, 200);
    assert.equal(resp.headers["content-type"]?.[0], "text/event-stream");

    const stream = resp.body;
    const reader = stream.getReader();
    const arrivals: { t: number; text: string }[] = [];
    const start = Date.now();
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      arrivals.push({ t: Date.now() - start, text: new TextDecoder().decode(value) });
    }

    // Concatenated body is correct
    assert.equal(
      arrivals.map(a => a.text).join(""),
      "event: one\ndata: 1\n\nevent: two\ndata: 2\n\nevent: three\ndata: 3\n\n",
    );

    // At least the first chunk must arrive well before the last — proves streaming
    // (if buffered, the first "arrival" time would be after all writes finish, ~60ms).
    assert.ok(arrivals.length >= 2, `expected multiple stream arrivals, got ${arrivals.length}`);
    const firstArrival = arrivals[0].t;
    const lastArrival = arrivals[arrivals.length - 1].t;
    assert.ok(
      lastArrival - firstArrival >= 20,
      `arrivals collapsed (firstArrival=${firstArrival}ms, lastArrival=${lastArrival}ms) — likely buffered`,
    );
  });
});

describe("Caller stream lifecycle edges", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("mid-stream Error envelope errors the body reader (not a hang)", async () => {
    // Server sends HTTPResponseStart + one chunk, then an Error envelope with
    // the same request_id. The reader must see a rejecting read, not block forever.
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", (data: Buffer) => {
        const env = unmarshal(data) as Envelope;
        if (env.type !== MessageType.HTTPRequest) return;
        const requestID = env.request_id!;

        serverWs.send(
          Buffer.from(marshal(newHTTPResponseStart(requestID, 200, { "content-type": ["text/plain"] }))),
        );
        serverWs.send(
          Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("part")))),
        );
        // Now a protocol error on the same request_id.
        setTimeout(() => {
          serverWs.send(Buffer.from(marshal(newError(requestID, 1006, "upstream exploded"))));
        }, 20);
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { readTimeout: 30_000 });
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({ method: "GET", url: "/x", headers: {} });
    assert.equal(resp.status_code, 200);

    const reader = resp.body.getReader();
    const first = await reader.read();
    assert.equal(first.done, false);
    assert.equal(new TextDecoder().decode(first.value), "part");

    const start = Date.now();
    await assert.rejects(reader.read(), { message: "upstream exploded" });
    const elapsed = Date.now() - start;
    assert.ok(elapsed < 1000, `reader.read took too long: ${elapsed}ms`);
  });

  it("mid-stream Error still delivers already-received chunks to a slow reader", async () => {
    // Regression: ReadableStreamDefaultController.error() erases its own
    // internal queue. Our caller must use an external buffer so that a peer
    // sending chunk A + chunk B + Error still lets a slow reader observe A
    // and B before the terminal read rejects.
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", (data: Buffer) => {
        const env = unmarshal(data) as Envelope;
        if (env.type !== MessageType.HTTPRequest) return;
        const requestID = env.request_id!;

        serverWs.send(Buffer.from(marshal(newHTTPResponseStart(requestID, 200, {}))));
        serverWs.send(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("A")))));
        serverWs.send(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("B")))));
        serverWs.send(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("C")))));
        serverWs.send(Buffer.from(marshal(newError(requestID, 1006, "boom"))));
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({ method: "GET", url: "/drain", headers: {} });

    // Let the server send everything BEFORE the reader starts reading, so all
    // chunks plus the Error are already queued in our buffer.
    await sleep(80);

    const reader = resp.body.getReader();
    const received: string[] = [];
    let readErr: unknown;
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        received.push(new TextDecoder().decode(value));
      }
    } catch (e) {
      readErr = e;
    }

    assert.deepEqual(received, ["A", "B", "C"], "all pre-error chunks must reach the reader");
    assert.ok(readErr instanceof Error && readErr.message === "boom", `expected 'boom' error, got ${readErr}`);
  });

  it("consumer cancel() drops pending entry and drops later frames cleanly", async () => {
    let chunkSendErrors = 0;
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", async (data: Buffer) => {
        const env = unmarshal(data) as Envelope;
        if (env.type !== MessageType.HTTPRequest) return;
        const requestID = env.request_id!;

        const trySend = (buf: Buffer) => {
          try { serverWs.send(buf); } catch { chunkSendErrors++; }
        };

        trySend(Buffer.from(marshal(newHTTPResponseStart(requestID, 200, {}))));
        trySend(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("first")))));
        // Give the client a tick to cancel.
        await sleep(40);
        // These should be silently dropped by the cancelled caller.
        trySend(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("second")))));
        trySend(Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("third")))));
        trySend(Buffer.from(marshal(newHTTPResponseEnd(requestID))));
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    // Capture any console.error raised from the message handler — cancellation
    // must not produce spurious errors.
    const originalError = console.error;
    const errors: string[] = [];
    console.error = (msg: unknown) => errors.push(String(msg));
    cleanups.push(() => { console.error = originalError; });

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({ method: "GET", url: "/cancel", headers: {} });
    const reader = resp.body.getReader();
    const first = await reader.read();
    assert.equal(new TextDecoder().decode(first.value), "first");

    // Consumer decides to stop early.
    await reader.cancel("done");

    // Give the server time to fire the post-cancel frames.
    await sleep(100);

    assert.equal(errors.length, 0, `unexpected console.error output: ${errors.join("; ")}`);
    assert.equal(chunkSendErrors, 0, "server's sends should have succeeded; they're dropped on the caller side");
  });
});

describe("Handler streaming resilience", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("sender throwing mid-stream does not crash the process", async () => {
    // Simulate a ws that is healthy when the handler starts but closes mid-stream:
    // the Sendable.sendBytes starts throwing. The adapter must catch the throw
    // from its httpRes "data" / "end" listeners (which run outside the promise
    // chain) and settle the outer streamImpl promise with an error.
    let byteCalls = 0;
    let sawError = false;

    const throwingSender = {
      sendBytes: (_data: Buffer | Uint8Array) => {
        byteCalls++;
        if (byteCalls > 1) {
          throw new Error("simulated ws closed");
        }
      },
      sendText: () => {},
    };

    const handler: http.RequestListener = async (_req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("first");
      await sleep(10);
      res.write("second"); // sender throws here
      await sleep(10);
      res.end();
    };

    // Capture unhandled rejections just in case something leaks out.
    const unhandled: unknown[] = [];
    const onUnhandled = (err: unknown) => unhandled.push(err);
    process.on("unhandledRejection", onUnhandled);
    process.on("uncaughtException", onUnhandled);
    cleanups.push(() => {
      process.off("unhandledRejection", onUnhandled);
      process.off("uncaughtException", onUnhandled);
    });

    // Capture internal console.error so we can confirm the adapter reported the error
    // via the normal logging path rather than a process-level crash.
    const originalError = console.error;
    const errors: string[] = [];
    console.error = (msg: unknown) => {
      errors.push(String(msg));
      if (String(msg).includes("simulated ws closed")) sawError = true;
    };
    cleanups.push(() => { console.error = originalError; });

    const howHandler = createHOWHandler(handler, throwingSender, { streaming: true });
    const reqEnv = newHTTPRequest("crash-test", {
      method: "GET",
      url: "/sse",
      headers: {},
    });
    howHandler.handleBinaryMessage(Buffer.from(marshal(reqEnv)));

    await sleep(120);
    assert.equal(unhandled.length, 0, `no unhandled errors expected, got: ${JSON.stringify(unhandled)}`);
    assert.ok(sawError, `adapter should have logged the sender failure; errors=${errors.join("; ")}`);
  });
});

describe("Handler streaming error after headers", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("handler throw after rw.started is surfaced as a read error, not EOF", async () => {
    // SSE-style handler writes header + first chunk, then throws.
    const handler: http.RequestListener = (_req, res) => {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("data: first\n\n");
      // Abort the response unexpectedly. Simulates an express handler that
      // throws mid-loop.
      setTimeout(() => {
        res.destroy(new Error("deliberate mid-stream failure"));
      }, 20);
    };

    const { url, close } = await startWSServer((serverWs) => {
      const howHandler = createHOWHandler(handler, wrapSendable(serverWs), { streaming: true });
      serverWs.on("message", (data: Buffer) => howHandler.handleBinaryMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs));
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));

    const resp = await caller.request({ method: "GET", url: "/sse", headers: {} });
    assert.equal(resp.status_code, 200);

    const reader = resp.body.getReader();
    const first = await reader.read();
    assert.equal(first.done, false);
    assert.ok(new TextDecoder().decode(first.value).includes("first"));

    // Next read should reject (not return { done: true }). The old behavior
    // was to emit HTTPResponseEnd on handler error → reader saw a clean EOF.
    await assert.rejects(reader.read(), (err: unknown) => err instanceof Error);
  });
});

describe("Caller transport disconnect", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("non-streaming pending request rejects immediately when transport closes", async () => {
    // Server receives request then immediately terminates the ws connection without responding.
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", () => serverWs.terminate());
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { readTimeout: 30_000 });
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));
    clientWs.on("close", () => caller.close(new Error("ws closed")));
    clientWs.on("error", (err) => caller.close(err));

    const start = Date.now();
    await assert.rejects(
      caller.request({ method: "GET", url: "/hello", headers: {} }),
      { message: "ws closed" },
    );
    const elapsed = Date.now() - start;
    assert.ok(elapsed < 1000, `rejected too slowly: ${elapsed}ms`);
  });

  it("streaming body errors when transport closes mid-stream", async () => {
    // Server sends HTTPResponseStart + one chunk, then drops the connection.
    const { url, close } = await startWSServer((serverWs) => {
      serverWs.on("message", (data: Buffer) => {
        const env = unmarshal(data) as Envelope;
        if (env.type !== MessageType.HTTPRequest) return;
        const requestID = env.request_id!;
        serverWs.send(
          Buffer.from(marshal(newHTTPResponseStart(requestID, 200, { "Content-Type": ["text/plain"] }))),
        );
        serverWs.send(
          Buffer.from(marshal(newHTTPResponseChunk(requestID, new TextEncoder().encode("first")))),
        );
        // Give the client a tick to process the chunk before yanking the socket.
        setTimeout(() => serverWs.terminate(), 20);
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { readTimeout: 30_000 });
    clientWs.on("message", (data: Buffer) => caller.handleBinaryMessage(data));
    clientWs.on("close", () => caller.close(new Error("ws closed")));
    clientWs.on("error", (err) => caller.close(err));

    const resp = await caller.request({ method: "GET", url: "/stream", headers: {} });
    assert.equal(resp.status_code, 200);

    const start = Date.now();
    const reader = (resp.body).getReader();
    // First chunk arrives cleanly.
    const first = await reader.read();
    assert.equal(first.done, false);
    assert.equal(new TextDecoder().decode(first.value), "first");

    // Next read should reject once the ws close propagates.
    await assert.rejects(reader.read(), { message: "ws closed" });
    const elapsed = Date.now() - start;
    assert.ok(elapsed < 1000, `stream errored too slowly: ${elapsed}ms`);
  });

  it("close is idempotent and blocks later requests", async () => {
    // No server interaction — just exercise caller.close() directly.
    const sent: Uint8Array[] = [];
    const caller = createHOWCaller({
      sendBytes: (data) => { sent.push(data instanceof Uint8Array ? data : new Uint8Array(data)); },
      sendText: () => {},
    });

    // Close with no pending requests should be a no-op.
    caller.close();
    caller.close(new Error("second close"));

    await assert.rejects(
      caller.request({ method: "GET", url: "/x", headers: {} }),
      { message: "transport closed" },
    );
    assert.equal(sent.length, 0, "request after close must not have sent any bytes");
  });
});

describe("Text mode: Caller + Handler over WebSocket", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanups) fn();
    cleanups.length = 0;
  });

  it("simple request/response in text mode", async () => {
    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(
        ((req: http.IncomingMessage, res: http.ServerResponse) => {
          res.writeHead(200, { "Content-Type": "text/plain" });
          res.end("hello from text handler");
        }) as http.RequestListener,
        wrapSendable(serverWs),
        { mode: "text" },
      );
      serverWs.on("message", (data: Buffer, isBinary: boolean) => {
        if (isBinary) {
          handler.handleBinaryMessage(data);
        } else {
          handler.handleTextMessage(data.toString());
        }
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { mode: "text" });
    clientWs.on("message", (data: Buffer, isBinary: boolean) => {
      if (isBinary) {
        caller.handleBinaryMessage(data);
      } else {
        caller.handleTextMessage(data.toString());
      }
    });

    const resp = await caller.request({
      method: "GET",
      url: "/test",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    const body = await readStreamBody(resp);
    assert.equal(body, "hello from text handler");
  });

  it("text mode with forward handler (streaming)", async () => {
    const targetServer = http.createServer((req, res) => {
      res.writeHead(200, { "Content-Type": "text/plain" });
      res.end(`forwarded: ${req.method} ${req.url}`);
    });
    await new Promise<void>((resolve) => targetServer.listen(0, "127.0.0.1", resolve));
    cleanups.push(() => targetServer.close());
    const targetAddr = targetServer.address();
    if (!targetAddr || typeof targetAddr === "string") throw new Error("bad address");
    const target = `http://127.0.0.1:${targetAddr.port}`;

    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWHandler(target, wrapSendable(serverWs), { mode: "text" });
      serverWs.on("message", (data: Buffer, isBinary: boolean) => {
        if (isBinary) {
          handler.handleBinaryMessage(data);
        } else {
          handler.handleTextMessage(data.toString());
        }
      });
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(wrapSendable(clientWs), { mode: "text" });
    clientWs.on("message", (data: Buffer, isBinary: boolean) => {
      if (isBinary) {
        caller.handleBinaryMessage(data);
      } else {
        caller.handleTextMessage(data.toString());
      }
    });

    const resp = await caller.request({
      method: "GET",
      url: "/hello",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    // Forward handler uses streaming, so body is a ReadableStream
    const body = await readStreamBody(resp);
    assert.equal(body, "forwarded: GET /hello");
  });
});
