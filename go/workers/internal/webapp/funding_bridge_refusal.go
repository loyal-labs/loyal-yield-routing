package webapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/bridge"
)

const bridgeRefusalPath = "/api/funding/bridge/transfers/{requestId}/sponsor-refusal"

var refusalBudget = budget{every: time.Minute, burst: 5}

type SponsorRefusals struct {
	counter *prometheus.CounterVec
	limits  *limiters
}

func NewSponsorRefusals(reg prometheus.Registerer) *SponsorRefusals {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "loyal_bridge_sponsor_refusals_total",
		Help: "Sponsored Move sends Privy refused (gas credits or policy), reported by the signed-in client for its own quoted transfer.",
	}, []string{"chain"})
	for _, n := range bridge.Networks {
		counter.WithLabelValues(string(n.Chain))
	}
	reg.MustRegister(counter)
	return &SponsorRefusals{counter: counter, limits: newLimiters()}
}

type BridgeSponsorRefusalRequest struct {
	Message string `json:"message" maxLength:"500" doc:"Privy's error text, logged for the operator"`
}

type bridgeRefusalInput struct {
	SessionInput
	RequestID string `path:"requestId" doc:"Relay request id from the quote"`
	Raw       rawJSON
}

func (h earnRoutes) registerBridgeRefusal(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-funding-bridge-sponsor-refusal",
		Method:      http.MethodPost,
		Path:        bridgeRefusalPath,
		Summary:     "Report that Privy refused to sponsor the send of a quoted bridge",
		Responses:   errorResponses[routeError](api, 400, 401, 404, 429, 503),
	}, h.bridgeSponsorRefusal)
	documentJSONBody[BridgeSponsorRefusalRequest](api, bridgeRefusalPath)
}

func (h earnRoutes) bridgeSponsorRefusal(ctx context.Context, in *bridgeRefusalInput) (*bridgeTransferOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	wallet, err := h.evmDepositWallet(in.SessionInput)
	if err != nil {
		return nil, err
	}
	var body BridgeSponsorRefusalRequest
	if json.Unmarshal(in.Raw.data, &body) != nil || len(body.Message) > 500 {
		return nil, earnError(http.StatusBadRequest, "invalid_body", "message must be text of at most 500 characters.")
	}
	limit := h.Refusals.limits.get(limiterKey{"sponsor-refusal", "wallet", wallet.String()}, refusalBudget, time.Now())
	if !limit.Allow() {
		return nil, earnError(http.StatusTooManyRequests, "rate_limited", "Too many reports. Please wait a minute.")
	}
	t, err := h.bridgeTransfer(ctx, wallet, in.RequestID)
	if err != nil {
		return nil, bridgeUnavailable("bridge transfer read", err)
	}
	if t == nil || t.State != bridge.TransferQuoted {
		return nil, earnError(http.StatusNotFound, "bridge_transfer_missing", "No quoted transfer with this id for this wallet.")
	}
	h.Refusals.counter.WithLabelValues(string(t.Chain)).Inc()
	slog.Warn("bridge sponsored send refused", "chain", t.Chain, "request_id", t.RequestID, "wallet", wallet.String(), "message", body.Message)
	return &bridgeTransferOutput{Body: *t}, nil
}
