package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// POST /api/smart-accounts/yield-optimization/autodeposit/setup/confirm ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts.
// The device echoes its prepared metadata; the server rebuilds every address
// canonically, asserts the on-chain artifacts, records the intent
// (autodeposit_repository.go) and, for an already-active target, schedules the
// bootstrap sweep. Mobile's sendWalletPush stays inside the op (pathmap.md:14).
// Remaining family paths (floor/toggle/close confirms, sweeps GET) are units 2-3.
const autodepositSetupConfirmPath = "/api/smart-accounts/yield-optimization/autodeposit/setup/confirm"

// AutodepositSetupConfirmRequest is the documented body: every unsigned
// integer arrives as a decimal string, as the web client sends it.
type AutodepositSetupConfirmRequest struct {
	AmountPerPeriodRaw                  string `json:"amountPerPeriodRaw"`
	Cluster                             string `json:"cluster"`
	ConfirmedSlot                       string `json:"confirmedSlot"`
	DelegatedSigner                     string `json:"delegatedSigner"`
	ExpiryTimestamp                     string `json:"expiryTimestamp"`
	LiquidityMint                       string `json:"liquidityMint"`
	Nonce                               string `json:"nonce"`
	PeriodLengthSeconds                 string `json:"periodLengthSeconds"`
	PolicyAccount                       string `json:"policyAccount"`
	PolicyID                            string `json:"policyId"`
	PolicySeed                          string `json:"policySeed"`
	RecurringDelegation                 string `json:"recurringDelegation"`
	Settings                            string `json:"settings"`
	SetupSignature                      string `json:"setupSignature"`
	SetupStage                          string `json:"setupStage"`
	StartTimestamp                      string `json:"startTimestamp"`
	SubscriptionAuthority               string `json:"subscriptionAuthority"`
	SubscriptionAuthorityInitialization string `json:"subscriptionAuthorityInitialization"`
	SubscriptionDelegatee               string `json:"subscriptionDelegatee"`
	VaultIndex                          int    `json:"vaultIndex"`
	VaultPubkey                         string `json:"vaultPubkey"`
	VaultUsdcAta                        string `json:"vaultUsdcAta"`
	WalletAddress                       string `json:"walletAddress"`
	WalletBalanceFloorRaw               string `json:"walletBalanceFloorRaw"`
	WalletUsdcAta                       string `json:"walletUsdcAta"`
}

type AutodepositSetupConfirmSweep struct {
	Classification        string  `json:"classification"`
	Confidence            string  `json:"confidence"`
	EligibleAfter         string  `json:"eligibleAfter"`
	ExecuteNowAvailableAt *string `json:"executeNowAvailableAt"`
	ID                    string  `json:"id"`
	LotCount              int64   `json:"lotCount"`
	OriginalAmountRaw     string  `json:"originalAmountRaw"`
	Reason                string  `json:"reason"`
	RemainingAmountRaw    string  `json:"remainingAmountRaw"`
	SlotID                string  `json:"slotId"`
	Status                string  `json:"status"`
}

type AutodepositSetupConfirmBootstrapSweep struct {
	Reason *string                       `json:"reason,omitempty"`
	Status string                        `json:"status"`
	Sweep  *AutodepositSetupConfirmSweep `json:"sweep,omitempty"`
}

type AutodepositSetupConfirmTarget struct {
	Active                bool    `json:"active"`
	BalanceSweepPolicyID  *string `json:"balanceSweepPolicyId"`
	ID                    string  `json:"id"`
	LifecycleStatus       string  `json:"lifecycleStatus"`
	PolicyAccount         string  `json:"policyAccount"`
	RecurringDelegation   *string `json:"recurringDelegation"`
	WalletBalanceFloorRaw *string `json:"walletBalanceFloorRaw"`
}

type AutodepositSetupConfirmResponse struct {
	ConfirmedSlot  string                                 `json:"confirmedSlot"`
	BootstrapSweep *AutodepositSetupConfirmBootstrapSweep `json:"bootstrapSweep,omitempty"`
	Target         *AutodepositSetupConfirmTarget         `json:"target,omitempty"`
}

