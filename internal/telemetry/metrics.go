package telemetry

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The metrics are Prometheus' own library and a few lines of ours around
// each request. OpenTelemetry's instrumentation of net/http could measure
// them too, at about 11 µs and 28 allocations for each one; this is one
// histogram, found by three labels.

// Threads is the state of the PHP threads at one moment (core.Stats).
type Threads struct {
	Threads, Busy      int
	OffloadThreads     int
	InFlight           int64
	Restarts, Recycled int64
}

// buckets are the upper bounds of the duration histograms, in seconds.
var buckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// metrics is what the servers count and the endpoint that serves it.
type metrics struct {
	http   *prometheus.HistogramVec // by method, route and status
	grpc   *prometheus.HistogramVec // by method and code
	reg    *prometheus.Registry
	scrape http.Handler
}

// startMetrics makes the metrics of the servers and of the PHP threads, and
// those of the Go runtime and the process, for one Prometheus endpoint.
func startMetrics(threads func() Threads) *metrics {
	m := &metrics{
		http: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cairo_http_request_duration_seconds",
			Help:    "HTTP requests by method, route and status, and how long they took.",
			Buckets: buckets,
		}, []string{"method", "route", "status"}),
		grpc: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cairo_grpc_call_duration_seconds",
			Help:    "gRPC calls served, by method and status code, and how long they took.",
			Buckets: buckets,
		}, []string{"method", "code"}),
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.http, m.grpc, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if threads != nil {
		reg.MustRegister(phpThreads(threads))
	}
	m.reg = reg
	m.scrape = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m
}

// statuses are the labels of the HTTP statuses, made once.
var statuses = func() (s [500]string) {
	for i := range s {
		s[i] = strconv.Itoa(100 + i)
	}
	return s
}()

// measure counts each request of the router and how long it took, under
// the route the router matched: a label is never the request's own path.
func (m *metrics) measure(router http.Handler, skip func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip(r) {
			router.ServeHTTP(w, r)
			return
		}
		c := counted{ResponseWriter: w}
		start := time.Now()
		router.ServeHTTP(&c, r)
		status := statuses[http.StatusOK-100] // what net/http sends for a handler that wrote nothing
		if c.status >= 100 && c.status < 600 {
			status = statuses[c.status-100]
		}
		m.http.WithLabelValues(method(r.Method), route(r.Pattern), status).Observe(time.Since(start).Seconds())
	})
}

// counted is a response that remembers its status.
type counted struct {
	http.ResponseWriter
	status int
}

func (c *counted) WriteHeader(code int) {
	if c.status == 0 && code >= 200 { // not an informational 1xx before the answer
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the server's writer.
func (c *counted) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// ReadFrom keeps the server's own way to send a file (sendfile) in reach
// of io.Copy, which looks for it on the writer it is given.
func (c *counted) ReadFrom(r io.Reader) (int64, error) { return io.Copy(c.ResponseWriter, r) }

// method is the request's method when it is one of HTTP's: any token is a
// method, and a label is not the client's to choose.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "other"
}

// route is the path of the router's pattern ("GET /users/{id}"), "" for a
// request that matched none.
func route(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}
	return ""
}

// phpThreads reads the PHP threads once for each scrape.
func phpThreads(read func() Threads) prometheus.Collector {
	return &threadsCollector{
		read:     read,
		threads:  prometheus.NewDesc("cairo_php_threads", "PHP threads, by pool: request or offload.", []string{"pool"}, nil),
		busy:     prometheus.NewDesc("cairo_php_threads_busy", "Request threads that are running PHP right now.", nil, nil),
		inFlight: prometheus.NewDesc("cairo_php_requests_in_flight", "Requests queued for the PHP threads or held by them.", nil, nil),
		restarts: prometheus.NewDesc("cairo_php_thread_restarts_total", "PHP threads started again after they stopped unexpectedly.", nil, nil),
		recycles: prometheus.NewDesc("cairo_php_thread_recycles_total", "PHP threads replaced at maxRequests() or maxMemory().", nil, nil),
	}
}

type threadsCollector struct {
	read                                        func() Threads
	threads, busy, inFlight, restarts, recycles *prometheus.Desc
}

func (c *threadsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.threads, c.busy, c.inFlight, c.restarts, c.recycles} {
		ch <- d
	}
}

func (c *threadsCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.read()
	ch <- prometheus.MustNewConstMetric(c.threads, prometheus.GaugeValue, float64(s.Threads), "request")
	ch <- prometheus.MustNewConstMetric(c.threads, prometheus.GaugeValue, float64(s.OffloadThreads), "offload")
	ch <- prometheus.MustNewConstMetric(c.busy, prometheus.GaugeValue, float64(s.Busy))
	ch <- prometheus.MustNewConstMetric(c.inFlight, prometheus.GaugeValue, float64(s.InFlight))
	ch <- prometheus.MustNewConstMetric(c.restarts, prometheus.CounterValue, float64(s.Restarts))
	ch <- prometheus.MustNewConstMetric(c.recycles, prometheus.CounterValue, float64(s.Recycled))
}
