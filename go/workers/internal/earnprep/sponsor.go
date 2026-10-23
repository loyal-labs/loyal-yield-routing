package earnprep

import (
	"errors"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
)

type Sponsor struct{ key solana.PrivateKey }

func NewSponsor(secretBase58 string) (*Sponsor, error) {
	secretBase58 = strings.TrimSpace(secretBase58)
	if secretBase58 == "" {
		return nil, nil
	}
	key, err := solana.PrivateKeyFromBase58(secretBase58)
	if err != nil || len(key) != 64 {
		return nil, errors.New("SMART_ACCOUNT_SPONSOR_PK: want a base58 64-byte Solana secret key")
	}
	return &Sponsor{key: key}, nil
}

func (s *Sponsor) PublicKey() solana.PublicKey { return s.key.PublicKey() }

func (s *Sponsor) Cosign(tx *solana.Transaction) error {
	pub := s.key.PublicKey()
	if !tx.IsSigner(pub) {
		return errors.New("earnprep: the sponsor is not a signer of this transaction")
	}
	_, err := tx.PartialSign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == pub {
			return &s.key
		}
		return nil
	})
	return err
}
