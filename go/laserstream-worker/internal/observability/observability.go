package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
)

const serviceName = "loyal-go-laserstream-worker"

type Health struct {
	startedAt    time.Time
	ready        atomic.Bool
	connected    atomic.Bool
	frontier     atomic.Uint64
	lastProgress atomic.Int64
	fatal        atomic.Value
	mu           sync.RWMutex
	domains      map[string]uint64
}

func NewHealth() *Health {
	return &Health{startedAt: time.Now().UTC(), domains: make(map[string]uint64)}
}

func (h *Health) SetReady(value bool)     { h.ready.Store(value) }
func (h *Health) SetConnected(value bool) { h.connected.Store(value) }
func (h *Health) ResetProgress() {
	h.frontier.Store(0)
	h.lastProgress.Store(0)
}
func (h *Health) Progress(slot uint64) {
	h.frontier.Store(slot)
	h.lastProgress.Store(time.Now().UnixNano())
}
func (h *Health) DomainProgress(domain string, slot uint64) {
	h.mu.Lock()
	if slot > h.domains[domain] {
		h.domains[domain] = slot
	}
	h.mu.Unlock()
}
func (h *Health) Fatal(err error) {
	if err != nil {
		h.fatal.Store(err.Error())
	}
	h.ready.Store(false)
}
func (h *Health) Stale(timeout time.Duration) bool {
	last := h.lastProgress.Load()
	return last > 0 && time.Since(time.Unix(0, last)) > timeout
}

func (h *Health) Handler(progressTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": serviceName})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		last := time.Unix(0, h.lastProgress.Load())
		ready := h.ready.Load() && h.connected.Load() && !last.IsZero() && time.Since(last) <= progressTimeout
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		h.mu.RLock()
		domains := make(map[string]uint64, len(h.domains))
		for key, value := range h.domains {
			domains[key] = value
		}
		h.mu.RUnlock()
		response := map[string]any{
			"status":    map[bool]string{true: "ready", false: "not_ready"}[ready],
			"connected": h.connected.Load(), "frontier": h.frontier.Load(),
			"lastProgressAt": last.UTC(), "domainFrontiers": domains,
			"startedAt": h.startedAt,
		}
		if fatal := h.fatal.Load(); fatal != nil {
			response["fatalError"] = fatal
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	return mux
}

type Metrics struct {
	Updates        *prometheus.CounterVec
	Failures       *prometheus.CounterVec
	Duplicates     *prometheus.CounterVec
	Frontier       prometheus.Gauge
	LastProgress   prometheus.Gauge
	HandlerSeconds *prometheus.HistogramVec
	Reconnects     prometheus.Counter
	Handoffs       *prometheus.CounterVec
	EarnPending    prometheus.Gauge
	EarnFailed     prometheus.Gauge
	EarnOldestAge  prometheus.Gauge
	otelUpdates    otelmetric.Int64Counter
	otelFailures   otelmetric.Int64Counter
	otelDuplicates otelmetric.Int64Counter
	otelHandlers   otelmetric.Float64Histogram
	otelReconnects otelmetric.Int64Counter
	otelHandoffs   otelmetric.Int64Counter
}

func NewMetrics() *Metrics {
	meter := otel.Meter(serviceName)
	otelUpdates, _ := meter.Int64Counter("loyal.laserstream.updates")
	otelFailures, _ := meter.Int64Counter("loyal.laserstream.failures")
	otelDuplicates, _ := meter.Int64Counter("loyal.laserstream.duplicates")
	otelHandlers, _ := meter.Float64Histogram("loyal.laserstream.handler.duration", otelmetric.WithUnit("s"))
	otelReconnects, _ := meter.Int64Counter("loyal.laserstream.reconnects")
	otelHandoffs, _ := meter.Int64Counter("loyal.laserstream.handoffs")
	metrics := &Metrics{
		Updates:        prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_laserstream_updates_total", Help: "Durably handled LaserStream updates."}, []string{"domain", "filter"}),
		Failures:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_laserstream_failures_total", Help: "Worker failures requiring retry or operator attention."}, []string{"operation"}),
		Duplicates:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_laserstream_duplicates_total", Help: "Durable replay duplicates."}, []string{"domain"}),
		Frontier:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "loyal_laserstream_frontier_slot", Help: "Latest application-durable stream slot."}),
		LastProgress:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "loyal_laserstream_last_progress_unixtime", Help: "Unix time of latest durable progress."}),
		HandlerSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "loyal_laserstream_handler_seconds", Help: "Durable handler latency.", Buckets: prometheus.DefBuckets}, []string{"domain"}),
		Reconnects:     prometheus.NewCounter(prometheus.CounterOpts{Name: "loyal_laserstream_reconnects_total", Help: "Full stream reconnect attempts."}),
		Handoffs:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_laserstream_handoffs_total", Help: "Filter-set handoff outcomes."}, []string{"outcome"}),
		EarnPending:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "loyal_laserstream_earn_pending_jobs", Help: "Pending durable Earn reconciliation jobs."}),
		EarnFailed:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "loyal_laserstream_earn_failed_pending_jobs", Help: "Pending Earn jobs with a recorded failure."}),
		EarnOldestAge:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "loyal_laserstream_earn_oldest_pending_age_seconds", Help: "Age of the oldest pending Earn job."}),
		otelUpdates:    otelUpdates, otelFailures: otelFailures, otelDuplicates: otelDuplicates,
		otelHandlers: otelHandlers, otelReconnects: otelReconnects, otelHandoffs: otelHandoffs,
	}
	prometheus.MustRegister(metrics.Updates, metrics.Failures, metrics.Duplicates, metrics.Frontier, metrics.LastProgress, metrics.HandlerSeconds, metrics.Reconnects, metrics.Handoffs, metrics.EarnPending, metrics.EarnFailed, metrics.EarnOldestAge)
	return metrics
}

