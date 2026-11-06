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
	DepositVaultDiscriminator               = anchor.Instruction("deposit_vault")
	RequestWithdrawVaultDiscriminator       = anchor.Instruction("request_withdraw_vault")
	WithdrawVaultDiscriminator              = anchor.Instruction("withdraw_vault")
	CancelRequestWithdrawVaultDiscriminator = anchor.Instruction("cancel_request_withdraw_vault")
	RequestWithdrawVaultEventDiscriminator  = [8]byte{59, 94, 26, 38, 47, 131, 158, 162}
)

const (
	bps                     = 10_000
	secondsPerYear          = 365 * 24 * 60 * 60
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

func (v Vault) TotalLP(lpMintSupplyRaw uint64, now time.Time) (*big.Int, error) {
	totalLP := new(big.Int).SetUint64(lpMintSupplyRaw)
	for _, raw := range []uint64{v.FeeAccumulatorManagerRaw, v.FeeAccumulatorAdminRaw, v.FeeAccumulatorProtocolRaw, v.LPSupplyDeadWeightRaw} {
		totalLP.Add(totalLP, new(big.Int).SetUint64(raw))
	}
	feeLP, err := v.unrealisedManagementFeeLP(totalLP, now)
	if err != nil {
		return nil, err
	}
	totalLP.Add(totalLP, feeLP)
	return totalLP, nil
}

func (v Vault) LPToAssets(lpRaw, lpMintSupplyRaw uint64, now time.Time) (uint64, error) {
	totalLP, err := v.TotalLP(lpMintSupplyRaw, now)
	if err != nil {
		return 0, err
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

func ceilDiv(a, b *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func (v Vault) unrealisedManagementFeeLP(accountedLP *big.Int, now time.Time) (*big.Int, error) {
	feeBPS := uint64(v.ManagerManagementFeeBPS) + uint64(v.AdminManagementFeeBPS) + uint64(v.ProtocolManagementFeeBPS)
	elapsed := new(big.Int).Sub(big.NewInt(now.Unix()), new(big.Int).SetUint64(v.LastManagementFeeUpdateUnix))
	if v.LastManagementFeeUpdateUnix == 0 || v.TotalValueRaw == 0 || feeBPS == 0 || elapsed.Sign() <= 0 {
		return new(big.Int), nil
	}
	totalValue := new(big.Int).SetUint64(v.TotalValueRaw)
	feeAsset := new(big.Int).Mul(totalValue, elapsed)
	feeAsset.Mul(feeAsset, new(big.Int).SetUint64(feeBPS))
	feeAsset = ceilDiv(feeAsset, big.NewInt(bps*secondsPerYear))
	if feeAsset.Cmp(totalValue) >= 0 {
		return nil, errors.New("voltr accrued management fee exceeds vault value")
	}
	feeLP := new(big.Int).Mul(feeAsset, accountedLP)
	return ceilDiv(feeLP, totalValue.Sub(totalValue, feeAsset)), nil
}

// DeadWeightLP is minted to no one by the first deposit into a vault.
const DeadWeightLP = 1_000

// DepositLP is the LP the program mints for amountRaw of asset at now,
// mirroring the SDK's calculateLpForDepositAmount: accrued management fee LP
// dilutes the mint, issuance fee is kept by the vault, and the mint floors.
func (v Vault) DepositLP(lpMintSupplyRaw uint64, assetDecimals, lpDecimals uint8, amountRaw uint64, now time.Time) (uint64, error) {
	if v.IssuanceFeeBPS > bps {
		return 0, errors.New("voltr issuance fee exceeds 100%")
	}
	totalLP, err := v.TotalLP(lpMintSupplyRaw, now)
	if err != nil {
		return 0, err
	}
	amount := new(big.Int).SetUint64(amountRaw)
	var lp *big.Int
	if totalLP.Sign() == 0 {
		lp = new(big.Int).Mul(amount, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(lpDecimals)), nil))
		lp.Quo(lp, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(assetDecimals)), nil))
	} else {
		keep := big.NewInt(bps - int64(v.IssuanceFeeBPS))
		denominator := new(big.Int).Add(new(big.Int).SetUint64(v.TotalValueRaw), amount)
		denominator.Mul(denominator, big.NewInt(bps))
		denominator.Sub(denominator, new(big.Int).Mul(amount, keep))
		if denominator.Sign() <= 0 {
			return 0, errors.New("voltr deposit has no asset basis to price LP")
		}
		lp = new(big.Int).Mul(amount, totalLP)
		lp.Mul(lp, keep)
		lp.Quo(lp, denominator)
	}
	if v.LPSupplyDeadWeightRaw == 0 {
		if lp.Cmp(big.NewInt(DeadWeightLP)) < 0 {
			return 0, errors.New("voltr first deposit mints less than the dead-weight LP")
		}
		lp.Sub(lp, big.NewInt(DeadWeightLP))
	}
	if !lp.IsUint64() {
		return 0, errors.New("voltr deposit LP exceeds u64")
	}
	return lp.Uint64(), nil
}
