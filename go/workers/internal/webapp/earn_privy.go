package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

// Privy sign-in: POST /api/auth/privy/complete. Ports
// web/apps/web/src/app/api/auth/privy/complete/route.ts — the route trades a
// verified Privy identity token for a Loyal session: identity writes (see
// privy_provisioning.go) plus a session JWT mint and the web session cookie
// (privy_session_cookie.go). Authenticated by the Privy token, never by a
// walletAddress query param: the body's walletAddress must be one of the
// Solana wallets linked to the Privy user.
var earnPrivyFamily = earnFamily{
	name: "privy complete",
	appGrants: []Grant{
		{"app_users", "SELECT"},
		{"app_users", "INSERT"},
		{"app_users", "UPDATE"},
		{"app_user_wallets", "INSERT"},
		{"app_user_wallets", "UPDATE"},
		{"app_user_smart_accounts", "SELECT"},
		{"app_user_smart_accounts", "UPDATE"},
		{"app_smart_account_signers", "SELECT"},
		{"app_smart_account_signers", "INSERT"},
		{"app_smart_account_signers", "UPDATE"},
	},
	ready: func(h earnRoutes) bool {
		return h.Privy != nil && h.Privy.AppID != "" && h.Session != nil && h.signedIn()
	},
	register: earnRoutes.registerPrivy,
}

const privyCompletePath = "/api/auth/privy/complete"

type PrivyCompleteRequest struct {
	_             struct{} `additionalProperties:"true"`
	WalletAddress string   `json:"walletAddress"`
}

type PrivyAuthUser struct {
	AuthMethod          string  `json:"authMethod"`
	SubjectAddress      string  `json:"subjectAddress"`
	DisplayAddress      string  `json:"displayAddress"`
	Provider            string  `json:"provider"`
	WalletAddress       string  `json:"walletAddress"`
	SmartAccountAddress string  `json:"smartAccountAddress"`
	SettingsPda         string  `json:"settingsPda"`
	Email               *string `json:"email,omitempty"`
}

type PrivyCompleteResponse struct {
	User PrivyAuthUser `json:"user"`
}

type privyCompleteInput struct {
	SessionInput
	PrivyIDToken   string `header:"privy-id-token"`
	ForwardedProto string `header:"X-Forwarded-Proto"`
	Raw            rawJSON
	scheme         string
}

func (i *privyCompleteInput) Resolve(ctx huma.Context) []error {
	i.scheme = ctx.URL().Scheme
	return nil
}

type privyCompleteOutput struct {
	SetCookie string                `header:"Set-Cookie"`
	Body      PrivyCompleteResponse `json:"body"`
}

func (h earnRoutes) registerPrivy(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-auth-privy-complete",
		Method:      http.MethodPost,
		Path:        privyCompletePath,
		Summary:     "Exchange a verified Privy identity token for a Loyal session",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 500, 503),
	}, h.privyComplete)
	documentJSONBody[PrivyCompleteRequest](api, privyCompletePath)
}

