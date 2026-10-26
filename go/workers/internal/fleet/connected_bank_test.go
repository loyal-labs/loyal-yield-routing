package fleet

import (
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func fixtureKey(t *testing.T, data []byte, offset int, address string) {
	t.Helper()
	key, err := decodePublicKey(address)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[offset:offset+32], key[:])
}

func connectedPolicyHeader(t *testing.T, settings, signer string, seed uint64) ([]byte, string) {
	return connectedPolicyHeaderForIndex(t, settings, signer, seed, 0)
}

func connectedPolicyHeaderForIndex(t *testing.T, settings, signer string, seed uint64, vaultIndex uint8) ([]byte, string) {
	t.Helper()
	data, err := buildExactPolicyFixture(settings, signer, vaultIndex, nil)
	if err != nil {
		t.Fatal(err)
	}
	address, bump, err := derivePolicyAccount(settings, seed)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[40:48], seed)
	data[48] = bump
	return data[:110], address
}

func connectedSwapPolicy(t *testing.T, binding CrossMintPolicyBindings, seed uint64) []byte {
	t.Helper()
	data, _ := connectedPolicyHeaderForIndex(t, binding.Settings, binding.DelegatedSigner, seed, binding.VaultIndex)
	data = appendU32x(data, 2)
	for i, disc := range [][]byte{jupiter.RouteV2Discriminator[:], jupiter.SharedAccountsRouteV2Discriminator[:]} {
		key, _ := decodePublicKey(jupiterProgram)
		data = append(data, key[:]...)
		data = appendU32x(data, 2)
		indexes := []byte{0, 2}
		offset := uint64(24)
		if i == 1 {
			indexes = []byte{1, 5}
			offset = 25
		}
		for j, index := range indexes {
			keys := []string{binding.VaultPubkey}
			if j == 1 {
				keys = nil
				for _, mint := range earnStableMints {
					ata, err := deriveATA(binding.VaultPubkey, mint, mustStableProgram(mint))
					if err != nil {
						t.Fatal(err)
					}
					keys = append(keys, ata)
				}
			}
			data = append(data, index, 0)
			data = appendU32x(data, uint32(len(keys)))
			for _, k := range keys {
				key, _ := decodePublicKey(k)
				data = append(data, key[:]...)
			}
			data = append(data, 0)
		}
		data = appendU32x(data, 3)
		data = appendU64x(data, 0)
		data = append(data, 5)
		data = appendU32x(data, uint32(len(disc)))
		data = append(data, disc...)
		data = append(data, 0)
		data = appendU64x(data, offset)
		data = append(data, 1)
		data = appendU16x(data, binding.Swap.MaxSlippageBPS)
		data = append(data, 5)
		data = appendU64x(data, offset+2)
		data = append(data, 0, 0, 0)
	}
	data = append(data, 0, 0)
	data = appendU32x(data, 3)
	for _, mint := range []string{USDCMint, USDTMint, USDSMint} {
		key, _ := decodePublicKey(mint)
		data = append(data, key[:]...)
		data = appendU64x(data, 0)
		data = append(data, 0, 1, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
		data = append(data, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
	}
	data = appendU64x(data, 0)
	data = append(data, 0)
	data = append(data, make([]byte, 32)...)
	return data
}

// connectedEarnPolicyData builds a deployed Earn policy account that pins every
// account of each instruction, accepting both route markets wherever a market
// is, as retained policy discovery requires.
func connectedEarnPolicyData(settings, signer string, instructions []RouteInstruction, positions []KaminoPositionAccounts) ([]byte, error) {
	return connectedEarnPolicyDataForIndex(settings, signer, 0, instructions, positions)
}

func connectedEarnPolicyDataForIndex(settings, signer string, vaultIndex uint8, instructions []RouteInstruction, positions []KaminoPositionAccounts) ([]byte, error) {
	b, err := buildExactPolicyFixture(settings, signer, vaultIndex, nil)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(b[len(b)-4:], uint32(len(instructions)))
	for _, ix := range instructions {
		program, err := decodePublicKey(ix.Program)
		if err != nil {
			return nil, err
		}
		b = append(b, program[:]...)
		b = appendU32x(b, uint32(len(ix.Accounts)))
		for i, account := range ix.Accounts {
			keys := []string{account.Address}
			for _, p := range positions {
				if account.Address == p.Market {
					keys = []string{positions[0].Market, positions[1].Market}
				}
			}
			b = append(b, byte(i), 0)
			b = appendU32x(b, uint32(len(keys)))
			for _, key := range keys {
				decoded, err := decodePublicKey(key)
				if err != nil {
					return nil, err
				}
				b = append(b, decoded[:]...)
			}
			b = append(b, 0)
		}
		b = appendU32x(b, 1)
		b = appendU64x(b, 0)
		b = append(b, 5)
		b = appendU32x(b, uint32(len(ix.Data)))
		b = append(b, ix.Data...)
		b = append(b, 0)
	}
	return b, nil
}
