package multiply

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/solana-foundation/solana-go/v2"
)

// A status-cache hit is not a transaction receipt. Missing/pruned receipts and
// transport errors keep the original attempt owned, including after restart.
var errReceiptUnavailable = errors.New("confirmed transaction receipt unavailable")

type confirmedTransactionReader interface {
	ConfirmedTransaction(context.Context, string) (json.RawMessage, error)
}

func (c *LiveRPCSurface) ConfirmedTransaction(ctx context.Context, signature string) (json.RawMessage, error) {
	if _, err := solana.SignatureFromBase58(signature); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.call(ctx, "getTransaction", []any{signature, map[string]any{
		"encoding": "base64", "commitment": "confirmed", "maxSupportedTransactionVersion": 0,
	}}, &raw); err != nil {
		return nil, fmt.Errorf("%w: %w", errReceiptUnavailable, err)
	}
	return raw, nil
}

type receiptToken struct {
	Index     *uint16 `json:"accountIndex"`
	Mint      string  `json:"mint"`
	Owner     string  `json:"owner"`
	ProgramID string  `json:"programId"`
	Amount    struct {
		Raw      string `json:"amount"`
		Decimals *uint8 `json:"decimals"`
	} `json:"uiTokenAmount"`
}

type confirmedReceipt struct {
	Slot        uint64            `json:"slot"`
	Transaction []json.RawMessage `json:"transaction"`
	Meta        *struct {
		Err          json.RawMessage `json:"err"`
		PreBalances  []uint64        `json:"preBalances"`
		PostBalances []uint64        `json:"postBalances"`
		PreTokens    []receiptToken  `json:"preTokenBalances"`
		PostTokens   []receiptToken  `json:"postTokenBalances"`
		Loaded       struct {
			Writable []solana.PublicKey `json:"writable"`
			Readonly []solana.PublicKey `json:"readonly"`
		} `json:"loadedAddresses"`
	} `json:"meta"`
}

// Receipt evidence lives beside unchanged Rust financial JSON. The raw RPC
// transaction and the resolved ALT vectors remain independently reviewable.
type ReconciledReceiptEvidence struct {
	OperationID            string                                     `json:"operationId"`
	Signature              string                                     `json:"signature"`
	WireSHA256             string                                     `json:"wireSha256"`
	FinancialAnchorsSHA256 string                                     `json:"financialAnchorsSha256"`
	ConfirmedSlot          uint64                                     `json:"confirmedSlot"`
	ObservationSlot        uint64                                     `json:"observationSlot"`
	Transaction            json.RawMessage                            `json:"transaction"`
	LookupTables           map[solana.PublicKey]solana.PublicKeySlice `json:"lookupTables,omitempty"`
}

// Its fields are private: only a successfully validated actual receipt creates
// a proof. The store rechecks identity and effects under the operation row lock.
type ReconciledReceiptProof struct{ evidence ReconciledReceiptEvidence }

func (e *Executor) readReceipt(ctx context.Context, op *MultiplyOperation, topology *EarnMaxTopology) (*ReconciledReceiptProof, error) {
	if _, err := PersistedTransaction(op); err != nil {
		return nil, err
	}
	reader, ok := e.RPC.(confirmedTransactionReader)
	if !ok {
		return nil, errReceiptUnavailable
	}
	raw, err := reader.ConfirmedTransaction(ctx, *op.TransactionSignature)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReceiptUnavailable, err)
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errReceiptUnavailable
	}
	tx, err := decodeVerifiedTransaction(op.SignedWire)
	if err != nil {
		return nil, err
	}
	var tables map[solana.PublicKey]solana.PublicKeySlice
	ids := tx.Message.GetAddressTableLookups().GetTableIDs()
	if len(ids) > 0 {
		tables, err = e.RPC.LookupTables(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("%w: ALT read: %w", errReceiptUnavailable, err)
		}
	}
	return validateConfirmedReceipt(op, topology, raw, tables)
}

