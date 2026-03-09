package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// ProtocolByte is the discriminator byte prefixed to every HOW binary frame.
const ProtocolByte = 0x69

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
	Method  string              `msgpack:"method" json:"method"`
	URL     string              `msgpack:"url" json:"url"`
	Headers map[string][]string `msgpack:"headers" json:"headers"`
	Body    []byte              `msgpack:"body,omitempty" json:"body,omitempty"`
}

// HTTPResponsePayload represents an HTTP response serialized for transport.
type HTTPResponsePayload struct {
	StatusCode uint16              `msgpack:"status_code" json:"status_code"`
	Headers    map[string][]string `msgpack:"headers" json:"headers"`
	Body       []byte              `msgpack:"body,omitempty" json:"body,omitempty"`
}

// HTTPResponseStartPayload begins a streaming response (headers only, no body).
type HTTPResponseStartPayload struct {
	StatusCode uint16              `msgpack:"status_code" json:"status_code"`
	Headers    map[string][]string `msgpack:"headers" json:"headers"`
}

// HTTPResponseChunkPayload carries a chunk of streaming response body.
type HTTPResponseChunkPayload struct {
	Data []byte `msgpack:"data" json:"data"`
}

// ErrorPayload represents a protocol-level error.
type ErrorPayload struct {
	Code    uint16 `msgpack:"code" json:"code"`
	Message string `msgpack:"message" json:"message"`
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

// TextEnvelope is the text-mode (JSON) counterpart of Envelope.
// Payload is json.RawMessage instead of msgpack.RawMessage.
type TextEnvelope struct {
	Type      MessageType     `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// MarshalText serializes a TextEnvelope to a JSON string.
func MarshalText(env *TextEnvelope) (string, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// UnmarshalText deserializes a JSON string into a TextEnvelope.
func UnmarshalText(data string) (*TextEnvelope, error) {
	var env TextEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return nil, fmt.Errorf("unmarshal text envelope: %w", err)
	}
	return &env, nil
}

// DecodeTextPayload decodes the JSON payload of a TextEnvelope into the given type.
func DecodeTextPayload[T any](env *TextEnvelope) (*T, error) {
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		return nil, fmt.Errorf("decode text payload (type=0x%02x): %w", env.Type, err)
	}
	return &v, nil
}

// encodeTextPayload encodes a value into a json.RawMessage.
func encodeTextPayload(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// NewTextHTTPRequest creates an HTTPRequest TextEnvelope.
func NewTextHTTPRequest(requestID string, req *HTTPRequestPayload) (*TextEnvelope, error) {
	payload, err := encodeTextPayload(req)
	if err != nil {
		return nil, err
	}
	return &TextEnvelope{Type: TypeHTTPRequest, RequestID: requestID, Payload: payload}, nil
}

// NewTextHTTPResponse creates an HTTPResponse TextEnvelope.
func NewTextHTTPResponse(requestID string, resp *HTTPResponsePayload) (*TextEnvelope, error) {
	payload, err := encodeTextPayload(resp)
	if err != nil {
		return nil, err
	}
	return &TextEnvelope{Type: TypeHTTPResponse, RequestID: requestID, Payload: payload}, nil
}

// NewTextHTTPResponseStart creates an HTTPResponseStart TextEnvelope.
func NewTextHTTPResponseStart(requestID string, statusCode uint16, headers map[string][]string) (*TextEnvelope, error) {
	payload, err := encodeTextPayload(&HTTPResponseStartPayload{StatusCode: statusCode, Headers: headers})
	if err != nil {
		return nil, err
	}
	return &TextEnvelope{Type: TypeHTTPResponseStart, RequestID: requestID, Payload: payload}, nil
}

// NewTextHTTPResponseChunk creates an HTTPResponseChunk TextEnvelope.
func NewTextHTTPResponseChunk(requestID string, data []byte) (*TextEnvelope, error) {
	payload, err := encodeTextPayload(&HTTPResponseChunkPayload{Data: data})
	if err != nil {
		return nil, err
	}
	return &TextEnvelope{Type: TypeHTTPResponseChunk, RequestID: requestID, Payload: payload}, nil
}

// NewTextHTTPResponseEnd creates an HTTPResponseEnd TextEnvelope.
func NewTextHTTPResponseEnd(requestID string) *TextEnvelope {
	return &TextEnvelope{Type: TypeHTTPResponseEnd, RequestID: requestID}
}

// NewTextError creates an Error TextEnvelope.
func NewTextError(requestID string, code uint16, message string) (*TextEnvelope, error) {
	payload, err := encodeTextPayload(&ErrorPayload{Code: code, Message: message})
	if err != nil {
		return nil, err
	}
	return &TextEnvelope{Type: TypeError, RequestID: requestID, Payload: payload}, nil
}

// Marshal serializes an Envelope to MessagePack bytes, prefixed with ProtocolByte.
func Marshal(env *Envelope) ([]byte, error) {
	packed, err := msgpack.Marshal(env)
	if err != nil {
		return nil, err
	}
	result := make([]byte, 1+len(packed))
	result[0] = ProtocolByte
	copy(result[1:], packed)
	return result, nil
}

// Unmarshal deserializes MessagePack bytes (with ProtocolByte prefix) into an Envelope.
func Unmarshal(data []byte) (*Envelope, error) {
	if len(data) == 0 || data[0] != ProtocolByte {
		if len(data) == 0 {
			return nil, fmt.Errorf("invalid HOW protocol byte: empty data")
		}
		return nil, fmt.Errorf("invalid HOW protocol byte: expected 0x%02x, got 0x%02x", ProtocolByte, data[0])
	}
	var env Envelope
	if err := msgpack.Unmarshal(data[1:], &env); err != nil {
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
