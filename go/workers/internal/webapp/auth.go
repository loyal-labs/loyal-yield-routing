package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const (
	walletSessionCookie  = "loyal_wallet_session"
	previewSessionCookie = "loyal_preview_session"
	cherrySessionCookie  = "__Host-loyal_cherry_session"
	cherryContextCookie  = "__Host-loyal_cherry_embed"

	mobileEarnAudience jwt.Audience = "loyal-mobile-earn"

	smartAccountsSolanaEnv = "mainnet"
)

var AuthGrants = []Grant{
	{"app_users", "SELECT"},
	{"app_user_smart_accounts", "SELECT"},
}

var previewHost = regexp.MustCompile(`(?i)(^|\.)preview\.askloyal\.com$`)

type Principal struct {
	Wallet       solana.PublicKey
	Settings     solana.PublicKey
	SmartAccount solana.PublicKey
}

type SessionInput struct {
	Authorization string `header:"Authorization"`
	Cookie        string `header:"Cookie"`
	ForwardedHost string `header:"X-Forwarded-Host"`
	host          string
}

func (s *SessionInput) Resolve(ctx huma.Context) []error {
	s.host = ctx.Host()
	return nil
}

type AuthFailure uint8

const (
	Unauthenticated AuthFailure = iota
	InvalidMobileSession
	WalletRequired
	SmartAccountRequired
)

var authFailures = [...]struct {
	status int
	code   string
}{
	Unauthenticated:      {http.StatusUnauthorized, "unauthenticated"},
	InvalidMobileSession: {http.StatusUnauthorized, "invalid_mobile_session"},
	WalletRequired:       {http.StatusUnauthorized, "wallet_required"},
	SmartAccountRequired: {http.StatusConflict, "smart_account_required"},
}

func (f AuthFailure) Status() int  { return authFailures[f].status }
func (f AuthFailure) Code() string { return authFailures[f].code }

type AuthError struct {
	Failure AuthFailure
	cause   error
}

func (e *AuthError) Error() string {
	return e.Failure.Code()
}

func (e *AuthError) Unwrap() error { return e.cause }

func refuse(failure AuthFailure, cause error) error {
	return &AuthError{Failure: failure, cause: cause}
}

type Auth struct {
	keys  jwt.Keys
	appDB *pgxpool.Pool
	chain *chain.Client
}

func NewAuth(secret, rs256PublicKeyPEM string, appDB *pgxpool.Pool, rpcClient *chain.Client) (*Auth, error) {
	if appDB == nil || rpcClient == nil {
		return nil, errors.New("session auth needs WEB_APP_DATABASE_URL and WEB_SOLANA_RPC_URL")
	}
	keys, err := jwt.NewKeys(secret, strings.ReplaceAll(rs256PublicKeyPEM, `\n`, "\n"))
	if err != nil {
		return nil, fmt.Errorf("AUTH_JWT_RS256_PUBLIC_KEY: %w", err)
	}
	if keys.Empty() {
		return nil, errors.New("session auth needs AUTH_JWT_SECRET or AUTH_JWT_RS256_PUBLIC_KEY")
	}
	return &Auth{keys: keys, appDB: appDB, chain: rpcClient}, nil
}

type Session struct {
	Wallet  solana.PublicKey
	account func(context.Context) (settings, smartAccount solana.PublicKey, err error)
}

func (a *Auth) Session(in SessionInput) (Session, error) {
	if token, ok := in.bearer(); ok {
		return a.bearerSession(token)
	}
	return a.cookieSession(in)
}

func (a *Auth) Principal(ctx context.Context, in SessionInput) (Principal, error) {
	session, err := a.Session(in)
	if err != nil {
		return Principal{}, err
	}
	settings, smartAccount, err := session.account(ctx)
	if err != nil {
		return Principal{}, err
	}
	claimed := Principal{Wallet: session.Wallet, Settings: settings, SmartAccount: smartAccount}
	if err := a.controls(ctx, claimed); err != nil {
		return Principal{}, err
	}
	return claimed, nil
}

