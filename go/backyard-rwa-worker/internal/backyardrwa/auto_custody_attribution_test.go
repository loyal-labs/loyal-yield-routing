package backyardrwa

// Behavioral tests for the durable shared-PYUSD custody attribution
// validator (auto_custody_attribution.go). Every positive fixture is built
// through the REAL ReconcileConfirmedTransaction over a receipt matching the
// built expected effects, so the journal bytes, their sha256 binding and the
// canonical account evidence are production bytes, not hand-written JSON.
// The DB-gated test proves the real PostgreSQL jsonb roundtrip: rows
// inserted through ::jsonb casts still verify against the original digests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"os"
)

// The candidate AUTO route catalog entry: its PYUSD debt custody is the
// SHARED account (AUTO, Ethena, primePRIMEPYUSD), authorized by the Kamino
// vault and owned by the Token-2022 program.
const custodyAttributionRouteKey = "auto-attribution-test"

func custodyAttributionConfig() sharedCustodyAttributionConfig {
	return autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, custodyAttributionRouteKey)
}

func custodyAttributionBalance(address, owner, mint, authority string, raw uint64) TransactionTokenBalance {
	return TransactionTokenBalance{Address: address, OwnerProgram: owner, Mint: mint, Authority: authority, Raw: raw}
}

// custodyAttributionReconcile runs the production reconciler over a receipt
// whose balances match expected exactly and returns the canonical evidence
// bytes plus their digest — the only way fixtures produce journal rows.
func custodyAttributionReconcile(t *testing.T, expected ExpectedEffects, signature string, slot int64, pre, post []TransactionTokenBalance) ([]byte, string) {
	t.Helper()
	reconciliation, evidence, err := ReconcileConfirmedTransaction(expected, ConfirmedTransactionEvidence{
		Finalized: true, Signature: signature, Slot: slot, PreTokenBalances: pre, PostTokenBalances: post,
	})
	if err != nil {
		t.Fatalf("fixture receipt failed real reconciliation: %v", err)
	}
	if reconciliation.ConfirmedSlot != slot {
		t.Fatalf("reconciler slot %d != fixture slot %d", reconciliation.ConfirmedSlot, slot)
	}
	return evidence, reconciliation.EffectsSHA256
}

// custodyAttributionParams describes one journal row; zero values mean
// "leave at the reconciled/finalized default" so mutation tests can override
// exactly one field.
type custodyAttributionParams struct {
	OperationID  string
	Action       string
	StrategyKey  string
	Signature    string
	Slot         int64
	Expected     ExpectedEffects
	Status       string
	Confirmation string
	Pre          []TransactionTokenBalance
	Post         []TransactionTokenBalance
}

func custodyAttributionRowFrom(t *testing.T, params custodyAttributionParams) custodyAttributionRow {
	t.Helper()
	evidence, digest := custodyAttributionReconcile(t, params.Expected, params.Signature, params.Slot, params.Pre, params.Post)
	expectedBytes, err := jsonMarshalExpectedEffects(params.Expected)
	if err != nil {
		t.Fatal(err)
	}
	row := custodyAttributionRow{
		OperationID: params.OperationID, StrategyKey: params.StrategyKey, RouteKey: custodyAttributionRouteKey,
		Action: params.Action, Status: params.Status, ConfirmationStatus: params.Confirmation,
		ConfirmedSlot: params.Slot, TransactionSignature: params.Signature,
		ReconciliationSHA256: digest, ReconciledEffects: evidence, ExpectedEffects: expectedBytes,
	}
	if row.Status == "" {
		row.Status = "reconciled"
	}
	if row.ConfirmationStatus == "" {
		row.ConfirmationStatus = "finalized"
	}
	if row.StrategyKey == "" {
		row.StrategyKey = autoAUTOPYUSD.Lane
	}
	return row
}

// custodyAttributionFundingExpected builds the reviewed funding swap with a
// caller-supplied collateral pre-balance so the fixture arithmetic matches
// the receipt the production reconciler is run against.
func custodyAttributionFundingExpected(collateralBefore, residueAfter uint64, minimum *uint64) ExpectedEffects {
	return ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap",
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: collateralBefore, AfterRaw: 0},
			{Address: autoAUTOPYUSD.DebtCustody, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: residueAfter, MinimumAfterRaw: minimum},
		},
	}
}

// custodyAttributionFundingRow is the reviewed funding swap: the actual
// Jupiter output (8.1 PYUSD) lands ABOVE the expected slippage minimum
// (8 PYUSD), exactly as real price improvement does.
func custodyAttributionFundingRow(t *testing.T, opID, signature string, slot int64) custodyAttributionRow {
	t.Helper()
	minimum := uint64(8_000_000_000)
	expected := custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, &minimum)
	return custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: opID, Action: string(SwapCollateralToDebtStep), Signature: signature, Slot: slot, Expected: expected,
		Pre: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 10_000_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 0),
		},
		Post: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 0),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 8_100_000_000),
		},
	})
}

func custodyAttributionRepayExpected(before, after, supplyBefore, supplyAfter uint64) ExpectedEffects {
	moved := before - after
	return ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-repay", Conserved: true,
		Repayment: &ExpectedRepayment{MinimumDebitRaw: moved, MaximumDebitRaw: moved},
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtCustody, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: before, AfterRaw: after},
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: supplyBefore, AfterRaw: supplyAfter},
		},
	}
}

func custodyAttributionRepayBalances(before, after, supplyBefore, supplyAfter uint64) ([]TransactionTokenBalance, []TransactionTokenBalance) {
	pre := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, before),
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, supplyBefore),
	}
	post := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, after),
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, supplyAfter),
	}
	return pre, post
}

// custodyAttributionRepayRow is a partial KLend repayment: 5 PYUSD leave the
// shared custody back into the debt liquidity supply.
func custodyAttributionRepayRow(t *testing.T, opID, signature string, slot int64) custodyAttributionRow {
	t.Helper()
	pre, post := custodyAttributionRepayBalances(8_100_000_000, 3_100_000_000, 1_000_000_000, 6_000_000_000)
	return custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: opID, Action: string(DeleverRouteStep), Signature: signature, Slot: slot,
		Expected: custodyAttributionRepayExpected(8_100_000_000, 3_100_000_000, 1_000_000_000, 6_000_000_000),
		Pre:      pre, Post: post,
	})
}

// custodyAttributionBorrowExpected is the exact production kaminoBorrowEffects
// shape for the open-route borrow: supply source, custody destination funded
// from zero, fee receiver — one PYUSD mint, market authority outside.
func custodyAttributionBorrowExpected() ExpectedEffects {
	return ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 10_000_000_000, AfterRaw: 5_000_000_000},
			{Address: autoAUTOPYUSD.DebtCustody, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: 4_900_000_000},
			{Address: autoAUTOPYUSD.DebtFeeReceiver, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 0, AfterRaw: 100_000_000},
		},
	}
}

