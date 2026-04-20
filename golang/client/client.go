package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/geminiwen/how/golang/protocol"
)

// ErrReadTimeout is returned when no message is received within the read timeout.
var ErrReadTimeout = errors.New("read timeout")

// ErrTransportClosed is the default error returned from pending / future Requests
// after Caller.Close is called without a specific error.
var ErrTransportClosed = errors.New("transport closed")

// frameQueue is an unbounded FIFO used to decouple the single transport read
// loop from per-request consumers. push is O(1) and never blocks, so one slow
// (or abandoned) streaming body cannot stall the read loop and introduce
// head-of-line blocking across unrelated requests multiplexed on the same
// connection. Consumers pull frames via wait(), which blocks until a frame
// is available or one of the supplied stop channels fires.
type frameQueue[T any] struct {
	mu    sync.Mutex
	queue []T
	sig   chan struct{} // buffered(1); serves as a "maybe something arrived" signal
}

func newFrameQueue[T any]() *frameQueue[T] {
	return &frameQueue[T]{sig: make(chan struct{}, 1)}
}

func (q *frameQueue[T]) push(env T) {
	q.mu.Lock()
	q.queue = append(q.queue, env)
	q.mu.Unlock()
	select {
	case q.sig <- struct{}{}:
	default:
	}
}

func (q *frameQueue[T]) tryPop() (T, bool) {
	var zero T
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queue) == 0 {
		return zero, false
	}
	env := q.queue[0]
	q.queue[0] = zero // help GC
	q.queue = q.queue[1:]
	return env, true
}

// wait blocks until a frame is available or one of the stop channels fires.
// On stop, returns (_, false) and the caller should inspect the stop channels
// itself to determine the reason (done / ctx / timer).
//
// When a stop channel fires, wait performs one final tryPop before returning
// false. This closes a race: Go's select is random when multiple branches are
// ready, so push → sig-send → done-close all happening back-to-back could
// otherwise return (zero, false) and orphan a frame that was already in the
// queue. Any frame enqueued before the stop-check returns must still be
// delivered to the consumer.
func (q *frameQueue[T]) wait(done, ctxDone <-chan struct{}, timerC <-chan time.Time) (T, bool) {
	var zero T
	for {
		if env, ok := q.tryPop(); ok {
			return env, true
		}
		select {
		case <-q.sig:
			// loop — there may or may not be a frame now; tryPop decides.
		case <-done:
			if env, ok := q.tryPop(); ok {
				return env, true
			}
			return zero, false
		case <-ctxDone:
			if env, ok := q.tryPop(); ok {
				return env, true
			}
			return zero, false
		case <-timerC:
			if env, ok := q.tryPop(); ok {
				return env, true
			}
			return zero, false
		}
	}
}

// Sendable abstracts the ability to send data over a transport.
type Sendable interface {
	SendBytes(data []byte) error
	SendText(data string) error
}

// ─── Caller ───

// DefaultReadTimeout is the default timeout for waiting for a response message.
// Each received message (including HTTPResponseChunk) resets the timer.
const DefaultReadTimeout = 30 * time.Second

// Caller sends HTTPRequests and waits for responses.
type Caller struct {
	sender      Sendable
	mu          sync.Mutex
	pending     map[string]*frameQueue[*protocol.Envelope]
	textPending map[string]*frameQueue[*protocol.TextEnvelope]
	ReadTimeout time.Duration // 0 means use DefaultReadTimeout; negative means no timeout
	textMode    bool
	done        chan struct{}
	closeOnce   sync.Once
	closeErr    error
}

// CallerOption configures a Caller.
type CallerOption func(*Caller)

// WithTextMode configures the Caller to use text (JSON) mode instead of binary (MessagePack) mode.
func WithTextMode() CallerOption {
	return func(c *Caller) {
		c.textMode = true
	}
}

