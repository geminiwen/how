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

// Write sends a chunk of response body (HTTPResponseChunk). Returns an error
// if the underlying transport rejected the send (e.g. the peer ws is already
// closed). Streaming handlers should check this error and stop writing —
// continuing past a send failure just wastes CPU and fills no buffers.
func (rw *ResponseWriter) Write(data []byte) error {
	return rw.handler.sendResponseChunk(rw.requestID, data)
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
			if err := w.Write(chunk); err != nil {
				// Peer is gone; stop pumping. Return the err so
				// dispatchRequest sends an Error envelope — though the peer
				// likely won't receive it either, it keeps the adapter
				// honest instead of claiming a successful End.
				return err
			}
		}
		if readErr != nil {
			break
		}
	}

	w.Close()
	return nil
}

// HTTPHandlerAdapter wraps a standard http.Handler as a HOW Handler.
// By default it buffers the whole response and returns it as a single
// HTTPResponse envelope. Use WithStreaming to opt in to per-Write chunking
// (HTTPResponseStart + HTTPResponseChunk* + HTTPResponseEnd), which the peer
// Caller then exposes as a streaming body.
type HTTPHandlerAdapter struct {
	Handler http.Handler
}

// streamingHTTPHandlerAdapter wraps HTTPHandlerAdapter and adds
// ServeHTTPOverWSStream. It is only returned when WithStreaming is passed to
// HTTPHandler; that way plain HTTPHandlerAdapter stays a non-StreamingHandler
// and HOW keeps the historic buffered behavior by default.
type streamingHTTPHandlerAdapter struct {
	*HTTPHandlerAdapter
}

// HTTPHandlerOption configures an HTTPHandler.
type HTTPHandlerOption func(*httpHandlerConfig)

type httpHandlerConfig struct {
	streaming bool
}

// WithStreaming makes HTTPHandler emit one HTTPResponseChunk per http.ResponseWriter.Write,
// instead of buffering the whole response body into a single HTTPResponse. Callers on the
// other end will see resp.Body as a stream of chunks rather than a complete buffer.
func WithStreaming() HTTPHandlerOption {
	return func(c *httpHandlerConfig) {
		c.streaming = true
	}
}

// HTTPHandler creates a Handler that delegates to an http.Handler.
// Default: buffered (single HTTPResponse). Pass WithStreaming() for streaming.
func HTTPHandler(handler http.Handler, opts ...HTTPHandlerOption) Handler {
	cfg := &httpHandlerConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	base := &HTTPHandlerAdapter{Handler: handler}
	if cfg.streaming {
		return &streamingHTTPHandlerAdapter{HTTPHandlerAdapter: base}
	}
	return base
}

func (a *HTTPHandlerAdapter) ServeHTTPOverWS(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
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

// streamingResponseWriter adapts a HOW ResponseWriter to http.ResponseWriter.
// Each Write is forwarded as an individual HTTPResponseChunk, so an http.Handler
// that calls w.Write (with or without Flush) produces a true streaming response
// over HOW.
type streamingResponseWriter struct {
	rw      *ResponseWriter
	hdr     http.Header
	started bool
}

func (s *streamingResponseWriter) Header() http.Header {
	if s.hdr == nil {
		s.hdr = make(http.Header)
	}
	return s.hdr
}

func (s *streamingResponseWriter) WriteHeader(statusCode int) {
	if s.started {
		return
	}
	s.started = true
	headers := make(map[string][]string, len(s.hdr))
	for k, v := range s.hdr {
		vs := make([]string, len(v))
		copy(vs, v)
		headers[k] = vs
	}
	s.rw.WriteHeader(uint16(statusCode), headers)
}

func (s *streamingResponseWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.WriteHeader(http.StatusOK)
	}
	if len(p) == 0 {
		return 0, nil
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	if err := s.rw.Write(chunk); err != nil {
		// Surface the send failure back to the wrapped http.Handler so it
		// can stop writing. Returning 0 (per io.Writer contract: "Write
		// must return a non-nil error if it returns n < len(p)") lets the
		// handler observe the problem the same way a closed TCP socket
		// would: subsequent calls to fmt.Fprintf / w.Write stop working.
		return 0, err
	}
	return len(p), nil
}

// Flush commits the response headers if they have not been sent yet. This
// mirrors net/http's behavior where Flush implicitly triggers WriteHeader(200),
// which is important for SSE handlers that Flush after setting headers but
// before their first Write — otherwise the peer caller would block on
// HTTPResponseStart until the first body byte arrives. Subsequent chunking is
// already per-Write, so there is nothing else to flush.
func (s *streamingResponseWriter) Flush() {
	if !s.started {
		s.WriteHeader(http.StatusOK)
	}
}

// ServeHTTPOverWSStream implements StreamingHandler. Every w.Write inside the
// wrapped http.Handler becomes a separate HTTPResponseChunk on the wire.
// Only the WithStreaming variant (streamingHTTPHandlerAdapter) exposes this
// method; the plain HTTPHandlerAdapter deliberately does not, so that
// NewHandler's type assertion (`h.handler.(StreamingHandler)`) falls through
// to the buffered path.
//
// The HOW request context is forwarded to the wrapped *http.Request, so long-
// lived handlers (e.g. SSE) that watch r.Context().Done() can stop work when
// the HOW caller cancels or disconnects.
func (a *streamingHTTPHandlerAdapter) ServeHTTPOverWSStream(ctx context.Context, req *protocol.HTTPRequestPayload, w *ResponseWriter) error {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	for key, values := range req.Headers {
		for _, v := range values {
			httpReq.Header.Add(key, v)
		}
	}

	srw := &streamingResponseWriter{rw: w}
	a.Handler.ServeHTTP(srw, httpReq)

	if !srw.started {
		srw.WriteHeader(http.StatusOK)
	}

	// Always emit a clean End after the handler returns. We deliberately do
	// NOT try to translate ctx cancellation into an Error envelope here:
	//
	// The adapter has no reliable way to tell whether a handler exited
	// because it observed r.Context().Done() (truncated) or simply because
	// it finished its response and returned (complete). Using `ctx.Err() !=
	// nil` as a proxy creates false positives on complete responses whenever
	// ctx happens to fire concurrently. Handlers that need to signal a mid-
	// stream failure should implement `StreamingHandler` directly and return
	// an error from `ServeHTTPOverWSStream` — `dispatchRequest` then sends a
	// proper Error envelope to the peer caller.
	w.Close()
	return nil
}
