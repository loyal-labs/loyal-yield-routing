package earnprep

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"slices"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

var (
	programConfigDiscriminator      = anchor.Account("ProgramConfig")
	createSmartAccountDiscriminator = anchor.Instruction("create_smart_account")
)

type ProgramConfig struct {
	SmartAccountIndex *big.Int
	Treasury          solana.PublicKey
}

func ProgramConfigAddress() (solana.PublicKey, error) {
	address, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("program_config")}, squads.ProgramID)
	return address, err
}

func DecodeProgramConfig(account *chain.Account) (ProgramConfig, error) {
	if account == nil || account.Owner != squads.ProgramID || account.Executable {
		return ProgramConfig{}, errors.New("program config account is absent or not owned by the smart account program")
	}
	data := account.Data
	if len(data) < 96 || !bytes.Equal(data[:8], programConfigDiscriminator[:]) {
		return ProgramConfig{}, errors.New("account is not a smart account ProgramConfig")
	}
	index := make([]byte, 16)
	for i := range index {
		index[i] = data[8+15-i]
	}
	return ProgramConfig{
		SmartAccountIndex: new(big.Int).SetBytes(index),
		Treasury:          solana.PublicKeyFromBytes(data[64:96]),
	}, nil
}

func SettingsForIndex(accountIndex *big.Int) (solana.PublicKey, error) {
	if accountIndex.Sign() < 0 || accountIndex.BitLen() > 128 {
		return solana.PublicKey{}, errors.New("settings account index is outside u128")
	}
	var seed [16]byte
	be := accountIndex.FillBytes(make([]byte, 16))
	for i := range seed {
		seed[i] = be[15-i]
	}
	settings, _, err := squads.SettingsAddress(seed)
	return settings, err
}

type SponsoredCreate struct {
	Sponsor   solana.PublicKey
	Treasury  solana.PublicKey
	Settings  solana.PublicKey
	Wallet    solana.PublicKey
	Blockhash solana.Hash
}

func (c SponsoredCreate) Compile() (*solana.Transaction, error) {
	programConfig, err := ProgramConfigAddress()
	if err != nil {
		return nil, err
	}
	data := slices.Concat(createSmartAccountDiscriminator[:], []byte{0})
	data = binary.LittleEndian.AppendUint16(data, 1)
	data = binary.LittleEndian.AppendUint32(data, 1)
	data = append(data, c.Wallet[:]...)
	data = append(data, squads.FullPermissions)
	data = binary.LittleEndian.AppendUint32(data, 0)
	data = append(data, 0, 0)
	create := squads.Instruction{
		ProgramID: squads.ProgramID,
		Accounts: []solana.AccountMeta{
			{PublicKey: programConfig, IsWritable: true},
			{PublicKey: c.Treasury, IsWritable: true},
			{PublicKey: c.Sponsor, IsWritable: true, IsSigner: true},
			{PublicKey: solana.SystemProgramID},
			{PublicKey: squads.ProgramID},
			{PublicKey: c.Settings, IsWritable: true},
		},
		Data: data,
	}
	return CompileV0(c.Sponsor, c.Blockhash, []squads.Instruction{create}, nil)
}
