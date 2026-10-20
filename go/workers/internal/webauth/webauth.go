// Package webauth answers "who is calling" for loyal-web's /api routes, the
// same two ways apps/web does: a Privy access token, or the loyal_email_session
// cookie apps/web issues (features/identity/server/session-token.ts).
package webauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const SessionCookie = "loyal_email_session"

// Session is the claims apps/web puts in loyal_email_session
// (packages/auth-core/src/session.ts authSessionTokenClaimsSchema).
type Session struct {
	AuthMethod          string `json:"authMethod"`
	SubjectAddress      string `json:"subjectAddress"`
	DisplayAddress      string `json:"displayAddress"`
	Email               string `json:"email,omitempty"`
	Provider            string `json:"provider,omitempty"`
	WalletAddress       string `json:"walletAddress,omitempty"`
	SmartAccountAddress string `json:"smartAccountAddress,omitempty"`
	SettingsPDA         string `json:"settingsPda,omitempty"`
}

type Config struct {
	PrivyAppID string
	// One of these signs loyal_email_session, as in apps/web: AUTH_JWT_SECRET
	// (HS256 keyed by its SHA-256) or AUTH_JWT_RS256 (PEM public key).
	SessionSecret    string
	SessionPublicKey string
}

type Verifier struct {
	privy   *privyKeys
	appID   string
	session jwt.ParseOption
}

func New(cfg Config) (*Verifier, error) {
	v := &Verifier{appID: cfg.PrivyAppID}
	if cfg.PrivyAppID != "" {
		v.privy = &privyKeys{url: fmt.Sprintf("https://api.privy.io/v1/apps/%s/jwks.json", cfg.PrivyAppID)}
	}
	switch {
	case cfg.SessionPublicKey != "":
		key, err := jwk.ParseKey([]byte(cfg.SessionPublicKey), jwk.WithPEM(true))
		if err != nil {
			return nil, fmt.Errorf("session public key: %w", err)
		}
		v.session = jwt.WithKey(jwa.RS256, key)
	case cfg.SessionSecret != "":
		sum := sha256.Sum256([]byte(cfg.SessionSecret))
		v.session = jwt.WithKey(jwa.HS256, sum[:])
	}
	return v, nil
}

var ErrUnauthenticated = errors.New("unauthenticated")

// Session verifies the loyal_email_session cookie and returns its claims.
func (v *Verifier) Session(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || v.session == nil {
		return Session{}, ErrUnauthenticated
	}
	token, err := jwt.Parse([]byte(cookie.Value), v.session, jwt.WithValidate(true))
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	claims := token.PrivateClaims()
	s := Session{
		AuthMethod:          str(claims, "authMethod"),
		SubjectAddress:      str(claims, "subjectAddress"),
		DisplayAddress:      str(claims, "displayAddress"),
		Email:               str(claims, "email"),
		Provider:            str(claims, "provider"),
		WalletAddress:       str(claims, "walletAddress"),
		SmartAccountAddress: str(claims, "smartAccountAddress"),
		SettingsPDA:         str(claims, "settingsPda"),
	}
	if s.AuthMethod == "" || s.SubjectAddress == "" || s.DisplayAddress == "" {
		return Session{}, fmt.Errorf("%w: incomplete session claims", ErrUnauthenticated)
	}
	return s, nil
}

// PrivyUser verifies a Privy access token (Authorization bearer or the
// privy-token cookie) and returns the Privy user id.
func (v *Verifier) PrivyUser(r *http.Request) (string, error) {
	if v.privy == nil {
		return "", ErrUnauthenticated
	}
	raw := bearer(r)
	if raw == "" {
		return "", ErrUnauthenticated
	}
	keys, err := v.privy.get(r.Context())
	if err != nil {
		return "", fmt.Errorf("privy jwks: %w", err)
	}
	token, err := jwt.Parse([]byte(raw), jwt.WithKeySet(keys), jwt.WithValidate(true),
		jwt.WithIssuer("privy.io"), jwt.WithAudience(v.appID))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if token.Subject() == "" {
		return "", fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}
	return token.Subject(), nil
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("privy-token"); err == nil {
		return c.Value
	}
	return ""
}

func str(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// privyKeys caches Privy's JWKS for 15 minutes and keeps the last good set
// when a refresh fails (wen server/internal/auth/auth.go).
type privyKeys struct {
	url     string
	mu      sync.Mutex
	keys    jwk.Set
	fetched time.Time
}

func (p *privyKeys) get(ctx context.Context) (jwk.Set, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keys != nil && time.Since(p.fetched) < 15*time.Minute {
		return p.keys, nil
	}
	set, err := jwk.Fetch(ctx, p.url)
	if err != nil {
		if p.keys != nil {
			return p.keys, nil
		}
		return nil, err
	}
	p.keys, p.fetched = set, time.Now()
	return set, nil
}
