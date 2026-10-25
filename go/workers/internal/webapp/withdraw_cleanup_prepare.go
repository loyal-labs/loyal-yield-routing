package webapp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Never writes money rows; the one DB write (a stale autodeposit row
// reconciled closed) is idempotent by policy account.

var cleanupProjectionRetries = []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

// findCleanupVaultState ports findEarnCleanupVaultState
// down to the rows the cleanup
// routes read: the vault's active (or recorded, when includeInactive) policy
// pair. The idle and reserve row lists the TS also selects are unused by both
// routes, so the read stops at the policies. Nil means the TS null.
func (h earnRoutes) findCleanupVaultState(ctx context.Context, includeInactive bool, authority, settings string, vault solana.PublicKey) (*RoutePolicyPair, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var vaultID, activePolicyID int64
	var setupPolicyID *int64
	err := h.YieldDB.QueryRow(ctx, `SELECT id, active_policy_id, setup_policy_id
FROM loyal_yield.managed_vaults
WHERE (`+"$4::boolean OR active"+`) AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3
ORDER BY last_seen_at DESC, id DESC LIMIT 1`,
		settings, int16(earnprep.EarnVaultIndex), vault.String(), includeInactive).Scan(&vaultID, &activePolicyID, &setupPolicyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read earn cleanup vault: %w", err)
	}
	policyBy := func(id int64) (*RoutePolicy, error) {
		var p RoutePolicy
		err := h.YieldDB.QueryRow(ctx, `SELECT id, cluster, policy_seed, policy_account, delegated_signers, threshold, route_modes,
  stable_mints, kamino_markets, kamino_liquidity_mints, universe_preset, risk_profile,
  swap_lanes, first_seen_at, last_seen_at, last_seen_slot, last_seen_signature
FROM loyal_yield.route_policies
WHERE ($6::boolean OR active) AND authority = $4 AND id = $5 AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3`,
			settings, int16(earnprep.EarnVaultIndex), vault.String(), authority, id, includeInactive).Scan(&p.ID, &p.Cluster, &p.PolicySeed, &p.PolicyAccount, &p.DelegatedSigners, &p.Threshold, &p.RouteModes,
			&p.StableMints, &p.KaminoMarkets, &p.KaminoLiquidityMints, &p.UniversePreset, &p.RiskProfile,
			&p.SwapLanes, &p.FirstSeenAt, &p.LastSeenAt, &p.LastSeenSlot, &p.LastSeenSignature)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read earn cleanup policy: %w", err)
		}
		return &p, nil
	}
	route, err := policyBy(activePolicyID)
	if err != nil || route == nil {
		return nil, err
	}
	var setup *RoutePolicy
	if setupPolicyID != nil {
		setup, err = policyBy(*setupPolicyID)
		if err != nil {
			return nil, err
		}
	}
	return &RoutePolicyPair{Route: *route, Setup: setup}, nil
}

func (h earnRoutes) cleanupLatestFullWithdrawalSlot(ctx context.Context, account EarnAccount) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var slot int64
	err := h.YieldDB.QueryRow(ctx, `SELECT confirmed_slot
FROM loyal_yield.user_yield_position_withdrawals
WHERE mode = 'full' AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3 AND wallet_address = $4
ORDER BY confirmed_slot DESC, id DESC LIMIT 1`,
		account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Vault.String(), account.Wallet.String()).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read latest full withdrawal: %w", err)
	}
	return slot, nil
}

type withdrawCleanupTokenAccount struct {
	Address        string
	AmountRaw      string
	Decimals       uint8
	Mint           string
	TokenProgramID string
}

func cleanupPolicyUniverse(cluster string, route RoutePolicy) *earnprep.WithdrawPolicyUniverse {
	canonical := earnprep.WithdrawCanonicalUniverse(cluster)
	stable, liquidity, policyMarkets := route.StableMints, route.KaminoLiquidityMints, route.KaminoMarkets
	if stable == nil {
		stable = canonical.StableMints
	}
	if liquidity == nil {
		liquidity = canonical.KaminoLiquidityMints
	}
	if policyMarkets == nil {
		policyMarkets = canonical.KaminoMarkets
	}
	return &earnprep.WithdrawPolicyUniverse{
		VaultIndex:           earnprep.EarnVaultIndex,
		StableMints:          stable,
		KaminoLiquidityMints: liquidity,
		KaminoMarkets:        policyMarkets,
	}
}

// cleanupVaultSweepLamports returns the vault PDA's SOL, every failure folded
// to zero.
func cleanupVaultSweepLamports(ctx context.Context, client *chain.Client, vault solana.PublicKey) uint64 {
	_, accounts, err := client.Accounts(ctx, []solana.PublicKey{vault}, rpc.CommitmentConfirmed, 0)
	if err != nil || len(accounts) == 0 || accounts[0] == nil {
		return 0
	}
	return accounts[0].Lamports
}

