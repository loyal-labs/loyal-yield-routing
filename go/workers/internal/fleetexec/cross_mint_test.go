package fleetexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

func TestCrossMintIdleCustodyRequiresAggregateAndCompleteRecognizedHistory(t *testing.T) {
	f := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(f.FeePayer)
	mint := sdk.MustPublicKeyFromBase58(fleet.USDCMint)
	custody := sweepKey("custody")
	for _, tc := range []struct {
		name                                       string
		amount                                     uint64
		slot                                       int64
		address                                    string
		foreignOwner, unknownHistory, prunedAnchor bool
		allow                                      bool
	}{
		{name: "exact aggregate covers attributed delta", amount: 1045, slot: 101, address: custody, allow: true},
		{name: "attributed amount is not aggregate", amount: 995, slot: 101, address: custody},
		{name: "changed larger aggregate", amount: 1046, slot: 101, address: custody},
		{name: "stale finalized bank", amount: 1045, slot: 99, address: custody},
		{name: "custody account absent", amount: 1045, slot: 101, address: sweepKey("other")},
		{name: "foreign vault authority", amount: 1045, slot: 101, address: custody, foreignOwner: true},
		{name: "restored amount after external spend", amount: 1045, slot: 101, address: custody, unknownHistory: true},
		{name: "pruned anchor is unknown", amount: 1045, slot: 101, address: custody, prunedAnchor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, 165)
			copy(data[:32], mint[:])
			copy(data[32:64], owner[:])
			if tc.foreignOwner {
				data[32] ^= 1
			}
			data[108] = 1
			binary.LittleEndian.PutUint64(data[64:72], tc.amount)
			reader := fixtureAccounts{accounts: map[string]chain.Account{tc.address: fixtureAccount(tc.address, sdk.TokenProgramID.String(), 1, data)}, slot: tc.slot}
			page := []chain.Signed{signed("receipt", 100)}
			if tc.unknownHistory {
				page = append([]chain.Signed{signed("external", 101)}, page...)
			}
			if tc.prunedAnchor {
				page = nil
			}
			m := CrossMintMovement{Phase: CrossMintSourceIdle, VaultPubkey: owner.String(), CustodyMint: mint.String(), CustodyAccount: custody, CustodyAmountRaw: 995, CustodyObservedBalanceRaw: crossInt(1045), CustodyReconciledSlot: crossInt(100)}
			err := verifyCrossMintIdleCustody(context.Background(), m, reader, &historyPages{pages: [][]chain.Signed{page}}, recognizedSignatures("receipt"))
			if (err == nil) != tc.allow {
				t.Fatalf("allow=%v err=%v", tc.allow, err)
			}
		})
	}
}

func crossInt(v int64) *int64      { return &v }
func crossString(v string) *string { return &v }
func crossMovement() CrossMintMovement {
	return CrossMintMovement{DecisionID: 1, OpportunityID: 2, VaultID: 3, SourceReserve: "source", IntendedTargetReserve: "target", ActiveTargetReserve: "target", SourceMint: "source-mint", TargetMint: "target-mint", PlannedAmountRaw: 1000, CustodyMint: "source-mint", CustodyAmountRaw: 1000, CustodyAccount: "source", Phase: CrossMintSourceReserve}
}
func crossWithdrawContract() (CrossMintExpectedEffect, CrossMintBalanceAnchors, CrossMintEffect, CrossMintBalanceAnchors) {
	expected := CrossMintExpectedEffect{CreditMint: crossString("source-mint"), CreditTokenAccount: crossString("source-ata"), MinimumCreditAmountRaw: crossInt(900)}
	pre := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: "source-mint", TokenAccount: "source-ata", AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: "source", Market: "market", Obligation: "source-obligation", ObligationExists: true, CollateralRaw: 1001}}
	actual := CrossMintEffect{Credit: &CrossMintTokenAmount{Mint: "source-mint", TokenAccount: "source-ata", AmountRaw: 995}}
	post := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: "source-mint", TokenAccount: "source-ata", AmountRaw: 1045}, Position: &CrossMintPositionAnchor{Reserve: "source", Market: "market", Obligation: "source-obligation", ObligationExists: true, CollateralRaw: 1}}
	return expected, pre, actual, post
}

func TestCrossMintFinalizedCreditIsAttributedCustodyNotAggregate(t *testing.T) {
	m := crossMovement()
	e, pre, actual, post := crossWithdrawContract()
	next, err := crossMintReceiptTransition(m, LegWithdraw, PurposeOptimizeYield, e, pre, actual, post, 10)
	if err != nil {
		t.Fatal(err)
	}
	if next.amount != 995 || next.observed == nil || *next.observed != 1045 || next.account != "source-ata" || next.outcome != nil {
		t.Fatalf("credited and aggregate custody conflated: %+v", next)
	}
	m.Phase, m.CustodyVersion, m.CustodyAccount, m.CustodyAmountRaw, m.CustodyObservedBalanceRaw, m.CustodyReconciledSlot = CrossMintSourceIdle, 1, next.account, next.amount, next.observed, crossInt(10)
	r, _ := nextCrossMintRequest(m, true)
	r.Leg, r.Purpose = LegSwap, PurposeOptimizeYield
	swap := CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: 1000}, CreditMint: crossString(m.TargetMint), CreditTokenAccount: crossString("target-ata"), MinimumCreditAmountRaw: crossInt(990)}
	anchors := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: m.CustodyAccount, AmountRaw: 1045}, Credit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: "target-ata", AmountRaw: 7}}
	if validateCrossMintLeg(r, swap, anchors) == nil {
		t.Fatal("planned withdrawal amount authorized instead of finalized credit")
	}
	swap.Debit.AmountRaw = 995
	if err := validateCrossMintLeg(r, swap, anchors); err != nil {
		t.Fatal(err)
	}
	anchors.Debit.AmountRaw = 995
	if validateCrossMintLeg(r, swap, anchors) == nil {
		t.Fatal("attributed amount substituted for aggregate historical anchor")
	}
}

