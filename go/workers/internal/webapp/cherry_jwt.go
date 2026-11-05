package webapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// cherryJWKS is the SDK's verifyLaunchToken on jose 5.10: an RS256-only
// compact JWS checked against the remote key set, then the exp, nbf and iat
// type rules of jwtVerify, then the app_id and origin equality the SDK adds.
// The key set is cached for ten minutes and refetched on an unknown key at
// most every thirty seconds, as a long-lived jose RemoteJWKSet does.
const (
	cherryJWKSTimeout  = 5 * time.Second
	cherryJWKSCooldown = 30 * time.Second
	cherryJWKSMaxAge   = 10 * time.Minute
	cherryJWKSMaxBytes = 1 << 20
	cherryMinModulus   = 2048
)

var (
	errCherryNoKey       = errors.New("no matching key")
	errCherryMultipleKey = errors.New("multiple matching keys")
)

type cherryJWKS struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	keys      []map[string]any
	fetchedAt time.Time
}

func newCherryJWKS(url string) *cherryJWKS {
	return &cherryJWKS{url: url, client: &http.Client{}}
}

func (s *cherryJWKS) verify(ctx context.Context, token, appID, origin string, now time.Time) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid compact JWS")
	}
	var header map[string]any
	if err := decodeJWTPart(parts[0], &header, false); err != nil || header == nil {
		return nil, errors.New("invalid protected header")
	}
	if alg, _ := header["alg"].(string); alg != "RS256" {
		return nil, errors.New("alg not allowed")
	}
	if _, critical := header["crit"]; critical {
		return nil, errors.New("critical header parameters are not supported")
	}
	kid, hasKID := header["kid"].(string)
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("invalid signature encoding")
	}
	key, err := s.key(ctx, kid, hasKID)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return nil, errors.New("signature verification failed")
	}
	var claims map[string]any
	if err := decodeJWTPart(parts[1], &claims, true); err != nil || claims == nil {
		return nil, errors.New("claims set must be a JSON object")
	}
	if err := checkJWTTimes(claims, now.Unix()); err != nil {
		return nil, err
	}
	if claims["app_id"] != appID {
		return nil, errors.New("app_id mismatch")
	}
	if claims["origin"] != origin {
		return nil, errors.New("origin mismatch")
	}
	return claims, nil
}

func decodeJWTPart(part string, into *map[string]any, useNumber bool) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if useNumber {
		decoder.UseNumber()
	}
	return decoder.Decode(into)
}

func checkJWTTimes(claims map[string]any, now int64) error {
	number := func(name string) (float64, bool, error) {
		value, present := claims[name]
		if !present {
			return 0, false, nil
		}
		parsed, ok := value.(json.Number)
		if !ok {
			return 0, true, fmt.Errorf("%q claim must be a number", name)
		}
		f, err := parsed.Float64()
		return f, true, err
	}
	if _, _, err := number("iat"); err != nil {
		return err
	}
	if nbf, present, err := number("nbf"); err != nil {
		return err
	} else if present && nbf > float64(now) {
		return errors.New(`"nbf" claim timestamp check failed`)
	}
	if exp, present, err := number("exp"); err != nil {
		return err
	} else if present && exp <= float64(now) {
		return errors.New(`"exp" claim timestamp check failed`)
	}
	return nil
}

func (s *cherryJWKS) key(ctx context.Context, kid string, hasKID bool) (*rsa.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil || time.Since(s.fetchedAt) >= cherryJWKSMaxAge {
		if err := s.reload(ctx); err != nil {
			return nil, err
		}
	}
	key, err := s.match(kid, hasKID)
	if errors.Is(err, errCherryNoKey) && time.Since(s.fetchedAt) >= cherryJWKSCooldown {
		if err := s.reload(ctx); err != nil {
			return nil, err
		}
		return s.match(kid, hasKID)
	}
	return key, err
}

func (s *cherryJWKS) reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, cherryJWKSTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	response, err := s.client.Do(req)
	if err != nil {
		slog.Warn("cherry jwks fetch failed", "error", err)
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("expected 200 OK from the JWKS response, got %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, cherryJWKSMaxBytes))
	if err != nil {
		return err
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil || set.Keys == nil {
		return errors.New("JSON Web Key Set malformed")
	}
	s.keys, s.fetchedAt = set.Keys, time.Now()
	return nil
}

func (s *cherryJWKS) match(kid string, hasKID bool) (*rsa.PublicKey, error) {
	var candidates []map[string]any
	for _, jwk := range s.keys {
		if jwk["kty"] != "RSA" {
			continue
		}
		if hasKID && jwk["kid"] != kid {
			continue
		}
		if alg, ok := jwk["alg"].(string); ok && alg != "RS256" {
			continue
		}
		if use, ok := jwk["use"].(string); ok && use != "sig" {
			continue
		}
		if ops, ok := jwk["key_ops"].([]any); ok && !slices.Contains(ops, any("verify")) {
			continue
		}
		candidates = append(candidates, jwk)
	}
	switch len(candidates) {
	case 0:
		return nil, errCherryNoKey
	case 1:
		return rsaKeyFromJWK(candidates[0])
	}
	return nil, errCherryMultipleKey
}

func rsaKeyFromJWK(jwk map[string]any) (*rsa.PublicKey, error) {
	n, _ := jwk["n"].(string)
	e, _ := jwk["e"].(string)
	modulus, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	exponent, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	if len(modulus)<<3 < cherryMinModulus {
		return nil, errors.New("RS256 requires key modulusLength to be 2048 bits or larger")
	}
	exp := new(big.Int).SetBytes(exponent)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
		return nil, errors.New("unsupported RSA public exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exp.Int64())}, nil
}