func custodyAttributionBorrowBalances() ([]TransactionTokenBalance, []TransactionTokenBalance) {
	pre := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 10_000_000_000),
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 0),
		custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 0),
	}
	post := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 5_000_000_000),
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 4_900_000_000),
		custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 100_000_000),
	}
	return pre, post
}

func custodyAttributionBorrowRow(t *testing.T, opID, signature string, slot int64, action string) custodyAttributionRow {
	t.Helper()
	pre, post := custodyAttributionBorrowBalances()
	return custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: opID, Action: action, Signature: signature, Slot: slot, Expected: custodyAttributionBorrowExpected(),
		Pre: pre, Post: post,
	})
}

// custodyAttributionSpendRow is a generic custody→supply conserved transfer
// used to build long chains for the depth-bound test.
func custodyAttributionSpendRow(t *testing.T, opID, signature string, slot int64, before, after, supplyBefore, supplyAfter uint64) custodyAttributionRow {
	t.Helper()
	pre, post := custodyAttributionRepayBalances(before, after, supplyBefore, supplyAfter)
	return custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: opID, Action: string(DeleverRouteStep), Signature: signature, Slot: slot,
		Expected: custodyAttributionRepayExpected(before, after, supplyBefore, supplyAfter), Pre: pre, Post: post,
	})
}

func custodyAttributionHoldReason(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected attribution refusal, got a proof")
	}
	var hold *BudgetHold
	if errors.As(err, &hold) {
		return hold.Reason
	}
	t.Fatalf("expected typed budget hold, got %v", err)
	return ""
}

// The core positive: zero → funding swap → partial repay → residue, walked
// newest-first back to the proven zero-start funding edge. The funding's
// actual output (8.1 PYUSD) is above the expected minimum (8 PYUSD): real
// price improvement is accepted through the actual receipt, never truncated.
// A restart over freshly built rows reconstructs the identical proof.
func TestSharedCustodyAttributionProvesFundedResidueChain(t *testing.T) {
	cfg := custodyAttributionConfig()
	build := func() sharedCustodyAttributionEvidence {
		return sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		}}
	}
	first, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, build(), 0)
	if err != nil {
		t.Fatalf("valid funded residue chain refused: %v", err)
	}
	if first.ObservedRaw != 3_100_000_000 || len(first.Steps) != 2 {
		t.Fatalf("unexpected proof shape: %+v", first)
	}
	if first.Origin.Signature != "sig-fund" || first.Origin.BeforeRaw != 0 || first.Origin.AfterRaw != 8_100_000_000 {
		t.Fatalf("origin is not the zero-start funding edge: %+v", first.Origin)
	}
	// Actual swap output above minimum: the chain carries the real 8.1 PYUSD
	// receipt, not the 8.0 expected minimum.
	if first.Steps[1].AfterRaw != 8_100_000_000 {
		t.Fatalf("price improvement not carried at actual receipt units: %+v", first.Steps[1])
	}
	if first.Steps[0].Signature != "sig-repay" || first.Steps[0].BeforeRaw != 8_100_000_000 || first.Steps[0].AfterRaw != 3_100_000_000 {
		t.Fatalf("repay link does not chain funding after to residue: %+v", first.Steps[0])
	}
	restarted, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, build(), 0)
	if err != nil || !reflect.DeepEqual(first, restarted) {
		t.Fatalf("restart did not reconstruct the identical proof: %v, %+v", err, restarted)
	}
}

// The alternative zero-start edge is the real OpenRouteStep borrow in the
// exact production kaminoBorrowEffects shape (supply → custody → fee
// receiver). A borrow row whose action label is swapped to the repay action
// is NOT an origin: the bind is the action plus expected-input shape, never
// the label alone.
func TestSharedCustodyAttributionProvesBorrowOriginOnlyInProductionShape(t *testing.T) {
	cfg := custodyAttributionConfig()
	chain := func(action string) sharedCustodyAttributionEvidence {
		pre, post := custodyAttributionRepayBalances(4_900_000_000, 3_100_000_000, 1_000_000_000, 2_800_000_000)
		repay := custodyAttributionRowFrom(t, custodyAttributionParams{
			OperationID: "auto-repay", Action: string(DeleverRouteStep), Signature: "sig-repay", Slot: 200,
			Expected: custodyAttributionRepayExpected(4_900_000_000, 3_100_000_000, 1_000_000_000, 2_800_000_000),
			Pre:      pre, Post: post,
		})
		return sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{repay, custodyAttributionBorrowRow(t, "auto-borrow", "sig-borrow", 100, action)}}
	}
	proof, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, chain(string(OpenRouteStep)), 0)
	if err != nil {
		t.Fatalf("real borrow origin refused: %v", err)
	}
	if proof.Origin.Signature != "sig-borrow" || proof.Origin.BeforeRaw != 0 || proof.Origin.AfterRaw != 4_900_000_000 {
		t.Fatalf("origin is not the zero-start borrow edge: %+v", proof.Origin)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, chain(string(DeleverRouteStep)), 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_origin_unproven" {
		t.Fatalf("borrow origin proven from a swapped action label: %s", reason)
	}
	// A borrow missing its fee leg is not the reviewed shape and proves
	// nothing, even under the right action. The fixture is still a real,
	// self-consistent reconcilable row: a fee-less two-account graph
	// (supply pays 4.9, custody receives 4.9) that the production reconciler
	// accepts but the origin bind rejects.
	feelessExpected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "kamino-borrow", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 10_000_000_000, AfterRaw: 5_100_000_000},
			{Address: autoAUTOPYUSD.DebtCustody, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: bridgeVault, BeforeRaw: 0, AfterRaw: 4_900_000_000},
		}}
	feelessPre := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 10_000_000_000),
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 0),
	}
	feelessPost := []TransactionTokenBalance{
		custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 5_100_000_000),
		custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 4_900_000_000),
	}
	short := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "auto-borrow", Action: string(OpenRouteStep), Signature: "sig-borrow", Slot: 100, Expected: feelessExpected, Pre: feelessPre, Post: feelessPost,
	})
	repay := chain(string(OpenRouteStep)).Rows[0]
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{repay, short}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_origin_unproven" {
		t.Fatalf("malformed two-account borrow accepted as origin: %s", reason)
	}
}

// An old funding row cannot be reused as proof after its proceeds were
// spent: the tip's actual after must equal the fresh observed residue.
func TestSharedCustodyAttributionRejectsSpentFundingReuse(t *testing.T) {
	cfg := custodyAttributionConfig()
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_balance_mismatch" {
		t.Fatalf("spent funding reused as residue proof: %s", reason)
	}
}