func TestCrossMintReceiptRejectsChangedPositionExtraAccountsAndOverflow(t *testing.T) {
	for _, name := range []string{"balance", "identity", "collateral", "missing-credit", "extra-debit", "overflow"} {
		t.Run(name, func(t *testing.T) {
			m := crossMovement()
			e, pre, actual, post := crossWithdrawContract()
			switch name {
			case "balance":
				post.Credit.AmountRaw++
			case "identity":
				post.Position.Obligation = "external"
			case "collateral":
				post.Position.CollateralRaw = 2
			case "missing-credit":
				actual.Credit = nil
				post.Credit = nil
			case "extra-debit":
				actual.Debit = &CrossMintTokenAmount{Mint: "source-mint", TokenAccount: "external", AmountRaw: 1}
				post.Debit = actual.Debit
			case "overflow":
				pre.Credit.AmountRaw = math.MaxInt64
				post.Credit.AmountRaw = math.MaxInt64
			}
			if _, err := crossMintReceiptTransition(m, LegWithdraw, PurposeOptimizeYield, e, pre, actual, post, 10); err == nil {
				t.Fatal("invalid receipt changed custody")
			}
		})
	}
}

func TestCrossMintPartialDepositRequiresFinalizedUnmintableDust(t *testing.T) {
	m := crossMovement()
	m.Phase = CrossMintTargetIdle
	m.CustodyVersion = 2
	m.CustodyMint = m.TargetMint
	m.CustodyAccount = "target-ata"
	m.CustodyAmountRaw = 1000
	m.CustodyObservedBalanceRaw = crossInt(1100)
	m.CustodyReconciledSlot = crossInt(20)
	e := CrossMintExpectedEffect{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 1000}}
	pre := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 1100}, Position: &CrossMintPositionAnchor{Reserve: "target", Market: "market", Obligation: "target-obligation", ObligationExists: true, CollateralRaw: 10}}
	actual := CrossMintEffect{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 999}}
	post := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: m.TargetMint, TokenAccount: m.CustodyAccount, AmountRaw: 101}, Position: &CrossMintPositionAnchor{Reserve: "target", Market: "market", Obligation: "target-obligation", ObligationExists: true, CollateralRaw: 1010}}
	if _, err := crossMintReceiptTransition(m, LegDeposit, PurposeOptimizeYield, e, pre, actual, post, 30); err == nil {
		t.Fatal("unproved residual completed deposit")
	}
	post.Position.MinimumDepositAmountRaw = crossInt(1)
	if _, err := crossMintReceiptTransition(m, LegDeposit, PurposeOptimizeYield, e, pre, actual, post, 30); err == nil {
		t.Fatal("mintable residual completed deposit")
	}
	post.Position.MinimumDepositAmountRaw = crossInt(2)
	next, err := crossMintReceiptTransition(m, LegDeposit, PurposeOptimizeYield, e, pre, actual, post, 30)
	if err != nil {
		t.Fatal(err)
	}
	if next.amount != 1 || next.observed == nil || *next.observed != 101 || next.account != "target-ata" || next.outcome == nil || *next.outcome != "completed_target" || next.reason == nil {
		t.Fatalf("dust custody lost: %+v", next)
	}
	var evidence map[string]json.RawMessage
	if json.Unmarshal(next.evidence, &evidence) != nil || string(evidence["residualAmountRaw"]) != "1" {
		t.Fatal("dust evidence lacks exact residual")
	}
	actual.Debit.AmountRaw = 1000
	post.Debit.AmountRaw = 100
	next, err = crossMintReceiptTransition(m, LegDeposit, PurposeOptimizeYield, e, pre, actual, post, 30)
	if err != nil || next.amount != 0 || next.account != "target" || next.observed != nil || next.evidence != nil {
		t.Fatalf("full deposit did not enter reserve: %+v %v", next, err)
	}
}

func TestCrossMintPhaseSelectionPreservesRecoveryAfterRolloutOff(t *testing.T) {
	m := crossMovement()
	m.Phase = CrossMintSourceIdle
	r, err := nextCrossMintRequest(m, false)
	if err != nil || r.Leg != LegDeposit || r.Purpose != PurposeRecoverSource {
		t.Fatalf("rollout off stranded source custody: %+v %v", r, err)
	}
	m.Phase = CrossMintTargetIdle
	m.ActiveTargetReserve = "fallback"
	r, err = nextCrossMintRequest(m, false)
	if err != nil || r.Leg != LegDeposit || r.Purpose != PurposeFallbackTarget {
		t.Fatalf("target fallback binding lost: %+v %v", r, err)
	}
	m.TerminalOutcome = crossString("completed_target")
	if _, err = nextCrossMintRequest(m, true); err == nil {
		t.Fatal("terminal custody became claimable")
	}
}
