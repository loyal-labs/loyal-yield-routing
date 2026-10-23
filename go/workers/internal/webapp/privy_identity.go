package webapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Privy sign-in, part 1 of 3: verifying the `privy-id-token` header. Ports
// web/apps/web/src/features/identity/server/privy-auth.ts
// verifyPrivyIdentityToken, whose @privy-io/node client verifies the identity
// token locally (node/lib/auth.js verifyIdentityToken): an ES256 JWT with
// iss "privy.io" and aud = the Privy app id, keyed by a remote JWKS with a
// 60 minute cache. Ported with stdlib crypto only; no jose dependency.
var privyJWKSBase = "https://api.privy.io/v1/apps/"

const (
	privyJWTIssuer = "privy.io"
	privyJWTAlg    = "ES256"
	// node/lib/auth.js createRemoteJWKSet: cacheMaxAge 1h, cooldownDuration 10m.
	privyJWKSCacheAge = time.Hour
	privyJWKSCooldown = 10 * time.Minute
	privyJWTTyp       = "JWT"
)

type PrivyLinkedWallet struct {
	Address  string
	Embedded bool
}

// PrivyIdentity mirrors privy-auth.ts PrivyIdentity: the Privy user id, a
// preferred email ("" when absent) and the Solana wallets linked in Privy.
type PrivyIdentity struct {
	PrivyUserID string
	Email       string
	Wallets     []PrivyLinkedWallet
}

type privyKey struct {
	x, y *big.Int
}

type privyJWKSCache struct {
	mu        sync.Mutex
	keys      map[string]privyKey
	fetchedAt time.Time
	failedAt  time.Time
}

var privyKeys = &privyJWKSCache{}