// NewCaller creates a new Caller.
func NewCaller(sender Sendable, opts ...CallerOption) *Caller {
	c := &Caller{
		sender:      sender,
		pending:     make(map[string]*frameQueue[*protocol.Envelope]),
		textPending: make(map[string]*frameQueue[*protocol.TextEnvelope]),
		done:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Close signals the caller that the underlying transport has died. All pending
// Request calls return err (or ErrTransportClosed if err is nil); further
// Request calls return the same error immediately. Idempotent.
func (c *Caller) Close(err error) {
	c.closeOnce.Do(func() {
		if err == nil {
			err = ErrTransportClosed
		}
		c.mu.Lock()
		c.closeErr = err
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Caller) readTimeout() time.Duration {
	if c.ReadTimeout < 0 {
		return 0 // no timeout
	}
	if c.ReadTimeout == 0 {
		return DefaultReadTimeout
	}
	return c.ReadTimeout
}

// cancellingBody wraps a pipe reader with a context cancel, so that calling
// Close() while the pump is idle (waiting for the next chunk over the
// transport) unblocks the pump through ctx.Done() and lets it remove the
// pending entry. Without this, an early Body.Close() would only be noticed
// when the next chunk arrives (or on timeout / Caller.Close), leaking the
// pending slot indefinitely for quiet streams.
//
// A finalizer is attached as a safety net: if the caller drops the Response
// without calling Body.Close(), the pump would otherwise keep draining chunks
// into an unbounded queue and the pending slot would survive until
// Caller.Close or the parent ctx fires. The finalizer logs a loud warning
// (the call site is a bug) and runs the same cleanup as Close.
type cancellingBody struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
}

func newCancellingBody(pr *io.PipeReader, cancel context.CancelFunc) *cancellingBody {
	b := &cancellingBody{pr: pr, cancel: cancel}
	runtime.SetFinalizer(b, func(b *cancellingBody) {
		log.Printf("how: Response.Body was garbage-collected without Close(); leaked a pending request. Callers MUST Close() the body.")
		b.cancel()
		b.pr.Close()
	})
	return b
}

func (b *cancellingBody) Read(p []byte) (int, error) { return b.pr.Read(p) }

func (b *cancellingBody) Close() error {
	// Disarm the finalizer so it doesn't run (and log a spurious warning)
	// once this body becomes unreachable after a proper Close.
	runtime.SetFinalizer(b, nil)
	b.cancel()
	return b.pr.Close()
}

// Response is returned by Caller.Request. Body yields the response bytes as
// they arrive over HOW — each HTTPResponseChunk from the peer becomes readable
// on Body, so downstream consumers can forward chunks in real time instead of
// waiting for the whole body to buffer. When the peer responds with a single
// HTTPResponse (non-streaming handler), Body yields that full body and then
// EOF. Callers MUST Close Body when done, whether or not they read to EOF.
type Response struct {
	StatusCode uint16
	Headers    map[string][]string
	Body       io.ReadCloser
}

// Request sends an HTTPRequest and returns a streaming response. The shape of
// Body depends on what the peer sends:
//
//   - HTTPResponse (non-streaming peer): Body yields the full buffered body,
//     then EOF.
//   - HTTPResponseStart + Chunks + End (streaming peer): Body yields each
//     chunk incrementally; EOF on HTTPResponseEnd. If the transport dies
//     (Caller.Close or ctx cancellation) mid-stream, Body.Read returns a
//     non-nil error.
//
// Request never accumulates chunks in memory — the caller controls flow by how
// fast it reads Body. For the common "I just want the full body" case, use
// `io.ReadAll(resp.Body)`.
func (c *Caller) Request(ctx context.Context, req *protocol.HTTPRequestPayload) (*Response, error) {
	select {
	case <-c.done:
		return nil, c.closeErr
	default:
	}

	requestID := uuid.New().String()
	if c.textMode {
		return c.requestText(ctx, requestID, req)
	}
	return c.requestBinary(ctx, requestID, req)
}

func (c *Caller) requestBinary(ctx context.Context, requestID string, req *protocol.HTTPRequestPayload) (*Response, error) {
	q := newFrameQueue[*protocol.Envelope]()
	c.mu.Lock()
	c.pending[requestID] = q
	c.mu.Unlock()

	removePending := func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}

	env, err := protocol.NewHTTPRequest(requestID, req)
	if err != nil {
		removePending()
		return nil, fmt.Errorf("create request: %w", err)
	}
	data, err := protocol.Marshal(env)
	if err != nil {
		removePending()
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := c.sender.SendBytes(data); err != nil {
		removePending()
		return nil, fmt.Errorf("send request: %w", err)
	}

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timerC = timer.C
	}

	first, ok := q.wait(c.done, ctx.Done(), timerC)
	if timer != nil {
		timer.Stop()
	}
	if !ok {
		removePending()
		select {
		case <-c.done:
			return nil, c.closeErr
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, ErrReadTimeout
		}
	}

	switch first.Type {
	case protocol.TypeHTTPResponse:
		payload, err := protocol.DecodePayload[protocol.HTTPResponsePayload](first)
		removePending()
		if err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return &Response{
			StatusCode: payload.StatusCode,
			Headers:    payload.Headers,
			Body:       io.NopCloser(bytes.NewReader(payload.Body)),
		}, nil

	case protocol.TypeHTTPResponseStart:
		start, err := protocol.DecodePayload[protocol.HTTPResponseStartPayload](first)
		if err != nil {
			removePending()
			return nil, fmt.Errorf("decode response start: %w", err)
		}
		pumpCtx, cancel := context.WithCancel(ctx)
		pr, pw := io.Pipe()
		go c.pumpBinary(pumpCtx, q, pw, removePending)
		return &Response{
			StatusCode: start.StatusCode,
			Headers:    start.Headers,
			Body:       newCancellingBody(pr, cancel),
		}, nil

	case protocol.TypeError:
		errPayload, decErr := protocol.DecodePayload[protocol.ErrorPayload](first)
		removePending()
		if decErr != nil {
			return nil, fmt.Errorf("decode error: %w", decErr)
		}
		return nil, fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message)

	default:
		removePending()
		return nil, fmt.Errorf("unexpected envelope type 0x%02x", first.Type)
	}
}

// Note on timer reset window: Request() above creates its own timer for the
// first wait (Start / HTTPResponse / Error), and the pump below creates a
// fresh timer on entry. So the combined worst case between "HTTPResponseStart
// arrived" and "the peer sends the first Chunk" is up to 2 × readTimeout (one
// full period each side of the handoff). This is usually invisible because
// real peers send Start and the first Chunk close together, but operators
// tuning readTimeout to a tight value should be aware of it.
func (c *Caller) pumpBinary(ctx context.Context, q *frameQueue[*protocol.Envelope], pw *io.PipeWriter, removePending func()) {
	defer removePending()

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		msg, ok := q.wait(c.done, ctx.Done(), timerC)
		if !ok {
			select {
			case <-c.done:
				pw.CloseWithError(c.closeErr)
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
			default:
				pw.CloseWithError(ErrReadTimeout)
			}
			return
		}
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		}
		switch msg.Type {
		case protocol.TypeHTTPResponseChunk:
			chunk, err := protocol.DecodePayload[protocol.HTTPResponseChunkPayload](msg)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("decode chunk: %w", err))
				return
			}
			if _, err := pw.Write(chunk.Data); err != nil {
				return // reader closed the pipe — stop pumping
			}
		case protocol.TypeHTTPResponseEnd:
			pw.Close()
			return
		case protocol.TypeError:
			errPayload, decErr := protocol.DecodePayload[protocol.ErrorPayload](msg)
			if decErr != nil || errPayload == nil {
				pw.CloseWithError(fmt.Errorf("malformed error envelope: %w", decErr))
				return
			}
			pw.CloseWithError(fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message))
			return
		}
	}
}

