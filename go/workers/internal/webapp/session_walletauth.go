package webapp

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
)

// Wallet-signed auth for mobile Earn requests, ported from
// web/apps/web/src/features/identity/server/mobile-wallet-auth.ts and
// wallet-auth-signature.ts. The web sign-in flow is gated by the Cap captcha,
// which a native client cannot produce, so mobile signs a short purpose-scoped
// message with the wallet key instead; the server rebuilds the exact message
// and verifies the Ed25519 signature inside a freshness window to bound replay.
const (
	mobileAuthMaxAge        = 5 * time.Minute
	mobileAuthMaxFutureSkew = time.Minute
)

// Every Earn purpose authenticates a session mint: the scoping stops
// cross-endpoint replay, not trust, so a signature scoped to any one of these
// proves control of the same wallet key.
var mobileWalletAuthPurposes = []string{
	"earn-deposit-prepare",
	"earn-deposit-confirm",
	"earn-withdraw-prepare",
	"earn-withdraw-confirm",
	"earn-autodeposit-setup-prepare",
	"earn-autodeposit-setup-confirm",
	"earn-autodeposit-floor-confirm",
	"earn-autodeposit-toggle-confirm",
	"earn-autodeposit-close-prepare",
	"earn-autodeposit-close-confirm",
	"earn-autodeposit-sweep-execute",
	"earn-refund-prepare",
	"earn-max",
}

// buildMobileWalletAuthMessage is the deterministic message the mobile client
// signs and the
// server rebuilds.
func buildMobileWalletAuthMessage(purpose, walletAddress, issuedAt string) string {
	return strings.Join([]string{
		"Loyal Mobile Earn",
		"purpose: " + purpose,
		"wallet: " + walletAddress,
		"issuedAt: " + issuedAt,
	}, "\n")
}

func authenticateMobileWalletRequest(now time.Time, body any) (solana.PublicKey, error) {
	object, ok := body.(map[string]any)
	if !ok {
		return solana.PublicKey{}, missingMobileWalletAuthFields()
	}
	walletAddress, signature, issuedAt, err := mobileWalletAuthFields(object)
	if err != nil {
		return solana.PublicKey{}, err
	}

	wallet, err := solana.PublicKeyFromBase58(walletAddress)
	if err != nil {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_wallet_address", "Wallet address is invalid.")
	}
	signedAt, err := time.Parse(time.RFC3339, issuedAt)
	if err != nil {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_mobile_auth", "issuedAt is not a valid timestamp.")
	}
	if signedAt.Sub(now) > mobileAuthMaxFutureSkew {
		return solana.PublicKey{}, earnError(http.StatusUnauthorized, "stale_mobile_auth", "Signed request timestamp is in the future.")
	}
	if now.Sub(signedAt) > mobileAuthMaxAge {
		return solana.PublicKey{}, earnError(http.StatusUnauthorized, "stale_mobile_auth", "Signed request has expired.")
	}
	decoded, err := base58.Decode(signature)
	if err != nil {
		return solana.PublicKey{}, earnError(http.StatusBadRequest, "invalid_wallet_signature", "Wallet signature is invalid.")
	}
	for _, purpose := range mobileWalletAuthPurposes {
		message := buildMobileWalletAuthMessage(purpose, walletAddress, issuedAt)
		if ed25519.Verify(ed25519.PublicKey(wallet[:]), []byte(message), decoded) {
			return wallet, nil
		}
	}
	return solana.PublicKey{}, earnError(http.StatusUnauthorized, "invalid_mobile_signature", "Wallet signature could not be verified.")
}

func missingMobileWalletAuthFields() error {
	return earnError(http.StatusBadRequest, "invalid_mobile_auth", "Missing mobile wallet auth fields.")
}

func mobileWalletAuthFields(object map[string]any) (walletAddress, signature, issuedAt string, err error) {
	for _, field := range [...]struct {
		name  string
		value *string
	}{
		{"walletAddress", &walletAddress},
		{"signature", &signature},
		{"issuedAt", &issuedAt},
	} {
		raw, ok := object[field.name]
		if !ok {
			return "", "", "", missingMobileWalletAuthFields()
		}
		text, ok := raw.(string)
		if !ok {
			return "", "", "", missingMobileWalletAuthFields()
		}
		*field.value = text
	}
	return walletAddress, signature, issuedAt, nil
}

func parseJSONBody(raw []byte) (any, error) {
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", "Invalid request body.")
	}
	return parsed, nil
}
