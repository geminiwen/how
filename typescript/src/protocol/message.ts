import { encode, decode } from "@msgpack/msgpack";

/** Protocol discriminator byte. Every HOW binary frame starts with this byte. */
export const ProtocolByte = 0x69;

/** Message type identifiers matching the HOW spec. */
export const MessageType = {
  HTTPRequest: 0x10,
  HTTPResponse: 0x11,
  HTTPResponseStart: 0x12,
  HTTPResponseChunk: 0x13,
  HTTPResponseEnd: 0x14,
  Error: 0xff,
} as const;

export type MessageTypeValue = (typeof MessageType)[keyof typeof MessageType];

/** Error code constants. */
export const ErrorCode = {
  Unknown: 1000,
  ClientNotFound: 1001,
  RequestTimeout: 1002,
  InvalidMessage: 1003,
  RegistrationFail: 1004,
  ClientIDConflict: 1005,
  Internal: 1006,
  RequestCancelled: 1007,
} as const;

/** Top-level wire format for all HOW messages. */
export interface Envelope {
  type: MessageTypeValue;
  request_id?: string;
  payload?: unknown;
}

export interface HTTPRequestPayload {
  method: string;
  url: string;
  headers: Record<string, string[]>;
  body?: Uint8Array;
}

export interface HTTPResponsePayload {
  status_code: number;
  headers: Record<string, string[]>;
  body?: Uint8Array;
}

export interface HTTPResponseStartPayload {
  status_code: number;
  headers: Record<string, string[]>;
}

export interface HTTPResponseChunkPayload {
  data: Uint8Array;
}

export interface ErrorPayload {
  code: number;
  message: string;
}

/** Serialize an Envelope to MessagePack bytes, prefixed with ProtocolByte. */
export function marshal(env: Envelope): Uint8Array {
  const packed = encode(env);
  const result = new Uint8Array(1 + packed.length);
  result[0] = ProtocolByte;
  result.set(packed, 1);
  return result;
}

/** Deserialize MessagePack bytes (with ProtocolByte prefix) into an Envelope. */
export function unmarshal(data: Uint8Array | ArrayBuffer): Envelope {
  const buf = data instanceof ArrayBuffer ? new Uint8Array(data) : data;
  if (buf.length === 0 || buf[0] !== ProtocolByte) {
    throw new Error(`invalid HOW protocol byte: expected 0x${ProtocolByte.toString(16)}, got 0x${buf.length > 0 ? buf[0].toString(16) : 'empty'}`);
  }
  return decode(buf.subarray(1)) as Envelope;
}

/** Decode the payload field of an Envelope as a specific type. */
export function decodePayload<T>(env: Envelope): T {
  return env.payload as T;
}

export function newHTTPRequest(
  requestID: string,
  req: HTTPRequestPayload,
): Envelope {
  return {
    type: MessageType.HTTPRequest,
    request_id: requestID,
    payload: req,
  };
}

export function newHTTPResponse(
  requestID: string,
  resp: HTTPResponsePayload,
): Envelope {
  return {
    type: MessageType.HTTPResponse,
    request_id: requestID,
    payload: resp,
  };
}

export function newHTTPResponseStart(
  requestID: string,
  statusCode: number,
  headers: Record<string, string[]>,
): Envelope {
  return {
    type: MessageType.HTTPResponseStart,
    request_id: requestID,
    payload: { status_code: statusCode, headers } satisfies HTTPResponseStartPayload,
  };
}

export function newHTTPResponseChunk(
  requestID: string,
  data: Uint8Array,
): Envelope {
  return {
    type: MessageType.HTTPResponseChunk,
    request_id: requestID,
    payload: { data } satisfies HTTPResponseChunkPayload,
  };
}

export function newHTTPResponseEnd(requestID: string): Envelope {
  return { type: MessageType.HTTPResponseEnd, request_id: requestID };
}

export function newError(
  requestID: string,
  code: number,
  message: string,
): Envelope {
  return {
    type: MessageType.Error,
    request_id: requestID,
    payload: { code, message } satisfies ErrorPayload,
  };
}
