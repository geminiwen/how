/**
 * Standalone Node script for cross-language integration testing.
 * Connects to a WebSocket server and acts as a HOW Handler.
 *
 * Usage: npx tsx src/client/cross_test_handler.ts <ws-url> [--text] [--streaming]
 *   --text        use text (JSON) envelope mode; default is binary (MessagePack)
 *   --streaming   wrap the RequestListener in the streaming HTTPHandlerAdapter,
 *                 so res.write emits one HTTPResponseChunk per write
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
  console.error("usage: cross_test_handler.ts <ws-url> [--text] [--streaming]");
  process.exit(1);
}
const mode: "binary" | "text" = process.argv.includes("--text") ? "text" : "binary";
const streaming = process.argv.includes("--streaming");

const ws = new WebSocket(wsURL);

ws.on("open", () => {
  // ws.send auto-picks frame type: Buffer/Uint8Array → binary, string → text.
  const sender = {
    sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
    sendText: (data: string) => ws.send(data),
  };

  const requestListener: http.RequestListener = (req, res) => {
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

    // Emits 5 chunks over ~50ms. When the handler is wired with
    // `streaming: true`, each res.write() becomes one HTTPResponseChunk on
    // the wire — so the Go caller on the other side receives them as an
    // incremental stream. When streaming is disabled, the whole body is
    // buffered into a single HTTPResponse. Either way the concatenated body
    // reads back as the same bytes, which is the property the test asserts.
    if (req.url === "/stream") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      let i = 0;
      const timer = setInterval(() => {
        res.write(`chunk-${i}\n`);
        i++;
        if (i === 5) {
          clearInterval(timer);
          res.end();
        }
      }, 10);
      return;
    }

    res.writeHead(404, { "Content-Type": "text/plain" });
    res.end("not found");
  };

  const handler = createHOWHandler(requestListener, sender, { mode, streaming });

  if (mode === "text") {
    // ws delivers text frames as Buffer; decode to string for handleTextMessage.
    ws.on("message", (data: Buffer) => handler.handleTextMessage(data.toString("utf8")));
  } else {
    ws.on("message", (data: Buffer) => handler.handleBinaryMessage(data));
  }

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
