package backyard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// stepLookupTablesPath holds the mainnet contents of the lookup tables a step
// can address: the route valuation table and every table the Jupiter fixture
// (jupiterV2FixturePath) names.
const stepLookupTablesPath = "testdata/step-lookup-tables.json"

func stepTestTables(t testing.TB) map[string]LookupTableSnapshot {
	t.Helper()
	var fixture struct{ Tables []LookupTableSnapshot }
	raw, err := os.ReadFile(stepLookupTablesPath)
	if err != nil || json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("step lookup table fixture unavailable", err)
	}
	out := map[string]LookupTableSnapshot{}
	for _, table := range fixture.Tables {
		if _, err := decodeMessageLookupTable(table); err != nil {
			t.Fatalf("%s: %v", table.Address, err)
		}
		out[table.Address] = table
	}
	return out
}

// stepTestPolicies is the captured policy set, with every literal the capture
// predates (the wYLDS lane's, Hastra's) at a stand-in account, and each
// account's literal.
func stepTestPolicies(t testing.TB) (installedPolicies, map[solana.PublicKey]policyKey, map[policyKey]squads.Policy) {
	t.Helper()
	policies := testPolicies(t)
	literals, err := backyardPolicies()
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[solana.PublicKey]policyKey{}
	for key := range literals {
		if policies[key].matches != 1 {
			policies[key] = installedPolicy{Installed: squads.Installed{Account: solana.PublicKey(sha256.Sum256([]byte(key.String())))}, matches: 1}
		}
		byAccount[policies[key].Account] = key
	}
	return policies, byAccount, literals
}

// A step variant: the Jupiter route as the fixture captured it (0), or a
// route of maxAccounts venue accounts behind the fixture's twelve fixed
// shared_accounts_route_v2 accounts, held by a Jupiter table of their own.
var stepRouteVariants = []int{0, 16, 32}

// stepTestConversion is the step's conversion on lane and the tables it
// brings: Hastra's at about the mainnet price, or the fixture's Jupiter swap.
func stepTestConversion(t *testing.T, policies installedPolicies, lane string, up bool, maxAccounts int) (any, []LookupTableSnapshot) {
	t.Helper()
	tables := stepTestTables(t)
	laneTables := []LookupTableSnapshot{tables[routeValuationLookupTable]}
	action := SwapCollateralToDebtStep
	if up {
		action = SwapDebtToCollateralStep
	}
	if leg, ok := hastraConversionLeg(action, lane); ok {
		account, err := policies.account(policyKey{hastra: hastraName()})
		if err != nil {
			t.Fatal(err)
		}
		price := hastra.StakePrice{Price: hastraTestPrice, Scale: 1_000_000_000_000_000_000, Timestamp: kaminoFixtureUnix}
		r := HastraConversionRequest{Action: action, AmountRaw: 1_000_000, Price: price.Price.String(), PriceScale: price.Scale, PriceTimestamp: price.Timestamp,
			Policy: account, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99, RouteLane: lane}
		if r.OutputRaw, err = hastraOutput(leg, price, r.AmountRaw); err != nil {
			t.Fatal(err)
		}
		return r, laneTables
	}
	r := fixtureSwapRequest(t, lane, action)
	for _, address := range r.Instruction.LookupTableAddresses {
		table, ok := tables[address]
		if !ok {
			t.Fatalf("no fixture contents for Jupiter table %s", address)
		}
		laneTables = append(laneTables, table)
	}
	if maxAccounts > 0 {
		head := r.Instruction.Accounts[:12]
		r.Instruction.Accounts = append([]jupiter.AccountMeta{}, head...)
		var venue []string
		for i := range maxAccounts {
			key := sha256.Sum256(fmt.Appendf(nil, "venue %s %d", lane, i))
			address := encodeBase58(key[:])
			venue = append(venue, address)
			r.Instruction.Accounts = append(r.Instruction.Accounts, jupiter.AccountMeta{Pubkey: address, IsWritable: i%2 == 0})
		}
		synthetic := autoFixtureLookupTable(t, venue)
		synthetic.Address = encodeBase58(fmt.Appendf(nil, "%032d", maxAccounts))
		laneTables = append(laneTables, synthetic)
	}
	return r, laneTables
}

