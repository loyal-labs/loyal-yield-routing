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
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/yield-optimization/withdrawals/cleanup/prepare ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/
// cleanup/prepare/route.ts plus prepareEarnUsdcCleanup (client.ts:10168-10331)
// and the zero-proof read it depends on. The route reads the Earn policy pair,
// proves the vault is at a full exit, then hands the client one
// execute-per-operation instruction list; it never writes money rows. The one
// DB write (a stale autodeposit row reconciled closed) is idempotent by
// policy account.

// cleanupProjectionRetries is PROJECTION_RETRY_DELAYS_MS (route.ts:33).
var cleanupProjectionRetries = []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

// findCleanupVaultState ports findEarnCleanupVaultState
// (yield-deposit-repository.server.ts:3153-3239) down to the rows the cleanup
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
		settings, int16(earnVaultIndex), vault.String(), includeInactive).Scan(&vaultID, &activePolicyID, &setupPolicyID)
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
			settings, int16(earnVaultIndex), vault.String(), authority, id, includeInactive).Scan(&p.ID, &p.Cluster, &p.PolicySeed, &p.PolicyAccount, &p.DelegatedSigners, &p.Threshold, &p.RouteModes,
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

// cleanupLatestFullWithdrawalSlot ports findLatestFullYieldWithdrawalForVault
// (yield-deposit-repository.server.ts:3241-3266): the newest full-mode
// withdrawal's confirmed slot, 0 when the projection has not landed.
func (h earnRoutes) cleanupLatestFullWithdrawalSlot(ctx context.Context, account EarnAccount) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var slot int64
	err := h.YieldDB.QueryRow(ctx, `SELECT confirmed_slot
FROM loyal_yield.user_yield_position_withdrawals
WHERE mode = 'full' AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3 AND wallet_address = $4
ORDER BY confirmed_slot DESC, id DESC LIMIT 1`,
		account.Settings.String(), int16(earnVaultIndex), account.Vault.String(), account.Wallet.String()).Scan(&slot)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read latest full withdrawal: %w", err)
	}
	return slot, nil
}

// cleanupZeroProof is EarnFullExitZeroProof with the cleanupTokenAccounts the
// prepare route feeds back into the builder (route.ts:269-275); the confirm
// route drops them.
type cleanupZeroProof struct {
	BlockingTokenAccounts []withdrawBlockingTokenAccount
	CleanupTokenAccounts  []withdrawCleanupTokenAccount
	RemainingHoldings     []earnprep.WithdrawHolding
	Status                string
}

// withdrawCleanupTokenAccount is the proof's cleanup entry (earn-full-exit-
// zero-proof.server.ts:183-191): decimals is the literal 6 the TS pins.
type withdrawCleanupTokenAccount struct {
	Address        string
	AmountRaw      string
	Decimals       uint8
	Mint           string
	TokenProgramID string
}

