package protocol

import (
	"testing"
)

func TestRoundTripHTTPRequest(t *testing.T) {
	req := &HTTPRequestPayload{
		Method:  "POST",
		URL:     "/api/users?page=1",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    []byte(`{"name":"test"}`),
	}

	env, err := NewHTTPRequest("req-123", req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.RequestID != "req-123" {
		t.Fatalf("expected request_id 'req-123', got %q", decoded.RequestID)
	}

	payload, err := DecodePayload[HTTPRequestPayload](decoded)
	if err != nil {
		t.Fatal(err)
	}

	if payload.Method != "POST" {
		t.Fatalf("expected method POST, got %q", payload.Method)
	}
	if payload.URL != "/api/users?page=1" {
		t.Fatalf("expected url '/api/users?page=1', got %q", payload.URL)
	}
	if string(payload.Body) != `{"name":"test"}` {
		t.Fatalf("expected body, got %q", string(payload.Body))
	}
}

func TestRoundTripHTTPResponse(t *testing.T) {
	resp := &HTTPResponsePayload{
		StatusCode: 200,
		Headers:    map[string][]string{"Content-Type": {"text/plain"}},
		Body:       []byte("hello"),
	}

	env, err := NewHTTPResponse("req-456", resp)
	if err != nil {
		t.Fatal(err)
	}

	data, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}

	payload, err := DecodePayload[HTTPResponsePayload](decoded)
	if err != nil {
		t.Fatal(err)
	}

	if payload.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", payload.StatusCode)
	}
	if string(payload.Body) != "hello" {
		t.Fatalf("expected body 'hello', got %q", string(payload.Body))
	}
}

func TestRoundTripError(t *testing.T) {
	env, err := NewError("req-789", ErrClientNotFound, "client not found")
	if err != nil {
		t.Fatal(err)
	}

	data, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}

	if decoded.Type != TypeError {
		t.Fatalf("expected Error type, got 0x%02x", decoded.Type)
	}

	payload, err := DecodePayload[ErrorPayload](decoded)
	if err != nil {
		t.Fatal(err)
	}

	if payload.Code != ErrClientNotFound {
		t.Fatalf("expected code %d, got %d", ErrClientNotFound, payload.Code)
	}
	if payload.Message != "client not found" {
		t.Fatalf("expected message 'client not found', got %q", payload.Message)
	}
}
