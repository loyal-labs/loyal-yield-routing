package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// RealtimeClaims is the Earn realtime token payload
// (token.server.ts:17-29). The field order is the JSON key order: the token
// signs the exact encoded payload, so the marshal order is contract.
type RealtimeClaims struct {
	Aud              string   `json:"aud"`
	ClientKind       string   `json:"clientKind"`
	EarnVaultAddress string   `json:"earnVaultAddress"`
	Exp              int64    `json:"exp"`
	Iat              int64    `json:"iat"`
	Iss              string   `json:"iss"`
	Scopes           []string `json:"scopes"`
	SettingsPda      string   `json:"settingsPda"`
	SolanaEnv        string   `json:"solanaEnv"`
	V                int      `json:"v"`
	WalletAddress    string   `json:"walletAddress"`
}

// RealtimeIssuer mints the Earn realtime event-stream token: a bare
// base64url-encoded claims payload HMAC'd under the raw secret, not a
// three-segment JWT (token.server.ts:95-98). loyal-yield-realtime verifies
// the token with the same shared secret.
type RealtimeIssuer struct{ secret []byte }

// NewRealtimeIssuer rejects secrets shorter than the realtime protocol
// minimum (token.server.ts:66-68) at construction, so a misconfigured
// loyal-web degrades at startup instead of minting weak tokens.
func NewRealtimeIssuer(secret string) (*RealtimeIssuer, error) {
	if len(secret) < 32 {
		return nil, errors.New("Earn realtime authentication secret is invalid.")
	}
	return &RealtimeIssuer{secret: []byte(secret)}, nil
}

// IssueRealtime encodes and signs claims (token.server.ts:82-98); the caller
// fills every claim, including iat and exp.
func (r *RealtimeIssuer) IssueRealtime(claims RealtimeClaims) (token string, err error) {
	if claims.SolanaEnv != "devnet" && claims.SolanaEnv != "mainnet-beta" {
		return "", errors.New("Earn realtime is unavailable on localnet.")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, r.secret)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
