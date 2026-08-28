package worker

import (
	"context"
	"fmt"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/ata"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/observability"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/subscription"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
)

type DurableHandler struct {
	Kamino  *kamino.Handler
	ATA     *ata.Handler
	Earn    *earn.Handler
	Bridge  *earn.Bridge
	Health  *observability.Health
	Metrics *observability.Metrics
}

func (h *DurableHandler) Handle(ctx context.Context, update *pb.SubscribeUpdate) error {
	if update == nil {
		return fmt.Errorf("nil LaserStream update")
	}
	filters := make(map[string]struct{}, len(update.Filters))
	for _, filter := range update.Filters {
		filters[filter] = struct{}{}
	}
	handled := false
	if _, ok := filters[subscription.KaminoReserves]; ok {
		started := time.Now()
		outcome, err := h.Kamino.HandleAccount(ctx, update)
		h.Metrics.ObserveHandler(ctx, "kamino", time.Since(started))
		if err != nil {
			h.Metrics.RecordFailure(ctx, "kamino_persist")
			return err
		}
		h.Metrics.RecordUpdate(ctx, "kamino", subscription.KaminoReserves)
		if !outcome.Inserted && !outcome.Malformed {
			h.Metrics.RecordDuplicate(ctx, "kamino")
		}
		h.Health.DomainProgress("kamino", outcome.Slot)
		handled = true
	}
	if _, ok := filters[watch.BalanceSweepWalletATAs]; ok {
		started := time.Now()
		outcome, err := h.ATA.HandleAccount(ctx, update)
		h.Metrics.ObserveHandler(ctx, "ata", time.Since(started))
		if err != nil {
			h.Metrics.RecordFailure(ctx, "ata_persist")
			return err
		}
		h.Metrics.RecordUpdate(ctx, "ata", watch.BalanceSweepWalletATAs)
		if !outcome.Inserted {
			h.Metrics.RecordDuplicate(ctx, "ata")
		}
		h.Health.DomainProgress("ata", outcome.Slot)
		handled = true
	}
	earnAccount := false
	for filter := range filters {
		if isEarnFilter(filter) {
			earnAccount = true
			break
		}
	}
	if earnAccount {
		started := time.Now()
		outcome, err := h.Earn.HandleAccount(ctx, update)
		h.Metrics.ObserveHandler(ctx, "earn", time.Since(started))
		if err != nil {
			h.Metrics.RecordFailure(ctx, "earn_enqueue")
			return err
		}
		h.Metrics.RecordUpdate(ctx, "earn", "account")
		if outcome.InsertedJobs == 0 {
			h.Metrics.RecordDuplicate(ctx, "earn")
		}
		h.Health.DomainProgress("earn", outcome.Cursor)
		handled = true
	}
	if _, ok := filters[subscription.EarnMaxPolicyTransactions]; ok {
		started := time.Now()
		if err := h.Bridge.HandleTransaction(ctx, update); err != nil {
			h.Metrics.RecordFailure(ctx, "earn_policy_projection")
			return err
		}
		h.Metrics.ObserveHandler(ctx, "earn_policy", time.Since(started))
		h.Metrics.RecordUpdate(ctx, "earn_policy", subscription.EarnMaxPolicyTransactions)
		if transaction := update.GetTransaction(); transaction != nil {
			h.Health.DomainProgress("earn_policy", transaction.GetSlot())
		}
		handled = true
	}
	if _, ok := filters[subscription.StreamProgress]; ok {
		if slot := update.GetSlot(); slot != nil {
			h.Health.Progress(slot.GetSlot())
			h.Metrics.Frontier.Set(float64(slot.GetSlot()))
			h.Metrics.LastProgress.Set(float64(time.Now().Unix()))
			handled = true
		}
	}
	if !handled {
		return fmt.Errorf("laserStream update matched no owned domain filter: %v", update.Filters)
	}
	return nil
}
func isEarnFilter(filter string) bool {
	switch filter {
	case watch.EarnSmartAccounts, watch.EarnPolicyAccounts, watch.EarnVaultAccounts, watch.EarnIdleTokenAccounts, watch.EarnWalletTokenAccounts, watch.EarnObligations, watch.EarnAutodepositWalletATAs, watch.EarnSubscriptionAuthorities, watch.EarnRecurringDelegations, watch.EarnWallets:
		return true
	default:
		return false
	}
}
