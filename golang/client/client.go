package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/geminiwen/how/golang/protocol"
)

// ErrReadTimeout is returned when no message is received within the read timeout.
var ErrReadTimeout = errors.New("read timeout")

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
	pending     map[string]chan *protocol.Envelope
	textPending map[string]chan *protocol.TextEnvelope
	ReadTimeout time.Duration // 0 means use DefaultReadTimeout; negative means no timeout
	textMode    bool
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
		pending:     make(map[string]chan *protocol.Envelope),
		textPending: make(map[string]chan *protocol.TextEnvelope),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
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

// Request sends an HTTPRequest and waits for the response.
// For streaming responses, it accumulates Start+Chunks+End into a complete HTTPResponsePayload.
func (c *Caller) Request(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	requestID := uuid.New().String()

	if c.textMode {
		return c.requestText(ctx, requestID, req)
	}
	return c.requestBinary(ctx, requestID, req)
}

func (c *Caller) requestBinary(ctx context.Context, requestID string, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	ch := make(chan *protocol.Envelope, 16)
	c.mu.Lock()
	c.pending[requestID] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}()

	env, err := protocol.NewHTTPRequest(requestID, req)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	data, err := protocol.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := c.sender.SendBytes(data); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Wait for response(s)
	var resp *protocol.HTTPResponsePayload
	var bodyChunks [][]byte

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timerC:
			return nil, ErrReadTimeout
		case msg := <-ch:
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
			case protocol.TypeHTTPResponse:
				payload, err := protocol.DecodePayload[protocol.HTTPResponsePayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response: %w", err)
				}
				return payload, nil

			case protocol.TypeHTTPResponseStart:
				start, err := protocol.DecodePayload[protocol.HTTPResponseStartPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response start: %w", err)
				}
				resp = &protocol.HTTPResponsePayload{
					StatusCode: start.StatusCode,
					Headers:    start.Headers,
				}

			case protocol.TypeHTTPResponseChunk:
				chunk, err := protocol.DecodePayload[protocol.HTTPResponseChunkPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response chunk: %w", err)
				}
				bodyChunks = append(bodyChunks, chunk.Data)

			case protocol.TypeHTTPResponseEnd:
				if resp == nil {
					return nil, fmt.Errorf("received end without start")
				}
				totalLen := 0
				for _, c := range bodyChunks {
					totalLen += len(c)
				}
				if totalLen > 0 {
					body := make([]byte, 0, totalLen)
					for _, c := range bodyChunks {
						body = append(body, c...)
					}
					resp.Body = body
				}
				return resp, nil

			case protocol.TypeError:
				errPayload, err := protocol.DecodePayload[protocol.ErrorPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode error: %w", err)
				}
				return nil, fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message)
			}
		}
	}
}

func (c *Caller) requestText(ctx context.Context, requestID string, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	ch := make(chan *protocol.TextEnvelope, 16)
	c.mu.Lock()
	c.textPending[requestID] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.textPending, requestID)
		c.mu.Unlock()
	}()

	env, err := protocol.NewTextHTTPRequest(requestID, req)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	data, err := protocol.MarshalText(env)
	if err != nil {
		return nil, fmt.Errorf("marshal text request: %w", err)
	}

	if err := c.sender.SendText(data); err != nil {
		return nil, fmt.Errorf("send text request: %w", err)
	}

	// Wait for response(s)
	var resp *protocol.HTTPResponsePayload
	var bodyChunks [][]byte

	timeout := c.readTimeout()
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timerC:
			return nil, ErrReadTimeout
		case msg := <-ch:
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
			case protocol.TypeHTTPResponse:
				payload, err := protocol.DecodeTextPayload[protocol.HTTPResponsePayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response: %w", err)
				}
				return payload, nil

			case protocol.TypeHTTPResponseStart:
				start, err := protocol.DecodeTextPayload[protocol.HTTPResponseStartPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response start: %w", err)
				}
				resp = &protocol.HTTPResponsePayload{
					StatusCode: start.StatusCode,
					Headers:    start.Headers,
				}

			case protocol.TypeHTTPResponseChunk:
				chunk, err := protocol.DecodeTextPayload[protocol.HTTPResponseChunkPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode response chunk: %w", err)
				}
				bodyChunks = append(bodyChunks, chunk.Data)

			case protocol.TypeHTTPResponseEnd:
				if resp == nil {
					return nil, fmt.Errorf("received end without start")
				}
				totalLen := 0
				for _, c := range bodyChunks {
					totalLen += len(c)
				}
				if totalLen > 0 {
					body := make([]byte, 0, totalLen)
					for _, c := range bodyChunks {
						body = append(body, c...)
					}
					resp.Body = body
				}
				return resp, nil

			case protocol.TypeError:
				errPayload, err := protocol.DecodeTextPayload[protocol.ErrorPayload](msg)
				if err != nil {
					return nil, fmt.Errorf("decode error: %w", err)
				}
				return nil, fmt.Errorf("remote error (code=%d): %s", errPayload.Code, errPayload.Message)
			}
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
	ch, ok := c.pending[env.RequestID]
	c.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- env:
	default:
		log.Printf("caller: channel full for request %s", env.RequestID)
	}
}

// HandleTextMessage processes an incoming text message and routes it to the pending request.
func (c *Caller) HandleTextMessage(ctx context.Context, data string) {
	env, err := protocol.UnmarshalText(data)
	if err != nil {
		log.Printf("caller text unmarshal error: %v", err)
		return
	}

	c.mu.Lock()
	ch, ok := c.textPending[env.RequestID]
	c.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- env:
	default:
		log.Printf("caller: text channel full for request %s", env.RequestID)
	}
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
				rw.Close()
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

func (h *HOWHandler) sendResponseChunk(requestID string, data []byte) {
	if h.textMode {
		env, err := protocol.NewTextHTTPResponseChunk(requestID, data)
		if err != nil {
			log.Printf("create text response chunk: %v", err)
			return
		}
		h.sendText(env)
	} else {
		env, err := protocol.NewHTTPResponseChunk(requestID, data)
		if err != nil {
			log.Printf("create response chunk: %v", err)
			return
		}
		h.sendBinary(env)
	}
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

func (h *HOWHandler) sendBinary(env *protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		log.Printf("marshal error: %v", err)
		return
	}
	if err := h.sender.SendBytes(data); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (h *HOWHandler) sendText(env *protocol.TextEnvelope) {
	data, err := protocol.MarshalText(env)
	if err != nil {
		log.Printf("marshal text error: %v", err)
		return
	}
	if err := h.sender.SendText(data); err != nil {
		log.Printf("send text error: %v", err)
	}
}
