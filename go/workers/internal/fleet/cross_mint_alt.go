package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
)

// External snapshots are finalized provider inputs, including tables which do
// not contribute to the compiled message. They never carry managed identities
// or acquire managed usage leases. Member order is the Solana index contract.
type CrossMintExternalALT struct {
	Address            string   `json:"tableAddress"`
	Addresses          []string `json:"addresses"`
	OrderedAddressHash string   `json:"orderedAddressHash"`
	ObservedSlot       int64    `json:"observedSlot"`
	UsableAfterSlot    int64    `json:"usableAfterSlot"`
}

func crossMintALTPartsHash(parts []string) string {
	h := sha256.New()
	var size [8]byte
	for _, part := range parts {
		binary.LittleEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CrossMintExternalAddressHash ports Rust's ordered_lookup_table_address_hash.
// Repeated members retain their original indexes; they must not be deduplicated.
func CrossMintExternalAddressHash(addresses []string) (string, error) {
	if len(addresses) == 0 || len(addresses) > 256 {
		return "", errors.New("external ALT requires bounded complete member vector")
	}
	for _, address := range addresses {
		if _, err := decodePublicKey(address); err != nil {
			return "", errors.New("external ALT member is not a canonical public key")
		}
	}
	return crossMintALTPartsHash(addresses), nil
}

func validateCrossMintExternalALTs(external []CrossMintExternalALT) error {
	if len(external) > 4 {
		return errors.New("too many external ALT snapshots")
	}
	seen := map[string]bool{}
	for _, table := range external {
		if _, err := decodePublicKey(table.Address); err != nil || seen[table.Address] || table.ObservedSlot <= 0 || table.UsableAfterSlot <= 0 || table.UsableAfterSlot > table.ObservedSlot {
			return errors.New("external ALT snapshot identity or actual warm clocks are invalid")
		}
		seen[table.Address] = true
		hash, err := CrossMintExternalAddressHash(table.Addresses)
		if err != nil || hash != table.OrderedAddressHash {
			return errors.New("external ALT snapshot changed its ordered member vector")
		}
	}
	return nil
}

func crossMintExternalRequirementsFingerprint(v1 string, external []CrossMintExternalALT) (string, error) {
	if err := validateCrossMintExternalALTs(external); err != nil {
		return "", err
	}
	if len(external) == 0 {
		return v1, nil
	}
	tables := append([]CrossMintExternalALT{}, external...)
	sort.Slice(tables, func(i, j int) bool {
		a, _ := decodePublicKey(tables[i].Address)
		b, _ := decodePublicKey(tables[j].Address)
		return bytes.Compare(a[:], b[:]) < 0
	})
	parts := []string{v1}
	for _, table := range tables {
		parts = append(parts, table.Address)
		parts = append(parts, table.Addresses...)
	}
	return crossMintALTPartsHash(parts), nil
}

// CrossMintALTSelectionFingerprint preserves the existing managed-only JSON
// hash byte for byte. External routes bind all provider snapshots and actual
// contributing wire order in a versioned envelope; no managed IDs are invented.
func CrossMintALTSelectionFingerprint(managed []ExecutionALT, external []CrossMintExternalALT, lookupOrder []string) (string, error) {
	if len(external) == 0 {
		encoded, err := json.Marshal(managed)
		if err != nil {
			return "", err
		}
		h := sha256.Sum256(encoded)
		return hex.EncodeToString(h[:]), nil
	}
	if err := validateCrossMintExternalALTs(external); err != nil {
		return "", err
	}
	tables := map[string][]string{}
	managedNames := map[string]bool{}
	for _, table := range managed {
		if _, err := decodePublicKey(table.Address); err != nil || managedNames[table.Address] || table.TableID <= 0 || table.FamilyID <= 0 || table.Generation < 0 || table.MutationEpoch < 0 {
			return "", errors.New("managed ALT selection lacks authentic identity")
		}
		if _, err := CrossMintExternalAddressHash(table.Addresses); err != nil {
			return "", err
		}
		tables[table.Address], managedNames[table.Address] = table.Addresses, true
	}
	for _, table := range external {
		if original, exists := tables[table.Address]; exists && !reflect.DeepEqual(original, table.Addresses) {
			return "", errors.New("managed and external copies disagree on the same table")
		}
		tables[table.Address] = table.Addresses
	}
	seen := map[string]bool{}
	for _, name := range lookupOrder {
		if _, exists := tables[name]; !exists || seen[name] {
			return "", errors.New("wire ALT order has unknown or repeated identity")
		}
		seen[name] = true
	}
	if len(lookupOrder) == 0 {
		return "", errors.New("external selection has no contributing wire lookup")
	}
	for name := range managedNames {
		if !seen[name] {
			return "", errors.New("selected managed ALT does not contribute to wire")
		}
	}
	if managed == nil {
		managed = []ExecutionALT{}
	}
	envelope := struct {
		Managed          []ExecutionALT         `json:"managed"`
		External         []CrossMintExternalALT `json:"external"`
		LookupTableOrder []string               `json:"lookupTableOrder"`
	}{managed, external, lookupOrder}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return crossMintALTPartsHash([]string{"loyal-cross-mint-external-alt-selection-v1", string(encoded)}), nil
}

func cloneCrossMintExternalALTs(tables []CrossMintExternalALT) []CrossMintExternalALT {
	if tables == nil {
		return nil
	}
	out := append([]CrossMintExternalALT{}, tables...)
	for i := range out {
		out[i].Addresses = append([]string{}, out[i].Addresses...)
	}
	return out
}

// The retained resolver compiles managed tables first, then provider inputs
// not already present. The compiler prunes unused tables from actual wireorder.
func combineCrossMintLookupTables(managed, external []LookupTable) ([]LookupTable, error) {
	out := append([]LookupTable{}, managed...)
	seen := map[string][]string{}
	for _, table := range managed {
		if _, exists := seen[table.Address]; exists {
			return nil, errors.New("managed ALT input has duplicate table identity")
		}
		seen[table.Address] = table.Addresses
	}
	provider := map[string]bool{}
	for _, table := range external {
		if provider[table.Address] {
			return nil, errors.New("provider ALT input has duplicate table identity")
		}
		provider[table.Address] = true
		if members, exists := seen[table.Address]; exists {
			if !reflect.DeepEqual(members, table.Addresses) {
				return nil, errors.New("managed and provider ALT vectors disagree")
			}
			continue
		}
		seen[table.Address] = table.Addresses
		out = append(out, table)
	}
	return out, nil
}
