package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	sdk "github.com/solana-foundation/solana-go/v2"
)

// Actual registered journal/lease scenarios. Snapshot and simulation DTOs are
// deterministic fixtures, not evidence that a provider table was fetched or a
// Squads/KLend/Jupiter transaction executed on chain.
func TestCrossMintExternalAppendPreservesManagedLeasesAndSchemaHold(t *testing.T) {
	for _, mode := range []string{"mixed", "external_only", "missing_vector", "forged_commitment", "missing_clock", "revoked_policy", "revoked_payer"} {
		t.Run(mode, func(t *testing.T) {
			store, pool := integrationStore(t)
			ctx := context.Background()
			a, signer, input := seedCrossMintActivation(t, ctx, pool)
			m, err := store.ActivateCrossMintMovement(ctx, a.Lease, input)
			if err != nil {
				t.Fatal(err)
			}
			l, err := store.ClaimCrossMintContinuation(ctx, m.Cluster, a.Lease.Owner, time.Minute)
			if err != nil || l == nil {
				t.Fatalf("claim %v", err)
			}
			bindings, err := crossMintBindings(m.ExecutionPlan)
			if err != nil {
				t.Fatal(err)
			}
			p := CrossMintPreparedLeg{Preparation: a.Preparation, LastValidBlockHeight: a.LastValidBlockHeight, PolicyAccount: bindings.Withdraw.PolicyAccount, ExpectedEffect: CrossMintExpectedEffect{CreditMint: crossString(m.SourceMint), CreditTokenAccount: crossString("source-ata"), MinimumCreditAmountRaw: crossInt(m.PlannedAmountRaw - 10)}, BalanceAnchors: CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: m.SourceMint, TokenAccount: "source-ata", AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: m.SourceReserve, Market: "market", Obligation: "obligation", ObligationExists: true, CollateralRaw: 1001}}, ConflictKeys: a.ConflictKeys, SelectedALTs: a.SelectedALTs}
			tx, err := sdk.TransactionFromBytes(p.Preparation.Transaction.UnsignedWire)
			if err != nil {
				t.Fatal(err)
			}
			key := func(label string) string {
				return sdk.PublicKey(sha256.Sum256([]byte(m.Cluster + mode + label))).String()
			}
			external := CrossMintExternalALT{Address: key("external-table"), Addresses: []string{key("external-member")}, ObservedSlot: 1000, UsableAfterSlot: 999}
			if mode != "mixed" {
				external.Addresses = append([]string(nil), p.SelectedALTs[0].Addresses...)
				p.SelectedALTs = nil
				tx.Message.AddressTableLookups[0].AccountKey = sdk.MustPublicKeyFromBase58(external.Address)
				p.Preparation.Transaction.LookupTables = []string{external.Address}
			} else {
				tx.Message.AddressTableLookups = append(tx.Message.AddressTableLookups, sdk.MessageAddressTableLookup{AccountKey: sdk.MustPublicKeyFromBase58(external.Address), WritableIndexes: []uint8{0}})
				tx.Message.Instructions[0].Accounts = append(tx.Message.Instructions[0].Accounts, uint16(len(tx.Message.AccountKeys)+1))
				p.Preparation.Transaction.LookupTables = append(append([]string(nil), p.Preparation.Transaction.LookupTables...), external.Address)
				p.Preparation.Transaction.WritableAccounts = append(append([]string(nil), p.Preparation.Transaction.WritableAccounts...), external.Addresses[0])
			}
			external.OrderedAddressHash = mustCrossMintExternalHash(t, external.Addresses)
			p.ExternalALTs = []CrossMintExternalALT{external}
			tx.Message.RecentBlockhash = sdk.Hash(sha256.Sum256([]byte(m.Cluster + mode + "blockhash")))
			message, err := tx.Message.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			unsigned, err := tx.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			mh, wh := sha256.Sum256(message), sha256.Sum256(unsigned)
			p.Preparation.Transaction.Message, p.Preparation.Transaction.UnsignedWire = message, unsigned
			p.Preparation.Transaction.MessageSHA256, p.Preparation.Transaction.WireSHA256 = hex.EncodeToString(mh[:]), hex.EncodeToString(wh[:])
			p.Preparation.Transaction.PacketBytes = len(unsigned)
			p.Preparation.Simulation.WireSHA256 = p.Preparation.Transaction.WireSHA256
			p.AltSelectionFingerprint, err = crossMintALTSelectionFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := signer.SignPreparedRoute(p.Preparation.Transaction, p.LastValidBlockHeight)
			if err != nil {
				t.Fatal(err)
			}
			request := CrossMintLegRequest{Movement: m, Leg: LegWithdraw, Purpose: PurposeOptimizeYield, Generation: 1, RemainingFeeLamports: 50000}
			bad := p
			bad.AltSelectionFingerprint = "forged"
			if _, err = store.AppendCrossMintLeg(ctx, *l, request, bad, wire); err == nil {
				t.Fatal("forged combined selection commitment persisted")
			}
			err = nil
			switch mode {
			case "missing_vector":
				p.ExternalALTs[0].Addresses = nil
			case "forged_commitment":
				p.ExternalALTs[0].OrderedAddressHash = "forged"
			case "missing_clock":
				p.ExternalALTs[0].ObservedSlot = 0
			case "revoked_policy":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET active=false,finalized_eligible=false WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, m.VaultID)
			case "revoked_payer":
				_, err = pool.Exec(ctx, `UPDATE loyal_yield.route_policies SET delegated_signers=ARRAY[$2]::text[] WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, m.VaultID, key("foreign-payer"))
			}
			if err != nil {
				t.Fatal(err)
			}
			id, err := store.AppendCrossMintLeg(ctx, *l, request, p, wire)
			if mode != "mixed" {
				if err == nil {
					t.Fatal("incomplete external proof or revoked financial authority accepted")
				}
				if mode == "external_only" {
					// Migration 0025's retained trigger requires real managed table
					// evidence for a policy payer. A future reviewed migration may
					// extend that contract; this port must never fabricate an ID.
					var databaseErr *pgconn.PgError
					if !errors.As(err, &databaseErr) || databaseErr.Code != "P0001" || databaseErr.Message != "policy route fee payer requires selected reusable-v2 table evidence" {
						t.Fatalf("external-only held for an unexpected cause: %v", err)
					}
				}
				var signed, usage int
				var state string
				var version int64
				if queryErr := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM loyal_yield.signed_route_submissions WHERE decision_id=d.id),(SELECT count(*) FROM loyal_yield.lookup_table_usage_leases WHERE reference_key=$2),d.custody_version,r.reservation_state FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, m.DecisionID, crossMintSemantic(m.DecisionID, LegWithdraw, 1)).Scan(&signed, &usage, &version, &state); queryErr != nil || signed != 0 || usage != 0 || version != 0 || state != "active" {
					t.Fatalf("failed external admission had partial effects: signed=%d usage=%d version=%d capacity=%s err=%v", signed, usage, version, state, queryErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var evidence json.RawMessage
			var managedCount, externalCount, usageCount int
			if err = pool.QueryRow(ctx, `SELECT alt_mutation_epochs,jsonb_array_length(alt_mutation_epochs->'tables'),jsonb_array_length(alt_mutation_epochs->'externalSnapshots'),(SELECT count(*) FROM loyal_yield.lookup_table_usage_leases WHERE lease_kind='prepared_transaction' AND reference_key=s.semantic_key) FROM loyal_yield.signed_route_submissions s WHERE id=$1`, id).Scan(&evidence, &managedCount, &externalCount, &usageCount); err != nil {
				t.Fatal(err)
			}
			if managedCount != len(p.SelectedALTs) || externalCount != 1 || usageCount != len(p.SelectedALTs) {
				t.Fatalf("external table obtained fabricated managed lease/ID: managed=%d external=%d usage=%d", managedCount, externalCount, usageCount)
			}
			snapshots, order, err := parseCrossMintExternalALTEvidence(evidence)
			if err != nil || len(snapshots) != 1 || snapshots[0].Address != external.Address || !sameStrings(order, p.Preparation.Transaction.LookupTables) {
				t.Fatalf("durable source proof incomplete: %v %v %v", snapshots, order, err)
			}
			var state string
			var version int64
			if err = pool.QueryRow(ctx, `SELECT r.reservation_state,d.custody_version FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.target_capacity_reservations r ON r.decision_id=d.id WHERE d.id=$1`, m.DecisionID).Scan(&state, &version); err != nil || state != "active" || version != 0 {
				t.Fatalf("signing inferred a custody effect or released capacity: %s %d %v", state, version, err)
			}
		})
	}
}

func TestCrossMintExternalOnlyPayerGuardCannotAuthorizeSameMintDecision(t *testing.T) {
	_, pool := integrationStore(t)
	ctx := context.Background()
	b := seedBaseline(t, ctx, pool, "external-route-guard:"+time.Now().Format("150405.000000000"))
	_, p := externalALTFixture(t)
	evidence, err := marshalCrossMintALTEvidence(p)
	if err != nil {
		t.Fatal(err)
	}
	var payer string
	if err = pool.QueryRow(ctx, `SELECT p.delegated_signers[1] FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id JOIN loyal_yield.route_policies p ON p.id=v.active_policy_id WHERE o.id=$1`, b.OpportunityID).Scan(&payer); err != nil {
		t.Fatal(err)
	}
	var allowed bool
	if err = pool.QueryRow(ctx, crossMintPolicyPayerSQL, b.OpportunityID, b.Cluster, payer, evidence).Scan(&allowed); err != nil || allowed {
		t.Fatalf("external proof extended payer authority to non-crossmint route: allowed=%v err=%v", allowed, err)
	}
}
