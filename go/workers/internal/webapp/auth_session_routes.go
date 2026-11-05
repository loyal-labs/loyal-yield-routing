package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

// GET /api/auth/session and POST /api/auth/session/refresh. Ports
// web/apps/web/src/app/api/auth/session/route.ts and session/refresh/route.ts
// with their helpers in features/identity/server/session-cookie.ts and
// packages/auth-core/src/session.ts (claims schema, claims-to-user mapping).
var earnAuthSessionFamily = earnFamily{
	name:     "auth session",
	ready:    func(h earnRoutes) bool { return h.auth != nil && h.Session != nil },
	register: earnRoutes.registerAuthSession,
}

const (
	authSessionPath        = "/api/auth/session"
	authSessionRefreshPath = "/api/auth/session/refresh"
	sessionRefreshMinAge   = 24 * time.Hour
)

var sessionEmail = regexp.MustCompile(`^[A-Za-z0-9_'+\-.]*[A-Za-z0-9_+-]@([A-Za-z0-9][A-Za-z0-9\-]*\.)+[A-Za-z]{2,}$`)

// Field order is JSON wire order, the order zod's authSessionUserSchema emits.
type AuthSessionUser struct {
	AuthMethod          string `json:"authMethod"`
	SubjectAddress      string `json:"subjectAddress"`
	DisplayAddress      string `json:"displayAddress"`
	Email               string `json:"email,omitempty"`
	Provider            string `json:"provider,omitempty"`
	WalletAddress       string `json:"walletAddress,omitempty"`
	SmartAccountAddress string `json:"smartAccountAddress,omitempty"`
	SettingsPda         string `json:"settingsPda,omitempty"`
}

type AuthSessionMetadata struct {
	ExpiresAt    string `json:"expiresAt"`
	RefreshAfter string `json:"refreshAfter"`
}

type AuthSessionResponse struct {
	User    AuthSessionUser     `json:"user"`
	Session AuthSessionMetadata `json:"session"`
}

type authSessionInput struct {
	SessionInput
}

type authSessionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AuthSessionResponse
}

type authSessionRefreshInput struct {
	SessionInput
	ForwardedProto string `header:"X-Forwarded-Proto"`
	scheme         string
}

func (i *authSessionRefreshInput) Resolve(ctx huma.Context) []error {
	i.scheme = ctx.URL().Scheme
	return i.SessionInput.Resolve(ctx)
}

type authSessionRefreshOutput struct {
	CacheControl string `header:"Cache-Control"`
	SetCookie    string `header:"Set-Cookie"`
	Body         AuthSessionResponse
}

func (h earnRoutes) registerAuthSession(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-auth-session",
		Method:      http.MethodGet,
		Path:        authSessionPath,
		Summary:     "Read the signed-in web session",
		Responses:   errorResponses[routeError](api, 401, 500),
	}, h.authSession)
	huma.Register(api, huma.Operation{
		OperationID: "post-auth-session-refresh",
		Method:      http.MethodPost,
		Path:        authSessionRefreshPath,
		Summary:     "Re-issue a wallet web session older than a day",
		Responses:   errorResponses[routeError](api, 401, 500),
	}, h.authSessionRefresh)
}

func (h earnRoutes) authSession(_ context.Context, in *authSessionInput) (*authSessionOutput, error) {
	claims, err := h.readSessionClaims(in.SessionInput)
	if err != nil {
		return nil, err
	}
	session, err := claims.metadata()
	if err != nil {
		return nil, authSessionFailed("auth session metadata", err)
	}
	return &authSessionOutput{CacheControl: perWalletCacheControl(), Body: AuthSessionResponse{User: claims.user, Session: session}}, nil
}

func (h earnRoutes) authSessionRefresh(_ context.Context, in *authSessionRefreshInput) (*authSessionRefreshOutput, error) {
	claims, err := h.readSessionClaims(in.SessionInput)
	if err != nil {
		return nil, err
	}
	session, err := claims.metadata()
	if err != nil {
		return nil, authSessionFailed("auth session metadata", err)
	}
	now := time.Now()
	out := &authSessionRefreshOutput{CacheControl: perWalletCacheControl(), Body: AuthSessionResponse{User: claims.user, Session: session}}
	if !claims.dueForRefresh(now) {
		return out, nil
	}

	ttl, err := privySessionTTL()
	if err != nil {
		return nil, authSessionFailed("auth session refresh ttl", err)
	}
	token, err := h.Session.Mint(claims.user.tokenClaims(), jwt.NoAudience, time.Duration(ttl)*time.Second, now)
	if err != nil {
		return nil, authSessionFailed("auth session refresh mint", err)
	}
	domains := ParseCookieParentDomains(os.Getenv("AUTH_COOKIE_PARENT_DOMAIN"))
	out.SetCookie, err = privySessionCookieHeader(domains, in.SessionInput.cookieName(), token, ttl,
		in.ForwardedProto, in.ForwardedHost, in.SessionInput.host, in.scheme)
	if err != nil {
		return nil, authSessionFailed("auth session refresh cookie", err)
	}
	out.SetCookie += "; Expires=" + now.Add(time.Duration(ttl)*time.Second).UTC().Format(http.TimeFormat)
	iat := float64(now.Unix())
	refreshed := sessionClaims{user: claims.user, iat: &iat}
	exp := float64(now.Add(time.Duration(ttl) * time.Second).Unix())
	refreshed.exp = &exp
	if out.Body.Session, err = refreshed.metadata(); err != nil {
		return nil, authSessionFailed("auth session refresh metadata", err)
	}
	return out, nil
}

