package fleetexec

import (
	"errors"
	"reflect"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// Journal decoding binds immutable provider inputs and authentic managed
// membership to the original SDK wire order. It never fetches replacement
// vectors or grants a managed ID to an external table.
func crossMintJournalALTProof(raw []byte, managed []fleet.ExecutionALT, wire []byte, fingerprint string) ([]CrossMintExternalALT, []string, map[string][]string, error) {
	external, savedOrder, err := parseCrossMintExternalALTEvidence(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	transaction, err := sdk.TransactionFromBytes(wire)
	if err != nil {
		return nil, nil, nil, err
	}
	order := make([]string, 0, len(transaction.Message.AddressTableLookups))
	for _, lookup := range transaction.Message.AddressTableLookups {
		order = append(order, lookup.AccountKey.String())
	}
	if len(external) > 0 && !reflect.DeepEqual(order, savedOrder) {
		return nil, nil, nil, errors.New("durable external ALT order differs from original signed message")
	}
	vectors, err := crossMintLookupVectors(managed, external, order)
	if err != nil {
		return nil, nil, nil, err
	}
	actual, err := fleet.CrossMintALTSelectionFingerprint(managed, external, order)
	if err != nil || actual != fingerprint {
		return nil, nil, nil, errors.New("durable ALT selected member identity changed")
	}
	return external, order, vectors, nil
}
