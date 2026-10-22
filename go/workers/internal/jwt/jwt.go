// Package jwt signs and verifies compact HS256 tokens on the standard library.
package jwt

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
)

type Header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

const algHS256 = "HS256"

func encodeSegment(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func decodeSegment(segment string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segment, "="))
	return raw, err == nil
}

func mac(key []byte, unsigned string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(unsigned))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// SignHS256 returns the token for claims under a {"alg":"HS256","typ":"JWT"} header.
func SignHS256(key []byte, claims any) string {
	unsigned := encodeSegment(Header{Alg: algHS256, Typ: "JWT"}) + "." + encodeSegment(claims)
	return unsigned + "." + mac(key, unsigned)
}

// VerifyHS256 returns the header and the raw claims JSON of a token whose
// header names HS256 and whose signature matches key. Claims policy is the caller's.
func VerifyHS256(key []byte, token string) (Header, []byte, bool) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return Header{}, nil, false
	}
	rawHeader, ok := decodeSegment(segments[0])
	var header Header
	if !ok || json.Unmarshal(rawHeader, &header) != nil || header.Alg != algHS256 {
		return Header{}, nil, false
	}
	if !hmac.Equal([]byte(segments[2]), []byte(mac(key, segments[0]+"."+segments[1]))) {
		return Header{}, nil, false
	}
	claims, ok := decodeSegment(segments[1])
	if !ok {
		return Header{}, nil, false
	}
	return header, claims, true
}
