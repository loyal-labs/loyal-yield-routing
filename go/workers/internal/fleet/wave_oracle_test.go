package fleet

// The oracle shares only single-edge economic/eligibility primitives with the
// planner. It reconstructs each frontier from the complete selected prefix and
// exhaustively enumerates unused sources and their permitted targets. It never
// calls the production candidate, ranking, conflict, or rescore helpers.
//
// The global objective below is the sum of the admitted marginal net holding
// gains, including their integer rounding. It is order-sensitive; it is not a
// prediction of final portfolio NAV or a claim about global production optimality.

import (
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type oracleMove struct {
	source FleetVault
	d      Decision
}

type oracleWave struct {
	snapshot MarketSnapshot
	vaults   []FleetVault
	limits   WaveLimits
	baseIn   map[string]int64
	baseOut  map[string]int64
}

func newOracleWave(snapshot MarketSnapshot, vaults []FleetVault, limits WaveLimits) oracleWave {
	w := oracleWave{snapshot: snapshot, vaults: vaults, limits: limits, baseIn: map[string]int64{}, baseOut: map[string]int64{}}
	for _, v := range vaults {
		for reserve, value := range v.CommittedInflows {
			if old, ok := w.baseIn[reserve]; ok && old != value {
				panic("incoherent oracle fixture inflow")
			}
			w.baseIn[reserve] = value
		}
		for reserve, value := range v.CommittedOutflows {
			if old, ok := w.baseOut[reserve]; ok && old != value {
				panic("incoherent oracle fixture outflow")
			}
			w.baseOut[reserve] = value
		}
	}
	return w
}

func oracleWideSum(values ...int64) int64 {
	n := new(big.Int)
	for _, value := range values {
		n.Add(n, big.NewInt(value))
	}
	if !n.IsInt64() {
		panic("oracle financial frontier exceeds SQL BIGINT")
	}
	return n.Int64()
}

func (w oracleWave) frontier(prefix []oracleMove, reserve string, inflow bool) int64 {
	parts := []int64{w.baseOut[reserve]}
	if inflow {
		parts[0] = w.baseIn[reserve]
	}
	for _, move := range prefix {
		if inflow && move.d.TargetReserve == reserve || !inflow && move.d.SourceReserve == reserve {
			parts = append(parts, move.d.PrincipalUSDMicros)
		}
	}
	return oracleWideSum(parts...)
}

func oracleConflictSet(move oracleMove) map[string]bool {
	p := move.source.Position
	keys := map[string]bool{"vault:" + p.VaultPubkey: true, fmt.Sprintf("policy:%d", p.PolicyID): true, "source-reserve:" + move.d.SourceReserve: true, "target-reserve:" + move.d.TargetReserve: true}
	if policy := move.d.PolicyBindings; policy != nil && move.d.RouteKind == "cross_mint_jupiter" {
		keys["swap-policy:"+policy.Swap.PolicyAccount] = true
		keys["earn-policy:"+policy.Withdraw.PolicyAccount] = true
		keys["earn-policy:"+policy.Deposit.PolicyAccount] = true
	}
	return keys
}

// Compare the published ranking contract as a lexicographic key; negative wide
// integers express descending dimensions without overflow or production Less.
func oracleRank(move oracleMove) ([]*big.Int, []string) {
	d, p := move.d, move.source.Position
	var numbers []*big.Int
	for _, value := range []int64{d.EconomicPriority, d.AnnualYieldGainUSDMicros, d.ExpectedNetGainUSDMicros, d.PrincipalUSDMicros, p.ObservedSlot} {
		numbers = append(numbers, new(big.Int).Neg(big.NewInt(value)))
	}
	numbers = append(numbers, big.NewInt(d.VaultID))
	return numbers, []string{p.Mint, d.SourceReserve, d.TargetReserve}
}

func oracleRanksBefore(a, b oracleMove) bool {
	na, sa := oracleRank(a)
	nb, sb := oracleRank(b)
	for i := range na {
		if comparison := na[i].Cmp(nb[i]); comparison != 0 {
			return comparison < 0
		}
	}
	for i := range sa {
		if sa[i] != sb[i] {
			return sa[i] < sb[i]
		}
	}
	return false
}

func (w oracleWave) choices(prefix []oracleMove) []oracleMove {
	if len(prefix) >= w.limits.MaxOpportunities {
		return nil
	}
	var choices []oracleMove
	for _, source := range w.vaults {
		p := source.Position
		if p.BlockedReason != "" {
			continue
		}
		used, tenantCount := false, 0
		notional := new(big.Int)
		for _, move := range prefix {
			used = used || move.d.VaultID == p.VaultID
			if move.source.Position.PolicyAuthority == p.PolicyAuthority {
				tenantCount++
			}
			notional.Add(notional, big.NewInt(move.d.PrincipalUSDMicros))
		}
		if used || tenantCount >= w.limits.MaxPerTenant {
			continue
		}
		targets := map[string]bool{}
		for _, target := range source.AllowedTargets {
			targets[target] = true
		}
		for target := range source.CrossMintTargets {
			targets[target] = true
		}
		for target := range targets {
			if _, exists := w.snapshot.Reserves[target]; !exists || target == p.SourceReserve {
				continue
			}
			p.SourceCommittedInflowUSDMicros = w.frontier(prefix, p.SourceReserve, true)
			p.SourceCommittedOutflowUSDMicros = w.frontier(prefix, p.SourceReserve, false)
			p.TargetCommittedInflowUSDMicros = w.frontier(prefix, target, true)
			p.TargetCommittedOutflowUSDMicros = w.frontier(prefix, target, false)
			d := Plan(w.snapshot, p, p.SourceReserve, target)
			if !d.Eligible {
				continue
			}
			if policy, exists := source.CrossMintTargets[target]; exists {
				d.PolicyBindings = &policy
				d.CrossMintMaxValueLossBPS = source.CrossMintMaxValueLossBPS
			}
			if new(big.Int).Add(notional, big.NewInt(d.PrincipalUSDMicros)).Cmp(big.NewInt(w.limits.MaxNotionalUSDMicros)) > 0 {
				continue
			}
			candidate := oracleMove{source: source, d: d}
			conflict := false
			for key := range oracleConflictSet(candidate) {
				count := 0
				for _, selected := range prefix {
					if oracleConflictSet(selected)[key] {
						count++
					}
				}
				conflict = conflict || count >= w.limits.MaxPerWritableConflictKey
			}
			if !conflict {
				choices = append(choices, candidate)
			}
		}
	}
	sort.Slice(choices, func(i, j int) bool { return oracleRanksBefore(choices[i], choices[j]) })
	return choices
}

func (w oracleWave) greedy() []oracleMove {
	var prefix []oracleMove
	for {
		choices := w.choices(prefix)
		if len(choices) == 0 {
			return prefix
		}
		prefix = append(prefix, choices[0])
	}
}

func oraclePublished(prefix []oracleMove) []Decision {
	out := []Decision{}
	for _, move := range prefix {
		if move.d.RouteKind != "cross_mint_jupiter" || move.d.EstimatedCostLamports >= 15_000 {
			out = append(out, move.d)
		}
	}
	return out
}

func oracleGain(prefix []oracleMove) *big.Int {
	n := new(big.Int)
	for _, d := range oraclePublished(prefix) {
		n.Add(n, big.NewInt(d.ExpectedNetGainUSDMicros))
	}
	return n
}

// Explore every feasible sequence and every stopping prefix. Only small test
// fixtures call this; each transition consumes a previously unused vault ID.
func (w oracleWave) globalBest() ([]oracleMove, *big.Int, int) {
	bestGain := new(big.Int)
	var best []oracleMove
	nodes := 0
	var visit func([]oracleMove)
	visit = func(prefix []oracleMove) {
		nodes++
		if gain := oracleGain(prefix); gain.Cmp(bestGain) > 0 {
			bestGain.Set(gain)
			best = append([]oracleMove(nil), prefix...)
		}
		for _, choice := range w.choices(prefix) {
			visit(append(append([]oracleMove(nil), prefix...), choice))
		}
	}
	visit(nil)
	return best, bestGain, nodes
}

func oracleSnapshot() MarketSnapshot {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return MarketSnapshot{Slot: 500, ObservedAt: now, ExpiresAt: now.Add(10 * time.Minute), Hash: "oracle-bank", MintExpiresAt: map[string]time.Time{USDCMint: now.Add(10 * time.Minute), USDTMint: now.Add(10 * time.Minute)}, Reserves: map[string]ReserveState{}}
}

func oracleReserve(snapshot *MarketSnapshot, address, mint string, apy, supply int64) {
	snapshot.Reserves[address] = ReserveState{ReserveIdentity: ReserveIdentity{Address: address, Market: "oracle-market", Mint: mint}, Slot: snapshot.Slot, LastUpdateSlot: snapshot.Slot, SupplyAPYBPS: apy, TotalSupplyUSDMicros: supply, EconomicLifetimeMillis: 600_000, DataHash: "hash:" + address}
}

func oracleVault(snapshot MarketSnapshot, id int64, source string, amount int64, targets ...string) FleetVault {
	r := snapshot.Reserves[source]
	return FleetVault{Position: VaultPosition{VaultID: id, PolicyID: id, Settings: fmt.Sprintf("settings:%d", id), VaultPubkey: fmt.Sprintf("vault:%d", id), PolicyAuthority: fmt.Sprintf("tenant:%d", id%2), SourceReserve: source, Market: r.Market, Mint: r.Mint, AmountRaw: amount, SourceCollateralAmountRaw: amount, SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited, SnapshotID: 7, ObservedSlot: 499, ObservedAt: snapshot.ObservedAt}, AllowedTargets: targets}
}

func checkWaveOracle(t *testing.T, w oracleWave) []oracleMove {
	t.Helper()
	want := w.greedy()
	got, err := PlanFleetWithLimits(w.snapshot, w.vaults, w.limits)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]Decision, 0, len(got.Opportunities))
	for _, opportunity := range got.Opportunities {
		actual = append(actual, opportunity.Decision)
	}
	if !reflect.DeepEqual(actual, oraclePublished(want)) {
		t.Fatalf("full-rescore sequence differs: got=%s want=%s", oracleSequenceDecisions(actual), oracleSequenceDecisions(oraclePublished(want)))
	}
	return want
}

