package earnmax

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func TestWithdrawalCostStatusGenerationVersionsAndUnknown(t *testing.T) {
	db := withdrawalConsentTestDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE loyal_yield.multiply_operations(operation_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../migrations/yield/0140_earn_max_withdrawal_costs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	c, _, _, _ := signedConsent(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	c.Owner = solana.PublicKeyFromBytes(key.Public().(ed25519.PublicKey)).String()
	c.Destination = c.Owner
	sign := func(c *WithdrawalConsent) {
		message, err := c.CanonicalMessage()
		if err != nil {
			t.Fatal(err)
		}
		c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, message))
	}
	sign(&c)
	store := NewWithdrawalConsentStore(db)
	id1, err := store.Accept(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	c2 := c
	c2.Version = "2"
	c2.Nonce = strings.Repeat("c", 64)
	c2.MinClaimUSDC = "980"
	c2.MaxUserFeeLamports = "20000"
	sign(&c2)
	id2, err := store.Accept(ctx, c2)
	if err != nil {
		t.Fatal(err)
	}
	other := c
	other.Nonce = strings.Repeat("d", 64)
	other.RequestSignature = solana.Signature{3}.String()
	sign(&other)
	otherID, err := store.Accept(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, consentID, state string, c WithdrawalConsent, cost int64) {
		t.Helper()
		if _, err := db.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations VALUES($1)`, id); err != nil {
			t.Fatal(err)
		}
		_, err := db.Exec(ctx, `INSERT INTO loyal_yield.backyard_withdrawal_cost_operations(operation_id,evidence_digest,message_sha256,effective_lp,max_cost_usdc,other_holders_usdc,max_incremental_cost_usdc,incremental_other_holders_usdc,network_usdc,network_lamports,evidence,state,receipt_signature,receipt_instruction,finalized_slot,actual_cost_usdc,actual_other_holders_usdc,actual_incremental_cost_usdc,actual_incremental_other_holders_usdc,actual_fee_lamports) VALUES($1,$2,$2,100,$3::bigint,0,$3::bigint,0,999999,5000,'{}',$4,CASE WHEN $4='settled' THEN $1 ELSE NULL END,CASE WHEN $4='settled' THEN 0 ELSE NULL END,CASE WHEN $4='settled' THEN 1 ELSE NULL END,CASE WHEN $4='settled' THEN $3::bigint ELSE NULL END,CASE WHEN $4='settled' THEN 0 ELSE NULL END,CASE WHEN $4='settled' THEN $3::bigint ELSE NULL END,CASE WHEN $4='settled' THEN 0 ELSE NULL END,CASE WHEN $4='settled' THEN 5000 ELSE NULL END)`, id, strings.Repeat("a", 64), cost, state)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(ctx, `INSERT INTO loyal_yield.backyard_withdrawal_cost_reservations(operation_id,consent_id,authority,request_signature,request_instruction,escrow_lp,max_cost_usdc,incremental_cost_usdc,projected_payable_usdc,actual_cost_usdc,actual_incremental_cost_usdc) VALUES($1,$2,$3,$4,$5,100,$6::bigint,$6::bigint,1000,CASE WHEN $7='settled' THEN $6::bigint ELSE NULL END,CASE WHEN $7='settled' THEN $6::bigint ELSE NULL END)`, id, consentID, c.Authority, c.RequestSignature, c.RequestInstruction, cost, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	add("settled", id1, "settled", c, 6)
	add("pending", id2, "reserved", c2, 4)
	add("released", id1, "released", c, 99)
	add("other-generation", otherID, "reserved", other, 100)
	for i, state := range []string{"settled", "reserved", "released"} {
		_, err := db.Exec(ctx, `INSERT INTO loyal_yield.earn_max_withdrawal_claim_attempts(attempt_id,consent_id,authority,request_signature,request_instruction,signed_wire,transaction_signature,blockhash,last_valid_block_height,reserved_fee_lamports,state,actual_fee_lamports,finalized_slot) VALUES($1,$2,$3,$4,$5,$6,$7,'hash',100,$8,$9,CASE WHEN $9='settled' THEN 3000 ELSE NULL END,CASE WHEN $9='settled' THEN 1 ELSE NULL END)`, strings.Repeat(fmt.Sprintf("%x", i+1), 64), id1, c.Authority, c.RequestSignature, c.RequestInstruction, []byte{1}, fmt.Sprintf("status-%d", i), []int{7000, 5000, 9999}[i], state)
		if err != nil {
			t.Fatal(err)
		}
	}
	reader := NewWithdrawalCostStatusReader(db)
	status, err := reader.Read(ctx, c2)
	if err != nil {
		t.Fatal(err)
	}
	if status.ApprovedCostUSDC != "20" || status.SettledCostUSDC == nil || *status.SettledCostUSDC != "6" || status.ReservedCostUSDC == nil || *status.ReservedCostUSDC != "4" || status.RemainingCostUSDC == nil || *status.RemainingCostUSDC != "10" {
		t.Fatalf("generation/vault attribution %#v", status)
	}
	if status.SpentClaimFee == nil || *status.SpentClaimFee != "3000" || status.ReservedClaimFee == nil || *status.ReservedClaimFee != "5000" || status.RemainingClaimFee == nil || *status.RemainingClaimFee != "12000" || status.ClaimPending == nil || !*status.ClaimPending {
		t.Fatalf("claim fee attribution %#v", status)
	}
	if _, err := db.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_operations SET state='unknown',unknown_reason='receipt_cost_unavailable' WHERE operation_id='pending'`); err != nil {
		t.Fatal(err)
	}
	status, err = reader.Read(ctx, c2)
	if err != nil || status.RemainingCostUSDC != nil || status.ReservedCostUSDC == nil || *status.ReservedCostUSDC != "4" || status.UnknownReason != "awaiting_cost_finalization" {
		t.Fatalf("unknown actual must retain reservation and unknown remaining: %#v %v", status, err)
	}
	if _, err := db.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_reservations SET actual_incremental_cost_usdc=NULL WHERE operation_id='settled'`); err != nil {
		t.Fatal(err)
	}
	status, err = reader.Read(ctx, c2)
	if err != nil || status.SettledCostUSDC != nil || status.RemainingCostUSDC != nil {
		t.Fatalf("missing settled attribution must stay unknown: %#v %v", status, err)
	}
	if _, err := db.Exec(ctx, `DROP TABLE loyal_yield.earn_max_withdrawal_claim_attempts`); err != nil {
		t.Fatal(err)
	}
	if status, err := reader.Read(ctx, c2); err == nil || status != nil {
		t.Fatalf("missing capability fabricated totals: %#v %v", status, err)
	}
}
