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

type accountBatch struct {
	Slot     int64
	Accounts []ConfirmedAccount
}

func readAccounts(ctx context.Context, c *chain.Client, commitment rpc.CommitmentType, addresses []string, minSlot int64, optional ...string) (accountBatch, error) {
	if len(addresses) == 0 {
		return accountBatch{}, fmt.Errorf("account addresses are required")
	}
	keys, err := publicKeys(addresses)
	if err != nil {
		return accountBatch{}, err
	}
	slot, read, err := c.Accounts(ctx, keys, commitment, uint64(minSlot))
	if err != nil {
		return accountBatch{}, confirmedObservationUnavailable(err)
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for i, account := range read {
		switch {
		case account != nil:
			accounts[i] = confirmedAccount(addresses[i], account)
		case slices.Contains(optional, addresses[i]):
			accounts[i] = ConfirmedAccount{Address: addresses[i]}
		default:
			return accountBatch{}, fmt.Errorf("required account %s is absent", addresses[i])
		}
	}
	return accountBatch{Slot: int64(slot), Accounts: accounts}, nil
}

func finalizedAccounts(ctx context.Context, c *chain.Client, addresses []string, minSlot int64, optional ...string) (accountBatch, error) {
	return readAccounts(ctx, c, rpc.CommitmentFinalized, addresses, minSlot, optional...)
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

func simulateSigned(ctx context.Context, c *chain.Client, signedWire []byte) (SimulationResult, error) {
	if len(signedWire) == 0 {
		return SimulationResult{}, fmt.Errorf("signed transaction wire is required")
	}
	simulated, err := c.Simulate(ctx, signedWire, rpc.SimulateTransactionOpts{SigVerify: true, Commitment: rpc.CommitmentConfirmed})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		raw, _ := json.Marshal(failure.Err)
		slot := int64(failure.Slot)
		logTail := failure.Logs
		if len(logTail) > 8 {
			logTail = logTail[len(logTail)-8:]
		}
		return SimulationResult{}, &simulationFailure{raw: raw, logs: failure.Logs,
			text: fmt.Sprintf("signed transaction simulation failed: slot=%d err=%s log_tail=%q", slot, string(raw), strings.Join(logTail, " | "))}
	}
	if err != nil {
		return SimulationResult{}, unavailable(err)
	}
	return SimulationResult{Slot: int64(simulated.Slot), UnitsConsumed: simulated.Units, Logs: simulated.Logs}, nil
}

type signatureCommitment uint8

const (
	signatureAbsent signatureCommitment = iota
	signatureProcessed
	signatureConfirmed
	signatureFinalized
	signatureFailed
)

func signatureStatus(ctx context.Context, c *chain.Client, signature string) (signatureCommitment, error) {
	if signature == "" {
		return signatureAbsent, fmt.Errorf("transaction signature is required")
	}
	state, err := c.SignatureState(ctx, signature)
	if err != nil {
		return signatureAbsent, unavailable(err)
	}
	switch {
	case !state.Found:
		return signatureAbsent, nil
	case state.Slot <= 0 || state.Commitment < chain.Confirmed:
		return signatureProcessed, nil
	case state.Err != "":
		return signatureFailed, nil
	case state.Commitment == chain.Finalized:
		return signatureFinalized, nil
	}
	return signatureConfirmed, nil
}

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
	Finalized              bool
	Signature              string
	Slot                   int64
	PreTokenBalances       []TransactionTokenBalance
	PostTokenBalances      []TransactionTokenBalance
	ReturnData             *ProgramReturnData
	Logs                   []string
	Initialization         *KaminoInitializationReceipt
	foreignSweepExtraRaw   uint64
	foreignSweepCheckpoint foreignCheckpoint
}

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
