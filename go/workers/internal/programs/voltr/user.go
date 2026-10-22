package voltr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

var (
	DepositVaultDiscriminator              = anchor.Instruction("deposit_vault")
	RequestWithdrawVaultDiscriminator      = anchor.Instruction("request_withdraw_vault")
	WithdrawVaultDiscriminator             = anchor.Instruction("withdraw_vault")
	RequestWithdrawVaultEventDiscriminator = [8]byte{59, 94, 26, 38, 47, 131, 158, 162}
)

const (
	bps                     = 10_000
	requestEventLPOffset    = 114
	requestEventAssetOffset = 122
	requestEventSize        = 138
)

type User struct {
	Receipt, LPATA solana.PublicKey
}

func UserAccounts(vault, lpMint, authority solana.PublicKey) (User, error) {
	receipt, _, err := WithdrawalReceiptAddress(vault, authority)
	if err != nil {
		return User{}, err
	}
	lpATA, err := spl.AssociatedTokenAddress(authority, lpMint, solana.TokenProgramID)
	if err != nil {
		return User{}, err
	}
	return User{Receipt: receipt, LPATA: lpATA}, nil
}

type RequestEvent struct {
	EscrowedLPRaw  uint64
	RequestedAsset U80F48
}

func DecodeRequestEvent(data []byte) (RequestEvent, bool) {
	if len(data) < requestEventSize || !bytes.HasPrefix(data, RequestWithdrawVaultEventDiscriminator[:]) {
		return RequestEvent{}, false
	}
	return RequestEvent{
		EscrowedLPRaw:  binary.LittleEndian.Uint64(data[requestEventLPOffset:]),
		RequestedAsset: U80F48(data[requestEventAssetOffset:requestEventSize]),
	}, true
}

func (v U80F48) FloorRaw() (uint64, bool) {
	raw := new(big.Int).Rsh(v.Big(), 48)
	return raw.Uint64(), raw.IsUint64()
}

func (v Vault) lockedProfit(now time.Time) *big.Int {
	duration := new(big.Int).SetUint64(v.LockedProfitDegradationSeconds)
	since := new(big.Int).Sub(big.NewInt(now.Unix()), new(big.Int).SetUint64(v.LastLockedProfitReportUnix))
	switch {
	case duration.Sign() == 0 || since.Cmp(duration) >= 0:
		return new(big.Int)
	case since.Sign() <= 0:
		return new(big.Int).SetUint64(v.LastUpdatedLockedProfitRaw)
	}
	remaining := new(big.Int).Sub(duration, since)
	locked := new(big.Int).Mul(new(big.Int).SetUint64(v.LastUpdatedLockedProfitRaw), remaining)
	return locked.Quo(locked, duration)
}

func (v Vault) LPToAssets(lpRaw, lpMintSupplyRaw uint64, now time.Time) (uint64, error) {
	totalLP := new(big.Int).SetUint64(lpMintSupplyRaw)
	for _, raw := range []uint64{v.FeeAccumulatorManagerRaw, v.FeeAccumulatorAdminRaw, v.FeeAccumulatorProtocolRaw, v.LPSupplyDeadWeightRaw} {
		totalLP.Add(totalLP, new(big.Int).SetUint64(raw))
	}
	if totalLP.Sign() == 0 {
		return 0, nil
	}
	unlocked := new(big.Int).Sub(new(big.Int).SetUint64(v.TotalValueRaw), v.lockedProfit(now))
	keep := big.NewInt(bps - int64(v.RedemptionFeeBPS))
	assets := new(big.Int).Mul(new(big.Int).SetUint64(lpRaw), unlocked)
	assets.Mul(assets, keep)
	assets.Quo(assets, totalLP.Mul(totalLP, big.NewInt(bps)))
	if assets.Sign() < 0 || !assets.IsUint64() {
		return 0, errors.New("voltr vault value does not redeem LP to a u64 asset amount")
	}
	return assets.Uint64(), nil
}
