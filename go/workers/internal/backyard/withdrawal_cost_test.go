package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func TestWithdrawalCostEmergencyPriorityAndMissingEvidence(t *testing.T) {
	ordinary := Snapshot{Target: LaneTarget{Rule: ruleWithdrawal}, WithdrawalDemandRaw: 100}
	d := Decision{Action: DeleverRouteStep, Reason: debtZeroReason}
	if _, err := prepareWithdrawalCost(context.Background(), nil, nil, nil, RouteManifest{}, d, Observation{Snapshot: ordinary}, StepRequest{}, ExpectedEffects{}); err == nil {
		t.Fatal("ordinary sale admitted without evidence")
	}
	for _, rule := range []targetRule{ruleHardLTV, ruleTermMaturity, ruleOperatorExit} {
		s := ordinary
		s.Target.Rule = rule
		request := StepRequest{}
		if got, err := prepareWithdrawalCost(context.Background(), nil, nil, nil, RouteManifest{}, d, Observation{Snapshot: s}, request, ExpectedEffects{}); err != nil || got == nil {
			t.Fatalf("risk action gated by missing consent store: %s %v", rule, err)
		}
	}
	ordinary.Trustful = true
	if !ordinaryWithdrawalCostRequired(ordinary, d) {
		t.Fatal("Trustful ordinary funding bypassed request consent")
	}
	d.Reason = hardLTVDeleverReason
	if ordinaryWithdrawalCostRequired(ordinary, d) {
		t.Fatal("hard LTV lost priority")
	}
}

func TestWithdrawalCostCountsEconomicExpenseAndAllowanceOnce(t *testing.T) {
	mark := kamino.ScaledFraction{1}
	s := Snapshot{net: &certifiedNAV{quotes: []ExitCost{{Lane: "lane", MarkSF: mark, QuotedCollateralRaw: 100, BPS: microBPSWhole / 100}}}}
	e := ExpectedEffects{Sale: &ExpectedSale{Lane: "lane", MarkSF: mark, SoldRaw: 50, MarkValueRaw: 1000}}
	total, inc, err := withdrawalOperationCost(JupiterSwapRequest{MinimumOutputRaw: 990}, e, s)
	if err != nil || total != 10 || inc != 0 {
		t.Fatalf("allowance double counted: total=%d incremental=%d err=%v", total, inc, err)
	}
	e.Sale.Lane = "different"
	_, inc, err = withdrawalOperationCost(JupiterSwapRequest{MinimumOutputRaw: 990}, e, s)
	if err != nil || inc != 10 {
		t.Fatalf("unrelated allowance applied: %d %v", inc, err)
	}
	c := earnmax.WithdrawalConsent{ReferenceUSDC: "1000", MinClaimUSDC: "990"}
	if withdrawalCostLimits(c, 4, 3, 3) != nil || withdrawalCostLimits(c, 4, 3, 4) == nil {
		t.Fatal("cumulative approved total cost not enforced")
	}
}

func TestWithdrawalCostSigningBoundaryRechecksAfterReservationDelay(t *testing.T) {
	r := bridgeTestRequest(VoltrRestoreIdle, 10)
	m := rwa.manifest
	now := time.Now()
	if err := validateWithdrawalRequestNAV(m, r, now, r.net.slot); err != nil {
		t.Fatalf("fresh prepared request rejected: %v", err)
	}
	if err := validateWithdrawalRequestNAV(m, r, now.Add(exitQuoteInterval+time.Second), r.net.slot); err == nil {
		t.Fatal("reservation delay permitted signing an expired NAV certificate")
	}
	if err := validateWithdrawalRequestNAV(m, r, now, r.net.slot+min(observationLagSlots(), adaptorMaxReportAgeSlots)+1); err == nil {
		t.Fatal("reservation delay permitted signing a slot-expired NAV certificate")
	}
}

