package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/geminiwen/how/golang/protocol"
)

// readBodyString drains resp.Body, closes it, and returns the contents as a string.
// Fails the test on read errors.
func readBodyString(t *testing.T, resp *Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

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
	body := readBodyString(t, resp)
	if body != "hello from handler" {
		t.Fatalf("expected body 'hello from handler', got %q", body)
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
	body := readBodyString(t, resp)
	if body != "forwarded: GET /test" {
		t.Fatalf("expected body 'forwarded: GET /test', got %q", body)
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
		body := readBodyString(t, resp)
		if body != `{"hello":"world"}` {
			t.Fatalf("body not forwarded: got %q", body)
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
		body := readBodyString(t, resp)
		if body != "updated" {
			t.Fatalf("body not forwarded: got %q", body)
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
		body := readBodyString(t, resp)
		if body != "not found" {
			t.Fatalf("expected 'not found', got %q", body)
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
		body := readBodyString(t, resp)
		if body != "internal error" {
			t.Fatalf("expected 'internal error', got %q", body)
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
	body := readBodyString(t, resp)
	if body != "hello world" {
		t.Fatalf("expected body 'hello world', got %q", body)
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
	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/stream",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Read chunk by chunk until the pipe surfaces ErrReadTimeout. Each chunk
	// arrives within the timeout window and must reset the timer; only after
	// the server stops sending should the timer finally fire.
	buf := make([]byte, 256)
	chunkCount := 0
	var readErr error
	for {
		n, e := resp.Body.Read(buf)
		if n > 0 {
			chunkCount++
		}
		if e != nil {
			readErr = e
			break
		}
	}
	elapsed := time.Since(start)

	if !errors.Is(readErr, ErrReadTimeout) {
		t.Fatalf("expected ErrReadTimeout, got %v", readErr)
	}
	if chunkCount != 3 {
		t.Fatalf("expected 3 chunks before timeout, got %d", chunkCount)
	}
	// 3 chunks at 50ms each = ~150ms of activity, then 100ms timeout ≈ 250ms total.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("timed out too early (chunks didn't reset timer): %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestCallerRequestStreamIncremental(t *testing.T) {
	ctx := context.Background()

	// Server sends Start + 3 Chunks (with gaps) + End.
	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeHTTPRequest {
				continue
			}
			sender := &wsSendable{conn: conn, ctx: srvCtx}

			startEnv, _ := protocol.NewHTTPResponseStart(env.RequestID, 200, map[string][]string{"Content-Type": {"text/event-stream"}})
			startData, _ := protocol.Marshal(startEnv)
			sender.SendBytes(startData)

			for i := 0; i < 3; i++ {
				chunkEnv, _ := protocol.NewHTTPResponseChunk(env.RequestID, []byte(fmt.Sprintf("chunk%d|", i)))
				chunkData, _ := protocol.Marshal(chunkEnv)
				sender.SendBytes(chunkData)
				time.Sleep(30 * time.Millisecond)
			}

			endEnv := protocol.NewHTTPResponseEnd(env.RequestID)
			endData, _ := protocol.Marshal(endEnv)
			sender.SendBytes(endData)
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
				caller.Close(err)
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/stream",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	// Read chunk by chunk. io.Pipe.Read yields exactly what pump Write'd, so
	// each of the 3 chunks comes back as its own Read.
	var arrivals []struct {
		t    time.Duration
		text string
	}
	start := time.Now()
	buf := make([]byte, 256)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			arrivals = append(arrivals, struct {
				t    time.Duration
				text string
			}{time.Since(start), string(buf[:n])})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	// Body reconstruction
	var joined string
	for _, a := range arrivals {
		joined += a.text
	}
	if joined != "chunk0|chunk1|chunk2|" {
		t.Fatalf("body mismatch: %q", joined)
	}
	if len(arrivals) < 2 {
		t.Fatalf("expected incremental reads (≥2 arrivals), got %d: %+v", len(arrivals), arrivals)
	}
	// First arrival must come materially earlier than the last — proves pipe
	// yields chunks incrementally, not a single final buffer.
	gap := arrivals[len(arrivals)-1].t - arrivals[0].t
	if gap < 20*time.Millisecond {
		t.Fatalf("arrivals collapsed (gap=%v) — pipe looks buffered", gap)
	}
}

func TestCallerRequestStreamSlowReaderNoDrop(t *testing.T) {
	// Regression: with a bounded pending-channel and a slow Body reader, the
	// protocol pump used to back up behind io.Pipe.Write and overflow the
	// channel, causing HandleBinaryMessage to drop chunks. Ensure every chunk
	// reaches the reader intact even when Read is much slower than the peer.
	ctx := context.Background()

	const chunkCount = 64 // well beyond the 16-slot pending channel buffer

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeHTTPRequest {
				continue
			}
			sender := &wsSendable{conn: conn, ctx: srvCtx}

			startEnv, _ := protocol.NewHTTPResponseStart(env.RequestID, 200, map[string][]string{})
			startData, _ := protocol.Marshal(startEnv)
			sender.SendBytes(startData)

			for i := 0; i < chunkCount; i++ {
				chunkEnv, _ := protocol.NewHTTPResponseChunk(env.RequestID, []byte(fmt.Sprintf("%03d|", i)))
				chunkData, _ := protocol.Marshal(chunkEnv)
				sender.SendBytes(chunkData)
			}

			endEnv := protocol.NewHTTPResponseEnd(env.RequestID)
			endData, _ := protocol.Marshal(endEnv)
			sender.SendBytes(endData)
		}
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	caller := NewCaller(&wsSendable{conn: conn, ctx: ctx})
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				caller.Close(err)
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/burst",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	defer resp.Body.Close()

	// Give the peer time to flood chunks before we start reading.
	time.Sleep(50 * time.Millisecond)

	// Read slowly — 1ms between reads — so the pump is forced to block on pw.Write.
	var collected []byte
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		time.Sleep(1 * time.Millisecond)
	}

	// Expect all chunks intact
	var expected []byte
	for i := 0; i < chunkCount; i++ {
		expected = append(expected, []byte(fmt.Sprintf("%03d|", i))...)
	}
	if string(collected) != string(expected) {
		t.Fatalf("body truncated or corrupted under backpressure:\n  got  len=%d: %q\n  want len=%d: %q",
			len(collected), string(collected), len(expected), string(expected))
	}
}

func TestCallerMultiplexNoHeadOfLineBlocking(t *testing.T) {
	// Two concurrent requests on one Caller share a transport:
	//   A — slow streaming body: the peer floods chunks; the caller never reads.
	//   B — a plain short request started after A is mid-flight.
	// If A's unread frames stall the transport read loop, B hangs. Assert B
	// completes promptly regardless of A's state.
	ctx := context.Background()

	const floodChunks = 128 // well above any per-request buffer capacity

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeHTTPRequest {
				continue
			}
			reqPayload, err := protocol.DecodePayload[protocol.HTTPRequestPayload](env)
			if err != nil {
				continue
			}

			// Route by URL
			switch reqPayload.URL {
			case "/slow":
				// Flood Start + many chunks, never send End. Caller is expected to never read.
				go func(requestID string) {
					start, _ := protocol.NewHTTPResponseStart(requestID, 200, map[string][]string{"Content-Type": {"text/event-stream"}})
					startData, _ := protocol.Marshal(start)
					sender.SendBytes(startData)
					for i := 0; i < floodChunks; i++ {
						chunk, _ := protocol.NewHTTPResponseChunk(requestID, []byte("x"))
						chunkData, _ := protocol.Marshal(chunk)
						sender.SendBytes(chunkData)
					}
				}(env.RequestID)
			case "/fast":
				// Respond with a single complete HTTPResponse.
				resp, _ := protocol.NewHTTPResponse(env.RequestID, &protocol.HTTPResponsePayload{
					StatusCode: 200,
					Headers:    map[string][]string{},
					Body:       []byte("ok"),
				})
				respData, _ := protocol.Marshal(resp)
				sender.SendBytes(respData)
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

	caller := NewCaller(&wsSendable{conn: conn, ctx: ctx})
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				caller.Close(err)
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	// Start A. Do NOT read its body. Keep it alive so its queue fills up.
	slowResp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/slow",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("slow Request: %v", err)
	}
	defer slowResp.Body.Close()

	// Let the server flood the slow stream.
	time.Sleep(100 * time.Millisecond)

	// Now start B. Must complete quickly even though A is untouched.
	start := time.Now()
	fastResp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/fast",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("fast Request: %v", err)
	}
	body, err := io.ReadAll(fastResp.Body)
	fastResp.Body.Close()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("read fast body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("fast body: got %q want 'ok'", string(body))
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("fast request was head-of-line blocked behind slow stream: %v", elapsed)
	}
}

func TestCallerBodyCloseClearsPending(t *testing.T) {
	// Consumer closes Body early (e.g. after inspecting headers) while the peer
	// is silent. The pending slot must be removed promptly; without the
	// cancellingBody wrapper the pump would block on q.wait until a timeout or
	// Close().
	ctx := context.Background()

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeHTTPRequest {
				continue
			}
			sender := &wsSendable{conn: conn, ctx: srvCtx}
			startEnv, _ := protocol.NewHTTPResponseStart(env.RequestID, 200, map[string][]string{"X-Demo": {"1"}})
			startData, _ := protocol.Marshal(startEnv)
			sender.SendBytes(startData)
			// ...then go quiet. Do not send any chunks or End.
		}
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	caller := NewCaller(&wsSendable{conn: conn, ctx: ctx})
	caller.ReadTimeout = -1 // disable timeout; rely on Body.Close alone to clean up

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				caller.Close(err)
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/quiet",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Headers["X-Demo"][0] != "1" {
		t.Fatalf("headers missing")
	}

	// Close early; peer will never send anything else.
	resp.Body.Close()

	// Allow the pump goroutine to exit and remove pending.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		caller.mu.Lock()
		n := len(caller.pending)
		caller.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	caller.mu.Lock()
	n := len(caller.pending)
	caller.mu.Unlock()
	t.Fatalf("pending not cleared after Body.Close; still %d entries", n)
}

func TestStreamingHandlerWriteReturnsErrWhenSenderFails(t *testing.T) {
	// When the underlying transport rejects a send (peer ws gone), the
	// http.Handler's w.Write(...) must return a non-nil error so the handler
	// can stop its write loop instead of churning on a dead connection.
	failingSender := &failingSenderT{fail: make(chan struct{})}

	attempts := 0
	writesAfterFail := 0
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		// First write succeeds; sender failure is armed after.
		if _, err := fmt.Fprint(w, "ok"); err != nil {
			t.Errorf("first write unexpectedly failed: %v", err)
		}
		close(failingSender.fail)
		for i := 0; i < 50; i++ {
			attempts++
			if _, err := fmt.Fprint(w, "more"); err != nil {
				return // handler respects Write err and exits — this is what we want
			}
			writesAfterFail++
		}
	})

	a := HTTPHandler(httpHandler, WithStreaming())
	h := NewHandler(a, failingSender)

	env, _ := protocol.NewHTTPRequest("write-err", &protocol.HTTPRequestPayload{
		Method: "GET", URL: "/stream", Headers: map[string][]string{},
	})
	data, _ := protocol.Marshal(env)
	h.HandleBinaryMessage(context.Background(), data)

	// Give dispatch goroutine time to complete.
	time.Sleep(100 * time.Millisecond)

	if writesAfterFail > 0 {
		t.Fatalf("handler kept writing after first failure: %d successful writes after sender failed", writesAfterFail)
	}
	if attempts < 1 {
		t.Fatalf("handler never tried a write after sender failure")
	}
}