// A newer foreign-lane touch of the shared custody is refused even when the
// balances happen to line up.
func TestSharedCustodyAttributionRejectsForeignNewerTouch(t *testing.T) {
	cfg := custodyAttributionConfig()
	foreign := custodyAttributionFundingRow(t, "ethena-fund", "sig-foreign", 400)
	foreign.StrategyKey = "Ethena/ETH/PYUSD"
	_, err := validateSharedCustodyAttribution(3_100_000_000, 500, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{foreign, custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_foreign_lane" {
		t.Fatalf("foreign newer touch not refused as foreign lane: %s", reason)
	}
}

// A touch of the custody ADDRESS under a different authority is an unknown
// record, never a skippable foreign account. A row whose expected effects
// intend a custody touch but whose strict receipt has no custody account is
// equally unclassifiable.
func TestSharedCustodyAttributionRejectsUnknownTopUps(t *testing.T) {
	cfg := custodyAttributionConfig()
	impostorExpected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap",
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.CollateralCustody, Owner: classicTokenProgram, Mint: autoAUTOPYUSD.Kamino.CollateralMint, Authority: bridgeVault, BeforeRaw: 1_000_000_000, AfterRaw: 0},
			{Address: autoAUTOPYUSD.DebtCustody, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 3_100_000_000, AfterRaw: 3_500_000_000},
		},
	}
	minimum := uint64(3_500_000_000)
	impostorExpected.Accounts[1].MinimumAfterRaw = &minimum
	impostor := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "impostor-topup", Action: string(SwapCollateralToDebtStep), StrategyKey: "Ethena/ETH/PYUSD",
		Signature: "sig-impostor", Slot: 300, Expected: impostorExpected,
		Pre: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 1_000_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 3_100_000_000),
		},
		Post: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 0),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 3_500_000_000),
		},
	})
	_, err := validateSharedCustodyAttribution(3_500_000_000, 400, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{impostor, custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unknown_record" {
		t.Fatalf("matching-address wrong-identity touch skipped instead of rejected: %s", reason)
	}
	// Strict receipt with NO custody account while the built expected effects
	// intend one: an unclassifiable capital movement.
	unrelatedExpected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 1_000_000_000, AfterRaw: 2_000_000_000},
			{Address: autoAUTOPYUSD.DebtFeeReceiver, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 2_000_000_000, AfterRaw: 1_000_000_000},
		},
	}
	row := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "unrelated", Action: string(DeleverRouteStep), Signature: "sig-unrelated", Slot: 250, Expected: unrelatedExpected,
		Pre:  []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000_000_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000_000_000)},
		Post: []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000_000_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000_000_000)},
	})
	// Swap the persisted expected effects for ones that DO intend the
	// custody: the strict receipt cannot classify that intention.
	withCustody, err := jsonMarshalExpectedEffects(custodyAttributionRepayExpected(3_100_000_000, 3_000_000_000, 6_100_000_000, 6_200_000_000))
	if err != nil {
		t.Fatal(err)
	}
	row.ExpectedEffects = withCustody
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{row, custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unknown_record" {
		t.Fatalf("expected-intended custody touch with absent actual receipt skipped: %s", reason)
	}
}

// Balance mismatch at the tip and a continuity gap between links are
// distinct refusals.
func TestSharedCustodyAttributionRejectsBalanceMismatchAndGap(t *testing.T) {
	cfg := custodyAttributionConfig()
	chain := sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{
		custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
		custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
	}}
	_, err := validateSharedCustodyAttribution(3_200_000_000, 300, cfg, chain, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_balance_mismatch" {
		t.Fatalf("observed drift from chain tip accepted: %s", reason)
	}
	// A funding edge that actually delivered 8.0 (not 8.1) leaves a gap
	// against the repay's recorded before.
	minimum := uint64(8_000_000_000)
	weaker := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "auto-fund", Action: string(SwapCollateralToDebtStep), Signature: "sig-fund-alt", Slot: 100,
		Expected: custodyAttributionFundingExpected(10_000_000_000, 8_000_000_000, &minimum),
		Pre: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 10_000_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 0),
		},
		Post: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 0),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 8_000_000_000),
		},
	})
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{chain.Rows[0], weaker},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_gap" {
		t.Fatalf("chain continuity gap accepted: %s", reason)
	}
}

// Unfinalized states refuse closed: a nonterminal tip may still land, an
// older nonterminal row may still move the custody, and a route-wide
// unresolved operation (which may carry NO custody evidence at all) fails
// the route regardless of the chain.
func TestSharedCustodyAttributionRejectsUnfinalizedAndRouteWideConflicts(t *testing.T) {
	cfg := custodyAttributionConfig()
	tip := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	tip.Status, tip.ConfirmationStatus = "confirmed", "confirmed"
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{tip, custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unresolved_tip" {
		t.Fatalf("nonterminal tip accepted: %s", reason)
	}
	older := custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)
	older.Status = "signed"
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), older},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unresolved_operation" {
		t.Fatalf("older nonterminal row accepted: %s", reason)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows:       []custodyAttributionRow{custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200), custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)},
		Unresolved: true,
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unresolved_operation" {
		t.Fatalf("route-wide unresolved operation without evidence ignored: %s", reason)
	}
}

// Receipt identity is bound to the journal record (signature AND slot), the
// stored digest is bound to the canonical bytes, and the chain may never
// reach past the fresh observation slot.
func TestSharedCustodyAttributionRejectsMutatedEvidenceAndSnapshotDrift(t *testing.T) {
	cfg := custodyAttributionConfig()
	mutated := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	funding := custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)
	mutated.ReconciledEffects = append([]byte(nil), mutated.ReconciledEffects...)
	// Flip the final byte of the canonical source tag: parse-safe, so the
	// stored-digest check is what refuses (flipping the closing brace would
	// trip JSON parsing first — the malformed path covered separately).
	mutated.ReconciledEffects[len(mutated.ReconciledEffects)-3] ^= 0x20
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{mutated, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_hash_mismatch" {
		t.Fatalf("mutated evidence accepted: %s", reason)
	}
	signatureDrift := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	signatureDrift.TransactionSignature = "sig-fund"
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{signatureDrift, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_identity_mismatch" {
		t.Fatalf("signature drift accepted: %s", reason)
	}
	slotDrift := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	slotDrift.ConfirmedSlot = 201
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{slotDrift, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_identity_mismatch" {
		t.Fatalf("slot drift accepted: %s", reason)
	}
	// A custody touch recorded ABOVE the fresh observation slot means the
	// observed balance cannot describe the chain tip, whatever the numbers.
	future := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 250)
	_, err = validateSharedCustodyAttribution(3_100_000_000, 200, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{future, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_snapshot_drift" {
		t.Fatalf("journal touch newer than the snapshot accepted: %s", reason)
	}
}

