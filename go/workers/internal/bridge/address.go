package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/mr-tron/base58"
	"golang.org/x/crypto/sha3"
)

func ParseRecipient(kind AddressKind, s string) (string, error) {
	switch kind {
	case AddressEVM:
		return parseEVMRecipient(s)
	case AddressTron:
		return parseTronRecipient(s)
	}
	return "", fmt.Errorf("unknown address kind %q", kind)
}

func parseEVMRecipient(s string) (string, error) {
	if !isAddress(s) {
		return "", fmt.Errorf("not a 0x address of 20 bytes")
	}
	lower := strings.ToLower(s)
	if s != lower && s != ChecksumAddress(lower) {
		return "", fmt.Errorf("EIP-55 checksum does not match")
	}
	return lower, nil
}

func ChecksumAddress(address string) string {
	lower := strings.ToLower(address[2:])
	hash := sha3.NewLegacyKeccak256()
	hash.Write([]byte(lower))
	sum := hex.EncodeToString(hash.Sum(nil))
	out := []byte(lower)
	for i, c := range out {
		if c >= 'a' && c <= 'f' && sum[i] >= '8' {
			out[i] = c - 'a' + 'A'
		}
	}
	return "0x" + string(out)
}

func parseTronRecipient(s string) (string, error) {
	raw, err := base58.Decode(s)
	if err != nil || len(raw) != 25 || raw[0] != 0x41 {
		return "", fmt.Errorf("not a Tron address")
	}
	first := sha256.Sum256(raw[:21])
	second := sha256.Sum256(first[:])
	if !bytes.Equal(raw[21:], second[:4]) {
		return "", fmt.Errorf("Tron checksum does not match")
	}
	return s, nil
}
