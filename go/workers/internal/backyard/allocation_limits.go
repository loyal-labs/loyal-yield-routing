package backyard

import "time"

// Admission limits never request a sale of existing collateral.
type allocationLimits struct {
	CashReserveRaw  int64 `json:"cashReserveRaw"`
	ProviderRoomRaw int64 `json:"providerRoomRaw"`
	EquityRoomRaw   int64 `json:"equityRoomRaw"`
	UnleveredEntry  bool  `json:"unleveredEntry"`
}

func (p SelectorPolicy) unleveredEntry(lane string) bool {
	return p.DiversifyNewCash && (lane == MapleSyrupUSDCLaneID || lane == primePRIMEUSDCTemplate.Lane ||
		lane == primePRIMEPYUSDTemplate.Lane || lane == primePRIMEUSDSTemplate.Lane)
}

func providerGroup(lane string) string {
	l, ok := findEarnLane(lane)
	if !ok {
		return ""
	}
	if l.route.Kamino.CollateralMint == autoAUTOPYUSDTemplate.Kamino.CollateralMint || l.route.Kamino.CollateralMint == primePRIMEUSDCTemplate.Kamino.CollateralMint {
		return "hastra"
	}
	return l.route.Kamino.CollateralMint
}

func allocationGroup(lane string) string {
	l, ok := findEarnLane(lane)
	if !ok {
		return ""
	}
	switch mint := l.route.Kamino.CollateralMint; mint {
	case onreONycUSDCRouteTemplate.Kamino.CollateralMint, autoAUTOPYUSDTemplate.Kamino.CollateralMint:
		return "onre_auto"
	case mapleSyrupUSDCUSDCTemplate.Kamino.CollateralMint, primePRIMEUSDCTemplate.Kamino.CollateralMint:
		return mint
	default:
		return ""
	}
}

func newAllocationLimits(s Snapshot, lanes []Snapshot, p SelectorPolicy, destination string) allocationLimits {
	limits := allocationLimits{CashReserveRaw: p.idleBufferRaw(s.TotalVaultNAVRaw), UnleveredEntry: p.unleveredEntry(destination)}
	provider, group := providerGroup(destination), allocationGroup(destination)
	if !s.netKnown(time.Now()) || provider == "" || group == "" || s.TotalVaultNAVRaw <= 0 || s.TotalVaultNAVRaw > 1<<53 || s.WithdrawalDemandRaw < 0 || s.WithdrawalDemandRaw > 1<<53 {
		return limits
	}
	if len(lanes) == 0 {
		lanes = []Snapshot{s}
	}
	// Gross exposure includes borrowing; allocation targets use our own capital.
	providerRoom := s.TotalVaultNAVRaw
	equityRoom := max(s.TotalVaultNAVRaw-limits.CashReserveRaw-s.WithdrawalDemandRaw, 0)
	if group == "onre_auto" {
		equityRoom = equityRoom * 9 / 10
	} else {
		equityRoom /= 20
	}
	seen, custody := map[string]bool{}, map[string]bool{}
	for _, lane := range lanes {
		g := providerGroup(lane.RouteLane)
		if seen[lane.RouteLane] || g == "" ||
			lane.PositionCollateralValueRaw < 0 || lane.PositionCollateralValueRaw > 1<<53 ||
			lane.PositionDebtValueRaw < 0 || lane.PositionDebtValueRaw > 1<<53 || lane.CollateralIdleValueRaw < 0 || lane.CollateralIdleValueRaw > 1<<53 {
			return limits
		}
		seen[lane.RouteLane] = true
		if g == provider {
			providerRoom -= min(providerRoom, lane.PositionCollateralValueRaw)
		}
		if allocationGroup(lane.RouteLane) == group {
			equityRoom -= min(equityRoom, max(laneEquityRaw(lane), 0))
		}
		l, _ := findEarnLane(lane.RouteLane)
		key := l.route.Kamino.CollateralMint
		if !custody[key] {
			if g == provider {
				providerRoom -= min(providerRoom, lane.CollateralIdleValueRaw)
			}
			if allocationGroup(lane.RouteLane) == group {
				equityRoom -= min(equityRoom, lane.CollateralIdleValueRaw)
			}
			custody[key] = true
		}
	}
	for _, lane := range s.heldLanes() {
		if !seen[lane] {
			return limits
		}
	}
	limits.ProviderRoomRaw, limits.EquityRoomRaw = providerRoom, equityRoom
	return limits
}

func applyAllocationLimits(lanes []Snapshot, p SelectorPolicy) {
	if !p.DiversifyNewCash {
		return
	}
	for i, s := range lanes {
		limits := newAllocationLimits(s, lanes, p, s.RouteLane)
		lanes[i].NewAllocation = &limits
	}
}

func newAllocationQuoteAllowed(s Snapshot, lanes []Snapshot, p SelectorPolicy, q MoveQuote) bool {
	l := newAllocationLimits(s, lanes, p, q.DestinationLane)
	return l.accepts(q) && q.EquityRaw <= max(s.VoltrIdleRaw-s.WithdrawalDemandRaw+s.SquadsIdleRaw-p.idleBufferRaw(s.TotalVaultNAVRaw), 0)
}

func (l allocationLimits) accepts(q MoveQuote) bool {
	if q.RotateFrom != "" || q.EquityRaw <= 0 || q.EquityRaw > l.EquityRoomRaw || q.EquityRaw > 1<<53 || q.ProceedsValueRaw > 1<<53 ||
		q.Cost.EntrySwapCostMicros < -(1<<53) || q.Cost.EntrySwapCostMicros > 1<<53 || l.UnleveredEntry && !q.Unlevered {
		return false
	}
	// Count purchased exposure and borrowed exposure, without netting debt off.
	gross := max(q.EquityRaw, q.EquityRaw-int64(q.Cost.EntrySwapCostMicros)) + int64(q.ProceedsValueRaw)
	return gross > 0 && gross <= l.ProviderRoomRaw
}

func allocationBorrowAllowed(s Snapshot, raw uint64) bool {
	l := s.NewAllocation
	if l == nil {
		return true
	}
	if l.UnleveredEntry || s.VoltrIdleRaw-s.WithdrawalDemandRaw < l.CashReserveRaw {
		return false
	}
	value, err := capacityBorrowExpectedUSDC(s, raw, true)
	return err == nil && l.ProviderRoomRaw >= 0 && value <= uint64(l.ProviderRoomRaw)
}

func allocationCashDeficit(s Snapshot) int64 {
	if s.NewAllocation == nil {
		return 0
	}
	return max(s.NewAllocation.CashReserveRaw+s.WithdrawalDemandRaw-s.VoltrIdleRaw, 0)
}