// Malformed canonical accounts (truncated JSON, NULL receipt, duplicate
// address, wrong field count, non-canonical slot encoding), same-slot
// ambiguity and depth exhaustion all refuse closed.
func TestSharedCustodyAttributionRejectsMalformedAmbiguousAndExhausted(t *testing.T) {
	cfg := custodyAttributionConfig()
	funding := custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100)
	truncated := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	truncated.ReconciledEffects = []byte(`{"schema":"loyal-backyard-rwa-reconciled-effects/v1"`)
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{truncated, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("truncated receipt JSON accepted: %s", reason)
	}
	missing := custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200)
	missing.ReconciledEffects = nil
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{missing, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("finalized row with a missing receipt accepted: %s", reason)
	}
	// Hand-built canonical evidence exercising parse strictness. The rows are
	// marshaled exactly the way reconcile.go writes them (sorted map keys,
	// integer slot) so the digest reconstruction succeeds and the PARSE is
	// what refuses.
	handRow := func(accounts []string, slotToken string) custodyAttributionRow {
		t.Helper()
		body := fmt.Sprintf(`{"accounts":[%s],"schema":%q,"signature":"sig-hand","slot":%s,"source":%q}`,
			strings.Join(quoteAll(t, accounts), ","), reconciledEffectsSchema, slotToken, reconciledEffectsSource)
		// A fixture whose encoding the reconstruction already refuses (e.g. a
		// non-canonical slot) still needs a row; the validator must reject it
		// during reconstruction before any digest comparison.
		digest := ""
		if canonical, err := reconstructReconciledEvidence([]byte(body)); err == nil {
			digest = sha256Bytes(canonical)
		}
		return custodyAttributionRow{
			OperationID: "hand", StrategyKey: cfg.Lane, RouteKey: cfg.RouteKey, Action: string(DeleverRouteStep),
			Status: "reconciled", ConfirmationStatus: "finalized", ConfirmedSlot: 200, TransactionSignature: "sig-hand",
			ReconciliationSHA256: digest, ReconciledEffects: []byte(body),
		}
	}
	duplicated := handRow([]string{
		fmt.Sprintf("%s:%s:%s:%s:1:2", autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault),
		fmt.Sprintf("%s:%s:%s:%s:2:3", autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault),
	}, "200")
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{duplicated, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("duplicate custody address accepted: %s", reason)
	}
	shortFields := handRow([]string{fmt.Sprintf("%s:%s:%s", autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint)}, "200")
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{shortFields, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("short canonical account accepted: %s", reason)
	}
	nonCanonical := handRow([]string{
		fmt.Sprintf("%s:%s:%s:%s:1:2", autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault),
	}, "2e2")
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{nonCanonical, funding}}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("non-canonical slot encoding accepted: %s", reason)
	}
	// Two chain touches inside one slot have no provable order.
	top := custodyAttributionSpendRow(t, "auto-spend-b", "sig-spend-b", 200, 3_100_000_000, 3_000_000_000, 6_100_000_000, 6_200_000_000)
	bottom := custodyAttributionSpendRow(t, "auto-spend-a", "sig-spend-a", 200, 3_200_000_000, 3_100_000_000, 6_000_000_000, 6_100_000_000)
	_, err = validateSharedCustodyAttribution(3_000_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{top, bottom, funding},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_ambiguous_order" {
		t.Fatalf("same-slot chain accepted: %s", reason)
	}
	// Sixteen links walk; the seventeenth (a fully consistent zero-start
	// funding edge exactly one link beyond the bound) refuses instead of
	// passing — continuity alone never satisfies the walk.
	rows := make([]custodyAttributionRow, 0, 17)
	rows = append(rows, custodyAttributionSpendRow(t, "chain-16", "sig-16", 500, 2_000_000, 1_000_000, 0, 1_000_000))
	for i := 15; i >= 1; i-- {
		rows = append(rows, custodyAttributionSpendRow(t, fmt.Sprintf("chain-%d", i), fmt.Sprintf("sig-%d", i), int64(100+i*10), uint64(18-i)*1_000_000, uint64(17-i)*1_000_000, uint64(i)*1_000_000, uint64(i+1)*1_000_000))
	}
	minimum := uint64(16_000_000)
	origin := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "auto-fund", Action: string(SwapCollateralToDebtStep), Signature: "sig-fund", Slot: 100,
		Expected: custodyAttributionFundingExpected(16_000_000, 16_000_000, &minimum),
		Pre: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 16_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 0),
		},
		Post: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.CollateralCustody, classicTokenProgram, autoAUTOPYUSD.Kamino.CollateralMint, bridgeVault, 0),
			custodyAttributionBalance(autoAUTOPYUSD.DebtCustody, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, bridgeVault, 16_000_000),
		},
	})
	rows = append(rows, origin)
	_, err = validateSharedCustodyAttribution(1_000_000, 600, cfg, sharedCustodyAttributionEvidence{Rows: rows}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_bound_exhausted" {
		t.Fatalf("chain beyond the depth bound accepted: %s", reason)
	}
}

// custodyAttributionJournalOnlyRow builds a reconciled, finalized journal
// row whose canonical receipt is an arbitrary hand-written envelope (its
// digest must be supplied consistently) that names no custody account —
// the shape of a corrupt non-custody record only an unfiltered window
// can surface.
func custodyAttributionJournalOnlyRow(opID, signature string, slot int64, body, digest string) custodyAttributionRow {
	return custodyAttributionRow{
		OperationID: opID, StrategyKey: autoAUTOPYUSD.Lane, RouteKey: custodyAttributionRouteKey,
		Action: "REPORT_NAV", Status: "reconciled", ConfirmationStatus: "finalized",
		ConfirmedSlot: slot, TransactionSignature: signature,
		ReconciliationSHA256: digest, ReconciledEffects: []byte(body),
	}
}

// A corrupt reconciled record whose canonical evidence names no custody
// account must refuse even when it sorts BETWEEN the tip and the origin of
// an otherwise provable chain: the unfiltered window carries it and the
// strict per-row checks run at every position.
func TestSharedCustodyAttributionRejectsMalformedInterveningNoCustodyRow(t *testing.T) {
	cfg := custodyAttributionConfig()
	corrupt := custodyAttributionJournalOnlyRow("nav-report", "sig-nav", 150, `{}`, sha256Bytes([]byte(`{}`)))
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			corrupt,
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("malformed intervening no-custody row skipped: %s", reason)
	}
}

