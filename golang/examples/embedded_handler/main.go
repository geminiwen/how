package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
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
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from embedded handler! Method: %s, URL: %s", r.Method, r.URL.String())
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	})

	handler := client.HTTPHandler(mux)

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

	log.Println("connected, handling messages...")

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Fatalf("read: %v", err)
		}
		h.HandleMessage(ctx, data)
	}
}