func oracleSequenceDecisions(decisions []Decision) string {
	var parts []string
	for _, d := range decisions {
		parts = append(parts, fmt.Sprintf("v%d:%s>%s[p=%d,edge=%d,net=%d]", d.VaultID, d.SourceReserve, d.TargetReserve, d.PrincipalUSDMicros, d.EdgeBPS, d.ExpectedNetGainUSDMicros))
	}
	return strings.Join(parts, ", ")
}

func TestWaveOracleGreedyEnumeratesFreshBestStep(t *testing.T) {
	rng := rand.New(rand.NewSource(20261003))
	for fixture := 0; fixture < 96; fixture++ {
		snapshot := oracleSnapshot()
		for target := 0; target < 3; target++ {
			oracleReserve(&snapshot, fmt.Sprintf("r%d", target), USDCMint, int64(600+rng.Intn(2000)), int64(2+rng.Intn(5))*100_000_000_000)
		}
		oracleReserve(&snapshot, "source", USDCMint, int64(100+rng.Intn(500)), 1_000_000_000_000)
		vaults := []FleetVault{}
		vaultCount := 2 + rng.Intn(4)
		for vault := 0; vault < vaultCount; vault++ {
			source := "source"
			if vault%2 == 1 {
				source = fmt.Sprintf("r%d", rng.Intn(3))
			}
			var targets []string
			for target := 0; target < 3; target++ {
				if rng.Intn(3) != 0 {
					targets = append(targets, fmt.Sprintf("r%d", target))
				}
			}
			v := oracleVault(snapshot, int64(vault+1), source, int64(1+rng.Intn(8))*1_000_000_000, targets...)
			// All sources see the same outstanding reservation frontier. It is
			// a reserve-wide commitment, never multiplied by the vault count.
			v.CommittedInflows = map[string]int64{"r0": 1_000_000_000}
			v.CommittedOutflows = map[string]int64{"r1": 500_000_000}
			vaults = append(vaults, v)
		}
		limits := WaveLimits{1 + rng.Intn(5), int64(3+rng.Intn(20)) * 1_000_000_000, 1 + rng.Intn(3), 1 + rng.Intn(3)}
		w := newOracleWave(snapshot, vaults, limits)
		first := checkWaveOracle(t, w)
		rng.Shuffle(len(vaults), func(i, j int) { vaults[i], vaults[j] = vaults[j], vaults[i] })
		second := checkWaveOracle(t, newOracleWave(snapshot, vaults, limits))
		if !reflect.DeepEqual(oraclePublished(first), oraclePublished(second)) {
			t.Fatalf("fixture %d selection depends on loader order", fixture)
		}
	}
}