func (c *Caller) requestText(ctx context.Context, requestID string, req *protocol.HTTPRequestPayload) (*Response, error) {
	q := newFrameQueue[*protocol.TextEnvelope]()
	c.mu.Lock()
	c.textPending[requestID] = q
	c.mu.Unlock()

	removePending := func() {
		c.mu.Lock()
		delete(c.textPending, requestID)
		c.mu.Unlock()
	}

	env, err := protocol.NewTextHTTPRequest(requestID, req)
	if err != nil {
		removePending()
		return nil, fmt.Errorf("create request: %w", err)
	}
	data, err := protocol.MarshalText(env)
	if err != nil {
		removePending()
		return nil, fmt.Errorf("marshal text request: %w", err)
	}
	if err := c.sender.SendText(data); err != nil {
		removePending()
		return nil, fmt.Errorf("send text request: %w", err)
	}

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timerC = timer.C
	}

	first, ok := q.wait(c.done, ctx.Done(), timerC)
	if timer != nil {
		timer.Stop()
	}
	if !ok {
		removePending()
		select {
		case <-c.done:
			return nil, c.closeErr
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, ErrReadTimeout
		}
	}

	switch first.Type {
	case protocol.TypeHTTPResponse:
		payload, err := protocol.DecodeTextPayload[protocol.HTTPResponsePayload](first)
		removePending()
		if err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return &Response{
			StatusCode: payload.StatusCode,
			Headers:    payload.Headers,
			Body:       io.NopCloser(bytes.NewReader(payload.Body)),
		}, nil

	case protocol.TypeHTTPResponseStart:
		start, err := protocol.DecodeTextPayload[protocol.HTTPResponseStartPayload](first)
		if err != nil {
			removePending()
			return nil, fmt.Errorf("decode response start: %w", err)
		}
		pumpCtx, cancel := context.WithCancel(ctx)
		pr, pw := io.Pipe()
		go c.pumpText(pumpCtx, q, pw, removePending)
		return &Response{
			StatusCode: start.StatusCode,
			Headers:    start.Headers,
			Body:       newCancellingBody(pr, cancel),
		}, nil

	case protocol.TypeError:
		errPayload, decErr := protocol.DecodeTextPayload[protocol.ErrorPayload](first)
		removePending()
		if decErr != nil {
			return nil, fmt.Errorf("decode error: %w", decErr)
		}
		return nil, fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message)

	default:
		removePending()
		return nil, fmt.Errorf("unexpected envelope type 0x%02x", first.Type)
	}
}

