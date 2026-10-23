package jwt

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"time"
)

type Signer struct {
	rs256 *rsa.PrivateKey
	hs256 []byte
}

func NewSigner(authSecret, rs256PrivateKeyPEM string) (*Signer, error) {
	if rs256PrivateKeyPEM != "" {
		key, err := parseRS256PrivateKey(rs256PrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		return &Signer{rs256: key}, nil
	}
	if authSecret == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(authSecret))
	return &Signer{hs256: sum[:]}, nil
}

func (s *Signer) Mint(claims map[string]any, aud Audience, ttl time.Duration, now time.Time) (string, error) {
	payload := maps.Clone(claims)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["iat"] = now.Unix()
	payload["exp"] = now.Add(ttl).Unix()
	if aud != NoAudience {
		payload["aud"] = string(aud)
	}
	alg := "HS256"
	if s.rs256 != nil {
		alg = "RS256"
	}
	head, err := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	var signature []byte
	if s.rs256 != nil {
		digest := sha256.Sum256([]byte(signing))
		if signature, err = rsa.SignPKCS1v15(rand.Reader, s.rs256, crypto.SHA256, digest[:]); err != nil {
			return "", err
		}
	} else {
		mac := hmac.New(sha256.New, s.hs256)
		mac.Write([]byte(signing))
		signature = mac.Sum(nil)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseRS256PrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("jwt: RS256 private key is not a PEM PRIVATE KEY (PKCS#8) block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("jwt: RS256 private key is not valid PKCS#8")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("jwt: RS256 private key must be RSA of at least 2048 bits")
	}
	return key, nil
}
