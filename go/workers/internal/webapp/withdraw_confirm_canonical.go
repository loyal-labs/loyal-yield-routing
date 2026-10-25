package webapp

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const withdrawMaxSafePolicySeed = uint64(1<<53 - 1) // Number.MAX_SAFE_INTEGER

func withdrawNormalizeCluster(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "devnet":
		return string(LoyalDevnet), nil
	case "mainnet-beta", "mainnet":
		return string(LoyalMainnetBeta), nil
	}
	return "", fmt.Errorf("unsupported Loyal cluster: %s", value)
}

func withdrawPublicKeyInput(value string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(value)
	if err != nil {
		// web3.js rejects the same inputs from its PublicKey constructor; the
		// constructor text is part of the Next error body.
		return solana.PublicKey{}, fmt.Errorf("Invalid public key input: %s", value)
	}
	return key, nil
}

func withdrawSafePolicySeed(seed *big.Int) (uint64, error) {
	if seed.Sign() <= 0 || seed.Cmp(new(big.Int).SetUint64(withdrawMaxSafePolicySeed)) > 0 {
		return 0, errors.New("policySeed is outside the supported earn policy range.")
	}
	return seed.Uint64(), nil
}

func withdrawAssertCanonical(actual, expected string, label string) error {
	if actual != expected {
		return fmt.Errorf("%s does not match the canonical earn withdrawal metadata.", label)
	}
	return nil
}

func withdrawAssertCanonicalOptional(actual, expected *string, label string) error {
	if actual == nil || expected == nil {
		if actual != expected {
			return fmt.Errorf("%s does not match the canonical earn withdrawal metadata.", label)
		}
		return nil
	}
	return withdrawAssertCanonical(*actual, *expected, label)
}

// withdrawCanonicalInput is the slice of the canonical input the confirm flow
// reads after canonicalization; the parser fields it drops are validated but
// never read (single-writer rule: confirm records nothing).
type withdrawCanonicalInput struct {
	Cluster             string
	WalletAddress       string
	Settings            string
	VaultIndex          int
	VaultPubkey         string
	PolicyAccount       string
	PolicySeed          uint64
	SetupPolicyAccount  *string
	SetupPolicySeed     *uint64
	WithdrawalSignature string
	ConfirmedSlot       *big.Int
	TargetReserve       string
	Market              *string
	LiquidityMint       string
	Mode                string
	IsFinalStep         *bool
	SourceType          string
	SourceAmountRaw     *big.Int
	WithdrawnAmountRaw  *big.Int
}