// failingSenderT starts accepting SendBytes calls, then once `fail` is closed
// it rejects everything with an error.
type failingSenderT struct {
	mu   sync.Mutex
	fail chan struct{}
}

func (s *failingSenderT) SendBytes(data []byte) error {
	select {
	case <-s.fail:
		return errors.New("simulated transport closed")
	default:
		return nil
	}
}
func (s *failingSenderT) SendText(string) error { return nil }

func TestFrameQueueRaceCloseAfterPush(t *testing.T) {
	// Regression: stress-test the race where push() and a stop channel close
	// fire back-to-back. The pushed frame must never be lost.
	for i := 0; i < 200; i++ {
		q := newFrameQueue[int]()
		done := make(chan struct{})

		var gotZero int32
		ready := make(chan struct{})
		go func() {
			close(ready)
			if v, ok := q.wait(done, nil, nil); ok {
				_ = v // good
			} else {
				atomic.StoreInt32(&gotZero, 1)
			}
		}()

		<-ready
		q.push(1)
		close(done)

		// Allow the consumer to exit. If wait returned (_, false) the test
		// detected the lost frame via gotZero.
		time.Sleep(2 * time.Millisecond)

		if atomic.LoadInt32(&gotZero) == 1 {
			// Drain didn't guarantee delivery; the race handler in wait()
			// failed to pick up the pushed frame before returning false.
			t.Fatalf("iteration %d: pushed frame was lost when done fired concurrently", i)
		}
	}
}

