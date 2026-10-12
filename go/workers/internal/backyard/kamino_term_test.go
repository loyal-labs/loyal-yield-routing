package backyard

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// The live wYLDS reserve 34GJ… lends for 30 days with no market-wide
// maturity, and Kamino Multiply's obligation EsDb54… last borrowed from it at
// 1791697379 (testdata/wylds-term-debt.json, slot 455559961): the obligation
// may borrow until its term ends at 1794289379 and not from then on, and a
// market-wide maturity closes the reserve to every obligation.
func TestWYLDSTermReadersFollowTheLiveTerm(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../programs/kamino/testdata/wylds-term-debt.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Accounts map[string]struct {
			Owner    solana.PublicKey
			Lamports uint64
			Data     []byte
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	const multiply = "EsDb54VmT1DsgamjQtnWuZmoLAyrQCr6bsMu3AuZnies"
	var accounts []ConfirmedAccount
	for address, a := range fixture.Accounts {
		accounts = append(accounts, ConfirmedAccount{Address: address, Owner: a.Owner.String(), Lamports: a.Lamports, Data: a.Data})
	}
	owner, err := kamino.DecodeObligation(chainAccount(accountAt(accounts, multiply), multiply))
	if err != nil {
		t.Fatal(err)
	}
	route := autoAUTOWYLDS
	route.Kamino.Obligation, route.Kamino.Vault = multiply, owner.Owner.String()
	end, err := observedDebtTermEnd(accounts, route)
	if err != nil || end != 1_791_697_379+2_592_000 {
		t.Fatalf("term end %d (%v)", end, err)
	}
	reserve, err := decodeKaminoReserve(accountAt(accounts, route.Kamino.DebtReserve), route.Kamino.DebtMint, route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, multiply), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	if !kaminoObligationTermOpen(obligation, reserve, int64(end)-1) || kaminoObligationTermOpen(obligation, reserve, int64(end)) {
		t.Fatal("the obligation's own term does not close borrowing at its end")
	}
	if !kaminoReserveTermOpen(reserve, int64(end)+1) {
		t.Fatal("a reserve with only a per-obligation term refuses every borrow")
	}
	matured := reserve
	matured.terms.DebtMaturityTimestamp = uint64(end) - 100
	if kaminoReserveTermOpen(matured, int64(end)-100) || !kaminoReserveTermOpen(matured, int64(end)-101) {
		t.Fatal("the market-wide maturity does not close the reserve at its timestamp")
	}
	if fresh := (decodedKaminoObligation{}); !kaminoObligationTermOpen(fresh, reserve, int64(end)+1_000_000) {
		t.Fatal("an obligation without debt is held to a term it never started")
	}
	if reserve.terms.ScopePriceFeed.String() != kaminoScopePrices {
		t.Fatal("the wYLDS reserve refreshes through another scope feed than the Kamino refresh pins")
	}
}
