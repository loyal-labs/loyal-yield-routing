package fleetexec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func externalALTFixture(t *testing.T) (*sdk.Transaction, CrossMintPreparedLeg) {
	t.Helper()
	f := mustSignedFixture(t)
	raw, err := base64.StdEncoding.DecodeString(f.SignedWireB64)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := sdk.TransactionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	loaded := sdk.PublicKeyFromBytes(rotateSeed(101, 6)).String()
	tx.Message.AddressTableLookups = sdk.MessageAddressTableLookupSlice{{AccountKey: sdk.MustPublicKeyFromBase58(f.LookupTable), WritableIndexes: []uint8{0}}}
	if len(tx.Message.Instructions) > 0 {
		tx.Message.Instructions[0].Accounts = append(tx.Message.Instructions[0].Accounts, uint16(len(tx.Message.AccountKeys)))
	}
	member := CrossMintExternalALT{Address: f.LookupTable, Addresses: []string{loaded}, ObservedSlot: 1000, UsableAfterSlot: 999}
	member.OrderedAddressHash = mustCrossMintExternalHash(t, member.Addresses)
	p := CrossMintPreparedLeg{ExternalALTs: []CrossMintExternalALT{member}, Preparation: fleet.RoutePreparation{Transaction: fleet.PreparedTransaction{LookupTables: []string{f.LookupTable}, WritableAccounts: []string{f.FeePayer, loaded}}, Simulation: fleet.SimulationEvidence{Slot: 1000}}}
	return tx, p
}

func TestCrossMintExternalHashUsesRetainedLengthPrefixAndOrder(t *testing.T) {
	parts := []string{"11111111111111111111111111111111", fleet.USDCMint}
	if actual := mustCrossMintExternalHash(t, parts); actual != "7afa268b2e4072cf800ce8683e7edbbcb254b11b957bcd4f5c44afeb6d387dc3" {
		t.Fatalf("retained digest differs: %s", actual)
	}
	if mustCrossMintExternalHash(t, parts) == mustCrossMintExternalHash(t, []string{parts[1], parts[0]}) {
		t.Fatal("external content order was discarded")
	}
}

