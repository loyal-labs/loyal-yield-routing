package backyardrwa

import (
	"crypto/sha256"
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
}

type ExpectedEffects struct {
	Schema    string                  `json:"schema"`
	Conserved bool                    `json:"conserved"`
	Accounts  []ExpectedAccountEffect `json:"accounts"`
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
	if expected.Schema != "loyal-backyard-rwa-expected-effects/v1" || !expected.Conserved || len(expected.Accounts) == 0 {
		return ExpectedEffects{}, fmt.Errorf("incomplete expected effects")
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
	}
	return expected, nil
}

func ReconcileTokenAccounts(slot int64, expected ExpectedEffects, accounts []ConfirmedAccount) (Reconciliation, []byte, error) {
	if slot <= 0 || len(accounts) != len(expected.Accounts) {
		return Reconciliation{}, nil, fmt.Errorf("incoherent reconciliation account set")
	}
	byAddress := make(map[string]ConfirmedAccount, len(accounts))
	for _, account := range accounts {
		if _, duplicate := byAddress[account.Address]; duplicate {
			return Reconciliation{}, nil, fmt.Errorf("duplicate observed custody")
		}
		byAddress[account.Address] = account
	}
	canonical := make([]string, 0, len(expected.Accounts))
	beforeByMint := make(map[string]uint64, len(expected.Accounts))
	afterByMint := make(map[string]uint64, len(expected.Accounts))
	for _, effect := range expected.Accounts {
		account, ok := byAddress[effect.Address]
		if !ok || account.Owner != effect.Owner || account.Executable || account.Lamports == 0 {
			return Reconciliation{}, nil, fmt.Errorf("custody identity mismatch: %s", effect.Address)
		}
		mint, err := decodeBase58PublicKey(effect.Mint)
		if err != nil {
			return Reconciliation{}, nil, fmt.Errorf("invalid expected mint")
		}
		authority, err := decodeBase58PublicKey(effect.Authority)
		if err != nil {
			return Reconciliation{}, nil, fmt.Errorf("invalid expected authority")
		}
		decoded, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
		if err != nil {
			return Reconciliation{}, nil, fmt.Errorf("decode reconciled custody %s: %w", effect.Address, err)
		}
		observedRaw := decoded.Raw
		if observedRaw != effect.AfterRaw {
			return Reconciliation{}, nil, fmt.Errorf("custody postcondition mismatch: %s", effect.Address)
		}
		if ^uint64(0)-beforeByMint[effect.Mint] < effect.BeforeRaw || ^uint64(0)-afterByMint[effect.Mint] < observedRaw {
			return Reconciliation{}, nil, fmt.Errorf("reconciliation conservation overflow")
		}
		beforeByMint[effect.Mint] += effect.BeforeRaw
		afterByMint[effect.Mint] += observedRaw
		canonical = append(canonical, fmt.Sprintf("%s:%s:%s:%s:%d:%d", effect.Address, effect.Owner, effect.Mint, effect.Authority, effect.BeforeRaw, observedRaw))
	}
	for mint, before := range beforeByMint {
		if afterByMint[mint] != before {
			return Reconciliation{}, nil, fmt.Errorf("token conservation mismatch for expected mint")
		}
	}
	sort.Strings(canonical)
	evidence, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-reconciled-effects/v1", "slot": slot, "accounts": canonical})
	if err != nil {
		return Reconciliation{}, nil, err
	}
	hash := sha256.Sum256(evidence)
	return Reconciliation{ConfirmedSlot: slot, EffectsSHA256: hex.EncodeToString(hash[:]), Conserved: expected.Conserved}, evidence, nil
}