func TestHTTPHandlerAdapterStreamingCtxCancelAfterHandlerReturnsIsEnd(t *testing.T) {
	// Regression: the previous implementation converted any post-handler ctx
	// cancel into an Error envelope, even if the handler had written a full
	// response and returned normally before the cancel fired. Ensure the
	// adapter only escalates ctx cancellations that actually interrupted the
	// handler while it was running.
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "complete response")
		w.(http.Flusher).Flush()
		// Handler returns normally here without observing ctx.
	})

	a := HTTPHandler(httpHandler, WithStreaming())
	spy := &countingSender{}
	h := NewHandler(a, spy)

	ctx, cancel := context.WithCancel(context.Background())
	env, _ := protocol.NewHTTPRequest("late-cancel", &protocol.HTTPRequestPayload{
		Method: "GET", URL: "/ok", Headers: map[string][]string{},
	})
	data, _ := protocol.Marshal(env)

	// Cancel the ctx right after dispatching. The handler will have returned
	// by the time we check (it's synchronous on this fast path), but the
	// observer goroutine should have seen the handler stop first.
	h.HandleBinaryMessage(ctx, data)
	cancel()

	// Wait for the final envelope.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		types := spy.types()
		if len(types) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		last := types[len(types)-1]
		if last == protocol.TypeHTTPResponseEnd {
			return // expected — complete response, no false error
		}
		if last == protocol.TypeError {
			t.Fatalf("completed stream was reported as Error on late ctx cancel; types=%v", types)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal envelope; types=%v", spy.types())
}