func mustCrossMintExternalHash(t *testing.T, addresses []string) string {
	t.Helper()
	hash, err := fleet.CrossMintExternalAddressHash(addresses)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestCrossMintExternalWireUsesActualSnapshotWithoutInventedManagedIDs(t *testing.T) {
	for _, change := range []string{"valid", "unknown_slot", "not_warm", "forged_hash", "unknown_lookup", "duplicate_lookup", "wrong_order", "wrong_writable", "out_of_bounds", "fake_managed_identity"} {
		t.Run(change, func(t *testing.T) {
			tx, p := externalALTFixture(t)
			switch change {
			case "unknown_slot":
				p.ExternalALTs[0].ObservedSlot = 0
			case "not_warm":
				p.ExternalALTs[0].UsableAfterSlot = 1001
			case "forged_hash":
				p.ExternalALTs[0].Addresses[0] = mustSignedFixture(t).FeePayer
			case "unknown_lookup":
				tx.Message.AddressTableLookups[0].AccountKey = sdk.MustPublicKeyFromBase58(mustSignedFixture(t).FeePayer)
				p.Preparation.Transaction.LookupTables[0] = mustSignedFixture(t).FeePayer
			case "duplicate_lookup":
				tx.Message.AddressTableLookups = append(tx.Message.AddressTableLookups, tx.Message.AddressTableLookups[0])
				p.Preparation.Transaction.LookupTables = append(p.Preparation.Transaction.LookupTables, p.Preparation.Transaction.LookupTables[0])
			case "wrong_order":
				p.Preparation.Transaction.LookupTables = []string{mustSignedFixture(t).FeePayer}
			case "wrong_writable":
				p.Preparation.Transaction.WritableAccounts = []string{mustSignedFixture(t).FeePayer}
			case "out_of_bounds":
				tx.Message.AddressTableLookups[0].WritableIndexes = []uint8{1}
			case "fake_managed_identity":
				p.SelectedALTs = []fleet.ExecutionALT{{Address: p.ExternalALTs[0].Address, Addresses: p.ExternalALTs[0].Addresses}}
			}
			err := verifyCrossMintPreparedWritables(tx, p)
			if (err == nil) != (change == "valid") {
				t.Fatalf("change %s: %v", change, err)
			}
		})
	}
}

func TestCrossMintExternalJournalRetainsExactResolverAndFullImmutableSnapshots(t *testing.T) {
	_, p := externalALTFixture(t)
	raw, err := marshalCrossMintALTEvidence(p)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err = json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	var resolver map[string][]map[string]json.RawMessage
	if err = json.Unmarshal(encoded["resolver"], &resolver); err != nil {
		t.Fatal(err)
	}
	row := resolver["externalLookupTables"][0]
	if len(row) != 3 || row["tableAddress"] == nil || row["addressCount"] == nil || row["orderedAddressHash"] == nil {
		t.Fatal("source resolver shape changed or invented managed fields")
	}
	external, order, err := parseCrossMintExternalALTEvidence(raw)
	if err != nil || !reflect.DeepEqual(external, p.ExternalALTs) || !reflect.DeepEqual(order, p.Preparation.Transaction.LookupTables) {
		t.Fatalf("snapshot/order lost: %v %v %v", external, order, err)
	}
	for _, change := range []string{"legacy_no_snapshot", "missing_clock", "changed_resolver_count", "changed_ordered_member"} {
		t.Run(change, func(t *testing.T) {
			var body map[string]any
			if err = json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "legacy_no_snapshot":
				delete(body, "externalSnapshots")
				delete(body, "lookupTableOrder")
			case "missing_clock":
				delete(body["externalSnapshots"].([]any)[0].(map[string]any), "observedSlot")
			case "changed_resolver_count":
				body["resolver"].(map[string]any)["externalLookupTables"].([]any)[0].(map[string]any)["addressCount"] = 2
			case "changed_ordered_member":
				body["externalSnapshots"].([]any)[0].(map[string]any)["addresses"] = []string{mustSignedFixture(t).FeePayer}
			}
			bad, _ := json.Marshal(body)
			_, _, err = parseCrossMintExternalALTEvidence(bad)
			if err == nil {
				t.Fatal("unknown/tampered evidence admitted")
			}
			if change == "legacy_no_snapshot" && !errors.Is(err, ErrExternalALTObservationRequired) {
				t.Fatal("legacy evidence was not classified for actual source adoption")
			}
		})
	}
	// Registered-only rows retain byte-identical legacy encoding.
	p.ExternalALTs = nil
	p.SelectedALTs = []fleet.ExecutionALT{{TableID: 1, FamilyID: 1, Generation: 0, MutationEpoch: 0, Address: mustSignedFixture(t).LookupTable, Addresses: []string{mustSignedFixture(t).SecondaryAccount}}}
	old, _ := marshalALTEpochs(p.SelectedALTs)
	current, err := marshalCrossMintALTEvidence(p)
	if err != nil || string(old) != string(current) {
		t.Fatal("registered-only journal encoding changed")
	}
}