func TestWaveOracleSourceDilutionChangesTheNextWinner(t *testing.T) {
	snapshot := oracleSnapshot()
	for address, apy := range map[string]int64{"a": 1000, "b": 2000, "c": 3000, "d": 1000, "e": 2010} {
		oracleReserve(&snapshot, address, USDCMint, apy, 1_000_000_000_000)
	}
	vaults := []FleetVault{oracleVault(snapshot, 1, "a", 20_000_000_000, "b"), oracleVault(snapshot, 2, "b", 10_000_000_000, "c"), oracleVault(snapshot, 3, "d", 10_000_000_000, "e")}
	w := newOracleWave(snapshot, vaults, WaveLimits{2, 40_000_000_000, 3, 3})
	initial := w.choices(nil)
	if len(initial) != 3 || initial[0].d.VaultID != 1 || initial[1].d.VaultID != 3 || initial[2].d.VaultID != 2 {
		t.Fatal("source-dilution fixture did not begin with a different next winner")
	}
	chosen := checkWaveOracle(t, w)
	if len(chosen) != 2 || chosen[1].d.VaultID != 2 {
		t.Fatal("selected target inflow did not reprice the remaining source")
	}
	// Keep a permitted edge even if it begins below the economic gate: the
	// first selected inflow can make it feasible without changing user intent.
	c := snapshot.Reserves["c"]
	c.SupplyAPYBPS = 2000
	snapshot.Reserves["c"] = c
	w = newOracleWave(snapshot, vaults[:2], WaveLimits{2, 40_000_000_000, 3, 3})
	if len(w.choices(nil)) != 1 {
		t.Fatal("reactivated permitted edge did not start ineligible")
	}
	if chosen := checkWaveOracle(t, w); len(chosen) != 2 || chosen[1].d.VaultID != 2 {
		t.Fatal("initially ineligible edge was lost before its source repriced")
	}
}

