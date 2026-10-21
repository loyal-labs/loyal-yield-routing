package fleet

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"
)

// replayFixture is a recorded production planning input: one Rust-era
// optimizer epoch's USDC reserves and the 128 vaults Rust published moves
// for from it.
type replayFixture struct {
	SourceReserve string   `json:"sourceReserve"`
	TargetReserve string   `json:"targetReserve"`
	PolicyMarkets []string `json:"policyMarkets"`
	Epoch         struct {
		Reserves []struct {
			Reserve                string  `json:"reserve"`
			Market                 string  `json:"market"`
			LiquidityMint          string  `json:"liquidityMint"`
			Slot                   int64   `json:"slot"`
			ReserveLastUpdateSlot  int64   `json:"reserveLastUpdateSlot"`
			EconomicSlotLag        int64   `json:"economicSlotLag"`
			ReserveLastUpdateStale bool    `json:"reserveLastUpdateStale"`
			SupplyAPYBPS           int64   `json:"supplyApyBps"`
			TotalSupplyRaw         int64   `json:"totalSupplyUsdMicros"`
			AvailableAmountRaw     string  `json:"availableAmountRaw"`
			DepositLimitRaw        *string `json:"depositLimitRaw"`
			AccountDataHash        string  `json:"accountDataHash"`
		} `json:"reserves"`
	} `json:"epoch"`
	// Positions are [amount_raw, source observed slot, Rust planning cycle].
	Positions [][3]int64 `json:"positions"`
}

type replayGolden struct {
	Cycles []struct {
		Cycle     int64            `json:"cycle"`
		Decisions []replayDecision `json:"decisions"`
		// Rejections and Movable are what the durable-epoch path reported.
		Rejections map[string]int   `json:"rejectionCounts"`
		Movable    map[string]int64 `json:"movable"`
	} `json:"cycles"`
}

type replayDecision struct {
	VaultID                  int64  `json:"vaultId"`
	RouteKind                string `json:"routeKind"`
	SourceReserve            string `json:"sourceReserve"`
	TargetReserve            string `json:"targetReserve"`
	Mint                     string `json:"mint"`
	AmountRaw                int64  `json:"amountRaw"`
	PrincipalUSDMicros       int64  `json:"principalUsdMicros"`
	SourceAPYBPS             int64  `json:"sourceApyBps"`
	TargetAPYBPS             int64  `json:"targetApyBps"`
	EdgeBPS                  int64  `json:"edgeBps"`
	AnnualYieldGainUSDMicros int64  `json:"annualYieldGainUsdMicros"`
	ExpectedNetGainUSDMicros int64  `json:"expectedNetGainUsdMicros"`
	EconomicPriority         int64  `json:"economicPriority"`
	EstimatedCostLamports    int64  `json:"estimatedCostLamports"`
	TargetCapacityUSDMicros  int64  `json:"targetCapacityUsdMicros"`
}

func syntheticPubkey(kind string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fleet-replay-2026-10-07/%s/%d", kind, n)))
	return solana.PublicKeyFromBytes(sum[:]).String()
}