func TestWithdrawalCostDeleverAndSaleSigningBoundaryRejectsLateEvidence(t *testing.T) {
	m := rwa.manifest
	message := []byte("sealed ordinary withdrawal")
	now := time.Now()
	p := &withdrawalCostPreparation{manifest: m, MessageSHA256: sha256Bytes(message), net: &certifiedNAV{vault: m.vault.squads, manifest: m.SHA256, slot: 77, validUntil: now.Add(exitQuoteInterval)}}
	for _, request := range []any{StepRequest{Open: KaminoLegRequest{Action: DeleverRouteStep}}, JupiterSwapRequest{Action: SwapCollateralToStableStep}} {
		if err := p.validateSigningCertificate(request, message, now, 77); err != nil {
			t.Fatal(err)
		}
		if err := p.validateSigningCertificate(request, message, now.Add(exitQuoteInterval+time.Second), 77); err == nil {
			t.Fatalf("expired ordinary sale permitted: %T", request)
		}
		if err := p.validateSigningCertificate(request, message, now, 77+min(observationLagSlots(), adaptorMaxReportAgeSlots)+1); err == nil {
			t.Fatalf("slot expired ordinary sale permitted: %T", request)
		}
	}
}

func TestWithdrawalCostComponentRoundingCannotExceedFullShare(t *testing.T) {
	p := &withdrawalCostPreparation{EffectiveLP: 5100, NetworkLamports: 5000, MessageSHA256: strings.Repeat("b", 64)}
	for i, lp := range []uint64{1500, 1500, 900, 500, 500, 200} {
		c := earnmax.WithdrawalConsent{Authority: fmt.Sprint(i)}
		id, _ := c.Digest()
		p.Participants = append(p.Participants, withdrawalCostParticipant{Consent: c, ID: id, EscrowLP: lp})
	}
	if err := p.allocate(26, 25); err != nil {
		t.Fatal(err)
	}
	full, incremental := p.OtherHolders, p.IncrementalOtherHolders
	for _, r := range p.Participants {
		if r.IncrementalCost > r.MaxCost {
			t.Fatalf("incremental exceeds full economic expense: %+v", r)
		}
		full += r.MaxCost
		incremental += r.IncrementalCost
	}
	if full != 26 || incremental != 25 {
		t.Fatalf("component rounding lost expense: %d/%d", full, incremental)
	}
	frozen := append([]withdrawalCostParticipant(nil), p.Participants...)
	for amount := earnmax.USDCRaw(0); amount <= 25; amount++ {
		actual := *p
		actual.Participants = append([]withdrawalCostParticipant(nil), frozen...)
		if err := actual.allocateSettlement(amount+1, amount); err != nil {
			t.Fatalf("bounded lower receipt %d: %v", amount, err)
		}
		total, inc := actual.OtherHolders, actual.IncrementalOtherHolders
		for i, r := range actual.Participants {
			if r.MaxCost > frozen[i].MaxCost || r.IncrementalCost > frozen[i].IncrementalCost {
				t.Fatalf("lower total exceeded prepared participant reservation: %+v > %+v", r, frozen[i])
			}
			total += r.MaxCost
			inc += r.IncrementalCost
		}
		if total != amount+1 || inc != amount {
			t.Fatal("bounded receipt allocation lost pool expense")
		}
	}
}

func TestWithdrawalCostFinalizedFailureBooksActualFeeAndUnknownRemainsExplicit(t *testing.T) {
	p := withdrawalCostPreparation{MaxCost: 30, NetworkUSDC: withdrawalTestUSDC(10), NetworkLamports: 5000}
	cost, err := withdrawalReceiptCost(p, 2500, "failed", nil, "", chain.Receipt{})
	if err != nil || cost != 0 {
		t.Fatalf("atomic failed operation charged sale or lost actual fee: %d %v", cost, err)
	}
	operator, err := withdrawalNetworkCost(p, 2500)
	if err != nil || operator != 5 {
		t.Fatalf("operator fee audit lost actual price: %d %v", operator, err)
	}
	p.NetworkLamports = 0
	if _, err := withdrawalNetworkCost(p, 2500); err == nil {
		t.Fatal("unknown network basis treated as zero")
	}
}

func withdrawalTestUSDC(raw int64) *earnmax.USDCRaw { value := earnmax.USDCRaw(raw); return &value }

