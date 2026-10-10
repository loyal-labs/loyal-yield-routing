package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type VoltrWithdrawalReceipt struct {
	Address string
	voltr.WithdrawalReceipt
}

type programAccount struct {
	Address string
	Account ConfirmedAccount
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
