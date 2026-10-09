package multiply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// A status-cache hit is not a transaction receipt. A missing or pruned receipt
// and an unavailable endpoint keep the original attempt owned, including after
// restart.
var errReceiptUnavailable = errors.New("confirmed transaction receipt unavailable")

// ReconciledReceiptEvidence is the confirmed receipt and the resolved ALT
// vectors it was proven against, so the store can prove it again.
type ReconciledReceiptEvidence struct {
	OperationID            string
	Signature              string
	WireSHA256             string
	FinancialAnchorsSHA256 string
	ConfirmedSlot          uint64
	ObservationSlot        uint64
	Receipt                chain.Receipt
	LookupTables           map[solana.PublicKey]solana.PublicKeySlice
}

// Its fields are private: only a successfully validated actual receipt creates
// a proof. The store rechecks identity and effects under the operation row lock.
type ReconciledReceiptProof struct{ evidence ReconciledReceiptEvidence }

func (e *Executor) readReceipt(ctx context.Context, op *MultiplyOperation, topology *EarnMaxTopology) (*ReconciledReceiptProof, error) {
	if _, err := PersistedTransaction(op); err != nil {
		return nil, err
	}
	signature, err := solana.SignatureFromBase58(*op.TransactionSignature)
	if err != nil {
		return nil, err
	}
	// Only an absent receipt or an endpoint that cannot answer waits; a receipt
	// the cluster returned but the client rejects is a verdict.
	receipt, err := e.Chain.Receipt(ctx, signature, rpc.CommitmentConfirmed)
	if errors.Is(err, chain.ErrNotFound) || errors.Is(err, chain.ErrUnavailable) {
		return nil, fmt.Errorf("%w: %w", errReceiptUnavailable, err)
	}
	if err != nil {
		return nil, err
	}
	tx, err := decodeVerifiedTransaction(op.SignedWire)
	if err != nil {
		return nil, err
	}
	var tables map[solana.PublicKey]solana.PublicKeySlice
	ids := tx.Message.GetAddressTableLookups().GetTableIDs()
	if len(ids) > 0 {
		tables, err = e.lookupTables(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("%w: ALT read: %w", errReceiptUnavailable, err)
		}
	}
	return validateConfirmedReceipt(op, topology, receipt, tables)
}

