// Copyright 2023 Cockroach Labs, Inc.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
