package referral

import (
	"crypto/rand"
	"errors"
	"strings"
)

const (
	crockford  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeLength = 7
)

type Code string

var ErrMalformedCode = errors.New("referral: a code is 7 Crockford base32 characters")

func ParseCode(raw string) (Code, error) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != codeLength || strings.Trim(code, crockford) != "" {
		return "", ErrMalformedCode
	}
	return Code(code), nil
}

func newCode() Code {
	var b [codeLength]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = crockford[b[i]%byte(len(crockford))]
	}
	return Code(b[:])
}
