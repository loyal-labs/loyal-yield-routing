package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	sdk "github.com/solana-foundation/solana-go/v2"
)

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
	e := CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: m.CustodyAmountRaw}, CreditMint: crossString(m.TargetMint), CreditTokenAccount: crossString(ata), MinimumCreditAmountRaw: &credit}
	pre := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: *m.CustodyObservedBalanceRaw}, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: 7}}
	effect := CrossMintEffect{Debit: e.Debit, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: credit}}
	post := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: 50}, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: ata, AmountRaw: credit + 7}}
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
	var rows []fleet.MarketEpochReserve
	for i, a := range []chain.Account{primary, account} {
		market, _, _, _, err := reservePostIdentity(&a, m.TargetMint, m.VaultPubkey)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(a.Data)
		rows = append(rows, fleet.MarketEpochReserve{Reserve: a.Key.String(), Market: &market, LiquidityMint: m.TargetMint, SupplyAPYBPS: int64(100 + i*100), TargetEligible: i == 1 || primaryEligible, Slot: 1016, StateSlot: 1016, AccountDataHash: hex.EncodeToString(hash[:])})
	}
	c := &CrossMintController{store: r.store, factory: &fallbackFactory{err: ErrCrossMintTargetUnavailable}, cluster: m.Cluster, owner: r.config.Owner, ttl: time.Minute, accounts: bank, history: addressHistory{pages: map[string][]chain.Signed{ata: {{Signature: sdk.MustSignatureFromBase58(l.Submission.Signature), Slot: 1015}}}}}
	c.SetMarketEpochSource(&fallbackEpochSource{epoch: fallbackEpochFixture(t, time.Now().UTC(), rows)})
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
	var reservation string
	var reserve string
	var principal int64
	if err = pool.QueryRow(ctx, `SELECT reservation_state,target_reserve,principal_usd_micros FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1`, m.DecisionID).Scan(&reservation, &reserve, &principal); err != nil || reservation != "active" || reserve != fallback || principal != 1000000000 {
		t.Fatalf("capacity detached or economics reapplied: %s %s %d %v", reservation, reserve, principal, err)
	}
	// A new claim observes the bound target. Deterministic preparation failure
	// holds there rather than oscillating capacity to another reserve.
	if _, worked, err := c.ContinueOne(ctx); !worked || err == nil || !strings.Contains(err.Error(), "cannot rebind again") {
		t.Fatalf("second fallback: %v %v", worked, err)
	}
	if c.factory.(*fallbackFactory).calls != 1 {
		t.Fatal("bound fallback did not receive independent preparation")
	}
}

func TestCrossMintFallbackTransientPreparationAndChangedBankRetainOriginalCapacity(t *testing.T) {
	for _, change := range []string{"transport", "reserve_hash", "external_custody_history"} {
		t.Run(change, func(t *testing.T) {
			c, pool, m, _ := crossMintFallbackFixture(t, change == "transport")
			ctx := context.Background()
			switch change {
			case "transport":
				c.factory.(*fallbackFactory).err = errors.New("provider timeout")
			case "reserve_hash":
				bank := c.accounts.(fixtureAccounts)
				for k, a := range bank.accounts {
					if k != m.ActiveTargetReserve && a.Owner.String() == kamino.ProgramID.String() && len(a.Data) == 8624 {
						a.Data = append([]byte(nil), a.Data...)
						a.Data[224] ^= 1
						bank.accounts[k] = a
					}
				}
				c.accounts = bank
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
			var reserve, state string
			if err = pool.QueryRow(ctx, `SELECT target_reserve,reservation_state FROM loyal_yield.target_capacity_reservations WHERE decision_id=$1`, m.DecisionID).Scan(&reserve, &state); err != nil || reserve != m.ActiveTargetReserve || state != "active" {
				t.Fatalf("failed proof released/rebound capacity: %s %s %v", reserve, state, err)
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
			var before int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=$1`, a.Lease.OpportunityID).Scan(&before); err != nil {
				t.Fatal(err)
			}
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
			var after int64
			if err = pool.QueryRow(ctx, `SELECT decision_id,(SELECT count(*) FROM loyal_yield.target_capacity_reservations WHERE opportunity_id=o.id) FROM loyal_yield.rebalance_opportunities o WHERE id=$1`, a.Lease.OpportunityID).Scan(&decision, &after); err != nil || decision != nil || after != before {
				t.Fatalf("stale proof partially activated: %v %d %d %v", decision, before, after, err)
			}
		})
	}
}
