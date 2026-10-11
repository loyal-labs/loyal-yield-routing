package kamino

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
)

func reserveAccount(available, collateralSupply uint64) *chain.Account {
	data := make([]byte, ReserveSize)
	copy(data, ReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:], reserveVersion)
	binary.LittleEndian.PutUint64(data[224:], available)
	binary.LittleEndian.PutUint64(data[2592:], collateralSupply)
	return &chain.Account{Owner: ProgramID, Lamports: 1, Data: data}
}

// A reserve outside KLend's current layout is never decoded.
func TestDecodeReserveRejectsForeignEnvelopes(t *testing.T) {
	for name, mutate := range map[string]func(*chain.Account){
		"owner":         func(a *chain.Account) { a.Owner = FarmsProgramID },
		"executable":    func(a *chain.Account) { a.Executable = true },
		"unfunded":      func(a *chain.Account) { a.Lamports = 0 },
		"size":          func(a *chain.Account) { a.Data = a.Data[:ReserveSize-1] },
		"discriminator": func(a *chain.Account) { a.Data[0] ^= 1 },
		"version":       func(a *chain.Account) { a.Data[8] = 2 },
	} {
		account := reserveAccount(1, 1)
		mutate(account)
		if _, err := DecodeReserve(account); err == nil {
			t.Fatalf("%s drift decoded", name)
		}
	}
	if _, err := DecodeReserve(nil); err == nil {
		t.Fatal("absent reserve decoded")
	}
}

func TestCollateralToLiquidityFloorsTheWideExchangeValue(t *testing.T) {
	account := reserveAccount(10, 3)
	reserve, err := DecodeReserve(account)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reserve.CollateralToLiquidity(2); err != nil || got != 6 {
		t.Fatalf("floor: %d %v", got, err)
	}
	// A half-unit fee stays fractional until the final floor.
	binary.LittleEndian.PutUint64(account.Data[344:], 1<<59)
	if reserve, err = DecodeReserve(account); err != nil {
		t.Fatal(err)
	}
	if got, err := reserve.CollateralToLiquidity(2); err != nil || got != 6 {
		t.Fatalf("fractional fee: %d %v", got, err)
	}
	if got, err := reserve.CollateralToLiquidity(0); err != nil || got != 0 {
		t.Fatalf("zero collateral: %d %v", got, err)
	}
	// Collateral cannot exist against a reserve with no collateral supply.
	reserve.CollateralMintTotalSupply = 0
	if _, err := reserve.CollateralToLiquidity(9); err == nil {
		t.Fatal("collateral redeemed without collateral supply")
	}
	reserve.CollateralMintTotalSupply, reserve.AvailableAmount = 1, math.MaxUint64
	if _, err := reserve.CollateralToLiquidity(math.MaxInt64); err == nil {
		t.Fatal("u64 overflow accepted")
	}
	reserve.AvailableAmount = 0
	if _, err := reserve.CollateralToLiquidity(1); err == nil {
		t.Fatal("fees above liquidity accepted")
	}
	// Above 2^53 float64 would round; the exchange stays exact.
	reserve = Reserve{AvailableAmount: 1<<53 + 3, CollateralMintTotalSupply: 3}
	want := new(big.Int).Quo(big.NewInt(2*(1<<53+3)), big.NewInt(3)).Uint64()
	if got, err := reserve.CollateralToLiquidity(2); err != nil || got != want {
		t.Fatalf("exact raw: %d want %d %v", got, want, err)
	}
}

// The term-debt fields read KLend v1.25.0's layout on live accounts
// (testdata/wylds-term-debt.json, slot 455559961): the wYLDS reserve 34GJ…
// has a 30-day debt term and no market-wide maturity or early-repay
// penalty, and the Kamino Multiply obligation EsDb54… borrowed it at
// 1791697379, the block time of its loop 2cbv4X… (slot 455539692), which
// starts its term.
func TestTermDebtFieldsReadLiveAccounts(t *testing.T) {
	raw, err := os.ReadFile("testdata/wylds-term-debt.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Accounts map[string]struct {
			Owner    solana.PublicKey
			Lamports uint64
			Data     []byte
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	account := func(address string) *chain.Account {
		a := fixture.Accounts[address]
		return &chain.Account{Key: solana.MustPublicKeyFromBase58(address), Owner: a.Owner, Lamports: a.Lamports, Data: a.Data}
	}
	reserve, err := DecodeReserve(account("34GJjXf7jGTdNL8Yw1HtqWSVvfX6JTsft3UsuFFUL7VU"))
	if err != nil {
		t.Fatal(err)
	}
	if reserve.DebtTermSeconds != 2_592_000 || reserve.DebtMaturityTimestamp != 0 || reserve.EarlyRepayRemainingInterestPct != 0 {
		t.Fatalf("reserve term %d maturity %d early repay %d", reserve.DebtTermSeconds, reserve.DebtMaturityTimestamp, reserve.EarlyRepayRemainingInterestPct)
	}
	if !errors.Is(reserve.SupportedInterestTerms(), ErrUnsupportedInterestTerms) {
		t.Fatal("term debt is priced as open-term interest")
	}
	obligation, err := DecodeObligation(account("EsDb54VmT1DsgamjQtnWuZmoLAyrQCr6bsMu3AuZnies"))
	if err != nil {
		t.Fatal(err)
	}
	borrow := obligation.Borrows[0]
	if borrow.Reserve.String() != "34GJjXf7jGTdNL8Yw1HtqWSVvfX6JTsft3UsuFFUL7VU" || borrow.LastBorrowedAtTimestamp != 1_791_697_379 || borrow.BorrowedAmountAtExpiration != 0 {
		t.Fatalf("borrow slot %s last borrowed %d at expiration %d", borrow.Reserve, borrow.LastBorrowedAtTimestamp, borrow.BorrowedAmountAtExpiration)
	}
	if end, ok := borrow.DebtTermEnd(reserve); !ok || end != 1_791_697_379+2_592_000 {
		t.Fatalf("term ends %d %t", end, ok)
	}
	if _, ok := borrow.DebtTermEnd(Reserve{}); ok {
		t.Fatal("an open-term reserve has a term end")
	}
}