// stepStaticTable is a stand-in lookup table holding every account step
// carries as a full key that a table may hold: all but the fee payer and the
// top-level programs. It measures a step addressed through one Backyard-owned
// table of its lane's accounts, which does not exist on chain.
func stepStaticTable(t *testing.T, step stepMessage, tables []LookupTableSnapshot) LookupTableSnapshot {
	t.Helper()
	invoked := map[solana.PublicKey]bool{}
	for _, ix := range decodeTestInstructions(t, step.Message, tables) {
		invoked[ix.ProgramID] = true
	}
	offset := 4
	count, err := decodeShortVec(step.Message, &offset)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 1; i < count; i++ { // the fee payer signs, so it stays static
		key := solana.PublicKeyFromBytes(step.Message[offset+32*i : offset+32*i+32])
		if !invoked[key] {
			keys = append(keys, key.String())
		}
	}
	table := autoFixtureLookupTable(t, keys)
	table.Address = encodeBase58([]byte("backyard step table stand-in 32b"))
	return table
}

// stepTestBuild compiles lane's up or down step through the captured
// policies: 1,000,000 raw borrowed and converted (up) or withdrawn and
// converted (down), the close leg the conversion's guaranteed output.
// extra tables join the captured ones.
func stepTestBuild(t *testing.T, lane string, up bool, maxAccounts int, extra ...LookupTableSnapshot) (stepMessage, []LookupTableSnapshot) {
	t.Helper()
	manifest := embeddedTestManifest(t)
	policies, _, _ := stepTestPolicies(t)
	route, err := runtimeRoute(lane)
	if err != nil {
		t.Fatal(err)
	}
	conversion, tables := stepTestConversion(t, policies, lane, up, maxAccounts)
	tables = append(tables, extra...)
	closeRaw, err := stepCloseAmount(0, conversion)
	if err != nil {
		t.Fatal(err)
	}
	openLeg, closeLeg, action := kaminoLegWithdraw, kaminoLegRepay, DeleverRouteStep
	if up {
		openLeg, closeLeg, action = kaminoLegBorrow, kaminoLegDeposit, OpenRouteStep
	}
	blockhash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	open, err := manifest.kaminoPacketForRoute(policies, action, openLeg, 1_000_000, blockhash, lane)
	if err != nil {
		t.Fatal(err)
	}
	open.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	closing, err := manifest.kaminoPacketForRoute(policies, action, closeLeg, closeRaw, blockhash, lane)
	if err != nil {
		t.Fatal(err)
	}
	step, err := buildStepMessage(atomicStep{Open: open, Conversion: conversion, Close: closing, Tables: tables}, mustKey(bridgeDelegate))
	if err != nil {
		t.Fatal(err)
	}
	return step, tables
}

// An atomic step is one message: each KLend execute directly behind its own
// refresh prefix, the conversion between them, every Squads execute at the
// one constraint of its installed literal that admits it. Its size against
// the packet cap is reported.
func TestAtomicStepIsOneMessageOfPolicyExecutes(t *testing.T) {
	t.Parallel()
	_, byAccount, literals := stepTestPolicies(t)
	for _, lane := range []string{autoAUTOWYLDSLane, autoAUTOPYUSD.Lane} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		for _, up := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/up=%t", lane, up), func(t *testing.T) {
				step, tables := stepTestBuild(t, lane, up, 0)
				if step.Fits() {
					t.Logf("%d bytes, %d under %d; %d accounts (%d static), tables %v", step.WireBytes, solanaPacketBytes-step.WireBytes, solanaPacketBytes, step.Accounts, step.StaticAccounts, step.Tables)
				} else {
					t.Logf("%d bytes, %d OVER %d; %d accounts (%d static), tables %v", step.WireBytes, step.WireBytes-solanaPacketBytes, solanaPacketBytes, step.Accounts, step.StaticAccounts, step.Tables)
				}
				instructions := decodeTestInstructions(t, step.Message, tables)
				heap := autoComputeBudgetHeapInstruction()
				if len(instructions) == 0 || instructions[0].ProgramID != solana.PublicKey(heap.program) || string(instructions[0].Data) != string(heap.data) {
					t.Fatal("AUTO step does not open with the heap frame")
				}
				instructions = instructions[1:]
				if len(instructions) != 9 {
					t.Fatalf("step has %d instructions after the heap frame, want 9", len(instructions))
				}
				open, closeLeg := kamino.BorrowV2Discriminator, kamino.DepositV2Discriminator
				if !up {
					open, closeLeg = kamino.WithdrawV2Discriminator, kamino.RepayV2Discriminator
				}
				for _, prefix := range []int{0, 5} {
					for i, want := range [][8]byte{kamino.RefreshReserveDiscriminator, kamino.RefreshReserveDiscriminator, kamino.RefreshObligationDiscriminator} {
						ix := instructions[prefix+i]
						if ix.ProgramID != kamino.ProgramID || [8]byte(ix.Data[:8]) != want {
							t.Fatalf("instruction %d is not the refresh prefix of the KLend execute at %d", prefix+i, prefix+3)
						}
					}
				}
				// The close leg's obligation refresh runs over the topology the open
				// leg left: the collateral deposit, then the debt.
				if refresh := instructions[7]; len(refresh.Accounts) != 4 || refresh.Accounts[2].PublicKey.String() != route.Kamino.CollateralReserve ||
					refresh.Accounts[3].PublicKey.String() != route.Kamino.DebtReserve {
					t.Fatalf("close refresh runs over %v", refresh.Accounts)
				}
				for i, want := range map[int][]byte{3: open[:], 4: nil, 8: closeLeg[:]} {
					ix := instructions[i]
					if ix.ProgramID != squads.ProgramID {
						t.Fatalf("instruction %d is not a Squads execute", i)
					}
					execution, err := squads.DecodeExecuteTransactionSyncV2(ix)
					if err != nil {
						t.Fatal(err)
					}
					key, ok := byAccount[execution.Policy]
					if !ok || len(execution.Inner) != 1 || len(execution.ConstraintIndexes) != 1 {
						t.Fatalf("instruction %d executes %+v through %s, no installed literal", i, execution, execution.Policy)
					}
					inner := execution.Inner[0]
					if want != nil && (inner.ProgramID != kamino.ProgramID || string(inner.Data[:8]) != string(want)) {
						t.Fatalf("instruction %d executes the wrong KLend leg", i)
					}
					var admitting []int
					for index, constraint := range literals[key].Constraints {
						if squads.Admits(constraint, inner, vaultObligation) {
							admitting = append(admitting, index)
						}
					}
					if fmt.Sprint(admitting) != fmt.Sprint([]int{int(execution.ConstraintIndexes[0])}) {
						t.Errorf("%s: instruction %d executes at constraint %d, but constraints %v admit it", key, i, execution.ConstraintIndexes[0], admitting)
					}
				}
			})
		}
	}
}

