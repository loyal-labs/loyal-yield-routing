package earnmax

import (
	"context"
	"encoding/json"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type WithdrawalHealth struct {
	Version      int        `json:"version"`
	Cluster      string     `json:"cluster"`
	Vault        string     `json:"vault"`
	Program      string     `json:"program"`
	RouteKey     string     `json:"routeKey"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	ObservedAt   time.Time  `json:"observedAt"`
	ObservedSlot int64      `json:"observedSlot"`
	BlockedSince *time.Time `json:"blockedSince"`
}

const (
	HealthNone              = "none"
	HealthWaiting           = "waiting"
	HealthOperatorAttention = "operator_attention"
	HealthUnavailable       = "unavailable"

	healthVersion = 1
	healthMaxAge  = 120 * time.Second
)

type HealthView struct {
	LastKnownAttention bool    `json:"lastKnownAttention"`
	ObservedAt         *string `json:"observedAt"`
	Status             string  `json:"status"`
}

func (r *Reader) Health(ctx context.Context, now time.Time) HealthView {
	raw, err := r.routeState(ctx, "'withdrawalHealth'")
	if err != nil {
		return HealthView{Status: HealthUnavailable}
	}
	return healthView(raw, now)
}

func healthView(raw []byte, now time.Time) HealthView {
	unavailable := HealthView{Status: HealthUnavailable}
	var h *WithdrawalHealth
	if raw == nil || json.Unmarshal(raw, &h) != nil || h == nil ||
		h.Version != healthVersion || h.Cluster != Cluster || h.Program != voltr.ProgramID.String() ||
		h.RouteKey != RouteKey || h.Vault != Vault.String() {
		return unavailable
	}
	switch h.Status {
	case HealthNone, HealthWaiting, HealthOperatorAttention, HealthUnavailable:
	default:
		return unavailable
	}
	unavailable.LastKnownAttention = h.Status == HealthOperatorAttention
	if h.ObservedAt.After(now) || now.Sub(h.ObservedAt) > healthMaxAge || h.ObservedSlot <= 0 ||
		h.Status == HealthOperatorAttention && (h.BlockedSince == nil || h.BlockedSince.After(h.ObservedAt)) {
		return unavailable
	}
	observedAt := iso(h.ObservedAt)
	view := HealthView{LastKnownAttention: unavailable.LastKnownAttention, ObservedAt: &observedAt, Status: HealthUnavailable}
	if h.Status == HealthWaiting || h.Status == HealthOperatorAttention {
		view.Status = h.Status
	}
	return view
}
