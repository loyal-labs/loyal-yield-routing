package fleetexec

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	sdk "github.com/solana-foundation/solana-go/v2"
)

func postFixture(t *testing.T, c sameMintPostContract, slot int64, sourceResidual, idleAmount int64) fixtureAccounts {
	t.Helper()
	accounts := map[string]chain.Account{}
	reserves := []string{c.target}
	if c.sourceKind == "reserve_position" {
		reserves = []string{c.source, c.target}
	}
	owner := sdk.MustPublicKeyFromBase58(c.vault)
	mint := sdk.MustPublicKeyFromBase58(c.mint)
	token := sdk.TokenProgramID
	for i, reserve := range reserves {
		data := make([]byte, 8624)
		copy(data[:8], kamino.ReserveDiscriminator[:])
		binary.LittleEndian.PutUint64(data[8:16], 1)
		binary.LittleEndian.PutUint64(data[16:24], uint64(slot))
		market := sdk.PublicKeyFromBytes(rotateSeed(44, byte(i)))
		copy(data[32:64], market[:])
		copy(data[128:160], mint[:])
		copy(data[408:440], token[:])
		copy(data[2560:2592], rotateSeed(66, byte(i)))
		binary.LittleEndian.PutUint64(data[224:232], 1000000)
		binary.LittleEndian.PutUint64(data[272:280], 6)
		binary.LittleEndian.PutUint64(data[2592:2600], 500000)
		for n := 0; n < 11; n++ {
			binary.LittleEndian.PutUint32(data[4856+64+n*8:], uint32(n*1000))
			binary.LittleEndian.PutUint32(data[4856+68+n*8:], uint32(n*200))
		}
		a := fixtureAccount(reserve, kamino.ProgramID.String(), 1, data)
		accounts[reserve] = a
		_, obligation, _, _, err := reservePostIdentity(&a, c.mint, c.vault)
		if err != nil {
			t.Fatal(err)
		}
		collateral := int64(60)
		if i == 0 && c.sourceKind == "reserve_position" {
			collateral = sourceResidual
		}
		od := make([]byte, 3344)
		copy(od[:8], kamino.ObligationDiscriminator[:])
		copy(od[32:64], market[:])
		copy(od[64:96], owner[:])
		reserveKey := sdk.MustPublicKeyFromBase58(reserve)
		copy(od[96:128], reserveKey[:])
		binary.LittleEndian.PutUint64(od[128:136], uint64(collateral))
		accounts[obligation] = fixtureAccount(obligation, kamino.ProgramID.String(), 1, od)
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, token.String())
	if err != nil {
		t.Fatal(err)
	}
	ad := make([]byte, 165)
	copy(ad[:32], mint[:])
	copy(ad[32:64], owner[:])
	binary.LittleEndian.PutUint64(ad[64:72], uint64(idleAmount))
	ad[108] = 1
	accounts[ata] = fixtureAccount(ata, token.String(), 1, ad)
	return fixtureAccounts{accounts: accounts, slot: slot}
}

func TestSameMintPostObservationKeepsResidualAndCoherentConversions(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	rpc := postFixture(t, c, 1001, 7, 11)
	proof, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if proof.positions[0].amount != 7 || proof.positions[0].redeemable != 14 || proof.positions[1].amount != 60 || proof.idleAmount != 11 {
		t.Fatalf("real custody changed: %+v", proof)
	}
	old := rpc
	old.slot = 999
	if _, err := observeSameMintPost(context.Background(), old, c, chain.Receipt{}, 2.5); err == nil {
		t.Fatal("old post-state accepted")
	}
	// Token account identity is part of custody, even with the expected amount.
	a := rpc.accounts[proof.idleATA]
	copy(a.Data[32:64], make([]byte, 32))
	rpc.accounts[proof.idleATA] = a
	if _, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5); err == nil {
		t.Fatal("external token authority accepted")
	}
}

// Mainnet 4x7SmHXG (vault 1027): the route withdrew the whole source deposit
// and KLend closed the emptied source obligation 78dVfK in the same
// transaction. The closed account proves zero collateral (Rust parity); it
// never proves the deposit target, and reserves stay required.
func TestSameMintPostObservationProvesClosedSourceObligationAsZero(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	closed := func() (fixtureAccounts, string, string) {
		rpc := postFixture(t, c, 1001, 0, 11)
		_, source, _, _, err := reservePostIdentity(rpc.at(c.source), c.mint, c.vault)
		if err != nil {
			t.Fatal(err)
		}
		_, target, _, _, err := reservePostIdentity(rpc.at(c.target), c.mint, c.vault)
		if err != nil {
			t.Fatal(err)
		}
		delete(rpc.accounts, source)
		return rpc, source, target
	}
	rpc, _, target := closed()
	proof, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5)
	if err != nil {
		t.Fatalf("closed source obligation blocked reconciliation: %v", err)
	}
	s, d := proof.positions[0], proof.positions[1]
	if s.reserve != c.source || s.exists || s.amount != 0 || s.redeemable != 0 {
		t.Fatalf("closed source is not a proven zero position: %+v", s)
	}
	if d.reserve != c.target || !d.exists || d.amount != 60 || d.redeemable != 120 || proof.idleAmount != 11 || !proof.idleATAExists {
		t.Fatalf("target custody changed: %+v idle=%d", d, proof.idleAmount)
	}

	rpc, _, _ = closed()
	delete(rpc.accounts, proof.idleATA)
	if p, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5); err != nil || p.idleAmount != 0 || p.idleATAExists {
		t.Fatalf("absent liquidity ATA is not a zero balance: %+v %v", p, err)
	}

	rpc, _, target = closed()
	delete(rpc.accounts, target)
	if _, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5); err == nil {
		t.Fatal("absent target obligation accepted as a landed deposit")
	}
	for _, reserve := range []string{c.source, c.target} {
		rpc, _, _ = closed()
		delete(rpc.accounts, reserve)
		if _, err := observeSameMintPost(context.Background(), rpc, c, chain.Receipt{}, 2.5); err == nil {
			t.Fatalf("absent reserve %s accepted", reserve)
		}
	}
}
