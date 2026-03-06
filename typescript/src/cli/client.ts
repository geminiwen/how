import { parseArgs } from "node:util";
import WebSocket from "ws";
import { createHOWSHandler } from "../client/index";

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

const ws = new WebSocket(values.server, ["hows.v1"]);
const handler = createHOWSHandler(values.target, ws);

ws.on("open", () => console.log("connected"));
ws.on("message", (data: Buffer) => handler.handleMessage(data));

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