func oracleCrossPolicy(v FleetVault) CrossMintPolicyBindings {
	return CrossMintPolicyBindings{Settings: v.Position.Settings, VaultPubkey: v.Position.VaultPubkey, DelegatedSigner: "oracle-signer",
		Withdraw: CrossMintEarnPolicyBinding{PolicyAccount: "withdraw", SourceCommitment: "finalized"},
		Swap:     CrossMintSwapPolicyBinding{PolicyAccount: "swap", SourceShard: "classic", SourceCommitment: "finalized", MaxSlippageBPS: 25, DailySourceMintSpendingCap: 100_000_000_000, ManifestFingerprint: strings.Repeat("a", 64)},
		Deposit:  CrossMintEarnPolicyBinding{PolicyAccount: "deposit", SourceCommitment: "finalized", ConstraintIndex: 1}}
}

func TestWaveOracleMixedMintsOwnedCapacityAndHiddenFeeAdmission(t *testing.T) {
	snapshot := oracleSnapshot()
	oracleReserve(&snapshot, "source-usdc", USDCMint, 100, 1_000_000_000_000)
	oracleReserve(&snapshot, "source-usdt", USDTMint, 200, 1_000_000_000_000)
	oracleReserve(&snapshot, "target-usdc", USDCMint, 1800, 500_000_000_000)
	oracleReserve(&snapshot, "target-usdt", USDTMint, 2200, 500_000_000_000)
	vaults := []FleetVault{oracleVault(snapshot, 1, "source-usdc", 4_000_000_000, "target-usdc"), oracleVault(snapshot, 2, "source-usdt", 5_000_000_000, "target-usdt"), oracleVault(snapshot, 3, "source-usdc", 8_000_000_000, "target-usdt")}
	vaults[0].CrossMintTargets = map[string]CrossMintPolicyBindings{"target-usdt": oracleCrossPolicy(vaults[0])}
	vaults[0].CrossMintMaxValueLossBPS = 50
	vaults[2].Position.BlockedReason = "active_opportunity"
	for i := range vaults {
		vaults[i].CommittedInflows = map[string]int64{"target-usdt": 5_000_000_000}
		vaults[i].CommittedOutflows = map[string]int64{"source-usdt": 2_000_000_000}
	}
	w := newOracleWave(snapshot, vaults, WaveLimits{3, 30_000_000_000, 3, 3})
	chosen := checkWaveOracle(t, w)
	if len(chosen) != 2 {
		t.Fatalf("incumbent or mint frontier changed allocation: %s", oracleSequenceDecisions(oraclePublished(chosen)))
	}
	for _, move := range chosen {
		if move.d.VaultID == 3 {
			t.Fatal("incumbent custody was admitted again")
		}
	}
	// The smallest economically eligible cross-mint attempt cannot fund all
	// three legs. Its retained admission consumes this wave's only selection
	// even though there is no publishable opportunity.
	feeVault := oracleVault(snapshot, 4, "source-usdc", 20_000_000, "target-usdt")
	feeVault.CrossMintTargets = map[string]CrossMintPolicyBindings{"target-usdt": oracleCrossPolicy(feeVault)}
	feeVault.CrossMintMaxValueLossBPS = 50
	for amount := int64(20_000_000); amount < 100_000_000; amount += 1_000_000 {
		feeVault.Position.AmountRaw, feeVault.Position.SourceCollateralAmountRaw = amount, amount
		d := Plan(snapshot, feeVault.Position, "source-usdc", "target-usdt")
		if d.Eligible && d.EstimatedCostLamports < 15_000 {
			feeWave := newOracleWave(snapshot, []FleetVault{feeVault}, WaveLimits{1, 100_000_000, 1, 1})
			selected := checkWaveOracle(t, feeWave)
			if len(selected) != 1 || len(oraclePublished(selected)) != 0 {
				t.Fatal("cross-mint publication fee fence did not preserve selected ownership")
			}
			return
		}
	}
	t.Fatal("no bounded three-leg fee fence fixture was found")
}

