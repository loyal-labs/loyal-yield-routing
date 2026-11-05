package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

type HS256 struct{ key []byte }

const hs256Header = `{"alg":"HS256","typ":"JWT"}`

func NewHS256(key []byte) *HS256 { return &HS256{key: append([]byte(nil), key...)} }

func (k *HS256) mac(data string) []byte {
	mac := hmac.New(sha256.New, k.key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func (k *HS256) SealHS256(payload []byte) string {
	signing := base64.RawURLEncoding.EncodeToString([]byte(hs256Header)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signing + "." + base64.RawURLEncoding.EncodeToString(k.mac(signing))
}

func (k *HS256) OpenHS256(token string) ([]byte, bool) {
	first := strings.IndexByte(token, '.')
	last := strings.LastIndexByte(token, '.')
	if first < 1 || last == first || strings.Count(token, ".") != 2 {
		return nil, false
	}
	signature, err := decodeSegment(strings.TrimRight(token[last+1:], "="))
	if err != nil || !hmac.Equal(signature, k.mac(token[:last])) {
		return nil, false
	}
	payload, err := decodeSegment(token[first+1 : last])
	if err != nil {
		return nil, false
	}
	return payload, true
}

func (k *HS256) DeriveHS256(info string) []byte { return k.mac(info) }

func (k *HS256) Keys() Keys { return NewRawHMACKeys(k.key) }
