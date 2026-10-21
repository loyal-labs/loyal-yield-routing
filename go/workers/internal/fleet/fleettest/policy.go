package fleettest

import (
	"encoding/binary"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

type Instruction struct {
	Program string
	Data    []byte
}

func ExactPolicy(settings, signer string, accountIndex uint8, instructions []Instruction) ([]byte, error) {
	s, err := solana.PublicKeyFromBase58(settings)
	if err != nil {
		return nil, err
	}
	sg, err := solana.PublicKeyFromBase58(signer)
	if err != nil {
		return nil, err
	}
	b := append([]byte(nil), squads.PolicyDiscriminator[:]...)
	b = append(b, s[:]...)
	b = append(b, make([]byte, 8+1+8+8)...)
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = append(b, sg[:]...)
	b = append(b, 7)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 3, accountIndex)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(instructions)))
	for _, ix := range instructions {
		p, e := solana.PublicKeyFromBase58(ix.Program)
		if e != nil {
			return nil, e
		}
		b = append(b, p[:]...)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint64(b, 0)
		b = append(b, 5)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(ix.Data)))
		b = append(b, ix.Data...)
		b = append(b, 0)
	}
	return b, nil
}
