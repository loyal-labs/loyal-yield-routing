package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Backyard reads, simulates and sends through the shared chain client, which
// never retries. These functions turn what the cluster says into the family's
// evidence types. An endpoint that could not answer (chain.ErrUnavailable: a
// transport failure, a rate limit, a node behind the asked slot) ends the tick
// as an unavailable observation, and the next tick reads again.

// unavailable marks an endpoint that could not answer as an unavailable
// observation; every other error passes through unchanged.
func unavailable(err error) error {
	if errors.Is(err, chain.ErrUnavailable) {
		return confirmedObservationUnavailable(err)
	}
	return err
}

type ConfirmedAccount struct {
	// Empty source denotes an ordinary confirmed read. A valuation capture
	// tags every account with its bank slot; only closed reserve refreshes
	// may mutate account data inside that capture.
	ValuationSource string `json:",omitempty"`
	ValuationSlot   int64  `json:",omitempty"`
	Address         string
	Owner           string
	Lamports        uint64
	Data            []byte
	Executable      bool
}

// chainAccount is the confirmed account at address for the shared program
// decoders; any other account is absent.
func chainAccount(a ConfirmedAccount, address string) *chain.Account {
	key, keyErr := solana.PublicKeyFromBase58(a.Address)
	owner, ownerErr := solana.PublicKeyFromBase58(a.Owner)
	if a.Address != address || keyErr != nil || ownerErr != nil {
		return nil
	}
	return &chain.Account{Key: key, Owner: owner, Lamports: a.Lamports, Data: a.Data, Executable: a.Executable}
}

func confirmedAccount(address string, account *chain.Account) ConfirmedAccount {
	return ConfirmedAccount{Address: address, Owner: account.Owner.String(), Lamports: account.Lamports, Data: account.Data, Executable: account.Executable}
}

func hashConfirmedAccounts(accounts []ConfirmedAccount) string {
	hash := sha256.New()
	for _, account := range accounts {
		hash.Write([]byte(account.Address))
		hash.Write([]byte{0})
		hash.Write([]byte(account.Owner))
		hash.Write([]byte{0})
		var lamports [8]byte
		for index := range lamports {
			lamports[index] = byte(account.Lamports >> (8 * index))
		}
		hash.Write(lamports[:])
		hash.Write([]byte{0})
		hash.Write(account.Data)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func publicKeys(addresses []string) ([]solana.PublicKey, error) {
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return nil, fmt.Errorf("account address %q: %w", address, err)
		}
		keys[i] = key
	}
	return keys, nil
}

func confirmedSlot(ctx context.Context, c *chain.Client) (int64, error) {
	slot, err := c.Slot(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, confirmedObservationUnavailable(err)
	}
	return int64(slot), nil
}

func finalizedSlot(ctx context.Context, c *chain.Client) (int64, error) {
	slot, err := c.Slot(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return 0, confirmedObservationUnavailable(err)
	}
	return int64(slot), nil
}

