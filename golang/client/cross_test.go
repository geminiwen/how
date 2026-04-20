package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/geminiwen/how/golang/protocol"
)

// tsDir returns the absolute path to the typescript/ directory.
func tsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "typescript")
}

// crossLangOpts parametrizes the subprocess-based cross-language test harness.
type crossLangOpts struct {
	mode      string // "binary" (default) or "text"
	streaming bool   // enable the streaming HTTPHandlerAdapter on the Node side
}

// bufferedResponse is a drained Response convenience shape used by the
// cross-language tests — each request buffers the full body so assertions
// stay simple.
type bufferedResponse struct {
	StatusCode uint16
	Headers    map[string][]string
	Body       []byte
}

// crossLangEnv is the handle returned by setupCrossLang; tests use it to
// fire requests and then call cleanup() at the end. The subprocess running
// the TS handler is torn down in cleanup.
type crossLangEnv struct {
	caller    *Caller
	doRequest func(t *testing.T, req *protocol.HTTPRequestPayload) (*bufferedResponse, error)
	cleanup   func()
}

// setupCrossLang boots a WS server, launches the Node handler subprocess with
// the requested mode/streaming flags, waits for READY + WS connect, and wires
// a Caller to the resulting connection. The returned cleanup function tears
// everything down and blocks until the Node process exits.
//
// Failure modes (Node crashes, timeout, READY not received) are surfaced as
// t.Fatal so tests don't have to duplicate error handling. Stderr from the
// Node process is buffered and logged via drainStderr on any failure path,
// which is essential because subprocess test failures are otherwise opaque.
func setupCrossLang(t *testing.T, opts crossLangOpts) *crossLangEnv {
	t.Helper()
	if opts.mode == "" {
		opts.mode = "binary"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	connCh := make(chan *websocket.Conn, 1)
	done := make(chan struct{})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		connCh <- conn
		// Keep the handler goroutine alive until test ends.
		<-done
		conn.CloseNow()
	})

	wsURL := "ws" + ts.URL[4:] // http -> ws

	scriptPath := filepath.Join(tsDir(), "src", "client", "cross_test_handler.ts")
	args := []string{"tsx", scriptPath, wsURL}
	if opts.mode == "text" {
		args = append(args, "--text")
	}
	if opts.streaming {
		args = append(args, "--streaming")
	}
	cmd := exec.CommandContext(ctx, "npx", args...)
	cmd.Dir = tsDir()

	// Capture stderr for debugging.
	stderrLines := make(chan string, 64)
	cmd.Stderr = writerFunc(func(p []byte) (int, error) {
		select {
		case stderrLines <- string(p):
		default:
		}
		return len(p), nil
	})

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		ts.Close()
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		ts.Close()
		cancel()
		t.Fatalf("start node: %v", err)
	}
	nodeExited := make(chan error, 1)
	go func() {
		nodeExited <- cmd.Wait()
	}()

	cleanup := func() {
		close(done)
		cmd.Process.Kill()
		<-nodeExited
		ts.Close()
		cancel()
	}

	// Wait for the Node script to print "READY".
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "READY" {
				close(ready)
				for scanner.Scan() {
				}
				return
			}
		}
	}()

	select {
	case <-ready:
	case exitErr := <-nodeExited:
		drainStderr(t, stderrLines)
		ts.Close()
		cancel()
		t.Fatalf("node process exited before READY (mode=%s streaming=%v): %v",
			opts.mode, opts.streaming, exitErr)
	case <-ctx.Done():
		drainStderr(t, stderrLines)
		cleanup()
		t.Fatalf("timed out waiting for node READY signal (mode=%s streaming=%v)",
			opts.mode, opts.streaming)
	}

	var conn *websocket.Conn
	select {
	case conn = <-connCh:
	case exitErr := <-nodeExited:
		drainStderr(t, stderrLines)
		ts.Close()
		cancel()
		t.Fatalf("node process exited before WS connect: %v", exitErr)
	case <-ctx.Done():
		cleanup()
		t.Fatal("timed out waiting for node WS connection")
	}

	sender := &wsSendable{conn: conn, ctx: ctx}
	var caller *Caller
	if opts.mode == "text" {
		caller = NewCaller(sender, WithTextMode())
	} else {
		caller = NewCaller(sender)
	}

	// Read loop — dispatch to the correct handler based on message type.
	// Both sides are expected to speak a single mode, but branching on the
	// incoming frame type means a mismatched subprocess will surface as
	// "unmarshal error" rather than silent hang.
	go func() {
		for {
			mt, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch mt {
			case websocket.MessageText:
				caller.HandleTextMessage(ctx, string(data))
			default:
				caller.HandleBinaryMessage(ctx, data)
			}
		}
	}()

	doRequest := func(t *testing.T, req *protocol.HTTPRequestPayload) (*bufferedResponse, error) {
		t.Helper()
		type result struct {
			resp *bufferedResponse
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			resp, err := caller.Request(ctx, req)
			if err != nil {
				ch <- result{nil, err}
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				ch <- result{nil, readErr}
				return
			}
			ch <- result{&bufferedResponse{
				StatusCode: resp.StatusCode,
				Headers:    resp.Headers,
				Body:       body,
			}, nil}
		}()
		select {
		case r := <-ch:
			return r.resp, r.err
		case exitErr := <-nodeExited:
			drainStderr(t, stderrLines)
			return nil, fmt.Errorf("node process exited during request: %v", exitErr)
		}
	}

	return &crossLangEnv{caller: caller, doRequest: doRequest, cleanup: cleanup}
}

