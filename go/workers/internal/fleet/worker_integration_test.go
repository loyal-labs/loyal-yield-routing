package fleet

import (
	"encoding/binary"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// obligationAccount is the vault's vanilla obligation in market holding
// collateral in each reserve.
func obligationAccount(t *testing.T, vault, market string, collateral map[string]uint64) chain.Account {
	t.Helper()
	address, err := kamino.VanillaObligation(solana.MustPublicKeyFromBase58(vault), solana.MustPublicKeyFromBase58(market))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, kamino.ObligationSize)
	copy(data, kamino.ObligationDiscriminator[:])
	fixtureKey(t, data, 32, market)
	fixtureKey(t, data, 64, vault)
	slot := 0
	for reserve, amount := range collateral {
		fixtureKey(t, data, 96+slot*136, reserve)
		binary.LittleEndian.PutUint64(data[96+slot*136+32:], amount)
		slot++
	}
	return fixtureAccount(address.String(), kamino.ProgramID.String(), 23_942_400, data)
}