// The close leg is exactly held custody plus what the conversion's own wire
// guarantees: a Jupiter swap's quoted out less its slippage, never the
// advisory JSON threshold; Hastra's output at its quoted price.
func TestStepCloseAmountIsHeldPlusTheGuaranteedOutput(t *testing.T) {
	t.Parallel()
	swap := fixtureSwapRequest(t, autoAUTOPYUSD.Lane, SwapDebtToCollateralStep)
	floor, err := jupiterInstructionWireFloor(swap.Instruction)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := stepCloseAmount(7, swap); err != nil || got != 7+floor {
		t.Fatalf("Jupiter close %d %v, want %d", got, err, 7+floor)
	}
	conversion := HastraConversionRequest{OutputRaw: 976_612}
	if got, err := stepCloseAmount(7, conversion); err != nil || got != 7+976_612 {
		t.Fatalf("Hastra close %d %v", got, err)
	}
	if _, err := stepCloseAmount(^uint64(0), conversion); err == nil {
		t.Fatal("an overflowing close amount builds")
	}
}

// Every earn lane's up and down step, compiled through the captured mainnet
// tables at the fixture's route and at maxAccounts 16 and 32. Run with -v for
// the size table; its last column is the same step addressed through one
// Backyard-owned table of its accounts as well (stepStaticTable).
func TestAtomicStepSizeEveryEarnLane(t *testing.T) {
	t.Parallel()
	var table strings.Builder
	fmt.Fprintf(&table, "%-20s %-4s %-8s %5s %6s %8s %6s %6s | %s\n", "lane", "dir", "route", "bytes", "slack", "accounts", "static", "tables", "with a Backyard table")
	for _, lane := range earnLaneIDs(true) {
		for _, up := range []bool{true, false} {
			for _, variant := range stepRouteVariants {
				route := fmt.Sprintf("max%d", variant)
				if variant == 0 {
					route = "fixture"
				}
				if _, hastra := hastraConversionLeg(SwapDebtToCollateralStep, lane); hastra {
					if variant != 0 {
						continue
					}
					route = "hastra"
				}
				t.Run(fmt.Sprintf("%s/up=%t/%s", lane, up, route), func(t *testing.T) {
					step, tables := stepTestBuild(t, lane, up, variant)
					owned, _ := stepTestBuild(t, lane, up, variant, stepStaticTable(t, step, tables))
					fmt.Fprintf(&table, "%-20s %-4s %-8s %5d %+6d %8d %6d %6d | %d (%+d)\n", lane, map[bool]string{true: "up", false: "down"}[up], route,
						step.WireBytes, solanaPacketBytes-step.WireBytes, step.Accounts, step.StaticAccounts, len(step.Tables), owned.WireBytes, solanaPacketBytes-owned.WireBytes)
				})
			}
		}
	}
	t.Cleanup(func() { t.Log("\n" + table.String()) })
}
