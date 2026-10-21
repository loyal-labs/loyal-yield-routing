package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Facts are the only health signal a family emits. Alerting rules live in
// Alertmanager; nothing in-process interprets them.
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
	leverageRoom        *prometheus.GaugeVec
	leverageGap         *prometheus.GaugeVec
	leverageLanded      *prometheus.CounterVec
	leverageReaction    *prometheus.HistogramVec
	leverageOutages     *prometheus.CounterVec
	termDebtDays        *prometheus.GaugeVec
	stepNoRoom          *prometheus.CounterVec
	hold                *prometheus.GaugeVec
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
		leverageRoom: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_leverage_borrow_room_usd", Help: "A held Earn Max lane's debt reserve borrow room in USD at the reserve's oracle price, recomputed on every reserve update.",
		}, []string{"vault", "lane"}),
		leverageGap: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_leverage_gap", Help: "A held Earn Max lane's stored leverage target minus its actual leverage (0 at or above target).",
		}, []string{"vault", "lane"}),
		leverageLanded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_backyard_leverage_up_landed_total", Help: "Earn Max leverage_up borrows that landed and reconciled.",
		}, []string{"vault", "lane"}),
		leverageReaction: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "loyal_backyard_leverage_reaction_seconds", Help: "From the first observed borrow room at or above the minimum, on a lane below its leverage target, to its leverage_up borrow landing.",
			Buckets: []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 900, 3600},
		}, []string{"vault"}),
		leverageOutages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_backyard_leverage_decision_outages_total", Help: "Earn Max leverage samples that could not be decided (the stored target and approval carry), by sanitized cause.",
		}, []string{"vault", "reason"}),
		termDebtDays: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_term_debt_days_to_maturity", Help: "A held Earn Max lane's days from the chain clock to its term debt's maturity (negative once matured); no series without term debt.",
		}, []string{"vault", "lane"}),
		stepNoRoom: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loyal_backyard_step_no_room_total", Help: "Backyard ticks whose step held because the chain had no room for the move its target wants, by lane and target rule.",
		}, []string{"vault", "lane", "rule"}),
		hold: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loyal_backyard_hold", Help: "Why the last Backyard tick did not move the vault: 1 for its lane and hold reason (at_target, no_room, stale, chain_refused, stopped); no series while it acts.",
		}, []string{"vault", "lane", "reason"}),
	}
	registerer.MustRegister(f.withdrawalAttention, f.withdrawalObserved, f.landed, f.failed, f.inflight, f.progress, f.lane, f.payer, f.oldestDue, f.fleetReplans, f.fleetSimulations, f.fleetMovable,
		f.leverageRoom, f.leverageGap, f.leverageLanded, f.leverageReaction, f.leverageOutages, f.termDebtDays, f.stepNoRoom, f.hold)
	return f
}

func (f *Facts) FleetCapacityReplan(reason string) { f.fleetReplans.WithLabelValues(reason).Inc() }

func (f *Facts) FleetSimulationFailed(stage string) { f.fleetSimulations.WithLabelValues(stage).Inc() }

func (f *Facts) FleetMovableCapital(reserve string, usd float64) {
	f.fleetMovable.WithLabelValues(reserve).Set(usd)
}

func (f *Facts) BackyardLeverageRoom(vault, lane string, usd float64) {
	f.leverageRoom.WithLabelValues(vault, lane).Set(usd)
}

func (f *Facts) BackyardLeverageGap(vault, lane string, gap float64, targeted bool) {
	if !targeted {
		f.leverageGap.DeleteLabelValues(vault, lane)
		return
	}
	f.leverageGap.WithLabelValues(vault, lane).Set(gap)
}

func (f *Facts) BackyardLeverageOutage(vault, reason string) {
	f.leverageOutages.WithLabelValues(vault, reason).Inc()
}

func (f *Facts) BackyardStepNoRoom(vault, lane, rule string) {
	f.stepNoRoom.WithLabelValues(vault, lane, rule).Inc()
}

func (f *Facts) BackyardHold(vault, lane, reason string) {
	f.BackyardHoldCleared(vault)
	f.hold.WithLabelValues(vault, lane, reason).Set(1)
}

func (f *Facts) BackyardHoldCleared(vault string) {
	f.hold.DeletePartialMatch(prometheus.Labels{"vault": vault})
}

func (f *Facts) BackyardTermDebtDays(vault, lane string, days float64, owed bool) {
	if !owed {
		f.termDebtDays.DeleteLabelValues(vault, lane)
		return
	}
	f.termDebtDays.WithLabelValues(vault, lane).Set(days)
}

func (f *Facts) BackyardLeverageLaneGone(vault, lane string) {
	f.leverageGap.DeleteLabelValues(vault, lane)
	f.leverageRoom.DeleteLabelValues(vault, lane)
	f.termDebtDays.DeleteLabelValues(vault, lane)
}

func (f *Facts) BackyardLeverageUpLanded(vault, lane string, reaction time.Duration, known bool) {
	f.leverageLanded.WithLabelValues(vault, lane).Inc()
	if known {
		f.leverageReaction.WithLabelValues(vault).Observe(reaction.Seconds())
	}
}

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

func (f *Facts) InflightAdd(family Family, delta int) {
	f.inflight.WithLabelValues(string(family)).Add(float64(delta))
}

func (f *Facts) Progress(family Family) {
	f.progress.WithLabelValues(string(family)).Set(float64(time.Now().Unix()))
}

func (f *Facts) LaneSucceeded(family Family, lane string) {
	f.lane.WithLabelValues(string(family), lane).Set(float64(time.Now().Unix()))
}

func (f *Facts) FeePayerBalance(payer string, lamports uint64) {
	f.payer.WithLabelValues(payer).Set(float64(lamports))
}

func (f *Facts) AutodepositOldestDueAge(seconds float64) { f.oldestDue.Set(seconds) }

func (f *Facts) SeedFamilySeriesAtZero(families ...Family) {
	for _, family := range families {
		f.landed.WithLabelValues(string(family))
		f.inflight.WithLabelValues(string(family))
		f.progress.WithLabelValues(string(family))
	}
}