// runBasicRequestSuite exercises the GET /hello and POST /echo routes.
// Shared by every cross-language configuration so wire-format regressions
// in any mode/streaming combination fail loudly.
func runBasicRequestSuite(t *testing.T, env *crossLangEnv) {
	t.Helper()
	t.Run("GET /hello", func(t *testing.T) {
		resp, err := env.doRequest(t, &protocol.HTTPRequestPayload{
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
		if string(resp.Body) != "hello from node" {
			t.Fatalf("expected body 'hello from node', got %q", string(resp.Body))
		}
	})

	t.Run("POST /echo", func(t *testing.T) {
		resp, err := env.doRequest(t, &protocol.HTTPRequestPayload{
			Method:  "POST",
			URL:     "/echo",
			Headers: map[string][]string{"Content-Type": {"text/plain"}},
			Body:    []byte("cross language test"),
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		if string(resp.Body) != "cross language test" {
			t.Fatalf("expected body 'cross language test', got %q", string(resp.Body))
		}
	})
}

// runStreamRoute verifies that the /stream route returns the expected
// concatenated payload. The handler emits 5 chunks over ~50ms; whether the
// Node adapter was configured with streaming=true (chunks on the wire) or
// streaming=false (single HTTPResponse) is transparent at this level — the
// Caller always exposes resp.Body as a stream, so the assertion is just the
// end-to-end byte sequence.
func runStreamRoute(t *testing.T, env *crossLangEnv) {
	t.Helper()
	t.Run("GET /stream", func(t *testing.T) {
		resp, err := env.doRequest(t, &protocol.HTTPRequestPayload{
			Method:  "GET",
			URL:     "/stream",
			Headers: map[string][]string{},
		})
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		want := "chunk-0\nchunk-1\nchunk-2\nchunk-3\nchunk-4\n"
		if string(resp.Body) != want {
			t.Fatalf("expected body %q, got %q", want, string(resp.Body))
		}
	})
}

// TestCrossLanguage verifies the baseline: Go caller ↔ TS handler in the
// default (binary + buffered) configuration. This is the original coverage
// and must stay green to guarantee the MessagePack wire format has not
// drifted between the two implementations.
func TestCrossLanguage(t *testing.T) {
	env := setupCrossLang(t, crossLangOpts{})
	defer env.cleanup()
	runBasicRequestSuite(t, env)
	runStreamRoute(t, env)
}

// TestCrossLanguageStreaming exercises the streaming HTTPHandlerAdapter on
// the Node side: each res.write() inside the RequestListener becomes one
// HTTPResponseChunk on the wire. The Go caller sees a streaming body that
// reads back as the concatenation of all chunks. Regressions in the
// HTTPResponseStart / Chunk / End envelope shapes (either encoding or
// decoding) fail here rather than in a rarely-run manual test.
func TestCrossLanguageStreaming(t *testing.T) {
	env := setupCrossLang(t, crossLangOpts{streaming: true})
	defer env.cleanup()
	runBasicRequestSuite(t, env)
	runStreamRoute(t, env)
}

// TestCrossLanguageTextMode exercises the JSON envelope path in both
// directions: Go caller encodes HTTPRequest as JSON + text WebSocket frame,
// TS handler decodes and routes, and responses come back as JSON text
// frames. Buffered (default) adapter. Without this test, nothing guarantees
// that Go's RawBody and TS's encodeBinaryFields agree on the "embed valid
// JSON as-is, otherwise stringify" convention.
func TestCrossLanguageTextMode(t *testing.T) {
	env := setupCrossLang(t, crossLangOpts{mode: "text"})
	defer env.cleanup()
	runBasicRequestSuite(t, env)
	runStreamRoute(t, env)
}

// TestCrossLanguageTextModeStreaming is the text-mode × streaming cell of
// the matrix — the least-trafficked combination, which is exactly why it
// needs its own guard. Catches regressions where the streaming-specific
// envelopes (Start / Chunk / End) get text-mode serialization wrong on one
// side but not the other.
func TestCrossLanguageTextModeStreaming(t *testing.T) {
	env := setupCrossLang(t, crossLangOpts{mode: "text", streaming: true})
	defer env.cleanup()
	runBasicRequestSuite(t, env)
	runStreamRoute(t, env)
}

// drainStderr logs all buffered stderr lines for debugging.
func drainStderr(t *testing.T, ch <-chan string) {
	t.Helper()
	for {
		select {
		case line := <-ch:
			t.Logf("[node stderr] %s", line)
		default:
			return
		}
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
