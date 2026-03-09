package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/geminiwen/how/golang/protocol"
)

// wsSendable wraps a websocket.Conn as a Sendable.
type wsSendable struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *wsSendable) SendBytes(data []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

func (s *wsSendable) SendText(data string) error {
	return s.conn.Write(s.ctx, websocket.MessageText, []byte(data))
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
			handler.HandleBinaryMessage(srvCtx, data)
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
			caller.HandleBinaryMessage(ctx, data)
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
			handler.HandleBinaryMessage(srvCtx, data)
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
			caller.HandleBinaryMessage(ctx, data)
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

func TestForwardHandler(t *testing.T) {
	// Target HTTP server that exercises various HTTP features.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			// Echo back method, headers, query, and body.
			w.Header().Set("Content-Type", "application/json")
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Echo-Method", r.Method)
			w.Header().Set("X-Echo-Query", r.URL.RawQuery)
			w.Header().Set("X-Custom-Header", r.Header.Get("X-Custom-Header"))
			w.WriteHeader(200)
			w.Write(body)

		case "/status/404":
			w.WriteHeader(404)
			fmt.Fprint(w, "not found")

		case "/status/500":
			w.WriteHeader(500)
			fmt.Fprint(w, "internal error")

		default:
			w.WriteHeader(200)
			fmt.Fprintf(w, "ok: %s %s", r.Method, r.URL.Path)
		}
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
			handler.HandleBinaryMessage(srvCtx, data)
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
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	t.Run("POST with body and headers", func(t *testing.T) {
		resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
			Method: "POST",
			URL:    "/echo?foo=bar",
			Headers: map[string][]string{
				"Content-Type":    {"application/json"},
				"X-Custom-Header": {"test-value"},
			},
			Body: []byte(`{"hello":"world"}`),
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		if string(resp.Body) != `{"hello":"world"}` {
			t.Fatalf("body not forwarded: got %q", string(resp.Body))
		}
		// Verify headers were forwarded
		if v := resp.Headers["X-Echo-Method"]; len(v) == 0 || v[0] != "POST" {
			t.Fatalf("method not forwarded: %v", v)
		}
		if v := resp.Headers["X-Echo-Query"]; len(v) == 0 || v[0] != "foo=bar" {
			t.Fatalf("query not forwarded: %v", v)
		}
		if v := resp.Headers["X-Custom-Header"]; len(v) == 0 || v[0] != "test-value" {
			t.Fatalf("custom header not forwarded: %v", v)
		}
	})

	t.Run("PUT request", func(t *testing.T) {
		resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
			Method:  "PUT",
			URL:     "/echo",
			Headers: map[string][]string{"Content-Type": {"text/plain"}},
			Body:    []byte("updated"),
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		if v := resp.Headers["X-Echo-Method"]; len(v) == 0 || v[0] != "PUT" {
			t.Fatalf("PUT method not forwarded: %v", v)
		}
		if string(resp.Body) != "updated" {
			t.Fatalf("body not forwarded: got %q", string(resp.Body))
		}
	})

	t.Run("DELETE request", func(t *testing.T) {
		resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
			Method:  "DELETE",
			URL:     "/echo",
			Headers: map[string][]string{},
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		if v := resp.Headers["X-Echo-Method"]; len(v) == 0 || v[0] != "DELETE" {
			t.Fatalf("DELETE method not forwarded: %v", v)
		}
	})

	t.Run("404 status code", func(t *testing.T) {
		resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
			Method:  "GET",
			URL:     "/status/404",
			Headers: map[string][]string{},
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 404 {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
		if string(resp.Body) != "not found" {
			t.Fatalf("expected 'not found', got %q", string(resp.Body))
		}
	})

	t.Run("500 status code", func(t *testing.T) {
		resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
			Method:  "GET",
			URL:     "/status/500",
			Headers: map[string][]string{},
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 500 {
			t.Fatalf("expected 500, got %d", resp.StatusCode)
		}
		if string(resp.Body) != "internal error" {
			t.Fatalf("expected 'internal error', got %q", string(resp.Body))
		}
	})
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
			handler.HandleBinaryMessage(srvCtx, data)
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
			caller.HandleBinaryMessage(ctx, data)
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

func TestCallerReadTimeout(t *testing.T) {
	ctx := context.Background()

	// Server receives request but never responds.
	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		for {
			_, _, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			// intentionally do nothing — no response sent
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
	caller.ReadTimeout = 100 * time.Millisecond

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	start := time.Now()
	_, err = caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/hello",
		Headers: map[string][]string{},
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("expected ErrReadTimeout, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestCallerReadTimeoutResetByChunks(t *testing.T) {
	ctx := context.Background()

	// Server sends streaming chunks that keep the connection alive,
	// then stops sending — caller should eventually time out.
	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}

		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil {
				return
			}
			if env.Type != protocol.TypeHTTPRequest {
				continue
			}

			// Send ResponseStart
			startEnv, _ := protocol.NewHTTPResponseStart(env.RequestID, 200, map[string][]string{"Content-Type": {"text/plain"}})
			startData, _ := protocol.Marshal(startEnv)
			sender.SendBytes(startData)

			// Send 3 chunks, each within the timeout window
			for i := 0; i < 3; i++ {
				time.Sleep(50 * time.Millisecond)
				chunkEnv, _ := protocol.NewHTTPResponseChunk(env.RequestID, []byte(fmt.Sprintf("chunk%d", i)))
				chunkData, _ := protocol.Marshal(chunkEnv)
				sender.SendBytes(chunkData)
			}

			// Then stop sending — no End message. Caller should time out.
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
	caller.ReadTimeout = 100 * time.Millisecond

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	start := time.Now()
	_, err = caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/stream",
		Headers: map[string][]string{},
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("expected ErrReadTimeout, got %v", err)
	}
	// 3 chunks at 50ms each = ~150ms of activity, then 100ms timeout = ~250ms total.
	// Should be well under 1 second but definitely more than 100ms (proving chunks reset the timer).
	if elapsed < 150*time.Millisecond {
		t.Fatalf("timed out too early (chunks didn't reset timer): %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestTextModeCallerHandlerOverWebSocket(t *testing.T) {
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello from text handler")
	})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		handler := NewHandler(HTTPHandler(mux), sender, WithHandlerTextMode())
		for {
			msgType, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			if msgType == websocket.MessageText {
				handler.HandleTextMessage(srvCtx, string(data))
			} else {
				handler.HandleBinaryMessage(srvCtx, data)
			}
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
	caller := NewCaller(sender, WithTextMode())

	go func() {
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if msgType == websocket.MessageText {
				caller.HandleTextMessage(ctx, string(data))
			} else {
				caller.HandleBinaryMessage(ctx, data)
			}
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
	if string(resp.Body) != "hello from text handler" {
		t.Fatalf("expected body 'hello from text handler', got %q", string(resp.Body))
	}
}

func TestTextModeForwardHandlerOverWebSocket(t *testing.T) {
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
		handler := NewHandler(fwdHandler, sender, WithHandlerTextMode())
		for {
			msgType, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			if msgType == websocket.MessageText {
				handler.HandleTextMessage(srvCtx, string(data))
			} else {
				handler.HandleBinaryMessage(srvCtx, data)
			}
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
	caller := NewCaller(sender, WithTextMode())

	go func() {
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if msgType == websocket.MessageText {
				caller.HandleTextMessage(ctx, string(data))
			} else {
				caller.HandleBinaryMessage(ctx, data)
			}
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
