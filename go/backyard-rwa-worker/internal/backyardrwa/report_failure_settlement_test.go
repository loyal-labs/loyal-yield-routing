package backyardrwa

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
)

func failureSettlementFixture(t *testing.T) (finalizedFailureReceipt, []byte, publicKey, ExpectedEffects) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	signed, err := buildAndSignBridgeTransactionForDelegate(bridgeTestRequest(ReportNAV, 0), key, delegate)
	if err != nil {
		t.Fatal(err)
	}
	effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 1214944, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	offset := 3
	count, err := decodeShortVec(signed.message, &offset)
	if err != nil {
		t.Fatal(err)
	}
	balances := make([]uint64, count)
	for i := range balances {
		balances[i] = 1000000
	}
	post := append([]uint64(nil), balances...)
	post[0] -= 5000
	tokens := []any{}
	for _, effect := range effects.Accounts {
		index := -1
		for i := 0; i < count; i++ {
			if keyString(signed.message[offset+i*32:offset+(i+1)*32]) == effect.Address {
				index = i
				break
			}
		}
		if index < 0 {
			t.Fatal("missing custody")
		}
		tokens = append(tokens, map[string]any{"accountIndex": index, "mint": effect.Mint, "owner": effect.Authority, "programId": effect.Owner, "uiTokenAmount": map[string]any{"amount": strconv.FormatUint(effect.BeforeRaw, 10), "decimals": 6}})
	}
	raw, err := json.Marshal(map[string]any{"slot": 75, "transaction": []string{base64.StdEncoding.EncodeToString(signed.signedWire), "base64"}, "meta": map[string]any{"err": map[string]any{"InstructionError": []any{0, map[string]any{"Custom": 9}}}, "fee": 5000, "preBalances": balances, "postBalances": post, "preTokenBalances": tokens, "postTokenBalances": tokens, "logMessages": adaptorFailureLogs(bridgeAdaptorProgram, 9)}})
	if err != nil {
		t.Fatal(err)
	}
	var receipt finalizedFailureReceipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt, signed.signedWire, delegate, effects
}

