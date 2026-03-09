/**
 * Standalone Node script for cross-language integration testing.
 * Connects to a WebSocket server and acts as a HOW Handler.
 *
 * Usage: npx tsx src/client/cross_test_handler.ts <ws-url>
 */
import http from "node:http";
import { WebSocket } from "ws";
import { createHOWHandler } from "./client";

process.on("uncaughtException", (err) => {
  console.error("uncaught exception:", err);
  process.exit(1);
});

process.on("unhandledRejection", (reason) => {
  console.error("unhandled rejection:", reason);
  process.exit(1);
});

const wsURL = process.argv[2];
if (!wsURL) {
  console.error("usage: cross_test_handler.ts <ws-url>");
  process.exit(1);
}

const ws = new WebSocket(wsURL);

ws.on("open", () => {
  const sender = {
    sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
    sendText: (data: string) => ws.send(data),
  };

  const handler = createHOWHandler(
    ((req: http.IncomingMessage, res: http.ServerResponse) => {
      if (req.url === "/hello") {
        res.writeHead(200, { "Content-Type": "text/plain" });
        res.end("hello from node");
        return;
      }

      if (req.url === "/echo") {
        const chunks: Buffer[] = [];
        req.on("data", (chunk: Buffer) => chunks.push(chunk));
        req.on("end", () => {
          const body = Buffer.concat(chunks);
          res.writeHead(200, { "Content-Type": "text/plain" });
          res.end(body);
        });
        return;
      }

      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
    }) as http.RequestListener,
    sender,
  );

  ws.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
  // Signal readiness to the Go test by printing to stdout
  console.log("READY");
});

ws.on("close", () => {
  process.exit(0);
});

ws.on("error", (err) => {
  console.error("ws error:", err);
  process.exit(1);
});
