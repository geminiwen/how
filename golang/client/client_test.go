package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/geminiwen/http-over-ws/protocol"
)

// wsSendable wraps a websocket.Conn as a Sendable.
type wsSendable struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *wsSendable) Send(data []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

// startWSServer starts an HTTP server with WebSocket upgrade.
// The onConnect callback is called for each WebSocket connection.
func startWSServer(t *testing.T, onConnect func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("ws accept: %v", err)
			return
		}
		onConnect(r.Context(), conn)
	}))
}

func TestCallerHandlerOverWebSocket(t *testing.T) {
	ctx := context.Background()

	// WS server with Handler
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello from handler")
	})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		handler := NewHandler(HTTPHandler(mux), sender)
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			handler.HandleMessage(srvCtx, data)
		}
	})
	defer ts.Close()

	// Caller side: connect via WS
	wsURL := "ws" + ts.URL[4:] // http -> ws
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sender := &wsSendable{conn: conn, ctx: ctx}
	caller := NewCaller(sender)

	// Read loop in background
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/hello",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != "hello from handler" {
		t.Fatalf("expected body 'hello from handler', got %q", string(resp.Body))
	}
}

func TestCallerHandlerForwardOverWebSocket(t *testing.T) {
	// Target HTTP server
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "forwarded: %s %s", r.Method, r.URL.Path)
	}))
	defer target.Close()

	ctx := context.Background()

	fwdHandler, err := ForwardTo(target.URL)
	if err != nil {
		t.Fatalf("ForwardTo: %v", err)
	}

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		handler := NewHandler(fwdHandler, sender)
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			handler.HandleMessage(srvCtx, data)
		}
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sender := &wsSendable{conn: conn, ctx: ctx}
	caller := NewCaller(sender)

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/test",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != "forwarded: GET /test" {
		t.Fatalf("expected body 'forwarded: GET /test', got %q", string(resp.Body))
	}
}

func TestCallerHandlerWithBodyOverWebSocket(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		body := make([]byte, 1024)
		n, _ := r.Body.Read(body)
		w.Write(body[:n])
	})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		handler := NewHandler(HTTPHandler(mux), sender)
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			handler.HandleMessage(srvCtx, data)
		}
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sender := &wsSendable{conn: conn, ctx: ctx}
	caller := NewCaller(sender)

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "POST",
		URL:     "/echo",
		Headers: map[string][]string{"Content-Type": {"text/plain"}},
		Body:    []byte("hello world"),
	})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != "hello world" {
		t.Fatalf("expected body 'hello world', got %q", string(resp.Body))
	}
}
