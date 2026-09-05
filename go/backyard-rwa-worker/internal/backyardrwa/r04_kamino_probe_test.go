package backyardrwa

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Feed actual deployed-program token poststates through the production
// compiler, amount selector, economic debit measurement and reconciliation.
// These are local SVM transitions, not confirmed RPC receipts or signer proof.
func TestPhase3KaminoRepaymentProbeMatchesProduction(t *testing.T) {
	dir, resultName := os.Getenv("PHASE3_KAMINO_PROBE_DIR"), os.Getenv("PHASE3_KAMINO_PROBE_RESULT")
	if dir == "" || resultName == "" {
		t.Skip("explicit local probe snapshot and execution result required")
	}
	if filepath.Base(resultName) != resultName {
		t.Fatal("result must be a filename within the probe directory")
	}
	read := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var plan struct {
		Lane  string
		Steps []struct {
			Leg     string
			Request KaminoPrimeUSDCRequest
		}
	}
	read("plan.json", &plan)
	if plan.Lane != ethenaUSDePYUSD.Lane || len(plan.Steps) != 4 || plan.Steps[2].Leg != "repay" {
		t.Fatal("unexpected repayment plan")
	}
	type capturedAccount struct {
		Address, Owner, DataBase64, DataSHA256 string
		Lamports                               uint64
		Present                                bool
	}
	var result struct {
		Slot                        int64
		BoundedRepaymentProofPassed bool
		BoundedRepaymentProbes      []struct {
			MaximumDebitRaw, ActualDebitRaw    uint64
			WireBase64, WireSHA256, BorrowedSF string
			Error                              *string
			ObligationDebtZero                 bool
			Before, After                      []capturedAccount
			ClockBefore, ExecutionClock        struct {
				Slot          uint64
				UnixTimestamp int64
			}
		}
	}
	read(resultName, &result)
	if !result.BoundedRepaymentProofPassed || len(result.BoundedRepaymentProbes) != 3 {
		t.Fatal("complete real-program repayment witnesses missing")
	}
	decodeAccounts := func(captured []capturedAccount) []ConfirmedAccount {
		t.Helper()
		accounts := []ConfirmedAccount{}
		for _, a := range captured {
			if !a.Present {
				continue
			}
			data, err := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
			if err != nil || sha256Bytes(data) != a.DataSHA256 {
				t.Fatal("captured account hash mismatch", a.Address, err)
			}
			accounts = append(accounts, ConfirmedAccount{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Data: data})
		}
		return accounts
	}
	route := ethenaUSDePYUSD
	source, destination := kaminoLegCustodiesForRoute(kaminoLegRepay, route)
	for i, probe := range result.BoundedRepaymentProbes {
		maximum := []uint64{1_010, 999, 1_001}[i]
		actual := uint64(1_000)
		if maximum == 1_001 {
			actual = 1_001
		}
		if maximum == 999 {
			actual = 0
			if probe.Error == nil || *probe.Error != "InstructionError(3, Custom(6092))" {
				t.Fatal("missing real-program residual-debt rejection")
			}
		} else if probe.Error != nil {
			t.Fatal("full repayment failed", *probe.Error)
		}
		if probe.MaximumDebitRaw != maximum || probe.ActualDebitRaw != actual || probe.ObligationDebtZero != (maximum >= 1_000) {
			t.Fatal("unexpected actual repayment or payoff result", maximum)
		}
		before, after := decodeAccounts(probe.Before), decodeAccounts(probe.After)
		if maximum == 1_001 {
			clock := accountAt(before, budgetClockAddress)
			if probe.ExecutionClock.UnixTimestamp-probe.ClockBefore.UnixTimestamp != kaminoPayoffWindowSeconds ||
				probe.ExecutionClock.Slot-probe.ClockBefore.Slot != uint64(budgetMaxObservationLagSlots) {
				t.Fatal("missing real execution-horizon clock advance")
			}
			// Price at the original clock against the post-borrow reserve, then
			// prove that maximum fully repaid at the future execution clock.
			binary.LittleEndian.PutUint64(clock.Data[:8], probe.ClockBefore.Slot)
			binary.LittleEndian.PutUint64(clock.Data[32:40], uint64(probe.ClockBefore.UnixTimestamp))
			bound, err := decodeKaminoPayoffBound(before, route, result.Slot)
			if err != nil || bound.InterestBasis != 1 || bound.UpperDebtRaw != maximum || bound.ThroughUnix != probe.ExecutionClock.UnixTimestamp {
				t.Fatal("payoff bound does not cover executed seconds-based interest", bound, err)
			}
		}
		beforeObligation, err := decodeKaminoObligation(accountAt(before, route.Kamino.Obligation), route.Kamino)
		if err != nil || beforeObligation.debtRaw != 1_000 {
			t.Fatal("witness did not start from the real borrow poststate", beforeObligation.debtRaw, err)
		}
		leg, amount, minimum, err := selectKaminoLeg(Decision{Action: DeleverRouteStep, StrategyKey: route.Lane, AmountRaw: int64(maximum)}, KaminoPosition{DebtRaw: beforeObligation.debtRaw})
		if err != nil || leg != kaminoLegRepay || amount != maximum {
			t.Fatal("production selected a different repayment", amount, err)
		}
		request := plan.Steps[2].Request
		request.AmountRaw = amount
		request.Data = append([]byte(nil), request.Data...)
		binary.LittleEndian.PutUint64(request.Data[8:], amount)
		message, err := CompileKaminoMessage(request)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(probe.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != probe.WireSHA256 || !bytes.Equal(wire[65:], message) {
			t.Fatal("executed repayment does not match current production compiler", err)
		}
		effects, err := boundedKaminoRepaymentEffects(before, source, destination, minimum, maximum)
		if err != nil {
			t.Fatal(err)
		}
		debit, err := MeasureExecutableDebit(request, effects)
		if err != nil || debit.Raw != maximum {
			t.Fatal("production did not reserve the maximum executable debit", debit, err)
		}
		receipt := ConfirmedTransactionEvidence{Signature: "local-svm-not-signed:" + probe.WireSHA256, Slot: result.Slot}
		for _, boundary := range []kaminoCustodyBoundary{source, destination} {
			mint, _ := decodeBase58PublicKey(boundary.Mint)
			authority, _ := decodeBase58PublicKey(boundary.Authority)
			for j, accounts := range [][]ConfirmedAccount{before, after} {
				a := accountAt(accounts, boundary.Address)
				custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
				if err != nil {
					t.Fatal(err)
				}
				balance := TransactionTokenBalance{Address: boundary.Address, OwnerProgram: a.Owner, Mint: boundary.Mint, Authority: boundary.Authority, Raw: custody.Raw}
				if j == 0 {
					receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
				} else {
					receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
				}
			}
		}
		if receipt.PreTokenBalances[0].Raw-receipt.PostTokenBalances[0].Raw != probe.ActualDebitRaw {
			t.Fatal("reported debit differs from captured custody")
		}
		if _, _, err := ReconcileConfirmedTransaction(effects, receipt); (err == nil) != (probe.Error == nil) {
			t.Fatal("clipped repayment rejected or failed partial repayment reconciled", err)
		}
		if maximum == 1_010 {
			exact := effects
			exact.Kind, exact.Repayment = "", nil
			if _, _, err := ReconcileConfirmedTransaction(exact, receipt); err == nil {
				t.Fatal("negative control: exact-request reconciliation should reject debt-clipped debit")
			}
		}
		remaining, err := decodeKaminoObligation(accountAt(after, route.Kamino.Obligation), route.Kamino)
		if err != nil || (remaining.debtRaw == 0) != probe.ObligationDebtZero {
			t.Fatal("token transfer was mistaken for an observed full payoff", remaining.debtRaw, err)
		}
	}
}

func TestPhase3KaminoProbeMatchesProduction(t *testing.T) {
	dir := os.Getenv("PHASE3_KAMINO_PROBE_DIR")
	if dir == "" {
		t.Skip("explicit local probe snapshot required")
	}
	data, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Lane  string
		Steps []struct {
			Leg, WireBase64, WireSHA256 string
			Request                     KaminoPrimeUSDCRequest
		}
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Lane != ethenaUSDePYUSD.Lane || len(plan.Steps) != 4 {
		t.Fatal("probe lane or coverage changed")
	}
	for i, name := range []string{"deposit", "borrow", "repay", "withdraw"} {
		s := plan.Steps[i]
		if s.Leg != name || s.Request.RouteLane != plan.Lane {
			t.Fatal("probe action order or identity changed")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(s.WireBase64)
		if err != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != s.WireSHA256 {
			t.Fatal("probe wire is not an exact unsigned local fixture")
		}
		message, err := CompileKaminoMessage(s.Request)
		if err != nil || !bytes.Equal(message, wire[65:]) {
			t.Fatal("probe message differs from current production compiler", err)
		}
	}
}

// Export only the four existing Ethena production messages for an explicit
// local-SVM probe. Zero signatures and an artificial blockhash make these
// unusable on mainnet. This exporter is not itself lifecycle proof.
func TestExportPhase3KaminoControlledProbe(t *testing.T) {
	path := os.Getenv("PHASE3_KAMINO_PROBE_PLAN")
	if path == "" {
		t.Skip("explicit local probe output required")
	}
	route := ethenaUSDePYUSD
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	rows := []any{}
	addresses := map[string]bool{budgetClockAddress: true, bridgeSettings: true}
	policies := map[string]string{}
	for _, step := range []struct {
		name   string
		leg    kaminoPrimeUSDCLeg
		action Action
		amount uint64
	}{
		// USDe has 9 decimals; PYUSD has 6. Use 0.1 collateral against
		// 0.001 debt, not equal raw-unit assumptions across the assets.
		{"deposit", kaminoLegDeposit, OpenRouteStep, 100_000_000},
		{"borrow", kaminoLegBorrow, OpenRouteStep, 1_000},
		{"repay", kaminoLegRepay, DeleverRouteStep, 100_000},
		{"withdraw", kaminoLegWithdraw, DeleverRouteStep, 1_000_000_000},
	} {
		r, err := manifest.kaminoPacketForRoute(step.action, step.leg, step.amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
		if err != nil {
			t.Fatal(err)
		}
		message, err := CompileKaminoMessage(r)
		if err != nil {
			t.Fatal(err)
		}
		// The account list is shortvec, but all reviewed legacy messages have
		// fewer than 128 static keys. Reject an encoding change here explicitly.
		if message[3] >= 128 {
			t.Fatal("probe static key decoder needs update")
		}
		for i := 0; i < int(message[3]); i++ {
			addresses[encodeBase58(message[4+32*i:4+32*(i+1)])] = true
		}
		wire := append(make([]byte, 65), message...)
		wire[0] = 1
		rows = append(rows, map[string]any{"leg": step.name, "amount": step.amount, "request": r, "wireBase64": base64.StdEncoding.EncodeToString(wire), "wireSha256": sha256Bytes(wire)})
		policies[r.Policy] = r.PolicyAccountDataSHA256
	}
	keys := []string{}
	for k := range addresses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	encoded, err := json.MarshalIndent(map[string]any{"schema": "phase3-kamino-controlled-probe/v1", "broadcast": false, "signatureProof": false, "lane": route.Lane,
		"vault": bridgeVault, "delegate": bridgeDelegate, "obligation": route.Kamino.Obligation, "collateralCustody": route.CollateralCustody, "debtCustody": route.DebtCustody,
		"addresses": keys, "policies": policies, "steps": rows}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("exported exact four-leg Go messages; not executed lifecycle proof")
}
