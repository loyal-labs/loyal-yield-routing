package webapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/jwt"
)

var earnWalletAuthFamily = earnFamily{
	name: "wallet sign-in",
	appGrants: []Grant{
		TableGrant("app_captcha_keys", "SELECT"),
		TableGrant("app_captcha_keys", "INSERT"),
		TableGrant("app_captcha_keys", "DELETE"),
		TableGrant("app_wallet_auth_completions", "SELECT"),
		TableGrant("app_wallet_auth_completions", "INSERT"),
		TableGrant("app_wallet_auth_completions", "UPDATE"),
		TableGrant("app_users", "SELECT"),
		TableGrant("app_users", "INSERT"),
		TableGrant("app_users", "UPDATE"),
		TableGrant("app_user_wallets", "INSERT"),
		TableGrant("app_user_wallets", "UPDATE"),
		TableGrant("app_user_smart_accounts", "SELECT"),
		TableGrant("app_user_smart_accounts", "INSERT"),
		TableGrant("app_user_smart_accounts", "UPDATE"),
		TableGrant("app_smart_account_signers", "SELECT"),
		TableGrant("app_smart_account_signers", "INSERT"),
		TableGrant("app_smart_account_signers", "UPDATE"),
	},
	ready: func(h earnRoutes) bool {
		return h.WalletAuth != nil && len(h.WalletAuth.CookieParentDomains) > 0 && h.Session != nil && h.signedIn()
	},
	register: earnRoutes.registerWalletAuth,
}

const (
	walletChallengePath         = "/api/auth/wallet/challenge"
	walletCompletePath          = "/api/auth/wallet/complete"
	walletChallengeTTL          = 10 * time.Minute
	walletChallengeTokenType    = "wallet_challenge"
	walletAuthDefaultAppName    = "askloyal"
	walletNonBroadcastBlockhash = "11111111111111111111111111111111"
	walletSiwsStatement         = "Clicking Sign or Approve only proves you control this wallet. This request will not trigger any blockchain transaction or cost any gas fee."
	walletTransactionStatement  = "This transaction only proves you control this wallet for Loyal sign-in. Loyal will not broadcast it. It does not transfer tokens or grant spending permissions."
)

var walletMemoProgram = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

type WalletAuth struct {
	Cap                 *jwt.HS256
	Challenge           *jwt.HS256
	AppName             string
	CookieParentDomains []string
}

var errCapSecretShort = errors.New("CAP_SECRET must be at least 16 bytes")

func NewWalletAuth(capSecret, authSecret, appName string, cookieParentDomains []string) (*WalletAuth, error) {
	if len(capSecret) < capMinSecretLength {
		return nil, errCapSecretShort
	}
	if authSecret == "" {
		return nil, errors.New("AUTH_JWT_SECRET is not set")
	}
	if appName == "" {
		appName = walletAuthDefaultAppName
	}
	challengeKey := sha256.Sum256([]byte(authSecret))
	return &WalletAuth{
		Cap:                 jwt.NewHS256([]byte(capSecret)),
		Challenge:           jwt.NewHS256(challengeKey[:]),
		AppName:             appName,
		CookieParentDomains: cookieParentDomains,
	}, nil
}

type walletAuthRefusal struct {
	Detail walletAuthRefusalDetail `json:"error"`
	status int
}

type walletAuthRefusalDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *walletAuthRefusal) Error() string  { return e.Detail.Code }
func (e *walletAuthRefusal) GetStatus() int { return e.status }

func walletRefusal(status int, code, message string) *walletAuthRefusal {
	return &walletAuthRefusal{Detail: walletAuthRefusalDetail{Code: code, Message: message}, status: status}
}

type walletOriginDetails struct {
	ExpectedOrigin string `json:"expectedOrigin"`
	ReceivedOrigin string `json:"receivedOrigin"`
}

type walletAddressDetails struct {
	WalletAddress string `json:"walletAddress"`
}

type WalletChallengeRequest struct {
	_             struct{} `additionalProperties:"true"`
	Kind          string   `json:"kind,omitempty" enum:"message,siws,transaction"`
	WalletAddress string   `json:"walletAddress,omitempty"`
	CaptchaToken  string   `json:"captchaToken"`
}

type WalletCompleteRequest struct {
	_                 struct{}        `additionalProperties:"true"`
	Kind              string          `json:"kind,omitempty" enum:"message,siws,transaction"`
	ChallengeToken    string          `json:"challengeToken"`
	Signature         string          `json:"signature,omitempty"`
	SignedTransaction string          `json:"signedTransaction,omitempty"`
	Output            json.RawMessage `json:"output,omitempty"`
}

