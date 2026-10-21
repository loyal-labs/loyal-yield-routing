package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

type staticReserves struct{ market fleet.MarketSnapshot }

func (s staticReserves) ReadReserves(context.Context) (fleet.MarketSnapshot, error) {
	return s.market, nil
}

type fallbackFactory struct {
	calls int
	err   error
}

func (f *fallbackFactory) PrepareCrossMintLeg(context.Context, CrossMintLegRequest) (CrossMintPreparedLeg, error) {
	f.calls++
	return CrossMintPreparedLeg{}, f.err
}

// These registered source-table scenarios combine existing durable custody
// transitions with pinned bank/history fixtures. They do not execute a swap or
// prove a live policy, provider quote, or monitor capture.
func crossMintFallbackFixture(t *testing.T, primaryEligible bool) (*CrossMintController, *pgxpool.Pool, CrossMintMovement, string) {
	t.Helper()
	ctx := context.Background()
	r, pool, id := crossMintRuntimeFixture(t)
	m := runtimeFinalizeWithdrawal(t, r, pool, id)
	ata, err := associatedCustodyAccount(m.VaultPubkey, m.TargetMint, sdk.TokenProgramID.String())
	if err != nil {
		t.Fatal(err)
	}
	b := seededBaseline{Cluster: m.Cluster, DecisionID: m.DecisionID, OpportunityID: m.OpportunityID, EpochID: m.OptimizerEpochID}
	credit := int64(999000000)
	e := CrossMintExpectedEffect{Debit: &fleet.CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: m.CustodyAmountRaw}, CreditMint: crossString(m.TargetMint), CreditTokenAccount: crossString(ata), MinimumCreditAmountRaw: &credit}
	pre := fleet.CrossMintAnchors{Debit: &fleet.CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: *m.CustodyObservedBalanceRaw}, Credit: &fleet.CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: 7}}
	effect := CrossMintEffect{Debit: e.Debit, Credit: &fleet.CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: credit}}
	post := fleet.CrossMintAnchors{Debit: &fleet.CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: 50}, Credit: &fleet.CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: credit + 7}}
	l := seedCrossMintReceiptAttempt(t, ctx, pool, b, LegSwap, PurposeOptimizeYield, e, pre, 1015)
	m, err = r.store.ReconcileCrossMintLeg(ctx, l, CrossMintReconciliation{FinalizedSlot: 1015, Effect: effect, BalanceAnchors: post})
	if err != nil {
		t.Fatal(err)
	}
	fallback := sdk.PublicKey(sha256.Sum256([]byte(m.Cluster + "fallback"))).String()
	bank := postFixture(t, sameMintPostContract{vault: m.VaultPubkey, target: fallback, mint: m.TargetMint, sourceKind: "idle_balance"}, 1016, 0, credit+7)
	account := bank.accounts[fallback]
	binary.LittleEndian.PutUint64(account.Data[224:232], 1000000000000)
	bank.accounts[fallback] = account
	primary := account
	primary.Key = sdk.MustPublicKeyFromBase58(m.ActiveTargetReserve)
	primary.Data = append([]byte(nil), account.Data...)
	if !primaryEligible {
		primary.Data[24] = 1
	}
	bank.accounts[m.ActiveTargetReserve] = primary
	reserves := fleet.MarketSnapshot{Slot: 1016, Reserves: map[string]fleet.ReserveState{}}
	for i, a := range []chain.Account{primary, account} {
		market, _, _, _, err := reservePostIdentity(&a, m.TargetMint, m.VaultPubkey)
		if err != nil {
			t.Fatal(err)
		}
		eligible := i == 1 || primaryEligible
		reserves.Reserves[a.Key.String()] = fleet.ReserveState{ReserveIdentity: fleet.ReserveIdentity{Address: a.Key.String(), Market: market, Mint: m.TargetMint}, Slot: 1016, LastUpdateSlot: 1016, LastUpdateStale: !eligible,
			SupplyAPYBPS: int64(100 + i*100), TotalSupplyRaw: 1_000_000_000_000, EconomicLifetimeMillis: 600_000}
	}
	c := &CrossMintController{store: r.store, factory: &fallbackFactory{err: ErrCrossMintTargetUnavailable}, cluster: m.Cluster, owner: r.config.Owner, ttl: time.Minute, accounts: bank, history: addressHistory{pages: map[string][]chain.Signed{ata: {{Signature: sdk.MustSignatureFromBase58(l.Submission.Signature), Slot: 1015}}}}}
	c.SetReserveSource(staticReserves{reserves})
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET start_new_movements=false,generation=generation+1 WHERE cluster=$1`, m.Cluster); err != nil {
		t.Fatal(err)
	}
	return c, pool, m, fallback
}

func TestCrossMintAutomaticFallbackRebindsOnceWithStartsDisabledAndFreshFence(t *testing.T) {
	c, pool, m, fallback := crossMintFallbackFixture(t, false)
	ctx := context.Background()
	if id, worked, err := c.ContinueOne(ctx); err != nil || !worked || id != 0 {
		t.Fatalf("rebind: %d %v %v", id, worked, err)
	}
	bound, err := c.store.CrossMintMovement(ctx, m.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.ActiveTargetReserve != fallback || bound.IntendedTargetReserve != m.IntendedTargetReserve || bound.CustodyVersion != m.CustodyVersion || !sameJSON(bound.ExecutionPlan, m.ExecutionPlan) {
		t.Fatal("fallback changed immutable plan/custody or failed to bind")
	}
	if c.factory.(*fallbackFactory).calls != 0 {
		t.Fatal("ineligible primary attempted signing preparation")
	}
	// The fallback's room counts the movement's principal: it is in flight
	// into the bound target until its deposit confirms.
	inflow, _, err := fleet.InflightFlows(ctx, pool, m.Cluster, 1016, 0)
	if err != nil || inflow[fallback] != 1000000000 || inflow[m.ActiveTargetReserve] != 0 {
		t.Fatalf("bound target not in flight: %v %v", inflow, err)
	}
	// A new claim observes the bound target. Deterministic preparation failure
	// holds there rather than oscillating to another reserve.
	if _, worked, err := c.ContinueOne(ctx); !worked || err == nil || !strings.Contains(err.Error(), "cannot rebind again") {
		t.Fatalf("second fallback: %v %v", worked, err)
	}
	if c.factory.(*fallbackFactory).calls != 1 {
		t.Fatal("bound fallback did not receive independent preparation")
	}
}

func TestCrossMintFallbackTransientPreparationAndChangedCustodyRetainTheTarget(t *testing.T) {
	for _, change := range []string{"transport", "external_custody_history"} {
		t.Run(change, func(t *testing.T) {
			c, _, m, _ := crossMintFallbackFixture(t, change == "transport")
			ctx := context.Background()
			switch change {
			case "transport":
				c.factory.(*fallbackFactory).err = errors.New("provider timeout")
			case "external_custody_history":
				h := c.history.(addressHistory)
				h.pages[m.CustodyAccount] = append([]chain.Signed{signed("external", 1016)}, h.pages[m.CustodyAccount]...)
				c.history = h
			}
			if _, worked, err := c.ContinueOne(ctx); !worked || err == nil {
				t.Fatalf("unsafe fallback accepted: %v %v", worked, err)
			}
			actual, err := c.store.CrossMintMovement(ctx, m.DecisionID)
			if err != nil || actual.ActiveTargetReserve != m.ActiveTargetReserve || actual.CustodyVersion != m.CustodyVersion {
				t.Fatalf("failed proof changed movement: %+v %v", actual, err)
			}
		})
	}
}

func TestCrossMintActivationRejectsUpgradedProofGenerationAndPreservesGenerationZero(t *testing.T) {
	for _, generation := range []int64{0, 3} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			initial := int64(1)
			if generation == 0 {
				initial = 0
			}
			a, _, input := seedCrossMintActivationWithControlGeneration(t, ctx, pool, initial)
			if generation != 0 {
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET start_new_movements=false,generation=2 WHERE cluster=$1`, a.Lease.Cluster); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE loyal_yield.cross_mint_movement_controls SET start_new_movements=true,generation=3 WHERE cluster=$1`, a.Lease.Cluster); err != nil {
					t.Fatal(err)
				}
			}
			m, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
			if generation == 0 {
				if err != nil || m.DecisionID <= 0 {
					t.Fatalf("actual generation zero rejected: %+v %v", m, err)
				}
				return
			}
			if err == nil {
				t.Fatal("revoked generation one proof upgraded to reenabled authority")
			}
			var decision *int64
			if err = pool.QueryRow(ctx, `SELECT decision_id FROM loyal_yield.rebalance_opportunities o WHERE id=$1`, a.Lease.OpportunityID).Scan(&decision); err != nil || decision != nil {
				t.Fatalf("stale proof partially activated: %v %v", decision, err)
			}
		})
	}
}
