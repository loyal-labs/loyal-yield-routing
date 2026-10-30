package backyard

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/solana-foundation/solana-go/v2"
)

var (
	classicTokenProgram = solana.TokenProgramID.String()
	token2022Program    = solana.Token2022ProgramID.String()
	bridgeTokenProgram  = classicTokenProgram
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
	Schema            string                       `json:"schema"`
	Kind              effectsKind                  `json:"kind,omitempty"`
	Conserved         bool                         `json:"conserved"`
	Accounts          []ExpectedAccountEffect      `json:"accounts"`
	ReturnData        *ExpectedReturnData          `json:"returnData,omitempty"`
	ExitLiabilityRaw  *uint64                      `json:"exitLiabilityRaw,omitempty"`
	Sale              *ExpectedSale                `json:"sale,omitempty"`
	Repayment         *ExpectedRepayment           `json:"repayment,omitempty"`
	Deposit           *ExpectedDeposit             `json:"deposit,omitempty"`
	Initialization    *KaminoInitializationRequest `json:"initialization,omitempty"`
	ForeignCustodyRaw *uint64                      `json:"foreignCustodyRaw,omitempty"`
}

type effectsKind string

const (
	effectsCrossMintSwap    effectsKind = "cross-mint-swap"
	effectsAtomicStep       effectsKind = "atomic-step"
	effectsKaminoDeposit    effectsKind = "kamino-deposit"
	effectsKaminoRepay      effectsKind = "kamino-repay"
	effectsKaminoInitialize effectsKind = "kamino-initialize"
	effectsBridge           effectsKind = "bridge"
)

func (k effectsKind) minimums() bool { return k == effectsCrossMintSwap || k == effectsAtomicStep }

func (k effectsKind) custodies(n int) bool { return n == 2 || (k == effectsAtomicStep && n == 1) }

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

func DecodeExpectedEffects(v *Vault, data []byte) (ExpectedEffects, error) {
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
	if expected.Kind == effectsKaminoInitialize || expected.Initialization != nil {
		if err := v.manifest.validateInitializationEffects(expected); err != nil {
			return ExpectedEffects{}, err
		}
		return expected, nil
	}
	if expected.Schema != "loyal-backyard-rwa-expected-effects/v1" || len(expected.Accounts) == 0 {
		return ExpectedEffects{}, fmt.Errorf("incomplete expected effects")
	}
	minimums := expected.Kind.minimums()
	if !expected.Conserved && (!minimums || !expected.Kind.custodies(len(expected.Accounts))) {
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
		if !minimums && account.MinimumAfterRaw != nil {
			return ExpectedEffects{}, fmt.Errorf("minimum balance is only supported for a cross-mint swap or an atomic step")
		}
	}
	if err := validateRepaymentEffects(expected); err != nil {
		return ExpectedEffects{}, err
	}
	if expected.Kind == effectsAtomicStep && (expected.Conserved || expected.Accounts[0].MinimumAfterRaw == nil ||
		(len(expected.Accounts) == 2 && expected.Accounts[0].Mint == expected.Accounts[1].Mint)) {
		return ExpectedEffects{}, fmt.Errorf("invalid atomic step postconditions")
	}
	if expected.Kind == effectsCrossMintSwap {
		if expected.Accounts[0].Mint == expected.Accounts[1].Mint || expected.Accounts[0].MinimumAfterRaw != nil ||
			expected.Accounts[1].MinimumAfterRaw == nil || *expected.Accounts[1].MinimumAfterRaw != expected.Accounts[1].AfterRaw {
			return ExpectedEffects{}, fmt.Errorf("invalid cross-mint swap postconditions")
		}
	}
	if expected.ReturnData != nil {
		data, err := base64.StdEncoding.DecodeString(expected.ReturnData.DataBase64)
		if expected.Kind != effectsBridge || expected.ReturnData.ProgramID != v.Adaptor || err != nil || len(data) != 8 {
			return ExpectedEffects{}, fmt.Errorf("invalid expected adaptor return data")
		}
	}
	if err := validateForeignSweep(v, expected); err != nil {
		return ExpectedEffects{}, err
	}
	return expected, nil
}