func (c *Caller) pumpText(ctx context.Context, q *frameQueue[*protocol.TextEnvelope], pw *io.PipeWriter, removePending func()) {
	defer removePending()

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		msg, ok := q.wait(c.done, ctx.Done(), timerC)
		if !ok {
			select {
			case <-c.done:
				pw.CloseWithError(c.closeErr)
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
			default:
				pw.CloseWithError(ErrReadTimeout)
			}
			return
		}
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		}
		switch msg.Type {
		case protocol.TypeHTTPResponseChunk:
			chunk, err := protocol.DecodeTextPayload[protocol.HTTPResponseChunkPayload](msg)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("decode chunk: %w", err))
				return
			}
			if _, err := pw.Write(chunk.Data); err != nil {
				return
			}
		case protocol.TypeHTTPResponseEnd:
			pw.Close()
			return
		case protocol.TypeError:
			errPayload, decErr := protocol.DecodeTextPayload[protocol.ErrorPayload](msg)
			if decErr != nil || errPayload == nil {
				pw.CloseWithError(fmt.Errorf("malformed error envelope: %w", decErr))
				return
			}
			pw.CloseWithError(fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message))
			return
		}
	}
}

// HandleBinaryMessage processes an incoming binary message and routes it to the pending request.
func (c *Caller) HandleBinaryMessage(ctx context.Context, data []byte) {
	env, err := protocol.Unmarshal(data)
	if err != nil {
		log.Printf("caller unmarshal error: %v", err)
		return
	}

	c.mu.Lock()
	q, ok := c.pending[env.RequestID]
	c.mu.Unlock()

	if !ok {
		return
	}

	// Non-blocking, unbounded push: the transport read loop never stalls behind
	// a slow or abandoned body reader. Memory grows with undrained frames of
	// the affected request only, so head-of-line blocking across concurrent
	// requests multiplexed on the same connection is avoided.
	q.push(env)
}

