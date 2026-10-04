package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"math"
	"math/big"
	"testing"
	"time"
)

func TestRedeemableCollateralUsesWideFractionAndFloors(t *testing.T) {
	data := make([]byte, 8624)
	binary.LittleEndian.PutUint64(data[224:232], 10)
	binary.LittleEndian.PutUint64(data[2592:2600], 3)
	if got, err := redeemableCollateral(data, 2); err != nil || got != 6 {
		t.Fatalf("floor: %d %v", got, err)
	}
	// A half-unit fee remains fractional until the final floor, rather than
	// being rounded away before applying the collateral exchange rate.
	binary.LittleEndian.PutUint64(data[344:352], 1<<59)
	if got, err := redeemableCollateral(data, 2); err != nil || got != 6 {
		t.Fatalf("fractional fee: %d %v", got, err)
	}
	binary.LittleEndian.PutUint64(data[2592:2600], 0)
	if got, err := redeemableCollateral(data, 9); err != nil || got != 9 {
		t.Fatalf("initial ratio: %d %v", got, err)
	}
	binary.LittleEndian.PutUint64(data[2592:2600], 1)
	binary.LittleEndian.PutUint64(data[224:232], math.MaxUint64)
	if _, err := redeemableCollateral(data, math.MaxInt64); err == nil {
		t.Fatal("wide overflow accepted")
	}
	binary.LittleEndian.PutUint64(data[224:232], 0)
	if _, err := redeemableCollateral(data, 1); err == nil {
		t.Fatal("negative fee-adjusted liquidity accepted")
	}
}

