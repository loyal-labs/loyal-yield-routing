package fleet

import (
	"bytes"
	"fmt"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func decodePublicKey(value string) ([32]byte, error) {
	var key [32]byte
	raw, err := decodeBase58(value)
	if err != nil || len(raw) != len(key) {
		return key, fmt.Errorf("not a 32-byte base58 value")
	}
	copy(key[:], raw)
	if encodeBase58(key[:]) != value {
		return [32]byte{}, fmt.Errorf("non-canonical base58 public key")
	}
	return key, nil
}

func samePublicKey(raw []byte, value string) bool {
	key, err := decodePublicKey(value)
	return err == nil && bytes.Equal(raw, key[:])
}

func decodeBase58(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("empty base58")
	}
	decoded := []byte{0}
	for _, char := range []byte(value) {
		digit := -1
		for index := range base58Alphabet {
			if base58Alphabet[index] == char {
				digit = index
				break
			}
		}
		if digit < 0 {
			return nil, fmt.Errorf("invalid base58 character")
		}
		carry := digit
		for index := len(decoded) - 1; index >= 0; index-- {
			carry += int(decoded[index]) * 58
			decoded[index] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			decoded = append([]byte{byte(carry)}, decoded...)
			carry >>= 8
		}
	}
	zeros := 0
	for zeros < len(value) && value[zeros] == '1' {
		zeros++
	}
	for len(decoded) > 0 && decoded[0] == 0 {
		decoded = decoded[1:]
	}
	return append(make([]byte, zeros), decoded...), nil
}

func encodeBase58(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	digits := []byte{0}
	for _, octet := range value {
		carry := int(octet)
		for index := len(digits) - 1; index >= 0; index-- {
			carry += int(digits[index]) << 8
			digits[index] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append([]byte{byte(carry % 58)}, digits...)
			carry /= 58
		}
	}
	zeros := 0
	for zeros < len(value) && value[zeros] == 0 {
		zeros++
	}
	out := make([]byte, zeros, zeros+len(digits))
	for _, digit := range digits {
		out = append(out, base58Alphabet[digit])
	}
	return string(out)
}
