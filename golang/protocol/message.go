package protocol

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// MessageType identifies the type of a HOW protocol message.
type MessageType uint8

const (
	TypeHTTPRequest       MessageType = 0x10
	TypeHTTPResponse      MessageType = 0x11
	TypeHTTPResponseStart MessageType = 0x12
	TypeHTTPResponseChunk MessageType = 0x13
	TypeHTTPResponseEnd   MessageType = 0x14
	TypeError             MessageType = 0xFF
)

// Envelope is the top-level wire format for all HOW messages.
type Envelope struct {
	Type      MessageType        `msgpack:"type"`
	RequestID string             `msgpack:"request_id,omitempty"`
	Payload   msgpack.RawMessage `msgpack:"payload,omitempty"`
}

// HTTPRequestPayload represents an HTTP request serialized for transport.
type HTTPRequestPayload struct {
	Method  string              `msgpack:"method"`
	URL     string              `msgpack:"url"`
	Headers map[string][]string `msgpack:"headers"`
	Body    []byte              `msgpack:"body,omitempty"`
}

// HTTPResponsePayload represents an HTTP response serialized for transport.
type HTTPResponsePayload struct {
	StatusCode uint16              `msgpack:"status_code"`
	Headers    map[string][]string `msgpack:"headers"`
	Body       []byte              `msgpack:"body,omitempty"`
}

// HTTPResponseStartPayload begins a streaming response (headers only, no body).
type HTTPResponseStartPayload struct {
	StatusCode uint16              `msgpack:"status_code"`
	Headers    map[string][]string `msgpack:"headers"`
}

// HTTPResponseChunkPayload carries a chunk of streaming response body.
type HTTPResponseChunkPayload struct {
	Data []byte `msgpack:"data"`
}

// ErrorPayload represents a protocol-level error.
type ErrorPayload struct {
	Code    uint16 `msgpack:"code"`
	Message string `msgpack:"message"`
}

// Error code constants.
const (
	ErrUnknown          uint16 = 1000
	ErrClientNotFound   uint16 = 1001
	ErrRequestTimeout   uint16 = 1002
	ErrInvalidMessage   uint16 = 1003
	ErrRegistrationFail uint16 = 1004
	ErrClientIDConflict uint16 = 1005
	ErrInternal         uint16 = 1006
	ErrRequestCancelled uint16 = 1007
)

// Marshal serializes an Envelope to MessagePack bytes.
func Marshal(env *Envelope) ([]byte, error) {
	return msgpack.Marshal(env)
}

// Unmarshal deserializes MessagePack bytes into an Envelope.
func Unmarshal(data []byte) (*Envelope, error) {
	var env Envelope
	if err := msgpack.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return &env, nil
}

// DecodePayload decodes the raw payload of an Envelope into the given type.
func DecodePayload[T any](env *Envelope) (*T, error) {
	var v T
	if err := msgpack.Unmarshal(env.Payload, &v); err != nil {
		return nil, fmt.Errorf("decode payload (type=0x%02x): %w", env.Type, err)
	}
	return &v, nil
}

// encodePayload encodes a value into a msgpack.RawMessage.
func encodePayload(v any) (msgpack.RawMessage, error) {
	data, err := msgpack.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// NewHTTPRequest creates an HTTPRequest envelope.
func NewHTTPRequest(requestID string, req *HTTPRequestPayload) (*Envelope, error) {
	payload, err := encodePayload(req)
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: TypeHTTPRequest, RequestID: requestID, Payload: payload}, nil
}

// NewHTTPResponse creates an HTTPResponse envelope.
func NewHTTPResponse(requestID string, resp *HTTPResponsePayload) (*Envelope, error) {
	payload, err := encodePayload(resp)
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: TypeHTTPResponse, RequestID: requestID, Payload: payload}, nil
}

// NewHTTPResponseStart creates an HTTPResponseStart envelope.
func NewHTTPResponseStart(requestID string, statusCode uint16, headers map[string][]string) (*Envelope, error) {
	payload, err := encodePayload(&HTTPResponseStartPayload{StatusCode: statusCode, Headers: headers})
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: TypeHTTPResponseStart, RequestID: requestID, Payload: payload}, nil
}

// NewHTTPResponseChunk creates an HTTPResponseChunk envelope.
func NewHTTPResponseChunk(requestID string, data []byte) (*Envelope, error) {
	payload, err := encodePayload(&HTTPResponseChunkPayload{Data: data})
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: TypeHTTPResponseChunk, RequestID: requestID, Payload: payload}, nil
}

// NewHTTPResponseEnd creates an HTTPResponseEnd envelope.
func NewHTTPResponseEnd(requestID string) *Envelope {
	return &Envelope{Type: TypeHTTPResponseEnd, RequestID: requestID}
}

// NewError creates an Error envelope.
func NewError(requestID string, code uint16, message string) (*Envelope, error) {
	payload, err := encodePayload(&ErrorPayload{Code: code, Message: message})
	if err != nil {
		return nil, err
	}
	return &Envelope{Type: TypeError, RequestID: requestID, Payload: payload}, nil
}
