package backyard

import (
	"encoding/binary"
	"encoding/json"
	"math/big"
	"testing"
)

func topupLoanFixture(t *testing.T) (topupLoan, []ConfirmedAccount) {
	t.Helper()
	route := autoAUTOPYUSD
	sf := new(big.Int).Lsh(big.NewInt(1), 60)
	reserve := reserveFixture(t, route.Kamino.DebtReserve, route.Kamino.DebtMint, 42, sf, 1_000_000, 1_000_000)
	putKey(t, reserve.Data[32:64], route.Kamino.Market)
	binary.LittleEndian.PutUint32(reserve.Data[28:32], 1000)
	reserve.Data[kaminoReserveConfigOffset+9] = 1
	for i := 0; i < 11; i++ {
		offset := kaminoReserveConfigOffset + 64 + i*8
		if i > 0 {
			binary.LittleEndian.PutUint32(reserve.Data[offset:], 10_000)
		}
		binary.LittleEndian.PutUint32(reserve.Data[offset+4:], 7500)
	}
	obligation := obligationFixture(t, 42, 100_000_000, 1_000)
	obligation.Address = route.Kamino.Obligation
	putKey(t, obligation.Data[32:64], route.Kamino.Market)
	putKey(t, obligation.Data[96:128], route.Kamino.CollateralReserve)
	putKey(t, obligation.Data[1208:1240], route.Kamino.DebtReserve)
	binary.LittleEndian.PutUint64(obligation.Data[1288:1296], 999)
	clock := ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Data: make([]byte, 40)}
	binary.LittleEndian.PutUint64(clock.Data[:8], 42)
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
	custody := ConfirmedAccount{Address: route.DebtCustody, Owner: route.DebtTokenProgram, Lamports: 1, Data: make([]byte, 165)}
	putKey(t, custody.Data[:32], route.Kamino.DebtMint)
	putKey(t, custody.Data[32:64], bridgeVault)
	custody.Data[108] = 1
	collateral := reserveFixture(t, route.Kamino.CollateralReserve, route.Kamino.CollateralMint, 42, sf, 1_000_000_000, 1_000_000_000)
	putKey(t, collateral.Data[32:64], route.Kamino.Market)
	accounts := []ConfirmedAccount{reserve, obligation, clock, custody, collateral}
	for _, row := range []struct{ address, mint, authority, program string }{
		{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority, route.DebtTokenProgram},
		{route.CollateralCustody, route.Kamino.CollateralMint, bridgeVault, route.CollateralTokenProgram},
		{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority, route.CollateralTokenProgram},
	} {
		data := make([]byte, 165)
		putKey(t, data[:32], row.mint)
		putKey(t, data[32:64], row.authority)
		data[108] = 1
		accounts = append(accounts, ConfirmedAccount{Address: row.address, Owner: row.program, Lamports: 1, Data: data})
	}
	loan, err := captureTopupLoan(accounts, route, 42)
	if err != nil {
		t.Fatal(err)
	}
	return loan, accounts
}

