package backyardrwa

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	classicTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022Program    = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
)

type ExpectedAccountEffect struct {
	Address   string `json:"address"`
	Owner     string `json:"owner"`
	Mint      string `json:"mint"`
	Authority string `json:"authority"`
	BeforeRaw uint64 `json:"beforeRaw"`
	AfterRaw  uint64 `json:"afterRaw"`
	// MinimumAfterRaw is used only for the destination of an exact-input
	// cross-mint swap. Jupiter may deliver more than its slippage threshold, so
	// demanding equality would turn price improvement into manual recovery.
	MinimumAfterRaw *uint64 `json:"minimumAfterRaw,omitempty"`
}

type ExpectedEffects struct {
	Schema         string                       `json:"schema"`
	Kind           string                       `json:"kind,omitempty"`
	Conserved      bool                         `json:"conserved"`
	Accounts       []ExpectedAccountEffect      `json:"accounts"`
	ReturnData     *ExpectedReturnData          `json:"returnData,omitempty"`
	Repayment      *ExpectedRepayment           `json:"repayment,omitempty"`
	Deposit        *ExpectedDeposit             `json:"deposit,omitempty"`
	Initialization *KaminoInitializationRequest `json:"initialization,omitempty"`
}

// Deposits round down to whole receipts and may debit less than the requested
// liquidity. Bounds describe only conserved token movement, not minted receipts.
type ExpectedDeposit struct {
	MinimumDebitRaw uint64 `json:"minimumDebitRaw"`
	MaximumDebitRaw uint64 `json:"maximumDebitRaw"`
}

// ExpectedRepayment bounds a finite Kamino repayment request, not the debt
// remaining afterward. KLend clips its transfer to the debt owed. Accounts are
// ordered source/destination, with AfterRaw describing the maximum debit;
// reconciliation requires the same actual debit and credit within these bounds.
// Only a subsequent obligation observation can establish a full payoff.
type ExpectedRepayment struct {
	MinimumDebitRaw uint64 `json:"minimumDebitRaw"`
	MaximumDebitRaw uint64 `json:"maximumDebitRaw"`
}

type ExpectedReturnData struct {
	ProgramID  string `json:"programId"`
	DataBase64 string `json:"dataBase64"`
}

func (r Reconciliation) Validate() error {
	if r.ConfirmedSlot <= 0 || len(r.EffectsSHA256) != 64 || !r.Conserved {
		return fmt.Errorf("reconciliation postcondition failed")
	}
	return nil
}

