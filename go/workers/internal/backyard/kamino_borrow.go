package backyard

import (
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func kaminoBorrowFeeAtRate(rate, receive uint64) (uint64, error) {
	if receive == 0 || receive > math.MaxInt64 {
		return 0, staleHold("invalid_borrow_receive_amount")
	}
	if rate == 0 {
		return 0, nil
	}
	one := new(big.Int).Lsh(big.NewInt(1), kamino.FractionBits)
	feeSF := new(big.Int).Mul(new(big.Int).SetUint64(receive), new(big.Int).SetUint64(rate))
	if feeSF.Cmp(one) < 0 {
		feeSF.Set(one)
	}
	if feeSF.Cmp(new(big.Int).Mul(new(big.Int).SetUint64(receive), one)) >= 0 {
		return 0, staleHold("borrow_below_origination_fee_minimum")
	}
	fee := feeSF.Add(feeSF, new(big.Int).Rsh(new(big.Int).Set(one), 1)).Quo(feeSF, one)
	if !fee.IsUint64() || fee.Uint64() > math.MaxInt64-receive {
		return 0, staleHold("borrow_fee_overflow")
	}
	return fee.Uint64(), nil
}
