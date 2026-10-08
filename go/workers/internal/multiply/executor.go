package multiply

// Durable execution ported from
// crates/loyal-fleet-worker/src/multiply/executor.rs. The ed25519 delegate
// key material lives only inside Executor; every other component plans,
// builds, or observes but can never sign.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"io"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gagliardetto/solana-go"
)

const (
	mainnetGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	computeUnitLimit   = uint32(400_000)
	maxTransactionFee  = uint64(20_000)
	solanaPacketBytes  = 1232
)

// BlockhashAndHeight is the fetched blockhash binding.
type BlockhashAndHeight struct {
	RecentBlockhash      string
	LastValidBlockHeight uint64
	ContextSlot          uint64
}

// SimulationOutcome is the bounded simulateTransaction result.
type SimulationOutcome struct {
	Err           *string
	Logs          []string
	UnitsConsumed uint64
}

// SignatureObservation is the bounded getSignatureStatuses result; nil
// outcome means the RPC has not seen the signature.
type SignatureObservation struct {
	Slot              int64
	ConfirmationState string // "processed" | "confirmed" | "finalized" | ""
	Err               *string
}

// RPCSurface is the consumer-defined Solana surface the executor needs.
// LiveRPCSurface is the production adapter; tests bind doubles. It is
// deliberately narrow: no generic transaction submission, no account writes.
type RPCSurface interface {
	GenesisHash(ctx context.Context) (string, error)
	LatestBlockhash(ctx context.Context) (*BlockhashAndHeight, error)
	// AccountAtConfirmed reads one raw account (nil data when absent) with
	// its context slot, used for the exact policy binding checks.
	AccountAtConfirmed(ctx context.Context, key solana.PublicKey) ([]byte, uint64, error)
	SimulateTransaction(ctx context.Context, wire []byte, minContextSlot uint64) (*SimulationOutcome, error)
	SendRawTransaction(ctx context.Context, wire []byte) (string, error)
	SignatureStatus(ctx context.Context, signature string) (*SignatureObservation, error)
	BlockHeight(ctx context.Context) (uint64, error)
	FeeForMessage(context.Context, []byte) (uint64, error)
	LookupTables(context.Context, []solana.PublicKey) (map[solana.PublicKey]solana.PublicKeySlice, error)
}

// LiveRPCSurface is the production JSON-RPC adapter (id-correlated JSON-RPC
// 2.0 over HTTP), bounded like the reviewed read-only clients.
type LiveRPCSurface struct {
	URL    string
	HTTP   *http.Client
	nextID atomic.Int64
}