type WalletSignInInput struct {
	Domain         *string   `json:"domain,omitempty"`
	Address        *string   `json:"address,omitempty"`
	Statement      *string   `json:"statement,omitempty"`
	URI            *string   `json:"uri,omitempty"`
	Version        *string   `json:"version,omitempty"`
	ChainID        *string   `json:"chainId,omitempty"`
	Nonce          *string   `json:"nonce,omitempty"`
	IssuedAt       *string   `json:"issuedAt,omitempty"`
	ExpirationTime *string   `json:"expirationTime,omitempty"`
	NotBefore      *string   `json:"notBefore,omitempty"`
	RequestID      *string   `json:"requestId,omitempty"`
	Resources      *[]string `json:"resources,omitempty"`
}

type WalletChallengeResponse struct {
	Kind           string             `json:"kind"`
	ChallengeToken string             `json:"challengeToken"`
	Message        string             `json:"message,omitempty"`
	SignInInput    *WalletSignInInput `json:"signInInput,omitempty"`
	Transaction    string             `json:"transaction,omitempty"`
	ExpiresAt      string             `json:"expiresAt"`
}

type WalletAuthUser struct {
	AuthMethod          string `json:"authMethod"`
	WalletAddress       string `json:"walletAddress"`
	SubjectAddress      string `json:"subjectAddress"`
	DisplayAddress      string `json:"displayAddress"`
	Provider            string `json:"provider"`
	SmartAccountAddress string `json:"smartAccountAddress"`
	SettingsPda         string `json:"settingsPda"`
}

type WalletCompleteResponse struct {
	User WalletAuthUser `json:"user"`
}

type walletChallengeClaims struct {
	TokenType     string             `json:"tokenType"`
	Version       int                `json:"version"`
	ProofKind     string             `json:"proofKind"`
	Origin        string             `json:"origin"`
	WalletAddress string             `json:"walletAddress,omitempty"`
	Message       string             `json:"message,omitempty"`
	SignInInput   *WalletSignInInput `json:"signInInput,omitempty"`
	Memo          string             `json:"memo,omitempty"`
	Transaction   string             `json:"transaction,omitempty"`
	Iat           int64              `json:"iat"`
	Exp           int64              `json:"exp"`
}

type walletAuthInput struct {
	SessionInput
	Origin         string `header:"Origin"`
	ForwardedProto string `header:"X-Forwarded-Proto"`
	Raw            rawJSON
	scheme         string
}

func (i *walletAuthInput) Resolve(ctx huma.Context) []error {
	i.scheme = ctx.URL().Scheme
	return i.SessionInput.Resolve(ctx)
}

func (i *walletAuthInput) requestOrigin() string {
	if i.Origin != "" {
		return i.Origin
	}
	proto, _, _ := strings.Cut(i.ForwardedProto, ",")
	proto = strings.TrimSpace(proto)
	if proto == "" {
		proto = i.scheme
	}
	if proto == "" {
		proto = "http"
	}
	host, _, _ := strings.Cut(i.ForwardedHost, ",")
	host = strings.TrimSpace(host)
	if host == "" {
		host = i.host
	}
	return proto + "://" + strings.ToLower(host)
}

type walletChallengeOutput struct{ Body WalletChallengeResponse }

type walletCompleteOutput struct {
	SetCookie string `header:"Set-Cookie"`
	Body      WalletCompleteResponse
}

func (h earnRoutes) registerWalletAuth(api huma.API) {
	h.registerCap(api)
	huma.Register(api, huma.Operation{
		OperationID: "post-auth-wallet-challenge",
		Method:      http.MethodPost,
		Path:        walletChallengePath,
		Summary:     "Issue a wallet sign-in challenge after a redeemed captcha",
		Responses:   errorResponses[walletAuthRefusal](api, 400, 403, 500),
	}, h.walletChallenge)
	documentJSONBody[WalletChallengeRequest](api, walletChallengePath)
	huma.Register(api, huma.Operation{
		OperationID: "post-auth-wallet-complete",
		Method:      http.MethodPost,
		Path:        walletCompletePath,
		Summary:     "Exchange a signed wallet challenge for a Loyal session",
		Responses:   errorResponses[walletAuthRefusal](api, 400, 401, 403, 409, 500, 502, 503),
	}, h.walletComplete)
	documentJSONBody[WalletCompleteRequest](api, walletCompletePath)
}

