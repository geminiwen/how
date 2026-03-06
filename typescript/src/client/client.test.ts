import { describe, it, afterEach } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { WebSocketServer, WebSocket } from "ws";
import { createHOWSCaller, createHOWSHandler } from "./client";

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
    // Handler side: WS server + createHOWSHandler
    const { url, close } = await startWSServer((serverWs) => {
      const handler = createHOWSHandler(
        ((req: http.IncomingMessage, res: http.ServerResponse) => {
          res.writeHead(200, { "Content-Type": "text/plain" });
          res.end("hello from handler");
        }) as http.RequestListener,
        serverWs,
      );
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    // Caller side: WS client + createHOWSCaller
    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWSCaller(clientWs);
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
      const handler = createHOWSHandler(
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

    const caller = createHOWSCaller(clientWs);
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
      const handler = createHOWSHandler(target, serverWs);
      serverWs.on("message", (data: Buffer) => handler.handleMessage(data));
    });
    cleanups.push(close);

    const clientWs = await connectWS(url);
    cleanups.push(() => clientWs.close());

    const caller = createHOWSCaller(clientWs);
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
