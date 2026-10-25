package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earncatalog"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

func (h dprepRoutes) gate(ctx context.Context, wallet solana.PublicKey, amount uint64, product earncatalog.Product) (Principal, error) {
	principal, err := h.provision(ctx, wallet, amount, product)
	if err == nil {
		return principal, nil
	}
	var refused *routeError
	if errors.As(err, &refused) {
		return Principal{}, refused
	}
	if errors.Is(err, errSponsorNotConfigured) {
		return Principal{}, provisioningFailure(err)
	}
	slog.Error("[earn-deposit-prepare] provisioning failed", "error", err, "walletAddress", wallet.String())
	return Principal{}, earnError(http.StatusBadGateway, "provisioning_failed", "Failed to resolve the smart account for this wallet.")
}

func (h dprepRoutes) provision(ctx context.Context, wallet solana.PublicKey, amount uint64, product earncatalog.Product) (Principal, error) {
	userID, err := h.privyGetOrCreateUser(ctx, wallet, "")
	if err != nil {
		return Principal{}, err
	}
	held, err := h.walletMintBalance(ctx, wallet, product)
	if err != nil {
		return Principal{}, err
	}
	if held < amount {
		return Principal{}, earnError(http.StatusPaymentRequired, "insufficient_balance", "Wallet must hold the selected mint before its Earn account can be created.")
	}
	ensured, err := h.ensureSmartAccount(ctx, userID, wallet)
	if err != nil {
		return Principal{}, err
	}
	settings, err := solana.PublicKeyFromBase58(ensured.SmartAccount.SettingsPda)
	if err != nil {
		return Principal{}, err
	}
	smartAccount, err := solana.PublicKeyFromBase58(ensured.SmartAccount.SmartAccountAddress)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Wallet: wallet, Settings: settings, SmartAccount: smartAccount}, nil
}

func (h dprepRoutes) walletMintBalance(ctx context.Context, wallet solana.PublicKey, product earncatalog.Product) (uint64, error) {
	mint, err := solana.PublicKeyFromBase58(product.Mint)
	if err != nil {
		return 0, err
	}
	program, err := solana.PublicKeyFromBase58(product.TokenProgram)
	if err != nil {
		return 0, err
	}
	_, accounts, err := h.Chain.TokenAccounts(ctx, wallet, program, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return 0, err
	}
	var total uint64
	for i := range accounts {
		token, err := spl.DecodeTokenAccount(&accounts[i])
		if err != nil || token.Mint != mint {
			continue
		}
		total += token.Amount
	}
	return total, nil
}
