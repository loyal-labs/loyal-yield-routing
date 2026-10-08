package fleetexec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func TestLookupRequestSatisfiedOnlyByCurrentPublishedActualBanks(t *testing.T) {
	pool := lookupRegisteredPool(t)
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	store, source := seedLookupSource(t, ctx, pool, f)
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.route_lookup_tables SET accepting_allocations=true,durable=true WHERE id=$1`, source.Intent.TableID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_families SET active_generation=0 WHERE id=$1`, source.Intent.FamilyID); err != nil {
		t.Fatal(err)
	}
	sharedFamily := seedLookupCatalogDemand(t, ctx, pool, f)
	requestID := seedLookupDemandRequest(t, ctx, pool, f, f.Addresses[:2])
	request, err := store.LeaseLookupPlanningRequest(ctx, "localnet", "satisfaction-seed", time.Minute)
	if err != nil || request == nil {
		t.Fatal(request, err)
	}
	policy := LookupPackingPolicy{HardCapacity: 256, LargestAtomicExpansion: 20, SafetyMargin: 8, GrowthReservation: 8, MaximumVaultCohort: 16}
	plan, err := store.planLookupVaultRequest(ctx, *request, policy, lookupPlanningBank{slot: 1000, authority: f.Manager})
	if err != nil {
		t.Fatal(err)
	}
	// Publish through the real C store, then retain that exact immutable
	// opportunity as a waiting consumer of the actual Go provisioning request.
	cStore, err := fleet.NewStoreFromPool(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var position fleet.VaultPosition
	position.VaultID = request.VaultID
	if err = pool.QueryRow(ctx, `SELECT settings,vault_pubkey,active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1`, request.VaultID).Scan(&position.Settings, &position.VaultPubkey, &position.PolicyID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,is_current,context) VALUES($1,$2,1000,$3,true,'{}') RETURNING id`, request.VaultID, position.PolicyID, now).Scan(&position.SnapshotID); err != nil {
		t.Fatal(err)
	}
	position.ObservedAt, position.ObservedSlot = now, 1000
	position.SourceAmountSemantics = "redeemable_liquidity_amount"
	market := f.Addresses[2]
	var catalog []fleet.SupportedReserveCatalogRow
	var verified []fleet.VerifiedSupportedReserveRow
	for n, reserve := range f.Addresses[:2] {
		catalog = append(catalog, fleet.SupportedReserveCatalogRow{Market: market, LiquidityMint: fleet.USDCMint, Reserve: reserve, RiskBaskets: []string{"safe"}, Source: "kamino-api", FetchedAt: now})
		verified = append(verified, fleet.VerifiedSupportedReserveRow{StateEventID: int64(n + 1), AccountDataHash: strings.Repeat(string(rune('a'+n)), 64), StateObservedAt: now, StateSlot: 1000, VerifiedAt: now, VerifiedSlot: 1000, VerificationCommitment: "confirmed", VerificationSource: "http_confirmed_refresh", Reserve: reserve, Market: &market, LiquidityMint: fleet.USDCMint, MintDecimals: 6, ReserveLastUpdateSlot: 1000, AvailableAmount: 1e15, TotalSupplyAmount: 1e15, MarketPriceUSD: 1, SupplyAPY: float64(n+1) / 100})
	}
	epoch, err := fleet.BuildImmutableMarketEpoch(fleet.SupportedReserveMarketSnapshot{CapturedAt: now, Catalog: catalog, VerifiedReserves: verified}, []string{fleet.USDCMint})
	if err != nil {
		t.Fatal(err)
	}
	decision := fleet.Decision{Eligible: true, RouteKind: "same_mint", VaultID: request.VaultID, SourceSnapshotID: position.SnapshotID, MarketSlot: 1000, SourceReserve: f.Addresses[0], TargetReserve: f.Addresses[1], Mint: fleet.USDCMint, AmountRaw: 1000000, PrincipalUSDMicros: 1000000, SourceAPYBPS: 100, TargetAPYBPS: 200, EdgeBPS: 100, AnnualYieldGainUSDMicros: 10000, ExpectedNetGainUSDMicros: 1000, EconomicPriority: 1, EstimatedCostLamports: 5000, EstimatedCostUSDMicros: 100, HoldingHorizonSeconds: 2592000, ConfidencePPM: 1000000, SnapshotHash: epoch.Fingerprint, ObservedAt: now}
	opportunity, err := cStore.Publish(ctx, "localnet", epoch, position, decision)
	if err != nil || !opportunity.Inserted {
		t.Fatal("initial C publication", opportunity, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='waiting_alt',requirements_fingerprint=$2,route_fingerprint=$2 WHERE id=$1`, opportunity.OpportunityID, request.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers(opportunity_id,provisioning_request_id) VALUES($1,$2)`, opportunity.OpportunityID, requestID); err != nil {
		t.Fatal(err)
	}
	before, err := cStore.Publish(ctx, "localnet", epoch, position, decision)
	if err != nil || before.OpportunityID != opportunity.OpportunityID || before.Reason == "alt_readmitted" {
		t.Fatal("C woke before actual ALT satisfaction", before, err)
	}
	planner, err := NewLookupPlanner(store, svm.rpc, LookupPlannerConfig{Cluster: "localnet", Owner: "satisfaction-plan", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewLookupWorker(store, svm.rpc, LookupWorkerConfig{Cluster: "localnet", Owner: "satisfaction-writer", LeaseTTL: time.Minute, TickDeadline: 25 * time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, func(context.Context, string) (ed25519.PrivateKey, error) {
		return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	for slot := 1001; slot <= 1016; slot++ {
		if err = svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_operations SET lease_expires_at=clock_timestamp()-interval '1 second',next_attempt_at=clock_timestamp()-interval '1 second' WHERE operation_state NOT IN ('complete','permanent_failure','cancelled')`); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `UPDATE loyal_yield.lookup_table_provisioning_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, requestID); err != nil {
			t.Fatal(err)
		}
		planner.nextCatalog = time.Time{}
		if _, err = planner.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = worker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err = pool.QueryRow(ctx, `SELECT request_status FROM loyal_yield.lookup_table_provisioning_requests WHERE id=$1`, requestID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "satisfied" {
			break
		}
	}
	if status != "satisfied" {
		t.Fatal("actual published catalog and binding did not satisfy request", status)
	}
	after, err := cStore.Publish(ctx, "localnet", epoch, position, decision)
	if err != nil || after.OpportunityID != opportunity.OpportunityID || after.Reason != "alt_readmitted" {
		t.Fatal("C did not wake identical opportunity after actual ALT satisfaction", after, err)
	}
	var awakened string
	if err = pool.QueryRow(ctx, `SELECT opportunity_state FROM loyal_yield.rebalance_opportunities WHERE id=$1`, opportunity.OpportunityID).Scan(&awakened); err != nil || awakened != "revalidate" {
		t.Fatal("C wakeup state", awakened, err)
	}
	plan.OperationID = 0
	bank := lookupPlanningBank{slot: 1000, authority: f.Manager}
	if err = planner.readPlanningReadiness(ctx, request.VaultID, &bank); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, sql string
		missing   bool
	}{
		{"current", "", false},
		{"missing-bank", "", true},
		{"catalog-pending", `UPDATE loyal_yield.lookup_table_shared_market_catalog_heads SET readiness_state='pending',target_generation=NULL,activated_at=NULL WHERE family_id=$1`, false},
		{"binding-superseded", `UPDATE loyal_yield.lookup_table_vault_desired_heads SET desired_revision=desired_revision+1 WHERE family_id=$1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, e := pool.BeginTx(ctx, pgx.TxOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			if tc.sql != "" {
				id := sharedFamily
				if tc.name == "binding-superseded" {
					id = source.Intent.FamilyID
				}
				if _, e = tx.Exec(ctx, tc.sql, id); e != nil {
					t.Fatal(e)
				}
			}
			proof := bank
			if tc.missing {
				proof.snapshots = nil
			}
			ok, e := lookupPlanningSatisfiedTx(ctx, tx, *request, plan, proof)
			if e != nil {
				t.Fatal(e)
			}
			if ok != (tc.name == "current") {
				t.Fatal("satisfaction accepted stale or missing ownership evidence", ok)
			}
		})
	}
}
