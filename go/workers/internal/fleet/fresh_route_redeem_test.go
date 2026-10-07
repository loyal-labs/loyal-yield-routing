package fleet

import (
	"encoding/binary"
	"math/big"
	"math/rand"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
)

// Root cause of the vault idle residue Autodeposit tolerates (9467ebf5,
// 2089d5fa, ASK-2164): the route withdraws all source collateral but
// deposited the planning estimate, so whatever the collateral redeems beyond
// it (interest accrued since planning) stayed in the vault ATA, and nothing
// drains idle. Here the collateral is worth 1.25e9 but was planned at 1e9.
func TestFreshRouteDepositsWhatTheWithdrawalRedeems(t *testing.T) {
	fresh, _ := loadFixtureFreshRoute(t, 1_600_000_000_000)
	if fresh.input.WithdrawCollateralAmount != 1_000_000_000 || fresh.input.DepositLiquidityAmount != 1_250_000_000 {
		t.Fatalf("withdraws %d collateral and deposits %d, want all of it and its 1250000000 redemption",
			fresh.input.WithdrawCollateralAmount, fresh.input.DepositLiquidityAmount)
	}
}

// klendRedeem models KLend's withdraw_obligation_collateral_and_redeem: the
// CollateralExchangeRate is the U68F60 Fraction mint_total_supply /
// total_supply, and collateral_to_liquidity is (collateral / rate).to_floor().
// U68F60 division truncates. This is a model of KLend's math, not KLend.
func klendRedeem(collateral, collateralSupply uint64, totalSupplySF *big.Int) uint64 {
	rate := new(big.Int).Lsh(new(big.Int).SetUint64(collateralSupply), 120)
	rate.Quo(rate, totalSupplySF)
	liquidity := new(big.Int).Lsh(new(big.Int).SetUint64(collateral), 120)
	liquidity.Quo(liquidity, rate)
	return liquidity.Rsh(liquidity, 60).Uint64()
}

func putU128(data []byte, value *big.Int) {
	bytes := value.FillBytes(make([]byte, 16))
	for i := range 16 {
		data[i] = bytes[15-i]
	}
}

// The deposit can never consume pre-existing idle custody: KLend redeems at
// least the exact floor KaminoRedeemableLiquidity computes from the same
// reserve (the truncated rate only rounds the redemption up), and the route's
// own refresh accrues interest, which only raises it. Verified against the
// model above over random reserves, not against the connected-SVM harness.
func TestKLendRedeemsAtLeastTheDepositedFloor(t *testing.T) {
	identity := ReserveIdentity{Address: testIdentity(1), Market: testIdentity(40), Mint: USDCMint}
	random := rand.New(rand.NewSource(7))
	for range 20_000 {
		available := uint64(random.Int63n(1e13))
		borrowedSF := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1e13), 60))
		feesSF := new(big.Int).Rand(random, new(big.Int).Add(borrowedSF, big.NewInt(1)))
		supply := uint64(random.Int63n(1e13)) + 1
		collateral := uint64(random.Int63n(int64(supply))) + 1
		account := reserveFixture(identity, available, 0)
		putU128(account.Data[232:248], borrowedSF)
		putU128(account.Data[344:360], feesSF)
		putU128(account.Data[360:376], big.NewInt(0))
		putU128(account.Data[376:392], big.NewInt(0))
		binary.LittleEndian.PutUint64(account.Data[2592:2600], supply)
		total := new(big.Int).Add(borrowedSF, new(big.Int).Lsh(new(big.Int).SetUint64(available), 60))
		total.Sub(total, feesSF)
		if total.Sign() <= 0 {
			continue
		}
		deposit, err := backyard.KaminoRedeemableLiquidity(backyard.ConfirmedAccount{Address: account.Address, Owner: account.Owner, Lamports: account.Lamports, Data: account.Data}, identity.Market, USDCMint, collateral)
		if err != nil {
			t.Fatal(err)
		}
		redeemed := klendRedeem(collateral, supply, total)
		// The refresh inside the route accrues interest; the protocol keeps
		// at most its take of it.
		interest := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1e9), 60))
		take := new(big.Int).Quo(new(big.Int).Mul(interest, big.NewInt(random.Int63n(10_001))), big.NewInt(10_000))
		accrued := new(big.Int).Add(total, new(big.Int).Sub(interest, take))
		if afterRefresh := klendRedeem(collateral, supply, accrued); deposit > redeemed || redeemed > afterRefresh {
			t.Fatalf("collateral %d of supply %d: deposit %d, redeemed %d, after refresh %d", collateral, supply, deposit, redeemed, afterRefresh)
		}
	}
}
