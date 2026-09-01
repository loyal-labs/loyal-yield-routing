package backyardrwa

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
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

// ObserveConfirmedBridgeSnapshot obtains the only observation that can be
// made from the currently pinned, locally evidenced bridge identities. Receipt
// accounts and all three USDC custodies must share one confirmed slot. Kamino
// state is intentionally not inferred here; without an exact current account
// graph the decision engine can only HOLD or select a bridge/withdrawal action.
func ObserveConfirmedBridgeSnapshot(ctx context.Context, rpc *RPCClient) (Observation, error) {
	if rpc == nil {
		return Observation{}, fmt.Errorf("RPC client is required")
	}
	minSlot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return Observation{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		receiptSlot, rawReceipts, err := rpc.getVoltrWithdrawalReceiptAccounts(ctx, bridgeVoltrProgram, bridgeVoltrVault, minSlot)
		if err != nil {
			return Observation{}, err
		}
		custodySlot, accounts, err := rpc.GetMultipleAccounts(ctx, []string{bridgeIdleATA, bridgeStrategyATA, bridgeSquadsATA}, minSlot)
		if err != nil {
			return Observation{}, err
		}
		if receiptSlot != custodySlot {
			if receiptSlot > custodySlot {
				minSlot = receiptSlot
			} else {
				minSlot = custodySlot
			}
			continue
		}
		idle, err := decodePinnedUSDC(accountAt(accounts, bridgeIdleATA), bridgeIdleAuthority)
		if err != nil {
			return Observation{}, fmt.Errorf("decode Voltr idle custody: %w", err)
		}
		strategy, err := decodePinnedUSDC(accountAt(accounts, bridgeStrategyATA), bridgeStrategyAuth)
		if err != nil {
			return Observation{}, fmt.Errorf("decode Voltr strategy custody: %w", err)
		}
		squads, err := decodePinnedUSDC(accountAt(accounts, bridgeSquadsATA), bridgeVault)
		if err != nil {
			return Observation{}, fmt.Errorf("decode Squads USDC custody: %w", err)
		}
		if idle.Raw > uint64(^uint64(0)>>1) || strategy.Raw > uint64(^uint64(0)>>1) || squads.Raw > uint64(^uint64(0)>>1) {
			return Observation{}, fmt.Errorf("bridge custody exceeds signed decision range")
		}
		demand, receiptFingerprint, err := decodeConfirmedWithdrawalDemand(rawReceipts)
		if err != nil {
			return Observation{}, err
		}
		stateHash := sha256.Sum256([]byte(fmt.Sprintf(
			"%s|voltr-idle:%d|strategy-idle:%d|squads-idle:%d",
			receiptFingerprint, idle.Raw, strategy.Raw, squads.Raw,
		)))
		return Observation{ObservedAt: time.Now().UTC(), Snapshot: Snapshot{
			ObservationID:        fmt.Sprintf("%x", stateHash[:]),
			Slot:                 receiptSlot,
			RouteKind:            RouteKind,
			Fresh:                true,
			WithdrawalDemandRaw:  demand,
			VoltrIdleRaw:         int64(idle.Raw),
			VoltrStrategyIdleRaw: int64(strategy.Raw),
			SquadsIdleRaw:        int64(squads.Raw),
			// No complete, current Kamino graph is checked into this repository.
			// Leaving these gates false prevents an unsupported OPEN decision.
			CapacityRaw:             0,
			PolicyLimitRaw:          0,
			MaxTargetLTVEntryRaw:    0,
			PolicyReady:             false,
			ExitBuildable:           false,
			LastReportAgeSeconds:    0,
			LiquidationThresholdBPS: 0,
		}}, nil
	}
	return Observation{}, fmt.Errorf("confirmed receipt and custody reads did not align after %d attempts", maxConfirmedObservationAttempts)
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
	mint, err := decodeBase58PublicKey(bridgeUSDC)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	owner, err := decodeBase58PublicKey(authority)
	if err != nil {
		return DecodedTokenCustody{}, err
	}
	return DecodeTokenCustody(account.Owner, account.Data, mint, owner)
}

func decodeConfirmedWithdrawalDemand(receipts []programAccount) (int64, string, error) {
	decoded := make([]VoltrWithdrawalReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		value, err := DecodeVoltrWithdrawalReceipt(receipt.Account, receipt.Address, bridgeVoltrProgram, bridgeVoltrVault, bridgeCapRaw)
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

func tokenRaw(data []byte) uint64 {
	if len(data) < 72 {
		return 0
	}
	return binary.LittleEndian.Uint64(data[64:72])
}
