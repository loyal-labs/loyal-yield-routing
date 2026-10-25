package webapp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	autodepositMonthPeriodSeconds uint64 = 30 * 24 * 60 * 60
	autodepositMaxSafePolicySeed  uint64 = 1<<53 - 1

	// SPL token account layout: the delegate COption sits between the amount
	// and the account state; its delegated amount follows is_native.
	autodepositTokenDelegateTagOffset     = 72
	autodepositTokenDelegateOffset        = 76
	autodepositTokenDelegatedAmountOffset = 121
)

var autodepositSubscriptionsProgram = solana.MustPublicKeyFromBase58(earncatalog.SubscriptionsProgram)

// autodepositSetupInput is the parsed POST /autodeposit/setup/confirm body,
// mirroring ConfirmedEarnAutodepositSetupInput (BigInt fields become uint64;
// the TS parser reads each of these from decimal strings except vaultIndex,
// which must be the JSON number 1).
type autodepositSetupInput struct {
	AmountPerPeriodRaw                  uint64
	Cluster                             string
	ConfirmedSlot                       uint64
	DelegatedSigner                     string
	ExpiryTimestamp                     uint64
	LiquidityMint                       string
	Nonce                               uint64
	PeriodLengthSeconds                 uint64
	PolicyAccount                       string
	PolicyID                            uint64
	PolicySeed                          uint64
	RecurringDelegation                 string
	Settings                            string
	SetupSignature                      string
	SetupStage                          string
	StartTimestamp                      uint64
	SubscriptionAuthority               string
	SubscriptionAuthorityInitialization string
	SubscriptionDelegatee               string
	VaultIndex                          int
	VaultPubkey                         string
	VaultUsdcAta                        string
	WalletAddress                       string
	WalletBalanceFloorRaw               uint64
	WalletUsdcAta                       string
}

func autodepositUSDCMint(cluster LoyalCluster) solana.PublicKey {
	if cluster == LoyalDevnet {
		return solana.MustPublicKeyFromBase58(earncatalog.DevnetUSDC)
	}
	return solana.MustPublicKeyFromBase58(earncatalog.USDC)
}

func normalizeLoyalCluster(cluster string) (string, error) {
	switch cluster {
	case string(LoyalDevnet):
		return string(LoyalDevnet), nil
	case string(LoyalMainnetBeta), "mainnet":
		return string(LoyalMainnetBeta), nil
	}
	return "", fmt.Errorf("unsupported Loyal cluster: %s", cluster)
}

func autodepositSubscriptionAuthority(wallet, mint solana.PublicKey) solana.PublicKey {
	key, _, err := solana.FindProgramAddress([][]byte{
		[]byte(earncatalog.SubscriptionAuthoritySeed), wallet[:], mint[:],
	}, autodepositSubscriptionsProgram)
	if err != nil {
		return solana.PublicKey{}
	}
	return key
}

func autodepositRecurringDelegation(authority, delegator, delegatee solana.PublicKey, nonce uint64) solana.PublicKey {
	key, _, err := solana.FindProgramAddress([][]byte{
		[]byte(earncatalog.SubscriptionDelegateSeed), authority[:], delegator[:], delegatee[:],
		binary.LittleEndian.AppendUint64(nil, nonce),
	}, autodepositSubscriptionsProgram)
	if err != nil {
		return solana.PublicKey{}
	}
	return key
}

func autodepositSubscriptionEventAuthority() solana.PublicKey {
	key, _, err := solana.FindProgramAddress([][]byte{
		[]byte(earncatalog.SubscriptionEventSeed),
	}, autodepositSubscriptionsProgram)
	if err != nil {
		return solana.PublicKey{}
	}
	return key
}