func postFixture(t *testing.T, c sameMintPostContract, slot int64, sourceResidual, idleAmount int64) fixtureAccounts {
	t.Helper()
	accounts := map[string]fleet.Account{}
	reserves := []string{c.target}
	if c.sourceKind == "reserve_position" {
		reserves = []string{c.source, c.target}
	}
	owner := sdk.MustPublicKeyFromBase58(c.vault)
	mint := sdk.MustPublicKeyFromBase58(c.mint)
	token := sdk.TokenProgramID
	for i, reserve := range reserves {
		data := make([]byte, 8624)
		copy(data[:8], []byte{43, 242, 204, 202, 26, 247, 59, 127})
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
		a := fleet.Account{Address: reserve, Owner: fleet.KaminoProgram, Lamports: 1, Data: data}
		accounts[reserve] = a
		_, obligation, _, _, err := reservePostIdentity(a, c.mint, c.vault)
		if err != nil {
			t.Fatal(err)
		}
		collateral := int64(60)
		if i == 0 && c.sourceKind == "reserve_position" {
			collateral = sourceResidual
		}
		od := make([]byte, 3344)
		copy(od[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167})
		copy(od[32:64], market[:])
		copy(od[64:96], owner[:])
		reserveKey := sdk.MustPublicKeyFromBase58(reserve)
		copy(od[96:128], reserveKey[:])
		binary.LittleEndian.PutUint64(od[128:136], uint64(collateral))
		accounts[obligation] = fleet.Account{Address: obligation, Owner: fleet.KaminoProgram, Lamports: 1, Data: od}
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
	accounts[ata] = fleet.Account{Address: ata, Owner: token.String(), Lamports: 1, Data: ad}
	return fixtureAccounts{accounts: accounts, slot: slot}
}

func TestSameMintPostObservationKeepsResidualAndCoherentConversions(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	rpc := postFixture(t, c, 1001, 7, 11)
	proof, err := observeSameMintPost(context.Background(), rpc, c, &TransactionReceipt{}, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if proof.positions[0].amount != 7 || proof.positions[0].redeemable != 14 || proof.positions[1].amount != 60 || proof.idleAmount != 11 {
		t.Fatalf("real custody changed: %+v", proof)
	}
	old := rpc
	old.slot = 999
	if _, err := observeSameMintPost(context.Background(), old, c, &TransactionReceipt{}, 400*time.Millisecond); err == nil {
		t.Fatal("old post-state accepted")
	}
	// Token account identity is part of custody, even with the expected amount.
	a := rpc.accounts[proof.idleATA]
	copy(a.Data[32:64], make([]byte, 32))
	rpc.accounts[proof.idleATA] = a
	if _, err := observeSameMintPost(context.Background(), rpc, c, &TransactionReceipt{}, 400*time.Millisecond); err == nil {
		t.Fatal("external token authority accepted")
	}
}

func TestSameMintReconciliationPublishesActualSubsetAndRetainsOtherReserves(t *testing.T) {
	for _, sourceKind := range []string{"reserve_position", "idle_vault_usdc"} {
		t.Run(sourceKind, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			suffix := fmt.Sprint(time.Now().UnixNano())
			b := seedBaseline(t, ctx, pool, suffix)
			wire, _ := integrationWire(t, 91)
			input := fixturePersistInput(t, ctx, pool, b, wire)
			fixture := mustSignedFixture(t)
			vaultHash := sha256.Sum256([]byte(suffix + "vault"))
			vault := sdk.PublicKey(vaultHash).String()
			source := fixture.SecondaryAccount
			target := fixture.RecentBlockhash
			var vaultID int64
			if err := pool.QueryRow(ctx, `SELECT vault_id FROM loyal_yield.rebalance_opportunities WHERE id=$1`, b.OpportunityID).Scan(&vaultID); err != nil {
				t.Fatal(err)
			}
			// Prepare exact family identity before publishing immutable signed evidence.
			if _, err := pool.Exec(ctx, `UPDATE loyal_yield.managed_vaults SET vault_pubkey=$2 WHERE id=$1`, vaultID, vault); err != nil {
				t.Fatal(err)
			}
			var sourceSQL any = source
			if sourceKind == "idle_vault_usdc" {
				sourceSQL = nil
			}
			ata, _ := associatedCustodyAccount(vault, fleet.USDCMint, sdk.TokenProgramID.String())
			plan, _ := json.Marshal(map[string]any{"route_kind": "same_mint", "source_kind": sourceKind, "source_amount_semantics": "kamino_obligation_collateral_deposited_amount", "settings": "settings:" + suffix, "vault_index": 0, "idle_token_account": ata})
			if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET source_reserve=$2,target_reserve=$3,liquidity_mint=$4,execution_plan=$5 WHERE id=$1`, b.OpportunityID, sourceSQL, target, fleet.USDCMint, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET source_reserve=$2,target_reserve=$3,liquidity_mint=$4,execution_plan=$5 WHERE id=$1`, b.DecisionID, sourceSQL, target, fleet.USDCMint, plan); err != nil {
				t.Fatal(err)
			}
			id, _, err := store.PersistSignedRoute(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			lease := claimOne(t, ctx, store, b.Cluster, "post-observer")
			if err := store.ConfirmSameMint(ctx, lease, 1000); err != nil {
				t.Fatal(err)
			}
			lease = claimOne(t, ctx, store, b.Cluster, "post-observer")
			c, err := store.loadPostContract(ctx, lease.Submission)
			if err != nil {
				t.Fatal(err)
			}
			// An unrelated product reserve remains current after observed-subset patch.
			if _, err := pool.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,liquidity_mint,amount_raw,has_value,snapshot_id,observed_slot,observed_at) SELECT $1,'unobserved', $2,17,true,id,observed_slot,observed_at FROM loyal_yield.vault_position_snapshots WHERE vault_id=$1 AND is_current`, vaultID, fleet.USDCMint); err != nil {
				t.Fatal(err)
			}
			tx, err := sdk.TransactionFromBytes(wire.SignedTransaction)
			if err != nil {
				t.Fatal(err)
			}
			message, _ := tx.Message.MarshalBinary()
			receipt := &TransactionReceipt{Slot: 1000, Signature: wire.TransactionSignature, SignedTransaction: wire.SignedTransaction, MessageB64: base64.StdEncoding.EncodeToString(message)}
			proof, err := observeSameMintPost(ctx, postFixture(t, c, 1001, 7, 11), c, receipt, 400*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.publishSameMintPost(ctx, lease, proof); err != nil {
				t.Fatal(err)
			}
			state, _, _, _, _ := durableRow(t, ctx, pool, id)
			if state != "reconciled" {
				t.Fatalf("submission state %s", state)
			}
			var decision, opportunity, capacity string
			var snapshot int64
			if err := pool.QueryRow(ctx, `SELECT d.status::text,d.post_snapshot_id,o.opportunity_state,r.reservation_state FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id JOIN loyal_yield.target_capacity_reservations r ON r.opportunity_id=o.id WHERE d.id=$1`, b.DecisionID).Scan(&decision, &snapshot, &opportunity, &capacity); err != nil {
				t.Fatal(err)
			}
			if decision != "confirmed" || opportunity != "completed" || capacity != "awaiting_telemetry" {
				t.Fatalf("custody closure %s %s %s", decision, opportunity, capacity)
			}
			var amount int64
			if err := pool.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve='unobserved'`, vaultID).Scan(&amount); err != nil || amount != 17 {
				t.Fatalf("unobserved reserve overwritten: %d %v", amount, err)
			}
			if sourceKind == "reserve_position" {
				if err := pool.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$2`, vaultID, source).Scan(&amount); err != nil || amount != 7 {
					t.Fatalf("source residual erased: %d %v", amount, err)
				}
			} else {
				if err := pool.QueryRow(ctx, `SELECT amount_raw FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=$1 AND mint=$2`, vaultID, fleet.USDCMint).Scan(&amount); err != nil || amount != 11 {
					t.Fatalf("idle surplus erased: %d %v", amount, err)
				}
			}
		})
	}
}

// Test conversion at a fractional boundary that float64 cannot represent.
func TestRedeemableCollateralDoesNotRoundLargeRawBalances(t *testing.T) {
	data := make([]byte, 8624)
	binary.LittleEndian.PutUint64(data[224:232], (1<<53)+3)
	binary.LittleEndian.PutUint64(data[2592:2600], 3)
	got, err := redeemableCollateral(data, 2)
	want := new(big.Int).Quo(new(big.Int).Mul(big.NewInt((1<<53)+3), big.NewInt(2)), big.NewInt(3)).Int64()
	if err != nil || got != want {
		t.Fatalf("exact raw: got %d want %d err %v", got, want, err)
	}
}
