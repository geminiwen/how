package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	"github.com/geminiwen/how/client"
)

type wsSender struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *wsSender) Send(data []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

func main() {
	handler, err := client.ForwardTo("http://localhost:8080")
	if err != nil {
		log.Fatalf("invalid target: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, _, err := websocket.Dial(ctx, "ws://localhost:8888/ws", &websocket.DialOptions{
		Subprotocols: []string{"how.v1"},
	})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sender := &wsSender{conn: conn, ctx: ctx}
	h := client.NewHandler(handler, sender)

	log.Println("connected, forwarding to http://localhost:8080...")

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Fatalf("read: %v", err)
		}
		h.HandleMessage(ctx, data)
	}
}