// canonicalizeAutodepositSetupInput mirrors the route's
// createCanonicalAutodepositSetupInput: rebuild every address from the
// canonical derivations and reject the request field by field, in the TS
// order, so the first failure names the same label.
func canonicalizeAutodepositSetupInput(in autodepositSetupInput, policySigner solana.PublicKey) (autodepositSetupInput, error) {
	cluster, err := normalizeLoyalCluster(in.Cluster)
	if err != nil {
		return in, err
	}
	settings, err := solana.PublicKeyFromBase58(in.Settings)
	if err != nil {
		return in, fmt.Errorf("settings is not a valid public key: %w", err)
	}
	wallet, err := solana.PublicKeyFromBase58(in.WalletAddress)
	if err != nil {
		return in, fmt.Errorf("walletAddress is not a valid public key: %w", err)
	}
	if in.PolicySeed == 0 || in.PolicySeed > autodepositMaxSafePolicySeed {
		return in, errors.New("policySeed is outside the supported autodeposit range.")
	}
	expectedPolicy, _, err := squads.PolicyAddress(settings, in.PolicySeed)
	if err != nil {
		return in, err
	}
	expectedVault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
	if err != nil {
		return in, err
	}
	usdcMint := autodepositUSDCMint(LoyalCluster(cluster))
	subscriptionAuthority := autodepositSubscriptionAuthority(wallet, usdcMint)
	recurringDelegation := autodepositRecurringDelegation(subscriptionAuthority, wallet, expectedVault, in.Nonce)
	walletUsdcAta, err := spl.AssociatedTokenAddress(wallet, usdcMint, solana.TokenProgramID)
	if err != nil {
		return in, err
	}
	vaultUsdcAta, err := spl.AssociatedTokenAddress(expectedVault, usdcMint, solana.TokenProgramID)
	if err != nil {
		return in, err
	}

	assert := func(actual, expected, label string) error {
		if actual != expected {
			return fmt.Errorf("%s does not match canonical Earn autodeposit metadata.", label)
		}
		return nil
	}
	if err := assert(in.DelegatedSigner, policySigner.String(), "delegatedSigner"); err != nil {
		return in, err
	}
	if err := assert(in.LiquidityMint, usdcMint.String(), "liquidityMint"); err != nil {
		return in, err
	}
	if in.PeriodLengthSeconds != autodepositMonthPeriodSeconds {
		return in, errors.New("periodLengthSeconds does not match canonical Earn autodeposit metadata.")
	}
	if err := assert(in.PolicyAccount, expectedPolicy.String(), "policyAccount"); err != nil {
		return in, err
	}
	if in.PolicyID != in.PolicySeed {
		return in, errors.New("policyId does not match canonical Earn autodeposit metadata.")
	}
	if err := assert(in.RecurringDelegation, recurringDelegation.String(), "recurringDelegation"); err != nil {
		return in, err
	}
	if err := assert(in.SubscriptionAuthority, subscriptionAuthority.String(), "subscriptionAuthority"); err != nil {
		return in, err
	}
	if err := assert(in.SubscriptionDelegatee, expectedVault.String(), "subscriptionDelegatee"); err != nil {
		return in, err
	}
	if in.VaultIndex != int(earnprep.EarnVaultIndex) {
		return in, errors.New("vaultIndex does not match canonical Earn autodeposit metadata.")
	}
	if err := assert(in.VaultPubkey, expectedVault.String(), "vaultPubkey"); err != nil {
		return in, err
	}
	if err := assert(in.VaultUsdcAta, vaultUsdcAta.String(), "vaultUsdcAta"); err != nil {
		return in, err
	}
	if err := assert(in.WalletUsdcAta, walletUsdcAta.String(), "walletUsdcAta"); err != nil {
		return in, err
	}
	if in.AmountPerPeriodRaw <= 0 {
		return in, errors.New("amountPerPeriodRaw must be greater than 0.")
	}
	if in.WalletBalanceFloorRaw < 0 {
		return in, errors.New("walletBalanceFloorRaw cannot be negative.")
	}

	in.Cluster = cluster
	in.DelegatedSigner = policySigner.String()
	in.LiquidityMint = usdcMint.String()
	in.PolicyAccount = expectedPolicy.String()
	in.PolicyID = in.PolicySeed
	in.RecurringDelegation = recurringDelegation.String()
	in.SubscriptionAuthority = subscriptionAuthority.String()
	in.SubscriptionDelegatee = expectedVault.String()
	in.VaultIndex = int(earnprep.EarnVaultIndex)
	in.VaultPubkey = expectedVault.String()
	in.VaultUsdcAta = vaultUsdcAta.String()
	in.WalletUsdcAta = walletUsdcAta.String()
	return in, nil
}