type cleanupVaultTokenAccount struct {
	Address        solana.PublicKey
	AmountRaw      uint64
	Decimals       uint8
	Mint           solana.PublicKey
	TokenProgramID solana.PublicKey
}

type cleanupAutodepositCloseInput struct {
	Policy              solana.PublicKey
	RecurringDelegation solana.PublicKey
}

type cleanupCloseInput struct {
	Cluster            string
	FeePayer           solana.PublicKey
	PolicySigner       solana.PublicKey
	SettingsPda        solana.PublicKey
	Vault              solana.PublicKey
	Wallet             solana.PublicKey
	VaultTokenAccounts []cleanupVaultTokenAccount
	RoutePolicyAccount solana.PublicKey
	RoutePolicySeed    uint64
	SetupPolicyAccount *solana.PublicKey
	SetupPolicySeed    *uint64
	AutodepositClose   *cleanupAutodepositCloseInput
}

func buildEarnUsdcCleanup(ctx context.Context, client *chain.Client, in cleanupCloseInput) (*EarnCleanupPrepareWire, error) {
	usdcMint := earnUSDCMint(LoyalCluster(in.Cluster))
	vaultUsdcAta, err := spl.AssociatedTokenAddress(in.Vault, usdcMint, solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	var tokenInstructions, walletAtaInstructions []squads.Instruction
	var idleUsdcTransferRaw uint64
	for _, tokenAccount := range in.VaultTokenAccounts {
		if tokenAccount.TokenProgramID != solana.TokenProgramID && tokenAccount.TokenProgramID != solana.Token2022ProgramID {
			return nil, errors.New("Earn cleanup received an unsupported token program.")
		}
		if tokenAccount.AmountRaw > 0 {
			walletAta, err := spl.AssociatedTokenAddress(in.Wallet, tokenAccount.Mint, tokenAccount.TokenProgramID)
			if err != nil {
				return nil, err
			}
			create, err := squads.InstructionOf(spl.CreateIdempotentATA(in.FeePayer, in.Wallet, tokenAccount.Mint, tokenAccount.TokenProgramID))
			if err != nil {
				return nil, err
			}
			walletAtaInstructions = append(walletAtaInstructions, create)
			transfer, err := vaultTransferCheckedInner(tokenAccount.Address, tokenAccount.Mint, walletAta, in.Vault, tokenAccount.AmountRaw, tokenAccount.Decimals)
			if err != nil {
				return nil, err
			}
			tokenInstructions = append(tokenInstructions, transfer)
			if tokenAccount.Mint == usdcMint {
				idleUsdcTransferRaw += tokenAccount.AmountRaw
			}
		}
		tokenInstructions = append(tokenInstructions, earnprep.WithdrawCloseAccount(tokenAccount.TokenProgramID, tokenAccount.Address, in.Wallet, in.Vault))
	}
	shouldCloseVaultUsdcAta := false
	for _, tokenAccount := range in.VaultTokenAccounts {
		if tokenAccount.Address == vaultUsdcAta {
			shouldCloseVaultUsdcAta = true
			break
		}
	}
	if sweep := cleanupVaultSweepLamports(ctx, client, in.Vault); sweep > 0 {
		transfer, err := vaultSystemTransferInner(in.Vault, in.Wallet, sweep)
		if err != nil {
			return nil, err
		}
		tokenInstructions = append(tokenInstructions, transfer)
	}
	var tokenOperation *squads.Instruction
	if len(tokenInstructions) > 0 {
		execute, err := earnprep.ExecuteMemberSync(earnprep.MemberSync{
			Settings:     in.SettingsPda,
			Vault:        in.Vault,
			AccountIndex: earnprep.EarnVaultIndex,
			Members:      []solana.PublicKey{in.Wallet},
			Inner:        tokenInstructions,
		})
		if err != nil {
			return nil, err
		}
		tokenOperation = &execute
	}

	policyOperation, err := buildCleanupPolicyCloseOperation(ctx, client, in)
	if err != nil {
		return nil, err
	}
	var autodepositClosePrepared *EarnCleanupAutodepositCloseWire
	if in.AutodepositClose != nil {
		autodepositClosePrepared, err = buildAutodepositClosePrepared(ctx, client, in)
		if err != nil {
			return nil, err
		}
	}

	if tokenOperation == nil && policyOperation == nil {
		return nil, errors.New("Nothing to clean up: yield routing policies are already closed.")
	}
	operation := "earnUsdcCleanup"
	var instructions []WirePreparedInstruction
	for _, ix := range walletAtaInstructions {
		instructions = append(instructions, wirePreparedInstruction(ix))
	}
	if tokenOperation != nil {
		instructions = append(instructions, wirePreparedInstruction(*tokenOperation))
	}
	if policyOperation != nil {
		instructions = append(instructions, wirePreparedInstruction(*policyOperation))
	}
	persistence := EarnCleanupPersistenceWire{
		Cluster:               in.Cluster,
		WalletAddress:         in.Wallet.String(),
		DelegatedSigner:       in.PolicySigner.String(),
		Settings:              in.SettingsPda.String(),
		VaultIndex:            earnprep.EarnVaultIndex,
		VaultPubkey:           in.Vault.String(),
		PolicyID:              strconv.FormatUint(in.RoutePolicySeed, 10),
		PolicyAccount:         in.RoutePolicyAccount.String(),
		PolicySeed:            strconv.FormatUint(in.RoutePolicySeed, 10),
		IdleTransferAmountRaw: strconv.FormatUint(idleUsdcTransferRaw, 10),
		ClosedVaultUsdcAta:    shouldCloseVaultUsdcAta,
		ClosedCollateralAtas:  []string{},
		AutodepositClose:      nil,
	}
	if autodepositClosePrepared != nil {
		closeCopy := autodepositClosePrepared.Persistence
		persistence.AutodepositClose = &closeCopy
	}
	for _, tokenAccount := range in.VaultTokenAccounts {
		if tokenAccount.Address != vaultUsdcAta {
			persistence.ClosedCollateralAtas = append(persistence.ClosedCollateralAtas, tokenAccount.Address.String())
		}
	}
	wire := &EarnCleanupPrepareWire{
		AutodepositClosePrepared: autodepositClosePrepared,
		Persistence:              persistence,
		Policy: EarnCleanupPolicyWire{
			Account: in.RoutePolicyAccount.String(),
			ID:      strconv.FormatUint(in.RoutePolicySeed, 10),
			Seed:    strconv.FormatUint(in.RoutePolicySeed, 10),
		},
		Prepared: WirePreparedOperation{
			Instructions:         instructions,
			LookupTableAccounts:  []WireAddressLookupTable{},
			Operation:            operation,
			Payer:                in.FeePayer.String(),
			ProgramID:            squads.ProgramID.String(),
			RequiresConfirmation: true,
		},
		Vault: EarnCleanupVaultWire{
			AccountIndex: earnprep.EarnVaultIndex,
			Pubkey:       in.Vault.String(),
			UsdcAta:      vaultUsdcAta.String(),
		},
	}
	if in.SetupPolicyAccount != nil && in.SetupPolicySeed != nil {
		setupSeed := strconv.FormatUint(*in.SetupPolicySeed, 10)
		wire.SetupPolicy = &EarnCleanupPolicyWire{
			Account: in.SetupPolicyAccount.String(),
			ID:      setupSeed,
			Seed:    setupSeed,
		}
		wire.Persistence.SetupPolicyID = setupSeed
		wire.Persistence.SetupPolicyAccount = in.SetupPolicyAccount.String()
		wire.Persistence.SetupPolicySeed = setupSeed
	}
	return wire, nil
}

func buildCleanupPolicyCloseOperation(ctx context.Context, client *chain.Client, in cleanupCloseInput) (*squads.Instruction, error) {
	policies := []solana.PublicKey{in.RoutePolicyAccount}
	if in.SetupPolicyAccount != nil {
		policies = append(policies, *in.SetupPolicyAccount)
	}
	deduped := make([]solana.PublicKey, 0, len(policies))
	for _, policy := range policies {
		if !slices.Contains(deduped, policy) {
			deduped = append(deduped, policy)
		}
	}
	_, infos, err := client.Accounts(ctx, deduped, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	var live []solana.PublicKey
	var liveInfos []*chain.Account
	for i, info := range infos {
		if info != nil {
			live = append(live, deduped[i])
			liveInfos = append(liveInfos, info)
		}
	}
	if len(live) == 0 {
		return nil, nil
	}
	for _, info := range liveInfos {
		decoded, decodeErr := squads.DecodeProgramInteractionPolicy(info)
		if decodeErr != nil && !errors.Is(decodeErr, squads.ErrNotProgramInteraction) {
			return nil, decodeErr
		}
		if decoded.Settings != in.SettingsPda {
			return nil, errors.New("Policy belongs to another vault.")
		}
		if decodeErr != nil {
			return nil, errors.New("Yield routing cleanup only accepts program-interaction policies.")
		}
	}
	remove, err := earnprep.WithdrawPolicyRemoveSync(in.SettingsPda, in.FeePayer, in.Wallet, live)
	if err != nil {
		return nil, err
	}
	return &remove, nil
}

func cleanupIsSubscriptionSweepPolicy(decoded squads.DecodedPolicy) bool {
	if decoded.VaultIndex != earnprep.EarnVaultIndex || len(decoded.Constraints) == 0 {
		return false
	}
	for _, constraint := range decoded.Constraints {
		if constraint.ProgramID != autodepositSubscriptionsProgram {
			return false
		}
	}
	return true
}

func buildAutodepositClosePrepared(ctx context.Context, client *chain.Client, in cleanupCloseInput) (*EarnCleanupAutodepositCloseWire, error) {
	close_ := in.AutodepositClose
	_, infos, err := client.Accounts(ctx, []solana.PublicKey{close_.Policy}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	if infos[0] != nil {
		decoded, err := squads.DecodeProgramInteractionPolicy(infos[0])
		if err != nil || !cleanupIsSubscriptionSweepPolicy(decoded) {
			return nil, errors.New("Refusing to close a policy that is not an Autodeposit sweep policy.")
		}
		if decoded.Settings != in.SettingsPda {
			return nil, errors.New("Policy belongs to another vault.")
		}
	}
	closePolicy, err := earnprep.WithdrawPolicyRemoveSync(in.SettingsPda, in.FeePayer, in.Wallet, []solana.PublicKey{close_.Policy})
	if err != nil {
		return nil, err
	}
	_, delegationInfos, err := client.Accounts(ctx, []solana.PublicKey{close_.RecurringDelegation}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	usdcMint := earnUSDCMint(LoyalCluster(in.Cluster))
	walletUsdcAta, err := spl.AssociatedTokenAddress(in.Wallet, usdcMint, solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	subscriptionAuthority := autodepositSubscriptionAuthority(in.Wallet, usdcMint)
	_, ataInfos, err := client.Accounts(ctx, []solana.PublicKey{walletUsdcAta}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	revokeTokenDelegate := false
	if ataInfo := ataInfos[0]; ataInfo != nil &&
		len(ataInfo.Data) >= autodepositTokenDelegatedAmountOffset+8 &&
		binary.LittleEndian.Uint32(ataInfo.Data[autodepositTokenDelegateTagOffset:autodepositTokenDelegateTagOffset+4]) == 1 &&
		solana.PublicKeyFromBytes(ataInfo.Data[autodepositTokenDelegateOffset:autodepositTokenDelegateOffset+32]) == subscriptionAuthority {
		revokeTokenDelegate = true
	}
	var instructions []WirePreparedInstruction
	if delegationInfos[0] != nil {
		revoke := squads.Instruction{
			ProgramID: autodepositSubscriptionsProgram,
			Accounts: []solana.AccountMeta{
				{PublicKey: in.Wallet, IsSigner: true, IsWritable: true},
				{PublicKey: close_.RecurringDelegation, IsWritable: true},
			},
			Data: []byte{earncatalog.DelegationDiscriminator},
		}
		instructions = append(instructions, wirePreparedInstruction(revoke))
	}
	instructions = append(instructions, wirePreparedInstruction(closePolicy))
	if revokeTokenDelegate {
		instructions = append(instructions, wirePreparedInstruction(squads.Instruction{
			ProgramID: solana.TokenProgramID,
			Accounts: []solana.AccountMeta{
				{PublicKey: walletUsdcAta, IsWritable: true},
				{PublicKey: in.Wallet, IsSigner: true},
			},
			Data: []byte{4},
		}))
	}
	wire := &EarnCleanupAutodepositCloseWire{
		Persistence: EarnCleanupClosePersistenceWire{
			Cluster:             in.Cluster,
			WalletAddress:       in.Wallet.String(),
			DelegatedSigner:     in.PolicySigner.String(),
			Settings:            in.SettingsPda.String(),
			VaultIndex:          earnprep.EarnVaultIndex,
			VaultPubkey:         in.Vault.String(),
			PolicyAccount:       close_.Policy.String(),
			RecurringDelegation: close_.RecurringDelegation.String(),
		},
		Prepared: WirePreparedOperation{
			Instructions:         instructions,
			LookupTableAccounts:  []WireAddressLookupTable{},
			Operation:            "earnUsdcAutodepositClose",
			Payer:                in.FeePayer.String(),
			ProgramID:            squads.ProgramID.String(),
			RequiresConfirmation: true,
		},
	}
	wire.Policy.Account = close_.Policy.String()
	wire.Subscription.RecurringDelegation = close_.RecurringDelegation.String()
	wire.Vault.AccountIndex = earnprep.EarnVaultIndex
	wire.Vault.Pubkey = in.Vault.String()
	return wire, nil
}