func (s SessionInput) bearer() (string, bool) {
	if len(s.Authorization) < len("bearer ") || !strings.EqualFold(s.Authorization[:len("bearer ")], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(s.Authorization[len("bearer "):]), true
}

func (s SessionInput) cookieName() string {
	if headerCookie(s.Cookie, cherryContextCookie) == "1" {
		return cherrySessionCookie
	}
	host := s.ForwardedHost
	if host == "" {
		host = s.host
	}
	host, _, _ = strings.Cut(host, ":")
	if previewHost.MatchString(host) {
		return previewSessionCookie
	}
	return walletSessionCookie
}

func headerCookie(header, name string) string {
	for part := range strings.SplitSeq(header, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && key == name {
			return value
		}
	}
	return ""
}

func (a *Auth) cookieSession(in SessionInput) (Session, error) {
	token := headerCookie(in.Cookie, in.cookieName())
	if token == "" {
		return Session{}, refuse(Unauthenticated, nil)
	}
	raw, err := a.keys.Verify(token, jwt.NoAudience, time.Now())
	if err != nil {
		return Session{}, refuse(Unauthenticated, err)
	}
	var claims struct {
		AuthMethod          string  `json:"authMethod"`
		SubjectAddress      string  `json:"subjectAddress"`
		DisplayAddress      string  `json:"displayAddress"`
		Provider            *string `json:"provider"`
		WalletAddress       string  `json:"walletAddress"`
		SmartAccountAddress string  `json:"smartAccountAddress"`
		SettingsPda         string  `json:"settingsPda"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.SubjectAddress == "" || claims.DisplayAddress == "" {
		return Session{}, refuse(Unauthenticated, errors.New("session claims do not match the web session schema"))
	}
	switch claims.AuthMethod {
	case "wallet":
	case "email":
		return Session{}, refuse(WalletRequired, nil)
	default:
		return Session{}, refuse(Unauthenticated, errors.New("unknown authMethod"))
	}
	if claims.Provider != nil && *claims.Provider != "solana" {
		return Session{}, refuse(WalletRequired, errors.New("session provider is not solana"))
	}
	wallet, err := solana.PublicKeyFromBase58(claims.WalletAddress)
	if err != nil || claims.SubjectAddress != claims.WalletAddress {
		return Session{}, refuse(Unauthenticated, errors.New("session wallet is not its subject"))
	}
	if claims.SettingsPda == "" || claims.SmartAccountAddress == "" {
		return Session{Wallet: wallet, account: noSmartAccount}, nil
	}
	settings, settingsErr := solana.PublicKeyFromBase58(claims.SettingsPda)
	smartAccount, smartAccountErr := solana.PublicKeyFromBase58(claims.SmartAccountAddress)
	if settingsErr != nil || smartAccountErr != nil {
		return Session{}, refuse(Unauthenticated, errors.New("session smart account is not a public key"))
	}
	return Session{Wallet: wallet, account: func(context.Context) (solana.PublicKey, solana.PublicKey, error) {
		return settings, smartAccount, nil
	}}, nil
}

func noSmartAccount(context.Context) (solana.PublicKey, solana.PublicKey, error) {
	return solana.PublicKey{}, solana.PublicKey{}, refuse(SmartAccountRequired, nil)
}

func (a *Auth) bearerSession(token string) (Session, error) {
	raw, err := a.keys.Verify(token, mobileEarnAudience, time.Now())
	if err != nil {
		return Session{}, refuse(InvalidMobileSession, err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return Session{}, refuse(InvalidMobileSession, errors.New("mobile session sub is not a string"))
	}
	wallet, err := solana.PublicKeyFromBase58(claims.Sub)
	if err != nil {
		return Session{}, refuse(InvalidMobileSession, errors.New("mobile session sub is not a wallet"))
	}
	return Session{Wallet: wallet, account: func(ctx context.Context) (solana.PublicKey, solana.PublicKey, error) {
		settings, err := a.readySettings(ctx, wallet)
		if err != nil {
			return solana.PublicKey{}, solana.PublicKey{}, err
		}
		smartAccount, _, err := squads.SmartAccountAddress(settings, 0)
		return settings, smartAccount, err
	}}, nil
}

func (a *Auth) readySettings(ctx context.Context, wallet solana.PublicKey) (solana.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var settingsPda string
	err := a.appDB.QueryRow(ctx, `SELECT a.settings_pda
FROM app_users u
INNER JOIN app_user_smart_accounts a ON a.user_id = u.id
WHERE u.provider = 'solana' AND u.subject_address = $1 AND a.solana_env = $2 AND a.state = 'ready'`,
		wallet.String(), smartAccountsSolanaEnv).Scan(&settingsPda)
	if errors.Is(err, pgx.ErrNoRows) {
		return solana.PublicKey{}, refuse(SmartAccountRequired, nil)
	}
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("read ready smart account: %w", err)
	}
	settings, err := solana.PublicKeyFromBase58(settingsPda)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("ready smart account settings_pda %q is not a public key", settingsPda)
	}
	return settings, nil
}

func (a *Auth) controls(ctx context.Context, claimed Principal) error {
	derived, _, err := squads.SmartAccountAddress(claimed.Settings, 0)
	if err != nil {
		return err
	}
	if derived != claimed.SmartAccount {
		return refuse(SmartAccountRequired, errors.New("smart account is not the settings' account 0"))
	}
	_, accounts, err := a.chain.Accounts(ctx, []solana.PublicKey{claimed.Settings}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return fmt.Errorf("read smart account settings: %w", err)
	}
	settings, err := squads.DecodeSettings(accounts[0])
	if err != nil {
		return refuse(SmartAccountRequired, err)
	}
	for _, signer := range settings.Signers {
		if signer.Key == claimed.Wallet {
			return nil
		}
	}
	return refuse(SmartAccountRequired, errors.New("wallet is not a signer of the smart account settings"))
}
