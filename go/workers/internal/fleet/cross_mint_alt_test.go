package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	solana "github.com/gagliardetto/solana-go"
)

func externalALTFixture(t *testing.T, key byte, members ...byte) CrossMintExternalALT {
	t.Helper()
	x := CrossMintExternalALT{Address: testPubkey(key), ObservedSlot: 100, UsableAfterSlot: 90}
	for _, member := range members {
		x.Addresses = append(x.Addresses, testPubkey(member))
	}
	var err error
	x.OrderedAddressHash, err = CrossMintExternalAddressHash(x.Addresses)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestCrossMintExternalSourceFingerprintGoldens(t *testing.T) {
	// Independent Python hashlib/struct oracle using u64LE UTF-8 lengths.
	hash, err := CrossMintExternalAddressHash([]string{testPubkey(31), testPubkey(32), testPubkey(31)})
	if err != nil || hash != "99e95dcf30fa58cc53a5c9dce4153c9362bd2565b55749a15fc2c7d960906119" {
		t.Fatalf("ordered hash changed index/duplicate contract: %s %v", hash, err)
	}
	external := []CrossMintExternalALT{externalALTFixture(t, 15, 32, 31), externalALTFixture(t, 14, 31, 32)}
	if external[0].Address >= external[1].Address {
		t.Fatal("fixture must distinguish base58 and raw pubkey sort")
	}
	fp, err := crossMintExternalRequirementsFingerprint(strings.Repeat("a", 64), external)
	if err != nil || fp != "4b3ecc90efc77ece5d3d441f7946ea38ab29fc2c42c4bc6b273926dae7b2e593" {
		t.Fatalf("source composite requirements changed raw-key ordering: %s %v", fp, err)
	}
	managed := []ExecutionALT{{TableID: 1, FamilyID: 2, Address: testPubkey(14), Addresses: []string{testPubkey(31)}}}
	fp, err = CrossMintALTSelectionFingerprint(managed, external[:1], []string{testPubkey(14), testPubkey(15)})
	if err != nil || fp != "ad94a2dfe0ce5c251ffde1dd096a93e38c77f5d343270db6af3953ee1f1edf2e" {
		t.Fatalf("combined selection envelope changed: %s %v", fp, err)
	}
	for _, managed := range [][]ExecutionALT{nil, {}, managed} {
		encoded, _ := json.Marshal(managed)
		digest := sha256.Sum256(encoded)
		got, err := CrossMintALTSelectionFingerprint(managed, nil, nil)
		if err != nil || got != hex.EncodeToString(digest[:]) {
			t.Fatal("managed-only v1 nil/empty/identity fingerprint changed")
		}
	}
}

func TestCrossMintExternalSelectionBindsUnusedInputsAndRejectsForgery(t *testing.T) {
	managed := []ExecutionALT{{TableID: 1, FamilyID: 2, Address: testPubkey(14), Addresses: []string{testPubkey(31)}}}
	external := []CrossMintExternalALT{externalALTFixture(t, 15, 32, 31)}
	order := []string{managed[0].Address, external[0].Address}
	base, err := CrossMintALTSelectionFingerprint(managed, external, order)
	if err != nil {
		t.Fatal(err)
	}
	withUnused := append(cloneCrossMintExternalALTs(external), externalALTFixture(t, 16, 33))
	if got, err := CrossMintALTSelectionFingerprint(managed, withUnused, order); err != nil || got == base {
		t.Fatal("zero-contribution provider input was dropped from source evidence")
	}
	if got, err := CrossMintALTSelectionFingerprint(managed, external, []string{order[1], order[0]}); err != nil || got == base {
		t.Fatal("combined selection did not bind actual wire order")
	}
	for name, change := range map[string]func([]ExecutionALT, []CrossMintExternalALT, []string){
		"fake managed id":      func(m []ExecutionALT, _ []CrossMintExternalALT, _ []string) { m[0].TableID = 0 },
		"negative generation":  func(m []ExecutionALT, _ []CrossMintExternalALT, _ []string) { m[0].Generation = -1 },
		"bad vector hash":      func(_ []ExecutionALT, x []CrossMintExternalALT, _ []string) { x[0].Addresses[0] = testPubkey(34) },
		"noncanonical member":  func(_ []ExecutionALT, x []CrossMintExternalALT, _ []string) { x[0].Addresses[0] = "not-a-key" },
		"unobserved":           func(_ []ExecutionALT, x []CrossMintExternalALT, _ []string) { x[0].ObservedSlot = 0 },
		"not warm":             func(_ []ExecutionALT, x []CrossMintExternalALT, _ []string) { x[0].UsableAfterSlot = 101 },
		"unknown wire table":   func(_ []ExecutionALT, _ []CrossMintExternalALT, o []string) { o[1] = testPubkey(35) },
		"duplicate wire table": func(_ []ExecutionALT, _ []CrossMintExternalALT, o []string) { o[1] = o[0] },
	} {
		t.Run(name, func(t *testing.T) {
			m := append([]ExecutionALT{}, managed...)
			x := cloneCrossMintExternalALTs(external)
			o := append([]string{}, order...)
			change(m, x, o)
			if _, err := CrossMintALTSelectionFingerprint(m, x, o); err == nil {
				t.Fatal("forged external selection accepted")
			}
		})
	}
	overlap := append(cloneCrossMintExternalALTs(external), externalALTFixture(t, 14, 31))
	if _, err := CrossMintALTSelectionFingerprint(managed, overlap, order); err != nil {
		t.Fatal("source-proven identical managed/provider overlap rejected:", err)
	}
	overlap[1] = externalALTFixture(t, 14, 32)
	if _, err := CrossMintALTSelectionFingerprint(managed, overlap, order); err == nil {
		t.Fatal("same table address accepted different provider members")
	}
}

func TestCrossMintExternalCompilationPreservesManagedFirstSDKWireOrder(t *testing.T) {
	managed := []LookupTable{{ID: 1, FamilyID: 2, Address: testPubkey(14), Addresses: []string{testPubkey(31)}, Active: true, UsableAfterSlot: 90, LastVerifiedSlot: 100}}
	external := []LookupTable{{Address: testPubkey(15), Addresses: []string{testPubkey(32), testPubkey(31)}, Active: true, UsableAfterSlot: 90, LastVerifiedSlot: 100}, {Address: testPubkey(16), Addresses: []string{testPubkey(33)}, Active: true, UsableAfterSlot: 90, LastVerifiedSlot: 100}}
	tables, err := combineCrossMintLookupTables(managed, external)
	if err != nil {
		t.Fatal(err)
	}
	prep, _, err := compileV0Transaction(testPubkey(40), testPubkey(41), []RouteInstruction{{Program: testPubkey(42), Accounts: []InstructionAccount{{Address: testPubkey(31), Writable: true}, {Address: testPubkey(32)}}}}, tables, 1, 200000)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := solana.TransactionFromBytes(prep.UnsignedWire)
	if err != nil || len(tx.Message.AddressTableLookups) != 2 || !reflect.DeepEqual(prep.LookupTables, []string{managed[0].Address, external[0].Address}) || tx.Message.AddressTableLookups[0].AccountKey.String() != managed[0].Address || tx.Message.AddressTableLookups[1].AccountKey.String() != external[0].Address {
		t.Fatalf("compiled lookup order changed or retained unused table: %+v %v", prep.LookupTables, err)
	}
}

func TestCrossMintExternalManifestBindsActualReadbackAndCompositeIdentity(t *testing.T) {
	input, policy, payer, instructions := manifestFixture()
	original, err := BuildRouteALTManifest(input, "", policy, payer, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	addresses := []string{input.Source.Reserve, policy}
	data := make([]byte, 56+32*len(addresses))
	binary.LittleEndian.PutUint32(data, 1)
	binary.LittleEndian.PutUint64(data[4:12], math.MaxUint64)
	binary.LittleEndian.PutUint64(data[12:20], 95)
	for i, address := range addresses {
		fixtureKey(t, data, 56+i*32, address)
	}
	r := &Revalidator{rpc: crossMintPrepareRPC(t, func(method string, params []json.RawMessage) any {
		var options struct {
			Commitment     string
			MinContextSlot int64
		}
		_ = json.Unmarshal(params[1], &options)
		if method != "getMultipleAccounts" || options.Commitment != "finalized" || options.MinContextSlot != 100 {
			t.Fatal("external proof used wrong commitment or anchor")
		}
		return map[string]any{"context": map[string]any{"slot": 110}, "value": []any{map[string]any{"owner": altProgram, "lamports": 1000, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}}}
	})}
	bound, err := r.bindFinalizedCrossMintALTManifest(context.Background(), original, []LookupTable{{Address: testPubkey(44), Addresses: addresses, Active: true, LastVerifiedSlot: 1, UsableAfterSlot: 1}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := ALTManifestExternalSnapshots(&bound)
	if err != nil || len(snapshots) != 1 || snapshots[0].ObservedSlot != 110 || snapshots[0].UsableAfterSlot != 96 {
		t.Fatalf("minimum input substituted for actual warm/readback facts: %+v %v", snapshots, err)
	}
	fp, err := ALTManifestRequirementsFingerprint(&bound)
	if err != nil || fp == original.Fingerprint || bound.Fingerprint != original.Fingerprint {
		t.Fatal("external requirements lost separate raw v1 identity")
	}
	want, err := crossMintExternalRequirementsFingerprint(original.Fingerprint, snapshots)
	if err != nil || want != fp {
		t.Fatal("manifest composite differs from mature source identity")
	}
	if untouched, err := ALTManifestRequirementsFingerprint(&original); err != nil || untouched != original.Fingerprint {
		t.Fatal("pure hash calculation forged finalized binder authority")
	}
	snapshots[0].Addresses[0] = testPubkey(45)
	if err := ValidateALTManifestIntegrity(&bound); err != nil {
		t.Fatal("accessor returned private mutable provenance")
	}
	bound.VaultAddresses[0].AccountRole = "forged"
	if _, err := ALTManifestRequirementsFingerprint(&bound); err == nil {
		t.Fatal("modified durable vector retained externally bound identity")
	}
	loaded, err := r.loadFinalizedJupiterTables(context.Background(), map[string][]string{testPubkey(44): addresses}, 100)
	if err != nil || loaded[0].LastVerifiedSlot != 110 || loaded[0].UsableAfterSlot != 96 {
		t.Fatalf("provider loader forged minimum clocks: %+v %v", loaded, err)
	}
}
