// Package jwt verifies compact JWS tokens on the standard library.
package jwt

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"time"
)

type Keys struct {
	RS256 *rsa.PublicKey
	HS256 []byte
}

type Audience string

const NoAudience Audience = ""

var (
	ErrMalformed   = errors.New("jwt: malformed token")
	ErrAlgorithm   = errors.New("jwt: algorithm not accepted by the configured keys")
	ErrSignature   = errors.New("jwt: signature mismatch")
	ErrExpired     = errors.New("jwt: token expired")
	ErrNotYetValid = errors.New("jwt: token not yet valid")
	ErrAudience    = errors.New("jwt: audience mismatch")
)

type header struct {
	Alg  string          `json:"alg"`
	Crit json.RawMessage `json:"crit"`
}

type registered struct {
	Exp *float64        `json:"exp"`
	Nbf *float64        `json:"nbf"`
	Aud json.RawMessage `json:"aud"`
}

func (k Keys) Verify(token string, want Audience, now time.Time) ([]byte, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return nil, ErrMalformed
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h header
	if json.Unmarshal(rawHeader, &h) != nil {
		return nil, ErrMalformed
	}
	if h.Crit != nil {
		return nil, ErrAlgorithm
	}
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		return nil, ErrMalformed
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	switch {
	case h.Alg == "RS256" && k.RS256 != nil:
		if rsa.VerifyPKCS1v15(k.RS256, crypto.SHA256, digest[:], signature) != nil {
			return nil, ErrSignature
		}
	case h.Alg == "HS256" && len(k.HS256) > 0:
		mac := hmac.New(sha256.New, k.HS256)
		mac.Write([]byte(segments[0] + "." + segments[1]))
		if !hmac.Equal(signature, mac.Sum(nil)) {
			return nil, ErrSignature
		}
	default:
		return nil, ErrAlgorithm
	}
	claims, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var object map[string]json.RawMessage
	var r registered
	if json.Unmarshal(claims, &object) != nil || object == nil || json.Unmarshal(claims, &r) != nil {
		return nil, ErrMalformed
	}
	seconds := float64(now.Unix())
	if r.Exp == nil || *r.Exp <= seconds {
		return nil, ErrExpired
	}
	if r.Nbf != nil && *r.Nbf > seconds {
		return nil, ErrNotYetValid
	}
	if !audienceMatches(r.Aud, want) {
		return nil, ErrAudience
	}
	return claims, nil
}

func audienceMatches(raw json.RawMessage, want Audience) bool {
	if want == NoAudience {
		return raw == nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == string(want)
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, aud := range many {
		if aud == string(want) {
			return true
		}
	}
	return false
}

func ParseRS256PublicKey(text string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("jwt: RS256 public key is not a PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("jwt: RS256 public key is not valid SPKI")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("jwt: RS256 public key must be RSA of at least 2048 bits")
	}
	return key, nil
}
