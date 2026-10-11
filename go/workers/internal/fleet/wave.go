package fleet

import (
	"fmt"
	"math/bits"
)

// WaveLimits match the retained Rust planner's scheduling limits. They bound
// admission, not durable execution concurrency; retained ownership still fences
// every published opportunity independently.
type WaveLimits struct {
	MaxOpportunities          int
	MaxNotionalUSDMicros      int64
	MaxPerTenant              int
	MaxPerWritableConflictKey int
}

func DefaultWaveLimits() WaveLimits {
	return WaveLimits{128, 1_000_000_000_000_000, 64, 64}
}

type waveCandidate struct {
	// The input frontier is immutable for this synchronous planning call.
	// Keep one source view instead of copying its position for every edge.
	vault     *FleetVault
	target    string
	d         Decision
	conflicts []string
}

type waveCandidates []waveCandidate

// Less packs room by net gain per dollar moved first: when a reserve's room
// binds, the holdings that earn most per dollar of it go in. Rust's durable
// economic priority and its tie-breaks order equal ratios.
func (h waveCandidates) Less(i, j int) bool {
	a, b := h[i], h[j]
	if c := compareGainPerDollar(a.d, b.d); c != 0 {
		return c > 0
	}
	if a.d.EconomicPriority != b.d.EconomicPriority {
		return a.d.EconomicPriority > b.d.EconomicPriority
	}
	// Annual gain is exactly lost-yield/hour * 8760 in Plan. Rust uses that
	// lost-yield tie-break before net holding gain, not after it.
	if a.d.AnnualYieldGainUSDMicros != b.d.AnnualYieldGainUSDMicros {
		return a.d.AnnualYieldGainUSDMicros > b.d.AnnualYieldGainUSDMicros
	}
	if a.d.ExpectedNetGainUSDMicros != b.d.ExpectedNetGainUSDMicros {
		return a.d.ExpectedNetGainUSDMicros > b.d.ExpectedNetGainUSDMicros
	}
	if a.d.PrincipalUSDMicros != b.d.PrincipalUSDMicros {
		return a.d.PrincipalUSDMicros > b.d.PrincipalUSDMicros
	}
	if a.vault.Position.ObservedSlot != b.vault.Position.ObservedSlot {
		return a.vault.Position.ObservedSlot > b.vault.Position.ObservedSlot
	}
	// Rust assigns candidate IDs after sorting its normalized source frontier.
	if a.d.VaultID != b.d.VaultID {
		return a.d.VaultID < b.d.VaultID
	}
	if a.vault.Position.Mint != b.vault.Position.Mint {
		return a.vault.Position.Mint < b.vault.Position.Mint
	}
	if a.d.SourceReserve != b.d.SourceReserve {
		return a.d.SourceReserve < b.d.SourceReserve
	}
	return a.target < b.target
}

// compareGainPerDollar orders a.net/a.principal against b.net/b.principal
// exactly, by cross-multiplying in 128 bits: 1 when a earns more per dollar.
// Only eligible decisions are ranked, and theirs are both positive.
func compareGainPerDollar(a, b Decision) int {
	if a.ExpectedNetGainUSDMicros <= 0 || a.PrincipalUSDMicros <= 0 || b.ExpectedNetGainUSDMicros <= 0 || b.PrincipalUSDMicros <= 0 {
		return 0
	}
	leftHigh, leftLow := bits.Mul64(uint64(a.ExpectedNetGainUSDMicros), uint64(b.PrincipalUSDMicros))
	rightHigh, rightLow := bits.Mul64(uint64(b.ExpectedNetGainUSDMicros), uint64(a.PrincipalUSDMicros))
	switch {
	case leftHigh != rightHigh:
		if leftHigh > rightHigh {
			return 1
		}
		return -1
	case leftLow != rightLow:
		if leftLow > rightLow {
			return 1
		}
		return -1
	}
	return 0
}

// Keep wave conflict admission and the durable execution manifest identical.
func opportunityConflictKeys(position VaultPosition, decision Decision) []string {
	keys := []string{"vault:" + position.VaultPubkey, "policy:" + fmt.Sprint(position.PolicyID), "source-reserve:" + decision.SourceReserve, "target-reserve:" + decision.TargetReserve}
	if bindings := decision.PolicyBindings; decision.RouteKind == "cross_mint_jupiter" && bindings != nil {
		keys = append(keys, "swap-policy:"+bindings.Swap.PolicyAccount, "earn-policy:"+bindings.Withdraw.PolicyAccount)
		if bindings.Deposit.PolicyAccount != bindings.Withdraw.PolicyAccount {
			keys = append(keys, "earn-policy:"+bindings.Deposit.PolicyAccount)
		}
	}
	return orderedUniqueStrings(keys)
}
