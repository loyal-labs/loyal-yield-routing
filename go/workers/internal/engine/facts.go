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
//	loyal_lane_last_success_timestamp_seconds{family,lane} last tick a lane finished without error
//	loyal_fee_payer_balance_lamports{payer}              lamports of a payer, read every pass
//	loyal_autodeposit_oldest_due_lot_age_seconds         age of the oldest owned or blocked deposit
//	loyal_fleet_capacity_replans_total{reason}           moves dropped at execution because they no longer fit
//	loyal_fleet_simulation_failures_total{stage}         execution simulations that failed; the move retries
//	loyal_fleet_movable_capital_usd{reserve}             capital in a reserve with an eligible move this cycle
type Facts struct {
	withdrawalAttention *prometheus.GaugeVec
	withdrawalObserved  *prometheus.GaugeVec
	landed              *prometheus.CounterVec
	failed              *prometheus.CounterVec
	inflight            *prometheus.GaugeVec
	progress            *prometheus.GaugeVec
	lane                *prometheus.GaugeVec
	payer               *prometheus.GaugeVec
	oldestDue           prometheus.Gauge
	fleetReplans        *prometheus.CounterVec
	fleetSimulations    *prometheus.CounterVec
	fleetMovable        *prometheus.GaugeVec
}

func NewFacts(registerer prometheus.Registerer) *Facts {
	f := &Facts{
		withdrawalAttention: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_withdrawal_attention", Help: "Durable Backyard withdrawal requires operator attention.",
		}, []string{"family", "route"}),
		withdrawalObserved: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_withdrawal_observed_timestamp_seconds", Help: "Coherent observation time of durable Backyard withdrawal health.",
		}, []string{"family", "route"}),
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
		lane: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_lane_last_success_timestamp_seconds", Help: "Last tick a lane finished without error; set at start.",
		}, []string{"family", "lane"}),
		payer: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_fee_payer_balance_lamports", Help: "Lamports of a fee payer, read every pass.",
		}, []string{"payer"}),
		oldestDue: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "loyal_autodeposit_oldest_due_lot_age_seconds", Help: "Age of the oldest Autodeposit claim or idle-blocked slot not yet deposited.",
		}),
		fleetReplans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_fleet_capacity_replans_total", Help: "Fleet moves dropped at execution because they no longer fit; the vault is planned again.",
		}, []string{"reason"}),
		fleetSimulations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_fleet_simulation_failures_total", Help: "Fleet execution simulations that failed before admission; the move retries.",
		}, []string{"stage"}),
		fleetMovable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_fleet_movable_capital_usd", Help: "Capital in a reserve whose move to a better reserve cleared every planner check this cycle.",
		}, []string{"reserve"}),
	}
	registerer.MustRegister(f.withdrawalAttention, f.withdrawalObserved, f.landed, f.failed, f.inflight, f.progress, f.lane, f.payer, f.oldestDue, f.fleetReplans, f.fleetSimulations, f.fleetMovable)
	return f
}

// FleetCapacityReplan counts a move the execution dropped because it no
// longer fits; it is not a failed operation, so it is not in failed_total.
func (f *Facts) FleetCapacityReplan(reason string) { f.fleetReplans.WithLabelValues(reason).Inc() }

// FleetSimulationFailed counts an execution simulation that failed.
func (f *Facts) FleetSimulationFailed(stage string) { f.fleetSimulations.WithLabelValues(stage).Inc() }

// FleetMovableCapital sets every planned reserve's movable capital each
// cycle, zero included, so a reserve that stops being movable reads 0.
func (f *Facts) FleetMovableCapital(reserve string, usd float64) {
	f.fleetMovable.WithLabelValues(reserve).Set(usd)
}

// BackyardWithdrawalHealth is called only after the display state commits.
func (f *Facts) BackyardWithdrawalHealth(route string, attention bool, observed time.Time) {
	value := float64(0)
	if attention {
		value = 1
	}
	f.withdrawalAttention.WithLabelValues(string(FamilyBackyard), route).Set(value)
	f.withdrawalObserved.WithLabelValues(string(FamilyBackyard), route).Set(float64(observed.Unix()))
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

// LaneSucceeded records a lane tick that finished without error. A lane
// shares its family's progress with the family's other lanes, so this is the
// one fact that shows a single lane failing every tick.
func (f *Facts) LaneSucceeded(family Family, lane string) {
	f.lane.WithLabelValues(string(family), lane).Set(float64(time.Now().Unix()))
}

func (f *Facts) FeePayerBalance(payer string, lamports uint64) {
	f.payer.WithLabelValues(payer).Set(float64(lamports))
}

func (f *Facts) AutodepositOldestDueAge(seconds float64) { f.oldestDue.Set(seconds) }

// Own creates the series of each family this process writes at zero, so a
// family that never completes work reads as progress 0, not as absent.
func (f *Facts) Own(families ...Family) {
	for _, family := range families {
		f.landed.WithLabelValues(string(family))
		f.inflight.WithLabelValues(string(family))
		f.progress.WithLabelValues(string(family))
	}
}
