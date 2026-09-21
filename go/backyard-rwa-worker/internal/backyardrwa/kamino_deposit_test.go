package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

func TestPhase3DepositRoundingMatchesProduction(t *testing.T) {
	dir, name := os.Getenv("PHASE3_JUPITER_RETURN_PROBE_DIR"), os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if dir == "" || name == "" {
		t.Skip("explicit linked deployed-program evidence required")
	}
	if filepath.Base(name) != name {
		t.Fatal("result must be a local filename")
	}
	var plan struct {
		LendingPrelude struct {
			Steps []struct{ Request KaminoPrimeUSDCRequest }
		}
	}
	planData, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil || json.Unmarshal(planData, &plan) != nil || len(plan.LendingPrelude.Steps) != 4 {
		t.Fatal("linked plan missing", err)
	}
	type captured struct {
		Address, Owner, DataBase64, DataSHA256 string
		Lamports                               uint64
		Present                                bool
	}
	var result struct {
		Slot                       int64
		PlanSHA256, SnapshotSHA256 string
		DepositRoundingProbes      []struct {
			RequestedRaw, ActualDebitRaw, ReceiptRaw uint64
			WireBase64, WireSHA256                   string
			Before, After                            []captured
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	snapshot, snapshotErr := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil || snapshotErr != nil || json.Unmarshal(data, &result) != nil || result.PlanSHA256 != sha256Bytes(planData) || result.SnapshotSHA256 != sha256Bytes(snapshot) || len(result.DepositRoundingProbes) != 2 {
		t.Fatal("rounding witness identity mismatch", err)
	}
	decode := func(rows []captured) []ConfirmedAccount {
		var accounts []ConfirmedAccount
		for _, a := range rows {
			if !a.Present {
				continue
			}
			b, e := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
			if e != nil || sha256Bytes(b) != a.DataSHA256 {
				t.Fatal("rounding account hash mismatch", e)
			}
			accounts = append(accounts, ConfirmedAccount{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Data: b})
		}
		return accounts
	}
	route := ethenaUSDePYUSD
	for i, probe := range result.DepositRoundingProbes {
		if probe.RequestedRaw != []uint64{1_000_000, 99_999_999}[i] {
			t.Fatal("unexpected rounding probe")
		}
		r := plan.LendingPrelude.Steps[0].Request
		r.AmountRaw, r.Data = probe.RequestedRaw, append([]byte(nil), r.Data...)
		binary.LittleEndian.PutUint64(r.Data[8:], r.AmountRaw)
		message, err := CompileKaminoMessage(r)
		wire, decodeErr := base64.StdEncoding.Strict().DecodeString(probe.WireBase64)
		if err != nil || decodeErr != nil || len(wire) <= 65 || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != probe.WireSHA256 || !bytes.Equal(message, wire[65:]) {
			t.Fatal("deposit wire differs from Go", err, decodeErr)
		}
		before, after := decode(probe.Before), decode(probe.After)
		effects, err := boundedKaminoDepositEffects(before, route, result.Slot, r.AmountRaw)
		if err != nil {
			t.Fatal(err)
		}
		debit, err := MeasureExecutableDebit(r, effects)
		if err != nil || debit.Raw != r.AmountRaw {
			t.Fatal("rounded deposit reduced cap debit", err)
		}
		receipt := ConfirmedTransactionEvidence{Signature: "local-zero-signature-deposit-proof", Slot: result.Slot}
		for j, accounts := range [][]ConfirmedAccount{before, after} {
			for _, effect := range effects.Accounts {
				a := accountAt(accounts, effect.Address)
				mint, _ := decodeBase58PublicKey(effect.Mint)
				authority, _ := decodeBase58PublicKey(effect.Authority)
				custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
				if err != nil {
					t.Fatal(err)
				}
				balance := TransactionTokenBalance{Address: a.Address, OwnerProgram: a.Owner, Mint: effect.Mint, Authority: effect.Authority, Raw: custody.Raw}
				if j == 0 {
					receipt.PreTokenBalances = append(receipt.PreTokenBalances, balance)
				} else {
					receipt.PostTokenBalances = append(receipt.PostTokenBalances, balance)
				}
			}
		}
		if receipt.PreTokenBalances[0].Raw-receipt.PostTokenBalances[0].Raw != probe.ActualDebitRaw {
			t.Fatal("reported deposit debit differs from actual custody")
		}
		if _, _, err := ReconcileConfirmedTransaction(effects, receipt); err != nil {
			t.Fatal("real rounded deposit rejected", err)
		}
		bad := receipt
		bad.PostTokenBalances = append([]TransactionTokenBalance(nil), receipt.PostTokenBalances...)
		outside := effects.Deposit.MinimumDebitRaw - 1
		bad.PostTokenBalances[0].Raw = receipt.PreTokenBalances[0].Raw - outside
		bad.PostTokenBalances[1].Raw = receipt.PreTokenBalances[1].Raw + outside
		if _, _, err := ReconcileConfirmedTransaction(effects, bad); err == nil {
			t.Fatal("conserved but out-of-bound debit reconciled")
		}
		if i == 0 {
			if probe.ActualDebitRaw != 999_999 {
				t.Fatal("missing real one-unit rounding witness")
			}
			exact := effects
			exact.Kind, exact.Deposit = "", nil
			if _, _, err := ReconcileConfirmedTransaction(exact, receipt); err == nil {
				t.Fatal("old exact-debit assumption did not reject negative control")
			}
		}
		// Reconstruct the refreshed exchange rate from the actual post-deposit
		// reserve, removing only the executed deposit and minted receipts.
		post, err := decodeKaminoReserve(accountAt(after, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if err != nil {
			t.Fatal(err)
		}
		pre, err := decodeKaminoReserve(accountAt(before, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
		if err != nil {
			t.Fatal(err)
		}
		if post.collateralMintSupply-pre.collateralMintSupply != probe.ReceiptRaw {
			t.Fatal("minted receipt mismatch")
		}
		refreshed := new(big.Int).Sub(post.totalLiquiditySF, new(big.Int).Lsh(new(big.Int).SetUint64(probe.ActualDebitRaw), 60))
		n := new(big.Int).Lsh(new(big.Int).Mul(new(big.Int).SetUint64(r.AmountRaw), new(big.Int).SetUint64(pre.collateralMintSupply)), 60)
		if n.Quo(n, refreshed).Uint64() != probe.ReceiptRaw {
			t.Fatal("deployed receipt rounding differs from refreshed-rate equation")
		}
		t.Logf("PHASE3_DEPOSIT_ROUNDING requested=%d actual=%d minimum=%d receipts=%d capDebit=%d", r.AmountRaw, probe.ActualDebitRaw, effects.Deposit.MinimumDebitRaw, probe.ReceiptRaw, debit.Raw)
	}
}

func TestDepositRoundingBoundsRevalidateCustodyAndRejectMalformedEffects(t *testing.T) {
	_, _, _, manifest, _, _, accounts := fundingAdmissionFixture(t, 20_000)
	route := ethenaUSDePYUSD
	reserve := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, new(big.Int).Lsh(big.NewInt(1), 60), 1_100_000_000, 1_000_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint64(reserve.Data[272:280], 9)
	binary.LittleEndian.PutUint64(reserve.Data[264:272], 1000)
	binary.LittleEndian.PutUint32(reserve.Data[28:32], 1000)
	reserve.Data[kaminoReserveConfigOffset+9] = 1
	for i := 0; i < 11; i++ {
		o := kaminoReserveConfigOffset + 64 + i*8
		if i > 0 {
			binary.LittleEndian.PutUint32(reserve.Data[o:], 10_000)
		}
		binary.LittleEndian.PutUint32(reserve.Data[o+4:], 7500)
	}
	accounts = append(accounts, reserve)
	r, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 1_000_000, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	effects, err := boundedKaminoDepositEffects(accounts, route, 42, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, accounts)
	if _, err = validateDepositRequest(context.Background(), rpc, r, effects, 42); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 20_000_001)
	_, err = validateDepositRequest(context.Background(), rpc, r, effects, 42)
	assertBudgetHold(t, err, "deposit_custody_changed")
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 20_000_000)
	binary.LittleEndian.PutUint64(reserve.Data[2592:2600], 500_000_000)
	_, err = validateDepositRequest(context.Background(), rpc, r, effects, 42)
	assertBudgetHold(t, err, "deposit_rounding_window_changed")
	for _, mutate := range []func(*ExpectedEffects){
		func(e *ExpectedEffects) { e.Kind = "kamino-repay" },
		func(e *ExpectedEffects) { e.Repayment = &ExpectedRepayment{1, 1_000_000} },
		func(e *ExpectedEffects) { e.Deposit.MinimumDebitRaw = 0 },
		func(e *ExpectedEffects) { e.Accounts[1].AfterRaw++ },
	} {
		copy := effects
		d := *effects.Deposit
		copy.Deposit = &d
		copy.Accounts = append([]ExpectedAccountEffect(nil), effects.Accounts...)
		mutate(&copy)
		if _, err := MeasureExecutableDebit(r, copy); err == nil {
			t.Fatal("invalid deposit effect accepted")
		}
	}
}