func DecodeExpectedEffects(data []byte) (ExpectedEffects, error) {
	var envelope struct {
		Schema          string          `json:"schema"`
		ExpectedEffects json.RawMessage `json:"expectedEffects"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ExpectedEffects{}, fmt.Errorf("decode expected effects: %w", err)
	}
	if envelope.Schema == "loyal-backyard-rwa-operation-evidence/v1" {
		if len(envelope.ExpectedEffects) == 0 || string(envelope.ExpectedEffects) == "null" {
			return ExpectedEffects{}, fmt.Errorf("operation has no built expected effects")
		}
		data = envelope.ExpectedEffects
	}
	var expected ExpectedEffects
	if err := json.Unmarshal(data, &expected); err != nil {
		return ExpectedEffects{}, fmt.Errorf("decode expected effects: %w", err)
	}
	if expected.Kind == "kamino-initialize" || expected.Initialization != nil {
		if err := validateInitializationEffects(expected); err != nil {
			return ExpectedEffects{}, err
		}
		return expected, nil
	}
	if expected.Schema != "loyal-backyard-rwa-expected-effects/v1" || len(expected.Accounts) == 0 {
		return ExpectedEffects{}, fmt.Errorf("incomplete expected effects")
	}
	if !expected.Conserved && (expected.Kind != "cross-mint-swap" || len(expected.Accounts) != 2) {
		return ExpectedEffects{}, fmt.Errorf("unsupported non-conserved expected effects")
	}
	seen := make(map[string]struct{}, len(expected.Accounts))
	for _, account := range expected.Accounts {
		if account.Address == "" || (account.Owner != classicTokenProgram && account.Owner != token2022Program) ||
			account.Mint == "" || account.Authority == "" {
			return ExpectedEffects{}, fmt.Errorf("unknown expected custody")
		}
		if _, err := decodeBase58PublicKey(account.Mint); err != nil {
			return ExpectedEffects{}, fmt.Errorf("invalid expected custody mint")
		}
		if _, err := decodeBase58PublicKey(account.Authority); err != nil {
			return ExpectedEffects{}, fmt.Errorf("invalid expected custody authority")
		}
		if _, duplicate := seen[account.Address]; duplicate {
			return ExpectedEffects{}, fmt.Errorf("duplicate expected custody")
		}
		seen[account.Address] = struct{}{}
		if expected.Kind != "cross-mint-swap" && account.MinimumAfterRaw != nil {
			return ExpectedEffects{}, fmt.Errorf("minimum balance is only supported for a cross-mint swap destination")
		}
	}
	if err := validateRepaymentEffects(expected); err != nil {
		return ExpectedEffects{}, err
	}
	if expected.Kind == "cross-mint-swap" {
		if expected.Accounts[0].Mint == expected.Accounts[1].Mint || expected.Accounts[0].MinimumAfterRaw != nil ||
			expected.Accounts[1].MinimumAfterRaw == nil || *expected.Accounts[1].MinimumAfterRaw != expected.Accounts[1].AfterRaw {
			return ExpectedEffects{}, fmt.Errorf("invalid cross-mint swap postconditions")
		}
	}
	if expected.ReturnData != nil {
		data, err := base64.StdEncoding.DecodeString(expected.ReturnData.DataBase64)
		if expected.Kind != "bridge" || expected.ReturnData.ProgramID != bridgeAdaptorProgram || err != nil || len(data) != 8 {
			return ExpectedEffects{}, fmt.Errorf("invalid expected adaptor return data")
		}
	}
	return expected, nil
}

func validateRepaymentEffects(expected ExpectedEffects) error {
	if expected.Deposit != nil {
		if expected.Kind != "kamino-deposit" || expected.Repayment != nil {
			return fmt.Errorf("deposit bounds require only a Kamino deposit")
		}
		// Share conservation/finite-range validation, without changing the
		// serialized operation kind or treating a deposit as repayment.
		copy := expected
		copy.Kind, copy.Deposit = "kamino-repay", nil
		copy.Repayment = &ExpectedRepayment{expected.Deposit.MinimumDebitRaw, expected.Deposit.MaximumDebitRaw}
		return validateRepaymentEffects(copy)
	}
	if expected.Kind == "kamino-deposit" {
		return fmt.Errorf("missing finite Kamino deposit bounds")
	}
	if expected.Kind != "kamino-repay" {
		if expected.Repayment != nil {
			return fmt.Errorf("repayment bounds require a Kamino repayment")
		}
		return nil
	}
	bounds := expected.Repayment
	if bounds == nil || !expected.Conserved || len(expected.Accounts) != 2 || expected.ReturnData != nil ||
		bounds.MinimumDebitRaw == 0 || bounds.MinimumDebitRaw > bounds.MaximumDebitRaw || bounds.MaximumDebitRaw == ^uint64(0) {
		return fmt.Errorf("invalid finite Kamino repayment bounds")
	}
	source, destination := expected.Accounts[0], expected.Accounts[1]
	if source.Address == destination.Address || source.Mint != destination.Mint || source.Owner != destination.Owner ||
		source.MinimumAfterRaw != nil || destination.MinimumAfterRaw != nil ||
		source.AfterRaw > source.BeforeRaw || destination.AfterRaw < destination.BeforeRaw ||
		source.BeforeRaw-source.AfterRaw != bounds.MaximumDebitRaw || destination.AfterRaw-destination.BeforeRaw != bounds.MaximumDebitRaw {
		return fmt.Errorf("Kamino repayment bounds do not match the conserved custody graph")
	}
	return nil
}

// ReconcileConfirmedTransaction verifies effects against the immutable receipt
// for the exact persisted signature. A later account read is intentionally not
// accepted: unrelated deposits and claims can mutate the same custodies after
// this transaction confirms.
func ReconcileConfirmedTransaction(expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	if expected.Kind == "kamino-initialize" || expected.Initialization != nil {
		return reconcileKaminoInitialization(expected, receipt)
	}
	if err := validateRepaymentEffects(expected); err != nil {
		return Reconciliation{}, nil, err
	}
	if receipt.Signature == "" || receipt.Slot <= 0 {
		return Reconciliation{}, nil, fmt.Errorf("confirmed transaction identity is incomplete")
	}
	preByAddress, err := transactionBalancesByAddress(receipt.PreTokenBalances)
	if err != nil {
		return Reconciliation{}, nil, err
	}
	postByAddress, err := transactionBalancesByAddress(receipt.PostTokenBalances)
	if err != nil {
		return Reconciliation{}, nil, err
	}
	canonical := make([]string, 0, len(expected.Accounts))
	bounds := expected.Repayment
	if expected.Deposit != nil {
		bounds = &ExpectedRepayment{expected.Deposit.MinimumDebitRaw, expected.Deposit.MaximumDebitRaw}
	}
	beforeByMint := make(map[string]uint64, len(expected.Accounts))
	afterByMint := make(map[string]uint64, len(expected.Accounts))
	for i, effect := range expected.Accounts {
		pre, preOK := preByAddress[effect.Address]
		post, postOK := postByAddress[effect.Address]
		if !preOK || !postOK || pre.OwnerProgram != effect.Owner || post.OwnerProgram != effect.Owner ||
			pre.Mint != effect.Mint || post.Mint != effect.Mint ||
			pre.Authority != effect.Authority || post.Authority != effect.Authority || pre.Raw != effect.BeforeRaw {
			return Reconciliation{}, nil, fmt.Errorf("transaction-scoped custody identity or precondition mismatch: %s", effect.Address)
		}
		if bounds != nil {
			var moved uint64
			if i == 0 && post.Raw <= pre.Raw {
				moved = pre.Raw - post.Raw
			} else if i == 1 && post.Raw >= pre.Raw {
				moved = post.Raw - pre.Raw
			}
			if moved < bounds.MinimumDebitRaw || moved > bounds.MaximumDebitRaw {
				return Reconciliation{}, nil, fmt.Errorf("transaction-scoped Kamino transfer outside finite bounds: %s", effect.Address)
			}
		} else if effect.MinimumAfterRaw != nil {
			if post.Raw < *effect.MinimumAfterRaw {
				return Reconciliation{}, nil, fmt.Errorf("transaction-scoped custody minimum postcondition mismatch: %s", effect.Address)
			}
		} else if post.Raw != effect.AfterRaw {
			return Reconciliation{}, nil, fmt.Errorf("transaction-scoped custody postcondition mismatch: %s", effect.Address)
		}
		if ^uint64(0)-beforeByMint[effect.Mint] < pre.Raw || ^uint64(0)-afterByMint[effect.Mint] < post.Raw {
			return Reconciliation{}, nil, fmt.Errorf("transaction reconciliation conservation overflow")
		}
		beforeByMint[effect.Mint] += pre.Raw
		afterByMint[effect.Mint] += post.Raw
		canonical = append(canonical, fmt.Sprintf("%s:%s:%s:%s:%d:%d", effect.Address, effect.Owner, effect.Mint, effect.Authority, pre.Raw, post.Raw))
	}
	if expected.Conserved {
		for mint, before := range beforeByMint {
			if afterByMint[mint] != before {
				return Reconciliation{}, nil, fmt.Errorf("transaction token conservation mismatch for expected mint")
			}
		}
	}
	if expected.ReturnData != nil {
		returnMatches := false
		if receipt.ReturnData != nil {
			returnMatches = receipt.ReturnData.ProgramID == expected.ReturnData.ProgramID &&
				receipt.ReturnData.DataBase64 == expected.ReturnData.DataBase64
		} else {
			wantLog := fmt.Sprintf("Program return: %s %s", expected.ReturnData.ProgramID, expected.ReturnData.DataBase64)
			for _, line := range receipt.Logs {
				if line == wantLog {
					returnMatches = true
					break
				}
			}
		}
		if !returnMatches {
			return Reconciliation{}, nil, fmt.Errorf("adaptor return data mismatch")
		}
	}
	sort.Strings(canonical)
	evidenceBody := map[string]any{
		"schema": "loyal-backyard-rwa-reconciled-effects/v1", "source": "confirmed-transaction-meta",
		"signature": receipt.Signature, "slot": receipt.Slot, "accounts": canonical,
	}
	if expected.ReturnData != nil {
		evidenceBody["returnData"] = expected.ReturnData
	}
	evidence, err := json.Marshal(evidenceBody)
	if err != nil {
		return Reconciliation{}, nil, err
	}
	hash := sha256.Sum256(evidence)
	return Reconciliation{ConfirmedSlot: receipt.Slot, EffectsSHA256: hex.EncodeToString(hash[:]), Conserved: true}, evidence, nil
}

// ReconcileConfirmedTransaction is the manifest-aware dispatch: only the
// native initializer branch differs — it reconciles through the explicit
// reviewed manifest — and every other kind delegates to the public reconciler
// unchanged.
func (m RouteManifest) ReconcileConfirmedTransaction(expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	if expected.Kind == "kamino-initialize" || expected.Initialization != nil {
		return m.reconcileKaminoInitialization(expected, receipt)
	}
	return ReconcileConfirmedTransaction(expected, receipt)
}

func transactionBalancesByAddress(balances []TransactionTokenBalance) (map[string]TransactionTokenBalance, error) {
	byAddress := make(map[string]TransactionTokenBalance, len(balances))
	for _, balance := range balances {
		if balance.Address == "" || (balance.OwnerProgram != classicTokenProgram && balance.OwnerProgram != token2022Program) ||
			balance.Mint == "" || balance.Authority == "" {
			return nil, fmt.Errorf("transaction token-balance identity is incomplete")
		}
		if _, duplicate := byAddress[balance.Address]; duplicate {
			return nil, fmt.Errorf("transaction token-balance address is duplicated")
		}
		byAddress[balance.Address] = balance
	}
	return byAddress, nil
}
