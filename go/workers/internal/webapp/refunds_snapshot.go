package webapp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

// Ports web/packages/smart-account-vaults/src/client.ts:10339-10422
// (fetchEarnVaultRefundSnapshot) and web/apps/web/src/lib/yield-optimization/
// earn-policy-refund-state.server.ts:711-752 (findEarnVaultRefundDbState).

const tokenAccountDataLen = 165

var (
	mainnetUSDCMint = solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	devnetUSDCMint  = solana.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
)

func earnUSDCMint(cluster LoyalCluster) solana.PublicKey {
	if cluster == LoyalDevnet {
		return devnetUSDCMint
	}
	return mainnetUSDCMint
}

// base58CollationOrder is the order JavaScript's localeCompare puts the 58
// base58 characters in (ICU root collation: primary weights first, then case
// with lowercase before uppercase — verified against the TS runtime). Byte
// order differs, e.g. 'H' < 'a' in bytes but 'a' < 'H' here, and the wire
// sort feeds both the response array and the close-account order.
const base58CollationOrder = "123456789aAbBcCdDeEfFgGhHijJkKLmMnNopPqQrRsStTuUvVwWxXyYzZ"

var base58CollationRank = func() map[byte]int {
	ranks := make(map[byte]int, len(base58CollationOrder))
	for i := 0; i < len(base58CollationOrder); i++ {
		ranks[base58CollationOrder[i]] = i
	}
	return ranks
}()

// collationLess compares base58 strings the way localeCompare does: the first
// differing position decides, prefix sorts first. Any character outside the
// alphabet cannot come from a base58 pubkey and falls back to byte order.
func collationLess(left, right string) bool {
	n := min(len(left), len(right))
	for i := 0; i < n; i++ {
		l, r := base58CollationRank[left[i]], base58CollationRank[right[i]]
		if l != r {
			return l < r
		}
	}
	return len(left) < len(right)
}

type vaultRefundTokenAccount struct {
	Address  solana.PublicKey
	Amount   uint64
	IsUsdc   bool
	Lamports uint64
	Mint     solana.PublicKey
	Program  solana.PublicKey
}

type earnVaultRefundSnapshot struct {
	Lamports      uint64
	TokenAccounts []vaultRefundTokenAccount
	Vault         solana.PublicKey
	VaultUsdcATA  solana.PublicKey
}

// fetchEarnVaultRefundSnapshot ports client.ts:10339-10422: the vault PDA's
// confirmed balance plus its token accounts under both token programs, sorted
// in localeCompare order of the base58 address.
func fetchEarnVaultRefundSnapshot(ctx context.Context, client *chain.Client, vault solana.PublicKey, usdcMint solana.PublicKey) (earnVaultRefundSnapshot, error) {
	vaultUsdcAta, err := spl.AssociatedTokenAddress(vault, usdcMint, solana.TokenProgramID)
	if err != nil {
		return earnVaultRefundSnapshot{}, err
	}
	snapshot := earnVaultRefundSnapshot{Vault: vault, VaultUsdcATA: vaultUsdcAta}

	// getVaultSweepLamportsOrZero (client.ts:3134-3146) folds a getBalance
	// failure into zero; here a missing account is zero and a failed read is an
	// error, so an RPC outage never reports a refundable vault as empty.
	_, accounts, err := client.Accounts(ctx, []solana.PublicKey{vault}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return earnVaultRefundSnapshot{}, fmt.Errorf("read Earn vault balance: %w", err)
	}
	if accounts[0] != nil {
		snapshot.Lamports = accounts[0].Lamports
	}

	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		_, tokenAccounts, err := client.TokenAccounts(ctx, vault, program, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return earnVaultRefundSnapshot{}, fmt.Errorf("read Earn vault token accounts: %w", err)
		}
		for i := range tokenAccounts {
			decoded, err := decodeVaultTokenAccount(&tokenAccounts[i], usdcMint)
			if err != nil {
				return earnVaultRefundSnapshot{}, err
			}
			snapshot.TokenAccounts = append(snapshot.TokenAccounts, decoded)
		}
	}
	// client.ts:10407-10409 sorts by address.toBase58().localeCompare.
	// Stable sort keeps the token-program accounts ahead of token-2022 ones on
	// the (impossible) duplicate address, matching the TS concat order.
	sortVaultTokenAccounts(snapshot.TokenAccounts)
	return snapshot, nil
}

func sortVaultTokenAccounts(accounts []vaultRefundTokenAccount) {
	// Insertion sort: the vault holds a handful of stale collateral ATAs at
	// most, so the quadratic bound is free here.
	for i := 1; i < len(accounts); i++ {
		for j := i; j > 0 && collationLess(accounts[j].Address.String(), accounts[j-1].Address.String()); j-- {
			accounts[j], accounts[j-1] = accounts[j-1], accounts[j]
		}
	}
}

