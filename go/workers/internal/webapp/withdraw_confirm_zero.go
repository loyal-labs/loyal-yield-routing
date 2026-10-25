package webapp

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	withdrawSlotLagAttempts = 3
	// SLOT_LAG_RETRY_DELAY_MS.
	withdrawSlotLagDelay = 500 * time.Millisecond
	// EARN_FINAL_EXIT_IDLE_DUST_TOLERANCE_RAW
	withdrawFinalExitDustRaw = 10_000
)

func withdrawIsMinContextSlotError(err error) bool {
	// chain.failed folds the RPC -32016 "minimum context slot" error into
	// ErrBehind; that is the one retryable shape.
	return errors.Is(err, chain.ErrBehind)
}

func withdrawSlotLagRetry(ctx context.Context, sleep func(context.Context) error, read func() error) error {
	var lastErr error
	for attempt := 0; attempt < withdrawSlotLagAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx); err != nil {
				return err
			}
		}
		err := read()
		if err == nil {
			return nil
		}
		if !withdrawIsMinContextSlotError(err) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

type withdrawVaultSnapshot struct {
	Lamports      uint64
	ObservedSlot  uint64
	TokenAccounts []vaultRefundTokenAccount
	VaultPda      solana.PublicKey
	VaultUsdcATA  solana.PublicKey
}

// fetchWithdrawVaultSnapshot ports fetchEarnVaultRefundSnapshot
// with the minContextSlot the confirm proof passes.
// The SOL read folds a failure into zero (getVaultSweepLamportsOrZero,
// client.ts:3134-3146); the token-account reads fail the proof.
func fetchWithdrawVaultSnapshot(ctx context.Context, client withdrawConfirmChain, settings, usdcMint solana.PublicKey, minContextSlot uint64) (withdrawVaultSnapshot, error) {
	vault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
	if err != nil {
		return withdrawVaultSnapshot{}, err
	}
	vaultUsdcAta, err := spl.AssociatedTokenAddress(vault, usdcMint, solana.TokenProgramID)
	if err != nil {
		return withdrawVaultSnapshot{}, err
	}
	snapshot := withdrawVaultSnapshot{VaultPda: vault, VaultUsdcATA: vaultUsdcAta}
	if _, accounts, err := client.Accounts(ctx, []solana.PublicKey{vault}, rpc.CommitmentConfirmed, minContextSlot); err == nil && len(accounts) > 0 && accounts[0] != nil {
		snapshot.Lamports = accounts[0].Lamports
	}
	snapshot.ObservedSlot, snapshot.TokenAccounts, err = readWithdrawVaultTokenAccounts(ctx, client, vault, usdcMint, solana.TokenProgramID, minContextSlot)
	if err != nil {
		return withdrawVaultSnapshot{}, err
	}
	token2022Slot, token2022Accounts, err := readWithdrawVaultTokenAccounts(ctx, client, vault, usdcMint, solana.Token2022ProgramID, minContextSlot)
	if err != nil {
		return withdrawVaultSnapshot{}, err
	}
	snapshot.TokenAccounts = append(snapshot.TokenAccounts, token2022Accounts...)
	if token2022Slot < snapshot.ObservedSlot {
		snapshot.ObservedSlot = token2022Slot
	}
	sortVaultTokenAccounts(snapshot.TokenAccounts)
	return snapshot, nil
}

func readWithdrawVaultTokenAccounts(ctx context.Context, client withdrawConfirmChain, vault, usdcMint, program solana.PublicKey, minContextSlot uint64) (uint64, []vaultRefundTokenAccount, error) {
	slot, accounts, err := client.TokenAccounts(ctx, vault, program, rpc.CommitmentConfirmed, minContextSlot)
	if err != nil {
		return 0, nil, err
	}
	out := make([]vaultRefundTokenAccount, 0, len(accounts))
	for i := range accounts {
		decoded, err := decodeVaultTokenAccount(&accounts[i], usdcMint)
		if err != nil {
			return 0, nil, err
		}
		out = append(out, decoded)
	}
	return slot, out, nil
}

// withdrawBlockingTokenAccount mirrors the proof's blocking entry; the Next
// response serializes the proof objects as they are, tokenProgramId included.
type withdrawBlockingTokenAccount struct {
	Address        string `json:"address"`
	AmountRaw      string `json:"amountRaw"`
	Mint           string `json:"mint"`
	TokenProgramID string `json:"tokenProgramId"`
}

