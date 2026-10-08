package fleetexec

import (
	"encoding/json"
	"errors"
	"reflect"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// External snapshots never acquire a managed ID, family, generation or lease.
// The source resolver preserves all provider input tables; the actual message
// chooses contributors and indexes independently from that complete evidence.
type CrossMintExternalALT = fleet.CrossMintExternalALT

type crossMintExternalALTRecord struct {
	Address            string `json:"tableAddress"`
	AddressCount       int    `json:"addressCount"`
	OrderedAddressHash string `json:"orderedAddressHash"`
}

// Legacy resolver evidence stores only count/hash. It must be matched to a
// finalized source read under the live journal fence before first-send adoption.
var ErrExternalALTObservationRequired = errors.New("external ALT requires finalized source snapshot adoption")

// A registered-only packet keeps its existing fingerprint exactly. External
// evidence uses a separate versioned domain and preserves both input snapshot
// order and actual wire order; neither vector is sorted into another meaning.
func crossMintALTSelectionFingerprint(p CrossMintPreparedLeg) (string, error) {
	if len(p.ExternalALTs) > 0 {
		if _, err := crossMintLookupVectors(p.SelectedALTs, p.ExternalALTs, p.Preparation.Transaction.LookupTables); err != nil {
			return "", err
		}
	}
	return fleet.CrossMintALTSelectionFingerprint(p.SelectedALTs, p.ExternalALTs, p.Preparation.Transaction.LookupTables)
}

func validateCrossMintExternalALT(a CrossMintExternalALT) error {
	if _, err := sdk.PublicKeyFromBase58(a.Address); err != nil {
		return err
	}
	if len(a.Addresses) == 0 || len(a.Addresses) > 256 || a.ObservedSlot <= 0 || a.UsableAfterSlot <= 0 || a.ObservedSlot < a.UsableAfterSlot {
		return errors.New("external ALT lacks bounded finalized warm snapshot")
	}
	for _, member := range a.Addresses {
		if _, err := sdk.PublicKeyFromBase58(member); err != nil {
			return err
		}
	}
	hash, err := fleet.CrossMintExternalAddressHash(a.Addresses)
	if err != nil || a.OrderedAddressHash != hash {
		return errors.New("external ALT ordered contents disagree with source commitment")
	}
	return nil
}

func crossMintLookupVectors(managed []fleet.ExecutionALT, external []CrossMintExternalALT, order []string) (map[string][]string, error) {
	if len(managed)+len(external) > 256 || len(external) > 4 {
		return nil, errors.New("external/managed ALT evidence exceeds bounded table count")
	}
	vectors := map[string][]string{}
	ids := map[int64]bool{}
	for _, a := range managed {
		if a.TableID <= 0 || a.FamilyID <= 0 || a.Generation < 0 || a.MutationEpoch < 0 || ids[a.TableID] || len(a.Addresses) == 0 || len(a.Addresses) > 256 {
			return nil, errors.New("managed ALT lacks authentic unique registered identity")
		}
		if _, err := sdk.PublicKeyFromBase58(a.Address); err != nil {
			return nil, err
		}
		if _, exists := vectors[a.Address]; exists {
			return nil, errors.New("ALT identity repeats")
		}
		for _, member := range a.Addresses {
			if _, err := sdk.PublicKeyFromBase58(member); err != nil {
				return nil, err
			}
		}
		ids[a.TableID] = true
		vectors[a.Address] = a.Addresses
	}
	externalNames := map[string]bool{}
	for _, a := range external {
		if err := validateCrossMintExternalALT(a); err != nil {
			return nil, err
		}
		if externalNames[a.Address] {
			return nil, errors.New("external ALT identity repeats")
		}
		externalNames[a.Address] = true
		if existing, exists := vectors[a.Address]; exists && !reflect.DeepEqual(existing, a.Addresses) {
			return nil, errors.New("external ALT conflicts with registered ordered contents")
		}
		vectors[a.Address] = a.Addresses
	}
	seen := map[string]bool{}
	managedOrder := []string{}
	managedNames := map[string]bool{}
	for _, a := range managed {
		managedNames[a.Address] = true
	}
	for _, name := range order {
		if _, exists := vectors[name]; !exists || seen[name] {
			return nil, errors.New("wire lookup order has unknown or repeated identity")
		}
		seen[name] = true
		if managedNames[name] {
			managedOrder = append(managedOrder, name)
		}
	}
	if len(order) > 256 || len(order) == 0 && len(external) > 0 {
		return nil, errors.New("wire lacks bounded contributing ALT order")
	}
	if len(managedOrder) != len(managed) {
		return nil, errors.New("managed selected ALT does not contribute")
	}
	for i, a := range managed {
		if managedOrder[i] != a.Address {
			return nil, errors.New("managed selected ALT order differs from wire")
		}
	}
	return vectors, nil
}

func verifyCrossMintPreparedWritables(transaction *sdk.Transaction, p CrossMintPreparedLeg) error {
	order := make([]string, 0, len(transaction.Message.AddressTableLookups))
	for _, lookup := range transaction.Message.AddressTableLookups {
		order = append(order, lookup.AccountKey.String())
	}
	if len(order) != len(p.Preparation.Transaction.LookupTables) {
		return errors.New("lookup order differs from immutable compiled message")
	}
	for i, name := range order {
		if name != p.Preparation.Transaction.LookupTables[i] {
			return errors.New("lookup order differs from immutable compiled message")
		}
	}
	vectors, err := crossMintLookupVectors(p.SelectedALTs, p.ExternalALTs, order)
	if err != nil {
		return err
	}
	h := transaction.Message.Header
	n := len(transaction.Message.AccountKeys)
	signed := int(h.NumRequiredSignatures)
	if signed > n || int(h.NumReadonlySignedAccounts) > signed || int(h.NumReadonlyUnsignedAccounts) > n-signed {
		return errors.New("invalid prepared account header roles")
	}
	physical := []string{}
	for i, key := range transaction.Message.AccountKeys {
		if i < signed-int(h.NumReadonlySignedAccounts) || i >= signed && i < n-int(h.NumReadonlyUnsignedAccounts) {
			physical = append(physical, key.String())
		}
	}
	for _, lookup := range transaction.Message.AddressTableLookups {
		members := vectors[lookup.AccountKey.String()]
		if len(lookup.WritableIndexes)+len(lookup.ReadonlyIndexes) == 0 {
			return errors.New("compiled ALT makes no contribution")
		}
		for _, index := range lookup.WritableIndexes {
			if int(index) >= len(members) {
				return errors.New("writable index exceeds external/managed snapshot")
			}
			physical = append(physical, members[index])
		}
		for _, index := range lookup.ReadonlyIndexes {
			if int(index) >= len(members) {
				return errors.New("readonly index exceeds external/managed snapshot")
			}
		}
	}
	if !sameStrings(physical, p.Preparation.Transaction.WritableAccounts) {
		return errors.New("prepared writable vector differs from exact external/managed wire roles")
	}
	return nil
}

func marshalCrossMintALTEvidence(p CrossMintPreparedLeg) ([]byte, error) {
	raw, err := marshalALTEpochs(p.SelectedALTs)
	if err != nil {
		return nil, err
	}
	if len(p.ExternalALTs) == 0 {
		return raw, nil
	}
	if _, err = crossMintLookupVectors(p.SelectedALTs, p.ExternalALTs, p.Preparation.Transaction.LookupTables); err != nil {
		return nil, err
	}
	var evidence map[string]json.RawMessage
	if err = json.Unmarshal(raw, &evidence); err != nil {
		return nil, err
	}
	for _, a := range p.ExternalALTs {
		if p.Preparation.Simulation.Slot < a.ObservedSlot {
			return nil, errors.New("simulation precedes finalized external snapshot")
		}
	}
	evidence["externalSnapshots"], err = json.Marshal(p.ExternalALTs)
	if err != nil {
		return nil, err
	}
	evidence["lookupTableOrder"], err = json.Marshal(p.Preparation.Transaction.LookupTables)
	if err != nil {
		return nil, err
	}
	records := make([]crossMintExternalALTRecord, 0, len(p.ExternalALTs))
	for _, a := range p.ExternalALTs {
		records = append(records, crossMintExternalALTRecord{a.Address, len(a.Addresses), a.OrderedAddressHash})
	}
	evidence["resolver"], err = json.Marshal(struct {
		External []crossMintExternalALTRecord `json:"externalLookupTables"`
	}{records})
	if err != nil {
		return nil, err
	}
	return json.Marshal(evidence)
}

func parseCrossMintExternalALTEvidence(raw []byte) ([]CrossMintExternalALT, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	if value, present := fields["externalSnapshots"]; present && string(value) == "null" {
		return nil, nil, ErrExternalALTObservationRequired
	}
	var body struct {
		External []CrossMintExternalALT `json:"externalSnapshots"`
		Order    []string               `json:"lookupTableOrder"`
		Resolver struct {
			External []crossMintExternalALTRecord `json:"externalLookupTables"`
		} `json:"resolver"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, err
	}
	if len(body.External) == 0 {
		if len(body.Resolver.External) > 0 {
			return nil, nil, ErrExternalALTObservationRequired
		}
		return nil, nil, nil
	}
	if len(body.Order) == 0 || len(body.Order) > 256 || len(body.External) > 4 || len(body.External) != len(body.Resolver.External) {
		return nil, nil, errors.New("external durable proof lacks complete resolver/order")
	}
	seen := map[string]bool{}
	for i, a := range body.External {
		if err := validateCrossMintExternalALT(a); err != nil {
			return nil, nil, err
		}
		if seen[a.Address] {
			return nil, nil, errors.New("durable external identity repeats")
		}
		seen[a.Address] = true
		r := body.Resolver.External[i]
		if r.Address != a.Address || r.AddressCount != len(a.Addresses) || r.OrderedAddressHash != a.OrderedAddressHash {
			return nil, nil, errors.New("external snapshot differs from source resolver")
		}
	}
	return body.External, body.Order, nil
}
