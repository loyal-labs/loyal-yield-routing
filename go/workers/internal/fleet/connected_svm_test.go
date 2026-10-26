package fleet

import (
	"testing"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// fixtureAccount is address's account as a test chain holds it.
func fixtureAccount(address, owner string, lamports uint64, data []byte) chain.Account {
	return chain.Account{Key: solana.MustPublicKeyFromBase58(address), Owner: solana.MustPublicKeyFromBase58(owner), Lamports: lamports, Data: data}
}

// testChain is the chain client against a local test endpoint.
func testChain(t *testing.T, url string) *chain.Client {
	t.Helper()
	client, err := chain.New(url, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
