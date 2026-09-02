package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	reserveConfigOffset        = 4856
	maximumEconomicSlotLag     = int64(1_500)
	minimumPublicationLifetime = 70 * time.Second
)

type curvePoint struct{ utilization, rate float64 }

func DecodeKaminoReserve(account Account, identity ReserveIdentity, contextSlot int64, slotDuration time.Duration) (ReserveState, error) {
	if account.Address != identity.Address || account.Owner != KaminoProgram || account.Executable || account.Lamports == 0 ||
		len(account.Data) != reserveLength || !bytes.Equal(account.Data[:8], reserveDiscriminator[:]) {
		return ReserveState{}, fmt.Errorf("reserve %s envelope or layout drifted", identity.Address)
	}
	if binary.LittleEndian.Uint64(account.Data[8:16]) != 1 || !samePublicKey(account.Data[32:64], identity.Market) || !samePublicKey(account.Data[128:160], identity.Mint) {
		return ReserveState{}, fmt.Errorf("reserve %s identity drifted", identity.Address)
	}
	if contextSlot <= 0 || slotDuration <= 0 {
		return ReserveState{}, fmt.Errorf("confirmed context and slot duration are required")
	}
	lastUpdateSlotRaw := binary.LittleEndian.Uint64(account.Data[16:24])
	if lastUpdateSlotRaw == 0 || lastUpdateSlotRaw > math.MaxInt64 {
		return ReserveState{}, fmt.Errorf("reserve %s has no bounded last update", identity.Address)
	}
	lastUpdateSlot := int64(lastUpdateSlotRaw)
	if account.Data[24] != 0 {
		return ReserveState{}, fmt.Errorf("reserve %s last update is explicitly stale", identity.Address)
	}
	lag := contextSlot - lastUpdateSlot
	if lag < 0 || lag > maximumEconomicSlotLag {
		return ReserveState{}, fmt.Errorf("reserve %s economic slot order or lag is invalid", identity.Address)
	}
	economicLifetime := time.Duration(maximumEconomicSlotLag-lag) * slotDuration
	if economicLifetime < minimumPublicationLifetime {
		return ReserveState{}, fmt.Errorf("reserve %s economic evidence has insufficient publication lifetime", identity.Address)
	}
	status := account.Data[reserveConfigOffset]
	emergency := account.Data[reserveConfigOffset+8]
	if status != 0 || emergency != 0 {
		return ReserveState{}, fmt.Errorf("reserve %s is not active", identity.Address)
	}
	decimals := binary.LittleEndian.Uint64(account.Data[272:280])
	if decimals != 6 {
		return ReserveState{}, fmt.Errorf("phase 1 reserve %s is not six-decimal USDC", identity.Address)
	}

	available := float64(binary.LittleEndian.Uint64(account.Data[224:232]))
	borrowed := scaledFraction(account.Data[232:248])
	protocolFees := scaledFraction(account.Data[344:360])
	referrerFees := scaledFraction(account.Data[360:376])
	pendingFees := scaledFraction(account.Data[376:392])
	totalSupply := math.Max(0, available+borrowed-protocolFees-referrerFees-pendingFees)
	if !finite(totalSupply) || totalSupply <= 0 || totalSupply > float64(math.MaxInt64) {
		return ReserveState{}, fmt.Errorf("reserve %s supply is invalid", identity.Address)
	}
	utilization := borrowed / totalSupply
	if !finite(utilization) || utilization < 0 || utilization > 1.01 {
		return ReserveState{}, fmt.Errorf("reserve %s utilization is invalid", identity.Address)
	}

	config := account.Data[reserveConfigOffset:]
	takeRate := config[14]
	if takeRate > 100 {
		return ReserveState{}, fmt.Errorf("reserve %s take rate is invalid", identity.Address)
	}
	points := make([]curvePoint, 0, 11)
	for index := 0; index < 11; index++ {
		offset := 64 + index*8
		points = append(points, curvePoint{
			utilization: float64(binary.LittleEndian.Uint32(config[offset:offset+4])) / 10_000,
			rate:        float64(binary.LittleEndian.Uint32(config[offset+4:offset+8])) / 10_000,
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].utilization < points[j].utilization })
	curveAPR := curveRate(points, utilization) * (1000 / 2 / float64(slotDuration.Milliseconds()))
	// KLend's host fixed rate is borrower-only and does not accrue to suppliers.
	supplyAPR := utilization * curveAPR * (1 - float64(takeRate)/100)
	periods := 365.25 * 24 * 60 * 60 * 1000 / float64(slotDuration.Milliseconds())
	supplyAPY := 0.0
	if supplyAPR > 0 {
		supplyAPY = math.Pow(1+supplyAPR/periods, periods) - 1
	}
	if !finite(supplyAPY) || supplyAPY < 0 || supplyAPY >= 0.5 {
		return ReserveState{}, fmt.Errorf("reserve %s APY is outside the production bound", identity.Address)
	}
	hash := sha256.Sum256(account.Data)
	return ReserveState{
		ReserveIdentity: identity, Slot: contextSlot, LastUpdateSlot: lastUpdateSlot,
		SupplyAPYBPS:           int64(math.Round(supplyAPY * 10_000)),
		TotalSupplyUSDMicros:   int64(math.Floor(totalSupply)),
		EconomicLifetimeMillis: economicLifetime.Milliseconds(),
		DataHash:               hex.EncodeToString(hash[:]),
	}, nil
}

func scaledFraction(value []byte) float64 {
	low := binary.LittleEndian.Uint64(value[:8])
	high := binary.LittleEndian.Uint64(value[8:16])
	return float64(high)*16 + float64(low)/float64(uint64(1)<<60)
}

func curveRate(points []curvePoint, utilization float64) float64 {
	if len(points) == 0 {
		return 0
	}
	if utilization <= points[0].utilization {
		return points[0].rate
	}
	for index := 1; index < len(points); index++ {
		floor, ceiling := points[index-1], points[index]
		if utilization <= ceiling.utilization {
			width := ceiling.utilization - floor.utilization
			if width <= math.SmallestNonzeroFloat64 {
				return ceiling.rate
			}
			return floor.rate + (ceiling.rate-floor.rate)*(utilization-floor.utilization)/width
		}
	}
	return points[len(points)-1].rate
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
