package fleet

import "fmt"

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
	vault  FleetVault
	target string
	d      Decision
}

type waveCandidates []waveCandidate

func (h waveCandidates) Len() int { return len(h) }
func (h waveCandidates) Less(i, j int) bool {
	a, b := h[i], h[j]
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
	if (a.vault.IdleTokenAccount == "") != (b.vault.IdleTokenAccount == "") {
		return a.vault.IdleTokenAccount != ""
	}
	if a.vault.Position.Mint != b.vault.Position.Mint {
		return a.vault.Position.Mint < b.vault.Position.Mint
	}
	if a.d.SourceReserve != b.d.SourceReserve {
		return a.d.SourceReserve < b.d.SourceReserve
	}
	return a.target < b.target
}

// Keep wave conflict admission and the durable execution manifest identical.
func opportunityConflictKeys(position VaultPosition, decision Decision) []string {
	source := decision.SourceReserve
	if decision.RouteKind == "idle_vault_deposit" {
		source = "idle"
	}
	keys := []string{"vault:" + position.VaultPubkey, "policy:" + fmt.Sprint(position.PolicyID), "source-reserve:" + source, "target-reserve:" + decision.TargetReserve}
	if bindings := decision.PolicyBindings; decision.RouteKind == "cross_mint_jupiter" && bindings != nil {
		keys = append(keys, "swap-policy:"+bindings.Swap.PolicyAccount, "earn-policy:"+bindings.Withdraw.PolicyAccount)
		if bindings.Deposit.PolicyAccount != bindings.Withdraw.PolicyAccount {
			keys = append(keys, "earn-policy:"+bindings.Deposit.PolicyAccount)
		}
	}
	return orderedUniqueStrings(keys)
}