func (m *Metrics) RecordUpdate(ctx context.Context, domain, filter string) {
	m.Updates.WithLabelValues(domain, filter).Inc()
	m.otelUpdates.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("domain", domain), attribute.String("filter", filter)))
}
func (m *Metrics) RecordFailure(ctx context.Context, operation string) {
	m.Failures.WithLabelValues(operation).Inc()
	m.otelFailures.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("operation", operation)))
}
func (m *Metrics) RecordDuplicate(ctx context.Context, domain string) {
	m.Duplicates.WithLabelValues(domain).Inc()
	m.otelDuplicates.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("domain", domain)))
}
func (m *Metrics) ObserveHandler(ctx context.Context, domain string, duration time.Duration) {
	seconds := duration.Seconds()
	m.HandlerSeconds.WithLabelValues(domain).Observe(seconds)
	m.otelHandlers.Record(ctx, seconds, otelmetric.WithAttributes(attribute.String("domain", domain)))
}
func (m *Metrics) RecordReconnect(ctx context.Context) {
	m.Reconnects.Inc()
	m.otelReconnects.Add(ctx, 1)
}
func (m *Metrics) RecordHandoff(ctx context.Context, outcome string) {
	m.Handoffs.WithLabelValues(outcome).Inc()
	m.otelHandoffs.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("outcome", outcome)))
}

func InitOTEL(ctx context.Context) (func(context.Context) error, error) {
	serviceResource, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("create OTEL resource: %w", err)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		provider := metric.NewMeterProvider(metric.WithResource(serviceResource))
		otel.SetMeterProvider(provider)
		return provider.Shutdown, nil
	}
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	provider := metric.NewMeterProvider(metric.WithResource(serviceResource), metric.WithReader(metric.NewPeriodicReader(exporter)))
	otel.SetMeterProvider(provider)
	return provider.Shutdown, nil
}

func Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", serviceName)
}