// privyComplete ports route.ts handleRequest: header check, body schema,
// completePrivyAuth, then the session cookie on a 200 user response.
func (h earnRoutes) privyComplete(ctx context.Context, in *privyCompleteInput) (*privyCompleteOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if in.PrivyIDToken == "" {
		return nil, earnError(http.StatusUnauthorized, "privy_identity_token_missing", "Missing Privy identity token.")
	}
	walletAddress, err := privyCompletionWallet(in.Raw.data)
	if err != nil {
		return nil, err
	}

	identity, err := h.verifyPrivyIdentityToken(ctx, in.PrivyIDToken)
	if err != nil {
		return nil, err
	}
	var linked *PrivyLinkedWallet
	for i := range identity.Wallets {
		if identity.Wallets[i].Address == walletAddress {
			linked = &identity.Wallets[i]
			break
		}
	}
	if linked == nil {
		return nil, earnError(http.StatusForbidden, "privy_wallet_not_linked", "Wallet is not linked to the authenticated Privy user.")
	}
	wallet, err := solana.PublicKeyFromBase58(linked.Address)
	if err != nil {
		return nil, earnError(http.StatusUnauthorized, "privy_identity_token_invalid", "Privy identity token is invalid.")
	}

	userID, err := h.privyGetOrCreateUser(ctx, wallet, identity.Email)
	if err != nil {
		return nil, earnUnavailable("privy user upsert", err,
			"privy_sign_in_unavailable", "Could not start your session. Please try again.")
	}
	smartAccount, err := h.privyEnsureSmartAccount(ctx, userID, wallet)
	if err != nil {
		var refused *routeError
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, earnUnavailable("privy smart account ensure", err,
			"privy_sign_in_unavailable", "Could not start your session. Please try again.")
	}

	ttl, err := privySessionTTLDuration()
	if err != nil {
		slog.Warn("privy session ttl is not configured", "error", err)
		return nil, earnError(http.StatusInternalServerError, "session_config_invalid", "Session signing is not configured.")
	}
	now := time.Now()
	claims := map[string]any{
		"authMethod":          "wallet",
		"subjectAddress":      wallet.String(),
		"displayAddress":      wallet.String(),
		"provider":            "solana",
		"walletAddress":       wallet.String(),
		"smartAccountAddress": smartAccount.SmartAccountAddress,
		"settingsPda":         smartAccount.SettingsPda,
	}
	if identity.Email != "" {
		claims["email"] = identity.Email
	}
	// The session mint for Privy complete signs the same web session claims
	// the Next route's issueSessionToken does (session-cookie.ts:202-225);
	// signer_test.go allows the mint only here and in earn_session.go.
	sessionToken, err := h.Session.Mint(claims, jwt.NoAudience, ttl, now)
	if err != nil {
		slog.Warn("privy session mint failed", "error", err, "wallet", wallet.String())
		return nil, earnError(http.StatusInternalServerError, "session_mint_failed", "Failed to start your session.")
	}

	cookieName := privySessionCookieName(in.Cookie, in.ForwardedHost, in.SessionInput.host)
	seconds, err := privySessionTTL()
	if err != nil {
		return nil, earnError(http.StatusInternalServerError, "session_config_invalid", "Session signing is not configured.")
	}
	setCookie, err := privySessionCookieHeader(cookieName, sessionToken, seconds,
		in.ForwardedProto, in.ForwardedHost, in.SessionInput.host, in.scheme)
	if err != nil {
		slog.Warn("privy session cookie refused", "error", err, "wallet", wallet.String())
		return nil, earnError(http.StatusInternalServerError, "session_cookie_failed", "Could not start your session.")
	}

	var email *string
	if identity.Email != "" {
		email = &identity.Email
	}
	return &privyCompleteOutput{SetCookie: setCookie, Body: PrivyCompleteResponse{
		User: PrivyAuthUser{
			AuthMethod:          "wallet",
			SubjectAddress:      wallet.String(),
			DisplayAddress:      wallet.String(),
			Provider:            "solana",
			WalletAddress:       wallet.String(),
			SmartAccountAddress: smartAccount.SmartAccountAddress,
			SettingsPda:         smartAccount.SettingsPda,
			Email:               email,
		},
	}}, nil
}

// privyCompletionWallet ports the zod body schema (route.ts:14-16): a JSON
// object whose walletAddress is a 32-64 character string. An unparseable body
// becomes an empty object, as `await request.json().catch(() => ({}))` does,
// and fails the same schema with 400 invalid_privy_completion_request.
func privyCompletionWallet(raw []byte) (string, error) {
	invalid := func() (string, error) {
		return "", earnError(http.StatusBadRequest, "invalid_privy_completion_request", "Request is invalid.")
	}
	var body struct {
		WalletAddress *string `json:"walletAddress"`
	}
	if json.Unmarshal(raw, &body) != nil || body.WalletAddress == nil {
		return invalid()
	}
	address := *body.WalletAddress
	if len(address) < 32 || len(address) > 64 {
		return invalid()
	}
	return address, nil
}
