package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	"github.com/geminiwen/how/golang/client"
)

type wsSender struct {
	conn *websocket.Conn
	ctx  context.Context
}

func (s *wsSender) SendBytes(data []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, data)
}

func (s *wsSender) SendText(data string) error {
	return s.conn.Write(s.ctx, websocket.MessageText, []byte(data))
}

func main() {
	serverURL := flag.String("server", "", "server WebSocket URL (e.g., ws://server:8888/ws)")
	target := flag.String("target", "", "local HTTP service to forward to (e.g., http://localhost:8080)")
	flag.Parse()

	if *serverURL == "" || *target == "" {
		flag.Usage()
		log.Fatal("--server and --target are required")
	}

	handler, err := client.ForwardTo(*target)
	if err != nil {
		log.Fatalf("invalid target: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, _, err := websocket.Dial(ctx, *serverURL, &websocket.DialOptions{
		Subprotocols: []string{"how.v1"},
	})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sender := &wsSender{conn: conn, ctx: ctx}
	h := client.NewHandler(handler, sender)

	log.Println("connected, handling messages...")

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Fatalf("read: %v", err)
		}
		h.HandleBinaryMessage(ctx, data)
	}
}
