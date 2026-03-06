import { describe, it, afterEach } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { WebSocketServer, WebSocket } from "ws";
import { createHOWCaller, createHOWHandler } from "./client";
import {
  unmarshal,
  marshal,
  MessageType,
  newHTTPResponseStart,
  newHTTPResponseChunk,
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
        serverWs,
      );
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    // Caller side: WS client + createHOWCaller
    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(clientWs);
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

    const resp = await caller.request({
      method: "GET",
      url: "/test",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    const body = new TextDecoder().decode(new Uint8Array(resp.body as ArrayLike<number>));
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
        serverWs,
      );
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(clientWs);
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

    const resp = await caller.request({
      method: "POST",
      url: "/echo",
      headers: { "Content-Type": ["application/json"] },
      body: new TextEncoder().encode('{"msg":"hi"}'),
    });

    assert.equal(resp.status_code, 200);
    const body = JSON.parse(new TextDecoder().decode(new Uint8Array(resp.body as ArrayLike<number>)));
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
      const handler = createHOWHandler(target, serverWs);
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(clientWs);
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

    const resp = await caller.request({
      method: "GET",
      url: "/hello",
      headers: {},
    });

    assert.equal(resp.status_code, 200);
    // Forward handler uses streaming, so body is a ReadableStream
    const stream = resp.body as unknown as ReadableStream<Uint8Array>;
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
  const stream = resp.body as unknown as ReadableStream<Uint8Array>;
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
      const handler = createHOWHandler(target, serverWs);
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWCaller(clientWs);
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

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

    const caller = createHOWCaller(clientWs, { readTimeout: 100 });
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

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

    const caller = createHOWCaller(clientWs, { readTimeout: 100 });
    clientWs.on("message", (data: Buffer) => caller.handleMessage(data));

    const start = Date.now();
    // request() resolves on HTTPResponseStart with a streaming body.
    // The timeout should fire after chunks stop arriving.
    const resp = await caller.request({ method: "GET", url: "/stream", headers: {} });
    assert.equal(resp.status_code, 200);

    // The body stream should error with timeout
    const stream = resp.body as unknown as ReadableStream<Uint8Array>;
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