func walletRequestBody(raw []byte) map[string]any {
	var body any
	if json.Unmarshal(raw, &body) != nil {
		return map[string]any{}
	}
	object, _ := body.(map[string]any)
	return object
}

func nonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && text != ""
}

func (h earnRoutes) walletChallenge(ctx context.Context, in *walletAuthInput) (*walletChallengeOutput, error) {
	body := walletRequestBody(in.Raw.data)
	captchaToken, _ := body["captchaToken"].(string)
	if body == nil || !h.verifyCaptchaToken(ctx, captchaToken) {
		return nil, walletRefusal(http.StatusForbidden, "captcha_verification_failed", "Captcha verification failed. Please try again.")
	}
	kind, hasKind := body["kind"]
	walletAddress, hasWallet := nonEmptyString(body["walletAddress"])
	switch {
	case (!hasKind || kind == "message") && hasWallet:
		kind = "message"
	case kind == "siws":
	case kind == "transaction" && hasWallet:
	default:
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_challenge_request", "Wallet challenge request is invalid.")
	}

	origin := in.requestOrigin()
	issuedAt := jsTime(time.Now())
	expiresAt := issuedAt.Add(walletChallengeTTL)
	claims := walletChallengeClaims{
		TokenType: walletChallengeTokenType, Version: 1, ProofKind: kind.(string), Origin: origin,
		Iat: issuedAt.Unix(), Exp: expiresAt.Unix(),
	}
	response := WalletChallengeResponse{Kind: claims.ProofKind, ExpiresAt: jsISO(expiresAt)}
	switch claims.ProofKind {
	case "siws":
		domain, uri, ok := walletOriginParts(origin)
		if !ok {
			return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_origin", "Wallet challenge origin is invalid.")
		}
		statement, version, chainID := walletSiwsStatement, "1", siwsChainID(smartAccountsSolanaEnv)
		nonce, issued, expires := siwsNonce(), jsISO(issuedAt), jsISO(expiresAt)
		claims.SignInInput = &WalletSignInInput{
			Domain: &domain, Statement: &statement, URI: &uri, Version: &version, ChainID: &chainID,
			Nonce: &nonce, IssuedAt: &issued, ExpirationTime: &expires,
		}
		response.SignInInput = claims.SignInInput
	case "transaction":
		wallet, err := decodeWalletAddress(walletAddress)
		if err != nil {
			return nil, err
		}
		claims.WalletAddress = walletAddress
		claims.Memo = walletAuthText(h.WalletAuth.AppName, origin, walletAddress, siwsNonce(), issuedAt, expiresAt, walletTransactionStatement)
		claims.Transaction = walletProofTransaction(wallet, claims.Memo)
		response.Transaction = claims.Transaction
	default:
		if _, err := decodeWalletAddress(walletAddress); err != nil {
			return nil, err
		}
		claims.WalletAddress = walletAddress
		claims.Message = walletAuthText(h.WalletAuth.AppName, origin, walletAddress, randomUUID(), issuedAt, expiresAt,
			"This request only verifies that you control this wallet.\nThis is not a transaction and will not cost gas.")
		response.Message = claims.Message
	}
	payload, err := marshalJS(claims)
	if err != nil {
		return nil, err
	}
	response.ChallengeToken = h.WalletAuth.Challenge.SealHS256(payload)
	slog.Info("wallet challenge created", "kind", claims.ProofKind, "origin", origin)
	return &walletChallengeOutput{Body: response}, nil
}

func walletAuthText(appName, origin, walletAddress, nonce string, issuedAt, expiresAt time.Time, statement string) string {
	return strings.Join([]string{
		"Sign in to " + appName,
		"",
		"Version: 1",
		"Origin: " + origin,
		"Wallet: " + walletAddress,
		"Nonce: " + nonce,
		"Issued At: " + jsISO(issuedAt),
		"Expires At: " + jsISO(expiresAt),
		"",
		statement,
	}, "\n")
}

func decodeWalletAddress(address string) (solana.PublicKey, error) {
	decoded, err := base58.Decode(address)
	if err != nil || len(decoded) != solana.PublicKeyLength {
		return solana.PublicKey{}, walletRefusal(http.StatusBadRequest, "invalid_wallet_address", "Wallet address is invalid.")
	}
	return solana.PublicKeyFromBytes(decoded), nil
}

func siwsChainID(env string) string {
	switch env {
	case "mainnet", "devnet", "testnet":
		return "solana:" + env
	}
	return "solana:localnet"
}