func TestWaveOracleTiesLimitsAndClosedReservePath(t *testing.T) {
	snapshot := oracleSnapshot()
	oracleReserve(&snapshot, "a", USDCMint, 200, 1_000_000_000_000)
	oracleReserve(&snapshot, "b", USDCMint, 1500, 1_000_000_000_000)
	oracleReserve(&snapshot, "c", USDCMint, 2500, 1_000_000_000_000)
	vaults := []FleetVault{oracleVault(snapshot, 3, "a", 3_000_000_000, "b", "c"), oracleVault(snapshot, 2, "a", 3_000_000_000, "b", "c"), oracleVault(snapshot, 1, "a", 3_000_000_000, "b", "c")}
	for _, maxMoves := range []int{1, 2, 5} {
		limits := WaveLimits{maxMoves, 30_000_000_000, 5, 5}
		chosen := checkWaveOracle(t, newOracleWave(snapshot, vaults, limits))
		if len(chosen) != min(maxMoves, 3) || chosen[0].d.VaultID != 1 {
			t.Fatal("tie-break or maximum selected moves changed")
		}
	}
	// The permitted graph contains a directed cycle, but its closing edge is
	// economically forbidden. This verifies this input, not a general DAG claim:
	// production additionally prevents any vault from moving twice in a wave.
	cycle := []FleetVault{oracleVault(snapshot, 1, "a", 3_000_000_000, "b"), oracleVault(snapshot, 2, "b", 3_000_000_000, "c"), oracleVault(snapshot, 3, "c", 3_000_000_000, "a")}
	chosen := checkWaveOracle(t, newOracleWave(snapshot, cycle, WaveLimits{5, 30_000_000_000, 5, 5}))
	if len(chosen) != 2 {
		t.Fatal("profitable open path was not admitted")
	}
	for _, move := range chosen {
		if move.d.SourceReserve == "c" && move.d.TargetReserve == "a" {
			t.Fatal("uneconomic cycle-closing edge admitted")
		}
	}
}