func (c *privyJWKSCache) key(ctx context.Context, client *http.Client, appID, kid string) (privyKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if k, ok := c.keys[kid]; ok && now.Sub(c.fetchedAt) < privyJWKSCacheAge {
		return k, nil
	}
	if now.Sub(c.failedAt) < privyJWKSCooldown {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
		return privyKey{}, fmt.Errorf("privy: JWKS has no key %q and is cooling down", kid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, privyJWKSBase+appID+"/jwks.json", nil)
	if err != nil {
		return privyKey{}, err
	}
	req.Header.Set("privy-client", "go")
	res, err := client.Do(req)
	if err != nil {
		c.failedAt = now
		return privyKey{}, fmt.Errorf("privy: fetch JWKS: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.failedAt = now
		return privyKey{}, fmt.Errorf("privy: fetch JWKS: status %d", res.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if json.Unmarshal(raw, &jwks) != nil {
		c.failedAt = now
		return privyKey{}, fmt.Errorf("privy: parse JWKS")
	}
	keys := make(map[string]privyKey, len(jwks.Keys))
	for _, jwk := range jwks.Keys {
		if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.Kid == "" {
			continue
		}
		x, errX := base64.RawURLEncoding.DecodeString(jwk.X)
		y, errY := base64.RawURLEncoding.DecodeString(jwk.Y)
		if errX != nil || errY != nil {
			continue
		}
		keys[jwk.Kid] = privyKey{x: new(big.Int).SetBytes(x), y: new(big.Int).SetBytes(y)}
	}
	if len(keys) == 0 {
		c.failedAt = now
		return privyKey{}, fmt.Errorf("privy: JWKS has no P-256 keys")
	}
	c.keys, c.fetchedAt = keys, now
	k, ok := keys[kid]
	if !ok {
		return privyKey{}, fmt.Errorf("privy: JWKS has no key %q", kid)
	}
	return k, nil
}

type privyIdentityTokenClaims struct {
	Sub            string          `json:"sub"`
	Iss            string          `json:"iss"`
	Aud            json.RawMessage `json:"aud"`
	Exp            int64           `json:"exp"`
	Nbf            int64           `json:"nbf"`
	LinkedAccounts json.RawMessage `json:"linked_accounts"`
}

type privyLinkedAccount struct {
	Type             string `json:"type"`
	Address          string `json:"address"`
	ChainType        string `json:"chain_type"`
	WalletClientType string `json:"wallet_client_type"`
	Email            string `json:"email"`
}

// verifyPrivyIdentityToken ports privy-auth.ts verifyPrivyIdentityToken: any
// verification failure is 401 privy_identity_token_invalid, exactly as the TS
// route maps a throwing SDK call.
func (h earnRoutes) verifyPrivyIdentityToken(ctx context.Context, identityToken string) (PrivyIdentity, error) {
	if h.Privy == nil || h.Privy.AppID == "" {
		return PrivyIdentity{}, earnError(http.StatusServiceUnavailable, "privy_not_configured", "Privy is not configured.")
	}
	identity, err := h.parsePrivyIdentityToken(ctx, identityToken, time.Now())
	if err != nil {
		return PrivyIdentity{}, earnError(http.StatusUnauthorized, "privy_identity_token_invalid", "Privy identity token is invalid.")
	}
	return identity, nil
}

func (h earnRoutes) parsePrivyIdentityToken(ctx context.Context, token string, now time.Time) (PrivyIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return PrivyIdentity{}, fmt.Errorf("privy: token is not a compact JWT")
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token header is undecodable")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(rawHeader, &header) != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token header is not a JSON object")
	}
	if header.Alg != privyJWTAlg || header.Typ != privyJWTTyp || header.Kid == "" {
		return PrivyIdentity{}, fmt.Errorf("privy: token header is not ES256 JWT with a kid")
	}
	key, err := privyKeys.key(ctx, h.HTTP, h.Privy.AppID, header.Kid)
	if err != nil {
		return PrivyIdentity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		return PrivyIdentity{}, fmt.Errorf("privy: token signature is not a 64-byte ES256 signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: key.x, Y: key.y}
	if !ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return PrivyIdentity{}, fmt.Errorf("privy: token signature does not verify")
	}

	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token payload is undecodable")
	}
	var claims privyIdentityTokenClaims
	if json.Unmarshal(rawClaims, &claims) != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token payload is not a JSON object")
	}
	if claims.Iss != privyJWTIssuer || !privyAudienceMatches(claims.Aud, h.Privy.AppID) {
		return PrivyIdentity{}, fmt.Errorf("privy: token issuer or audience does not match the app")
	}
	if claims.Exp == 0 || now.Unix() >= claims.Exp {
		return PrivyIdentity{}, fmt.Errorf("privy: token is expired")
	}
	if claims.Nbf != 0 && now.Unix() < claims.Nbf {
		return PrivyIdentity{}, fmt.Errorf("privy: token is not yet valid")
	}
	return parsePrivyIdentity(claims)
}

func privyAudienceMatches(raw json.RawMessage, appID string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == appID
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, aud := range many {
			if aud == appID {
				return true
			}
		}
	}
	return false
}

// parsePrivyIdentity folds the token's JSON-string linked_accounts claim into
// the shape privy-auth.ts derives from the SDK's user: Solana wallets with an
// embedded marker, plus the first email claim (email account, then Google
// OAuth) — node/lib/identity-token.js parseUserFromIdentityTokenPayload.
func parsePrivyIdentity(claims privyIdentityTokenClaims) (PrivyIdentity, error) {
	if claims.Sub == "" {
		return PrivyIdentity{}, fmt.Errorf("privy: token payload has no subject")
	}
	var linked string
	if json.Unmarshal(claims.LinkedAccounts, &linked) != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token linked_accounts claim is not a JSON string")
	}
	var accounts []privyLinkedAccount
	if json.Unmarshal([]byte(linked), &accounts) != nil {
		return PrivyIdentity{}, fmt.Errorf("privy: token linked_accounts claim is not an account array")
	}
	identity := PrivyIdentity{PrivyUserID: claims.Sub}
	for _, account := range accounts {
		switch {
		case account.Type == "wallet" && account.ChainType == "solana" && account.Address != "":
			identity.Wallets = append(identity.Wallets, PrivyLinkedWallet{
				Address:  account.Address,
				Embedded: account.WalletClientType == "privy",
			})
		case account.Type == "email" && account.Address != "":
			identity.Email = account.Address
		case account.Type == "google_oauth" && identity.Email == "" && account.Email != "":
			identity.Email = account.Email
		}
	}
	return identity, nil
}