// withdrawFullExitZeroProof is EarnFullExitZeroProof: the withdraw confirm
// response serializes all but cleanupTokenAccounts, which the cleanup prepare
// feeds back into its builder.
type withdrawFullExitZeroProof struct {
	BlockingTokenAccounts []withdrawBlockingTokenAccount
	CleanupTokenAccounts  []withdrawCleanupTokenAccount
	ObservedSlot          string
	RemainingHoldings     []earnprep.WithdrawHolding
	Status                string
}

func classifyWithdrawZeroProof(cluster string, holdingsSnapshot earnprep.WithdrawSnapshot, vaultSnapshot withdrawVaultSnapshot, minContextSlot uint64) (withdrawFullExitZeroProof, error) {
	if holdingsSnapshot.ObservedSlot < minContextSlot {
		return withdrawFullExitZeroProof{}, errors.New("Earn full-exit proof was observed before the withdrawal confirmation slot.")
	}
	if vaultSnapshot.ObservedSlot < minContextSlot {
		return withdrawFullExitZeroProof{}, errors.New("Earn vault token inventory was observed before the withdrawal confirmation slot.")
	}

	// A vault token account blocks policy close when it holds a non-dust
	// balance, or any positive balance of a mint that is not a canonical idle
	// product asset at the expected ATA and token program.
	idleAssetByTokenAccount := map[string]earnprep.WithdrawProductAsset{}
	for _, asset := range earnprep.WithdrawProductAssets(cluster) {
		ata, err := spl.AssociatedTokenAddress(vaultSnapshot.VaultPda, asset.Mint, asset.TokenProgram)
		if err != nil {
			return withdrawFullExitZeroProof{}, err
		}
		idleAssetByTokenAccount[ata.String()] = asset
	}
	var blocking []withdrawBlockingTokenAccount
	cleanup := []withdrawCleanupTokenAccount{}
	for _, account := range vaultSnapshot.TokenAccounts {
		asset, known := idleAssetByTokenAccount[account.Address.String()]
		if account.Amount > 0 && (!known || asset.Mint != account.Mint || asset.TokenProgram != account.Program || account.Amount >= withdrawFinalExitDustRaw) {
			blocking = append(blocking, withdrawBlockingTokenAccount{
				Address:        account.Address.String(),
				AmountRaw:      strconv.FormatUint(account.Amount, 10),
				Mint:           account.Mint.String(),
				TokenProgramID: account.Program.String(),
			})
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
	var remainingReserveHoldings int
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
	return withdrawFullExitZeroProof{
		BlockingTokenAccounts: blocking,
		CleanupTokenAccounts:  cleanup,
		ObservedSlot:          strconv.FormatUint(holdingsSnapshot.ObservedSlot, 10),
		RemainingHoldings:     remaining,
		Status:                status,
	}, nil
}

func verifyWithdrawFullExitZeroBalances(ctx context.Context, client withdrawConfirmChain, sleep func(context.Context) error, now func() time.Time, cluster string, in withdrawCanonicalInput, minContextSlot uint64) (withdrawFullExitZeroProof, error) {
	settings, err := withdrawPublicKeyInput(in.Settings)
	if err != nil {
		return withdrawFullExitZeroProof{}, err
	}
	return verifyFullExitZeroBalances(ctx, client, sleep, now, cluster, earnprep.WithdrawCanonicalUniverse(cluster), settings, minContextSlot)
}

func verifyFullExitZeroBalances(ctx context.Context, client withdrawConfirmChain, sleep func(context.Context) error, now func() time.Time, cluster string, policy *earnprep.WithdrawPolicyUniverse, settings solana.PublicKey, minContextSlot uint64) (withdrawFullExitZeroProof, error) {
	var proof withdrawFullExitZeroProof
	usdcMint := earnUSDCMint(LoyalCluster(cluster))
	err := withdrawSlotLagRetry(ctx, sleep, func() error {
		holdings, err := earnprep.FetchWithdrawHoldingsSnapshot(ctx, client, earnprep.WithdrawHoldingsArgs{
			Cluster:                     cluster,
			MinContextSlot:              minContextSlot,
			Now:                         now,
			Policy:                      policy,
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
		proof, err = classifyWithdrawZeroProof(cluster, holdings, vaultSnapshot, minContextSlot)
		return err
	})
	return proof, err
}