func siwsNonce() string { return strings.ReplaceAll(randomUUID(), "-", "") }

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func jsISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

var walletDefaultPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21"}

func walletOriginParts(origin string) (domain, uri string, ok bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" {
		return "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort, special := walletDefaultPorts[scheme]
	host := u.Host
	if !special {
		return host, "null", true
	}
	host = strings.ToLower(host)
	if u.Hostname() == "" {
		return "", "", false
	}
	if u.Port() == defaultPort {
		host = strings.TrimSuffix(host, ":"+defaultPort)
	}
	return host, scheme + "://" + host, true
}

type walletProof struct {
	kind              string
	challengeToken    string
	signature         string
	signedTransaction string
	output            siwsOutput
}

func parseWalletProof(body map[string]any) (walletProof, bool) {
	token, hasToken := nonEmptyString(body["challengeToken"])
	if !hasToken {
		return walletProof{}, false
	}
	kind, hasKind := body["kind"]
	if signature, ok := nonEmptyString(body["signature"]); ok && (!hasKind || kind == "message") {
		return walletProof{kind: "message", challengeToken: token, signature: signature}, true
	}
	if kind == "siws" {
		if output, ok := parseSiwsOutput(body["output"]); ok {
			return walletProof{kind: "siws", challengeToken: token, output: output}, true
		}
	}
	if signed, ok := nonEmptyString(body["signedTransaction"]); ok && kind == "transaction" {
		return walletProof{kind: "transaction", challengeToken: token, signedTransaction: signed}, true
	}
	return walletProof{}, false
}

func (h earnRoutes) walletComplete(ctx context.Context, in *walletAuthInput) (*walletCompleteOutput, error) {
	proof, ok := parseWalletProof(walletRequestBody(in.Raw.data))
	if !ok {
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_completion_request", "Wallet completion request is invalid.")
	}
	claims, err := h.verifyWalletChallengeToken(proof.challengeToken, time.Now())
	if err != nil {
		return nil, err
	}
	origin := in.requestOrigin()
	if claims.Origin != origin {
		refusal := walletRefusal(http.StatusForbidden, "invalid_wallet_origin", "Wallet challenge origin is invalid.")
		refusal.Detail.Details = walletOriginDetails{ExpectedOrigin: claims.Origin, ReceivedOrigin: origin}
		return nil, refusal
	}
	if proof.kind != claims.ProofKind {
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_proof_kind", "Wallet challenge proof type is invalid.")
	}
	var walletAddress string
	switch claims.ProofKind {
	case "siws":
		walletAddress, err = verifySiwsOutput(claims.SignInInput, proof.output)
	case "transaction":
		walletAddress, err = verifyWalletProofTransaction(claims.Memo, proof.signedTransaction, claims.WalletAddress)
	default:
		walletAddress, err = verifyWalletMessage(claims.WalletAddress, claims.Message, proof.signature)
	}
	if err != nil {
		return nil, err
	}
	wallet, err := solana.PublicKeyFromBase58(walletAddress)
	if err != nil {
		return nil, walletRefusal(http.StatusBadRequest, "invalid_wallet_address", "Wallet address is invalid.")
	}
	user, err := h.completeWalletAuth(ctx, sha256Hex(proof.challengeToken), wallet)
	if err != nil {
		return nil, err
	}
	setCookie, err := h.walletSession(in, user)
	if err != nil {
		return nil, err
	}
	return &walletCompleteOutput{SetCookie: setCookie, Body: WalletCompleteResponse{User: user}}, nil
}

func (h earnRoutes) verifyWalletChallengeToken(token string, now time.Time) (walletChallengeClaims, error) {
	invalid := walletRefusal(http.StatusUnauthorized, "invalid_wallet_challenge", "Wallet challenge token is invalid.")
	raw, err := h.WalletAuth.Challenge.Keys().Verify(token, jwt.NoAudience, now)
	if errors.Is(err, jwt.ErrExpired) {
		return walletChallengeClaims{}, walletRefusal(http.StatusUnauthorized, "expired_wallet_challenge", "Wallet challenge expired. Please try again.")
	}
	if err != nil {
		return walletChallengeClaims{}, invalid
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return walletChallengeClaims{}, invalid
	}
	claims, ok := parseWalletChallengeClaims(payload)
	if !ok {
		return walletChallengeClaims{}, invalid
	}
	return claims, nil
}

