package engine

import "github.com/prometheus/client_golang/prometheus"

type jupiterFacts struct {
	requests  *prometheus.CounterVec
	deferred  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	quota     *prometheus.GaugeVec
	coalesced prometheus.Counter
}

func newJupiterFacts(registerer prometheus.Registerer) jupiterFacts {
	f := jupiterFacts{
		requests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_jupiter_requests_total", Help: "Actual Jupiter HTTP attempts, including redirects; local deferrals are excluded."}, []string{"class", "endpoint", "outcome"}),
		deferred:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "loyal_jupiter_deferred_total", Help: "Jupiter requests withheld locally, by bounded reason."}, []string{"class", "reason"}),
		duration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "loyal_jupiter_request_seconds", Help: "Jupiter request latency including budget admission; response body download excluded.", Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 20}}, []string{"class", "endpoint"}),
		quota:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "loyal_jupiter_quota", Help: "Last valid gateway quota headers; observed_at is their Unix observation timestamp."}, []string{"field"}),
		coalesced: prometheus.NewCounter(prometheus.CounterOpts{Name: "loyal_jupiter_quotes_coalesced_total", Help: "Background quote callers served by another identical in-flight request."}),
	}
	registerer.MustRegister(f.requests, f.deferred, f.duration, f.quota, f.coalesced)
	return f
}

func (f *Facts) JupiterRequest(class, endpoint, outcome string, seconds float64) {
	f.requests.WithLabelValues(class, endpoint, outcome).Inc()
	f.duration.WithLabelValues(class, endpoint).Observe(seconds)
}

func (f *Facts) JupiterDeferred(class, reason string) {
	f.deferred.WithLabelValues(class, reason).Inc()
}

func (f *Facts) JupiterQuota(current, remaining, reset float64) {
	f.quota.WithLabelValues("current").Set(current)
	f.quota.WithLabelValues("remaining").Set(remaining)
	f.quota.WithLabelValues("reset").Set(reset)
	f.quota.WithLabelValues("observed_at").SetToCurrentTime()
}

func (f *Facts) JupiterCoalesced() { f.coalesced.Inc() }