func validateConfirmedReceipt(op *MultiplyOperation, topology *EarnMaxTopology, r chain.Receipt, tables map[solana.PublicKey]solana.PublicKeySlice) (*ReconciledReceiptProof, error) {
	wire, err := PersistedTransaction(op)
	if err != nil {
		return nil, err
	}
	if topology == nil {
		return nil, errors.New("receipt topology is invalid")
	}
	if len(op.ExpectedEffects.TokenDeltas) == 0 {
		return nil, errors.New("receipt omitted the source financial token contract")
	}
	if r.Slot == 0 || r.Slot > math.MaxInt64 || r.Err != nil {
		return nil, errors.New("receipt has no successful supported confirmed slot")
	}
	if op.ConfirmedSlot != nil && r.Slot != *op.ConfirmedSlot {
		return nil, errors.New("receipt disagrees with persisted confirmed slot")
	}
	if !bytes.Equal(r.Wire, wire) {
		return nil, errors.New("receipt transaction differs from immutable signed wire")
	}
	tx, err := decodeVerifiedTransaction(r.Wire)
	if err != nil {
		return nil, err
	}
	static := append(solana.PublicKeySlice(nil), tx.Message.AccountKeys...)
	var writable, readonly solana.PublicKeySlice
	for _, lookup := range tx.Message.GetAddressTableLookups() {
		addresses, ok := tables[lookup.AccountKey]
		if !ok {
			return nil, errors.New("receipt omitted exact ALT vector")
		}
		for _, pair := range []struct {
			indexes []uint8
			out     *solana.PublicKeySlice
		}{{lookup.WritableIndexes, &writable}, {lookup.ReadonlyIndexes, &readonly}} {
			for _, index := range pair.indexes {
				if int(index) >= len(addresses) {
					return nil, errors.New("receipt ALT index is outside exact vector")
				}
				*pair.out = append(*pair.out, addresses[index])
			}
		}
	}
	keys := append(append(static, writable...), readonly...)
	if !equalPublicKeys(writable, r.LoadedWritable) || !equalPublicKeys(readonly, r.LoadedReadonly) || !equalPublicKeys(keys, r.Keys) {
		return nil, errors.New("receipt loaded addresses disagree with exact wire lookup order")
	}
	seen := map[solana.PublicKey]bool{}
	for _, key := range keys {
		if seen[key] {
			return nil, errors.New("receipt account keys are invalid or duplicated")
		}
		seen[key] = true
	}
	if len(r.PreLamports) != len(keys) || len(r.PostLamports) != len(keys) {
		return nil, errors.New("receipt omitted complete indexed balance metadata")
	}
	for _, balances := range []map[solana.PublicKey]chain.TokenBalance{r.Pre, r.Post} {
		for _, balance := range balances {
			if balance.Mint.IsZero() || balance.Owner.IsZero() || (balance.Program != mustKey(TokenProgram) && balance.Program != mustKey(Token2022Program)) {
				return nil, errors.New("receipt token metadata has an invalid mint, owner or program")
			}
		}
	}
	for _, anchor := range op.ExpectedEffects.TokenAmountsBefore {
		account, err := solana.PublicKeyFromBase58(anchor.Account)
		if err != nil {
			return nil, errors.New("receipt token anchor account is invalid")
		}
		before, ok := r.Pre[account]
		after, postOK := r.Post[account]
		if !ok || !postOK || before.Mint.String() != anchor.Mint || after.Mint != before.Mint || before.Amount != anchor.AmountRaw || before.Owner != after.Owner || before.Program != after.Program {
			return nil, errors.New("receipt token anchors are absent or disagree with immutable prestate")
		}
		if custodyOwnedByVault(anchor.Account, topology) && before.Owner != topology.Vault {
			return nil, errors.New("receipt custody has a foreign token authority")
		}
		if expected := custodyTokenProgram(anchor.Account, topology); expected != "" && before.Program.String() != expected {
			return nil, errors.New("receipt custody has a different token program")
		}
	}
	// RPC transaction metadata supplies token amounts, not KLend obligation
	// account bytes. Attribute token movement here; verify source obligation
	// direction against the subsequent coherent account bank separately.
	effects := op.ExpectedEffects
	effects.ObligationDelta = nil
	after := &ObservedRoute{Slot: r.Slot}
	for account, balance := range r.Post {
		after.ExternalCustody = append(after.ExternalCustody, TokenBalance{Account: account.String(), Mint: balance.Mint.String(), TokenProgram: balance.Program.String(), AmountRaw: balance.Amount})
	}
	if err := VerifyExpectedEffects(&effects, op.Action, nil, after, topology); err != nil {
		return nil, fmt.Errorf("transaction token receipt: %w", err)
	}
	hash, err := financialAnchorsHash(op.ExpectedEffects)
	if err != nil {
		return nil, err
	}
	return &ReconciledReceiptProof{evidence: ReconciledReceiptEvidence{OperationID: op.OperationID, Signature: *op.TransactionSignature, WireSHA256: *op.SignedWireSHA256, FinancialAnchorsSHA256: hash, ConfirmedSlot: r.Slot, Receipt: r, LookupTables: tables}}, nil
}

func custodyOwnedByVault(account string, topology *EarnMaxTopology) bool {
	if account == topology.ClaimCustody.String() {
		return true
	}
	for _, config := range topology.StrategyCatalog() {
		if account == config.CollateralCustody.String() || account == config.DebtCustody.String() {
			return true
		}
	}
	return false
}

func custodyTokenProgram(account string, topology *EarnMaxTopology) string {
	if account == topology.ClaimCustody.String() {
		return TokenProgram
	}
	for _, config := range topology.StrategyCatalog() {
		if account == config.CollateralCustody.String() {
			return TokenProgram
		}
		if account == config.DebtCustody.String() {
			return config.DebtTokenProgram.String()
		}
	}
	return ""
}

func equalPublicKeys(a, b solana.PublicKeySlice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