func parseWalletChallengeClaims(payload map[string]any) (walletChallengeClaims, bool) {
	origin, hasOrigin := nonEmptyString(payload["origin"])
	if payload["tokenType"] != walletChallengeTokenType || payload["version"] != float64(1) || !hasOrigin {
		return walletChallengeClaims{}, false
	}
	claims := walletChallengeClaims{TokenType: walletChallengeTokenType, Version: 1, Origin: origin}
	walletAddress, hasWallet := nonEmptyString(payload["walletAddress"])
	message, hasMessage := nonEmptyString(payload["message"])
	switch payload["proofKind"] {
	case "message":
		if hasWallet && hasMessage {
			claims.ProofKind, claims.WalletAddress, claims.Message = "message", walletAddress, message
			return claims, true
		}
	case "siws":
		if input, ok := parseSiwsInput(payload["signInInput"]); ok {
			claims.ProofKind, claims.SignInInput = "siws", input
			return claims, true
		}
	case "transaction":
		memo, hasMemo := nonEmptyString(payload["memo"])
		transaction, hasTransaction := nonEmptyString(payload["transaction"])
		if hasWallet && hasMemo && hasTransaction {
			claims.ProofKind, claims.WalletAddress, claims.Memo, claims.Transaction = "transaction", walletAddress, memo, transaction
			return claims, true
		}
	}
	if hasWallet && hasMessage {
		claims.ProofKind, claims.WalletAddress, claims.Message = "message", walletAddress, message
		return claims, true
	}
	return walletChallengeClaims{}, false
}

func verifyWalletMessage(walletAddress, message, signature string) (string, error) {
	publicKey, err := decodeWalletAddress(walletAddress)
	if err != nil {
		return "", err
	}
	signatureBytes, err := base58.Decode(signature)
	if err != nil {
		return "", walletRefusal(http.StatusBadRequest, "invalid_wallet_signature", "Wallet signature is invalid.")
	}
	if !ed25519.Verify(publicKey[:], []byte(message), signatureBytes) {
		refusal := walletRefusal(http.StatusUnauthorized, "invalid_wallet_signature", "Wallet signature could not be verified.")
		refusal.Detail.Details = walletAddressDetails{WalletAddress: walletAddress}
		return "", refusal
	}
	return walletAddress, nil
}

func (h earnRoutes) walletSession(in *walletAuthInput, user WalletAuthUser) (string, error) {
	ttl, err := privySessionTTLDuration()
	if err != nil {
		slog.Warn("wallet session ttl is not configured", "error", err)
		return "", walletProvisioningUnavailable()
	}
	claims := map[string]any{
		"authMethod":          user.AuthMethod,
		"subjectAddress":      user.SubjectAddress,
		"displayAddress":      user.DisplayAddress,
		"provider":            user.Provider,
		"walletAddress":       user.WalletAddress,
		"smartAccountAddress": user.SmartAccountAddress,
		"settingsPda":         user.SettingsPda,
	}
	sessionToken, err := h.Session.Mint(claims, jwt.NoAudience, ttl, time.Now())
	if err != nil {
		slog.Warn("wallet session mint failed", "error", err, "wallet", user.WalletAddress)
		return "", walletProvisioningUnavailable()
	}
	seconds, err := privySessionTTL()
	if err != nil {
		return "", walletProvisioningUnavailable()
	}
	cookieName := privySessionCookieName(in.Cookie, in.ForwardedHost, in.SessionInput.host)
	setCookie, err := privySessionCookieHeader(h.WalletAuth.CookieParentDomains, cookieName, sessionToken, seconds,
		in.ForwardedProto, in.ForwardedHost, in.SessionInput.host, in.scheme)
	if err != nil {
		slog.Warn("wallet session cookie refused", "error", err, "wallet", user.WalletAddress)
		return "", earnError(http.StatusInternalServerError, "session_cookie_failed", "Could not start your session.")
	}
	return setCookie, nil
}

func walletProvisioningUnavailable() error {
	return walletRefusal(http.StatusServiceUnavailable, "smart_account_provisioning_failed",
		"We could not finish setting up your session due to a temporary server issue. Your wallet signature was valid — please try again.")
}

func walletSessionUser(walletAddress, smartAccountAddress, settingsPda string) WalletAuthUser {
	return WalletAuthUser{
		AuthMethod: "wallet", WalletAddress: walletAddress, SubjectAddress: walletAddress, DisplayAddress: walletAddress,
		Provider: "solana", SmartAccountAddress: smartAccountAddress, SettingsPda: settingsPda,
	}
}
