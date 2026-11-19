package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/referral"
)

// Wallet address lookup covers Privy sessions, signed wallet sessions and
// mobile bearer sessions without changing what authenticates any of them.
func (h earnRoutes) referralIdentity(ctx context.Context, wallet string) (referral.Identity, error) {
	if h.Privy == nil {
		// Wallet-only deployments have no Privy identity source.
		return referral.Identity{}, nil
	}
	if h.Privy.AppID == "" || h.Privy.AppSecret == "" {
		return referral.Identity{}, errors.New("referral: Privy lookup is unavailable")
	}
	payload, err := json.Marshal(map[string]string{"address": wallet})
	if err != nil {
		return referral.Identity{}, err
	}
	var user struct {
		ID             string `json:"id"`
		LinkedAccounts []struct {
			Type       string `json:"type"`
			ChainType  string `json:"chain_type"`
			Address    string `json:"address"`
			VerifiedAt int64  `json:"verified_at"`
		} `json:"linked_accounts"`
	}
	err = h.privyCall(ctx, http.MethodPost, privyAPIBase+"/users/wallet/address", payload, nil, &user)
	var refused *privyRefused
	if errors.As(err, &refused) {
		if refused.status == http.StatusNotFound {
			return referral.Identity{}, nil
		}
		return referral.Identity{}, fmt.Errorf("referral: Privy lookup status %d", refused.status)
	}
	if err != nil {
		return referral.Identity{}, err
	}
	if !strings.HasPrefix(user.ID, "did:privy:") || len(user.ID) == len("did:privy:") {
		return referral.Identity{}, errors.New("referral: Privy returned an invalid identity")
	}
	identity := referral.Identity{Subject: user.ID}
	for _, account := range user.LinkedAccounts {
		if account.Type != "wallet" || account.ChainType != "solana" || account.VerifiedAt <= 0 {
			continue
		}
		key, err := solana.PublicKeyFromBase58(account.Address)
		if err != nil || key.String() != account.Address {
			return referral.Identity{}, errors.New("referral: Privy returned an invalid verified wallet")
		}
		identity.Wallets = append(identity.Wallets, account.Address)
	}
	if !slices.Contains(identity.Wallets, wallet) {
		return referral.Identity{}, errors.New("referral: requested wallet missing from verified Privy links")
	}
	return identity, nil
}