// parseAutodepositSetupConfirmBody ports
// parseEarnAutodepositSetupConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:466): fields evaluate in the
// TS object-literal order and every failure names its key, so the first bad
// field matches Next's error message exactly.
func parseAutodepositSetupConfirmBody(raw []byte) (autodepositSetupInput, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return autodepositSetupInput{}, errors.New("Request body must be an object.")
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return autodepositSetupInput{}, errors.New("Request body must be an object.")
	}
	in := autodepositSetupInput{}
	read := func() error {
		if err := readAutodepositUint(object, "amountPerPeriodRaw", &in.AmountPerPeriodRaw); err != nil {
			return err
		}
		if err := readAutodepositString(object, "cluster", &in.Cluster); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "confirmedSlot", &in.ConfirmedSlot); err != nil {
			return err
		}
		if err := readAutodepositString(object, "delegatedSigner", &in.DelegatedSigner); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "expiryTimestamp", &in.ExpiryTimestamp); err != nil {
			return err
		}
		if err := readAutodepositString(object, "liquidityMint", &in.LiquidityMint); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "nonce", &in.Nonce); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "periodLengthSeconds", &in.PeriodLengthSeconds); err != nil {
			return err
		}
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "policyId", &in.PolicyID); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "policySeed", &in.PolicySeed); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositString(object, "settings", &in.Settings); err != nil {
			return err
		}
		if err := readAutodepositString(object, "setupSignature", &in.SetupSignature); err != nil {
			return err
		}
		stage, err := requireAutodepositString(object, "setupStage")
		if err != nil {
			return err
		}
		switch stage {
		case "initialize_subscription_authority", "approve_token_delegate", "create_policy", "create_recurring_delegation":
			in.SetupStage = stage
		default:
			return errors.New("setupStage is not a supported autodeposit stage.")
		}
		if err := readAutodepositUint(object, "startTimestamp", &in.StartTimestamp); err != nil {
			return err
		}
		if err := readAutodepositString(object, "subscriptionAuthority", &in.SubscriptionAuthority); err != nil {
			return err
		}
		initialization, err := requireAutodepositString(object, "subscriptionAuthorityInitialization")
		if err != nil {
			return err
		}
		if initialization != "exists" && initialization != "required" {
			return errors.New("subscriptionAuthorityInitialization must be exists or required.")
		}
		in.SubscriptionAuthorityInitialization = initialization
		if err := readAutodepositString(object, "subscriptionDelegatee", &in.SubscriptionDelegatee); err != nil {
			return err
		}
		// readVaultIndex: the JSON number must be exactly 1.
		if value, ok := object["vaultIndex"]; !ok || value != float64(1) {
			return errors.New("vaultIndex must be 1 for Earn autodeposit.")
		}
		in.VaultIndex = 1
		if err := readAutodepositString(object, "vaultPubkey", &in.VaultPubkey); err != nil {
			return err
		}
		if err := readAutodepositString(object, "vaultUsdcAta", &in.VaultUsdcAta); err != nil {
			return err
		}
		if err := readAutodepositString(object, "walletAddress", &in.WalletAddress); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "walletBalanceFloorRaw", &in.WalletBalanceFloorRaw); err != nil {
			return err
		}
		return readAutodepositString(object, "walletUsdcAta", &in.WalletUsdcAta)
	}
	if err := read(); err != nil {
		return autodepositSetupInput{}, err
	}
	return in, nil
}

func requireAutodepositString(object map[string]any, key string) (string, error) {
	value, ok := object[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string.", key)
	}
	return strings.TrimSpace(value), nil
}

func readAutodepositString(object map[string]any, key string, into *string) error {
	value, err := requireAutodepositString(object, key)
	if err != nil {
		return err
	}
	*into = value
	return nil
}