// HandleTextMessage processes an incoming text message and routes it to the pending request.
func (c *Caller) HandleTextMessage(ctx context.Context, data string) {
	env, err := protocol.UnmarshalText(data)
	if err != nil {
		log.Printf("caller text unmarshal error: %v", err)
		return
	}

	c.mu.Lock()
	q, ok := c.textPending[env.RequestID]
	c.mu.Unlock()

	if !ok {
		return
	}

	// See HandleBinaryMessage for rationale.
	q.push(env)
}

// ─── Handler ───

// HandlerOption configures a HOWHandler.
type HandlerOption func(*HOWHandler)

// WithHandlerTextMode configures the Handler to use text (JSON) mode.
func WithHandlerTextMode() HandlerOption {
	return func(h *HOWHandler) {
		h.textMode = true
	}
}

// HOWHandler receives HTTPRequest messages and dispatches them to a Handler.
type HOWHandler struct {
	handler  Handler
	sender   Sendable
	textMode bool
}

// NewHandler creates a new HOWHandler.
func NewHandler(handler Handler, sender Sendable, opts ...HandlerOption) *HOWHandler {
	h := &HOWHandler{
		handler: handler,
		sender:  sender,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HandleBinaryMessage processes an incoming binary message.
func (h *HOWHandler) HandleBinaryMessage(ctx context.Context, data []byte) {
	env, err := protocol.Unmarshal(data)
	if err != nil {
		log.Printf("handler unmarshal error: %v", err)
		return
	}

	switch env.Type {
	case protocol.TypeHTTPRequest:
		reqPayload, err := protocol.DecodePayload[protocol.HTTPRequestPayload](env)
		if err != nil {
			log.Printf("decode request: %v", err)
			errEnv, _ := protocol.NewError(env.RequestID, protocol.ErrInvalidMessage, err.Error())
			h.sendBinary(errEnv)
			return
		}
		go h.dispatchRequest(ctx, env.RequestID, reqPayload)
	default:
		log.Printf("handler: unexpected message type 0x%02x", env.Type)
	}
}

// HandleTextMessage processes an incoming text message.
func (h *HOWHandler) HandleTextMessage(ctx context.Context, data string) {
	tenv, err := protocol.UnmarshalText(data)
	if err != nil {
		log.Printf("handler text unmarshal error: %v", err)
		return
	}

	switch tenv.Type {
	case protocol.TypeHTTPRequest:
		reqPayload, err := protocol.DecodeTextPayload[protocol.HTTPRequestPayload](tenv)
		if err != nil {
			log.Printf("decode text request: %v", err)
			errEnv, _ := protocol.NewTextError(tenv.RequestID, protocol.ErrInvalidMessage, err.Error())
			h.sendText(errEnv)
			return
		}
		go h.dispatchRequest(ctx, tenv.RequestID, reqPayload)
	default:
		log.Printf("handler: unexpected text message type 0x%02x", tenv.Type)
	}
}

// dispatchRequest handles the actual request processing, mode-agnostic.
// It uses h.sendResponse/h.sendResponseStart/h.sendResponseChunk/h.sendResponseEnd
// which internally pick binary or text based on h.textMode.
func (h *HOWHandler) dispatchRequest(ctx context.Context, requestID string, reqPayload *protocol.HTTPRequestPayload) {
	// If handler implements StreamingHandler, use streaming path
	if sh, ok := h.handler.(StreamingHandler); ok {
		rw := newResponseWriter(requestID, h)
		if err := sh.ServeHTTPOverWSStream(ctx, reqPayload, rw); err != nil {
			log.Printf("streaming handler error: %v", err)
			if !rw.started {
				h.sendResponse(requestID, &protocol.HTTPResponsePayload{
					StatusCode: 502,
					Headers:    map[string][]string{"Content-Type": {"text/plain"}},
					Body:       []byte(err.Error()),
				})
			} else {
				// Headers already on the wire — use Error, not End, so the
				// peer caller surfaces the failure through Body.Read instead
				// of seeing a clean EOF on a truncated stream.
				h.sendErrorEnvelope(requestID, protocol.ErrInternal, err.Error())
			}
		}
		return
	}

	// Non-streaming path
	resp, err := h.handler.ServeHTTPOverWS(ctx, reqPayload)
	if err != nil {
		log.Printf("handler error: %v", err)
		resp = &protocol.HTTPResponsePayload{
			StatusCode: 502,
			Headers:    map[string][]string{"Content-Type": {"text/plain"}},
			Body:       []byte(err.Error()),
		}
	}

	h.sendResponse(requestID, resp)
}

func (h *HOWHandler) sendResponse(requestID string, resp *protocol.HTTPResponsePayload) {
	if h.textMode {
		env, err := protocol.NewTextHTTPResponse(requestID, resp)
		if err != nil {
			log.Printf("create text response: %v", err)
			return
		}
		h.sendText(env)
	} else {
		env, err := protocol.NewHTTPResponse(requestID, resp)
		if err != nil {
			log.Printf("create response: %v", err)
			return
		}
		h.sendBinary(env)
	}
}

func (h *HOWHandler) sendResponseStart(requestID string, statusCode uint16, headers map[string][]string) {
	if h.textMode {
		env, err := protocol.NewTextHTTPResponseStart(requestID, statusCode, headers)
		if err != nil {
			log.Printf("create text response start: %v", err)
			return
		}
		h.sendText(env)
	} else {
		env, err := protocol.NewHTTPResponseStart(requestID, statusCode, headers)
		if err != nil {
			log.Printf("create response start: %v", err)
			return
		}
		h.sendBinary(env)
	}
}

func (h *HOWHandler) sendResponseChunk(requestID string, data []byte) error {
	if h.textMode {
		env, err := protocol.NewTextHTTPResponseChunk(requestID, data)
		if err != nil {
			log.Printf("create text response chunk: %v", err)
			return err
		}
		return h.sendText(env)
	}
	env, err := protocol.NewHTTPResponseChunk(requestID, data)
	if err != nil {
		log.Printf("create response chunk: %v", err)
		return err
	}
	return h.sendBinary(env)
}

// sendErrorEnvelope sends a protocol Error addressed to the given requestID.
// Used when a streaming handler fails after headers have been flushed — the
// peer caller routes this to its body stream's error hook instead of treating
// HTTPResponseEnd as a successful EOF.
func (h *HOWHandler) sendErrorEnvelope(requestID string, code uint16, message string) {
	if h.textMode {
		env, err := protocol.NewTextError(requestID, code, message)
		if err != nil {
			log.Printf("create text error envelope: %v", err)
			return
		}
		h.sendText(env)
		return
	}
	env, err := protocol.NewError(requestID, code, message)
	if err != nil {
		log.Printf("create error envelope: %v", err)
		return
	}
	h.sendBinary(env)
}

func (h *HOWHandler) sendResponseEnd(requestID string) {
	if h.textMode {
		env := protocol.NewTextHTTPResponseEnd(requestID)
		h.sendText(env)
	} else {
		env := protocol.NewHTTPResponseEnd(requestID)
		h.sendBinary(env)
	}
}

func (h *HOWHandler) sendBinary(env *protocol.Envelope) error {
	data, err := protocol.Marshal(env)
	if err != nil {
		log.Printf("marshal error: %v", err)
		return err
	}
	if err := h.sender.SendBytes(data); err != nil {
		log.Printf("send error: %v", err)
		return err
	}
	return nil
}

func (h *HOWHandler) sendText(env *protocol.TextEnvelope) error {
	data, err := protocol.MarshalText(env)
	if err != nil {
		log.Printf("marshal text error: %v", err)
		return err
	}
	if err := h.sender.SendText(data); err != nil {
		log.Printf("send text error: %v", err)
		return err
	}
	return nil
}