// NewLiveRPCSurface bounds the production client.
func NewLiveRPCSurface(url string) *LiveRPCSurface {
	return &LiveRPCSurface{URL: url, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type rpcResponse struct {
	ID      int64           `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *LiveRPCSurface) call(ctx context.Context, method string, params []any, output any) error {
	requestID := c.nextID.Add(1)
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": requestID, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<22)+1))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("rpc %s returned status %d", method, response.StatusCode)
	}
	if len(body) > 1<<22 {
		return errors.New("RPC response exceeded size limit")
	}
	var parsed rpcResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("rpc %s response: %w", method, err)
	}
	if parsed.ID != requestID || parsed.JSONRPC != "2.0" {
		return errors.New("RPC response identity mismatch")
	}
	if parsed.Error != nil {
		return fmt.Errorf("rpc %s error %d: %s", method, parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Result) == 0 || bytes.Equal(parsed.Result, []byte("null")) {
		return errors.New("RPC response omitted result")
	}
	if output != nil {
		return json.Unmarshal(parsed.Result, output)
	}
	return nil
}

// GenesisHash implements RPCSurface.
func (c *LiveRPCSurface) GenesisHash(ctx context.Context) (string, error) {
	var genesis string
	if err := c.call(ctx, "getGenesisHash", nil, &genesis); err != nil {
		return "", err
	}
	return genesis, nil
}

// LatestBlockhash implements RPCSurface.
func (c *LiveRPCSurface) LatestBlockhash(ctx context.Context) (*BlockhashAndHeight, error) {
	var raw struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getLatestBlockhash",
		[]any{map[string]string{"commitment": "confirmed"}}, &raw); err != nil {
		return nil, err
	}
	if raw.Value.Blockhash == "" || raw.Value.LastValidBlockHeight == 0 || raw.Context.Slot == 0 {
		return nil, errors.New("rpc returned an incomplete blockhash")
	}
	return &BlockhashAndHeight{
		RecentBlockhash:      raw.Value.Blockhash,
		LastValidBlockHeight: raw.Value.LastValidBlockHeight,
		ContextSlot:          raw.Context.Slot,
	}, nil
}

// AccountAtConfirmed implements RPCSurface.
func (c *LiveRPCSurface) AccountAtConfirmed(ctx context.Context, key solana.PublicKey) ([]byte, uint64, error) {
	var raw struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value *struct {
			Data       []interface{} `json:"data"`
			Owner      string        `json:"owner"`
			Executable bool          `json:"executable"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getAccountInfo",
		[]any{key.String(), map[string]any{"encoding": "base64", "commitment": "confirmed"}}, &raw); err != nil {
		return nil, 0, err
	}
	if raw.Value == nil || len(raw.Value.Data) != 2 {
		return nil, raw.Context.Slot, nil
	}
	if raw.Context.Slot == 0 || raw.Value.Executable || raw.Value.Owner != SquadsProgram {
		return nil, 0, errors.New("policy account has invalid owner or context")
	}
	encoded, ok := raw.Value.Data[0].(string)
	if !ok || raw.Value.Data[1] != "base64" {
		return nil, 0, errors.New("account encoding is invalid")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, 0, fmt.Errorf("account data is not base64: %w", err)
	}
	return data, raw.Context.Slot, nil
}

// BlockHeight implements RPCSurface.
func (c *LiveRPCSurface) BlockHeight(ctx context.Context) (uint64, error) {
	var height uint64
	if err := c.call(ctx, "getBlockHeight", []any{map[string]string{"commitment": "confirmed"}}, &height); err != nil {
		return 0, err
	}
	return height, nil
}

type simulatedTransactionValue struct {
	Err   *json.RawMessage `json:"err"`
	Logs  []string         `json:"logs"`
	Units *uint64          `json:"unitsConsumed"`
}

// SimulateTransaction implements RPCSurface with sigVerify=true and
// replaceRecentBlockhash=false, exactly as the Rust executor simulates.
func (c *LiveRPCSurface) SimulateTransaction(ctx context.Context, wire []byte, minContextSlot uint64) (*SimulationOutcome, error) {
	options := map[string]any{
		"commitment":             "confirmed",
		"sigVerify":              true,
		"replaceRecentBlockhash": false,
		"encoding":               "base64",
	}
	if minContextSlot > 0 {
		options["minContextSlot"] = minContextSlot
	}
	var raw struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value *simulatedTransactionValue `json:"value"`
	}
	if err := c.call(ctx, "simulateTransaction",
		[]any{base64.StdEncoding.EncodeToString(wire), options}, &raw); err != nil {
		return nil, err
	}
	if raw.Value == nil || raw.Context.Slot == 0 || raw.Context.Slot < minContextSlot {
		return nil, errors.New("simulation context is missing or stale")
	}
	outcome := &SimulationOutcome{Logs: raw.Value.Logs}
	if raw.Value.Units != nil {
		outcome.UnitsConsumed = *raw.Value.Units
	}
	if raw.Value.Err != nil {
		message := string(*raw.Value.Err)
		outcome.Err = &message
	}
	return outcome, nil
}

// SendRawTransaction implements RPCSurface with skipPreflight=true and
// maxRetries=0: one shot, no client-side retry of an ambiguous send.
func (c *LiveRPCSurface) SendRawTransaction(ctx context.Context, wire []byte) (string, error) {
	var signature string
	if err := c.call(ctx, "sendTransaction", []any{
		base64.StdEncoding.EncodeToString(wire),
		map[string]any{"encoding": "base64", "skipPreflight": true, "maxRetries": 0},
	}, &signature); err != nil {
		return "", err
	}
	if _, err := solana.SignatureFromBase58(signature); err != nil {
		return "", fmt.Errorf("rpc returned a malformed signature: %w", err)
	}
	return signature, nil
}

