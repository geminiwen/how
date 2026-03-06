import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  marshal,
  unmarshal,
  decodePayload,
  newHTTPRequest,
  newHTTPResponse,
  newError,
  MessageType,
  ErrorCode,
} from "./message";
import type {
  HTTPRequestPayload,
  HTTPResponsePayload,
  ErrorPayload,
} from "./message";

describe("protocol round-trip", () => {
  it("HTTPRequest", () => {
    const req: HTTPRequestPayload = {
      method: "POST",
      url: "/api/users?page=1",
      headers: { "Content-Type": ["application/json"] },
      body: new TextEncoder().encode('{"name":"test"}'),
    };
    const env = newHTTPRequest("req-123", req);
    const decoded = unmarshal(marshal(env));
    assert.equal(decoded.type, MessageType.HTTPRequest);
    assert.equal(decoded.request_id, "req-123");
    const payload = decodePayload<HTTPRequestPayload>(decoded);
    assert.equal(payload.method, "POST");
    assert.equal(payload.url, "/api/users?page=1");
    assert.deepEqual(
      new TextDecoder().decode(new Uint8Array(payload.body as ArrayLike<number>)),
      '{"name":"test"}',
    );
  });

  it("HTTPResponse", () => {
    const resp: HTTPResponsePayload = {
      status_code: 200,
      headers: { "Content-Type": ["text/plain"] },
      body: new TextEncoder().encode("hello"),
    };
    const env = newHTTPResponse("req-456", resp);
    const decoded = unmarshal(marshal(env));
    assert.equal(decoded.type, MessageType.HTTPResponse);
    const payload = decodePayload<HTTPResponsePayload>(decoded);
    assert.equal(payload.status_code, 200);
    assert.deepEqual(
      new TextDecoder().decode(new Uint8Array(payload.body as ArrayLike<number>)),
      "hello",
    );
  });

  it("Error", () => {
    const env = newError("req-789", ErrorCode.ClientNotFound, "not found");
    const decoded = unmarshal(marshal(env));
    assert.equal(decoded.type, MessageType.Error);
    const payload = decodePayload<ErrorPayload>(decoded);
    assert.equal(payload.code, ErrorCode.ClientNotFound);
    assert.equal(payload.message, "not found");
  });
});