// A row whose built expected effects cannot be DECODED must never be
// classified inert just because its strict actual receipt names no custody
// account: the custody intent of the built operation is unreadable, so the
// proof refuses instead of silently skipping the row.
func TestSharedCustodyAttributionRejectsUnreadableExpectedEffectsAsInert(t *testing.T) {
	cfg := custodyAttributionConfig()
	unrelatedExpected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 1_000_000_000, AfterRaw: 2_000_000_000},
			{Address: autoAUTOPYUSD.DebtFeeReceiver, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 2_000_000_000, AfterRaw: 1_000_000_000},
		},
	}
	row := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "unrelated", Action: string(DeleverRouteStep), Signature: "sig-unrelated", Slot: 250, Expected: unrelatedExpected,
		Pre:  []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000_000_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000_000_000)},
		Post: []TransactionTokenBalance{custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 2_000_000_000), custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 1_000_000_000)},
	})
	// Replace the readable expected effects with a truncated envelope: the
	// strict receipt is unchanged, so the inert-skip path is exactly what
	// would swallow the row if the decode error were treated as "no touch".
	row.ExpectedEffects = []byte(`{"schema":"loyal-backyard-rwa-operation-evidence/v1"`)
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{
			row,
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("unreadable expected effects deemed inert: %s", reason)
	}
}

// A continuous chain that never reaches a proven zero-start edge is a
// refusal even though every link is internally consistent: continuity alone
// never proves ownership, and the completeness boundary is the origin, not
// the window bottom.
func TestSharedCustodyAttributionRejectsConsistentChainWithoutOrigin(t *testing.T) {
	cfg := custodyAttributionConfig()
	_, err := validateSharedCustodyAttribution(3_000_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{
			custodyAttributionSpendRow(t, "auto-spend-b", "sig-spend-b", 200, 3_100_000_000, 3_000_000_000, 6_100_000_000, 6_200_000_000),
			custodyAttributionSpendRow(t, "auto-spend-a", "sig-spend-a", 150, 3_200_000_000, 3_100_000_000, 6_000_000_000, 6_100_000_000),
		},
	}, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_origin_unproven" {
		t.Fatalf("consistent chain without a zero-start edge accepted: %s", reason)
	}
}

// Strict history is only required THROUGH the proven zero-start edge: at the
// edge's actual before==0 the walk STOPS, so what sorts behind it — a prior
// FOREIGN-LANE lifecycle on the shared account, inert report rows, and the
// route's own kamino-initialize record — is never parsed by token-account
// strictness. What keeps behind-origin records closed is the reader's
// route-wide gates: the malformed-ordering-identity flag holds for direct
// callers exactly as the reader's EXISTS holds for the database.
func TestSharedCustodyAttributionAcceptsPriorLifecycleOnlyBehindProvenOrigin(t *testing.T) {
	cfg := custodyAttributionConfig()
	foreignPrior := custodyAttributionFundingRow(t, "ethena-prior", "sig-prior", 50)
	foreignPrior.StrategyKey = "Ethena/ETH/PYUSD"
	inertPrior := custodyAttributionRowFrom(t, custodyAttributionParams{
		OperationID: "nav-prior", Action: "REPORT_NAV", Signature: "sig-nav-prior", Slot: 60,
		Expected: ExpectedEffects{
			Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true,
			Accounts: []ExpectedAccountEffect{
				{Address: autoAUTOPYUSD.DebtLiquiditySupply, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 500_000_000, AfterRaw: 400_000_000},
				{Address: autoAUTOPYUSD.DebtFeeReceiver, Owner: token2022Program, Mint: autoAUTOPYUSD.Kamino.DebtMint, Authority: autoAUTOPYUSD.Kamino.MarketAuthority, BeforeRaw: 400_000_000, AfterRaw: 500_000_000},
			},
		},
		Pre: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 500_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 400_000_000),
		},
		Post: []TransactionTokenBalance{
			custodyAttributionBalance(autoAUTOPYUSD.DebtLiquiditySupply, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 400_000_000),
			custodyAttributionBalance(autoAUTOPYUSD.DebtFeeReceiver, token2022Program, autoAUTOPYUSD.Kamino.DebtMint, autoAUTOPYUSD.Kamino.MarketAuthority, 500_000_000),
		},
	})
	proof, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
			foreignPrior,
			inertPrior,
		},
	}, 0)
	if err != nil {
		t.Fatalf("prior shared-account lifecycle behind a proven zero-start edge refused: %v", err)
	}
	if proof.Origin.Signature != "sig-fund" || len(proof.Steps) != 2 {
		t.Fatalf("prior lifecycle leaked into the proven segment: %+v", proof)
	}
	// Behind-origin records are not silently drop-capable for direct
	// callers: the route-wide malformed-ordering-identity flag holds the
	// same way the reader's EXISTS holds a NULL-slot reconciled row that
	// sorts behind the origin or beyond the window bound.
	gated := sharedCustodyAttributionEvidence{
		Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		},
		MalformedIdentity: true,
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, cfg, gated, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_identity" {
		t.Fatalf("malformed ordering identity gate ignored: %s", reason)
	}
}

func quoteAll(t *testing.T, values []string) []string {
	t.Helper()
	encoded := make([]string, 0, len(values))
	for _, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, string(data))
	}
	return encoded
}

// An unrecognized terminal/manual-recovery state has unproven provenance:
// the reconciled account list is only the expected-effects projection, so no
// content classification proves such a record never moved the shared
// custody. The route-wide unknown-state precheck holds it unconditionally —
// however it sorts (NULL slots sort last, behind a proven zero-start origin)
// and however the window bound truncates. The walk itself stops at the
// proven origin and never parses what sorts behind it, so this gate is what
// keeps behind-origin records closed.
func TestSharedCustodyAttributionRejectsUnknownStateRegardlessOfChronology(t *testing.T) {
	cfg := custodyAttributionConfig()
	chain := func() sharedCustodyAttributionEvidence {
		return sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		}}
	}
	if _, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, chain(), 0); err != nil {
		t.Fatalf("pristine funded chain refused: %v", err)
	}
	// The manual-recovery record as the journal holds it: NULL slot, NULL
	// receipts, empty expected effects — surfaced by the independent
	// unknown-state precheck however it sorts.
	blocked := chain()
	blocked.Unknown = true
	_, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, blocked, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unknown_record" {
		t.Fatalf("manual-recovery precheck record ignored: %s", reason)
	}
}

// The reader is scoped to the worker Database's OWN current lease: no
// current lease, or a foreign lease identity, refuses before any SQL runs.
func TestSharedCustodyAttributionReaderRequiresWorkerLease(t *testing.T) {
	reader := &Database{}
	_, err := reader.observeSharedCustodyAttributionEvidence(context.Background(), RouteLease{RouteKey: custodyAttributionRouteKey, Owner: "worker", FencingToken: 1}, custodyAttributionConfig(), 0, nil)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_lease_unavailable" {
		t.Fatalf("reader ran without a current worker lease: %s", reason)
	}
}

