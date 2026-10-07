package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Facts are the only health signal a family emits. Alerting rules live in
// Alertmanager; nothing in-process interprets them.
//
//	loyal_family_landed_total{family}                    operations that reached their on-chain outcome
//	loyal_family_failed_total{family,code}               operations that ended without it, by stable code
//	loyal_family_inflight{family}                        operations signed or sent and not yet terminal
//	loyal_family_last_progress_timestamp_seconds{family} last time the family completed a unit of work
type Facts struct {
	landed   *prometheus.CounterVec
	failed   *prometheus.CounterVec
	inflight *prometheus.GaugeVec
	progress *prometheus.GaugeVec
}

func NewFacts(registerer prometheus.Registerer) *Facts {
	f := &Facts{
		landed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_family_landed_total", Help: "Operations that reached their on-chain outcome.",
		}, []string{"family"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_family_failed_total", Help: "Operations that ended without their outcome, by stable code.",
		}, []string{"family", "code"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_family_inflight", Help: "Operations signed or sent and not yet terminal.",
		}, []string{"family"}),
		progress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_family_last_progress_timestamp_seconds", Help: "Last time the family completed a unit of work.",
		}, []string{"family"}),
	}
	registerer.MustRegister(f.landed, f.failed, f.inflight, f.progress)
	return f
}

func (f *Facts) Landed(family Family) {
	f.landed.WithLabelValues(string(family)).Inc()
	f.Progress(family)
}

func (f *Facts) Failed(family Family, code string) {
	f.failed.WithLabelValues(string(family), code).Inc()
}

func (f *Facts) Inflight(family Family, n int) {
	f.inflight.WithLabelValues(string(family)).Set(float64(n))
}

func (f *Facts) Progress(family Family) {
	f.progress.WithLabelValues(string(family)).Set(float64(time.Now().Unix()))
}

// Own creates the series of each family this process writes at zero, so a
// family that never completes work reads as progress 0, not as absent.
func (f *Facts) Own(families ...Family) {
	for _, family := range families {
		f.landed.WithLabelValues(string(family))
		f.inflight.WithLabelValues(string(family))
		f.progress.WithLabelValues(string(family))
	}
}