func autodepositConfirmedSlot(ctx context.Context, client *chain.Client, signature string) (uint64, error) {
	state, err := client.SignatureState(ctx, signature)
	if err != nil {
		return 0, err
	}
	if !state.Found || state.Err != "" || state.Commitment == chain.Processed {
		return 0, errors.New("Autodeposit setup transaction is not confirmed.")
	}
	if state.Slot == 0 {
		return 0, errors.New("Confirmed transaction slot is unavailable.")
	}
	return state.Slot, nil
}

var autodepositRetryableArtifact = regexp.MustCompile(`(?i)does not exist|not found|unable to find|unavailable`)

// withAutodepositArtifactRetry ports withEarnAutodepositArtifactRetry: an
// account that lands a moment after the transaction confirms is the happy
// path, so existence-shaped failures are retried before the route gives up.
func withAutodepositArtifactRetry(ctx context.Context, op func() error) error {
	var last error
	for attempt := 0; attempt < 8; attempt++ {
		if err := op(); err == nil {
			return nil
		} else {
			last = err
			if !autodepositRetryableArtifact.MatchString(err.Error()) || attempt == 7 {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(350 * time.Millisecond):
		}
	}
	return last
}

func autodepositAccountAt(ctx context.Context, client *chain.Client, keys ...solana.PublicKey) ([]*chain.Account, error) {
	_, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

type autodepositArtifactProbe struct {
	exists       bool
	invalidOwner bool
}

func assertAutodepositArtifactsExist(accounts []*chain.Account, requirePolicy, requireDelegation bool) error {
	policy := autodepositArtifactProbe{exists: accounts[0] != nil}
	if accounts[0] != nil && accounts[0].Owner != squads.ProgramID {
		policy.invalidOwner = true
	}
	delegation := autodepositArtifactProbe{exists: accounts[1] != nil}
	if accounts[1] != nil && accounts[1].Owner != autodepositSubscriptionsProgram {
		delegation.invalidOwner = true
	}
	if requirePolicy && !policy.exists {
		return errors.New("Confirmed Autodeposit policy account does not exist.")
	}
	if policy.invalidOwner {
		return errors.New("Confirmed Autodeposit policy has an unexpected owner.")
	}
	if !requireDelegation {
		return nil
	}
	if !delegation.exists {
		return errors.New("Confirmed Autodeposit recurring delegation account does not exist.")
	}
	if delegation.invalidOwner {
		return errors.New("Confirmed Autodeposit recurring delegation has an unexpected owner.")
	}
	return nil
}

func autodepositCanonicalTransferConstraint(in autodepositSetupInput, eventAuthority solana.PublicKey, vault, subscriptionAuthority, walletUsdcAta, vaultUsdcAta, usdcMint solana.PublicKey) squads.InstructionConstraintView {
	bytesAt := func(offset int, key solana.PublicKey) squads.DataConstraintView {
		return squads.DataConstraintView{DataOffset: uint64(offset), Operator: squads.OpEquals, DataValue: squads.DataValueView{Kind: squads.ValueBytes, Bytes: key.Bytes()}}
	}
	u8At := func(offset int, value uint8) squads.DataConstraintView {
		return squads.DataConstraintView{DataOffset: uint64(offset), Operator: squads.OpEquals, DataValue: squads.DataValueView{Kind: squads.ValueU8, U8: value}}
	}
	pinned := func(index uint8, keys ...solana.PublicKey) squads.AccountConstraintView {
		return squads.AccountConstraintView{AccountIndex: index, Pubkeys: keys}
	}
	owned := func(index uint8, owner solana.PublicKey, keys ...solana.PublicKey) squads.AccountConstraintView {
		return squads.AccountConstraintView{AccountIndex: index, Pubkeys: keys, Owner: &owner}
	}
	return squads.InstructionConstraintView{
		ProgramID: autodepositSubscriptionsProgram,
		AccountConstraints: []squads.AccountConstraintView{
			{
				AccountIndex: 0,
				Owner:        &autodepositSubscriptionsProgram,
				AccountData: []squads.DataConstraintView{
					u8At(0, earncatalog.DelegationDiscriminator),
					bytesAt(earncatalog.DelegationDelegatorOffset, walletFromInput(in)),
					bytesAt(earncatalog.DelegationDelegateeOffset, vault),
					bytesAt(earncatalog.DelegationAuthorityOffset, subscriptionAuthority),
					bytesAt(earncatalog.DelegationMintOffset, usdcMint),
					{DataOffset: earncatalog.DelegationPerPeriodOffset, Operator: squads.OpLessThanOrEqualTo, DataValue: squads.DataValueView{Kind: squads.ValueU64, U64: in.AmountPerPeriodRaw}},
				},
			},
			owned(1, autodepositSubscriptionsProgram, subscriptionAuthority),
			owned(2, solana.TokenProgramID, walletUsdcAta),
			owned(3, solana.TokenProgramID, vaultUsdcAta),
			owned(4, solana.TokenProgramID, usdcMint),
			pinned(5, solana.TokenProgramID),
			pinned(6, vault),
			pinned(7, eventAuthority),
			pinned(8, autodepositSubscriptionsProgram),
		},
		DataConstraints: []squads.DataConstraintView{
			u8At(0, 5), // SUBSCRIPTIONS_TRANSFER_RECURRING
			bytesAt(9, walletFromInput(in)),
			bytesAt(41, usdcMint),
		},
	}
}

func walletFromInput(in autodepositSetupInput) solana.PublicKey {
	key, _ := solana.PublicKeyFromBase58(in.WalletAddress)
	return key
}

func vaultFromInput(in autodepositSetupInput) solana.PublicKey {
	key, _ := solana.PublicKeyFromBase58(in.VaultPubkey)
	return key
}

func assertAutodepositCanonicalArtifacts(accounts []*chain.Account, in autodepositSetupInput, policySigner solana.PublicKey, requirePolicy, requireDelegation bool) error {
	if in.PolicySeed == 0 || in.PolicySeed > autodepositMaxSafePolicySeed {
		return errors.New("Autodeposit policy seed is outside the supported range.")
	}
	settings, err := solana.PublicKeyFromBase58(in.Settings)
	if err != nil {
		return err
	}
	wallet := walletFromInput(in)
	vault := vaultFromInput(in)
	usdcMint := solana.MustPublicKeyFromBase58(in.LiquidityMint)
	subscriptionAuthority := solana.MustPublicKeyFromBase58(in.SubscriptionAuthority)
	walletUsdcAta := solana.MustPublicKeyFromBase58(in.WalletUsdcAta)
	vaultUsdcAta := solana.MustPublicKeyFromBase58(in.VaultUsdcAta)

	expectedPolicy, _, err := squads.PolicyAddress(settings, in.PolicySeed)
	if err != nil {
		return err
	}
	if in.PolicyAccount != expectedPolicy.String() {
		return errors.New("Autodeposit policy account does not match its seed.")
	}

	if requirePolicy {
		decoded, err := decodeAutodepositPolicy(accounts[0])
		if err != nil {
			return err
		}
		if decoded.Settings != settings {
			return errors.New("Autodeposit policy settings do not match.")
		}
		if decoded.Seed != in.PolicySeed {
			return errors.New("Autodeposit policy seed does not match.")
		}
		if decoded.Threshold != 1 {
			return errors.New("Autodeposit policy threshold is not canonical.")
		}
		if decoded.TimeLock != 0 {
			return errors.New("Autodeposit policy timelock is not canonical.")
		}
		if len(decoded.Signers) != 1 {
			return errors.New("Autodeposit policy signer set is not canonical.")
		}
		if decoded.Signers[0] != policySigner {
			return errors.New("Autodeposit policy signer does not match.")
		}
		if len(decoded.Permissions) != 1 || decoded.Permissions[0] != squads.FullPermissions {
			return errors.New("Autodeposit policy signer permissions are incomplete.")
		}
		expected := autodepositCanonicalTransferConstraint(in, autodepositSubscriptionEventAuthority(), vault, subscriptionAuthority, walletUsdcAta, vaultUsdcAta, usdcMint)
		if decoded.Full == nil || decoded.Full.PreHook || decoded.Full.PostHook ||
			!decoded.Full.Payload.Equal(squads.Policy{VaultIndex: earnprep.EarnVaultIndex, Constraints: []squads.InstructionConstraintView{expected}}) {
			return errors.New("Autodeposit policy state is not canonical.")
		}
	}

	if !requireDelegation {
		return nil
	}
	delegation := accounts[1]
	if delegation == nil {
		return errors.New("Autodeposit recurring delegation account does not exist.")
	}
	if delegation.Owner != autodepositSubscriptionsProgram {
		return errors.New("Autodeposit recurring delegation owner is invalid.")
	}
	if len(delegation.Data) < earncatalog.DelegationDataLen {
		return errors.New("Autodeposit recurring delegation data is incomplete.")
	}
	if delegation.Data[0] != earncatalog.DelegationDiscriminator {
		return errors.New("Autodeposit recurring delegation discriminator is invalid.")
	}
	for _, check := range []struct {
		label    string
		offset   int
		expected solana.PublicKey
	}{
		{"delegator", earncatalog.DelegationDelegatorOffset, wallet},
		{"delegatee", earncatalog.DelegationDelegateeOffset, vault},
		{"authority", earncatalog.DelegationAuthorityOffset, subscriptionAuthority},
		{"mint", earncatalog.DelegationMintOffset, usdcMint},
	} {
		if solana.PublicKeyFromBytes(delegation.Data[check.offset:check.offset+32]) != check.expected {
			return fmt.Errorf("Autodeposit recurring delegation %s does not match.", check.label)
		}
	}
	if binary.LittleEndian.Uint64(delegation.Data[earncatalog.DelegationPerPeriodOffset:earncatalog.DelegationPerPeriodOffset+8]) != in.AmountPerPeriodRaw {
		return errors.New("Autodeposit recurring delegation amount does not match.")
	}
	return nil
}

func decodeAutodepositPolicy(account *chain.Account) (squads.DecodedPolicy, error) {
	if account == nil || account.Owner != squads.ProgramID || account.Executable {
		return squads.DecodedPolicy{}, errors.New("policy account is absent or not owned by Squads")
	}
	return squads.DecodeProgramInteractionPolicy(account)
}

func assertAutodepositWalletTokenApproval(account *chain.Account, in autodepositSetupInput) error {
	if account == nil {
		return errors.New("Wallet USDC ATA does not exist.")
	}
	data := account.Data
	if len(data) < autodepositTokenDelegatedAmountOffset+8 ||
		binary.LittleEndian.Uint32(data[autodepositTokenDelegateTagOffset:autodepositTokenDelegateTagOffset+4]) != 1 {
		return errors.New("Wallet USDC ATA delegate does not match autodeposit.")
	}
	if solana.PublicKeyFromBytes(data[autodepositTokenDelegateOffset:autodepositTokenDelegateOffset+32]) != solana.MustPublicKeyFromBase58(in.SubscriptionAuthority) {
		return errors.New("Wallet USDC ATA delegate does not match autodeposit.")
	}
	delegated := binary.LittleEndian.Uint64(data[autodepositTokenDelegatedAmountOffset : autodepositTokenDelegatedAmountOffset+8])
	if delegated < in.AmountPerPeriodRaw {
		return errors.New("Wallet USDC ATA delegated amount is below autodeposit period amount.")
	}
	return nil
}

// formatAutodepositUSDC ports formatUsdcAmountRaw:
// Number(amountRaw)/1e6 with toLocaleString("en-US", {maximumFractionDigits: 2})
// — thousands grouping, at most two decimals, no trailing zeros.
func formatAutodepositUSDC(amountRaw uint64) string {
	cents := (amountRaw + 5_000) / 10_000
	whole := cents / 100
	grouped := strconv.FormatUint(whole, 10)
	for end := len(grouped) - 3; end > 0; end -= 3 {
		grouped = grouped[:end] + "," + grouped[end:]
	}
	out := "$" + grouped
	switch fraction := cents % 100; {
	case fraction == 0:
	case fraction%10 == 0:
		out += "." + strconv.FormatUint(fraction/10, 10)
	default:
		out += fmt.Sprintf(".%02d", fraction)
	}
	return out
}
