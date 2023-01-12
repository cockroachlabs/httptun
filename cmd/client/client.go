package main

import (
	"context"
	"io"
	"log"
	"net"

	"github.com/cockroachlabs/httptun"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func main() {
	ctx := context.Background()
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	client := httptun.NewClient(ctx, func(ctx context.Context) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:4600/ws", nil)
		return conn, err
	}, 3, logger.Sugar())

	err = client.Connect(ctx)
	if err != nil {
		panic(err)
	}

	listener, err := net.Listen("tcp", "localhost:4601")
	if err != nil {
		panic(err)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			panic(err)
		}

		go func() {
			defer conn.Close()

			proxy, err := client.Dial(ctx)
			if err != nil {
				log.Println(err)
				return
			}

			defer proxy.Close()

			go func() {
				defer conn.Close()
				defer proxy.Close()
				io.Copy(conn, proxy)
			}()

			io.Copy(proxy, conn)
		}()
	}
}