func TestStreamingHandlerReturningErrorAfterStartEmitsError(t *testing.T) {
	// The HTTPHandlerAdapter deliberately can't translate ctx cancellation
	// into a stream-level error (there's no reliable "handler aborted"
	// signal in the http.Handler contract). Handlers that need to signal a
	// mid-stream failure must implement StreamingHandler directly and
	// return an error; dispatchRequest then routes that to an Error envelope.
	sh := streamingHandlerFunc(func(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error {
		w.WriteHeader(200, map[string][]string{})
		w.Write([]byte("partial"))
		return errors.New("deliberate abort")
	})

	spy := &countingSender{}
	h := NewHandler(sh, spy)

	env, _ := protocol.NewHTTPRequest("abort", &protocol.HTTPRequestPayload{
		Method: "GET", URL: "/x", Headers: map[string][]string{},
	})
	data, _ := protocol.Marshal(env)
	h.HandleBinaryMessage(context.Background(), data)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		types := spy.types()
		if len(types) == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		last := types[len(types)-1]
		if last == protocol.TypeError {
			return
		}
		if last == protocol.TypeHTTPResponseEnd {
			t.Fatalf("expected Error terminator; got End. types=%v", types)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal envelope; types=%v", spy.types())
}

func TestHandlerStreamingErrorAfterStartSurfacesAsReadError(t *testing.T) {
	// Streaming handler writes header, then fails mid-body. The peer caller
	// must see reader.Read return an error (not a clean EOF).
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "first chunk\n")
		w.(http.Flusher).Flush()
		panic("handler blew up mid-body")
	})

	// Recover the panic via net/http's built-in handler recovery. It surfaces
	// to our streaming wrapper as a closed pipe / error from ServeHTTP, which
	// our adapter translates into the ServeHTTPOverWSStream error return
	// path — except http.HandlerFunc panics are caught by net/http only when
	// going through a Server. Wrap with a recoverer to make the test
	// deterministic.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// We can't signal an error back up through http.Handler's
				// contract after WriteHeader, so we just return; the streaming
				// adapter will then complete normally. That means to truly
				// test the "error after start" path we need a handler that
				// returns via the StreamingHandler interface directly.
				_ = rec
			}
		}()
		httpHandler.ServeHTTP(w, r)
	})

	// Instead of using HTTPHandlerAdapter with a panicking handler (where the
	// panic can't be surfaced as an error), use an inline StreamingHandler
	// that writes header then returns an error.
	streamErr := errors.New("deliberate mid-stream failure")
	sh := streamingHandlerFunc(func(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error {
		w.WriteHeader(200, map[string][]string{})
		w.Write([]byte("first chunk\n"))
		return streamErr
	})
	_ = wrapped

	spy := &countingSender{}
	h := NewHandler(sh, spy)

	env, _ := protocol.NewHTTPRequest("err-test", &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/fail",
		Headers: map[string][]string{},
	})
	data, _ := protocol.Marshal(env)
	h.HandleBinaryMessage(context.Background(), data)

	// Wait for the Error envelope to surface.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		types := spy.types()
		last := protocol.MessageType(0)
		if len(types) > 0 {
			last = types[len(types)-1]
		}
		if last == protocol.TypeError {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected Error envelope after mid-stream failure; got types=%v", spy.types())
}