// On 2026-10-07 at 23:47:58Z the Rust planner published 128 moves from
// AYL4LM to D6q6wu (18.72% vs 33.34% supply APY) in two cycles of 64, the
// per-reserve writable-conflict limit. The durable-epoch Go path planned the
// same moves; its decisions are recorded in burst-2026-10-07.decisions.json.
// The same facts, as one snapshot holds them (each reserve's state as its
// account decodes at the recorded slot, each vault's collateral in its
// obligation, a policy delegating to the planner), must plan exactly those
// decisions through the registry and snapshot path.
func TestSnapshotPlannerReplaysTheDurableEpochDecisions(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/fleet/burst-2026-10-07.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture replayFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../testdata/fleet/burst-2026-10-07.decisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden replayGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	const slotsPerSecond, signer = 2.5, "planner-signer"
	snapshot := Snapshot{ObservedAt: time.Now().UTC(), Reserves: map[string]ReserveState{}, Vaults: map[string]VaultFacts{}}
	var catalog []ReserveIdentity
	for _, r := range fixture.Epoch.Reserves {
		identity := ReserveIdentity{Address: r.Reserve, Market: r.Market, Mint: r.LiquidityMint}
		catalog = append(catalog, identity)
		available, err := strconv.ParseInt(r.AvailableAmountRaw, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		state := ReserveState{ReserveIdentity: identity, Slot: r.Slot, ObservedAt: snapshot.ObservedAt, LastUpdateSlot: r.ReserveLastUpdateSlot,
			LastUpdateStale: r.ReserveLastUpdateStale, EconomicSlotLag: r.EconomicSlotLag, SupplyAPYBPS: r.SupplyAPYBPS,
			TotalSupplyRaw: r.TotalSupplyRaw, DataHash: r.AccountDataHash, AvailableLiquidityUSDMicros: &available}
		// DecodeKaminoReserve's lifetime: the slots left of the 1500-slot lag.
		state.EconomicLifetimeMillis = int64(float64(max(maximumEconomicSlotLag-r.EconomicSlotLag, 0)) / slotsPerSecond * 1000)
		if r.DepositLimitRaw != nil {
			limit, err := strconv.ParseUint(*r.DepositLimitRaw, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			state.DepositLimitRaw = &limit
		}
		snapshot.Reserves[r.Reserve] = state
		snapshot.Slot = max(snapshot.Slot, r.Slot)
	}
	source := snapshot.Reserves[fixture.SourceReserve]
	var registry []RegisteredVault
	for i, p := range fixture.Positions {
		n := i + 1
		v := RegisteredVault{VaultID: int64(n), Settings: syntheticPubkey("settings", n), VaultPubkey: syntheticPubkey("vault", n),
			PolicyID: int64(n), PolicyAuthority: syntheticPubkey("authority", n), PolicyAccount: syntheticPubkey("policy", n),
			Markets: fixture.PolicyMarkets, StableMints: []string{USDCMint}, KaminoMints: []string{USDCMint}}
		registry = append(registry, v)
		// The recorded amount is both the collateral and what it redeems.
		snapshot.Vaults[v.VaultPubkey] = VaultFacts{Policy: &DecodedSquadsPolicy{Settings: v.Settings, DelegatedSigners: []string{signer}},
			Holdings: []Holding{{Reserve: source.Address, Market: source.Market, Mint: source.Mint, CollateralRaw: uint64(p[0]), LiquidityRaw: uint64(p[0])}}}
	}
	if len(golden.Cycles) != 2 {
		t.Fatalf("golden has %d cycles", len(golden.Cycles))
	}
	options := FleetLoadOptions{DelegatedSigner: signer}
	record, err := recordOf(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range golden.Cycles {
		vaults, err := PlanningVaults(snapshot, catalog, registry, nil, nil, options)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanFleet(snapshot.Market(record.Key), vaults)
		if err != nil {
			t.Fatal(err)
		}
		var got []replayDecision
		chosen := map[int64]bool{}
		for _, o := range plan.Opportunities {
			d := o.Decision
			chosen[d.VaultID] = true
			got = append(got, replayDecision{d.VaultID, d.RouteKind, d.SourceReserve, d.TargetReserve, d.Mint, d.AmountRaw, d.PrincipalUSDMicros, d.SourceAPYBPS, d.TargetAPYBPS, d.EdgeBPS, d.AnnualYieldGainUSDMicros, d.ExpectedNetGainUSDMicros, d.EconomicPriority, d.EstimatedCostLamports, d.TargetCapacityUSDMicros})
		}
		sort.Slice(got, func(i, j int) bool { return got[i].VaultID < got[j].VaultID })
		if !reflect.DeepEqual(got, want.Decisions) {
			t.Fatalf("cycle %d: snapshot planner decided\n%+v\ndurable-epoch path decided\n%+v", want.Cycle, got, want.Decisions)
		}
		if counts := rejectionCounts(plan.Rejections); !reflect.DeepEqual(counts, want.Rejections) {
			t.Fatalf("cycle %d rejections %v, durable-epoch path %v", want.Cycle, counts, want.Rejections)
		}
		if !reflect.DeepEqual(plan.Movable, want.Movable) {
			t.Fatalf("cycle %d movable %v, durable-epoch path %v", want.Cycle, plan.Movable, want.Movable)
		}
		// The executor holds the chosen vaults' moves; the next cycle plans
		// the rest.
		remaining := registry[:0:0]
		for _, v := range registry {
			if !chosen[v.VaultID] {
				remaining = append(remaining, v)
			}
		}
		registry = remaining
	}
	if len(registry) != 0 {
		t.Fatalf("%d vaults were never moved; Rust moved all %d", len(registry), len(fixture.Positions))
	}
}
