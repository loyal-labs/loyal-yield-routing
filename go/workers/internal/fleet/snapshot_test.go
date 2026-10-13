package fleet

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// snapshotNode serves accounts at one slot and records each call's slot
// floor.
type snapshotNode struct {
	mu       sync.Mutex
	slot     uint64
	accounts map[string]chain.Account
	floors   []uint64
}

func (n *snapshotNode) Accounts(_ context.Context, keys []solana.PublicKey, _ rpc.CommitmentType, minContextSlot uint64) (uint64, []*chain.Account, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.floors = append(n.floors, minContextSlot)
	out := make([]*chain.Account, len(keys))
	for i, key := range keys {
		if account, ok := n.accounts[key.String()]; ok {
			out[i] = &account
		}
	}
	return n.slot, out, nil
}

// One snapshot is the catalog reserves at a slot and every vault account
// read at or after it: each catalog reserve routable or named unroutable,
// each obligation's catalog deposits valued at that slot's exchange rate,
// each named policy decoded or its failure kept, and idle cash per mint.
func TestReadSnapshotReadsEveryVaultFactAtTheReservesSlot(t *testing.T) {
	market, otherMarket := testIdentity(40), testIdentity(41)
	routable := ReserveIdentity{Address: testIdentity(3), Market: market, Mint: USDCMint}
	absent := ReserveIdentity{Address: testIdentity(4), Market: market, Mint: USDCMint}
	drifted := ReserveIdentity{Address: testIdentity(5), Market: market, Mint: USDTMint}
	outside := ReserveIdentity{Address: testIdentity(6), Market: otherMarket, Mint: USDCMint}
	node := &snapshotNode{slot: 1_000, accounts: map[string]chain.Account{}}
	put := func(account chain.Account) { node.accounts[account.Key.String()] = account }
	reserve := reserveFixture(routable, 1_000_000_000_000_000, 1_000_000_000_000_000)
	// 2e15 liquidity over 1e15 collateral: one unit redeems two.
	binary.LittleEndian.PutUint64(reserve.Data[2592:2600], 1_000_000_000_000_000)
	put(reserve)
	// Listed as USDT, the account holds USDC: the listing drifted.
	put(reserveFixture(ReserveIdentity{Address: drifted.Address, Market: market, Mint: USDCMint}, 1, 1))

	settings, signer := testIdentity(60), testIdentity(61)
	held, bare, foreign := testIdentity(70), testIdentity(71), testIdentity(72)
	policyAccount, unreadable := testIdentity(80), testIdentity(81)
	policyData, err := BuildExactPolicyFixture(settings, signer, 0, []RouteInstruction{{Program: kamino.ProgramID.String(), Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	put(fixtureAccount(policyAccount, squads.ProgramID.String(), 1, policyData))
	put(fixtureAccount(unreadable, squads.ProgramID.String(), 1, []byte{1, 2, 3}))
	put(obligationAccount(t, held, market, map[string]uint64{routable.Address: 1_000, outside.Address: 9_000}))
	idle, err := spl.AssociatedTokenAddress(solana.MustPublicKeyFromBase58(held), solana.MustPublicKeyFromBase58(USDCMint), solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	idleData := make([]byte, 165)
	usdc, vault := solana.MustPublicKeyFromBase58(USDCMint), solana.MustPublicKeyFromBase58(held)
	copy(idleData[0:32], usdc[:])
	copy(idleData[32:64], vault[:])
	binary.LittleEndian.PutUint64(idleData[64:72], 7_000_000)
	idleData[108] = 1
	put(fixtureAccount(idle.String(), solana.TokenProgramID.String(), 1, idleData))

	request := SnapshotRequest{Catalog: []ReserveIdentity{routable, absent, drifted}, SlotsPerSecond: 2.5, Idle: true,
		Vaults: []SnapshotVault{{Pubkey: held, PolicyAccount: policyAccount, Markets: []string{market, otherMarket}}, {Pubkey: bare, PolicyAccount: unreadable, Markets: []string{market}}}}
	snapshot, err := ReadSnapshot(context.Background(), node, request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Slot != 1_000 || len(snapshot.Reserves) != 1 || snapshot.Reserves[routable.Address].Slot != 1_000 ||
		snapshot.Unroutable[absent.Address] != "absent" || snapshot.Unroutable[drifted.Address] != "identity_or_layout_drift" {
		t.Fatalf("reserves %+v unroutable %v", snapshot.Reserves, snapshot.Unroutable)
	}
	if node.floors[0] != 0 || len(node.floors) < 2 {
		t.Fatalf("floors %v", node.floors)
	}
	for _, floor := range node.floors[1:] {
		if floor != 1_000 {
			t.Fatalf("a vault read was not pinned to the reserves' slot: %v", node.floors)
		}
	}
	facts := snapshot.Vaults[held]
	want := []Holding{{Reserve: routable.Address, Market: market, Mint: USDCMint, CollateralRaw: 1_000, LiquidityRaw: 2_000}}
	if len(facts.Holdings) != 1 || facts.Holdings[0] != want[0] {
		t.Fatalf("holdings %+v, want %+v (a non-catalog deposit is not a holding)", facts.Holdings, want)
	}
	if facts.Policy == nil || facts.Policy.Settings != settings || facts.Idle[USDCMint] != 7_000_000 || facts.Idle[USDTMint] != 0 {
		t.Fatalf("held vault facts %+v", facts)
	}
	if b := snapshot.Vaults[bare]; b.Policy != nil || b.PolicyErr == "" || len(b.Holdings) != 0 {
		t.Fatalf("unreadable policy vault %+v", b)
	}

	// Holdings only: no reserve is priced, holdings still convert.
	request.SlotsPerSecond = 0
	if snapshot, err = ReadSnapshot(context.Background(), node, request); err != nil || len(snapshot.Reserves) != 0 || snapshot.Vaults[held].Holdings[0].LiquidityRaw != 2_000 {
		t.Fatalf("holdings-only snapshot %+v %v", snapshot, err)
	}

	// An obligation at the vault's address that another owner holds is not
	// the vault's: the snapshot fails rather than guess.
	put(obligationAccount(t, foreign, market, map[string]uint64{routable.Address: 1}))
	stolen := node.accounts
	address, _ := kamino.VanillaObligation(solana.MustPublicKeyFromBase58(held), solana.MustPublicKeyFromBase58(market))
	foreignAddress, _ := kamino.VanillaObligation(solana.MustPublicKeyFromBase58(foreign), solana.MustPublicKeyFromBase58(market))
	moved := stolen[foreignAddress.String()]
	moved.Key = address
	stolen[address.String()] = moved
	if _, err := ReadSnapshot(context.Background(), node, request); err == nil {
		t.Fatal("another owner's obligation was read as the vault's")
	}
	if _, err := ReadSnapshot(context.Background(), node, SnapshotRequest{}); err == nil {
		t.Fatal("a snapshot without a catalog was read")
	}
}