func validateConfirmedReceipt(op *MultiplyOperation, topology *EarnMaxTopology, raw json.RawMessage, tables map[solana.PublicKey]solana.PublicKeySlice) (*ReconciledReceiptProof, error) {
	wire, err := PersistedTransaction(op)
	if err != nil {
		return nil, err
	}
	if topology == nil || len(raw) > 1<<22 {
		return nil, errors.New("receipt topology or size is invalid")
	}
	if len(op.ExpectedEffects.TokenDeltas) == 0 {
		return nil, errors.New("receipt omitted the source financial token contract")
	}
	var r confirmedReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("invalid transaction receipt: %w", err)
	}
	if r.Slot == 0 || r.Slot > math.MaxInt64 || r.Meta == nil || !bytes.Equal(bytes.TrimSpace(r.Meta.Err), []byte("null")) {
		return nil, errors.New("receipt has no successful supported confirmed slot")
	}
	if op.ConfirmedSlot != nil && r.Slot != *op.ConfirmedSlot {
		return nil, errors.New("receipt disagrees with persisted confirmed slot")
	}
	if len(r.Transaction) != 2 {
		return nil, errors.New("receipt omitted base64 signed transaction")
	}
	var encoded, encoding string
	if json.Unmarshal(r.Transaction[0], &encoded) != nil || json.Unmarshal(r.Transaction[1], &encoding) != nil || encoding != "base64" {
		return nil, errors.New("receipt transaction encoding is invalid")
	}
	actualWire, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(actualWire, wire) {
		return nil, errors.New("receipt transaction differs from immutable signed wire")
	}
	tx, err := decodeVerifiedTransaction(actualWire)
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
	if !equalPublicKeys(writable, r.Meta.Loaded.Writable) || !equalPublicKeys(readonly, r.Meta.Loaded.Readonly) {
		return nil, errors.New("receipt loaded addresses disagree with exact wire lookup order")
	}
	keys := append(append(static, writable...), readonly...)
	seen := map[solana.PublicKey]bool{}
	for _, key := range keys {
		if seen[key] {
			return nil, errors.New("receipt account keys are invalid or duplicated")
		}
		seen[key] = true
	}
	if len(r.Meta.PreBalances) != len(keys) || len(r.Meta.PostBalances) != len(keys) {
		return nil, errors.New("receipt omitted complete indexed balance metadata")
	}
	pre, err := receiptBalances(r.Meta.PreTokens, keys)
	if err != nil {
		return nil, err
	}
	post, err := receiptBalances(r.Meta.PostTokens, keys)
	if err != nil {
		return nil, err
	}
	for _, anchor := range op.ExpectedEffects.TokenAmountsBefore {
		identity := anchor.Account + ":" + anchor.Mint
		before, ok := pre[identity]
		after, postOK := post[identity]
		if !ok || !postOK || before.AmountRaw != anchor.AmountRaw || before.Owner != after.Owner || before.TokenProgram != after.TokenProgram || before.Decimals != after.Decimals {
			return nil, errors.New("receipt token anchors are absent or disagree with immutable prestate")
		}
		if custodyOwnedByVault(anchor.Account, topology) && before.Owner != topology.Vault.String() {
			return nil, errors.New("receipt custody has a foreign token authority")
		}
		if expected := custodyTokenProgram(anchor.Account, topology); expected != "" && before.TokenProgram != expected {
			return nil, errors.New("receipt custody has a different token program")
		}
	}
	// RPC transaction metadata supplies token amounts, not KLend obligation
	// account bytes. Attribute token movement here; verify source obligation
	// direction against the subsequent coherent account bank separately.
	effects := op.ExpectedEffects
	effects.ObligationDelta = nil
	after := &ObservedRoute{Slot: r.Slot}
	for _, balance := range post {
		after.ExternalCustody = append(after.ExternalCustody, balance.TokenBalance)
	}
	if err := VerifyExpectedEffects(&effects, op.Action, nil, after, topology); err != nil {
		return nil, fmt.Errorf("transaction token receipt: %w", err)
	}
	hash, err := financialAnchorsHash(op.ExpectedEffects)
	if err != nil {
		return nil, err
	}
	return &ReconciledReceiptProof{evidence: ReconciledReceiptEvidence{OperationID: op.OperationID, Signature: *op.TransactionSignature, WireSHA256: *op.SignedWireSHA256, FinancialAnchorsSHA256: hash, ConfirmedSlot: r.Slot, Transaction: append(json.RawMessage(nil), raw...), LookupTables: tables}}, nil
}

type receiptBalance struct {
	TokenBalance
	Owner    string
	Decimals uint8
}

func receiptBalances(tokens []receiptToken, keys solana.PublicKeySlice) (map[string]receiptBalance, error) {
	result := make(map[string]receiptBalance, len(tokens))
	indexes := map[uint16]bool{}
	for _, token := range tokens {
		if token.Index == nil || int(*token.Index) >= len(keys) || indexes[*token.Index] || token.Amount.Decimals == nil || (token.ProgramID != TokenProgram && token.ProgramID != Token2022Program) {
			return nil, errors.New("receipt token metadata has invalid or duplicated index/program")
		}
		indexes[*token.Index] = true
		if key, err := solana.PublicKeyFromBase58(token.Mint); err != nil || key.IsZero() {
			return nil, errors.New("receipt token mint is invalid")
		}
		if key, err := solana.PublicKeyFromBase58(token.Owner); err != nil || key.IsZero() {
			return nil, errors.New("receipt token owner is missing or invalid")
		}
		amount, err := strconv.ParseUint(token.Amount.Raw, 10, 64)
		if err != nil || strconv.FormatUint(amount, 10) != token.Amount.Raw {
			return nil, errors.New("receipt token amount is not canonical u64")
		}
		account := keys[*token.Index].String()
		result[account+":"+token.Mint] = receiptBalance{TokenBalance: TokenBalance{Account: account, Mint: token.Mint, TokenProgram: token.ProgramID, AmountRaw: amount}, Owner: token.Owner, Decimals: *token.Amount.Decimals}
	}
	return result, nil
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