func authSessionFailed(step string, err error) error {
	slog.Warn("auth session route failed", "step", step, "error", err)
	return earnError(http.StatusInternalServerError, "auth_session_failed", "Auth session request failed.")
}

type sessionClaims struct {
	user     AuthSessionUser
	iat, exp *float64
}

func (h earnRoutes) readSessionClaims(in SessionInput) (sessionClaims, error) {
	unauthenticated := earnError(http.StatusUnauthorized, "unauthenticated", "No active auth session.")
	token := lastCookie(in.Cookie, in.cookieName())
	if token == "" {
		return sessionClaims{}, unauthenticated
	}
	raw, err := h.auth.keys.Verify(token, jwt.NoAudience, time.Now())
	if err != nil {
		return sessionClaims{}, unauthenticated
	}
	claims, ok := parseSessionClaims(raw)
	if !ok {
		return sessionClaims{}, unauthenticated
	}
	return claims, nil
}

// lastCookie is session-cookie.ts parseCookieHeader: the last duplicate wins
// and an empty value reads as absent.
func lastCookie(header, name string) string {
	var value string
	for part := range strings.SplitSeq(header, ";") {
		key, v, found := strings.Cut(jsTrim(part), "=")
		if found && key == name {
			value = v
		}
	}
	return value
}

func parseSessionClaims(raw []byte) (sessionClaims, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return sessionClaims{}, false
	}
	text := func(key string, required bool) (string, bool) {
		value, present := object[key]
		if !present {
			return "", !required
		}
		var s string
		if json.Unmarshal(value, &s) != nil || s == "" {
			return "", false
		}
		return s, true
	}
	var c sessionClaims
	var ok [8]bool
	_, ok[0] = text("sub", false)
	c.user.AuthMethod, ok[1] = text("authMethod", true)
	c.user.SubjectAddress, ok[2] = text("subjectAddress", true)
	c.user.DisplayAddress, ok[3] = text("displayAddress", true)
	c.user.Provider, ok[4] = text("provider", false)
	c.user.WalletAddress, ok[5] = text("walletAddress", false)
	c.user.SmartAccountAddress, ok[6] = text("smartAccountAddress", false)
	c.user.SettingsPda, ok[7] = text("settingsPda", false)
	for _, valid := range ok {
		if !valid {
			return sessionClaims{}, false
		}
	}
	if c.user.AuthMethod != "wallet" && c.user.AuthMethod != "email" {
		return sessionClaims{}, false
	}
	if value, present := object["email"]; present {
		var email string
		if json.Unmarshal(value, &email) != nil {
			return sessionClaims{}, false
		}
		email = jsTrim(email)
		if !validEmail(email) {
			return sessionClaims{}, false
		}
		c.user.Email = email
	}
	var numeric bool
	c.iat, numeric = claimNumber(object["iat"])
	if !numeric {
		return sessionClaims{}, false
	}
	c.exp, _ = claimNumber(object["exp"])
	return c, true
}

func claimNumber(raw json.RawMessage) (*float64, bool) {
	if raw == nil {
		return nil, true
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || string(raw) == "null" {
		return nil, false
	}
	return &n, true
}

func validEmail(s string) bool {
	return !strings.HasPrefix(s, ".") && !strings.Contains(s, "..") && sessionEmail.MatchString(s)
}

func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == 0xFEFF || (r != 0x85 && unicode.IsSpace(r))
	})
}

func (c sessionClaims) metadata() (AuthSessionMetadata, error) {
	if c.iat == nil || c.exp == nil {
		return AuthSessionMetadata{}, errors.New("wallet session is missing iat/exp claims")
	}
	expiresAt, err := isoFromUnixSeconds(*c.exp)
	if err != nil {
		return AuthSessionMetadata{}, err
	}
	refreshAfter, err := isoFromUnixSeconds(*c.iat + sessionRefreshMinAge.Seconds())
	if err != nil {
		return AuthSessionMetadata{}, err
	}
	return AuthSessionMetadata{ExpiresAt: expiresAt, RefreshAfter: refreshAfter}, nil
}

func isoFromUnixSeconds(seconds float64) (string, error) {
	ms := math.Trunc(seconds * 1000)
	if math.Abs(ms) > 8.64e15 {
		return "", errors.New("session time is outside the Date range")
	}
	t := time.UnixMilli(int64(ms)).UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return "", errors.New("session time is outside the ISO year range")
	}
	return t.Format("2006-01-02T15:04:05.000Z"), nil
}

func (c sessionClaims) dueForRefresh(now time.Time) bool {
	if c.user.AuthMethod != "wallet" || c.iat == nil {
		return false
	}
	return float64(now.UnixMilli())-*c.iat*1000 >= float64(sessionRefreshMinAge.Milliseconds())
}

func (u AuthSessionUser) tokenClaims() map[string]any {
	claims := map[string]any{
		"authMethod":     u.AuthMethod,
		"subjectAddress": u.SubjectAddress,
		"displayAddress": u.DisplayAddress,
	}
	for key, value := range map[string]string{
		"email":               u.Email,
		"provider":            u.Provider,
		"walletAddress":       u.WalletAddress,
		"smartAccountAddress": u.SmartAccountAddress,
		"settingsPda":         u.SettingsPda,
	} {
		if value != "" {
			claims[key] = value
		}
	}
	return claims
}
