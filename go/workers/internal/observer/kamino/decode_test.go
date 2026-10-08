package kamino

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func TestDecodeReserveMatchesKlendLayout(t *testing.T) {
	data := make([]byte, 8+reserveStructSize)
	discriminator := sha256.Sum256([]byte("account:Reserve"))
	copy(data[:8], discriminator[:8])
	body := data[8:]
	market := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	copy(body[24:56], market[:])
	liquidity := body[liquidityOffset : liquidityOffset+1232]
	copy(liquidity[:32], mint[:])
	binary.LittleEndian.PutUint64(body[8:16], 99)
	body[16] = 1
	body[17] = 7
	binary.LittleEndian.PutUint64(liquidity[96:104], 100)
	putFraction(liquidity[104:120], 50)
	putFraction(liquidity[120:136], 2)
	binary.LittleEndian.PutUint64(liquidity[136:144], 1234)
	binary.LittleEndian.PutUint64(liquidity[144:152], 6)
	putFraction(liquidity[216:232], 5)
	putFraction(liquidity[232:248], 2)
	putFraction(liquidity[248:264], 3)
	for index := range 4 {
		binary.LittleEndian.PutUint64(liquidity[168+index*8:], uint64(index+1))
	}
	config := body[configOffset : configOffset+952]
	config[0] = 2
	binary.LittleEndian.PutUint16(config[2:4], 25)
	config[8] = 1
	config[14] = 10
	config[16] = 70
	config[17] = 80
	for index := range 11 {
		binary.LittleEndian.PutUint32(config[64+index*8:], uint32(index*1000))
		binary.LittleEndian.PutUint32(config[68+index*8:], uint32(index*100))
	}
	binary.LittleEndian.PutUint64(config[152:160], 110)
	binary.LittleEndian.PutUint64(config[160:168], 1000)
	binary.LittleEndian.PutUint64(config[168:176], 500)
	copy(config[176:208], []byte("FIXTURE"))
	config[644] = 1
	config[645] = 91
	binary.LittleEndian.PutUint64(config[648:656], 300)
	binary.LittleEndian.PutUint64(body[borrowedOutsideOffset:], 12)
	observedAt := time.Unix(1_700_000_000, 0).UTC()
	marketString, mintString := market.String(), mint.String()
	snapshot, err := Decode(Target{Reserve: solana.NewWallet().PublicKey().String(), Market: &marketString, LiquidityMint: &mintString}, 200, observedAt, data, 400)
	if err != nil {
		t.Fatalf("decode fixture reserve: %v", err)
	}
	if snapshot.ReserveLastUpdateSlot != 99 || !snapshot.ReserveLastUpdateStale || snapshot.ReservePriceStatus != 7 {
		t.Fatalf("last update decoded incorrectly: %+v", snapshot)
	}
	if snapshot.AvailableAmount != 100 || snapshot.BorrowedAmount != 50 || snapshot.TotalSupplyAmount != 140 {
		t.Fatalf("liquidity decoded incorrectly: available=%f borrowed=%f supply=%f", snapshot.AvailableAmount, snapshot.BorrowedAmount, snapshot.TotalSupplyAmount)
	}
	if math.Abs(snapshot.Utilization-50.0/140.0) > 1e-12 {
		t.Fatalf("utilization = %f", snapshot.Utilization)
	}
	if snapshot.MarketPriceUSD != 2 || snapshot.MintDecimals != 6 || snapshot.Symbol == nil || *snapshot.Symbol != "FIXTURE" {
		t.Fatalf("token metadata decoded incorrectly: %+v", snapshot)
	}
	if snapshot.BorrowFactorPct != 110 || snapshot.DepositLimit != 1000 || snapshot.BorrowLimit != 500 || snapshot.BorrowedAmountOutsideElevationGroup != 12 {
		t.Fatalf("reserve config decoded incorrectly: %+v", snapshot)
	}
	if snapshot.CumulativeBorrowRateBSF != [4]uint64{1, 2, 3, 4} || snapshot.BorrowRateCurve[10].BorrowRateBPS != 1000 {
		t.Fatalf("rate state decoded incorrectly: %+v", snapshot)
	}
	if snapshot.BorrowAPR <= 0 || snapshot.BorrowAPY <= snapshot.BorrowAPR || snapshot.SupplyAPY <= snapshot.SupplyAPR {
		t.Fatalf("APY calculation invalid: borrow=%f/%f supply=%f/%f", snapshot.BorrowAPR, snapshot.BorrowAPY, snapshot.SupplyAPR, snapshot.SupplyAPY)
	}
}

func putFraction(destination []byte, integer int64) {
	value := new(big.Int).Lsh(big.NewInt(integer), fractionBits)
	bytes := value.Bytes()
	for index := range bytes {
		destination[index] = bytes[len(bytes)-1-index]
	}
}