func TestWaveOracleSmallExhaustiveObjectiveBoundsGreedy(t *testing.T) {
	rng := rand.New(rand.NewSource(46017))
	var maximumRegretPPM int64
	totalNodes := 0
	for fixture := 0; fixture < 12; fixture++ {
		snapshot := oracleSnapshot()
		for target := 0; target < 3; target++ {
			oracleReserve(&snapshot, fmt.Sprintf("target%d", target), USDCMint, int64(800+rng.Intn(1600)), int64(2+rng.Intn(5))*100_000_000_000)
		}
		vaults := []FleetVault{}
		for vault := 0; vault < 4; vault++ {
			source := fmt.Sprintf("source%d", vault)
			oracleReserve(&snapshot, source, USDCMint, int64(100+rng.Intn(400)), 1_000_000_000_000)
			var targets []string
			for target := 0; target < 3; target++ {
				if rng.Intn(3) != 0 {
					targets = append(targets, fmt.Sprintf("target%d", target))
				}
			}
			vaults = append(vaults, oracleVault(snapshot, int64(vault+1), source, int64(2+rng.Intn(7))*1_000_000_000, targets...))
		}
		w := newOracleWave(snapshot, vaults, WaveLimits{3, 20_000_000_000, 3, 3})
		greedy := checkWaveOracle(t, w)
		_, optimum, nodes := w.globalBest()
		totalNodes += nodes
		regret := new(big.Int).Sub(optimum, oracleGain(greedy))
		if regret.Sign() < 0 {
			t.Fatal("exhaustive search omitted the greedy feasible sequence")
		}
		if optimum.Sign() > 0 {
			ppm := new(big.Int).Quo(new(big.Int).Mul(regret, big.NewInt(1_000_000)), optimum).Int64()
			maximumRegretPPM = max(maximumRegretPPM, ppm)
		}
	}
	if totalNodes > 10_000 {
		t.Fatalf("small oracle unexpectedly expanded %d states", totalNodes)
	}
	t.Logf("12 exhaustive small fixtures: states=%d maximum marginal-gain regret=%d ppm", totalNodes, maximumRegretPPM)
}

func TestWaveOracleQuantifiesGreedyAssignmentRegret(t *testing.T) {
	snapshot := oracleSnapshot()
	oracleReserve(&snapshot, "source-a", USDCMint, 100, 1_000_000_000_000)
	oracleReserve(&snapshot, "source-b", USDCMint, 200, 1_000_000_000_000)
	oracleReserve(&snapshot, "target-a", USDCMint, 2000, 500_000_000_000)
	oracleReserve(&snapshot, "target-b", USDCMint, 1900, 500_000_000_000)
	vaults := []FleetVault{oracleVault(snapshot, 1, "source-a", 10_000_000_000, "target-a", "target-b"), oracleVault(snapshot, 2, "source-b", 10_000_000_000, "target-a")}
	w := newOracleWave(snapshot, vaults, WaveLimits{2, 20_000_000_000, 2, 2})
	greedy := checkWaveOracle(t, w)
	best, bestGain, nodes := w.globalBest()
	greedyGain := oracleGain(greedy)
	regret := new(big.Int).Sub(bestGain, greedyGain)
	if len(greedy) != 1 || greedy[0].d.VaultID != 1 || greedy[0].d.TargetReserve != "target-a" || len(best) != 2 || regret.Sign() <= 0 {
		t.Fatal("capacity-constrained assignment did not demonstrate greedy regret")
	}
	// This fixture is a stable intentional distinction: local greedy ordering
	// is correct, while assigning the flexible vault elsewhere funds both moves.
	if new(big.Int).Mul(regret, big.NewInt(100)).Cmp(new(big.Int).Mul(bestGain, big.NewInt(40))) < 0 {
		t.Fatal("fixture no longer exposes material (>40%) marginal-gain regret")
	}
	t.Logf("greedy=%s; exhaustive=%s; marginal net micros greedy=%s best=%s regret=%s; visited=%d", oracleSequenceDecisions(oraclePublished(greedy)), oracleSequenceDecisions(oraclePublished(best)), greedyGain, bestGain, regret, nodes)
}
