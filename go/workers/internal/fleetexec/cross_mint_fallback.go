package fleetexec

import (
	"context"
	"errors"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Only a concrete deterministic target admission/policy/simulation refusal may
// return this sentinel. Transport and provider failures must retain the target.
var ErrCrossMintTargetUnavailable = errors.New("cross-mint target is deterministically unavailable")

// SetReserveSource configures the fresh reserves a target fallback is chosen
// from: the planner's catalog, read over RPC.
func (c *CrossMintController) SetReserveSource(source fleet.ReserveSource) {
	c.reserves = source
}

func crossMintActiveTargetEligible(reserves fleet.MarketSnapshot, m CrossMintMovement) bool {
	r, ok := reserves.Reserves[m.ActiveTargetReserve]
	return ok && r.Mint == m.TargetMint && r.TargetEligible()
}

// Rust cross_mint.rs:3787 chooses greatest APY, then greatest reserve string.
// Sunk swap economics are not reapplied while protecting already owned funds.
func selectCrossMintFallback(reserves fleet.MarketSnapshot, m CrossMintMovement) (fleet.ReserveState, error) {
	if m.ActiveTargetReserve != m.IntendedTargetReserve {
		return fleet.ReserveState{}, errors.New("bound fallback cannot rebind again")
	}
	var best fleet.ReserveState
	for _, r := range reserves.Reserves {
		if r.Mint != m.TargetMint || r.Address == m.ActiveTargetReserve || !r.TargetEligible() {
			continue
		}
		if best.Address == "" || r.SupplyAPYBPS > best.SupplyAPYBPS || r.SupplyAPYBPS == best.SupplyAPYBPS && r.Address > best.Address {
			best = r
		}
	}
	if best.Address == "" {
		return best, errors.New("no safe same-target-mint fallback reserve is currently eligible")
	}
	return best, nil
}

func (c *CrossMintController) rebindFallback(ctx context.Context, l CrossMintContinuationLease, reserves fleet.MarketSnapshot) error {
	m := l.Movement
	if m.Phase != CrossMintTargetIdle || m.CustodyMint != m.TargetMint || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0 {
		return errors.New("fallback requires finalized idle target custody")
	}
	candidate, err := selectCrossMintFallback(reserves, m)
	if err != nil {
		return err
	}
	if err = c.verifyFallbackBank(ctx, m, candidate); err != nil {
		return err
	}
	err = c.store.RebindCrossMintFallbackCapacity(ctx, l, CrossMintCapacityProjection{Cluster: m.Cluster, TargetReserve: candidate.Address, LiquidityMint: m.TargetMint, ObservedSupplyUSDMicros: candidate.TotalSupplyUSDMicros, ObservedSlot: candidate.Slot})
	// Rebind invalidates the old lease. The next bounded tick must reclaim and
	// revalidate against the new durable binding before preparing or signing.
	return err
}

func (c *CrossMintController) verifyFallbackBank(ctx context.Context, m CrossMintMovement, r fleet.ReserveState) error {
	if c.accounts == nil || c.history == nil || m.CustodyObservedBalanceRaw == nil || m.CustodyReconciledSlot == nil || r.Market == "" {
		return errors.New("fallback lacks actual finalized proof owner")
	}
	floor := max(r.Slot, *m.CustodyReconciledSlot)
	keys := []string{r.Address, m.CustodyAccount}
	slot, accounts, err := fleet.ReadAccounts(ctx, c.accounts, keys, rpc.CommitmentFinalized, floor)
	if err != nil {
		return err
	}
	market, _, _, program, err := reservePostIdentity(accounts[0], m.TargetMint, m.VaultPubkey)
	canonical, tokenErr := canonicalCustodyTokenProgram(m.TargetMint)
	if err != nil || tokenErr != nil || market != r.Market || program != canonical {
		return errors.New("fallback finalized reserve differs from the catalog reserve")
	}
	amount, err := custodyTokenAmount(accounts[1], m.CustodyMint, m.VaultPubkey)
	if err != nil {
		return err
	}
	ata, err := associatedCustodyAccount(m.VaultPubkey, m.TargetMint, canonical)
	if err != nil || ata != m.CustodyAccount || amount != *m.CustodyObservedBalanceRaw || amount < m.CustodyAmountRaw || m.CustodyAmountRaw <= 0 {
		return errors.New("fallback attributable custody aggregate changed")
	}
	recognized, err := c.store.crossMintRecognizedSignatures(ctx, m.DecisionID, *m.CustodyReconciledSlot)
	if err != nil {
		return err
	}
	_, err = verifyCustodyHistory(ctx, c.history, m.CustodyAccount, *m.CustodyReconciledSlot, slot, recognized, true)
	return err
}