// decodeVaultTokenAccount reads the SPL token account layout the way
// AccountLayout.decode does (mint, amount, owner program); it does not
// validate the TS-forgiving option tags.
func decodeVaultTokenAccount(account *chain.Account, usdcMint solana.PublicKey) (vaultRefundTokenAccount, error) {
	if len(account.Data) < tokenAccountDataLen {
		return vaultRefundTokenAccount{}, fmt.Errorf("token account %s is %d bytes, shorter than a token account", account.Key, len(account.Data))
	}
	mint := solana.PublicKeyFromBytes(account.Data[0:32])
	return vaultRefundTokenAccount{
		Address:  account.Key,
		Amount:   binary.LittleEndian.Uint64(account.Data[64:72]),
		IsUsdc:   mint == usdcMint,
		Lamports: account.Lamports,
		Mint:     mint,
		Program:  account.Owner,
	}, nil
}

// vaultRefundDbState mirrors findEarnVaultRefundDbState
// (earn-policy-refund-state.server.ts:711-752). Any non-closed autodeposit
// target counts, including pending_delegation rows mid-setup.
type vaultRefundDbState struct {
	HasActivePosition     bool
	HasActiveManagedVault bool
	HasActiveAutodeposit  bool
}

func findEarnVaultRefundDbState(ctx context.Context, yieldDB *pgxpool.Pool, settings string) (vaultRefundDbState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var state vaultRefundDbState
	// user_yield_positions.status / managed_vaults.active /
	// balance_sweep_targets.chain_status are the columns the Drizzle read
	// filters on (yield-neon-client.server.ts:327-371, 283-308, 766-817).
	err := yieldDB.QueryRow(ctx, `SELECT
	  EXISTS (SELECT 1 FROM loyal_yield.user_yield_positions WHERE settings = $1 AND vault_index = $2 AND status = 'active'),
	  EXISTS (SELECT 1 FROM loyal_yield.managed_vaults WHERE active AND settings = $1 AND vault_index = $2),
	  EXISTS (SELECT 1 FROM loyal_yield.balance_sweep_targets WHERE settings = $1 AND vault_index = $2 AND chain_status <> 'closed')`,
		settings, int16(earnVaultIndex)).Scan(&state.HasActivePosition, &state.HasActiveManagedVault, &state.HasActiveAutodeposit)
	if errors.Is(err, pgx.ErrNoRows) {
		return vaultRefundDbState{}, nil
	}
	if err != nil {
		return vaultRefundDbState{}, fmt.Errorf("read Earn vault refund state: %w", err)
	}
	return state, nil
}

// buildEarnVaultRefundEntry ports buildVaultRefundEntry
// (earn-policy-refund.server.ts:141-181): the blocked reason precedence and
// the rent totals are part of the response contract.
func buildEarnVaultRefundEntry(state vaultRefundDbState, snapshot earnVaultRefundSnapshot) EarnVaultRefundEntry {
	entry := EarnVaultRefundEntry{
		Account:       snapshot.Vault.String(),
		Lamports:      snapshot.Lamports,
		TokenAccounts: make([]EarnVaultRefundTokenAccount, 0, len(snapshot.TokenAccounts)),
	}
	entry.BlockedReason = vaultBlockedReason(state, snapshot)
	total := snapshot.Lamports
	for _, tokenAccount := range snapshot.TokenAccounts {
		total += tokenAccount.Lamports
		entry.TokenAccounts = append(entry.TokenAccounts, EarnVaultRefundTokenAccount{
			Account:   tokenAccount.Address.String(),
			AmountRaw: fmt.Sprintf("%d", tokenAccount.Amount),
			IsUsdc:    tokenAccount.IsUsdc,
			Lamports:  tokenAccount.Lamports,
			Mint:      tokenAccount.Mint.String(),
		})
	}
	entry.TotalRefundableLamports = total
	entry.CanRefund = entry.BlockedReason == nil && total > 0
	return entry
}

// vaultBlockedReason mirrors the blockedReason ladder in
// buildVaultRefundEntry: chain funds first (the DB flags are a read model that
// can lag a deposit, ASK-2252), then the DB liveness flags.
func vaultBlockedReason(state vaultRefundDbState, snapshot earnVaultRefundSnapshot) *string {
	for _, tokenAccount := range snapshot.TokenAccounts {
		if tokenAccount.Amount > 0 && tokenAccount.Address != snapshot.VaultUsdcATA {
			blocked := "Vault still holds funds on chain"
			return &blocked
		}
	}
	var reason string
	switch {
	case state.HasActivePosition:
		reason = "Active Earn position"
	case state.HasActiveAutodeposit:
		reason = "Active Autodeposit"
	case state.HasActiveManagedVault:
		reason = "Active Earn vault policy"
	default:
		return nil
	}
	return &reason
}
