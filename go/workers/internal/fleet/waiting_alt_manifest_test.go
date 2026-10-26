package fleet

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func waitingManifestFixture(t *testing.T, settings, vault, policy string) ALTManifest {
	return waitingManifestFixtureForRoute(t, settings, vault, policy, manifestKey(5), manifestKey(6))
}

func waitingManifestFixtureForRoute(t *testing.T, settings, vault, policy, source, target string) ALTManifest {
	t.Helper()
	payer := manifestKey(90)
	input := KaminoSameMintRouteRequest{Vault: vault, Source: KaminoPositionAccounts{Market: manifestKey(4), Reserve: source, LiquidityMint: USDCMint, Obligation: manifestKey(60), VaultLiquidityATA: manifestKey(61)}}
	input.Target = input.Source
	input.Target.Reserve = target
	input.Target.Obligation = manifestKey(62)
	accounts := []InstructionAccount{{settings, false, false}, {vault, false, true}, {policy, false, false}, {input.Source.Market, false, false}, {input.Source.Reserve, false, true}, {input.Target.Reserve, false, true}, {USDCMint, false, false}, {input.Source.Obligation, false, true}, {input.Target.Obligation, false, true}, {input.Source.VaultLiquidityATA, false, true}}
	for i := byte(10); i < 28; i++ {
		key := manifestKey(i)
		input.Source.ObligationDepositReserves = append(input.Source.ObligationDepositReserves, key)
		accounts = append(accounts, InstructionAccount{key, false, i%2 == 0})
	}
	manifest, err := BuildRouteALTManifest(input, settings, policy, payer, []RouteInstruction{{Program: kamino.ProgramID.String(), Accounts: accounts, Data: []byte{1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func waitingManifestPreparation(m ALTManifest) RoutePreparation {
	return RoutePreparation{RouteFingerprint: strings.Repeat("a", 64), RequirementsFingerprint: m.Fingerprint, Manifest: &m, ExecutionPlan: json.RawMessage(`{"route_kind":"same_mint","source_kind":"reserve_position"}`)}
}

func TestWaitingALTUsesCompleteTypedVectorsAndDistinctOrderContracts(t *testing.T) {
	m := waitingManifestFixture(t, manifestKey(80), manifestKey(81), manifestKey(82))
	p := waitingManifestPreparation(m)
	missing := []string{m.VaultAddresses[0].Address}
	shared, vault, err := waitingALTManifestAddresses(p, missing)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != len(m.SharedAddresses) || len(vault) != len(m.VaultAddresses) || len(vault) <= len(missing) {
		t.Fatal("missing-only demand lost full manifest")
	}
	if provisioningAddressesHash(shared) == m.Fingerprint {
		t.Fatal("provisioning row hash substituted for v1 requirements fingerprint")
	}
	var raw [][32]byte
	for i, a := range shared {
		if i > 0 && shared[i-1].Address >= a.Address {
			t.Fatal("persisted order is not base58 lexical")
		}
		key, err := decodePublicKey(a.Address)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, key)
	}
	sort.Slice(raw, func(i, j int) bool { return bytes.Compare(raw[i][:], raw[j][:]) < 0 })
	differs := false
	for i, key := range raw {
		if shared[i].Address != encodeBase58(key[:]) {
			differs = true
		}
	}
	if !differs {
		t.Fatal("fixture failed to exercise raw-key/base58 ordering difference")
	}
	for _, mutate := range []func(*RoutePreparation){
		func(p *RoutePreparation) { p.Manifest = nil },
		func(p *RoutePreparation) { p.RequirementsFingerprint = strings.Repeat("b", 64) },
		func(p *RoutePreparation) { p.Manifest.VaultAddresses[0].AccountRole = "route" },
		func(p *RoutePreparation) {
			p.Manifest.VaultAddresses[0].Writable = !p.Manifest.VaultAddresses[0].Writable
		},
		func(p *RoutePreparation) { p.Manifest.SharedAddresses[0].Ordinal = 99 },
	} {
		copy := m
		copy.SharedAddresses = append([]ALTManifestAddress(nil), m.SharedAddresses...)
		copy.VaultAddresses = append([]ALTManifestAddress(nil), m.VaultAddresses...)
		changed := waitingManifestPreparation(copy)
		mutate(&changed)
		if _, _, err := waitingALTManifestAddresses(changed, missing); err == nil {
			t.Fatal("changed or fabricated manifest accepted")
		}
	}
	for _, missing := range [][]string{nil, {m.SharedAddresses[0].Address}, {manifestKey(99)}, {m.VaultAddresses[0].Address, m.VaultAddresses[0].Address}} {
		if _, _, err := waitingALTManifestAddresses(p, missing); err == nil {
			t.Fatalf("invalid missing membership accepted: %v", missing)
		}
	}
}

// A manifest taken over compute-budget instructions (Rust cross-mint
// preflight, cross_mint.rs:2944-2955) hashes the static budget program but not
// its data values. The same-mint manifest is taken before the budget is added;
// see TestSameMintManifestMatchesRustSetupPolicyFingerprint.
func TestALTManifestHashesStaticBudgetProgramNotItsValues(t *testing.T) {
	input := KaminoSameMintRouteRequest{Vault: manifestKey(81), Source: KaminoPositionAccounts{Reserve: manifestKey(5)}}
	instructions := []RouteInstruction{{Program: kamino.ProgramID.String(), Accounts: []InstructionAccount{{input.Vault, false, true}, {manifestKey(82), false, false}, {input.Source.Reserve, false, true}}}}
	build := func(ixs []RouteInstruction) ALTManifest {
		t.Helper()
		m, err := BuildRouteALTManifest(input, manifestKey(80), manifestKey(82), manifestKey(90), ixs, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	baseline := build(instructions)
	waiting := build(append(computeBudgetInstructions(1_400_000, 0), instructions...))
	final := build(append(computeBudgetInstructions(350_000, 900), instructions...))
	if waiting.Fingerprint != final.Fingerprint || baseline.Fingerprint == final.Fingerprint {
		t.Fatal("raw v1 fingerprint omitted static budget program or hashed dynamic compute values")
	}
	if len(baseline.SharedAddresses) != len(final.SharedAddresses) || len(baseline.VaultAddresses) != len(final.VaultAddresses) {
		t.Fatal("static program became durable ALT demand")
	}
	for i, a := range baseline.SharedAddresses {
		if a != final.SharedAddresses[i] {
			t.Fatal("static budget changed shared demand")
		}
	}
	for i, a := range baseline.VaultAddresses {
		if a != final.VaultAddresses[i] {
			t.Fatal("static budget changed vault demand")
		}
	}
}
