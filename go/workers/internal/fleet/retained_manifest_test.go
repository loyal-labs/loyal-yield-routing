package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func TestRetainedManifestDurableRecordsKeepBase58OrdinalOrder(t *testing.T) {
	input, policy, payer, ixs := manifestFixture()
	manifest, err := BuildRouteALTManifest(input, "", policy, payer, ixs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Fingerprint != "7ae3fd571fb2842e7e8d64191083b1ab0bcc4a4d7dbc61a76418b13e51d1f07c" {
		t.Fatal("typed export changed independent Rust-compatible hash golden")
	}
	sharedRoles := map[byte]string{10: "market", 11: "market_authority", 12: "reserve", 13: "liquidity_mint", 14: "liquidity_supply", 15: "collateral_mint", 16: "collateral_supply", 17: "reserve_farm_state", 18: "oracle,scope_prices", 19: "oracle", 20: "reserve", 21: "reserve", 22: "reserve", 23: "reserve_farm_state"}
	vaultRoles := map[byte]string{30: "vault", 31: "policy", 32: "obligation", 33: "vault_token_account", 34: "farm_user_state", 35: "obligation", 36: "farm_user_state"}
	for _, group := range []struct {
		class string
		roles map[byte]string
		got   []ALTManifestAddress
	}{{"shared_market", sharedRoles, manifest.SharedAddresses}, {"vault", vaultRoles, manifest.VaultAddresses}} {
		var want []ALTManifestAddress
		for n, role := range group.roles {
			want = append(want, ALTManifestAddress{Address: manifestKey(n), SemanticClass: group.class, AccountRole: role, Writable: n%2 == 0})
		}
		sort.Slice(want, func(i, j int) bool { return want[i].Address < want[j].Address })
		for i := range want {
			want[i].Ordinal = int32(i)
		}
		if !reflect.DeepEqual(want, group.got) {
			t.Fatalf("durable %s records differ: got=%+v want=%+v", group.class, group.got, want)
		}
	}
	// Raw byte order starts at n=10; the durable base58 lexical order does not.
	if manifest.SharedAddresses[0].Address == manifestKey(10) {
		t.Fatal("persisted ordinal reused raw-pubkey hash ordering")
	}
}

func TestRetainedManifestSettingsUsesMatureVaultRole(t *testing.T) {
	input, policy, payer, ixs := manifestFixture()
	settings := manifestKey(45)
	ixs[0].Accounts = append(ixs[0].Accounts, InstructionAccount{Address: settings})
	manifest, err := BuildRouteALTManifest(input, settings, policy, payer, ixs, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Independent hashlib/struct oracle: existing golden rows plus vault
	// (45,readonly,[Settings=0]), appended in raw-pubkey order.
	if manifest.Fingerprint != "be2112f0171d5832a4f8a35d7703d9902dfc338e72c29e222e98194b9efc9f3c" {
		t.Fatalf("settings hash drift: %s", manifest.Fingerprint)
	}
	found := false
	for _, record := range manifest.VaultAddresses {
		if record.Address == settings {
			found = record.AccountRole == "settings" && !record.Writable
		}
	}
	if !found {
		t.Fatal("actual settings account omitted from durable vault records")
	}
}

func TestRetainedManifestValidatedSwapUsesSourceProvenance(t *testing.T) {
	body, plan := validJupiterBuildFixture(t)
	vault := testPubkey(31)
	swap, err := validateJupiterEnvelope(body, plan, vault, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, payer, settings := manifestKey(91), manifestKey(92), manifestKey(93)
	wrapped, err := wrapSquadsPolicy(policy, payer, 0, []uint8{0}, []RouteInstruction{swap.Swap})
	if err != nil {
		t.Fatal(err)
	}
	input := KaminoSameMintRouteRequest{Vault: vault, Source: KaminoPositionAccounts{LiquidityMint: USDCMint}, Target: KaminoPositionAccounts{LiquidityMint: USDTMint}}
	manifest, err := BuildRouteALTManifest(input, settings, policy, payer, []RouteInstruction{wrapped}, &swap.Swap)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]ALTManifestAddress{}
	for _, row := range manifest.SharedAddresses {
		roles[row.Address] = row
	}
	if roles[USDCMint].AccountRole != "liquidity_mint" || roles[USDTMint].AccountRole != "liquidity_mint" || roles[testPubkey(33)].AccountRole != "infrastructure" || !roles[testPubkey(33)].Writable {
		t.Fatal("Jupiter builder-owned mint/pool provenance changed")
	}
	if _, ok := roles[payer]; ok {
		t.Fatal("delegated signer entered durable ALT records")
	}
	// A new outer account is not part of the validated swap and therefore has
	// no source-owned semantic role, even if a reusable ALT happens to contain it.
	wrapped.Accounts = append(wrapped.Accounts, InstructionAccount{Address: manifestKey(94)})
	if _, err := BuildRouteALTManifest(input, settings, policy, payer, []RouteInstruction{wrapped}, &swap.Swap); err == nil {
		t.Fatal("untyped outer aggregator account became durable infrastructure")
	}
}

func TestRetainedManifestCompositeKeepsBothBuilderProvenanceSets(t *testing.T) {
	q, plan, bank := crossMintPreparationFixture(t)
	body, _ := jupiterBuildForVault(t, q.Movement.VaultPubkey, 999, 999, 1)
	swap, err := validateJupiterEnvelope(body, plan, q.Movement.VaultPubkey, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	protected := RouteInstruction{Program: KLendProgram, Accounts: []InstructionAccount{{Address: bank.source.Position.Reserve, Writable: true}, {Address: bank.source.Obligation, Writable: true}, {Address: bank.source.Position.VaultLiquidityATA, Writable: true}}}
	withdraw, err := wrapSquadsPolicy(plan.Bindings.Withdraw.PolicyAccount, plan.Bindings.DelegatedSigner, 0, []uint8{0}, []RouteInstruction{protected})
	if err != nil {
		t.Fatal(err)
	}
	wrappedSwap, err := wrapSquadsPolicy(plan.Bindings.Swap.PolicyAccount, plan.Bindings.DelegatedSigner, 0, []uint8{0}, []RouteInstruction{swap.Swap})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildCrossMintPreflightALTManifest(KaminoSameMintRouteRequest{Vault: q.Movement.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position}, plan.Bindings.Settings, plan.Bindings.Withdraw.PolicyAccount, plan.Bindings.Swap.PolicyAccount, plan.Bindings.DelegatedSigner, []RouteInstruction{withdraw, wrappedSwap}, swap.Swap)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, row := range append(append([]ALTManifestAddress{}, manifest.SharedAddresses...), manifest.VaultAddresses...) {
		roles[row.Address] = row.AccountRole
	}
	if roles[bank.source.Position.Reserve] != "reserve" || roles[bank.source.Obligation] != "obligation" || roles[testPubkey(33)] != "infrastructure" || roles[plan.Bindings.Swap.PolicyAccount] != "policy" || roles[plan.Bindings.Withdraw.PolicyAccount] != "policy" {
		t.Fatalf("composite lost a builder provenance set: %+v", roles)
	}
	if err := ValidateALTManifestIntegrity(&manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedManifestIntegrityRejectsMutatedPublicDTO(t *testing.T) {
	input, policy, payer, ixs := manifestFixture()
	for _, mutate := range []func(*ALTManifest){
		func(m *ALTManifest) { m.SharedAddresses[0].Writable = !m.SharedAddresses[0].Writable },
		func(m *ALTManifest) { m.VaultAddresses[0].AccountRole = "policy" },
		func(m *ALTManifest) { m.Fingerprint = strings.Repeat("0", 64) },
	} {
		m, err := BuildRouteALTManifest(input, "", policy, payer, ixs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateALTManifestIntegrity(&m); err != nil {
			t.Fatal(err)
		}
		mutate(&m)
		if err := ValidateALTManifestIntegrity(&m); err == nil {
			t.Fatal("public manifest mutation retained source provenance")
		}
	}
	if err := ValidateALTManifestIntegrity(&ALTManifest{Fingerprint: manifestHash(t, input, policy, payer, ixs)}); err == nil {
		t.Fatal("bare public fingerprint became builder provenance")
	}
}

func TestRetainedManifestExternalCoverageKeepsOriginalSourceIdentities(t *testing.T) {
	input, policy, payer, instructions := manifestFixture()
	original, err := BuildRouteALTManifest(input, "", policy, payer, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	covered := []string{input.Source.Reserve, policy}
	table := LookupTable{Address: manifestKey(244), Addresses: covered, Active: true, Generation: 0}
	data := make([]byte, 56+32*len(covered))
	binary.LittleEndian.PutUint32(data, 1)
	binary.LittleEndian.PutUint64(data[4:12], math.MaxUint64)
	binary.LittleEndian.PutUint64(data[12:20], 900)
	for i, address := range covered {
		fixtureKey(t, data, 56+32*i, address)
	}
	reads := 0
	r := &Revalidator{rpc: crossMintPrepareRPC(t, func(method string, params []json.RawMessage) any {
		var options struct{ Commitment string }
		_ = json.Unmarshal(params[1], &options)
		if method != "getMultipleAccounts" || options.Commitment != "finalized" {
			t.Fatal("external coverage was accepted without actual finalized reader")
		}
		reads++
		return map[string]any{"context": map[string]any{"slot": 1000}, "value": []any{map[string]any{"owner": altProgram, "lamports": 1_000_000, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}}}
	})}
	filtered, err := r.filterFinalizedExternalALTManifest(context.Background(), original, []LookupTable{table}, 1000)
	if err != nil || reads != 1 || filtered.Fingerprint != original.Fingerprint {
		t.Fatalf("source external projection changed full v1 identity: reads=%d err=%v", reads, err)
	}
	if err := ValidateALTManifestIntegrity(&filtered); err != nil {
		t.Fatal(err)
	}
	for _, records := range [][]ALTManifestAddress{filtered.SharedAddresses, filtered.VaultAddresses} {
		for i, row := range records {
			if row.Ordinal != int32(i) || row.Address == policy || row.Address == input.Source.Reserve {
				t.Fatal("filtered durable vector kept external demand or ordinal gap")
			}
		}
	}
	shared, vault, err := ALTManifestSourceAddresses(&filtered)
	if err != nil || !reflect.DeepEqual(shared, original.SharedAddresses) || !reflect.DeepEqual(vault, original.VaultAddresses) {
		t.Fatal("filtered durable demand lost actual source identities")
	}
	shared[0].AccountRole = "altered-return-copy"
	shared, _, err = ALTManifestSourceAddresses(&filtered)
	if err != nil || shared[0].AccountRole == "altered-return-copy" {
		t.Fatal("source accessor returned mutable private provenance")
	}
	// The provider-supplied member vector must equal actual chain bytes. A
	// claimed replacement policy cannot be dropped from demand on assertion.
	table.Addresses = []string{input.Source.Reserve, manifestKey(245)}
	if _, err := r.filterFinalizedExternalALTManifest(context.Background(), original, []LookupTable{table}, 1000); err == nil {
		t.Fatal("unverified external member vector became provisioning coverage")
	}
}

func manifestKey(n byte) string {
	var key solana.PublicKey
	for i := range key {
		key[i] = n
	}
	return key.String()
}

func manifestFixture() (KaminoSameMintRouteRequest, string, string, []RouteInstruction) {
	p := KaminoPositionAccounts{
		Market: manifestKey(10), MarketAuthority: manifestKey(11), Reserve: manifestKey(12),
		LiquidityMint: manifestKey(13), LiquiditySupply: manifestKey(14), CollateralMint: manifestKey(15), CollateralSupply: manifestKey(16),
		ReserveFarmState: manifestKey(17), PythOracle: manifestKey(18), SwitchboardPriceOracle: manifestKey(18), SwitchboardTWAPOracle: manifestKey(19), ScopePrices: manifestKey(18),
		ObligationDepositReserves: []string{manifestKey(20), manifestKey(12)}, ObligationBorrowReserves: []string{manifestKey(21), manifestKey(20)},
		Obligation: manifestKey(32), VaultLiquidityATA: manifestKey(33), ObligationFarmUserState: manifestKey(34),
	}
	target := p
	target.Reserve, target.ReserveFarmState = manifestKey(22), manifestKey(23)
	target.Obligation, target.ObligationFarmUserState = manifestKey(35), manifestKey(36)
	input := KaminoSameMintRouteRequest{Vault: manifestKey(30), Source: p, Target: target}
	ix := RouteInstruction{Program: manifestKey(41), Accounts: []InstructionAccount{{Address: manifestKey(41), Signer: true}}}
	for _, n := range []byte{36, 35, 34, 33, 32, 31, 30, 23, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10} {
		ix.Accounts = append(ix.Accounts, InstructionAccount{Address: manifestKey(n), Writable: n%2 == 0})
	}
	return input, manifestKey(31), manifestKey(40), []RouteInstruction{ix}
}

func retainedSameMintRequirementsFingerprint(input KaminoSameMintRouteRequest, policy, payer string, instructions []RouteInstruction) (string, error) {
	manifest, err := BuildRouteALTManifest(input, "", policy, payer, instructions, nil)
	return manifest.Fingerprint, err
}

func manifestHash(t *testing.T, input KaminoSameMintRouteRequest, policy, payer string, ixs []RouteInstruction) string {
	t.Helper()
	got, err := retainedSameMintRequirementsFingerprint(input, policy, payer, ixs)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Independently assembled with Python hashlib/struct from the Rust wire contract
// in loyal-actions/src/lookup_tables.rs (hash_tag, compiler_accounts, append_*).
// No Go encoder or Rust build produced this oracle. Keys are bytes([n])*32.
// Prefix: b"loyal-route-alt-manifest-v1\0". For each class: pack('<Q', count),
// then bytes([class])+key+bytes([writable])+pack('<Q',len(tags))+bytes(tags).
// Static rows: (40,1,[0,1]), (41,0,[1,3]). Shared rows (n,tags):
// 10:[0],11:[1],12:[2],13:[3],14:[4],15:[5],16:[6],17:[9],18:[7,8],
// 19:[7],20:[2],21:[2],22:[2],23:[9]. Vault rows:
// 30:[1],31:[3],32:[2],33:[5],34:[7],35:[2],36:[7]. Nonstatic writable=n%2==0.
// Canonical input length: 1044 bytes. Includes every position role, role union,
// duplicate reserves/oracles, multiple obligations/farms, and static reason union.
func TestRetainedManifestRustCompatibleGolden(t *testing.T) {
	input, policy, payer, ixs := manifestFixture()
	const want = "7ae3fd571fb2842e7e8d64191083b1ab0bcc4a4d7dbc61a76418b13e51d1f07c"
	if got := manifestHash(t, input, policy, payer, ixs); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// Pubkey byte order is not base58 lexical order for these addresses.
	if strings.Compare(manifestKey(10), manifestKey(18)) < 0 {
		t.Fatal("fixture must distinguish base58 sorting from raw-byte sorting")
	}
	input.Source, input.Target = input.Target, input.Source
	for i, j := 0, len(ixs[0].Accounts)-1; i < j; i, j = i+1, j-1 {
		ixs[0].Accounts[i], ixs[0].Accounts[j] = ixs[0].Accounts[j], ixs[0].Accounts[i]
	}
	ixs = append(ixs, ixs[0])
	ixs[0].Data = []byte{99}
	ixs[0].Step = "not hashed"
	input.WithdrawCollateralAmount = 123
	input.DepositLiquidityAmount = 456
	input.Source.ObligationBorrowReserves = append(input.Source.ObligationBorrowReserves, manifestKey(99)) // unused provenance
	if got := manifestHash(t, input, policy, payer, ixs); got != want {
		t.Fatalf("canonical invariance: got %s want %s", got, want)
	}
}

func TestRetainedManifestMutations(t *testing.T) {
	original, policy, payer, originalIXs := manifestFixture()
	baseline := manifestHash(t, original, policy, payer, originalIXs)
	tests := []struct {
		name   string
		mutate func(*KaminoSameMintRouteRequest, *[]RouteInstruction)
	}{
		{"access", func(_ *KaminoSameMintRouteRequest, ixs *[]RouteInstruction) { (*ixs)[0].Accounts[1].Writable = false }},
		{"signer", func(_ *KaminoSameMintRouteRequest, ixs *[]RouteInstruction) { (*ixs)[0].Accounts[1].Signer = true }},
		{"invoked_program", func(_ *KaminoSameMintRouteRequest, ixs *[]RouteInstruction) {
			*ixs = append(*ixs, RouteInstruction{Program: manifestKey(10)})
		}},
		{"shared_role", func(p *KaminoSameMintRouteRequest, _ *[]RouteInstruction) {
			p.Source.ScopePrices = manifestKey(19)
			p.Target.ScopePrices = manifestKey(19)
		}},
		{"vault_role", func(p *KaminoSameMintRouteRequest, _ *[]RouteInstruction) {
			p.Source.Obligation, p.Source.ObligationFarmUserState = p.Source.ObligationFarmUserState, p.Source.Obligation
		}},
		{"membership", func(_ *KaminoSameMintRouteRequest, ixs *[]RouteInstruction) {
			(*ixs)[0].Accounts = (*ixs)[0].Accounts[:len((*ixs)[0].Accounts)-1]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, policy, payer, ixs := manifestFixture()
			tt.mutate(&input, &ixs)
			if got := manifestHash(t, input, policy, payer, ixs); got == baseline {
				t.Fatal("semantic mutation retained hash")
			}
		})
	}
	// Access is ORed over all occurrences, never last-write-wins.
	_, _, _, ixs := manifestFixture()
	ixs = append(ixs, RouteInstruction{Program: manifestKey(41), Accounts: []InstructionAccount{{Address: manifestKey(36)}}})
	if got := manifestHash(t, original, policy, payer, ixs); got != baseline {
		t.Fatal("readonly duplicate downgraded access")
	}
}

func TestRetainedManifestSentinelsAndInfrastructure(t *testing.T) {
	const zero = "11111111111111111111111111111111"
	const token2022 = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	input := KaminoSameMintRouteRequest{}
	ixs := []RouteInstruction{{Program: manifestKey(41), Accounts: []InstructionAccount{{Address: KLendProgram}, {Address: zero}, {Address: farmsProgram}}}}
	baseline := manifestHash(t, input, "", manifestKey(40), ixs)
	// Independent Python wire oracle, 268 bytes: static 40:[0,1],41:[3];
	// shared zero/KLend/farms:[10], sorted by decoded pubkey, all readonly.
	if baseline != "42b6878c5f08d4f2d2e69c905f23ff7cde044ce46cfc6e6217c519e34666adea" {
		t.Fatalf("sentinel infrastructure golden: %s", baseline)
	}
	input.Source.PythOracle = zero
	input.Source.ScopePrices = zero
	input.Source.ReserveFarmState = zero
	input.Source.ObligationFarmUserState = zero
	if got := manifestHash(t, input, "", manifestKey(40), ixs); got != baseline {
		t.Fatal("absent optional accounts acquired roles")
	}
	input.Source.PythOracle = KLendProgram
	if got := manifestHash(t, input, "", manifestKey(40), ixs); got == baseline {
		t.Fatal("decoded non-default oracle must retain role even at KLend address")
	}
	input.Source.PythOracle = ""
	ixs[0].Accounts = append(ixs[0].Accounts, InstructionAccount{Address: token2022})
	if _, err := retainedSameMintRequirementsFingerprint(input, "", manifestKey(40), ixs); err == nil {
		t.Fatal("unrelated token program must not get guessed provenance")
	}
	input.Source.LiquidityTokenProgram = token2022
	manifestHash(t, input, "", manifestKey(40), ixs)
	// A nonstandard decoded liquidity token program is typed infrastructure too;
	// token-program allowlisting is the route validator's separate responsibility.
	input.Source.LiquidityTokenProgram = manifestKey(50)
	ixs[0].Accounts[len(ixs[0].Accounts)-1].Address = manifestKey(50)
	manifestHash(t, input, "", manifestKey(40), ixs)
}

func TestRetainedManifestRejectsMissingConflictingAndMalformedProvenance(t *testing.T) {
	tests := []struct {
		name, want    string
		input         KaminoSameMintRouteRequest
		policy, payer string
		ixs           []RouteInstruction
	}{
		{name: "missing", want: "missing typed provenance", payer: manifestKey(40), ixs: []RouteInstruction{{Program: manifestKey(41), Accounts: []InstructionAccount{{Address: manifestKey(99)}}}}},
		{name: "unused_conflict", want: "conflicting typed provenance", payer: manifestKey(40), input: KaminoSameMintRouteRequest{Vault: manifestKey(10), Source: KaminoPositionAccounts{Market: manifestKey(10)}}},
		{name: "static_conflict", want: "conflicting typed provenance", payer: manifestKey(10), input: KaminoSameMintRouteRequest{Vault: manifestKey(10), Source: KaminoPositionAccounts{Market: manifestKey(10)}}},
		{name: "placeholder_in_decoded_farm_user", want: "conflicting typed provenance", payer: manifestKey(40), input: KaminoSameMintRouteRequest{Source: KaminoPositionAccounts{ObligationFarmUserState: KLendProgram}}},
		{name: "required_obligation_is_not_optional", want: "conflicting typed provenance", payer: manifestKey(40), input: KaminoSameMintRouteRequest{Source: KaminoPositionAccounts{Obligation: "11111111111111111111111111111111"}}},
		{name: "malformed_role", want: "typed provenance", payer: manifestKey(40), input: KaminoSameMintRouteRequest{Source: KaminoPositionAccounts{PythOracle: "not a pubkey"}}},
		{name: "malformed_payer", want: "manifest account", payer: ""},
		{name: "malformed_program", want: "manifest account", payer: manifestKey(40), ixs: []RouteInstruction{{Program: "invalid"}}},
		{name: "malformed_meta", want: "manifest account", payer: manifestKey(40), ixs: []RouteInstruction{{Program: manifestKey(41), Accounts: []InstructionAccount{{Address: "invalid"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := retainedSameMintRequirementsFingerprint(tt.input, tt.policy, tt.payer, tt.ixs)
			if got != "" || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, %v; want empty hash and %s", got, err, tt.want)
			}
		})
	}
}

func TestRetainedManifestNonceContract(t *testing.T) {
	const system = "11111111111111111111111111111111"
	nonce := manifestKey(60)
	ix := RouteInstruction{Program: system, Data: []byte{4, 0, 0, 0}, Accounts: []InstructionAccount{{Address: nonce, Writable: true}}}
	baseline := manifestHash(t, KaminoSameMintRouteRequest{}, "", manifestKey(40), []RouteInstruction{ix})
	// Independent Python wire oracle, 182 bytes: static zero:(0,[3]),
	// 40:(1,[0,1]), 60:(1,[2]); both lookup-eligible groups empty.
	if baseline != "508c13ecc58a4b2e624bf73703c2880713aabf7488d1b4c41b5b0c31a6bdb227" {
		t.Fatalf("nonce static golden: %s", baseline)
	}
	// Rust checks the prefix, not exact data length, and does not force writable.
	ix.Data = append(ix.Data, 9)
	if got := manifestHash(t, KaminoSameMintRouteRequest{}, "", manifestKey(40), []RouteInstruction{ix}); got != baseline {
		t.Fatal("nonce prefix rejected trailing data")
	}
	for _, name := range []string{"not_first", "wrong_program", "short_data", "wrong_discriminator", "no_account"} {
		t.Run(name, func(t *testing.T) {
			changed := ix
			changed.Data = bytes.Clone(ix.Data)
			ixs := []RouteInstruction{changed}
			switch name {
			case "not_first":
				ixs = append([]RouteInstruction{{Program: manifestKey(41)}}, ixs...)
			case "wrong_program":
				ixs[0].Program = manifestKey(41)
			case "short_data":
				ixs[0].Data = ixs[0].Data[:3]
			case "wrong_discriminator":
				ixs[0].Data[0] = 5
			case "no_account":
				ixs[0].Accounts = nil
			}
			got, err := retainedSameMintRequirementsFingerprint(KaminoSameMintRouteRequest{}, "", manifestKey(40), ixs)
			if name == "no_account" {
				if err != nil || got == baseline {
					t.Fatalf("empty accounts: %s %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "missing typed provenance") {
				t.Fatalf("non-nonce should lack provenance: %s %v", got, err)
			}
		})
	}
}
