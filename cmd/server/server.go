package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cockroachlabs/httptun"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	// Default to 1 minute timeout.
	timeout := time.Minute
	if t := os.Getenv("HTTPTUN_JANITOR_TIMEOUT"); t != "" {
		var err error
		timeout, err = time.ParseDuration(t)
		if err != nil {
			panic(err)
		}
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
	if enableDebug {
		logger.Debug("verbose logging enabled")
	}

	http.Handle("/ws", httptun.NewServer(dstAddr, timeout, logger.Sugar()))

	http.Handle("/metrics", promhttp.Handler())

	log.Fatalln(http.ListenAndServe(listenAddr, nil))
}
