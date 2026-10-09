package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// Only a concrete deterministic target admission/policy/simulation refusal may
// return this sentinel. Transport and provider failures must retain the target.
var ErrCrossMintTargetUnavailable = errors.New("cross-mint target is deterministically unavailable")

// Configure before Run with the same source-owned market evidence used by C.
func (c *CrossMintController) SetMarketEpochSource(source fleet.MarketEpochSource) {
	c.marketEvidence = source
}

func (c *CrossMintController) loadFallbackEpoch(ctx context.Context, m CrossMintMovement) (fleet.ImmutableMarketEpoch, error) {
	if c.marketEvidence == nil {
		return fleet.ImmutableMarketEpoch{}, errors.New("target continuation requires current supported market evidence")
	}
	e, err := c.marketEvidence.LoadImmutableMarketEpoch(ctx)
	if err != nil {
		return e, err
	}
	return e, validateCrossMintFallbackEpoch(e, m, time.Now().UTC())
}

func validateCrossMintFallbackEpoch(e fleet.ImmutableMarketEpoch, m CrossMintMovement, now time.Time) error {
	if m.Phase != CrossMintTargetIdle || m.CustodyMint != m.TargetMint || m.CustodyReconciledSlot == nil || *m.CustodyReconciledSlot <= 0 {
		return errors.New("fallback requires finalized idle target custody")
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("fallback market evidence: %w", err)
	}
	if e.CapturedAt.IsZero() || e.CapturedAt.After(now) || !e.OptimizerEnvelopeExpiresAt().After(now.Add(5*time.Second)) {
		return errors.New("fallback market evidence lifetime is unavailable")
	}
	for _, coverage := range e.MintCoverage {
		if coverage.Mint == m.TargetMint && coverage.Complete && coverage.ExpiresAt != nil && coverage.ExpiresAt.After(now.Add(5*time.Second)) {
			return nil
		}
	}
	return errors.New("fallback target mint coverage is incomplete or expired")
}

func crossMintActiveTargetEligible(e fleet.ImmutableMarketEpoch, m CrossMintMovement) bool {
	for _, r := range e.Reserves {
		if r.Reserve == m.ActiveTargetReserve && r.LiquidityMint == m.TargetMint && r.TargetEligible {
			return true
		}
	}
	return false
}

// Rust cross_mint.rs:3787 chooses greatest APY, then greatest reserve string.
// Sunk swap economics are not reapplied while protecting already owned funds.
func selectCrossMintFallback(reserves []fleet.MarketEpochReserve, m CrossMintMovement) (fleet.MarketEpochReserve, error) {
	if m.ActiveTargetReserve != m.IntendedTargetReserve {
		return fleet.MarketEpochReserve{}, errors.New("bound fallback cannot rebind again")
	}
	var best fleet.MarketEpochReserve
	for _, r := range reserves {
		if r.LiquidityMint != m.TargetMint || r.Reserve == m.ActiveTargetReserve || !r.TargetEligible {
			continue
		}
		if r.Reserve == "" || r.Market == nil || *r.Market == "" || r.Slot <= 0 || r.StateSlot <= 0 || r.TotalSupplyUSDMicros < 0 || len(r.AccountDataHash) != 64 {
			return best, errors.New("eligible fallback has incomplete reserve evidence")
		}
		if best.Reserve == "" || r.SupplyAPYBPS > best.SupplyAPYBPS || r.SupplyAPYBPS == best.SupplyAPYBPS && r.Reserve > best.Reserve {
			best = r
		}
	}
	if best.Reserve == "" {
		return best, errors.New("no safe same-target-mint fallback reserve is currently eligible")
	}
	return best, nil
}

func (c *CrossMintController) rebindFallback(ctx context.Context, l CrossMintContinuationLease, e fleet.ImmutableMarketEpoch) error {
	if err := validateCrossMintFallbackEpoch(e, l.Movement, time.Now().UTC()); err != nil {
		return err
	}
	candidate, err := selectCrossMintFallback(e.Reserves, l.Movement)
	if err != nil {
		return err
	}
	if err = c.verifyFallbackBank(ctx, l.Movement, candidate); err != nil {
		return err
	}
	p, err := c.store.refreshCrossMintFallbackProjection(ctx, l.Movement, candidate)
	if err != nil {
		return err
	}
	if err = c.verifyFallbackBank(ctx, l.Movement, candidate); err != nil {
		return err
	}
	if err = validateCrossMintFallbackEpoch(e, l.Movement, time.Now().UTC()); err != nil {
		return err
	}
	_, err = c.store.RebindCrossMintFallbackCapacity(ctx, l, p)
	// Rebind invalidates the old lease. The next bounded tick must reclaim and
	// revalidate against the new durable binding before preparing or signing.
	return err
}

func (c *CrossMintController) verifyFallbackBank(ctx context.Context, m CrossMintMovement, r fleet.MarketEpochReserve) error {
	if c.accounts == nil || c.history == nil || m.CustodyObservedBalanceRaw == nil || m.CustodyReconciledSlot == nil || r.Market == nil {
		return errors.New("fallback lacks actual finalized proof owner")
	}
	floor := r.Slot
	if r.StateSlot > floor {
		floor = r.StateSlot
	}
	if *m.CustodyReconciledSlot > floor {
		floor = *m.CustodyReconciledSlot
	}
	keys := []string{r.Reserve, m.CustodyAccount}
	slot, accounts, err := fleet.ReadAccounts(ctx, c.accounts, keys, rpc.CommitmentFinalized, floor)
	if err != nil {
		return err
	}
	market, _, _, program, err := reservePostIdentity(accounts[0], m.TargetMint, m.VaultPubkey)
	canonical, tokenErr := canonicalCustodyTokenProgram(m.TargetMint)
	if err != nil || tokenErr != nil || market != *r.Market || program != canonical {
		return errors.New("fallback finalized reserve differs from supported market evidence")
	}
	if hash := sha256.Sum256(accounts[0].Data); hex.EncodeToString(hash[:]) != r.AccountDataHash {
		return errors.New("fallback finalized reserve differs from supported market evidence")
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