// SignatureStatus implements RPCSurface.
func (c *LiveRPCSurface) SignatureStatus(ctx context.Context, signature string) (*SignatureObservation, error) {
	var raw struct {
		Value []struct {
			Slot               int64            `json:"slot"`
			ConfirmationStatus string           `json:"confirmationStatus"`
			Err                *json.RawMessage `json:"err"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getSignatureStatuses",
		[]any{[]string{signature}, map[string]any{"searchTransactionHistory": true}}, &raw); err != nil {
		return nil, err
	}
	if len(raw.Value) != 1 {
		return nil, errors.New("RPC signature result count mismatch")
	}
	if raw.Value[0].Slot == 0 {
		return nil, nil
	}
	entry := raw.Value[0]
	outcome := &SignatureObservation{Slot: entry.Slot, ConfirmationState: entry.ConfirmationStatus}
	if entry.Err != nil {
		message := string(*entry.Err)
		outcome.Err = &message
	}
	return outcome, nil
}

// Executor owns the delegate signing capability and the durable send path.
type Executor struct {
	RPC      RPCSurface
	Signer   ed25519.PrivateKey // held here and nowhere else
	feePayer ed25519.PrivateKey
}

// NewExecutor validates the signer capability and pins mainnet before any
// other use, like Executor::new.
func NewExecutor(rpc RPCSurface, signer ed25519.PrivateKey) (*Executor, error) {
	return NewExecutorWithFeePayer(rpc, signer, signer)
}

// NewRecoveryExecutorContext pins the same chain identity without acquiring
// private keys. Durable signed bytes are the only submission capability.
func NewRecoveryExecutorContext(ctx context.Context, rpc RPCSurface) (*Executor, error) {
	if ctx == nil || rpc == nil {
		return nil, errors.New("recovery executor requires context and RPC")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	genesis, err := rpc.GenesisHash(probe)
	if probe.Err() != nil {
		return nil, probe.Err()
	}
	if err != nil {
		return nil, err
	}
	if genesis != mainnetGenesisHash {
		return nil, errors.New("recovery RPC is not mainnet-beta")
	}
	return &Executor{RPC: rpc}, nil
}

func NewExecutorWithFeePayer(rpc RPCSurface, feePayer, signer ed25519.PrivateKey) (*Executor, error) {
	return NewExecutorWithFeePayerContext(context.Background(), rpc, feePayer, signer)
}

// NewExecutorWithFeePayerContext pins mainnet under the engine startup context.
// The local timeout bounds preflight without discarding caller cancellation.
func NewExecutorWithFeePayerContext(ctx context.Context, rpc RPCSurface, feePayer, signer ed25519.PrivateKey) (*Executor, error) {
	if ctx == nil {
		return nil, errors.New("multiply executor requires caller context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validPrivateKey(signer) || !validPrivateKey(feePayer) {
		return nil, errors.New("multiply executor requires an ed25519 delegate key")
	}
	if rpc == nil {
		return nil, errors.New("multiply executor requires an RPC surface")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	genesis, err := rpc.GenesisHash(ctx)
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, fmt.Errorf("genesis preflight: %w", cancellation)
	}
	if err != nil {
		return nil, fmt.Errorf("genesis preflight: %w", err)
	}
	if genesis != mainnetGenesisHash {
		return nil, fmt.Errorf("rpc is not mainnet-beta (genesis %s)", genesis)
	}
	return &Executor{RPC: rpc, Signer: append(ed25519.PrivateKey(nil), signer...), feePayer: append(ed25519.PrivateKey(nil), feePayer...)}, nil
}

// Delegate returns the policy signer public key, which may differ from the fee payer.
func (e *Executor) Delegate() solana.PublicKey {
	return solana.PublicKey(e.Signer[32:])
}

// PrepareAndSign mirrors the executor's prepare+sign stage: fetch the
// blockhash, assemble the compute-budget preamble, the KLend refresh chain,
// and the single Squads policy terminal, compile and sign the legacy wire,
// and return the immutable SignedOperation. Nothing is sent here.
func (e *Executor) PrepareAndSign(ctx context.Context, built *BuiltOperation, policy solana.PublicKey, accountIndex uint8, constraintIndexes []byte, minContextSlot uint64) (*SignedOperation, uint64, error) {
	if built != nil && len(built.PolicyInstructions) == 1 && built.PolicyInstructions[0].ProgramID == mustKey(TokenProgram) {
		return nil, 0, errors.New("delegate cannot sign wallet-owned token claims")
	}
	if built == nil || len(built.PolicyInstructions) != 1 {
		return nil, 0, errors.New("policy stage must contain exactly one terminal instruction")
	}
	if len(constraintIndexes) != 1 || len(built.PolicyInstructions[0].Accounts) > 252 || len(built.PolicyInstructions[0].Data) > 65535 {
		return nil, 0, errors.New("terminal policy instruction exceeds bounds")
	}
	if len(built.LookupTables) > 4 {
		return nil, 0, errors.New("lookup table count exceeds four")
	}
	seenTables := map[solana.PublicKey]bool{}
	for _, key := range built.LookupTables {
		if seenTables[key] {
			return nil, 0, errors.New("duplicated lookup table")
		}
		seenTables[key] = true
	}
	if !validPrivateKey(e.Signer) || !validPrivateKey(e.feePayer) {
		return nil, 0, errors.New("executor lost its signing capability")
	}
	blockhash, err := e.RPC.LatestBlockhash(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch blockhash: %w", err)
	}
	slot := blockhash.ContextSlot
	if minContextSlot > slot {
		slot = minContextSlot
	}
	if built.QuoteContextSlot != nil {
		if *built.QuoteContextSlot == 0 {
			return nil, 0, errors.New("swap quote omitted its context slot")
		}
		if *built.QuoteContextSlot > slot {
			slot = *built.QuoteContextSlot
		}
	}
	blockhashKey, err := solana.HashFromBase58(blockhash.RecentBlockhash)
	if err != nil {
		return nil, 0, fmt.Errorf("blockhash: %w", err)
	}
	transactionAccounts := make([]AccountMeta, 0, 32)
	inner := CompileSquadsInnerInstruction(&transactionAccounts, built.PolicyInstructions[0])
	terminal := ExecuteProgramInteractionInstruction(policy, e.Delegate(), accountIndex, []CompiledInstruction{inner}, constraintIndexes, transactionAccounts)
	outer := append(computeBudgetPreamble(), built.PreInstructions...)
	outer = append(outer, terminal)
	instructions := make([]solana.Instruction, 0, len(outer))
	for _, ix := range outer {
		instructions = append(instructions, sdkInstruction(ix))
	}
	feePayer := solana.PublicKey(e.feePayer[32:])
	opts := []solana.TransactionOption{solana.TransactionPayer(feePayer)}
	if len(built.LookupTables) > 0 {
		tables, err := e.RPC.LookupTables(ctx, built.LookupTables)
		if err != nil {
			return nil, 0, err
		}
		if len(tables) != len(built.LookupTables) {
			return nil, 0, errors.New("lookup table result count mismatch")
		}
		for _, key := range built.LookupTables {
			if len(tables[key]) == 0 || len(tables[key]) > 256 {
				return nil, 0, errors.New("lookup table has invalid address count")
			}
		}
		opts = append(opts, solana.TransactionAddressTables(tables))
	}
	tx, err := solana.NewTransaction(instructions, blockhashKey, opts...)
	if err != nil {
		return nil, 0, err
	}
	if len(built.LookupTables) > 0 {
		tx.Message.SetVersion(solana.MessageVersionV0)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}
	fee, err := e.RPC.FeeForMessage(ctx, message)
	if err != nil {
		return nil, 0, err
	}
	if fee > maxTransactionFee {
		return nil, 0, fmt.Errorf("transaction fee %d exceeds %d", fee, maxTransactionFee)
	}
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		var private ed25519.PrivateKey
		switch key {
		case feePayer:
			private = e.feePayer
		case e.Delegate():
			private = e.Signer
		default:
			return nil
		}
		sdk := solana.PrivateKey(private)
		return &sdk
	})
	if err != nil {
		return nil, 0, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}
	if len(wire) > solanaPacketBytes {
		return nil, 0, fmt.Errorf("multiply packet is %d bytes, exceeds %d", len(wire), solanaPacketBytes)
	}
	signed, err := NewSignedOperation(wire, tx.Signatures[0].String(), blockhash.RecentBlockhash, blockhash.LastValidBlockHeight)
	return signed, slot, err
}

func validPrivateKey(key ed25519.PrivateKey) bool {
	return len(key) == ed25519.PrivateKeySize && bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]))
}

func sdkInstruction(ix Instruction) solana.Instruction {
	metas := make(solana.AccountMetaSlice, len(ix.Accounts))
	for i, meta := range ix.Accounts {
		metas[i] = &solana.AccountMeta{PublicKey: meta.PubKey, IsSigner: meta.IsSigner, IsWritable: meta.IsWritable}
	}
	return solana.NewInstruction(ix.ProgramID, metas, ix.Data)
}

func computeBudgetPreamble() []Instruction {
	ix := computebudget.NewSetComputeUnitLimitInstruction(computeUnitLimit).Build()
	data, err := ix.Data()
	if err != nil {
		panic(err)
	}
	return []Instruction{{ProgramID: ix.ProgramID(), Data: data}}
}

// PolicyEvidence mirrors executor::PolicyEvidence.
type PolicyEvidence struct {
	Account           solana.PublicKey
	DataSHA256        string
	ConstraintIndexes []byte
}

// EnsureExactPolicy is the Go port of ensure_exact_policy: the delegate may
// only fire a terminal instruction through an installed canonical policy
// whose payload still equals the Earn MAX contract and that delegates to
// exactly this signer.
func (e *Executor) EnsureExactPolicy(ctx context.Context, topology *EarnMaxTopology, plan *ActionPlan, built *BuiltOperation) (*PolicyEvidence, error) {
	if plan == nil || plan.Action == ActionClaim {
		return nil, errors.New("claim belongs to the app root-wallet owner; delegate execution is forbidden")
	}
	if !plan.StrategyKey.Valid() {
		return nil, errors.New("delegate operation has no strategy")
	}
	config, err := topology.Strategy(plan.StrategyKey)
	if err != nil {
		return nil, err
	}
	policy, ok := config.PolicyForAction(plan.Action)
	if !ok {
		return nil, errors.New("strategy has no policy for action")
	}
	family, err := FamilyForAction(plan.Action)
	if err != nil {
		return nil, err
	}
	data, _, err := e.RPC.AccountAtConfirmed(ctx, policy.Account)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("exact ProgramInteraction policy is absent")
	}
	expected, err := CanonicalConstraints(topology, family)
	if err != nil {
		return nil, err
	}
	matches, err := CurrentPolicyMatches(data, policy, e.Delegate(), expected, topology.VaultIndex)
	if err != nil {
		return nil, err
	}
	if !matches {
		return nil, fmt.Errorf("stable production policy %s is not installed", policy.Account)
	}
	constraintIndexes, err := ConstraintIndexes(config, plan.Action, built.PolicyInstructions)
	if err != nil {
		return nil, err
	}
	return &PolicyEvidence{
		Account:           policy.Account,
		DataSHA256:        PolicyDataHash(data),
		ConstraintIndexes: constraintIndexes,
	}, nil
}

// PersistedTransaction re-verifies the stored wire before any recovery send:
// the SHA-256 must match the persisted column, the first signature must equal
// the persisted transaction signature, and the message blockhash must equal
// the persisted recent blockhash. Any drift refuses the send.
func decodeVerifiedTransaction(wire []byte) (*solana.Transaction, error) {
	if len(wire) == 0 || len(wire) > solanaPacketBytes {
		return nil, errors.New("signed wire size is invalid")
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return nil, err
	}
	if len(tx.Signatures) < 1 || len(tx.Signatures) > 2 || int(tx.Message.Header.NumRequiredSignatures) != len(tx.Signatures) {
		return nil, errors.New("invalid signing authority count")
	}
	canonical, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(wire, canonical) {
		return nil, errors.New("wire is not canonical or contains trailing bytes")
	}
	if err := tx.VerifySignatures(); err != nil {
		return nil, err
	}
	return tx, nil
}

func PersistedTransaction(operation *MultiplyOperation) ([]byte, error) {
	if operation == nil {
		return nil, errors.New("operation is missing")
	}
	wire := operation.SignedWire
	digest := sha256.Sum256(wire)
	if operation.SignedWireSHA256 == nil || hexEncode(digest[:]) != *operation.SignedWireSHA256 {
		return nil, errors.New("persisted wire hash drifted")
	}
	tx, err := decodeVerifiedTransaction(wire)
	if err != nil {
		return nil, err
	}
	if operation.TransactionSignature == nil || tx.Signatures[0].String() != *operation.TransactionSignature {
		return nil, errors.New("persisted signature drifted")
	}
	if operation.RecentBlockhash == nil || tx.Message.RecentBlockhash.String() != *operation.RecentBlockhash {
		return nil, errors.New("persisted blockhash drifted")
	}
	hash, err := MessageSHA256(wire)
	if err != nil {
		return nil, err
	}
	if operation.MessageSHA256 == nil || hash != *operation.MessageSHA256 {
		return nil, errors.New("persisted message hash drifted")
	}
	return append([]byte(nil), wire...), nil
}

func MessageSHA256(wire []byte) (string, error) {
	tx, err := decodeVerifiedTransaction(wire)
	if err != nil {
		return "", err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(message)
	return hexEncode(digest[:]), nil
}

// Simulate runs the persisted wire under sigVerify; failure before broadcast
// keeps the operation in prepared.
func (e *Executor) Simulate(ctx context.Context, signed *SignedOperation, minContextSlot uint64) (*SimulationOutcome, error) {
	return e.RPC.SimulateTransaction(ctx, signed.Wire, minContextSlot)
}

// VerifyExpectedEffects compares confirmed balances against immutable persisted
// anchors, following Rust's exact-input, bounded-swap and repay-all rules.
// The historical before parameter is ignored; a fresh read cannot establish
// what the account held before an already broadcast operation.
func VerifyExpectedEffects(effects *ExpectedEffects, action MultiplyAction, _, after *ObservedRoute, topology *EarnMaxTopology) error {
	if effects == nil || after == nil || topology == nil {
		return errors.New("effect evidence is missing")
	}
	seen := map[string]bool{}
	for _, delta := range effects.TokenDeltas {
		identity := delta.Account + ":" + delta.Mint
		if seen[identity] {
			return errors.New("duplicated token effect")
		}
		seen[identity] = true
		var anchor *TokenAmountBefore
		for i := range effects.TokenAmountsBefore {
			value := &effects.TokenAmountsBefore[i]
			if value.Account == delta.Account && value.Mint == delta.Mint {
				if anchor != nil {
					return errors.New("duplicate persisted token anchor")
				}
				anchor = value
			}
		}
		if anchor == nil {
			return errors.New("operation omitted its pre-transaction token amount")
		}
		post, ok := observedTokenAmount(after, delta.Account, delta.Mint)
		if !ok {
			return errors.New("post-state omitted expected custody identity")
		}
		if delta.RawDelta < 0 {
			if post > anchor.AmountRaw {
				return errors.New("source custody increased")
			}
			spent := anchor.AmountRaw - post
			expected := uint64(-(delta.RawDelta + 1)) + 1
			valid := spent == expected
			switch action {
			case ActionSwapCollateralToDebt:
				valid = spent > 0 && spent <= expected
			case ActionRepayDebt:
				if effects.ObligationBefore != nil && expected >= effects.ObligationBefore.DebtRaw {
					valid = spent > 0 && spent <= anchor.AmountRaw
				}
			}
			if !valid {
				return errors.New("confirmed source custody delta violated exact input or maximum")
			}
		} else if delta.RawDelta > 0 {
			if post < anchor.AmountRaw || post-anchor.AmountRaw < uint64(delta.RawDelta) {
				return errors.New("confirmed destination custody delta missed minimum")
			}
		}
	}
	if effects.ObligationDelta != nil {
		before := effects.ObligationBefore
		delta := effects.ObligationDelta
		if before == nil || before.Obligation != delta.Obligation {
			return errors.New("obligation anchor is missing or has different identity")
		}
		obligation, err := solana.PublicKeyFromBase58(delta.Obligation)
		if err != nil {
			return err
		}
		post := positionForObligation(after, obligation, topology)
		if post == nil {
			return errors.New("post-state omitted obligation")
		}
		direction := func(delta int64, pre, post uint64) bool {
			return delta == 0 || (delta > 0 && post > pre) || (delta < 0 && post < pre)
		}
		if !direction(delta.CollateralRawDelta, before.CollateralRaw, post.CollateralDepositedRaw) || !direction(delta.DebtRawDelta, before.DebtRaw, post.DebtRaw) {
			return errors.New("confirmed obligation movement had wrong direction")
		}
		debtMagnitude := uint64(delta.DebtRawDelta)
		if delta.DebtRawDelta < 0 {
			debtMagnitude = uint64(-(delta.DebtRawDelta + 1)) + 1
		}
		if action == ActionRepayDebt && debtMagnitude >= before.DebtRaw && post.DebtRaw != 0 {
			return errors.New("repay-all left confirmed obligation debt")
		}
	}
	return nil
}

func observedTokenAmount(observed *ObservedRoute, account, mint string) (uint64, bool) {
	if observed == nil {
		return 0, false
	}
	balances := []TokenBalance{observed.Claim}
	for _, v := range observed.CollateralCustodies {
		balances = append(balances, v.Balance)
	}
	for _, v := range observed.DebtCustodies {
		balances = append(balances, v.Balance)
	}
	balances = append(balances, observed.ExternalCustody...)
	var amount uint64
	found := false
	for _, v := range balances {
		if v.Account == account && v.Mint == mint {
			if found && amount != v.AmountRaw {
				return 0, false
			}
			amount = v.AmountRaw
			found = true
		}
	}
	return amount, found
}

func positionForObligation(observed *ObservedRoute, obligation solana.PublicKey, topology *EarnMaxTopology) *StrategyObservation {
	for _, post := range observed.Strategies {
		if post == nil {
			continue
		}
		config, err := topology.Strategy(post.StrategyKey)
		if err == nil && config.Obligation == obligation {
			return post
		}
	}
	return nil
}

func hexEncode(value []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(value)*2)
	for _, b := range value {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}

func (c *LiveRPCSurface) FeeForMessage(ctx context.Context, message []byte) (uint64, error) {
	var raw struct {
		Value *uint64 `json:"value"`
	}
	if err := c.call(ctx, "getFeeForMessage", []any{base64.StdEncoding.EncodeToString(message), map[string]string{"commitment": "confirmed"}}, &raw); err != nil {
		return 0, err
	}
	if raw.Value == nil {
		return 0, errors.New("RPC omitted transaction fee")
	}
	return *raw.Value, nil
}

func (c *LiveRPCSurface) LookupTables(ctx context.Context, keys []solana.PublicKey) (map[solana.PublicKey]solana.PublicKeySlice, error) {
	if len(keys) == 0 || len(keys) > 4 {
		return nil, errors.New("lookup table count is invalid")
	}
	names := make([]string, len(keys))
	seen := map[solana.PublicKey]bool{}
	for i, key := range keys {
		if seen[key] {
			return nil, errors.New("duplicate lookup table")
		}
		seen[key] = true
		names[i] = key.String()
	}
	var raw struct {
		Value []*struct {
			Owner      string            `json:"owner"`
			Executable bool              `json:"executable"`
			Data       []json.RawMessage `json:"data"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getMultipleAccounts", []any{names, map[string]string{"encoding": "base64", "commitment": "confirmed"}}, &raw); err != nil {
		return nil, err
	}
	if len(raw.Value) != len(keys) {
		return nil, errors.New("RPC lookup table result count mismatch")
	}
	tables := make(map[solana.PublicKey]solana.PublicKeySlice, len(keys))
	for i, value := range raw.Value {
		if value == nil || value.Owner != solana.AddressLookupTableProgramID.String() || value.Executable || len(value.Data) != 2 {
			return nil, errors.New("lookup table is absent or invalid")
		}
		var encoded, encoding string
		if err := json.Unmarshal(value.Data[0], &encoded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(value.Data[1], &encoding); err != nil {
			return nil, err
		}
		if encoding != "base64" {
			return nil, errors.New("lookup table encoding is invalid")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		// Solana lookup-table state has 56 bytes of metadata, then public keys.
		if len(data) < 56 || (len(data)-56)%32 != 0 || binary.LittleEndian.Uint32(data[:4]) != 1 || binary.LittleEndian.Uint64(data[4:12]) != math.MaxUint64 || data[21] > 1 {
			return nil, errors.New("lookup table state is inactive or malformed")
		}
		count := (len(data) - 56) / 32
		if count == 0 || count > 256 || int(data[20]) > count {
			return nil, errors.New("lookup table address count is invalid")
		}
		addresses := make(solana.PublicKeySlice, count)
		for j := range addresses {
			copy(addresses[j][:], data[56+j*32:56+(j+1)*32])
		}
		tables[keys[i]] = addresses
	}
	return tables, nil
}
