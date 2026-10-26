package earnmax

import (
	"context"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"golang.org/x/sync/errgroup"
)

type PolicyBinding struct {
	Account string `json:"account"`
	Matches bool   `json:"matches"`
	Seed    string `json:"seed"`
}

type WithdrawalView struct {
	AmountRaw          string      `json:"amountRaw"`
	CanCancel          bool        `json:"canCancel"`
	CanClaim           bool        `json:"canClaim"`
	ClaimBlockedReason ClaimBlock  `json:"claimBlockedReason,omitempty"`
	Health             *HealthView `json:"health,omitempty"`
	ReadyBy            string      `json:"readyBy"`
	RequestID          string      `json:"requestId"`
	Status             string      `json:"status"`
}

type Summary struct {
	ObservedSlot string `json:"observedSlot"`
	// RN adapter: delete once shipped RN reads /api/earn-max/apy.
	APYHistory         []APYPoint      `json:"apyHistory" nullable:"false"`
	BalanceUsd         float64         `json:"balanceUsd"`
	ClaimAmountRaw     string          `json:"claimAmountRaw"`
	Coverage           string          `json:"coverage"`
	CurrentApyBps      *int64          `json:"currentApyBps"`
	CurrentOperationID *string         `json:"currentOperationId"`
	EarnedUsd          *float64        `json:"earnedUsd"`
	ForecastApyBps     *int64          `json:"forecastApyBps"`
	Goal               string          `json:"goal"`
	PolicyAccounts     []PolicyBinding `json:"policyAccounts" nullable:"false"`
	PolicyStatus       string          `json:"policyStatus"`
	RealizedApyBps     *int64          `json:"realizedApyBps"`
	StrategyKey        *string         `json:"strategyKey"`
	Withdrawal         *WithdrawalView `json:"withdrawal"`
}

func (r *Reader) snapshot(ctx context.Context, a Authority, now time.Time) (Position, History, error) {
	position, err := r.Position(ctx, a, now)
	if err != nil {
		return Position{}, History{}, err
	}
	history, err := r.History(ctx, a, position.ObservedSlot)
	return position, history, err
}

func (r *Reader) Summary(ctx context.Context, settings solana.PublicKey) (Summary, error) {
	a, err := AuthorityOf(settings)
	if err != nil {
		return Summary{}, err
	}
	now := r.now()
	var (
		position Position
		history  History
		apy      APY
		rate     CurrentRate
	)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() (err error) { position, history, err = r.snapshot(groupCtx, a, now); return err })
	group.Go(func() error { apy = r.APY(); return nil })
	group.Go(func() error { rate = r.currentRate(); return nil })
	if err := group.Wait(); err != nil {
		return Summary{}, err
	}
	public := publicAPY(apy, rate)
	summary := Summary{
		ObservedSlot: strconv.FormatUint(position.ObservedSlot, 10), APYHistory: rnHistory(public.History),
		BalanceUsd: float64(position.Value) / 1e6, ClaimAmountRaw: "0", Coverage: "complete", CurrentApyBps: public.CurrentApyBps,
		ForecastApyBps: public.APYBps, Goal: "active", PolicyAccounts: []PolicyBinding{}, PolicyStatus: "ready", RealizedApyBps: apy.Bps,
	}
	if !history.Complete() {
		summary.Coverage = "history_incomplete"
	}
	if e := earned(history, position); e != nil {
		usd := float64(*e) / 1e6
		summary.EarnedUsd = &usd
	}
	pending := position.Withdrawal
	if pending == nil {
		return summary, nil
	}
	blocked := position.ClaimBlock(r.now())
	view := &WithdrawalView{
		AmountRaw: strconv.FormatInt(int64(pending.Payout), 10), CanClaim: blocked == ClaimOpen,
		ReadyBy: iso(pending.WithdrawableFrom), RequestID: pending.Receipt.String(), Status: "unwinding",
	}
	switch blocked {
	case ClaimOpen:
		view.Status = "claimable"
	case ClaimNotReady:
		view.Status = "requested"
	default:
		view.ClaimBlockedReason = blocked
	}
	if blocked != ClaimOpen {
		health := r.Health(ctx, r.now())
		view.Health = &health
	}
	summary.ClaimAmountRaw, summary.Withdrawal = view.AmountRaw, view
	return summary, nil
}