func createWithdrawCanonicalInput(in withdrawConfirmInput) (withdrawCanonicalInput, error) {
	cluster, err := withdrawNormalizeCluster(in.Cluster)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	settings, err := withdrawPublicKeyInput(in.Settings)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	policySeed, err := withdrawSafePolicySeed(in.PolicySeed)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	setupSeed := policySeed + 1
	if setupSeed > withdrawMaxSafePolicySeed {
		return withdrawCanonicalInput{}, errors.New("policySeed is outside the supported earn policy range.")
	}
	expectedPolicyAccount, _, err := squads.PolicyAddress(settings, policySeed)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	expectedSetupPolicyAccount, _, err := squads.PolicyAddress(settings, setupSeed)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	expectedVault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	hasSetupPolicyMetadata := in.SetupPolicyAccount != nil || in.SetupPolicyID != nil || in.SetupPolicySeed != nil

	liquidityMint, err := withdrawPublicKeyInput(in.LiquidityMint)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}
	productAsset, err := earnprep.WithdrawResolveProductAsset(cluster, liquidityMint)
	if err != nil {
		return withdrawCanonicalInput{}, err
	}

	sourceType := "reserve"
	if in.SourceType != nil {
		sourceType = *in.SourceType
	}
	var target struct {
		liquidityMint      string
		market             *string
		sourceTokenAccount *string
		targetReserve      string
	}
	switch sourceType {
	case "idle":
		vaultLiquidityAta, err := spl.AssociatedTokenAddress(expectedVault, productAsset.Mint, productAsset.TokenProgram)
		if err != nil {
			return withdrawCanonicalInput{}, err
		}
		ataText := vaultLiquidityAta.String()
		if in.Market != nil {
			return withdrawCanonicalInput{}, errors.New("Earn idle withdrawal market must be null.")
		}
		if in.SourceTokenAccount == nil || *in.SourceTokenAccount != ataText {
			return withdrawCanonicalInput{}, errors.New("Earn idle withdrawal source token account does not match the vault mint account.")
		}
		if in.SourceMint != nil && *in.SourceMint != productAsset.Mint.String() {
			return withdrawCanonicalInput{}, errors.New("Earn idle withdrawal source mint does not match the withdrawal mint.")
		}
		if in.TargetReserve != ataText {
			return withdrawCanonicalInput{}, errors.New("Earn idle withdrawal target does not match the vault mint account.")
		}
		target.liquidityMint = productAsset.Mint.String()
		target.sourceTokenAccount = &ataText
		target.targetReserve = ataText
	case "reserve":
		if in.Market == nil {
			return withdrawCanonicalInput{}, errors.New("Earn reserve withdrawal market is required.")
		}
		if _, err := withdrawPublicKeyInput(*in.Market); err != nil {
			return withdrawCanonicalInput{}, err
		}
		if _, err := withdrawPublicKeyInput(in.TargetReserve); err != nil {
			return withdrawCanonicalInput{}, err
		}
		target.liquidityMint = productAsset.Mint.String()
		target.market = in.Market
		target.sourceTokenAccount = in.SourceTokenAccount
		target.targetReserve = in.TargetReserve
	default:
		return withdrawCanonicalInput{}, errors.New("Earn withdrawal source type is invalid.")
	}

	canonical := withdrawCanonicalInput{
		Cluster:             cluster,
		WalletAddress:       in.WalletAddress,
		Settings:            in.Settings,
		VaultIndex:          int(earnprep.EarnVaultIndex),
		VaultPubkey:         expectedVault.String(),
		PolicyAccount:       expectedPolicyAccount.String(),
		PolicySeed:          policySeed,
		WithdrawalSignature: in.WithdrawalSignature,
		ConfirmedSlot:       in.ConfirmedSlot,
		TargetReserve:       target.targetReserve,
		Market:              target.market,
		LiquidityMint:       target.liquidityMint,
		Mode:                in.Mode,
		IsFinalStep:         in.IsFinalStep,
		SourceType:          sourceType,
		SourceAmountRaw:     in.SourceAmountRaw,
		WithdrawnAmountRaw:  in.WithdrawnAmountRaw,
	}
	if hasSetupPolicyMetadata {
		setupAccount := expectedSetupPolicyAccount.String()
		canonical.SetupPolicyAccount = &setupAccount
		canonical.SetupPolicySeed = &setupSeed
	}

	// The cluster, policySeed and targetReserve asserts compare parsed values
	// against themselves (or against a value an earlier branch already
	// checked), so only the fields that can actually disagree stay.
	if err := withdrawAssertCanonical(in.LiquidityMint, canonical.LiquidityMint, "liquidityMint"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if err := withdrawAssertCanonicalOptional(in.Market, canonical.Market, "market"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if err := withdrawAssertCanonicalOptional(in.SourceTokenAccount, target.sourceTokenAccount, "sourceTokenAccount"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if err := withdrawAssertCanonical(in.PolicyAccount, canonical.PolicyAccount, "policyAccount"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if in.PolicyID.Cmp(in.PolicySeed) != 0 {
		return withdrawCanonicalInput{}, fmt.Errorf("policyId does not match the canonical earn withdrawal metadata.")
	}
	if hasSetupPolicyMetadata {
		if err := withdrawAssertCanonicalOptional(in.SetupPolicyAccount, canonical.SetupPolicyAccount, "setupPolicyAccount"); err != nil {
			return withdrawCanonicalInput{}, err
		}
		setupExpected := new(big.Int).SetUint64(setupSeed)
		if in.SetupPolicyID == nil || in.SetupPolicyID.Cmp(setupExpected) != 0 {
			return withdrawCanonicalInput{}, fmt.Errorf("setupPolicyId does not match the canonical earn withdrawal metadata.")
		}
		if in.SetupPolicySeed == nil || in.SetupPolicySeed.Cmp(setupExpected) != 0 {
			return withdrawCanonicalInput{}, fmt.Errorf("setupPolicySeed does not match the canonical earn withdrawal metadata.")
		}
	}
	if err := withdrawAssertCanonical(in.SmartAccountAddress, canonical.VaultPubkey, "smartAccountAddress"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if in.VaultIndex != canonical.VaultIndex {
		return withdrawCanonicalInput{}, fmt.Errorf("vaultIndex does not match the canonical earn withdrawal metadata.")
	}
	if err := withdrawAssertCanonical(in.VaultPubkey, canonical.VaultPubkey, "vaultPubkey"); err != nil {
		return withdrawCanonicalInput{}, err
	}
	if in.AutodepositClose != nil {
		return withdrawCanonicalInput{}, errors.New("Withdrawal confirmation cannot include policy close metadata; close policies only after zero-balance verification.")
	}
	return canonical, nil
}
