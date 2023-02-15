package httptun

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	streamsCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "httptun_streams_count",
		Help: "The number of streams.",
	})
)

func init() {
	prometheus.MustRegister(streamsCount)
}
