package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"net/http/pprof"
)

var MetricSvcBytesHist = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_bytes",
		Help:      "Amount of bytes sent to/received from a service",
		Buckets:   []float64{1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 50000, 100000},
	},
	[]string{"node", "direction"},
)

var MetricSvcPacketsHist = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_packets",
		Help:      "Amount of packets sent to/received from a service",
		Buckets:   []float64{1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 50000, 100000},
	},
	[]string{"node", "direction"},
)

var MetricSvcDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_duration_seconds",
		Help:      "Conntrack flow duration (includes timeout waiting time)",
		Buckets:   []float64{0, 1, 2.5, 5, 10, 30, 31, 35, 119.9, 119.99, 120, 120.01, 120.015, 120.02, 120.025, 120.03, 120.1, 120.5, 121, 125, 130},
	},
	[]string{"node"},
)

var MetricSvcSeenReplyLatency = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_seen_reply_latency_seconds",
		Help:      "Latency between conntrack flow being created and SEEN_REPLY event",
		Buckets:   []float64{0, .00001, .00005, .00010, .00025, .00050, .00075, .001, .0025, .005, .0075, .01, .025, .05, .1, .2, .5, 1, 5, 10},
	},
	[]string{"node"},
)

var MetricSvcSeenReplyLatencySummary *prometheus.SummaryVec

var MetricSvcTCPFinLatency = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_TCP_FIN_latency_seconds",
		Help:      "Latency between conntrack flow being created and TCP FIN packet seen",
		Buckets:   []float64{0, .001, .005, .0075, .01, .02, .03, .04, .05, 0.075, .1, .125, .15, .175, .2, .3, .4, .5, 1, 3, 5, 10, 20, 50},
	},
	[]string{"node"},
)

var MetricSvcTCPFinLatencySummary *prometheus.SummaryVec

var MetricSvcTCPFinThroughput = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "service_TCP_FIN_throughput_bytes_per_second",
		Help:      "Throughput=(bytes sent as recorded when TCP FIN packet sent)/(service_TCP_FIN_latency_seconds)",
		Buckets:   []float64{100, 500, 1000, 5000, 10000, 25000, 50000, 100000, 500000, 10000000, 100000000},
	},
	[]string{"node", "direction"},
)

var MetricSvcTCPFinThroughputSummary *prometheus.SummaryVec

var MetricConntrackEventsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "conntrack_monitoring",
		Name:      "events_counter",
		Help:      "Number of handled conntrack events.",
	},
	[]string{"node", "type"},
)

var MetricConntrackQueueSizeHist = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "queue_size_hist",
		Help:      "Size of the conntrack events queue, histogram",
		Buckets:   []float64{10, 100, 500, 1000, 5000, 10000, 25000, 49000, 99000, 149000, 199000},
	},
	[]string{"node"},
)

var MetricConntrackWorkerQueueSizeHist = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "worker_queue_size_hist",
		Help:      "Size of the worker queues, histogram",
		Buckets:   []float64{10, 100, 500, 1000, 5000, 10000, 25000, 49000, 100000},
	},
	[]string{"node", "type"},
)

var MetricConntrackBatchSizeHist = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "mmsg_batch_size_hist",
		Help:      "Size of the conntrack events queue, histogram",
		Buckets:   []float64{1, 5, 10, 30, 50, 75, 100},
	},
	[]string{"node"},
)

var MetricConntrackNoBufsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "conntrack_monitoring",
		Name:      "no_bufs_counter",
		Help:      "How many times socket reader didn't have free buffers",
	},
	[]string{"node"},
)

var MetricDispatchTime = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "dispatch_handling_time_second",
		Help:      "Time spent by dispatcher for one message handling",
		Buckets:   []float64{0.0000005, 0.000001, 0.0000025, 0.000005, 0.0000075, 0.00001, .00005, .0001, 0.001, 0.01, 0.1},
	},
	[]string{"node"},
)

var MetricHandlingTime = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "event_handling_time_second",
		Help:      "Time spent by event handler for one event",
		Buckets:   []float64{0.0000001, 0.0000005, 0.000001, 0.000005, 0.00001, .00005, .0001, .0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	},
	[]string{"node", "type"},
)

var MetricEventInSystemTime = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "event_in_system_time_second",
		Help:      "Time spent by event in system before being fully processed",
		Buckets:   []float64{0.000001, 0.00001, .0001, 0.001, 0.01, 0.1, 0.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	},
	[]string{"node", "type"},
)

var MetricEventTillWorkerQueueTime = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "event_till_worker_queue_time_second",
		Help:      "Time spent by event in system before being processed by worker",
		Buckets:   []float64{0.000001, 0.00001, .0001, 0.001, 0.01, 0.1, 0.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	},
	[]string{"node", "type"},
)

var MetricRcvMsgsSpeed = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "conntrack_monitoring",
		Name:      "rcv_netlink_msg_per_100ms",
		Help:      "Received netlink messages speed per 100ms",
		Buckets:   []float64{10, 100, 1000, 10000, 12000, 15000, 17000, 20000, 25000, 30000, 40000, 50000, 60000, 70000, 80000, 90000, 100000},
	},
	[]string{"node"},
)

var MetricErrorCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "conntrack_monitoring",
		Name:      "error_counter",
		Help:      "Number of errors occurred in conntrack monitoring",
	},
	[]string{"node", "type"},
)

const (
	ErrorEventQueueFull         = "event queue full"
	ErrorWorkerEventQueueFull   = "worker event queue full"
	ErrorWorkerMetricsQueueFull = "worker metrics queue full"
	ErrorWorkerWaitQueueFull    = "worker wait queue full"
	ErrorWrongEventsOrder       = "events order"
)

func registerMetrics(nodeName string, summaryTimeout int) {
	MetricSvcSeenReplyLatencySummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "conntrack_monitoring",
			Name:       "service_seen_reply_latency_seconds_summary",
			Help:       "Latency between conntrack flow being created and SEEN_REPLY event",
			Objectives: map[float64]float64{0.01: 0.001, 0.1: 0.01, 0.5: 0.05, 0.9: 0.01, 0.95: 0.001, 0.99: 0.001, 1.0: 0.001},
			MaxAge:     time.Duration(summaryTimeout) * time.Minute,
		},
		[]string{"node"},
	)
	MetricSvcTCPFinLatencySummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "conntrack_monitoring",
			Name:       "service_TCP_FIN_latency_seconds_summary",
			Help:       "Latency between conntrack flow being created and TCP FIN packet seen",
			Objectives: map[float64]float64{0.01: 0.001, 0.1: 0.01, 0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			MaxAge:     time.Duration(summaryTimeout) * time.Minute,
		},
		[]string{"node"},
	)
	MetricSvcTCPFinThroughputSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace:  "conntrack_monitoring",
			Name:       "service_TCP_FIN_throughput_bytes_per_second_summary",
			Help:       "Throughput=(bytes sent as recorded when TCP FIN packet sent)/(service_TCP_FIN_latency_seconds)",
			Objectives: map[float64]float64{0.01: 0.001, 0.1: 0.01, 0.5: 0.05, 0.9: 0.01, 0.95: 0.001, 0.99: 0.001, 1.0: 0.001},
			MaxAge:     time.Duration(summaryTimeout) * time.Minute,
		},
		[]string{"node", "direction"},
	)

	for _, hist := range []*prometheus.HistogramVec{MetricSvcBytesHist, MetricSvcPacketsHist, MetricSvcDuration,
		MetricSvcSeenReplyLatency, MetricSvcTCPFinLatency, MetricHandlingTime, MetricDispatchTime,
		MetricConntrackQueueSizeHist, MetricConntrackWorkerQueueSizeHist, MetricEventInSystemTime,
		MetricConntrackBatchSizeHist, MetricEventTillWorkerQueueTime, MetricRcvMsgsSpeed, MetricSvcTCPFinThroughput} {
		hist.Reset()
		conntrackRegistry.MustRegister(hist)
	}
	for _, sum := range []*prometheus.SummaryVec{MetricSvcSeenReplyLatencySummary, MetricSvcTCPFinLatencySummary,
		MetricSvcTCPFinThroughputSummary} {

		sum.Reset()
		conntrackRegistry.MustRegister(sum)
	}
	for _, counter := range []*prometheus.CounterVec{MetricConntrackEventsCounter, MetricConntrackNoBufsCounter, MetricErrorCounter} {
		counter.Reset()
		conntrackRegistry.MustRegister(counter)
	}
	for _, gauge := range []*prometheus.GaugeVec{} {
		gauge.Reset()
		conntrackRegistry.MustRegister(gauge)
	}
	MetricConntrackNoBufsCounter.WithLabelValues(nodeName)
	for _, label := range []string{ErrorEventQueueFull, ErrorWorkerEventQueueFull, ErrorWorkerMetricsQueueFull,
		ErrorWorkerWaitQueueFull, ErrorWrongEventsOrder} {
		MetricErrorCounter.WithLabelValues(nodeName, label)
	}
}

var conntrackRegistry = prometheus.NewRegistry()

func startMetricsServer(bindAddress string, ctx context.Context) {
	handler := promhttp.InstrumentMetricHandler(conntrackRegistry,
		promhttp.HandlerFor(conntrackRegistry, promhttp.HandlerOpts{}))
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	var server *http.Server
	go utilwait.Until(func() {
		log.Printf("Starting metrics server to serve at address %q", bindAddress)
		server = &http.Server{
			Addr:    bindAddress,
			Handler: mux,
		}
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("starting metrics server to serve at address %q failed: %v", bindAddress, err)
		}
		log.Printf("metrics server has stopped serving at address %q", bindAddress)
	}, 5*time.Second, ctx.Done())

	<-ctx.Done()
	log.Printf("Stopping metrics server %s", server.Addr)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error stopping metrics server: %v", err)
	}
}
