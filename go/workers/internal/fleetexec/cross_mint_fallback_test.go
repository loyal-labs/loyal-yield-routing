package fleetexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// sweepKey is a valid public key named by label.
func sweepKey(label string) string {
	hash := sha256.Sum256([]byte(label))
	return sdk.PublicKeyFromBytes(hash[:]).String()
}

func fallbackReserves(states ...fleet.ReserveState) fleet.MarketSnapshot {
	snapshot := fleet.MarketSnapshot{Slot: 1000, Reserves: map[string]fleet.ReserveState{}}
	for _, state := range states {
		snapshot.Reserves[state.Address] = state
	}
	return snapshot
}

func fallbackReserve(address, mint string, apy int64, eligible bool) fleet.ReserveState {
	return fleet.ReserveState{ReserveIdentity: fleet.ReserveIdentity{Address: address, Market: "market", Mint: mint}, Slot: 1000, LastUpdateSlot: 1000, LastUpdateStale: !eligible,
		SupplyAPYBPS: apy, TotalSupplyUSDMicros: 1_000_000_000_000, EconomicLifetimeMillis: 600_000, DataHash: strings.Repeat("a", 64)}
}

func TestCrossMintFallbackRanksSameMintEligibleReservesDeterministically(t *testing.T) {
	m := CrossMintMovement{TargetMint: fleet.USDTMint, ActiveTargetReserve: "primary", IntendedTargetReserve: "primary"}
	reserves := fallbackReserves(fallbackReserve("primary", m.TargetMint, 9999, true), fallbackReserve("wrong-mint", fleet.USDCMint, 9999, true), fallbackReserve("blocked", m.TargetMint, 9999, false), fallbackReserve("a", m.TargetMint, 400, true), fallbackReserve("z", m.TargetMint, 400, true), fallbackReserve("lower", m.TargetMint, 399, true))
	for range 3 {
		r, err := selectCrossMintFallback(reserves, m)
		if err != nil || r.Address != "z" {
			t.Fatalf("wrong fallback %s %v", r.Address, err)
		}
	}
	m.ActiveTargetReserve = "z"
	if _, err := selectCrossMintFallback(reserves, m); err == nil {
		t.Fatal("bound fallback oscillated to another reserve")
	}
	m.ActiveTargetReserve = "primary"
	if _, err := selectCrossMintFallback(fallbackReserves(), m); err == nil {
		t.Fatal("unknown reserve universe authorized fallback")
	}
}

type failingReserves struct{ err error }

func (s failingReserves) ReadReserves(context.Context) (fleet.MarketSnapshot, error) {
	return fleet.MarketSnapshot{}, s.err
}

// The active target is kept while the fresh read says it may take the
// deposit; a stale primary is not; a fallback needs finalized idle target
// custody; a read failure is an outage, never a deterministic refusal.
func TestCrossMintFallbackReadsTheTargetFromTheChain(t *testing.T) {
	anchor := int64(900)
	m := CrossMintMovement{Phase: CrossMintTargetIdle, TargetMint: fleet.USDTMint, CustodyMint: fleet.USDTMint, CustodyReconciledSlot: &anchor, ActiveTargetReserve: "primary", IntendedTargetReserve: "primary"}
	reserves := fallbackReserves(fallbackReserve("primary", m.TargetMint, 100, false), fallbackReserve("fallback", m.TargetMint, 200, true))
	if crossMintActiveTargetEligible(reserves, m) {
		t.Fatal("stale primary admitted as target")
	}
	if !crossMintActiveTargetEligible(fallbackReserves(fallbackReserve("primary", m.TargetMint, 100, true)), m) {
		t.Fatal("eligible primary was replaced")
	}
	unanchored := m
	unanchored.CustodyReconciledSlot = nil
	c := CrossMintController{}
	if err := c.rebindFallback(context.Background(), CrossMintContinuationLease{Movement: unanchored}, reserves); err == nil {
		t.Fatal("unknown custody clock admitted a fallback")
	}
	transient := errors.New("source timeout")
	if _, err := (failingReserves{transient}).ReadReserves(context.Background()); !errors.Is(err, transient) || errors.Is(err, ErrCrossMintTargetUnavailable) {
		t.Fatal("source outage became deterministic unavailable")
	}
}
