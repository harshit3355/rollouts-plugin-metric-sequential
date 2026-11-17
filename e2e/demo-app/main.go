// Command demo-app serves HTTP 500 for a configurable share of requests and
// exposes the counts for Prometheus. It is only used by the end-to-end test.
package main

import (
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	errorRate, err := strconv.ParseFloat(os.Getenv("ERROR_RATE"), 64)
	if err != nil || errorRate < 0 || errorRate > 1 {
		log.Fatalf("ERROR_RATE must be a number between 0 and 1, got %q", os.Getenv("ERROR_RATE"))
	}
	requests := promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "demo_requests_total", Help: "Requests served, by status code.",
	}, []string{"code"})
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		if rand.Float64() < errorRate {
			requests.WithLabelValues("500").Inc()
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		requests.WithLabelValues("200").Inc()
		_, _ = w.Write([]byte("ok\n"))
	})
	log.Printf("serving with ERROR_RATE=%g", errorRate)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