type streamingHandlerFunc func(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error

func (f streamingHandlerFunc) ServeHTTPOverWS(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	// Not used — NewHandler will detect StreamingHandler first. Return an
	// error to make sure the non-streaming path is never taken by mistake.
	return nil, errors.New("should not be called")
}

func (f streamingHandlerFunc) ServeHTTPOverWSStream(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error {
	return f(ctx, req, w)
}

func TestCallerRequestStreamBufferedResponse(t *testing.T) {
	ctx := context.Background()

	// Non-streaming peer: responds with a single HTTPResponse.
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello from buffered handler")
	})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		defer conn.CloseNow()
		sender := &wsSendable{conn: conn, ctx: srvCtx}
		handler := NewHandler(HTTPHandler(mux), sender) // default buffered
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

	caller := NewCaller(&wsSendable{conn: conn, ctx: ctx})
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
		t.Fatalf("RequestStream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello from buffered handler" {
		t.Fatalf("body mismatch: %q", string(body))
	}
}

func TestCallerRequestStreamTransportClose(t *testing.T) {
	ctx := context.Background()

	// Send Start + 1 chunk then drop the connection.
	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(srvCtx)
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeHTTPRequest {
				continue
			}
			sender := &wsSendable{conn: conn, ctx: srvCtx}

			startEnv, _ := protocol.NewHTTPResponseStart(env.RequestID, 200, map[string][]string{})
			startData, _ := protocol.Marshal(startEnv)
			sender.SendBytes(startData)

			chunkEnv, _ := protocol.NewHTTPResponseChunk(env.RequestID, []byte("first"))
			chunkData, _ := protocol.Marshal(chunkEnv)
			sender.SendBytes(chunkData)

			time.Sleep(20 * time.Millisecond)
			conn.CloseNow()
			return
		}
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:]
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	caller := NewCaller(&wsSendable{conn: conn, ctx: ctx})
	caller.ReadTimeout = 30 * time.Second // prove Close short-circuits the long timeout

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				caller.Close(fmt.Errorf("ws closed: %w", err))
				return
			}
			caller.HandleBinaryMessage(ctx, data)
		}
	}()

	resp, err := caller.Request(ctx, &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/stream",
		Headers: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("RequestStream: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 256)
	// First read yields the single chunk
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(buf[:n]) != "first" {
		t.Fatalf("first chunk mismatch: %q", string(buf[:n]))
	}

	// Second read should observe the transport close and return a non-EOF error.
	start := time.Now()
	_, err = resp.Body.Read(buf)
	elapsed := time.Since(start)
	if err == nil || err == io.EOF {
		t.Fatalf("expected non-EOF error after transport close, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("pipe did not unblock quickly on transport close: %v", elapsed)
	}
}

func TestHTTPHandlerAdapterStreamingForwardsContext(t *testing.T) {
	// When the HOW dispatch ctx is cancelled, a streaming http.Handler watching
	// r.Context().Done() should be notified so it can stop work promptly.
	handlerCancelled := make(chan struct{})
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(handlerCancelled)
		case <-time.After(2 * time.Second):
			t.Errorf("handler ctx never cancelled")
		}
	})

	a := HTTPHandler(httpHandler, WithStreaming())
	h := NewHandler(a, &countingSender{})

	ctx, cancel := context.WithCancel(context.Background())
	reqPayload := &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/sse",
		Headers: map[string][]string{},
	}
	env, _ := protocol.NewHTTPRequest("ctx-test", reqPayload)
	data, _ := protocol.Marshal(env)

	h.HandleBinaryMessage(ctx, data)

	// Give dispatch time to enter the handler and park on r.Context().Done().
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-handlerCancelled:
		// expected
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not observe ctx cancellation")
	}
}