// custodyAttributionSchema creates the disposable test schema the reader
// queries: the same journal shape production uses, without any store.go
// migration.
func custodyAttributionSchema(ctx context.Context, t *testing.T, db *Database) {
	t.Helper()
	_, err := db.pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS loyal_yield;
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_route_states (
	 route_key text PRIMARY KEY,state_version bigint NOT NULL DEFAULT 1,state jsonb NOT NULL,
	 lease_owner text,lease_expires_at timestamptz,fencing_token bigint NOT NULL DEFAULT 0,
	 updated_at timestamptz NOT NULL DEFAULT now(),
	 CHECK ((state->>'generation')::bigint=state_version),
	 CHECK ((lease_owner IS NULL)=(lease_expires_at IS NULL)));
	CREATE TABLE IF NOT EXISTS loyal_yield.multiply_operations (
	 operation_id text PRIMARY KEY,route_key text NOT NULL REFERENCES loyal_yield.multiply_route_states,
	 status text NOT NULL,expected_effects jsonb NOT NULL,signed_wire bytea,broadcast_intent_at timestamptz,
	 updated_at timestamptz NOT NULL DEFAULT now());
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS signed_wire bytea;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS signed_wire_sha256 text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS recovery_reason text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS message_sha256 text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS recent_blockhash text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS last_valid_block_height bigint;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS simulation_slot bigint;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS action text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS strategy_key text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS transaction_signature text,
	 ADD COLUMN IF NOT EXISTS confirmed_slot bigint,ADD COLUMN IF NOT EXISTS confirmation_status text,
	 ADD COLUMN IF NOT EXISTS reconciliation_sha256 text,ADD COLUMN IF NOT EXISTS reconciled_effects jsonb;
	-- Columns the REAL decision persistence (recordDecisionTx/OperationInsert)
	-- writes, so the lifecycle test records decisions through the production
	-- store instead of a fabricated insert.
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS cycle bigint NOT NULL DEFAULT 1;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS engine_version text NOT NULL DEFAULT 'backyard_rwa_v1';
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS idempotency_key text;
	ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT now();
	-- No unique index on idempotency_key: this table is shared with other
	-- suites whose legacy rows carry '' keys, and recordDecisionTx's dedupe
	-- is the SELECT before the insert, not the index.
	CREATE UNIQUE INDEX IF NOT EXISTS multiply_operations_one_nonterminal_per_route
	 ON loyal_yield.multiply_operations(route_key) WHERE status IN ('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling');`)
	if err != nil {
		t.Fatal(err)
	}
}

// Real PostgreSQL roundtrip: rows inserted through ::jsonb casts (text
// formatting differs from json.Marshal bytes) still verify against the
// ORIGINAL digests; the unfiltered window carries every reconciled row
// regardless of any custody substring; foreign-lane, manual-recovery, and
// route-wide unresolved rows (NULL slot, NULL evidence, no custody
// substring) fail closed; and a corrupt reconciled record naming no custody
// account is surfaced by the same unfiltered window and refused.
func TestSharedCustodyAttributionDatabaseRoundtrip(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	routeKey := fmt.Sprintf("auto-attribution-%d", time.Now().UnixNano())
	// Exactly this test's own route keys (the tail-window block below appends
	// its second key); the pool closes INSIDE the cleanup callback after the
	// checked row deletes (a deferred Close would run before t.Cleanup and
	// strand the cleanup SQL on a closed pool).
	ownedKeys := []string{routeKey}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, owned := range ownedKeys {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, owned); err != nil {
				t.Errorf("cleanup operations for %s: %v", owned, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, owned); err != nil {
				t.Errorf("cleanup route state for %s: %v", owned, err)
			}
		}
		db.Close()
	})
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, routeKey)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, routeKey); err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireRouteLease(ctx, routeKey, "attribution-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(row custodyAttributionRow) {
		t.Helper()
		_, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
			(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects)
			VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11::jsonb)`,
			row.OperationID, row.RouteKey, row.Status, row.Action, row.StrategyKey, row.ExpectedEffects,
			row.TransactionSignature, row.ConfirmedSlot, row.ConfirmationStatus, row.ReconciliationSHA256, row.ReconciledEffects)
		if err != nil {
			t.Fatal(err)
		}
	}
	funding := custodyAttributionFundingRow(t, routeKey+"-fund", "sig-fund", 100)
	repay := custodyAttributionRepayRow(t, routeKey+"-repay", "sig-repay", 200)
	funding.RouteKey, repay.RouteKey = routeKey, routeKey
	funding.StrategyKey, repay.StrategyKey = cfg.Lane, cfg.Lane
	insert(repay)
	insert(funding)
	evidence, err := db.observeSharedCustodyAttributionEvidence(ctx, lease, cfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Rows) != 2 || evidence.Unresolved || evidence.Unknown {
		t.Fatalf("unexpected read bundle: %d rows, unresolved=%v, unknown=%v", len(evidence.Rows), evidence.Unresolved, evidence.Unknown)
	}
	proof, err := validateSharedCustodyAttribution(3_100_000_000, 300, cfg, evidence, 0)
	if err != nil {
		t.Fatalf("jsonb roundtrip digest verification failed: %v", err)
	}
	if proof.Origin.Signature != "sig-fund" || len(proof.Steps) != 2 {
		t.Fatalf("unexpected roundtrip proof: %+v", proof)
	}
	// A foreign lease identity is refused before any read.
	_, err = db.observeSharedCustodyAttributionEvidence(ctx, RouteLease{RouteKey: routeKey, Owner: "other", FencingToken: 9}, cfg, 0, nil)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_lease_unavailable" {
		t.Fatalf("foreign lease identity accepted: %s", reason)
	}
	// A newer foreign-lane touch is refused even through the database read:
	// it is the newest participant, so the lane gate fires before any older
	// record is consulted.
	foreign := custodyAttributionFundingRow(t, routeKey+"-foreign", "sig-foreign", 400)
	foreign.RouteKey, foreign.StrategyKey = routeKey, "Ethena/ETH/PYUSD"
	insert(foreign)
	evidence, err = db.observeSharedCustodyAttributionEvidence(ctx, lease, cfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 500, cfg, evidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_foreign_lane" {
		t.Fatalf("foreign newer touch not refused through the database read: %s", reason)
	}
	// Root regression, against the REAL database: a manual-recovery record
	// with a NULL slot, NULL receipts, and no custody substring sorts LAST in
	// the journal composite order — behind the proven zero-start origin — and
	// could be truncated by the candidate limit. The chronology-independent
	// unknown-state precheck must still fail the route closed.
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,expected_effects) VALUES($1,$2,'manual_recovery','{}')`, routeKey+"-manual", routeKey); err != nil {
		t.Fatal(err)
	}
	evidence, err = db.observeSharedCustodyAttributionEvidence(ctx, lease, cfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Unknown {
		t.Fatalf("manual_recovery record not raised by the unknown-state precheck: %+v", evidence)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 500, cfg, evidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unknown_record" {
		t.Fatalf("manual_recovery record with NULL slot and NULL receipts not held: %s", reason)
	}
	// A route-wide unresolved operation (status 'built', NULL reconciled
	// evidence, expected effects that do not name the custody) fails the
	// route via the separate unresolved query.
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,strategy_key,expected_effects,confirmed_slot) VALUES($1,$2,'built',$3,'{}',450)`, routeKey+"-pending", routeKey, cfg.Lane); err != nil {
		t.Fatal(err)
	}
	evidence, err = db.observeSharedCustodyAttributionEvidence(ctx, lease, cfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Unresolved {
		t.Fatalf("unresolved operation not raised by the unresolved precheck: %+v", evidence)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 500, cfg, evidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unresolved_operation" {
		t.Fatalf("route-wide unresolved row ignored: %s", reason)
	}
	// A corrupt reconciled record whose canonical evidence names no custody
	// substring is carried by the SAME unfiltered window as every other
	// reconciled row (no substring filter, no separate sample) and is forced
	// through strict digest/parse classification, failing closed. Fresh
	// route key so no earlier conflict masks the window check.
	tailKey := fmt.Sprintf("auto-attribution-tail-%d", time.Now().UnixNano())
	ownedKeys = append(ownedKeys, tailKey)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, tailKey); err != nil {
		t.Fatal(err)
	}
	tailLease, err := db.AcquireRouteLease(ctx, tailKey, "attribution-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tailCfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, tailKey)
	corruptDigest := sha256Bytes([]byte(`{}`))
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects)
		VALUES($1,$2,'reconciled','REPORT_NAV',$3,'{}','sig-corrupt',150,'finalized',$4,'{}')`,
		tailKey+"-corrupt", tailKey, tailCfg.Lane, corruptDigest); err != nil {
		t.Fatal(err)
	}
	tailEvidence, err := db.observeSharedCustodyAttributionEvidence(ctx, tailLease, tailCfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tailEvidence.Rows) != 1 || tailEvidence.Unresolved || tailEvidence.Unknown {
		t.Fatalf("unfiltered window did not carry the corrupt record: %+v", tailEvidence)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, tailCfg, tailEvidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_record" {
		t.Fatalf("corrupt reconciled record without custody substring deemed inert: %s", reason)
	}
}

// Real-database lifecycle facts, against the ACTUAL production journal paths:
// the route's own reconciled kamino-initialize row (native-balance evidence
// with no token accounts, built by the real reconciler) sorts behind the
// funding origin and must not block a proof, because strict history is only
// required THROUGH the proven zero-start edge; a reconciled row with a NULL
// confirmed slot beyond the window bound is held by the independent
// malformed-identity gate instead of hiding behind the origin or the LIMIT;
// a failed row written by the actual MarkPreBroadcastFailed path (provably
// never signed, never broadcast) does not hold the route, while an ambiguous
// failed row that still carries a signature does.
func TestSharedCustodyAttributionDatabaseLifecycleGates(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid local test database config")
	}
	if !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	custodyAttributionSchema(ctx, t, db)
	// Exactly the route keys openRoute creates — never a prefix wildcard —
	// with every delete error checked, and the pool closed INSIDE the
	// callback after the deletes (a deferred Close would run before
	// t.Cleanup and strand the cleanup SQL on a closed pool).
	var ownedKeys []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for _, routeKey := range ownedKeys {
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_operations WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup operations for %s: %v", routeKey, err)
			}
			if _, err := db.pool.Exec(cleanupCtx, `DELETE FROM loyal_yield.multiply_route_states WHERE route_key = $1`, routeKey); err != nil {
				t.Errorf("cleanup route state for %s: %v", routeKey, err)
			}
		}
		db.Close()
	})
	openRoute := func(prefix string) (string, *Database, sharedCustodyAttributionConfig, RouteLease, func(custodyAttributionRow)) {
		t.Helper()
		key := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
		ownedKeys = append(ownedKeys, key)
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1}')`, key); err != nil {
			t.Fatal(err)
		}
		lease, err := db.AcquireRouteLease(ctx, key, "attribution-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		routeCfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key)
		insert := func(row custodyAttributionRow) {
			t.Helper()
			_, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
				(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects)
				VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11::jsonb)`,
				row.OperationID, row.RouteKey, row.Status, row.Action, row.StrategyKey, row.ExpectedEffects,
				row.TransactionSignature, row.ConfirmedSlot, row.ConfirmationStatus, row.ReconciliationSHA256, row.ReconciledEffects)
			if err != nil {
				t.Fatal(err)
			}
		}
		return key, db, routeCfg, lease, insert
	}

	// (1) A real reconciled initializer behind the funding origin is in the
	// unfiltered window and is never parsed: the walk stops at the proven
	// zero-start edge, so the route's oldest native-balance record cannot
	// hold the lane's proof.
	key, _, initCfg, lease, insert := openRoute("auto-attribution-init")
	initializerExpected, initializerReceipt := initializationReconcileFixture(t)
	initializerReconciliation, initializerEvidence, err := ReconcileConfirmedTransaction(initializerExpected, initializerReceipt)
	if err != nil {
		t.Fatalf("initializer fixture failed real reconciliation: %v", err)
	}
	initializerBytes, err := jsonMarshalExpectedEffects(initializerExpected)
	if err != nil {
		t.Fatal(err)
	}
	insert(custodyAttributionRow{
		OperationID: key + "-init", StrategyKey: initializerExpected.Initialization.RouteLane, RouteKey: key,
		Action: string(InitializeKaminoObligation), Status: "reconciled", ConfirmationStatus: "finalized",
		ConfirmedSlot: initializerReconciliation.ConfirmedSlot, TransactionSignature: initializerReceipt.Signature,
		ReconciliationSHA256: initializerReconciliation.EffectsSHA256, ReconciledEffects: initializerEvidence,
		ExpectedEffects: initializerBytes,
	})
	funding := custodyAttributionFundingRow(t, key+"-fund", "sig-fund", 100)
	repay := custodyAttributionRepayRow(t, key+"-repay", "sig-repay", 200)
	funding.RouteKey, repay.RouteKey, funding.StrategyKey, repay.StrategyKey = key, key, initCfg.Lane, initCfg.Lane
	insert(repay)
	insert(funding)
	evidence, err := db.observeSharedCustodyAttributionEvidence(ctx, lease, initCfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Rows) != 3 || evidence.Unresolved || evidence.Unknown || evidence.MalformedIdentity {
		t.Fatalf("initializer row missing from the unfiltered window: %d rows, unresolved=%v unknown=%v malformedIdentity=%v",
			len(evidence.Rows), evidence.Unresolved, evidence.Unknown, evidence.MalformedIdentity)
	}
	proof, err := validateSharedCustodyAttribution(3_100_000_000, 300, initCfg, evidence, 0)
	if err != nil {
		t.Fatalf("real reconciled initializer behind the proven origin held the proof: %v", err)
	}
	if proof.Origin.Signature != "sig-fund" || len(proof.Steps) != 2 {
		t.Fatalf("unexpected proof with initializer history: %+v", proof)
	}

	// (2) A reconciled row with a NULL confirmed slot sorts LAST: with a full
	// bounded window it sits beyond the LIMIT, where the walk could never
	// reach it. The route-wide malformed-identity gate holds it anyway.
	slotKey, _, slotCfg, slotLease, insert := openRoute("auto-attribution-nullslot")
	for i := 0; i < 4; i++ {
		row := custodyAttributionSpendRow(t, fmt.Sprintf("%s-spend-%d", slotKey, i), fmt.Sprintf("sig-slot-%d", i), int64(400-i*100),
			4_000_000, 3_000_000, 6_000_000, 7_000_000)
		row.RouteKey, row.StrategyKey = slotKey, slotCfg.Lane
		insert(row)
	}
	slotEvidence, err := db.observeSharedCustodyAttributionEvidence(ctx, slotLease, slotCfg, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(slotEvidence.Rows) != 4 || slotEvidence.MalformedIdentity {
		t.Fatalf("bounded window misread before the NULL-slot row: %d rows, malformedIdentity=%v", len(slotEvidence.Rows), slotEvidence.MalformedIdentity)
	}
	nullSlot := custodyAttributionFundingRow(t, slotKey+"-nullslot", "sig-nullslot", 77)
	nullSlot.RouteKey, nullSlot.StrategyKey = slotKey, slotCfg.Lane
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,confirmed_slot,confirmation_status,reconciliation_sha256,reconciled_effects)
		VALUES($1,$2,'reconciled',$3,$4,$5::jsonb,$6,NULL,'finalized',$7,$8::jsonb)`,
		nullSlot.OperationID, nullSlot.RouteKey, nullSlot.Action, nullSlot.StrategyKey, nullSlot.ExpectedEffects,
		nullSlot.TransactionSignature, nullSlot.ReconciliationSHA256, nullSlot.ReconciledEffects); err != nil {
		t.Fatal(err)
	}
	slotEvidence, err = db.observeSharedCustodyAttributionEvidence(ctx, slotLease, slotCfg, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(slotEvidence.Rows) != 4 || !slotEvidence.MalformedIdentity {
		t.Fatalf("NULL-slot reconciled row beyond the window limit not gated: %d rows, malformedIdentity=%v", len(slotEvidence.Rows), slotEvidence.MalformedIdentity)
	}
	_, err = validateSharedCustodyAttribution(3_000_000, 500, slotCfg, slotEvidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_malformed_identity" {
		t.Fatalf("NULL-slot reconciled row beyond the window limit not held: %s", reason)
	}

	// (3) The actual MarkPreBroadcastFailed path — a decided/built operation
	// that provably never signed and never broadcast — must not hold the
	// route forever.
	failKey, _, failCfg, failLease, insert := openRoute("auto-attribution-presend")
	presellFunding := custodyAttributionFundingRow(t, failKey+"-fund", "sig-fund", 100)
	presendRepay := custodyAttributionRepayRow(t, failKey+"-repay", "sig-repay", 200)
	presellFunding.RouteKey, presendRepay.RouteKey, presellFunding.StrategyKey, presendRepay.StrategyKey = failKey, failKey, failCfg.Lane, failCfg.Lane
	insert(presendRepay)
	insert(presellFunding)
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'built','DECIDE',$3,'{}')`,
		failKey+"-pending", failKey, failCfg.Lane); err != nil {
		t.Fatal(err)
	}
	if err = db.MarkPreBroadcastFailed(ctx, failKey+"-pending", Built, "presend policy rejection"); err != nil {
		t.Fatalf("actual MarkPreBroadcastFailed path failed: %v", err)
	}
	var storedSignature *string
	var storedWire []byte
	var storedReason *string
	var storedBroadcast *time.Time
	if err = db.pool.QueryRow(ctx, `SELECT transaction_signature, signed_wire, recovery_reason, broadcast_intent_at
		FROM loyal_yield.multiply_operations WHERE operation_id=$1`, failKey+"-pending").Scan(&storedSignature, &storedWire, &storedReason, &storedBroadcast); err != nil {
		t.Fatal(err)
	}
	if storedSignature != nil || storedWire != nil || storedBroadcast != nil || storedReason == nil || *storedReason == "" {
		t.Fatalf("presend failure row is not provably no-sign/no-broadcast: sig=%v wire=%v broadcast=%v reason=%v", storedSignature, storedWire, storedBroadcast, storedReason)
	}
	failEvidence, err := db.observeSharedCustodyAttributionEvidence(ctx, failLease, failCfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if failEvidence.Unknown || failEvidence.Unresolved || failEvidence.MalformedIdentity {
		t.Fatalf("proven presend rejection held the route: unknown=%v unresolved=%v malformedIdentity=%v", failEvidence.Unknown, failEvidence.Unresolved, failEvidence.MalformedIdentity)
	}
	if _, err = validateSharedCustodyAttribution(3_100_000_000, 300, failCfg, failEvidence, 0); err != nil {
		t.Fatalf("legitimate presend rejection permanently disabled attribution: %v", err)
	}
	// An ambiguous failed row that still carries signing state — signature
	// and wire digest, reason notwithstanding — keeps the route closed.
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations
		(operation_id,route_key,status,action,strategy_key,expected_effects,transaction_signature,signed_wire_sha256,recovery_reason)
		VALUES($1,$2,'failed','DECIDE',$3,'{}','sig-ambiguous',$4,'unclear outcome')`,
		failKey+"-ambiguous", failKey, failCfg.Lane, sha256Bytes([]byte("carried wire"))); err != nil {
		t.Fatal(err)
	}
	failEvidence, err = db.observeSharedCustodyAttributionEvidence(ctx, failLease, failCfg, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !failEvidence.Unknown {
		t.Fatalf("ambiguous signed failed row excluded from the unknown gate: %+v", failEvidence)
	}
	_, err = validateSharedCustodyAttribution(3_100_000_000, 300, failCfg, failEvidence, 0)
	if reason := custodyAttributionHoldReason(t, err); reason != "custody_attribution_unknown_record" {
		t.Fatalf("ambiguous signed failed row not held: %s", reason)
	}
}
