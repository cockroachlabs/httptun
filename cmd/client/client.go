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

// This is a test client that is unused for in-situ investigations.

func main() {
	ctx := context.Background()
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	client := &httptun.Client{
		Dialer:         websocket.DefaultDialer,
		Addr:           "ws://localhost:4600/ws",
		RequestHeaders: nil,
		Logger:         logger.Sugar(),
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
