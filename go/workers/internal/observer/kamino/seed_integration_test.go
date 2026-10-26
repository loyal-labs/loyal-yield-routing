package kamino

import (
	"encoding/binary"

	klend "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// reserveAccount is a KLend reserve whose last refresh happened at
// lastUpdateSlot with the given available liquidity.
func reserveAccount(market, mint solana.PublicKey, lastUpdateSlot, available uint64) []byte {
	data := make([]byte, klend.ReserveSize)
	copy(data, klend.ReserveDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 1)
	body := data[8:]
	copy(body[24:56], market[:])
	binary.LittleEndian.PutUint64(body[8:16], lastUpdateSlot)
	liquidity := data[128:1360]
	copy(liquidity[:32], mint[:])
	binary.LittleEndian.PutUint64(liquidity[96:104], available)
	putFraction(liquidity[104:120], 50)
	putFraction(liquidity[120:136], 1)
	binary.LittleEndian.PutUint64(liquidity[144:152], 6)
	for index := range 4 {
		binary.LittleEndian.PutUint64(liquidity[168+index*8:], uint64(index+1))
	}
	return data
}