// readAccounts reads one account set at commitment, all at one slot no older
// than minSlot; that slot is the only one a Snapshot built from it may use.
// Only an optional address may be absent: it comes back with its address
// alone.
func readAccounts(ctx context.Context, c *chain.Client, commitment rpc.CommitmentType, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, error) {
	if len(addresses) == 0 || minSlot <= 0 {
		return 0, nil, fmt.Errorf("account addresses and minContextSlot are required")
	}
	keys, err := publicKeys(addresses)
	if err != nil {
		return 0, nil, err
	}
	slot, read, err := c.Accounts(ctx, keys, commitment, uint64(minSlot))
	if err != nil {
		return 0, nil, confirmedObservationUnavailable(err)
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, account := range read {
		switch {
		case account != nil:
			accounts[i] = confirmedAccount(addresses[i], account)
		case slices.Contains(optional, addresses[i]):
			accounts[i] = ConfirmedAccount{Address: addresses[i]}
		default:
			return 0, nil, fmt.Errorf("required account %s is absent", addresses[i])
		}
	}
	return int64(slot), accounts, nil
}

// confirmedAccounts is readAccounts at confirmed.
func confirmedAccounts(ctx context.Context, c *chain.Client, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, error) {
	return readAccounts(ctx, c, rpc.CommitmentConfirmed, addresses, minSlot, optional...)
}

// finalizedAccounts is readAccounts at finalized.
func finalizedAccounts(ctx context.Context, c *chain.Client, addresses []string, minSlot int64, optional ...string) (int64, []ConfirmedAccount, error) {
	return readAccounts(ctx, c, rpc.CommitmentFinalized, addresses, minSlot, optional...)
}

// confirmedReader is confirmedAccounts for c, for observers that take a reader.
func confirmedReader(c *chain.Client) func(context.Context, []string, int64) (int64, []ConfirmedAccount, error) {
	return func(ctx context.Context, addresses []string, minSlot int64) (int64, []ConfirmedAccount, error) {
		return confirmedAccounts(ctx, c, addresses, minSlot)
	}
}

type LatestBlockhash struct {
	Blockhash            string
	LastValidBlockHeight int64
}

func latestBlockhash(ctx context.Context, c *chain.Client) (LatestBlockhash, error) {
	hash, lastValid, _, err := c.Blockhash(ctx, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return LatestBlockhash{}, confirmedObservationUnavailable(err)
	}
	return LatestBlockhash{Blockhash: hash.String(), LastValidBlockHeight: int64(lastValid)}, nil
}

type MessageFeeObservation struct {
	MessageSHA256 string `json:"messageSha256"`
	Slot          int64  `json:"slot"`
	Lamports      uint64 `json:"lamports"`
}

// observeMessageFee asks the chain to price the exact unsigned message,
// including its compute-budget instructions. Null (expired blockhash) or any
// failed read is a HOLD, never a zero-fee assumption.
func observeMessageFee(ctx context.Context, c *chain.Client, message []byte, minimumSlot int64) (MessageFeeObservation, error) {
	if _, err := checkedUnsignedMessage(message); err != nil {
		return MessageFeeObservation{}, err
	}
	if minimumSlot <= 0 {
		return MessageFeeObservation{}, budgetHold("invalid_fee_observation_slot")
	}
	fee, slot, err := c.Fee(ctx, message, rpc.CommitmentConfirmed, uint64(minimumSlot))
	if err != nil || fee == 0 {
		return MessageFeeObservation{}, budgetHold("network_fee_unavailable")
	}
	return MessageFeeObservation{MessageSHA256: sha256Bytes(message), Slot: int64(slot), Lamports: fee}, nil
}

// simulateSigned verifies the exact signed bytes that would later be
// persisted, with signature checks and the wire's own blockhash.
func simulateSigned(ctx context.Context, c *chain.Client, signedWire []byte) (SimulationResult, error) {
	if len(signedWire) == 0 {
		return SimulationResult{}, fmt.Errorf("signed transaction wire is required")
	}
	simulated, err := c.Simulate(ctx, signedWire, rpc.SimulateTransactionOpts{SigVerify: true, Commitment: rpc.CommitmentConfirmed})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		raw, _ := json.Marshal(failure.Err)
		slot := int64(failure.Slot)
		if squadsSpendingLimitExceeded(raw, failure.Logs) {
			return SimulationResult{}, &SquadsSpendingLimitError{Slot: slot, Err: raw}
		}
		logTail := failure.Logs
		if len(logTail) > 8 {
			logTail = logTail[len(logTail)-8:]
		}
		err := fmt.Errorf("signed transaction simulation failed: slot=%d err=%s log_tail=%q", slot, string(raw), strings.Join(logTail, " | "))
		if code, ok := decodeInstructionErrorCustom(raw); ok && code == adaptorErrorReportSlot && failingProgramFromLogs(failure.Logs) == bridgeAdaptorProgram {
			return SimulationResult{}, &ReportSlotSimulationError{Slot: slot, err: err}
		}
		return SimulationResult{}, err
	}
	if err != nil {
		return SimulationResult{}, unavailable(err)
	}
	return SimulationResult{Slot: int64(simulated.Slot), UnitsConsumed: simulated.Units, Logs: simulated.Logs}, nil
}

// ReportSlotSimulationError is a simulation the NAV adaptor refused with
// ReportSlot (Custom 9). Slot is the simulation's confirmed slot.
type ReportSlotSimulationError struct {
	Slot int64
	err  error
}

func (e *ReportSlotSimulationError) Error() string { return e.err.Error() }

// SquadsSpendingLimitError marks a simulation the pinned Squads program
// refused with its spending-limit-exceeded custom error (6073). It is a
// specific, non-retryable pre-broadcast refusal, never a generic simulation
// failure.
type SquadsSpendingLimitError struct {
	Slot int64
	Err  json.RawMessage
}

func (e *SquadsSpendingLimitError) Error() string {
	return fmt.Sprintf("squads spending limit exceeded: slot=%d err=%s", e.Slot, string(e.Err))
}