func withdrawalCostTestDB(t *testing.T) *Database {
	t.Helper()
	dsn := os.Getenv("BACKYARD_WITHDRAWAL_TEST_DSN")
	if dsn == "" {
		t.Skip("explicit disposable withdrawal database required")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "vault_cost_test") {
		t.Fatal("withdrawal database must be an explicitly disposable localhost vault_cost_test database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE loyal_yield.backyard_withdrawal_cost_reservations,loyal_yield.backyard_withdrawal_cost_operations,loyal_yield.earn_max_withdrawal_claim_attempts,loyal_yield.earn_max_withdrawal_consents,loyal_yield.multiply_operations`); err != nil {
		t.Fatal(err)
	}
	return &Database{pool: pool}
}

func insertWithdrawalCostConsent(t *testing.T, d *Database, c earnmax.WithdrawalConsent) string {
	t.Helper()
	id, _ := c.Digest()
	payload, _ := json.Marshal(c)
	message, _ := c.CanonicalMessage()
	signature, _ := base64.StdEncoding.DecodeString(c.OwnerSignature)
	if len(signature) != 64 {
		signature = make([]byte, 64)
	}
	_, err := d.pool.Exec(context.Background(), `INSERT INTO loyal_yield.earn_max_withdrawal_consents(consent_id,authority,vault,request_signature,request_instruction,version,owner,nonce,expires_at,canonical_message,owner_signature,payload) VALUES($1,$2,COALESCE(NULLIF($10,''),'vault'),$3,$4,$5,COALESCE(NULLIF($11,''),'owner'),$6,$7,$12,$8,$9)`, id, c.Authority, c.RequestSignature, c.RequestInstruction, c.Version, c.Nonce, time.Now().Add(time.Hour), signature, payload, c.Vault, c.Owner, message)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWithdrawalCostDatabaseCrossWorkerReservationAndConsentWidening(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	c := earnmax.WithdrawalConsent{Authority: "authority", WithdrawalGeneration: earnmax.WithdrawalGeneration{RequestSignature: "request", RequestInstruction: 2}, Version: "1", ReferenceUSDC: "1000", MinClaimUSDC: "990", MaxUserFeeLamports: "0", ExpiresAt: fmt.Sprint(time.Now().Add(time.Hour).Unix()), Nonce: strings.Repeat("a", 64)}
	id := insertWithdrawalCostConsent(t, d, c)
	p := &withdrawalCostPreparation{EffectiveLP: 100, MaxCost: 6, IncrementalCost: 6, NetworkUSDC: withdrawalTestUSDC(1), NetworkLamports: 5000, MessageSHA256: strings.Repeat("b", 64), Participants: []withdrawalCostParticipant{{Consent: c, ID: id, EscrowLP: 100, Payable: 1000, MaxCost: 6, IncrementalCost: 6}}}
	for _, op := range []string{"a", "b", "c"} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES($1,'route','decided')`, op); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, op := range []string{"a", "b"} {
		wg.Add(1)
		go func(op string) { defer wg.Done(); results <- d.reserveWithdrawalCostRecord(ctx, op, p) }(op)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("overlapping reservations accepted %d operations", accepted)
	}
	var winner string
	if err := d.pool.QueryRow(ctx, `SELECT operation_id FROM loyal_yield.backyard_withdrawal_cost_operations`).Scan(&winner); err != nil {
		t.Fatal(err)
	}
	if err := d.reserveWithdrawalCostRecord(ctx, winner, p); err != nil {
		t.Fatalf("exact restart not idempotent: %v", err)
	}
	// A replacement intent cannot erase already signed exposure.
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire='wire',signed_wire_sha256=repeat('a',64),transaction_signature='signed',recent_blockhash='blockhash',last_valid_block_height=99,simulation_slot=77,simulation_result='{"fixture":true}' WHERE operation_id=$1`, winner); err != nil {
		t.Fatal(err)
	}
	if err := d.releaseWithdrawalCost(ctx, winner); err != nil {
		t.Fatal(err)
	}
	c.Version = "2"
	c.Nonce = strings.Repeat("c", 64)
	c.MinClaimUSDC = "989"
	newID := insertWithdrawalCostConsent(t, d, c)
	q := *p
	q.Participants = append([]withdrawalCostParticipant(nil), p.Participants...)
	q.Participants[0].Consent = c
	q.Participants[0].ID = newID
	if err := d.reserveWithdrawalCostRecord(ctx, "c", &q); err == nil {
		t.Fatal("consent version erased signed reservation")
	}
	var count int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.backyard_withdrawal_cost_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("refusal left partial reservation: count=%d err=%v", count, err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',recovery_reason='signature_absent_after_blockhash_expiry' WHERE operation_id=$1`, winner); err != nil {
		t.Fatal(err)
	}
	if err := d.releaseWithdrawalCost(ctx, winner); err != nil {
		t.Fatal(err)
	}
	if err := d.reserveWithdrawalCostRecord(ctx, "c", &q); err != nil {
		t.Fatalf("proven nonlanding did not free request expense: %v", err)
	}
}

func TestWithdrawalCostDatabaseCoveredSaleAndObservationOrdering(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	c := earnmax.WithdrawalConsent{Authority: "authority", WithdrawalGeneration: earnmax.WithdrawalGeneration{RequestSignature: "request", RequestInstruction: 2}, Version: "1", ReferenceUSDC: "1000", MinClaimUSDC: "1000", MaxUserFeeLamports: "0", ExpiresAt: fmt.Sprint(time.Now().Add(time.Hour).Unix()), Nonce: strings.Repeat("a", 64)}
	id := insertWithdrawalCostConsent(t, d, c)
	for _, op := range []string{"covered", "cash", "first", "stale", "favorable"} {
		if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES($1,'route','decided')`, op); err != nil {
			t.Fatal(err)
		}
	}
	p := &withdrawalCostPreparation{EffectiveLP: 100, MaxCost: 100, IncrementalCost: 0, NetworkLamports: 5000, MessageSHA256: strings.Repeat("b", 64), Participants: []withdrawalCostParticipant{{Consent: c, ID: id, EscrowLP: 100, Payable: 1000, MaxCost: 100}}}
	if err := d.reserveWithdrawalCostRecord(ctx, "covered", p); err != nil {
		t.Fatalf("already discounted liquidation charged twice: %v", err)
	}
	if got, err := withdrawalActualIncremental(*p, 80, 5000); err != nil || got != 0 {
		t.Fatalf("frozen NAV allowance lost at receipt: %d %v", got, err)
	}
	if err := d.releaseUnsignedWithdrawalCost(ctx, "covered"); err != nil {
		t.Fatal(err)
	}
	cash := *p
	cash.Participants = append([]withdrawalCostParticipant(nil), p.Participants...)
	cash.MaxCost = 0
	cash.IncrementalCost = 0
	cash.Participants[0].MaxCost = 0
	cash.NetworkUSDC = withdrawalTestUSDC(999)
	if total, inc, err := withdrawalOperationCost(BridgeBuildRequest{}, ExpectedEffects{}, Snapshot{}); err != nil || total != 0 || inc != 0 {
		t.Fatalf("protocol-funded cash movement charged user: %d/%d %v", total, inc, err)
	}
	if err := d.reserveWithdrawalCostRecord(ctx, "cash", &cash); err != nil {
		t.Fatalf("zero-extra consent blocked protocol-funded cash movement: %v", err)
	}
	c.Version = "2"
	c.MinClaimUSDC = "990"
	c.Nonce = strings.Repeat("c", 64)
	id = insertWithdrawalCostConsent(t, d, c)
	p.MaxCost = 6
	p.IncrementalCost = 6
	p.Slot = 100
	p.Participants[0] = withdrawalCostParticipant{Consent: c, ID: id, EscrowLP: 100, Payable: 1000, MaxCost: 6, IncrementalCost: 6}
	if err := d.reserveWithdrawalCostRecord(ctx, "first", p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE loyal_yield.backyard_withdrawal_cost_reservations SET actual_cost_usdc=6,actual_incremental_cost_usdc=6 WHERE operation_id='first'; UPDATE loyal_yield.backyard_withdrawal_cost_operations SET state='settled',actual_cost_usdc=6,actual_incremental_cost_usdc=6,actual_other_holders_usdc=0,actual_incremental_other_holders_usdc=0,actual_fee_lamports=5000,receipt_signature='receipt',receipt_instruction=0,finalized_slot=200 WHERE operation_id='first'`); err != nil {
		t.Fatal(err)
	}
	p.MaxCost = 1
	p.IncrementalCost = 1
	p.Slot = 199
	p.Participants[0].MaxCost = 1
	p.Participants[0].IncrementalCost = 1
	p.Participants[0].Payable = 991
	if err := d.reserveWithdrawalCostRecord(ctx, "stale", p); err == nil {
		t.Fatal("observation older than finalized settlement forgot its effect")
	}
	p.Slot = 200
	if err := d.reserveWithdrawalCostRecord(ctx, "stale", p); err != nil {
		t.Fatalf("fresh NAV double counted finalized effect: %v", err)
	}
	p.MaxCost = 4
	p.IncrementalCost = 4
	p.Participants[0].MaxCost = 4
	p.Participants[0].IncrementalCost = 4
	p.Participants[0].Payable = 2000
	if err := d.reserveWithdrawalCostRecord(ctx, "favorable", p); err == nil {
		t.Fatal("favorable market price expanded signed incremental expense budget")
	}
}
