package meteora

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
)

var ProgramID = solana.MustPublicKeyFromBase58("LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo")

var (
	LbPairDiscriminator     = anchor.Account("LbPair")
	PositionV2Discriminator = anchor.Account("PositionV2")
	BinArrayDiscriminator   = anchor.Account("BinArray")
)

const (
	LbPairSize          = 904
	BinArraySize        = 10136
	PositionV2Size      = 8120
	BinsPerArray        = 70
	positionBaseBins    = 70
	positionBinDataSize = 112
	binSize             = 144
	scaleOffset         = 64
	basisPointMax       = 10000
)

type LbPair struct {
	ActiveID               int32
	BinStep                uint16
	Status                 uint8
	TokenXMint, TokenYMint solana.PublicKey
}

func DecodeLbPair(a *chain.Account) (LbPair, error) {
	if err := envelope(a, func(n int) bool { return n == LbPairSize }, LbPairDiscriminator, "pool"); err != nil {
		return LbPair{}, err
	}
	d := a.Data
	return LbPair{
		ActiveID: int32(binary.LittleEndian.Uint32(d[76:80])), BinStep: binary.LittleEndian.Uint16(d[80:82]), Status: d[82],
		TokenXMint: key(d, 88), TokenYMint: key(d, 120),
	}, nil
}

type RawPriceYPerX float64

func PriceOfBin(binID int32, binStep uint16) RawPriceYPerX {
	return RawPriceYPerX(math.Pow(1+float64(binStep)/basisPointMax, float64(binID)))
}

type PositionBin struct {
	LiquidityShare                             *big.Int
	FeeXPerTokenComplete, FeeYPerTokenComplete *big.Int
	FeeXPending, FeeYPending                   uint64
}

type Position struct {
	LbPair, Owner          solana.PublicKey
	LowerBinID, UpperBinID int32
	Bins                   []PositionBin
}

func DecodePosition(a *chain.Account) (Position, error) {
	if err := envelope(a, func(n int) bool { return n >= PositionV2Size }, PositionV2Discriminator, "position"); err != nil {
		return Position{}, err
	}
	d := a.Data
	p := Position{
		LbPair: key(d, 8), Owner: key(d, 40),
		LowerBinID: int32(binary.LittleEndian.Uint32(d[7912:7916])), UpperBinID: int32(binary.LittleEndian.Uint32(d[7916:7920])),
	}
	width := int(p.UpperBinID) - int(p.LowerBinID) + 1
	if width <= 0 || len(d) < PositionV2Size+max(0, width-positionBaseBins)*positionBinDataSize {
		return Position{}, fmt.Errorf("Meteora position %s width %d does not fit its account", a.Key, width)
	}
	p.Bins = make([]PositionBin, width)
	for i := range p.Bins {
		share, fee := 72+i*16, 4552+i*48
		if i >= positionBaseBins {
			share = PositionV2Size + (i-positionBaseBins)*positionBinDataSize
			fee = share + 64
		}
		p.Bins[i] = PositionBin{
			LiquidityShare:       u128(d, share),
			FeeXPerTokenComplete: u128(d, fee), FeeYPerTokenComplete: u128(d, fee+16),
			FeeXPending: binary.LittleEndian.Uint64(d[fee+32:]), FeeYPending: binary.LittleEndian.Uint64(d[fee+40:]),
		}
	}
	return p, nil
}

func (p Position) BinArrayIndexes() []int64 {
	var out []int64
	for i := BinArrayIndex(p.LowerBinID); i <= BinArrayIndex(p.UpperBinID); i++ {
		out = append(out, i)
	}
	return out
}

type Bin struct {
	AmountX, AmountY                       uint64
	LiquiditySupply                        *big.Int
	FeeXPerTokenStored, FeeYPerTokenStored *big.Int
}

type BinArray struct {
	Index  int64
	LbPair solana.PublicKey
	Bins   [BinsPerArray]Bin
}

func DecodeBinArray(a *chain.Account) (BinArray, error) {
	if err := envelope(a, func(n int) bool { return n == BinArraySize }, BinArrayDiscriminator, "bin array"); err != nil {
		return BinArray{}, err
	}
	d := a.Data
	out := BinArray{Index: int64(binary.LittleEndian.Uint64(d[8:16])), LbPair: key(d, 24)}
	for i := range out.Bins {
		b := 56 + i*binSize
		out.Bins[i] = Bin{
			AmountX: binary.LittleEndian.Uint64(d[b:]), AmountY: binary.LittleEndian.Uint64(d[b+8:]), LiquiditySupply: u128(d, b+32),
			FeeXPerTokenStored: u128(d, b+80), FeeYPerTokenStored: u128(d, b+96),
		}
	}
	return out, nil
}

