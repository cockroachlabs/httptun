package main

import (
	"log"
	"net/http"
	"os"

	"github.com/cockroachlabs/httptun"
	"go.uber.org/zap"
)

func main() {
	listenAddr := os.Getenv("HTTPTUN_LISTEN_ADDR")
	dstAddr := os.Getenv("HTTPTUN_DST_ADDR")

	if listenAddr == "" {
		panic("HTTPTUN_LISTEN_ADDR is not set")
	}

	if dstAddr == "" {
		panic("HTTPTUN_DST_ADDR is not set")
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}

	http.Handle("/ws", httptun.NewServer(dstAddr, logger.Sugar()))

	log.Fatalln(http.ListenAndServe(listenAddr, nil))
}
