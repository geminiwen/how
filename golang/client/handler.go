package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/geminiwen/how/golang/protocol"
)

// Handler processes an HTTP request and returns a complete response (non-streaming).
type Handler interface {
	ServeHTTPOverWS(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error)
}

// StreamingHandler processes an HTTP request and writes a streaming response.
// If a handler implements StreamingHandler, it takes precedence over Handler.
type StreamingHandler interface {
	ServeHTTPOverWSStream(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error
}

// HandlerFunc adapts an ordinary function to the Handler interface.
type HandlerFunc func(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error)

func (f HandlerFunc) ServeHTTPOverWS(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	return f(ctx, req)
}

// ResponseWriter allows handlers to write streaming responses.
type ResponseWriter struct {
	requestID string
	handler   *HOWHandler
	started   bool
}

func newResponseWriter(requestID string, handler *HOWHandler) *ResponseWriter {
	return &ResponseWriter{requestID: requestID, handler: handler}
}

// WriteHeader sends the status code and headers (HTTPResponseStart).
// Must be called exactly once before any Write calls.
func (rw *ResponseWriter) WriteHeader(statusCode uint16, headers map[string][]string) {
	if rw.started {
		return
	}
	rw.started = true
	rw.handler.sendResponseStart(rw.requestID, statusCode, headers)
}

// Write sends a chunk of response body (HTTPResponseChunk).
func (rw *ResponseWriter) Write(data []byte) {
	rw.handler.sendResponseChunk(rw.requestID, data)
}

// Close signals the end of the streaming response (HTTPResponseEnd).
func (rw *ResponseWriter) Close() {
	rw.handler.sendResponseEnd(rw.requestID)
}

// ForwardHandler forwards requests to a local HTTP service.
type ForwardHandler struct {
	Target *url.URL
	Client *http.Client
}

// ForwardTo creates a Handler that forwards requests to the given target URL.
func ForwardTo(target string) (Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse target URL: %w", err)
	}
	return &ForwardHandler{Target: u, Client: http.DefaultClient}, nil
}

func (h *ForwardHandler) ServeHTTPOverWS(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	targetURL := h.Target.String() + req.URL

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}

	resp, err := h.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("forward request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	headers := make(map[string][]string)
	for key, values := range resp.Header {
		headers[key] = values
	}

	return &protocol.HTTPResponsePayload{
		StatusCode: uint16(resp.StatusCode),
		Headers:    headers,
		Body:       body,
	}, nil
}

// ServeHTTPOverWSStream implements StreamingHandler for ForwardHandler.
func (h *ForwardHandler) ServeHTTPOverWSStream(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error {
	targetURL := h.Target.String() + req.URL

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}

	resp, err := h.Client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("forward request: %w", err)
	}
	defer resp.Body.Close()

	headers := make(map[string][]string)
	for key, values := range resp.Header {
		headers[key] = values
	}

	w.WriteHeader(uint16(resp.StatusCode), headers)

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			w.Write(chunk)
		}
		if readErr != nil {
			break
		}
	}

	w.Close()
	return nil
}

// HTTPHandlerAdapter wraps a standard http.Handler as a HOW Handler.
type HTTPHandlerAdapter struct {
	Handler http.Handler
}

// HTTPHandler creates a Handler that delegates to an http.Handler.
func HTTPHandler(handler http.Handler) Handler {
	return &HTTPHandlerAdapter{Handler: handler}
}

func (a *HTTPHandlerAdapter) ServeHTTPOverWS(_ context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	httpReq, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}

	recorder := httptest.NewRecorder()
	a.Handler.ServeHTTP(recorder, httpReq)

	result := recorder.Result()
	body, _ := io.ReadAll(result.Body)

	headers := make(map[string][]string)
	for key, values := range result.Header {
		headers[key] = values
	}

	return &protocol.HTTPResponsePayload{
		StatusCode: uint16(result.StatusCode),
		Headers:    headers,
		Body:       body,
	}, nil
}
