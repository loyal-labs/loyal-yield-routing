package backyard

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func (o Observation) Validate() error {
	if o.ObservedAt.IsZero() || o.Snapshot.ObservationID == "" || o.Snapshot.Slot <= 0 {
		return fmt.Errorf("incomplete observation identity")
	}
	if o.Snapshot.RouteKind == "" {
		return fmt.Errorf("missing route observation kind")
	}
	return nil
}

func accountAt(accounts []ConfirmedAccount, address string) ConfirmedAccount {
	for _, account := range accounts {
		if account.Address == address {
			return account
		}
	}
	return ConfirmedAccount{}
}

func decodePinnedUSDC(account ConfirmedAccount, authority string) (DecodedTokenCustody, error) {
	if account.Address == "" || account.Executable || account.Lamports == 0 {
		return DecodedTokenCustody{}, fmt.Errorf("missing or invalid custody envelope")
	}
	mint, err := decodeBase58PublicKey(fleet.USDCMint)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	owner, err := decodeBase58PublicKey(authority)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	return DecodeTokenCustody(account.Owner, account.Data, mint, owner)
}

func decodeConfirmedWithdrawalDemand(v *Vault, receipts []programAccount) (int64, string, error) {
	decoded := make([]VoltrWithdrawalReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		value, err := DecodeVoltrWithdrawalReceipt(receipt.Account, receipt.Address, v.VoltrVault, bridgeCapRaw)
		if err != nil {
			return 0, "", fmt.Errorf("decode withdrawal receipt: %w", err)
		}
		decoded = append(decoded, value)
	}
	sort.Slice(decoded, func(i, j int) bool { return decoded[i].Address < decoded[j].Address })
	var total uint64
	parts := make([]string, 0, len(decoded))
	for _, receipt := range decoded {
		if ^uint64(0)-total < receipt.UpperBoundAssetRaw {
			return 0, "", fmt.Errorf("withdrawal demand overflow")
		}
		total += receipt.UpperBoundAssetRaw
		parts = append(parts, fmt.Sprintf("%s:%d:%d", receipt.Address, receipt.UpperBoundAssetRaw, receipt.WithdrawableFromTS))
	}
	if total > uint64(^uint64(0)>>1) {
		return 0, "", fmt.Errorf("withdrawal demand exceeds signed range")
	}
	hash := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return int64(total), fmt.Sprintf("%x", hash[:]), nil
}