func TestFinalizedReportFailureRequiresCompleteExactRollbackReceipt(t *testing.T) {
	base, wire, delegate, effects := failureSettlementFixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(*finalizedFailureReceipt)
	}{
		{"valid", func(*finalizedFailureReceipt) {}},
		{"missing meta", func(r *finalizedFailureReceipt) { r.Meta = nil }},
		{"success", func(r *finalizedFailureReceipt) { r.Meta.Err = json.RawMessage(`null`) }},
		{"status only error", func(r *finalizedFailureReceipt) { r.Meta.Err = json.RawMessage(`"BlockhashNotFound"`) }},
		{"changed wire", func(r *finalizedFailureReceipt) {
			r.Transaction[0] = base64.StdEncoding.EncodeToString(append(wire, 0))
		}},
		{"missing fee", func(r *finalizedFailureReceipt) { r.Meta.Fee = nil }},
		{"payer mismatch", func(r *finalizedFailureReceipt) { r.Meta.PostBalances[0]++ }},
		{"capital debit", func(r *finalizedFailureReceipt) { r.Meta.PostBalances[1]-- }},
		{"truncated balances", func(r *finalizedFailureReceipt) {
			r.Meta.PreBalances = r.Meta.PreBalances[:1]
			r.Meta.PostBalances = r.Meta.PostBalances[:1]
		}},
		{"empty tokens", func(r *finalizedFailureReceipt) {
			r.Meta.PreTokenBalances = []json.RawMessage{}
			r.Meta.PostTokenBalances = []json.RawMessage{}
		}},
		{"missing matching token", func(r *finalizedFailureReceipt) {
			r.Meta.PreTokenBalances = r.Meta.PreTokenBalances[1:]
			r.Meta.PostTokenBalances = r.Meta.PostTokenBalances[1:]
		}},
		{"duplicate matching token", func(r *finalizedFailureReceipt) {
			r.Meta.PreTokenBalances[1] = r.Meta.PreTokenBalances[0]
			r.Meta.PostTokenBalances[1] = r.Meta.PostTokenBalances[0]
		}},
		{"unknown token identity", func(r *finalizedFailureReceipt) {
			r.Meta.PreTokenBalances[0] = json.RawMessage(`{"accountIndex":0}`)
			r.Meta.PostTokenBalances[0] = r.Meta.PreTokenBalances[0]
		}},
		{"token effect", func(r *finalizedFailureReceipt) { r.Meta.PostTokenBalances[0] = json.RawMessage(`{"accountIndex":0}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var r finalizedFailureReceipt
			_ = json.Unmarshal(raw, &r)
			tc.mutate(&r)
			err := validateFinalizedFailureReceipt(r, wire, encodeBase58(wire[1:65]), sha256Bytes(wire), sha256Bytes(wire[65:]), delegate, effects)
			if (err == nil) != (tc.name == "valid") {
				t.Fatal("incorrect proof result", err)
			}
		})
	}
	if err := validateFinalizedFailureReceipt(base, wire, encodeBase58(wire[1:65]), sha256Bytes(wire), sha256Bytes(wire[65:]), mustKey(bridgeDelegate), effects); err == nil {
		t.Fatal("wrong signer admitted")
	}
}

func TestFailedReportFeeSettlementChargesAndRestoresExitWithoutReplay(t *testing.T) {
	for _, pilot := range []bool{false, true} {
		budget := emptyTestBudget()
		family := "OnRe"
		if pilot {
			budget = pilotTestBudget(t)
			family = "Prime"
		}
		budget.Families[family] = FamilyBudget{SpentMicros: 100, ExitMicros: 2000}
		reservation := BudgetReservation{OperationID: "failed", Family: family, IntentSHA256: sha256Bytes([]byte("intent")), UpperMicros: 1000, ExitAfterMicros: 500, Recovery: true}
		if pilot {
			reservation.ExecutionCostUpperMicros = 1000
		}
		if err := budget.Admit(reservation); err != nil {
			t.Fatal(err)
		}
		messageHash := sha256Bytes([]byte("message"))
		wireHash := sha256Bytes([]byte("wire"))
		cost := ValuedTransactionCost{MessageSHA256: messageHash, Fee: MessageFeeObservation{MessageSHA256: messageHash, Slot: 42, Lamports: 5000}, NativePrice: budgetTestPrice(nativeSOLBudgetAsset, "11111111111111111111111111111111", 9, 100, 1), ObservationSlot: 42, NetworkFeeMicros: 500}
		auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: reservation.IntentSHA256, SignedWireSHA256: wireHash, SendKnownCost: &cost}
		proof := finalizedFailureSettlement{SignedWireSHA256: wireHash, MessageSHA256: messageHash, Slot: 75, AtomicNoCapitalMovement: true, FeeLamports: 5000}
		before, _ := json.Marshal(budget)
		next, booked, err := settleFailedFeeBudget(budget, auth, "failed", &proof)
		if err != nil {
			t.Fatal(err)
		}
		if next.Families[family].SpentMicros != 600 || next.Families[family].ExitMicros != 2000 || len(next.Reservations) != 0 || booked.ReservationReleased || booked.BookedSpentMicros != 500 || proof.BookedFeeMicros != 500 {
			t.Fatal("fee lost or exit reserve changed", next, booked, proof)
		}
		if pilot && (next.Families[family].ExecutionCostSpentMicros != 500 || booked.BookedExecutionCostMicros != 500) {
			t.Fatal("pilot fee missing")
		}
		after, _ := json.Marshal(budget)
		if !bytes.Equal(before, after) {
			t.Fatal("caller mutated")
		}
		if _, _, err = settleFailedFeeBudget(next, booked, "failed", &proof); err == nil {
			t.Fatal("fee replay accepted")
		}
		for _, mutation := range []func(*phase3OperationAuthorization, *finalizedFailureSettlement){
			func(a *phase3OperationAuthorization, p *finalizedFailureSettlement) { a.SendKnownCost = nil },
			func(a *phase3OperationAuthorization, p *finalizedFailureSettlement) { p.FeeLamports++ },
			func(a *phase3OperationAuthorization, p *finalizedFailureSettlement) {
				p.AtomicNoCapitalMovement = false
			},
			func(a *phase3OperationAuthorization, p *finalizedFailureSettlement) { a.ReservationReleased = true },
			func(a *phase3OperationAuthorization, p *finalizedFailureSettlement) { p.MessageSHA256 = wireHash },
		} {
			a, p := auth, proof
			mutation(&a, &p)
			if _, _, err = settleFailedFeeBudget(budget, a, "failed", &p); err == nil {
				t.Fatal("unproven spend admitted")
			}
		}
	}
}
