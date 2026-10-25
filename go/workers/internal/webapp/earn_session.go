package webapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

var earnSessionFamily = earnFamily{
	name:     "earn session",
	ready:    func(h earnRoutes) bool { return h.Session != nil },
	register: earnRoutes.registerSession,
}

const (
	mobileEarnSessionPath = "/api/smart-accounts/earn/session"
	mobileEarnSessionTTL  = 30 * 24 * time.Hour
)

type EarnSessionRequest struct {
	WalletAddress string `json:"walletAddress"`
	Signature     string `json:"signature"`
	IssuedAt      string `json:"issuedAt"`
}

type EarnSessionResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

type earnSessionInput struct {
	Raw rawJSON
}

type earnSessionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EarnSessionResponse
}

func (h earnRoutes) registerSession(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-session",
		Method:      http.MethodPost,
		Path:        mobileEarnSessionPath,
		Summary:     "Mint a mobile Earn session token from one wallet-signed auth message",
		Responses:   errorResponses[routeError](api, 400, 401, 500),
	}, h.earnSession)
	documentJSONBody[EarnSessionRequest](api, mobileEarnSessionPath)
}

func (h earnRoutes) earnSession(ctx context.Context, in *earnSessionInput) (*earnSessionOutput, error) {
	body, err := parseJSONBody(in.Raw.data)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	wallet, err := authenticateMobileWalletRequest(now, body)
	if err != nil {
		var refused *routeError
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, earnError(http.StatusUnauthorized, "unauthenticated", "Mobile wallet auth failed.")
	}

	// Only the session mint and Privy complete may call h.Session.Mint
	// (signer_test.go); the token authorizes nothing a fresh signed message
	// would not, it only amortizes the on-device approval prompt.
	token, err := h.Session.Mint(map[string]any{"sub": wallet.String()}, mobileEarnAudience, mobileEarnSessionTTL, now)
	if err != nil {
		slog.Warn("mobile earn session mint failed", "error", err, "wallet", wallet.String())
		return nil, earnError(http.StatusInternalServerError, "session_mint_failed", "Failed to mint a mobile Earn session.")
	}
	return &earnSessionOutput{CacheControl: "no-store", Body: EarnSessionResponse{
		Token:     token,
		ExpiresAt: isoMilliseconds(now.Add(mobileEarnSessionTTL)),
	}}, nil
}

// isoMilliseconds renders JavaScript Date.prototype.toISOString: millisecond
// precision, always, in UTC.
func isoMilliseconds(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
}
