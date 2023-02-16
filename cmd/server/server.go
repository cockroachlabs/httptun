package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cockroachlabs/httptun"
	"go.uber.org/zap"
)

func main() {
	listenAddr := os.Getenv("HTTPTUN_LISTEN_ADDR")
	dstAddr := os.Getenv("HTTPTUN_DST_ADDR")
	enableDebug := os.Getenv("HTTPTUN_DEBUG") != "" && os.Getenv("HTTPTUN_DEBUG") != "0"

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

	cfg := zap.NewProductionConfig()

	if enableDebug {
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	} else {
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}

	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	http.Handle("/ws", httptun.NewServer(dstAddr, time.Minute, logger.Sugar()))

	log.Fatalln(http.ListenAndServe(listenAddr, nil))
}
