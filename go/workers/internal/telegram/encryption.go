package telegram

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

type MessageCipher struct {
	aead cipher.AEAD
}

type sealed struct {
	Ciphertext string `json:"ciphertext"`
	IV         string `json:"iv"`
}

func NewMessageCipher(keyBase64 string) (*MessageCipher, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, errors.New("MESSAGE_ENCRYPTION_KEY: not standard base64")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("MESSAGE_ENCRYPTION_KEY: want 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("MESSAGE_ENCRYPTION_KEY: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("MESSAGE_ENCRYPTION_KEY: %w", err)
	}
	return &MessageCipher{aead: aead}, nil
}

func (c *MessageCipher) seal(plaintext []byte) sealed {
	iv := make([]byte, c.aead.NonceSize())
	rand.Read(iv)
	return sealed{
		Ciphertext: base64.StdEncoding.EncodeToString(c.aead.Seal(nil, iv, plaintext, nil)),
		IV:         base64.StdEncoding.EncodeToString(iv),
	}
}

func (c *MessageCipher) open(s sealed) ([]byte, error) {
	iv, err := base64.StdEncoding.DecodeString(s.IV)
	if err != nil || len(iv) != c.aead.NonceSize() {
		return nil, errors.New("decrypt: bad iv")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(s.Ciphertext)
	if err != nil {
		return nil, errors.New("decrypt: bad ciphertext encoding")
	}
	plaintext, err := c.aead.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, errors.New("decrypt: authentication failed")
	}
	return plaintext, nil
}
