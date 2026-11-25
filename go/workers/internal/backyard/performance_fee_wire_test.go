package backyard

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func feeWireFixture(t *testing.T) (BridgeBuildRequest, ExpectedEffects) {
	t.Helper()
	return feeWireWithUnbookedIdle(t, 0)
}
func feeWireWithUnbookedIdle(t *testing.T, unbooked uint64) (BridgeBuildRequest, ExpectedEffects) {
	t.Helper()
	f := feeAnchor(t, feeMonday-7200, 100)
	s := feeBank(f, feeMonday-3600, 1024, 1024, 1000)
	s.performanceCashDelta = 1000
	f = feeStep(t, f, s, 1224, 1224, 1000, 0, true).After
	s = feeBank(f, feeMonday+60, 1024, f.PublishedValue, 1000)
	s.VoltrIdleRaw = int64(unbooked)
	s.performanceFee = &f
	s.TotalVaultNAVRaw = f.PublishedValue
	s.StrategyNAVRaw = f.PublishedStrategy
	s.net = &certifiedNAV{vault: looping.squads, manifest: looping.manifest.SHA256, slot: s.Slot, validUntil: time.Now().Add(exitQuoteInterval)}
	bindSnapshotNAV(&s)
	s.feeAllowance = &StageAllowance{RemainingRaw: 100, MaxRaw: 100, ResetAt: feeMonday + 200, ObservedAt: s.ChainUnix}
	decision := Decision{Action: PayPerformanceFee, Reason: "weekly_performance_fee", AmountRaw: 20, StrategyKey: MapleSyrupUSDCLaneID}
	x, idle, strategy, cash, err := bridgeExpectedEffects(looping, decision, unbooked, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	post := RouteNAVCustodies{VoltrIdleRaw: idle, StrategyUSDCraw: strategy, SquadsUSDCraw: cash}
	nav := RouteNAVSnapshot{net: s.net, Slot: s.Slot, StrategyNAVRaw: 1204, TotalVaultNAVRaw: 1204 + unbooked, LPSupplyRaw: 1024, PriorReportUpdatedTS: uint64(f.LastAssessment)}
	nav.Report = BridgeReport{ObservedSlot: uint64(s.Slot), Sequence: uint64(s.Slot), NAVAfterRaw: nav.StrategyNAVRaw, SnapshotDigest: navDigest(sha256Bytes([]byte("Looping wire fixture")))}
	nav.net.report = nav.Report
	vault := voltrVaultFixture(t, uint64(f.PublishedValue))
	vault.Address = looping.VoltrVault
	w, err := prepareFeeReport(looping.manifest, decision, s, nav, []ConfirmedAccount{vault}, post)
	if err != nil {
		t.Fatal(err)
	}
	w.PayoutPolicy = testPublicKey(52)
	x.Kind = effectsBridge
	x.PerformanceFee = &w.Entry
	x.ReturnData = expectedAdaptorReturnData(looping, uint64(w.Entry.After.PublishedStrategy))
	return bindBridgeRequest(BridgeBuildRequest{fee: w, net: nav.net, Action: BridgeAction(PayPerformanceFee), AmountRaw: 20,
		Report: nav.Report, Policy: testPublicKey(42), AdaptorConfig: looping.Strategy, Settings: backyardSettings,
		RecentBlockhash: testPublicKey(81), LastValidBlockHeight: 650}), x
}

func TestLoopingFeeRealWireAndPolicy(t *testing.T) {
	r, x := feeWireFixture(t)
	literal, err := PerformanceFeePolicy("looping", x.PerformanceFee.Before.WeeklyCapRaw)
	if err != nil || len(literal.Constraints) != 1 || literal.VaultIndex != 2 || len(literal.SpendingLimits) != 1 {
		t.Fatal(literal, err)
	}
	limit := literal.SpendingLimits[0]
	if limit.Period != squads.PeriodWeekly || limit.Mint != solanaKey(fleet.USDCMint) || limit.MaxPerPeriod != 100 {
		t.Fatal(limit)
	}
	for _, bad := range []struct {
		vault string
		cap   int64
	}{{"rwa", 100}, {"looping", 0}, {"looping", int64(bridgeMaxNAV) + 1}} {
		if _, err := PerformanceFeePolicy(bad.vault, bad.cap); err == nil {
			t.Fatal(bad)
		}
	}
	message, _, _, err := compileRequest(looping.manifest, r)
	if err != nil {
		t.Fatal(err)
	}
	wire := simulationWire(message)
	m, err := decodeV1Message(message)
	if err != nil || len(wire) > maxTransactionBytes || m.Header.NumRequiredSignatures != 1 || m.AccountKeys[0] != solanaKey(backyardDelegate) || len(m.Instructions) != 12 {
		t.Fatalf("wire %d bytes, %d instructions: %v", len(wire), len(m.Instructions), err)
	}
	decode := func(i int) squads.ExecuteSync {
		ix := m.Instructions[i]
		metas, err := ix.ResolveInstructionAccounts(&m)
		if err != nil {
			t.Fatal(err)
		}
		outer := squads.Instruction{ProgramID: m.AccountKeys[ix.ProgramIDIndex], Data: ix.Data}
		for _, meta := range metas {
			outer.Accounts = append(outer.Accounts, *meta)
		}
		inner, err := squads.DecodeExecuteTransactionSyncV2(outer)
		if err != nil || inner.Signer != solanaKey(backyardDelegate) || inner.AccountIndex != 2 || len(inner.Inner) != 1 {
			t.Fatal(inner, err)
		}
		return inner
	}
	for i, ix := range m.Instructions {
		if i != 8 && i != 9 && m.AccountKeys[ix.ProgramIDIndex] != solanaKey(feeLighthouse) {
			t.Fatalf("unapproved top-level instruction %d", i)
		}
	}
	checkPin := func(i int, want compiledInstruction) {
		ix := m.Instructions[i]
		if !bytes.Equal(ix.Data, want.data) || len(ix.Accounts) != len(want.accounts) {
			t.Fatalf("assertion %d data", i)
		}
		for j, v := range want.accounts {
			if m.AccountKeys[ix.Accounts[j]] != solana.PublicKey(v.key) {
				t.Fatalf("assertion %d address", i)
			}
		}
	}
	owner := solanaKey(backyardSettingsSigner)
	canonical, err := spl.AssociatedTokenAddress(owner, solanaKey(fleet.USDCMint), solana.TokenProgramID)
	if err != nil || canonical != solanaKey(loopingTreasuryUSDC) {
		t.Fatal("treasury is not the admin USDC ATA", canonical, err)
	}
	checkPin(7, feeAssertBytes(loopingTreasuryUSDC, feeBytePin{32, owner[:]}))
	checkPin(10, feeAssertBytes(looping.strategyReceipt, feeU64(104, r.Report.NAVAfterRaw)))
	checkPin(11, feeAssertBytes(looping.VoltrVault, feeU64(168, uint64(x.PerformanceFee.After.PublishedValue))))
	if got := binary.LittleEndian.Uint64(m.Instructions[1].Data[3:11]); got != uint64(feeMonday+7*86400-1) {
		t.Fatal("wire can cross calendar allowance", got)
	}
	payout, nav := decode(8), decode(9)
	move := payout.Inner[0]
	allow := func(ix squads.Instruction) bool { return squads.Admits(literal.Constraints[0], ix, nil) }
	if payout.Policy != solanaKey(r.fee.PayoutPolicy) || !bytes.Equal(payout.ConstraintIndexes, []byte{0}) || !allow(move) || len(move.Accounts) != 4 ||
		move.ProgramID != solana.TokenProgramID || len(move.Data) != 10 || move.Data[0] != 12 || binary.LittleEndian.Uint64(move.Data[1:]) != 20 || move.Data[9] != 6 {
		t.Fatal("invalid transfer", move)
	}
	pins := []string{looping.cash, fleet.USDCMint, loopingTreasuryUSDC, looping.squads}
	for i, pin := range pins {
		if move.Accounts[i].PublicKey != solanaKey(pin) {
			t.Fatal("wrong account", i)
		}
		bad := move
		bad.Accounts = append([]solana.AccountMeta(nil), move.Accounts...)
		bad.Accounts[i].PublicKey = solanaKey(rwa.squads)
		if allow(bad) {
			t.Fatal("policy accepted changed account", i)
		}
	}
	for _, bad := range []struct {
		tag, dec byte
		amount   uint64
	}{{3, 6, 20}, {12, 5, 20}, {12, 6, 0}, {12, 6, 101}} {
		ix := move
		ix.Data = bytes.Clone(move.Data)
		ix.Data[0], ix.Data[9] = bad.tag, bad.dec
		binary.LittleEndian.PutUint64(ix.Data[1:9], bad.amount)
		if allow(ix) {
			t.Fatal("unapproved transfer", bad)
		}
	}
	report := nav.Inner[0]
	if nav.Policy != solanaKey(r.Policy) || !bytes.Equal(nav.ConstraintIndexes, []byte{trustfulDepositLeg}) ||
		report.ProgramID != voltr.ProgramID || !squads.Admits(trustfulBridgePolicies(looping)[VoltrAllocateToSquads].Constraints[trustfulDepositLeg], report, nil) {
		t.Fatal("wrong net NAV policy", nav)
	}
	if _, _, _, err := compileRequest(rwa.manifest, r); err == nil {
		t.Fatal("compiled payment for Earn Max")
	}
	changed := r
	changed.AmountRaw++
	if _, _, _, err := compileRequest(looping.manifest, changed); err == nil {
		t.Fatal("payout escaped signing seal")
	}
	copied := *r.fee
	copied.PayoutPolicy = r.Policy
	changed = r
	changed.fee = &copied
	if _, _, _, err := compileRequest(looping.manifest, changed); err == nil {
		t.Fatal("policy substitution escaped signing seal")
	}
	if err := validateSignedFeeWire(SignedTransaction{message: message, signedWire: wire}, SimulationResult{Slot: r.fee.Entry.Slot, MessageSHA256: sha256Bytes(message)}, mustKey(backyardDelegate)); err == nil {
		t.Fatal("unsigned simulation could be persisted for broadcast")
	}
}

func feeFinalReceipt(x ExpectedEffects) ConfirmedTransactionEvidence {
	receipt := ConfirmedTransactionEvidence{Finalized: true, Slot: x.PerformanceFee.Slot + 1, Signature: "fee-fixture", Logs: []string{"Program return: " + x.ReturnData.ProgramID + " " + x.ReturnData.DataBase64}}
	for _, a := range x.Accounts {
		pre := TransactionTokenBalance{Address: a.Address, OwnerProgram: a.Owner, Mint: a.Mint, Authority: a.Authority, Raw: a.BeforeRaw}
		post := pre
		post.Raw = a.AfterRaw
		if a.Address == loopingTreasuryUSDC {
			pre.Raw += 300
			post.Raw += 300
		}
		receipt.PreTokenBalances = append(receipt.PreTokenBalances, pre)
		receipt.PostTokenBalances = append(receipt.PostTokenBalances, post)
	}
	return receipt
}

func TestLoopingFeeFinalizedReceiptRequiresExactTransfer(t *testing.T) {
	_, x := feeWireFixture(t)
	raw, _ := json.Marshal(x)
	if _, err := DecodeExpectedEffects(looping, raw); err != nil {
		t.Fatal(err)
	}
	receipt := feeFinalReceipt(x)
	_, evidence, err := ReconcileConfirmedTransaction(looping, x, receipt)
	if err != nil || !bytes.Contains(evidence, []byte(`"loopingCashDelta":-20`)) {
		t.Fatal(string(evidence), err)
	}
	receipt.Finalized = false
	if _, _, err := ReconcileConfirmedTransaction(looping, x, receipt); err == nil {
		t.Fatal("accepted unfinalized treasury credit")
	}
	receipt.Finalized = true
	receipt.PostTokenBalances[len(receipt.PostTokenBalances)-1].Raw--
	if _, _, err := ReconcileConfirmedTransaction(looping, x, receipt); err == nil {
		t.Fatal("accepted short treasury transfer")
	}
	x.PerformanceFee = nil
	if _, _, err := ReconcileConfirmedTransaction(looping, x, receipt); err == nil {
		t.Fatal("accepted bare treasury transfer")
	}
}

func TestLoopingFeeApproved64BitCeiling(t *testing.T) {
	const approved int64 = 10_000_000_000
	p, err := PerformanceFeePolicy(looping.Name, approved)
	if err != nil {
		t.Fatal(err)
	}
	admin := solanaKey(backyardSettingsSigner)
	wire, err := (squads.PolicyApply{Settings: solanaKey(backyardSettings), RentPayer: admin, Signer: admin, Delegate: solanaKey(backyardDelegate), Seed: 40, VaultIndex: p.VaultIndex, Constraints: p.Constraints, SpendingLimits: p.SpendingLimits}).Instruction()
	if err != nil {
		t.Fatal(err)
	}
	actions, err := squads.DecodeSettingsActions(wire)
	if err != nil || len(actions) != 1 || !actions[0].Payload.Policy.Equal(p) || actions[0].Payload.SpendingLimits[0].MaxPerPeriod != uint64(approved) {
		t.Fatal("settings wire truncated approved amount", actions, err)
	}
	var bounded bool
	for _, c := range actions[0].Payload.Constraints[0].DataConstraints {
		if c.DataOffset == 1 && c.Operator == squads.OpLessThanOrEqualTo && c.DataValue.Kind == squads.ValueU64 && c.DataValue.U64 == uint64(approved) {
			bounded = true
		}
	}
	if !bounded {
		t.Fatal("single-transfer constraint lost its 64-bit ceiling")
	}
	for _, cap := range []int64{1, approved, approved + 1} {
		_, err := PerformanceFeePolicy("looping", cap)
		s := performanceFeeState{ActivationSlot: 500, WeeklyCapRaw: cap}
		if (err == nil) != (cap <= approved) || s.valid() != (cap <= approved) {
			t.Fatal("state and policy disagree", cap, err)
		}
	}
}

func TestLoopingFeeUnbookedIdleExcludedFromActualReport(t *testing.T) {
	clean, _ := feeWireFixture(t)
	gift, _ := feeWireWithUnbookedIdle(t, 800)
	if gift.fee.Entry != clean.fee.Entry || gift.Report != clean.Report || gift.fee.Idle != 800 {
		t.Fatal("unsolicited idle USDC changed the fee or reported net wealth")
	}
	if _, _, _, err := compileRequest(looping.manifest, gift); err != nil {
		t.Fatal(err)
	}
}
