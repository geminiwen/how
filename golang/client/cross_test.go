package client

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/geminiwen/how/protocol"
)

// tsDir returns the absolute path to the typescript/ directory.
func tsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "typescript")
}

func TestCrossLanguage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Channel to receive the WS connection from the Node client.
	connCh := make(chan *websocket.Conn, 1)
	done := make(chan struct{})

	ts := startWSServer(t, func(srvCtx context.Context, conn *websocket.Conn) {
		connCh <- conn
		// Keep the handler goroutine alive until test ends.
		<-done
		conn.CloseNow()
	})
	defer ts.Close()

	wsURL := "ws" + ts.URL[4:] // http -> ws

	// Launch the Node handler subprocess.
	scriptPath := filepath.Join(tsDir(), "src", "client", "cross_test_handler.ts")
	cmd := exec.CommandContext(ctx, "npx", "tsx", scriptPath, wsURL)
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
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}
	// Monitor Node process exit so we can fail fast with a clear error.
	nodeExited := make(chan error, 1)
	go func() {
		nodeExited <- cmd.Wait()
	}()

	defer func() {
		close(done)
		cmd.Process.Kill()
		// Wait for the monitoring goroutine to finish.
		<-nodeExited
	}()

	// Wait for the Node script to print "READY".
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "READY" {
				close(ready)
				// Keep draining stdout to avoid blocking the subprocess.
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
		t.Fatalf("node process exited before READY: %v", exitErr)
	case <-ctx.Done():
		drainStderr(t, stderrLines)
		t.Fatal("timed out waiting for node READY signal")
	}

	// Wait for the WebSocket connection.
	var conn *websocket.Conn
	select {
	case conn = <-connCh:
	case exitErr := <-nodeExited:
		drainStderr(t, stderrLines)
		t.Fatalf("node process exited before WS connect: %v", exitErr)
	case <-ctx.Done():
		t.Fatal("timed out waiting for node WS connection")
	}

	sender := &wsSendable{conn: conn, ctx: ctx}
	caller := NewCaller(sender)

	// Read loop in background.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			caller.HandleMessage(ctx, data)
		}
	}()

	// Helper to make a request with node crash detection.
	doRequest := func(t *testing.T, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
		t.Helper()
		type result struct {
			resp *protocol.HTTPResponsePayload
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			resp, err := caller.Request(ctx, req)
			ch <- result{resp, err}
		}()
		select {
		case r := <-ch:
			return r.resp, r.err
		case exitErr := <-nodeExited:
			drainStderr(t, stderrLines)
			return nil, fmt.Errorf("node process exited during request: %v", exitErr)
		}
	}

	t.Run("GET /hello", func(t *testing.T) {
		resp, err := doRequest(t, &protocol.HTTPRequestPayload{
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
		resp, err := doRequest(t, &protocol.HTTPRequestPayload{
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