func TestTopupLoanRejectsPrincipalRebaseAcrossRestart(t *testing.T) {
	loan, accounts := topupLoanFixture(t)
	raw, err := json.Marshal(loan)
	if err != nil {
		t.Fatal(err)
	}
	var restarted topupLoan
	if json.Unmarshal(raw, &restarted) != nil {
		t.Fatal("loan restart decode failed")
	}
	route := autoAUTOPYUSD
	r := accountAt(accounts, route.Kamino.DebtReserve)
	o := accountAt(accounts, route.Kamino.Obligation)
	clock := accountAt(accounts, budgetClockAddress)
	// A real rate increase and obligation refresh preserve the original
	// scaled-fraction principal, including across durable JSON recovery.
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	rate := new(big.Int).Add(one, new(big.Int).Quo(new(big.Int).Set(one), big.NewInt(1_000_000)))
	putScaledFraction(r.Data[296:328], rate)
	putScaledFraction(o.Data[1240:1272], rate)
	amount := new(big.Int).Quo(new(big.Int).Mul(littleInt(loan.DebtAmountSF[:]), rate), littleInt(loan.CumulativeBorrowRate[:]))
	putScaledFraction(o.Data[1296:1312], amount)
	binary.LittleEndian.PutUint64(clock.Data[:8], 43)
	binary.LittleEndian.PutUint64(clock.Data[32:40], 1010)
	if err := restarted.validatePrincipal(accounts, route, 43); err != nil {
		t.Fatal("validated interest was rejected", err)
	}
	for _, change := range []int64{-1, 1} {
		changed := new(big.Int).Add(amount, new(big.Int).Mul(big.NewInt(change), one))
		putScaledFraction(o.Data[1296:1312], changed)
		if err := restarted.validatePrincipal(accounts, route, 43); err == nil {
			t.Fatal("principal mutation was rebased", change)
		}
	}
	putScaledFraction(o.Data[1296:1312], amount)
	// Even a sub-token change hidden by raw ceil equality is rejected.
	putScaledFraction(o.Data[1296:1312], new(big.Int).Add(amount, big.NewInt(1)))
	if err := restarted.validatePrincipal(accounts, route, 43); err == nil {
		t.Fatal("fractional principal drift hidden by raw ceil")
	}
	putScaledFraction(o.Data[1296:1312], amount)
	binary.LittleEndian.PutUint64(o.Data[128:136], loan.CollateralRaw+1)
	if err := restarted.validatePrincipal(accounts, route, 43); err == nil {
		t.Fatal("collateral baseline changed")
	}
	binary.LittleEndian.PutUint64(o.Data[128:136], loan.CollateralRaw)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 1)
	if err := restarted.validatePrincipal(accounts, route, 43); err == nil {
		t.Fatal("borrowed residue adopted")
	}
	binary.LittleEndian.PutUint64(accountAt(accounts, route.DebtCustody).Data[64:72], 0)
	binary.LittleEndian.PutUint64(clock.Data[32:40], uint64(loan.ChainUnix+kaminoPayoffWindowSeconds+1))
	if err := restarted.validatePrincipal(accounts, route, 43); err != nil {
		t.Fatal("unchanged principal expired with an execution attempt", err)
	}
}

// This is an activation gate, not a reason to permit a principal tolerance.
// KLend a08760976f51a3a58c4a0c6ea27b4a0e565bca79 ObligationLiquidity
// calculate_amount_with_accrued_interest floors at EVERY obligation refresh.
func TestTopupLoanSequentialInterestRefreshesPreserveOriginalPrincipal(t *testing.T) {
	_, accounts := topupLoanFixture(t)
	route := autoAUTOPYUSD
	o := accountAt(accounts, route.Kamino.Obligation)
	r := accountAt(accounts, route.Kamino.DebtReserve)
	clock := accountAt(accounts, budgetClockAddress)
	one := new(big.Int).Lsh(big.NewInt(1), 60)
	amount := new(big.Int).Add(new(big.Int).Mul(big.NewInt(1000), one), new(big.Int).Quo(new(big.Int).Set(one), big.NewInt(3)))
	putScaledFraction(o.Data[1296:1312], amount)
	loan, err := captureTopupLoan(accounts, route, 42)
	if err != nil {
		t.Fatal(err)
	}
	former := new(big.Int).Set(one)
	increment := new(big.Int).Quo(new(big.Int).Set(one), big.NewInt(1_000_000))
	for i := int64(1); i <= 3; i++ {
		rate := new(big.Int).Add(one, new(big.Int).Mul(big.NewInt(i), increment))
		// This is the pinned integer multiplication/division expression; do
		// not collapse the sequence into an origin-to-latest calculation.
		amount = new(big.Int).Quo(new(big.Int).Mul(amount, rate), former)
		putScaledFraction(o.Data[1296:1312], amount)
		putScaledFraction(o.Data[1240:1272], rate)
		putScaledFraction(r.Data[296:328], rate)
		binary.LittleEndian.PutUint64(clock.Data[:8], uint64(42+i))
		binary.LittleEndian.PutUint64(clock.Data[32:40], uint64(1000+10*i))
		if err := loan.validatePrincipal(accounts, route, 42+i); err != nil {
			t.Fatalf("legitimate refresh %d rejected without any borrow/repay: %v", i, err)
		}
		former = rate
	}
}