// classifyCleanupZeroProof ports classifyZeroProof
// (earn-full-exit-zero-proof.server.ts:124-206) with the cleanup list: a
// vault token account blocks policy close when it holds a non-dust balance or
// any balance of a mint that is not a canonical idle product asset at the
// expected ATA and token program; everything unblocked is cleanup material.
func classifyCleanupZeroProof(holdingsSnapshot earnprep.WithdrawSnapshot, vaultSnapshot withdrawVaultSnapshot, minContextSlot uint64) (cleanupZeroProof, error) {
	if holdingsSnapshot.ObservedSlot < minContextSlot {
		return cleanupZeroProof{}, errors.New("Earn full-exit proof was observed before the withdrawal confirmation slot.")
	}
	if vaultSnapshot.ObservedSlot < minContextSlot {
		return cleanupZeroProof{}, errors.New("Earn vault token inventory was observed before the withdrawal confirmation slot.")
	}
	idleAssetByTokenAccount := map[string]earnprep.WithdrawProductAsset{}
	for _, asset := range earnprep.WithdrawProductAssets("mainnet-beta") {
		ata, err := spl.AssociatedTokenAddress(vaultSnapshot.VaultPda, asset.Mint, asset.TokenProgram)
		if err != nil {
			return cleanupZeroProof{}, err
		}
		idleAssetByTokenAccount[ata.String()] = asset
	}
	var blocking []withdrawBlockingTokenAccount
	blocked := map[string]bool{}
	for _, account := range vaultSnapshot.TokenAccounts {
		if account.Amount == 0 {
			continue
		}
		asset, known := idleAssetByTokenAccount[account.Address.String()]
		if !known || asset.Mint != account.Mint || asset.TokenProgram != account.Program || account.Amount >= withdrawFinalExitDustRaw {
			blocking = append(blocking, withdrawBlockingTokenAccount{
				Address:        account.Address.String(),
				AmountRaw:      strconv.FormatUint(account.Amount, 10),
				Mint:           account.Mint.String(),
				TokenProgramID: account.Program.String(),
			})
			blocked[account.Address.String()] = true
		}
	}
	var cleanup []withdrawCleanupTokenAccount
	for _, account := range vaultSnapshot.TokenAccounts {
		if blocked[account.Address.String()] {
			continue
		}
		cleanup = append(cleanup, withdrawCleanupTokenAccount{
			Address:        account.Address.String(),
			AmountRaw:      strconv.FormatUint(account.Amount, 10),
			Decimals:       6,
			Mint:           account.Mint.String(),
			TokenProgramID: account.Program.String(),
		})
	}
	remainingReserveHoldings := 0
	for _, holding := range holdingsSnapshot.Holdings {
		if holding.Kind == "kamino" && holding.AmountRaw > 0 {
			remainingReserveHoldings++
		}
	}
	status := "full_exit_incomplete"
	if remainingReserveHoldings == 0 && len(blocking) == 0 {
		status = "policy_close_required"
	}
	remaining := make([]earnprep.WithdrawHolding, 0, len(holdingsSnapshot.Holdings))
	for _, holding := range holdingsSnapshot.Holdings {
		if holding.AmountRaw > 0 {
			remaining = append(remaining, holding)
		}
	}
	if cleanup == nil {
		cleanup = []withdrawCleanupTokenAccount{}
	}
	return cleanupZeroProof{
		BlockingTokenAccounts: blocking,
		CleanupTokenAccounts:  cleanup,
		RemainingHoldings:     remaining,
		Status:                status,
	}, nil
}

// cleanupPolicyUniverse ports resolveFullExitPolicyMetadata
// (earn-full-exit-zero-proof.server.ts:58-75): the DB policy's mints and
// markets, with the canonical product mints and the Safe basket filling a
// null list.
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
		VaultIndex:           earnprep.WithdrawVaultIndex,
		StableMints:          stable,
		KaminoLiquidityMints: liquidity,
		KaminoMarkets:        policyMarkets,
	}
}

// verifyCleanupFullExitZeroBalances ports verifyEarnFullExitZeroBalances
// (earn-full-exit-zero-proof.server.ts:208-285) for the cleanup routes: the
// holdings snapshot (through the DB policy's universe) and the vault token
// inventory under one slot-lag retry, then the classification.
func verifyCleanupFullExitZeroBalances(ctx context.Context, client withdrawConfirmChain, now func() time.Time, cluster string, route RoutePolicy, settings solana.PublicKey, minContextSlot uint64) (cleanupZeroProof, error) {
	var proof cleanupZeroProof
	usdcMint := earnUSDCMint(LoyalCluster(cluster))
	err := withdrawSlotLagRetry(ctx, withdrawSleep, func() error {
		holdings, err := earnprep.FetchWithdrawHoldingsSnapshot(ctx, client, earnprep.WithdrawHoldingsArgs{
			Cluster:                     cluster,
			MinContextSlot:              minContextSlot,
			Now:                         now,
			Policy:                      cleanupPolicyUniverse(cluster, route),
			SettingsPda:                 settings,
			RequireCompleteReserveReads: true,
		})
		if err != nil {
			return err
		}
		vaultSnapshot, err := fetchWithdrawVaultSnapshot(ctx, client, settings, usdcMint, minContextSlot)
		if err != nil {
			return err
		}
		proof, err = classifyCleanupZeroProof(holdings, vaultSnapshot, minContextSlot)
		return err
	})
	return proof, err
}

// cleanupVaultSweepLamports ports getVaultSweepLamportsOrZero
// (client.ts:3134-3146): the vault PDA's SOL, every failure folded to zero.
func cleanupVaultSweepLamports(ctx context.Context, client *chain.Client, vault solana.PublicKey) uint64 {
	_, accounts, err := client.Accounts(ctx, []solana.PublicKey{vault}, rpc.CommitmentConfirmed, 0)
	if err != nil || len(accounts) == 0 || accounts[0] == nil {
		return 0
	}
	return accounts[0].Lamports
}

// cleanupVaultTokenAccount mirrors the vaultTokenAccounts entries the prepare
// route feeds the builder (route.ts:269-275).
type cleanupVaultTokenAccount struct {
	Address        solana.PublicKey
	AmountRaw      uint64
	Decimals       uint8
	Mint           solana.PublicKey
	TokenProgramID solana.PublicKey
}

