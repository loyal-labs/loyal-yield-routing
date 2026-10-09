package backyard

import (
	"context"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const maxConfirmedObservationAttempts = 3

type VoltrWithdrawalReceipt struct {
	Address string
	voltr.WithdrawalReceipt
}

type programAccount struct {
	Address string
	Account ConfirmedAccount
}

func getVoltrWithdrawalReceiptAccounts(ctx context.Context, c *chain.Client, vault string, minContextSlot int64) (int64, []programAccount, error) {
	if minContextSlot <= 0 {
		return 0, nil, fmt.Errorf("positive minimum context slot is required")
	}
	vaultKey, err := solana.PublicKeyFromBase58(vault)
	if err != nil {
		return 0, nil, err
	}
	slot, read, err := c.ProgramAccounts(ctx, voltr.ProgramID, voltr.WithdrawalReceiptFilters(vaultKey), rpc.CommitmentConfirmed, uint64(minContextSlot))
	if err != nil {
		return 0, nil, confirmedObservationUnavailable(err)
	}
	seen := make(map[solana.PublicKey]struct{}, len(read))
	accounts := make([]programAccount, 0, len(read))
	for _, account := range read {
		if _, duplicate := seen[account.Key]; duplicate {
			return 0, nil, fmt.Errorf("duplicate receipt account %s", account.Key)
		}
		seen[account.Key] = struct{}{}
		address := account.Key.String()
		accounts = append(accounts, programAccount{Address: address, Account: confirmedAccount(address, &account)})
	}
	return int64(slot), accounts, nil
}

// DecodeVoltrWithdrawalReceipt admits a deployed withdrawal receipt of vault
// at address only when it is the canonical vault/user PDA with its bump and
// owes no more than vaultCapRaw.
func DecodeVoltrWithdrawalReceipt(account ConfirmedAccount, address, vault string, vaultCapRaw uint64) (VoltrWithdrawalReceipt, error) {
	if account.Address != "" && account.Address != address {
		return VoltrWithdrawalReceipt{}, fmt.Errorf("receipt address envelope mismatch")
	}
	account.Address = address
	receipt, err := voltr.DecodeWithdrawalReceipt(chainAccount(account, address))
	if err != nil {
		return VoltrWithdrawalReceipt{}, fmt.Errorf("receipt layout mismatch: %w", err)
	}
	if receipt.Vault.String() != vault {
		return VoltrWithdrawalReceipt{}, fmt.Errorf("receipt vault mismatch")
	}
	if receipt.UpperBoundAssetRaw > vaultCapRaw {
		return VoltrWithdrawalReceipt{}, fmt.Errorf("receipt amount exceeds approved cap")
	}
	expected, bump, err := voltr.WithdrawalReceiptAddress(receipt.Vault, receipt.User)
	if err != nil || expected.String() != address || receipt.Bump != bump {
		return VoltrWithdrawalReceipt{}, fmt.Errorf("receipt is not the canonical vault/user PDA")
	}
	return VoltrWithdrawalReceipt{Address: address, WithdrawalReceipt: receipt}, nil
}

func decodeBase58PublicKey(input string) ([32]byte, error) {
	var result [32]byte
	decoded, err := decodeKey(input)
	if err != nil || encodeBase58(decoded[:]) != input {
		return result, fmt.Errorf("non-canonical base58 public key")
	}
	copy(result[:], decoded[:])
	return result, nil
}