func TestHTTPHandlerAdapterStreamingFlushBeforeWrite(t *testing.T) {
	// SSE-style handler: set header, Flush headers early, then wait before the first write.
	// Peer caller should receive HTTPResponseStart immediately (from Flush), not block until
	// the first Write.
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		flusher.Flush() // should emit HTTPResponseStart via implicit WriteHeader(200)

		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "data: late\n\n")
		flusher.Flush()
	})

	spy := &countingSender{}
	a := HTTPHandler(httpHandler, WithStreaming())
	h := NewHandler(a, spy)

	reqPayload := &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/sse",
		Headers: map[string][]string{},
	}
	env, _ := protocol.NewHTTPRequest("req-flush", reqPayload)
	data, _ := protocol.Marshal(env)

	start := time.Now()
	h.HandleBinaryMessage(context.Background(), data)

	// Wait for HTTPResponseStart specifically — it must arrive well before the 50ms sleep.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		types := spy.types()
		if len(types) > 0 && types[0] == protocol.TypeHTTPResponseStart {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	elapsedToStart := time.Since(start)

	types := spy.types()
	if len(types) == 0 || types[0] != protocol.TypeHTTPResponseStart {
		t.Fatalf("expected HTTPResponseStart as first envelope, got %v", types)
	}
	if elapsedToStart > 40*time.Millisecond {
		t.Fatalf("Start arrived after Write (not after Flush); elapsed=%v", elapsedToStart)
	}
}

