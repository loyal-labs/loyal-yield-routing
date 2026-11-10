package backyard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func TestBackyardDirectWithdrawalRetainsReceiptAndCostAccounting(t *testing.T) {
	db, ctx := withdrawalCostTestDB(t), context.Background()
	v := looping
	owner := solana.NewWallet().PublicKey().String()
	address, data := receiptFixture(t, v.VoltrVault, owner, 100, 1000<<48)
	vault, mint := voltrVaultFixture(t, 10000), voltrLPMintFixture(t, 1000)
	vault.Address, mint.Address = v.VoltrVault, v.lpMint
	accounts := []ConfirmedAccount{vault, mint, {Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: data}}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 77, accounts)
	view := &View{accounts: map[string]viewAccount{}, receipts: map[string]uint64{address: 0}, through: 77, advanced: make(chan struct{})}
	for _, a := range accounts {
		view.accounts[a.Address] = viewAccount{ConfirmedAccount: a, slot: 77}
	}
	o := Observation{ObservedAt: time.Now(), routeBatch: &routeObservationBatch{Accounts: accounts}, Snapshot: Snapshot{Slot: 77, ChainUnix: time.Now().Unix(), TotalVaultNAVRaw: 10000, WithdrawalDemandRaw: 1000}}
	net := &certifiedNAV{vault: v.squads, manifest: v.manifest.SHA256, slot: 77, validUntil: time.Now().Add(exitQuoteInterval)}
	o.Snapshot.net = net
	p := &withdrawalCostPreparation{Slot: 77, view: view, chain: rpc, manifest: v.manifest, net: net, NetworkUSDC: withdrawalTestUSDC(1), NetworkLamports: 5000, MessageSHA256: strings.Repeat("b", 64)}
	if err := p.load(ctx, db, rpc, o); err != nil {
		t.Fatal("non-admin wallet not admitted", err)
	}
	if len(p.Participants) != 1 || p.Participants[0].class() != withdrawalOwnerDirect || p.Participants[0].Wait != "" || p.Participants[0].consented() {
		t.Fatal("direct receipt treated as user consent", p.Participants)
	}
	if err := p.allocate(20, 0); err != nil {
		t.Fatal(err)
	}
	if p.OtherHolders+p.Participants[0].MaxCost != 20 {
		t.Fatal("cost attribution lost")
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES('direct',$1,'decided')`, v.RouteKey); err != nil {
		t.Fatal(err)
	}
	if err := db.reserveWithdrawalCostRecord(ctx, "direct", p); err != nil {
		t.Fatal("direct cost not journaled", err)
	}
	var recorded bool
	if err := db.pool.QueryRow(ctx, `SELECT owner_class='direct' AND authority=$1 AND receipt=$2 AND consent_id IS NULL AND wait_reason IS NULL FROM loyal_yield.backyard_withdrawal_cost_reservations WHERE operation_id='direct'`, owner, address).Scan(&recorded); err != nil || !recorded {
		t.Fatal("receipt accounting missing", err)
	}
	o.Snapshot.Target = LaneTarget{Rule: ruleWithdrawal}
	d := Decision{Action: SwapCollateralToStableStep}
	r := releaseRequestOf(v, o.Snapshot, d, withdrawalCostRequest{cost: p})
	if r == nil || len(r.owners) != 1 || r.owners[0] != owner {
		t.Fatal("operator release control lost wallet scope", r)
	}
	bad := view.accounts[address]
	bad.Owner = solana.SystemProgramID.String()
	view.accounts[address] = bad
	if err := p.load(ctx, db, rpc, o); err == nil {
		t.Fatal("invalid receipt accepted")
	}
}

func TestBackyardReserveScalesWithoutReservingTheWholeTestVault(t *testing.T) {
	p := productionLoopingPolicy
	for _, c := range []struct{ nav, reserve int64 }{{17_000_000, 1_700_000}, {1_000_000_000, 100_000_000}, {175_000_000_000, 17_500_000_000}, {300_000_000_000, 25_000_000_000}} {
		if got := p.idleBufferRaw(c.nav); got != c.reserve {
			t.Fatalf("reserve %d for NAV %d, want %d", got, c.nav, c.reserve)
		}
	}
	s := Snapshot{Slot: 77, RouteLane: MapleSyrupUSDCLaneID, TotalVaultNAVRaw: 1_000_000_000}
	s.net = fixtureSnapshotCertificate(s)
	for _, lane := range []string{MapleSyrupUSDCLaneID, primePRIMEUSDCTemplate.Lane, onreONycUSDC} {
		limits := newAllocationLimits(s, nil, p, lane)
		if !limits.accepts(MoveQuote{EquityRaw: 900_000_000, Unlevered: true}) || limits.CashReserveRaw != 100_000_000 {
			t.Fatalf("%s forced a percentage allocation or lost its cash reserve: %+v", lane, limits)
		}
	}
}
