package kamino

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
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