func validateRepaymentEffects(expected ExpectedEffects) error {
	bounds := expected.Repayment
	switch {
	case expected.Deposit != nil:
		if expected.Kind != effectsKaminoDeposit || expected.Repayment != nil {
			return fmt.Errorf("deposit bounds require only a Kamino deposit")
		}
		bounds = &ExpectedRepayment{expected.Deposit.MinimumDebitRaw, expected.Deposit.MaximumDebitRaw}
	case expected.Kind == effectsKaminoDeposit:
		return fmt.Errorf("missing finite Kamino deposit bounds")
	case expected.Kind != effectsKaminoRepay:
		if expected.Repayment != nil {
			return fmt.Errorf("repayment bounds require a Kamino repayment")
		}
		return nil
	}
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

func ReconcileConfirmedTransaction(v *Vault, expected ExpectedEffects, receipt ConfirmedTransactionEvidence) (Reconciliation, []byte, error) {
	if err := validateForeignSweep(v, expected); err != nil {
		return Reconciliation{}, nil, err
	}
	if expected.Kind == effectsKaminoInitialize || expected.Initialization != nil {
		return v.manifest.reconcileKaminoInitialization(expected, receipt)
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
		bounds = &ExpectedRepayment{1, expected.Deposit.MaximumDebitRaw}
	}
	beforeByMint := make(map[string]uint64, len(expected.Accounts))
	afterByMint := make(map[string]uint64, len(expected.Accounts))
	for i, effect := range expected.Accounts {
		pre, preOK := preByAddress[effect.Address]
		post, postOK := postByAddress[effect.Address]
		if !preOK || !postOK || pre.OwnerProgram != effect.Owner || post.OwnerProgram != effect.Owner ||
			pre.Mint != effect.Mint || post.Mint != effect.Mint ||
			pre.Authority != effect.Authority || post.Authority != effect.Authority {
			return Reconciliation{}, nil, fmt.Errorf("transaction-scoped custody identity or precondition mismatch: %s", effect.Address)
		}
		moved, planned := int64(post.Raw)-int64(pre.Raw), int64(effect.AfterRaw)-int64(effect.BeforeRaw)
		if receipt.foreignSweepExtraRaw != 0 {
			if expected.ForeignCustodyRaw == nil || !receipt.Finalized {
				return Reconciliation{}, nil, fmt.Errorf("foreign sweep proof without planned sweep")
			}
			switch effect.Address {
			case v.strategyATA:
				planned -= int64(receipt.foreignSweepExtraRaw)
			case v.idleATA:
				planned += int64(receipt.foreignSweepExtraRaw)
			}
		}
		if bounds != nil {
			if i == 0 {
				moved = -moved
			}
			if moved < int64(bounds.MinimumDebitRaw) || moved > int64(bounds.MaximumDebitRaw) {
				return Reconciliation{}, nil, fmt.Errorf("transaction-scoped Kamino transfer outside finite bounds: %s", effect.Address)
			}
		} else if effect.MinimumAfterRaw != nil {
			if moved < int64(*effect.MinimumAfterRaw)-int64(effect.BeforeRaw) {
				return Reconciliation{}, nil, fmt.Errorf("transaction-scoped custody minimum postcondition mismatch: %s", effect.Address)
			}
		} else if moved != planned {
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
	if receipt.foreignSweepExtraRaw != 0 {
		evidenceBody["foreignSweepExtraRaw"] = receipt.foreignSweepExtraRaw
		evidenceBody["foreignSweepCheckpoint"] = receipt.foreignSweepCheckpoint
	}
	if expected.ReturnData != nil {
		evidenceBody["returnData"] = expected.ReturnData
	}
	if sale := expected.Sale; sale != nil {
		pre, post := preByAddress[sale.OutputAccount], postByAddress[sale.OutputAccount]
		if post.Raw > pre.Raw {
			evidenceBody["sale"] = realizedSale{ExpectedSale: *sale, ReturnedRaw: post.Raw - pre.Raw, Slot: receipt.Slot}
		}
	}
	evidence, err := json.Marshal(evidenceBody)
	if err != nil {
		return Reconciliation{}, nil, err
	}
	hash := sha256.Sum256(evidence)
	return Reconciliation{ConfirmedSlot: receipt.Slot, EffectsSHA256: hex.EncodeToString(hash[:]), Conserved: true}, evidence, nil
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