func TestCrossMintExternalInputsRemainSeparateFromContributingManagedWireOrder(t *testing.T) {
	tx, p := externalALTFixture(t)
	key := func(seed byte) string { return sdk.PublicKeyFromBytes(rotateSeed(seed, 3)).String() }
	managed := fleet.ExecutionALT{TableID: 42, FamilyID: 7, Generation: 0, MutationEpoch: 0, Address: key(102), Addresses: []string{key(103)}}
	p.SelectedALTs = []fleet.ExecutionALT{managed}
	tx.Message.AddressTableLookups = append(tx.Message.AddressTableLookups, sdk.MessageAddressTableLookup{AccountKey: sdk.MustPublicKeyFromBase58(managed.Address), ReadonlyIndexes: []uint8{0}})
	tx.Message.Instructions[0].Accounts = append(tx.Message.Instructions[0].Accounts, uint16(len(tx.Message.AccountKeys)+1))
	p.Preparation.Transaction.LookupTables = append(p.Preparation.Transaction.LookupTables, managed.Address)
	unused := CrossMintExternalALT{Address: key(104), Addresses: []string{key(105)}, ObservedSlot: 1000, UsableAfterSlot: 999}
	unused.OrderedAddressHash = mustCrossMintExternalHash(t, unused.Addresses)
	p.ExternalALTs = append(p.ExternalALTs, unused)
	if err := verifyCrossMintPreparedWritables(tx, p); err != nil {
		t.Fatal(err)
	}
	raw, err := marshalCrossMintALTEvidence(p)
	if err != nil {
		t.Fatal(err)
	}
	external, order, err := parseCrossMintExternalALTEvidence(raw)
	if err != nil || len(external) != 2 || len(order) != 2 || order[0] != p.ExternalALTs[0].Address || order[1] != managed.Address {
		t.Fatalf("resolver inputs confused with wire contributors: %v %v %v", external, order, err)
	}
	// A provider input may also be registered, but its exact ordered vector must
	// match the authentic managed identity rather than supplying a second lease.
	p.ExternalALTs[0].Address = managed.Address
	if err = verifyCrossMintPreparedWritables(tx, p); err == nil {
		t.Fatal("managed/external ordered content conflict admitted")
	}
}

func TestCrossMintExternalSelectionFingerprintBindsCompleteSnapshotAndWireOrder(t *testing.T) {
	_, p := externalALTFixture(t)
	key := func(seed byte) string { return sdk.PublicKeyFromBytes(rotateSeed(seed, 5)).String() }
	unused := CrossMintExternalALT{Address: key(108), Addresses: []string{key(109)}, ObservedSlot: 1000, UsableAfterSlot: 999}
	unused.OrderedAddressHash = mustCrossMintExternalHash(t, unused.Addresses)
	p.ExternalALTs = append(p.ExternalALTs, unused)
	want, err := crossMintALTSelectionFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"unused_contents", "source_clock", "input_order", "contributor_order"} {
		t.Run(change, func(t *testing.T) {
			raw, _ := json.Marshal(p)
			var changed CrossMintPreparedLeg
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "unused_contents":
				changed.ExternalALTs[1].Addresses = []string{key(110)}
				changed.ExternalALTs[1].OrderedAddressHash = mustCrossMintExternalHash(t, changed.ExternalALTs[1].Addresses)
			case "source_clock":
				changed.ExternalALTs[1].ObservedSlot++
			case "input_order":
				changed.ExternalALTs[0], changed.ExternalALTs[1] = changed.ExternalALTs[1], changed.ExternalALTs[0]
			case "contributor_order":
				changed.Preparation.Transaction.LookupTables = []string{unused.Address, p.ExternalALTs[0].Address}
			}
			got, err := crossMintALTSelectionFingerprint(changed)
			if err != nil || got == want {
				t.Fatalf("complete external meaning not committed: %s %v", got, err)
			}
		})
	}
	for _, managed := range [][]fleet.ExecutionALT{nil, {}, {{TableID: 42, FamilyID: 7, Generation: 0}}} {
		legacy := CrossMintPreparedLeg{SelectedALTs: managed}
		raw, _ := json.Marshal(managed)
		digest := sha256.Sum256(raw)
		got, err := crossMintALTSelectionFingerprint(legacy)
		if err != nil || got != hex.EncodeToString(digest[:]) {
			t.Fatal("managed-only fingerprint byte compatibility changed")
		}
	}
}

func TestCrossMintExternalRegisteredOverlapRequiresExactOrderedContents(t *testing.T) {
	_, p := externalALTFixture(t)
	ext := p.ExternalALTs[0]
	p.SelectedALTs = []fleet.ExecutionALT{{TableID: 42, FamilyID: 7, Generation: 0, MutationEpoch: 0, Address: ext.Address, Addresses: append([]string(nil), ext.Addresses...)}}
	if _, err := crossMintALTSelectionFingerprint(p); err != nil {
		t.Fatal(err)
	}
	p.ExternalALTs = append(p.ExternalALTs, ext)
	if _, err := crossMintALTSelectionFingerprint(p); err == nil {
		t.Fatal("duplicate provider input accepted")
	}
}
