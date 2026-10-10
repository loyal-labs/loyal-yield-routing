package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

func TestPhase3BorrowFeesMatchProduction(t *testing.T) {
	t.Parallel()
	dir, name := os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR"), os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if dir == "" || name == "" {
		t.Skip("explicit linked deployed-program evidence required")
	}
	if filepath.Base(name) != name {
		t.Fatal("result must be a local filename")
	}
	var plan struct {
		LendingPrelude struct {
			Steps []struct {
				Request    KaminoPrimeUSDCRequest
				WireSHA256 string
			}
		}
	}
	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil || json.Unmarshal(planBytes, &plan) != nil || len(plan.LendingPrelude.Steps) != 4 {
		t.Fatal("missing linked borrow plan", err)
	}
	type captured struct {
		Address, Owner, DataBase64, DataSHA256 string
		Lamports                               uint64
		Present                                bool
	}
	var result struct {
		Slot                       int64
		PlanSHA256, SnapshotSHA256 string
		BorrowFeeProbes            []struct {
			FeeSF, OverrideAccount, WireSHA256, BorrowedSF string
			OverrideOffset                                 int
			ActualDebitRaw, ReceivedRaw, FeeRaw            uint64
			ComputeUnits                                   uint64
			Before, After                                  []captured
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	snapshot, snapshotErr := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil || snapshotErr != nil || json.Unmarshal(data, &result) != nil || result.PlanSHA256 != sha256Bytes(planBytes) || result.SnapshotSHA256 != sha256Bytes(snapshot) || len(result.BorrowFeeProbes) != 2 {
		t.Fatal("borrow fee witness identity mismatch", err)
	}
	decode := func(rows []captured) []ConfirmedAccount {
		var accounts []ConfirmedAccount
		for _, a := range rows {
			if !a.Present {
				continue
			}
			b, e := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
			if e != nil || sha256Bytes(b) != a.DataSHA256 {
				t.Fatal("borrow account hash mismatch", e)
			}
			accounts = append(accounts, ConfirmedAccount{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Data: b})
		}
		return accounts
	}
	route := ethenaUSDePYUSD
	request := plan.LendingPrelude.Steps[1].Request
	message, err := CompileKaminoMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	for i, probe := range result.BorrowFeeProbes {
		if probe.OverrideAccount != route.Kamino.DebtReserve || probe.OverrideOffset != kaminoReserveConfigOffset+40 || probe.WireSHA256 != sha256Bytes(wire) || probe.WireSHA256 != plan.LendingPrelude.Steps[1].WireSHA256 || probe.FeeRaw != []uint64{1, 4}[i] {
			t.Fatal("fee probe changed the intended wire or override")
		}
		before, after := decode(probe.Before), decode(probe.After)
		rate := binary.LittleEndian.Uint64(accountAt(before, route.Kamino.DebtReserve).Data[kaminoReserveConfigOffset+40:])
		if rate != []uint64{1, 1 << 52}[i] || new(big.Int).SetUint64(rate).String() != probe.FeeSF {
			t.Fatal("fee input differs from declared override")
		}
		effects, err := kaminoBorrowEffects(before, route, request.AmountRaw)
		if err != nil {
			t.Fatal(err)
		}
		debit, err := MeasureExecutableDebit(request, effects)
		if err != nil || debit.Raw != probe.ActualDebitRaw || debit.Raw != request.AmountRaw+probe.FeeRaw || probe.ReceivedRaw != request.AmountRaw {
			t.Fatal("borrow fee omitted from cap debit", err)
		}
		receipt := ConfirmedTransactionEvidence{Signature: "local-zero-signature-borrow-fee-proof", Slot: result.Slot}
		for j, accounts := range [][]ConfirmedAccount{before, after} {
			for _, e := range effects.Accounts {
				a := accountAt(accounts, e.Address)
				mint, _ := decodeBase58PublicKey(e.Mint)
				authority, _ := decodeBase58PublicKey(e.Authority)
				cash, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
				if err != nil {
					t.Fatal(err)
				}
				balance := TransactionTokenBalance{Address: a.Address, OwnerProgram: a.Owner, Mint: e.Mint, Authority: e.Authority, Raw: cash.Raw}
				if j == 0 {
					receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
				} else {
					receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
				}
			}
		}
		if _, _, err := ReconcileConfirmedTransaction(effects, receipt); err != nil {
			t.Fatal("real fee transfer rejected", err)
		}
		obligation, err := decodeKaminoObligation(accountAt(after, route.Kamino.Obligation), route.Kamino)
		wantDebt := new(big.Int).Lsh(new(big.Int).SetUint64(debit.Raw), 60)
		if err != nil || littleInt(obligation.debtAmountSF[:]).Cmp(wantDebt) != 0 || wantDebt.String() != probe.BorrowedSF {
			t.Fatal("deployed debt excludes fee", err)
		}
		source, destination := kaminoLegCustodiesForRoute(kaminoLegBorrow, route)
		old, err := exactKaminoTokenEffects(before, source, destination, request.AmountRaw)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReconcileConfirmedTransaction(old, receipt); err == nil {
			t.Fatal("old principal-only graph did not reject actual fee debit")
		}
		receipt.PostTokenBalances[2].Raw++
		if _, _, err := ReconcileConfirmedTransaction(effects, receipt); err == nil {
			t.Fatal("changed fee receipt reconciled")
		}
		t.Logf("PHASE3_BORROW_FEE receive=%d fee=%d grossDebit=%d", request.AmountRaw, probe.FeeRaw, debit.Raw)
	}
}

func TestBorrowFeesValueTheGrossDebitAndRejectWrongGraph(t *testing.T) {
	t.Parallel()
	_, _, _, m, _, _, accounts := fundingAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	reserve := accountAt(accounts, route.Kamino.DebtReserve)
	putKey(t, reserve.Data[192:224], route.DebtFeeReceiver)
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1<<52)
	fee := accountAt(accounts, route.DebtLiquiditySupply)
	fee.Address, fee.Data = route.DebtFeeReceiver, append([]byte(nil), fee.Data...)
	binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
	accounts = append(accounts, fee)
	_, _, _, _, rpc, _ := debtResidueAdmissionFixture(t, 20_000, accounts...)
	r, err := m.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegBorrow, 1000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	e, err := kaminoBorrowEffects(accounts, route, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Accounts[2].AfterRaw != 9 {
		t.Fatal("rounding omitted fee")
	}
	cost, err := observePhase3KnownBuildCost(context.Background(), rpc, fixtureView(t, rpc), r, e)
	if err != nil || cost.PrincipalMicros <= 2000 {
		t.Fatal("gross debit not valued", err, cost)
	}
	if _, err := m.validateRequestPrestate(context.Background(), rpc, fixtureView(t, rpc), r, e); err != nil {
		t.Fatal(err)
	}
	// The shared fee receiver's balance moves with other borrowers' fees:
	// only this borrow's fee delta is bound (live 2026-09-29).
	binary.LittleEndian.PutUint64(fee.Data[64:72], 6)
	if _, err := m.validateRequestPrestate(context.Background(), rpc, fixtureView(t, rpc), r, e); err != nil {
		t.Fatal("shared fee receiver balance move refused the borrow", err)
	}
	binary.LittleEndian.PutUint64(fee.Data[64:72], 5)
	for _, variant := range []string{"omit_fee", "destination", "fee_authority", "conservation", "payoff", "deposit"} {
		copy := e
		copy.Accounts = append([]ExpectedAccountEffect(nil), e.Accounts...)
		request := r
		switch variant {
		case "omit_fee":
			copy.Accounts = copy.Accounts[:2]
		case "destination":
			copy.Accounts[1].Address = bridgeSquadsATA
		case "fee_authority":
			copy.Accounts[2].Authority = bridgeVault
		case "conservation":
			copy.Accounts[0].AfterRaw++
		case "payoff":
			request.FullPayoff = true
		case "deposit":
			copy.Deposit = &ExpectedDeposit{1, 1000}
		}
		if _, err := MeasureExecutableDebit(request, copy); err == nil {
			t.Fatal("malformed borrow admitted", variant)
		}
	}
	// Minimum, nearest-integer (including half-up), and zero fee semantics.
	for _, tc := range []struct{ rate, receive, want uint64 }{{0, 1, 0}, {1, 2, 1}, {1 << 52, 1000, 4}, {1 << 51, 1280, 3}} {
		binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], tc.rate)
		got, err := kaminoBorrowFee(accounts, route, tc.receive)
		if err != nil || got != tc.want {
			t.Fatal("fee rounding mismatch", tc, got, err)
		}
	}
	binary.LittleEndian.PutUint64(reserve.Data[kaminoReserveConfigOffset+40:], 1)
	if _, err := kaminoBorrowFee(accounts, route, 1); err == nil {
		t.Fatal("borrow below minimum fee accepted")
	}
}