// signatureStatus reads one signature with full history. A processed-only
// result can still be forked away, so it proves nothing about failure or
// success and may never drive a terminal transition. Only a settled
// observation reports Failed; Confirmed additionally excludes a settled
// on-chain error.
func signatureStatus(ctx context.Context, c *chain.Client, signature string) (SignatureObservation, error) {
	if signature == "" {
		return SignatureObservation{}, fmt.Errorf("transaction signature is required")
	}
	state, err := c.SignatureState(ctx, signature)
	if err != nil {
		return SignatureObservation{}, unavailable(err)
	}
	if !state.Found {
		return SignatureObservation{}, nil
	}
	settled := state.Slot > 0 && state.Commitment >= chain.Confirmed
	failed := settled && state.Err != ""
	return SignatureObservation{
		Found: true, Confirmed: settled && !failed,
		Finalized: settled && state.Commitment == chain.Finalized,
		Settled:   settled, ProcessedOnly: !settled,
		ConfirmationSlot: int64(state.Slot), Failed: failed,
	}, nil
}

// finalizedReceipt reads the exact signature's transaction at commitment. A
// receipt the cluster does not have yet, or an endpoint that could not answer,
// is an unavailable observation: the next tick reads again.
func finalizedReceipt(ctx context.Context, c *chain.Client, signature string) (chain.Receipt, error) {
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return chain.Receipt{}, fmt.Errorf("transaction signature: %w", err)
	}
	receipt, err := c.Receipt(ctx, sig, rpc.CommitmentFinalized)
	if errors.Is(err, chain.ErrNotFound) {
		return chain.Receipt{}, confirmedObservationUnavailable(err)
	}
	if err != nil {
		return chain.Receipt{}, unavailable(err)
	}
	return receipt, nil
}

type TransactionTokenBalance struct {
	Address, OwnerProgram, Mint, Authority string
	Raw                                    uint64
}

type ProgramReturnData struct {
	ProgramID  string
	DataBase64 string
}

type ConfirmedTransactionEvidence struct {
	Finalized         bool
	Signature         string
	Slot              int64
	PreTokenBalances  []TransactionTokenBalance
	PostTokenBalances []TransactionTokenBalance
	ReturnData        *ProgramReturnData
	Logs              []string
	Initialization    *KaminoInitializationReceipt
}

// finalizedTransaction reads the immutable finalized receipt for the exact
// persisted signature. Reconciliation must use these transaction-scoped
// pre/post token balances rather than a later account read that can include
// unrelated user deposits or claims.
func finalizedTransaction(ctx context.Context, c *chain.Client, signature string) (ConfirmedTransactionEvidence, error) {
	receipt, err := finalizedReceipt(ctx, c, signature)
	if err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	if receipt.Err != nil || len(receipt.Keys) == 0 {
		return ConfirmedTransactionEvidence{}, fmt.Errorf("confirmed transaction receipt is invalid")
	}
	pre, err := transactionTokenBalances(receipt.Pre)
	if err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	post, err := transactionTokenBalances(receipt.Post)
	if err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	evidence := ConfirmedTransactionEvidence{
		Finalized: true, Signature: signature, Slot: int64(receipt.Slot),
		PreTokenBalances: pre, PostTokenBalances: post, Logs: receipt.Logs,
	}
	if !receipt.ReturnProgram.IsZero() {
		evidence.ReturnData = &ProgramReturnData{ProgramID: receipt.ReturnProgram.String(), DataBase64: base64.StdEncoding.EncodeToString(receipt.ReturnData)}
	}
	return evidence, nil
}

func transactionTokenBalances(balances map[solana.PublicKey]chain.TokenBalance) ([]TransactionTokenBalance, error) {
	out := make([]TransactionTokenBalance, 0, len(balances))
	for address, balance := range balances {
		program := balance.Program.String()
		if program != classicTokenProgram && program != token2022Program {
			return nil, fmt.Errorf("transaction token-balance identity is incomplete")
		}
		out = append(out, TransactionTokenBalance{Address: address.String(), OwnerProgram: program, Mint: balance.Mint.String(), Authority: balance.Owner.String(), Raw: balance.Amount})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// failedTransactionEvidence reads the immutable failure receipt for the exact
// persisted signature, keeping the chain error and the failing program's log
// lines. Only a finalized receipt is acceptable: a failure can only be
// classified once it provably cannot be forked away.
func failedTransactionEvidence(ctx context.Context, c *chain.Client, signature string) (ConfirmedFailureEvidence, error) {
	receipt, err := finalizedReceipt(ctx, c, signature)
	if err != nil {
		return ConfirmedFailureEvidence{}, err
	}
	if receipt.Err == nil {
		return ConfirmedFailureEvidence{}, fmt.Errorf("failed transaction receipt is unavailable")
	}
	raw, err := json.Marshal(receipt.Err)
	if err != nil {
		return ConfirmedFailureEvidence{}, fmt.Errorf("failed transaction error: %w", err)
	}
	return ConfirmedFailureEvidence{Slot: int64(receipt.Slot), Err: raw, Logs: receipt.Logs}, nil
}