func BinArrayIndex(binID int32) int64 {
	index := int64(binID) / BinsPerArray
	if binID < 0 && int64(binID)%BinsPerArray != 0 {
		index--
	}
	return index
}

func BinArrayAddress(lbPair solana.PublicKey, index int64) (solana.PublicKey, error) {
	address, _, err := solana.FindProgramAddress([][]byte{[]byte("bin_array"), lbPair[:], binary.LittleEndian.AppendUint64(nil, uint64(index))}, ProgramID)
	return address, err
}

// PositionBinAmount is the position's share of one bin and the fees it can
// claim there, floored as the program floors them.
type PositionBinAmount struct {
	BinID            int32
	LiquidityShare   *big.Int
	AmountX, AmountY *big.Int
	FeeX, FeeY       *big.Int
}

// Amounts walks the position's bins; a bin array absent from arrays holds
// empty bins.
func (p Position) Amounts(arrays map[int64]BinArray) ([]PositionBinAmount, error) {
	out := make([]PositionBinAmount, len(p.Bins))
	for i, pos := range p.Bins {
		binID := p.LowerBinID + int32(i)
		bin := Bin{LiquiditySupply: new(big.Int), FeeXPerTokenStored: new(big.Int), FeeYPerTokenStored: new(big.Int)}
		if array, ok := arrays[BinArrayIndex(binID)]; ok {
			bin = array.Bins[int64(binID)-array.Index*BinsPerArray]
		}
		feeX, err := claimable(pos.LiquidityShare, bin.FeeXPerTokenStored, pos.FeeXPerTokenComplete, pos.FeeXPending)
		if err != nil {
			return nil, fmt.Errorf("Meteora bin %d: %w", binID, err)
		}
		feeY, err := claimable(pos.LiquidityShare, bin.FeeYPerTokenStored, pos.FeeYPerTokenComplete, pos.FeeYPending)
		if err != nil {
			return nil, fmt.Errorf("Meteora bin %d: %w", binID, err)
		}
		out[i] = PositionBinAmount{
			BinID: binID, LiquidityShare: pos.LiquidityShare,
			AmountX: share(pos.LiquidityShare, bin.AmountX, bin.LiquiditySupply), AmountY: share(pos.LiquidityShare, bin.AmountY, bin.LiquiditySupply),
			FeeX: feeX, FeeY: feeY,
		}
	}
	return out, nil
}

func share(liquidity *big.Int, amount uint64, supply *big.Int) *big.Int {
	if supply.Sign() == 0 {
		return new(big.Int)
	}
	out := new(big.Int).Mul(liquidity, new(big.Int).SetUint64(amount))
	return out.Quo(out, supply)
}

func claimable(liquidity, stored, complete *big.Int, pending uint64) (*big.Int, error) {
	out := new(big.Int).SetUint64(pending)
	if liquidity.Sign() == 0 {
		return out, nil
	}
	delta := new(big.Int).Sub(stored, complete)
	if delta.Sign() < 0 {
		return nil, errors.New("fee per token stored is below the position's checkpoint")
	}
	fee := new(big.Int).Rsh(liquidity, scaleOffset)
	fee.Mul(fee, delta).Rsh(fee, scaleOffset)
	return out.Add(out, fee), nil
}

func envelope(a *chain.Account, size func(int) bool, discriminator [8]byte, name string) error {
	if a == nil {
		return fmt.Errorf("Meteora %s is missing from finalized mainnet state.", name)
	}
	if a.Owner != ProgramID {
		return fmt.Errorf("Meteora %s has an unexpected program owner.", name)
	}
	if !size(len(a.Data)) || [8]byte(a.Data[:8]) != discriminator {
		return fmt.Errorf("Meteora %s %s envelope or layout drifted", name, a.Key)
	}
	return nil
}

func key(data []byte, offset int) solana.PublicKey {
	return solana.PublicKeyFromBytes(data[offset : offset+32])
}

func u128(data []byte, offset int) *big.Int {
	le := data[offset : offset+16]
	be := make([]byte, 16)
	for i := range le {
		be[15-i] = le[i]
	}
	return new(big.Int).SetBytes(be)
}