// readAutodepositUint ports readBigIntString: a trimmed decimal string, parsed
// strictly. A value beyond uint64 is rejected with the same message the TS
// reader uses for malformed input.
func readAutodepositUint(object map[string]any, key string, into *uint64) error {
	value, ok := object[key].(string)
	if !ok {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	trimmed := strings.TrimSpace(value)
	for _, digit := range trimmed {
		if digit < '0' || digit > '9' {
			return fmt.Errorf("%s must be an unsigned integer string.", key)
		}
	}
	if trimmed == "" {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	parsed, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	*into = parsed
	return nil
}

func serializeAutodepositSweep(sweep *autodepositSweepRow) *AutodepositSetupConfirmSweep {
	if sweep == nil {
		return nil
	}
	out := &AutodepositSetupConfirmSweep{
		Classification:     sweep.Classification,
		Confidence:         sweep.Confidence,
		EligibleAfter:      isoMilliseconds(sweep.EligibleAfter),
		ID:                 strconv.FormatInt(sweep.ID, 10),
		LotCount:           sweep.LotCount,
		OriginalAmountRaw:  strconv.FormatUint(sweep.OriginalAmountRaw, 10),
		Reason:             sweep.Reason,
		RemainingAmountRaw: strconv.FormatUint(sweep.RemainingAmountRaw, 10),
		SlotID:             strconv.FormatInt(sweep.SlotID, 10),
		Status:             sweep.Status,
	}
	if sweep.ExecuteNowAvailableAt != nil {
		at := isoMilliseconds(*sweep.ExecuteNowAvailableAt)
		out.ExecuteNowAvailableAt = &at
	}
	return out
}

// serializeAutodepositTarget ports serializeTarget (route.ts:265).
func serializeAutodepositTarget(target autodepositTargetRow) *AutodepositSetupConfirmTarget {
	out := &AutodepositSetupConfirmTarget{
		Active:              target.Active,
		ID:                  strconv.FormatInt(target.ID, 10),
		LifecycleStatus:     target.LifecycleStatus,
		PolicyAccount:       target.PolicyAccount,
		RecurringDelegation: target.RecurringDelegation,
	}
	if target.BalanceSweepPolicyID != nil {
		id := strconv.FormatInt(*target.BalanceSweepPolicyID, 10)
		out.BalanceSweepPolicyID = &id
	}
	if target.WalletBalanceFloorRaw != nil {
		floor := strconv.FormatUint(*target.WalletBalanceFloorRaw, 10)
		out.WalletBalanceFloorRaw = &floor
	}
	return out
}

// readAutodepositBootstrapSnapshot ports readBootstrapWalletBalanceSnapshot
// (route.ts:309): the wallet USDC balance at confirm time, or a skip reason.
func (h earnRoutes) readAutodepositBootstrapSnapshot(ctx context.Context, in autodepositSetupInput) (autodepositBootstrapSnapshot, *string, error) {
	skip := func(reason string) (autodepositBootstrapSnapshot, *string, error) {
		return autodepositBootstrapSnapshot{}, &reason, nil
	}
	walletAta, err := solana.PublicKeyFromBase58(in.WalletUsdcAta)
	if err != nil {
		return autodepositBootstrapSnapshot{}, nil, err
	}
	slot, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{walletAta}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return autodepositBootstrapSnapshot{}, nil, err
	}
	account := accounts[0]
	if account == nil {
		return skip("wallet_usdc_ata_missing")
	}
	if account.Owner != solana.TokenProgramID {
		return skip("wallet_usdc_ata_invalid_owner")
	}
	data := account.Data
	if len(data) < 165 {
		return skip("wallet_usdc_ata_invalid_data")
	}
	mint := solana.PublicKeyFromBytes(data[0:32])
	if mint.String() != in.LiquidityMint {
		return skip("wallet_usdc_ata_non_usdc")
	}
	owner := solana.PublicKeyFromBytes(data[32:64])
	if owner.String() != in.WalletAddress {
		return skip("wallet_usdc_ata_wallet_mismatch")
	}
	hash := sha256.Sum256(data)
	return autodepositBootstrapSnapshot{
		AmountRaw:        binary.LittleEndian.Uint64(data[64:72]),
		ObservedSlot:     slot,
		ObservedAt:       time.Now(),
		Mint:             mint.String(),
		Owner:            owner.String(),
		AccountDataHash:  fmt.Sprintf("%x", hash[:]),
		AccountLamports:  strconv.FormatUint(account.Lamports, 10),
		AccountOwner:     account.Owner.String(),
		SetupSignature:   in.SetupSignature,
		Source:           autodepositBootstrapEventSource,
		SourceCommitment: "confirmed",
	}, nil, nil
}