func TestTopupLoanPrincipalBoundaryAndRejectedEvidence(t *testing.T) {
	route := autoAUTOPYUSD
	t.Run("raw ceil boundary follows SF interval", func(t *testing.T) {
		_, accounts := topupLoanFixture(t)
		o, r, clock := accountAt(accounts, route.Kamino.Obligation), accountAt(accounts, route.Kamino.DebtReserve), accountAt(accounts, budgetClockAddress)
		f := new(big.Int).Lsh(big.NewInt(1), 60)
		amount := new(big.Int).Sub(new(big.Int).Mul(big.NewInt(2), f), big.NewInt(2))
		putScaledFraction(o.Data[1296:1312], amount)
		loan, err := captureTopupLoan(accounts, route, 42)
		if err != nil {
			t.Fatal(err)
		}
		former := new(big.Int).Set(f)
		for i := int64(1); i <= 2; i++ {
			rate := new(big.Int).Add(f, big.NewInt(i))
			amount = new(big.Int).Quo(new(big.Int).Mul(amount, rate), former)
			putScaledFraction(o.Data[1296:1312], amount)
			putScaledFraction(o.Data[1240:1272], rate)
			putScaledFraction(r.Data[296:328], rate)
			binary.LittleEndian.PutUint64(clock.Data[:8], uint64(42+i))
			if err := loan.validatePrincipal(accounts, route, 42+i); err != nil {
				t.Fatal("lawful floor crossed raw ceil", err)
			}
			former = rate
		}
		actualRaw, _ := ceilScaledBigFraction(amount)
		collapsed := new(big.Int).Quo(new(big.Int).Mul(littleInt(loan.DebtAmountSF[:]), former), f)
		collapsedRaw, _ := ceilScaledBigFraction(collapsed)
		if actualRaw != 2 || collapsedRaw != 3 {
			t.Fatal("missing raw-ceil boundary witness")
		}
	})
	for name, mutate := range map[string]func([]ConfirmedAccount){
		"same-rate fractional decrease": func(a []ConfirmedAccount) {
			d := accountAt(a, route.Kamino.Obligation).Data[1296:1312]
			putScaledFraction(d, new(big.Int).Sub(littleInt(d), big.NewInt(1)))
		},
		"borrow timestamp": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, route.Kamino.Obligation).Data[1288:1296], 1000)
		},
		"marker reset": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, route.Kamino.Obligation).Data[1288:1296], 0)
		},
		"rollover timestamp": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, route.Kamino.Obligation).Data[1288:1296], 1001)
		},
		"clock regression": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[32:40], 999)
		},
		"clock slot mismatch": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, budgetClockAddress).Data[:8], 41)
		},
		"foreign reserve": func(a []ConfirmedAccount) {
			putKey(t, accountAt(a, route.Kamino.Obligation).Data[1208:1240], bridgeUSDC)
		},
		"multiple borrows": func(a []ConfirmedAccount) {
			d := accountAt(a, route.Kamino.Obligation).Data
			copy(d[1408:1608], d[1208:1408])
		},
		"virtual origin": func(a []ConfirmedAccount) {
			a[0].ValuationSource = routeRefreshValuationSource
			a[0].ValuationSlot = 42
		},
		"duplicate account": func(a []ConfirmedAccount) { a[0] = a[1] },
	} {
		t.Run(name, func(t *testing.T) {
			loan, accounts := topupLoanFixture(t)
			mutate(accounts)
			if err := loan.validatePrincipal(accounts, route, 42); err == nil {
				t.Fatal("unproven principal accepted")
			}
		})
	}
	t.Run("rounding interval cannot grow to one raw unit", func(t *testing.T) {
		_, accounts := topupLoanFixture(t)
		o := accountAt(accounts, route.Kamino.Obligation)
		putScaledFraction(o.Data[1296:1312], big.NewInt(1))
		loan, err := captureTopupLoan(accounts, route, 42)
		if err != nil {
			t.Fatal(err)
		}
		f := new(big.Int).Lsh(big.NewInt(1), 60)
		rate := new(big.Int).Mul(f, f)
		putScaledFraction(o.Data[1296:1312], f)
		putScaledFraction(o.Data[1240:1272], rate)
		putScaledFraction(accountAt(accounts, route.Kamino.DebtReserve).Data[296:328], rate)
		binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], 43)
		assertBudgetHold(t, loan.validatePrincipal(accounts, route, 43), "topup_loan_rounding_proof_unavailable")
	})
	t.Run("origin may hold an older obligation rate", func(t *testing.T) {
		_, accounts := topupLoanFixture(t)
		r := accountAt(accounts, route.Kamino.DebtReserve)
		f := new(big.Int).Lsh(big.NewInt(1), 60)
		rate := new(big.Int).Mul(big.NewInt(2), f)
		putScaledFraction(r.Data[296:328], rate)
		loan, err := captureTopupLoan(accounts, route, 42)
		if err != nil {
			t.Fatal(err)
		}
		o := accountAt(accounts, route.Kamino.Obligation)
		putScaledFraction(o.Data[1296:1312], new(big.Int).Mul(littleInt(loan.DebtAmountSF[:]), big.NewInt(2)))
		putScaledFraction(o.Data[1240:1272], rate)
		if err := loan.validatePrincipal(accounts, route, 42); err != nil {
			t.Fatal("pending refresh in origin slot rejected", err)
		}
	})
}
