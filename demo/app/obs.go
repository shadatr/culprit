package main

import (
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the instruments shared by every role. Every series carries a
// constant service label so recording rules can aggregate by service.
type Metrics struct {
	reg      *prometheus.Registry
	wrapped  prometheus.Registerer
	duration *prometheus.HistogramVec
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
}

func newMetrics(service string) *Metrics {
	reg := prometheus.NewRegistry()
	w := prometheus.WrapRegistererWith(prometheus.Labels{"service": service}, reg)
	w.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	m := &Metrics{
		reg:     reg,
		wrapped: w,
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP server request latency.",
			Buckets: []float64{.005, .01, .025, .05, .075, .1, .15, .25, .4, .6, 1, 1.5, 2.5, 5, 10},
		}, []string{"route", "code"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP server requests.",
		}, []string{"route", "code"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_errors_total",
			Help: "HTTP server requests answered with a 5xx code.",
		}, []string{"route", "code"}),
	}
	w.MustRegister(m.duration, m.requests, m.errors)
	return m
}

// Register adds role-specific collectors (they get the service label too).
func (m *Metrics) Register(cs ...prometheus.Collector) { m.wrapped.MustRegister(cs...) }

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// routeOf turns a ServeMux pattern ("GET /products/{id}") into a route label.
func routeOf(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	return pattern
}

// Instrument records metrics and access logs for every request served by mux.
// Errors (5xx) are always logged; 4xx at info; 2xx sampled to keep volume sane.
func (m *Metrics) Instrument(mux *http.ServeMux, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		mux.ServeHTTP(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		route := routeOf(r.Pattern)
		if route == "/metrics" || route == "/healthz" {
			return
		}
		el := time.Since(start)
		code := strconv.Itoa(sw.code)
		m.duration.WithLabelValues(route, code).Observe(el.Seconds())
		m.requests.WithLabelValues(route, code).Inc()
		if sw.code >= 500 {
			m.errors.WithLabelValues(route, code).Inc()
		}
		attrs := []any{"method", r.Method, "route", route, "code", sw.code, "duration_ms", el.Milliseconds()}
		switch {
		case sw.code >= 500:
			log.Warn("request failed", attrs...)
		case sw.code >= 400:
			log.Info("request", attrs...)
		case rand.Float64() < 0.05:
			log.Info("request", attrs...)
		}
	})
}

// newLogger logs JSON to stdout and, if lokiURL is set, to Loki as well.
func newLogger(service, version, lokiURL string) (*slog.Logger, *LokiPusher) {
	var w io.Writer = os.Stdout
	var p *LokiPusher
	if lokiURL != "" {
		p = NewLokiPusher(LokiConfig{URL: lokiURL, Labels: map[string]string{"service": service}})
		w = io.MultiWriter(os.Stdout, p)
	}
	lvl := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		lvl = slog.LevelDebug
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service, "version", version), p
}