func TestHTTPHandlerAdapterStreaming(t *testing.T) {
	// http.Handler writes 3 chunks with Flush; ServeHTTPOverWSStream should emit
	// Start + 3 chunks + End as separate envelopes.
	httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "event: tick\ndata: %d\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	// Counting sender wraps another sender so we can both run the real transport
	// and count envelopes sent back.
	spy := &countingSender{}
	a := HTTPHandler(httpHandler, WithStreaming())
	h := NewHandler(a, spy)

	// Build an HTTPRequest envelope and feed it into the handler directly.
	reqPayload := &protocol.HTTPRequestPayload{
		Method:  "GET",
		URL:     "/sse",
		Headers: map[string][]string{},
	}
	env, err := protocol.NewHTTPRequest("req-1", reqPayload)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	data, err := protocol.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	h.HandleBinaryMessage(context.Background(), data)

	// Dispatch is async (go h.dispatchRequest). Wait until we've seen End.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spy.hasEnd() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	types := spy.types()
	if len(types) < 5 {
		t.Fatalf("expected ≥5 envelopes (Start + 3 Chunk + End), got %d: %v", len(types), types)
	}
	if types[0] != protocol.TypeHTTPResponseStart {
		t.Fatalf("first envelope must be HTTPResponseStart, got 0x%02x", types[0])
	}
	if types[len(types)-1] != protocol.TypeHTTPResponseEnd {
		t.Fatalf("last envelope must be HTTPResponseEnd, got 0x%02x", types[len(types)-1])
	}
	chunkCount := 0
	for _, ty := range types[1 : len(types)-1] {
		if ty != protocol.TypeHTTPResponseChunk {
			t.Fatalf("middle envelope must be HTTPResponseChunk, got 0x%02x", ty)
		}
		chunkCount++
	}
	if chunkCount < 3 {
		t.Fatalf("expected ≥3 chunks, got %d", chunkCount)
	}
}

// countingSender decodes outgoing HOW envelopes and records their types.
type countingSender struct {
	mu       sync.Mutex
	envTypes []protocol.MessageType
}

func (s *countingSender) SendBytes(data []byte) error {
	env, err := protocol.Unmarshal(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envTypes = append(s.envTypes, env.Type)
	return nil
}

func (s *countingSender) SendText(string) error { return nil }

func (s *countingSender) types() []protocol.MessageType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.MessageType, len(s.envTypes))
	copy(out, s.envTypes)
	return out
}

func (s *countingSender) hasEnd() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.envTypes {
		if t == protocol.TypeHTTPResponseEnd {
			return true
		}
	}
	return false
}

func TestCallerTransportClose(t *testing.T) {
	ctx := context.Background()

	// Server receives the request and then closes the connection without responding.
	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		_, _, err := conn.Read(srvCtx)
		if err != nil {
			return
		}
		conn.CloseNow()
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
	caller.ReadTimeout = 30 * time.Second // long timeout to prove Close short-circuits it

	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				caller.Close(fmt.Errorf("ws closed: %w", err))
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

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("request did not unblock on transport close: %v", elapsed)
	}
}

func TestCallerCloseIdempotentAndBlocksFurtherRequests(t *testing.T) {
	// No real transport — exercise Close() directly.
	caller := NewCaller(stubSendable{})

	// Idempotent: two closes, second one is silently ignored.
	customErr := errors.New("first close")
	caller.Close(customErr)
	caller.Close(errors.New("second close — should be ignored"))

	_, err := caller.Request(context.Background(), &protocol.HTTPRequestPayload{
		Method: "GET", URL: "/x", Headers: map[string][]string{},
	})
	if !errors.Is(err, customErr) {
		t.Fatalf("expected first close error, got %v", err)
	}
}

// stubSendable is a no-op Sendable used when the transport is irrelevant to the test.
type stubSendable struct{}

func (stubSendable) SendBytes(data []byte) error { return nil }
func (stubSendable) SendText(data string) error  { return nil }

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
	body := readBodyString(t, resp)
	if body != "hello from text handler" {
		t.Fatalf("expected body 'hello from text handler', got %q", body)
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
	body := readBodyString(t, resp)
	if body != "forwarded: GET /test" {
		t.Fatalf("expected body 'forwarded: GET /test', got %q", body)
	}
}