// cleanupAutodepositCloseInput is the route's autodepositClose pair
// (route.ts:214-219, 234-239).
type cleanupAutodepositCloseInput struct {
	Policy              solana.PublicKey
	RecurringDelegation solana.PublicKey
}

// cleanupCloseInput is SmartAccountEarnUsdcCleanupInput (route.ts:263-289).
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

// buildEarnUsdcCleanup ports prepareEarnUsdcCleanup (client.ts:10168-10331):
// per-account idle transfers and closes wrapped in one member-sync execution,
// the vault's SOL swept, the live yield-routing policies removed by one
// settings sync, and the autodeposit close riding ahead when one is open.
// Every failure is a plain error; the route folds them into 500
// prepare_failed exactly like the Next catch-all.
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
			AccountIndex: earnVaultIndex,
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
		VaultIndex:            earnVaultIndex,
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
			AccountIndex: earnVaultIndex,
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

// cleanupCloseError marks the two plain builder errors the TS messages name,
// so the route can answer 500 prepare_failed with the exact text.
type cleanupCloseError struct{ err error }

func (e cleanupCloseError) Error() string { return e.err.Error() }

// buildCleanupPolicyCloseOperation ports prepareCloseLiveYieldRoutingPoliciesSync
// (client.ts:11868-11886) and its precheck: keep the policies that still
// exist on chain, refuse when none do (the caller sees a nil operation), then
// check each live policy's owner and kind through fetchPolicy
// (client.ts:12188-12212) before removing them with one settings sync.
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
	for i, info := range liveInfos {
		decoded, decodeErr := squads.DecodeProgramInteractionPolicy(info)
		if decodeErr != nil {
			if decodeErr.Error() == "policy is not ProgramInteraction" {
				return nil, cleanupCloseError{errors.New("Yield routing cleanup only accepts program-interaction policies.")}
			}
			return nil, decodeErr
		}
		if decoded.Settings != in.SettingsPda {
			return nil, cleanupCloseError{errors.New("Policy belongs to another vault.")}
		}
		if live[i] != in.RoutePolicyAccount && in.SetupPolicyAccount != nil && live[i] != *in.SetupPolicyAccount {
			return nil, cleanupCloseError{errors.New("Policy belongs to another vault.")}
		}
	}
	remove, err := earnprep.WithdrawPolicyRemoveSync(in.SettingsPda, in.FeePayer, in.Wallet, live)
	if err != nil {
		return nil, err
	}
	return &remove, nil
}

// cleanupIsSubscriptionSweepPolicy ports isSubscriptionSweepPolicy
// (client.ts:4461-4473): a ProgramInteraction policy on the Earn vault whose
// every instruction constraint targets the subscriptions program.
func cleanupIsSubscriptionSweepPolicy(decoded squads.DecodedPolicy) bool {
	if decoded.VaultIndex != earnVaultIndex || len(decoded.Constraints) == 0 {
		return false
	}
	for _, constraint := range decoded.Constraints {
		if constraint.ProgramID != autodepositSubscriptionsProgram {
			return false
		}
	}
	return true
}

// buildAutodepositClosePrepared ports prepareEarnUsdcAutodepositClose
// (client.ts:11523-11621): the sweep policy is verified on chain, removed by
// one settings sync, the recurring delegation revoked when it exists, and the
// wallet's USDC delegate approval revoked when it is still ours.
func buildAutodepositClosePrepared(ctx context.Context, client *chain.Client, in cleanupCloseInput) (*EarnCleanupAutodepositCloseWire, error) {
	close_ := in.AutodepositClose
	_, infos, err := client.Accounts(ctx, []solana.PublicKey{close_.Policy}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	if infos[0] != nil {
		decoded, err := squads.DecodeProgramInteractionPolicy(infos[0])
		if err != nil || !cleanupIsSubscriptionSweepPolicy(decoded) {
			return nil, cleanupCloseError{errors.New("Refusing to close a policy that is not an Autodeposit sweep policy.")}
		}
		if decoded.Settings != in.SettingsPda {
			return nil, cleanupCloseError{errors.New("Policy belongs to another vault.")}
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
			Data: []byte{autodepositDelegationDiscriminator},
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
			VaultIndex:          earnVaultIndex,
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
	wire.Vault.AccountIndex = earnVaultIndex
	wire.Vault.Pubkey = in.Vault.String()
	return wire, nil
}
