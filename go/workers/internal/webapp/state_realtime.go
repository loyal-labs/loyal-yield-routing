package webapp

// The realtime token route rides the earn-state family and mints the
// short-lived HMAC token the loyal-yield-realtime event stream accepts:
//
//	POST /api/smart-accounts/yield-optimization/realtime/token
//
// <- web/apps/web/src/app/api/smart-accounts/yield-optimization/realtime/token/route.ts
// via web/apps/web/src/features/earn-realtime/server/token.server.ts. The
// op registers with the family unconditionally: without
// REALTIME_AUTH_SECRET it still answers 503 realtime_unavailable itself
// instead of falling through to Next (route.ts:44-54).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

const (
	earnRealtimeTokenPath     = "/api/smart-accounts/yield-optimization/realtime/token"
	earnRealtimeTokenLifetime = 300 * time.Second

	// Earn realtime token constants (token.server.ts:11-14,82-93).
	earnRealtimeTokenIssuer    = "loyal-apps"
	earnRealtimeTokenAudience  = "loyal-yield-realtime"
	earnRealtimeTokenClient    = "web"
	earnRealtimeTokenVersion   = 1
	earnRealtimeTokenSchema    = 1
	earnRealtimeEventsProdURL  = "https://realtime.askloyal.com/events"
	earnRealtimeEventsLocalURL = "http://127.0.0.1:10000/events"
)

// ResolveRealtimeEventsURL ports resolveEarnRealtimeEventsUrl
// (web/apps/web/src/lib/core/config/earn-realtime.ts:11-26): an explicit
// trimmed REALTIME_EVENTS_URL wins, otherwise the local decoder URL applies
// only to a local app environment outside Vercel and the deployed URL
// everywhere else.
func ResolveRealtimeEventsURL(appEnvironment string) string {
	if configured := strings.TrimSpace(os.Getenv("REALTIME_EVENTS_URL")); configured != "" {
		return configured
	}
	onVercel := os.Getenv("VERCEL") == "1" || os.Getenv("VERCEL_ENV") != ""
	if appEnvironment == "local" && !onVercel {
		return earnRealtimeEventsLocalURL
	}
	return earnRealtimeEventsProdURL
}

type realtimeTokenInput struct {
	SessionInput
	ContentType string `header:"Content-Type"`
	Raw         rawJSON
}

type RealtimeTokenResponse struct {
	AccessToken   string `json:"accessToken"`
	EventsURL     string `json:"eventsUrl"`
	ExpiresAt     string `json:"expiresAt"`
	SchemaVersion int    `json:"schemaVersion"`
}

type realtimeTokenOutput struct {
	// Next pins Cache-Control: no-store on success and errors alike
	// (route.ts:8-12,62-70).
	CacheControl string `header:"Cache-Control"`
	Body         RealtimeTokenResponse
}

func (h earnRoutes) registerRealtime(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-realtime-token",
		Method:      http.MethodPost,
		Path:        earnRealtimeTokenPath,
		Summary:     "Mint a short-lived Earn realtime events token for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 503),
	}, h.realtimeToken)
}

func (h earnRoutes) realtimeToken(ctx context.Context, in *realtimeTokenInput) (*realtimeTokenOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn realtime token auth", err, "unauthenticated", "No active auth session.")
	}
	return h.realtimeIssue(account, in.ContentType, in.Raw.data)
}

// realtimeIssue is the post-auth body of the route (route.ts:33-86): a
// client-controlled body is a 400, a missing issuer or a minting failure is
// a 503, and everything else mints from the authenticated identity.
func (h earnRoutes) realtimeIssue(account EarnAccount, contentType string, body []byte) (*realtimeTokenOutput, error) {
	if realtimeHasClientControlledClaims(contentType, body) {
		return nil, earnError(http.StatusBadRequest, "invalid_request", "Realtime identity is derived from the authenticated session.")
	}
	if h.Realtime == nil {
		slog.Error("earn-realtime-token REALTIME_AUTH_SECRET is not set",
			"settingsPda", account.Settings.String(), "walletAddress", account.Wallet.String())
		return nil, earnError(http.StatusServiceUnavailable, "realtime_unavailable", "Earn realtime is temporarily unavailable.")
	}
	issuedAt := time.Now().Unix()
	expiresAt := issuedAt + int64(earnRealtimeTokenLifetime/time.Second)
	token, err := h.Realtime.IssueRealtime(jwt.RealtimeClaims{
		Aud:              earnRealtimeTokenAudience,
		ClientKind:       earnRealtimeTokenClient,
		EarnVaultAddress: account.Vault.String(),
		Exp:              expiresAt,
		Iat:              issuedAt,
		Iss:              earnRealtimeTokenIssuer,
		Scopes:           []string{"autodeposit", "earn"},
		SettingsPda:      account.Settings.String(),
		SolanaEnv:        string(account.Cluster),
		V:                earnRealtimeTokenVersion,
		WalletAddress:    account.Wallet.String(),
	})
	if err != nil {
		slog.Error("earn-realtime-token issuance failed", "error", err,
			"settingsPda", account.Settings.String(), "walletAddress", account.Wallet.String())
		return nil, earnError(http.StatusServiceUnavailable, "realtime_unavailable", "Earn realtime is temporarily unavailable.")
	}
	return &realtimeTokenOutput{CacheControl: "no-store", Body: RealtimeTokenResponse{
		AccessToken:   token,
		EventsURL:     h.RealtimeURL,
		ExpiresAt:     isoMilliseconds(time.Unix(expiresAt, 0)),
		SchemaVersion: earnRealtimeTokenSchema,
	}}, nil
}

// realtimeHasClientControlledClaims ports hasClientControlledClaims
// (route.ts:14-25): an application/json body carrying a JSON object or array
// with at least one key. Invalid JSON reads as no body, like the
// request.json().catch(() => null) fallback.
func realtimeHasClientControlledClaims(contentType string, body []byte) bool {
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return false
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return false
	}
	switch typed := parsed.(type) {
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	}
	return false
}
