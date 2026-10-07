package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/ata"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/earn"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/subscription"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
)

type DurableHandler struct {
	Kamino *kamino.Handler
	ATA    *ata.Handler
	Earn   *earn.Handler
	Bridge *earn.Bridge
	Facts  *engine.Facts
	// lastSlotAt is when the stream last delivered a durable slot; zero after
	// a reconnect. The runtime restarts a session that stops delivering.
	lastSlotAt atomic.Int64
}

func (h *DurableHandler) failed(code string) { h.Facts.Failed(engine.FamilyObserver, code) }

// stalled reports whether a session that has delivered slots stopped for longer than timeout.
func (h *DurableHandler) stalled(timeout time.Duration) bool {
	last := h.lastSlotAt.Load()
	return last > 0 && time.Since(time.Unix(0, last)) > timeout
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
		if _, err := h.Kamino.HandleAccount(ctx, update); err != nil {
			h.failed("kamino_persist")
			return err
		}
		handled = true
	}
	if _, ok := filters[watch.BalanceSweepWalletATAs]; ok {
		if _, err := h.ATA.HandleAccount(ctx, update); err != nil {
			h.failed("ata_persist")
			return err
		}
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
		if _, err := h.Earn.HandleAccount(ctx, update); err != nil {
			h.failed("earn_enqueue")
			return err
		}
		handled = true
	}
	if _, ok := filters[subscription.EarnMaxPolicyTransactions]; ok {
		if err := h.Bridge.HandleTransaction(ctx, update); err != nil {
			h.failed("earn_policy_projection")
			return err
		}
		handled = true
	}
	if _, ok := filters[subscription.StreamProgress]; ok {
		if slot := update.GetSlot(); slot != nil {
			h.lastSlotAt.Store(time.Now().UnixNano())
			h.Facts.Progress(engine.FamilyObserver)
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
