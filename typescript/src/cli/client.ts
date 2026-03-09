import { parseArgs } from "node:util";
import WebSocket from "ws";
import { createHOWHandler } from "../client/index";

const { values } = parseArgs({
  options: {
    server: { type: "string", short: "s" },
    target: { type: "string", short: "t" },
  },
});

if (!values.server || !values.target) {
  console.error("Usage: client --server <ws-url> --target <http-url>");
  process.exit(1);
}

const ws = new WebSocket(values.server, ["how.v1"]);
const sender = {
  sendBytes: (data: Buffer | Uint8Array) => ws.send(data),
  sendText: (data: string) => ws.send(data),
};
const handler = createHOWHandler(values.target, sender);

ws.on("open", () => console.log("connected"));
ws.on("message", (data: Buffer) => handler.handleBinaryMessage(data));

ws.on("error", (err) => {
  console.error("ws error:", err.message);
  process.exit(1);
});

ws.on("close", () => {
  console.log("disconnected");
  process.exit(0);
});

process.on("SIGINT", () => ws.close());
process.on("SIGTERM", () => ws.close());
