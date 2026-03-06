package client

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"

	"github.com/geminiwen/how/protocol"
)

// Sendable abstracts the ability to send binary data over a transport.
type Sendable interface {
	Send(data []byte) error
}

// ─── Caller ───

// Caller sends HTTPRequests and waits for responses.
type Caller struct {
	sender  Sendable
	mu      sync.Mutex
	pending map[string]chan *protocol.Envelope
}

// NewCaller creates a new Caller.
func NewCaller(sender Sendable) *Caller {
	return &Caller{
		sender:  sender,
		pending: make(map[string]chan *protocol.Envelope),
	}
}

// Request sends an HTTPRequest and waits for the response.
// For streaming responses, it accumulates Start+Chunks+End into a complete HTTPResponsePayload.
func (c *Caller) Request(ctx context.Context, req *protocol.HTTPRequestPayload) (*protocol.HTTPResponsePayload, error) {
	requestID := uuid.New().String()

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

	if err := c.sender.Send(data); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Wait for response(s)
	var resp *protocol.HTTPResponsePayload
	var bodyChunks [][]byte

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg := <-ch:
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

// HandleMessage processes an incoming message and routes it to the pending request.
func (c *Caller) HandleMessage(ctx context.Context, data []byte) {
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

// ─── Handler ───

// HOWSHandler receives HTTPRequest messages and dispatches them to a Handler.
type HOWSHandler struct {
	handler Handler
	sender  Sendable
}

// NewHandler creates a new HOWSHandler.
func NewHandler(handler Handler, sender Sendable) *HOWSHandler {
	return &HOWSHandler{
		handler: handler,
		sender:  sender,
	}
}

// HandleMessage processes an incoming message.
func (h *HOWSHandler) HandleMessage(ctx context.Context, data []byte) {
	env, err := protocol.Unmarshal(data)
	if err != nil {
		log.Printf("handler unmarshal error: %v", err)
		return
	}

	switch env.Type {
	case protocol.TypeHTTPRequest:
		go h.handleRequest(ctx, env)
	default:
		log.Printf("handler: unexpected message type 0x%02x", env.Type)
	}
}

func (h *HOWSHandler) handleRequest(ctx context.Context, env *protocol.Envelope) {
	reqPayload, err := protocol.DecodePayload[protocol.HTTPRequestPayload](env)
	if err != nil {
		log.Printf("decode request: %v", err)
		errEnv, _ := protocol.NewError(env.RequestID, protocol.ErrInvalidMessage, err.Error())
		h.sendEnv(errEnv)
		return
	}

	// If handler implements StreamingHandler, use streaming path
	if sh, ok := h.handler.(StreamingHandler); ok {
		rw := &ResponseWriter{
			requestID: env.RequestID,
			sendFn:    h.sendEnv,
		}
		if err := sh.ServeHTTPOverWSStream(ctx, reqPayload, rw); err != nil {
			log.Printf("streaming handler error: %v", err)
			if !rw.started {
				resp := &protocol.HTTPResponsePayload{
					StatusCode: 502,
					Headers:    map[string][]string{"Content-Type": {"text/plain"}},
					Body:       []byte(err.Error()),
				}
				respEnv, _ := protocol.NewHTTPResponse(env.RequestID, resp)
				h.sendEnv(respEnv)
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

	respEnv, err := protocol.NewHTTPResponse(env.RequestID, resp)
	if err != nil {
		log.Printf("create response envelope: %v", err)
		return
	}
	h.sendEnv(respEnv)
}

func (h *HOWSHandler) sendEnv(env *protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		log.Printf("marshal error: %v", err)
		return
	}
	if err := h.sender.Send(data); err != nil {
		log.Printf("send error: %v", err)
	}
}
