package webapp

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

type withdrawConfirmChain interface {
	SignatureState(ctx context.Context, signature string) (chain.SignatureState, error)
	Execution(ctx context.Context, signature solana.Signature, commitment rpc.CommitmentType) (chain.Execution, error)
	Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error)
	TokenAccounts(ctx context.Context, owner, program solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (uint64, []chain.Account, error)
}

// The verification reads can land on RPC nodes that lag behind the client's
// own confirmation; poll briefly before treating a missing read as a
// rejection (launch night 2026-07-08).
const withdrawVerifyAttempts = 6

var withdrawVerifyDelay = 500 * time.Millisecond

func withdrawSleep(ctx context.Context) error {
	timer := time.NewTimer(withdrawVerifyDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func withdrawConfirmedSignatureSlot(ctx context.Context, client withdrawConfirmChain, sleep func(context.Context) error, operation, signature string) (*big.Int, error) {
	for attempt := 1; attempt <= withdrawVerifyAttempts; attempt++ {
		state, err := client.SignatureState(ctx, signature)
		if err != nil {
			return nil, err
		}
		switch {
		case state.Found && state.Err != "":
			return nil, fmt.Errorf("%s transaction failed on-chain.", operation)
		case state.Found && (state.Commitment == chain.Confirmed || state.Commitment == chain.Finalized):
			return new(big.Int).SetUint64(state.Slot), nil
		}
		if attempt < withdrawVerifyAttempts {
			if err := sleep(ctx); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("%s transaction is not confirmed.", operation)
}

func withdrawBalanceAmount(balances map[solana.PublicKey]chain.TokenBalance, key solana.PublicKey) uint64 {
	if balance, ok := balances[key]; ok {
		return balance.Amount
	}
	return 0
}

// withdrawTokenBalanceDeltaRaw ports getParsedTokenBalanceDeltaRaw
// : the summed token-balance change
// over every account the mint touches, matched by token account or, when no
// account is given, by owner.
func withdrawTokenBalanceDeltaRaw(execution chain.Execution, mint solana.PublicKey, tokenAccount, fallbackOwner string) int64 {
	touched := map[solana.PublicKey]bool{}
	for key, balance := range execution.Pre {
		if balance.Mint == mint {
			touched[key] = true
		}
	}
	for key, balance := range execution.Post {
		if balance.Mint == mint {
			touched[key] = true
		}
	}
	var delta int64
	for key := range touched {
		owner := ""
		if post, ok := execution.Post[key]; ok {
			owner = post.Owner.String()
		} else if pre, ok := execution.Pre[key]; ok {
			owner = pre.Owner.String()
		}
		if tokenAccount != "" {
			if key.String() != tokenAccount {
				continue
			}
		} else if fallbackOwner == "" || owner != fallbackOwner {
			continue
		}
		delta += int64(withdrawBalanceAmount(execution.Post, key)) - int64(withdrawBalanceAmount(execution.Pre, key))
	}
	return delta
}

func withdrawTransactionProof(ctx context.Context, client withdrawConfirmChain, sleep func(context.Context) error, cluster string, in *withdrawCanonicalInput) error {
	signature, err := solana.SignatureFromBase58(in.WithdrawalSignature)
	if err != nil {
		return fmt.Errorf("getTransaction: %w", err)
	}
	var execution chain.Execution
	found := false
	for attempt := 1; attempt <= withdrawVerifyAttempts && !found; attempt++ {
		// Parsed transactions propagate later than signature statuses — a
		// missing transaction here is usually lag, not a bad signature.
		read, err := client.Execution(ctx, signature, rpc.CommitmentConfirmed)
		if errors.Is(err, chain.ErrNotFound) {
			if attempt < withdrawVerifyAttempts {
				if err := sleep(ctx); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		execution, found = read, true
	}
	if !found {
		return errors.New("Confirmed withdrawal transaction details are unavailable.")
	}
	if execution.Err != nil {
		return errors.New("Withdrawal transaction proof has an execution error.")
	}
	if !in.ConfirmedSlot.IsUint64() || in.ConfirmedSlot.Uint64() != execution.Slot {
		return errors.New("Confirmed withdrawal transaction slot does not match the recorded slot.")
	}

	liquidityMint, err := withdrawPublicKeyInput(in.LiquidityMint)
	if err != nil {
		return err
	}
	productAsset, err := earnprep.WithdrawResolveProductAsset(cluster, liquidityMint)
	if err != nil {
		return err
	}
	wallet, err := withdrawPublicKeyInput(in.WalletAddress)
	if err != nil {
		return err
	}
	vault, err := withdrawPublicKeyInput(in.VaultPubkey)
	if err != nil {
		return err
	}
	walletLiquidityAta, err := spl.AssociatedTokenAddress(wallet, liquidityMint, productAsset.TokenProgram)
	if err != nil {
		return err
	}
	// ATA addresses depend on the mint's token program (CASH/USDG/PYUSD are
	// Token-2022); resolving through the product catalog also rejects
	// unsupported mints before any balance evidence is read.
	vaultLiquidityAta, err := spl.AssociatedTokenAddress(vault, liquidityMint, productAsset.TokenProgram)
	if err != nil {
		return err
	}

	walletAtaDelta := withdrawTokenBalanceDeltaRaw(execution, liquidityMint, walletLiquidityAta.String(), "")
	walletTransfer := walletAtaDelta
	if walletTransfer <= 0 {
		walletTransfer = withdrawTokenBalanceDeltaRaw(execution, liquidityMint, "", in.WalletAddress)
	}
	if walletTransfer <= 0 {
		return errors.New("Confirmed withdrawal transaction does not transfer the withdrawal mint to the authenticated wallet.")
	}
	vaultLiquidityDelta := withdrawTokenBalanceDeltaRaw(execution, liquidityMint, vaultLiquidityAta.String(), "")

	if in.SourceType == "idle" {
		if vaultLiquidityDelta >= 0 || -vaultLiquidityDelta != walletTransfer {
			return errors.New("Confirmed idle withdrawal does not debit the selected vault token account by the wallet credit amount.")
		}
	} else if in.SourceType == "reserve" {
		vaultIdleDelta := max(vaultLiquidityDelta, 0)
		if in.SourceAmountRaw == nil {
			in.SourceAmountRaw = new(big.Int).SetInt64(walletTransfer + vaultIdleDelta)
		}
	}
	in.WithdrawnAmountRaw = new(big.Int).SetInt64(walletTransfer)
	return nil
}
