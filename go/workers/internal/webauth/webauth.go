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

// The session cookie names of apps/web: getSessionCookieName
// (session-cookie.ts:294-298) picks the Cherry cookie when the Cherry embed
// context cookie is "1" (cherry-session-cookie.ts:3-4,33-40), else the wallet
// cookie. Its preview-host name never applies here: Vercel sees every proxied
// request on the production host.
const (
	WalletSessionCookie      = "loyal_wallet_session"
	CherrySessionCookie      = "__Host-loyal_cherry_session"
	CherryEmbedContextCookie = "__Host-loyal_cherry_embed"
)

// Session is the claims apps/web puts in the session cookie
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

type Principal struct {
	WalletAddress       string
	SmartAccountAddress string
	SettingsPDA         string
}

type Config struct {
	PrivyAppID string
	// What verifies the session cookie, in the order apps/web's
	// verifyAuthSessionTokenMulti tries them (session-token.ts:104-121): the
	// RS256 public key (PEM), then AUTH_JWT_SECRET (HS256 keyed by its SHA-256).
	SessionPublicKey string
	SessionSecret    string
}

type Verifier struct {
	privy   *privyKeys
	appID   string
	session []jwt.ParseOption
}

func New(cfg Config) (*Verifier, error) {
	v := &Verifier{appID: cfg.PrivyAppID}
	if cfg.PrivyAppID != "" {
		v.privy = &privyKeys{url: fmt.Sprintf("https://api.privy.io/v1/apps/%s/jwks.json", cfg.PrivyAppID)}
	}
	if cfg.SessionPublicKey != "" {
		key, err := jwk.ParseKey([]byte(cfg.SessionPublicKey), jwk.WithPEM(true))
		if err != nil {
			return nil, fmt.Errorf("session public key: %w", err)
		}
		v.session = append(v.session, jwt.WithKey(jwa.RS256, key))
	}
	if cfg.SessionSecret != "" {
		sum := sha256.Sum256([]byte(cfg.SessionSecret))
		v.session = append(v.session, jwt.WithKey(jwa.HS256, sum[:]))
	}
	return v, nil
}

var (
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrNotWalletPrincipal = errors.New("not a wallet principal")
)

func sessionCookieName(r *http.Request) string {
	if c, err := r.Cookie(CherryEmbedContextCookie); err == nil && c.Value == "1" {
		return CherrySessionCookie
	}
	return WalletSessionCookie
}

func (v *Verifier) Session(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(sessionCookieName(r))
	if err != nil || len(v.session) == 0 {
		return Session{}, ErrUnauthenticated
	}
	var token jwt.Token
	for _, key := range v.session {
		if token, err = jwt.Parse([]byte(cookie.Value), key, jwt.WithValidate(true)); err == nil {
			break
		}
	}
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

// Principal verifies the session and requires what apps/web's
// mapAuthSessionUserToAuthenticatedPrincipal requires: a Solana wallet
// session whose subject is its wallet, with a smart account and settings PDA.
func (v *Verifier) Principal(r *http.Request) (Principal, error) {
	s, err := v.Session(r)
	if err != nil {
		return Principal{}, err
	}
	switch {
	case s.AuthMethod != "wallet":
		return Principal{}, fmt.Errorf("%w: auth method %q", ErrNotWalletPrincipal, s.AuthMethod)
	case s.Provider != "" && s.Provider != "solana":
		return Principal{}, fmt.Errorf("%w: provider %q", ErrNotWalletPrincipal, s.Provider)
	case s.WalletAddress == "" || s.SubjectAddress != s.WalletAddress:
		return Principal{}, fmt.Errorf("%w: subject is not the wallet", ErrNotWalletPrincipal)
	case s.SmartAccountAddress == "" || s.SettingsPDA == "":
		return Principal{}, fmt.Errorf("%w: no smart account", ErrNotWalletPrincipal)
	}
	return Principal{WalletAddress: s.WalletAddress, SmartAccountAddress: s.SmartAccountAddress, SettingsPDA: s.SettingsPDA}, nil
}

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
