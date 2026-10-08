package earn

// Parity with the retired Rust implementation. The golden files are the
// recorded output of loyal-actions, the Rust policy monitor and
// loyal-yield-store; their generator was deleted with the Rust workers.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	sp "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

type goldenInstruction struct {
	ProgramID string `json:"program_id"`
	Accounts  []struct {
		Pubkey     string `json:"pubkey"`
		IsSigner   bool   `json:"is_signer"`
		IsWritable bool   `json:"is_writable"`
	} `json:"accounts"`
	Data string `json:"data"`
}

func (g goldenInstruction) decode(t *testing.T) sp.Instruction {
	t.Helper()
	data, err := hex.DecodeString(g.Data)
	if err != nil {
		t.Fatal(err)
	}
	out := sp.Instruction{ProgramID: solana.MustPublicKeyFromBase58(g.ProgramID), Data: data}
	for _, account := range g.Accounts {
		out.Accounts = append(out.Accounts, sp.AccountMeta{PublicKey: solana.MustPublicKeyFromBase58(account.Pubkey), IsSigner: account.IsSigner, IsWritable: account.IsWritable})
	}
	return out
}

func canonicalJSON(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPolicyDetectionMatchesRustMonitor(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/earn/policy-detection.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Name        string            `json:"name"`
			Instruction goldenInstruction `json:"instruction"`
			Events      json.RawMessage   `json:"events"`
		} `json:"cases"`
		EarnMax []struct {
			Settings       string `json:"settings"`
			PolicySeedBase uint64 `json:"policy_seed_base"`
			Delegate       string `json:"delegate"`
			Families       []struct {
				Family string            `json:"family"`
				Create goldenInstruction `json:"create"`
				Update goldenInstruction `json:"update"`
			} `json:"families"`
		} `json:"earn_max"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	monitor := &PolicyMonitor{cluster: "mainnet-beta"}
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var events []map[string]any
			for _, event := range monitor.events("golden-signature", 77, c.Instruction.decode(t)) {
				switch {
				case event.route != nil:
					events = append(events, map[string]any{"kind": "route", "input": event.route})
				case event.setup != nil:
					events = append(events, map[string]any{"kind": "setup", "input": event.setup})
				case event.sweep != nil:
					events = append(events, map[string]any{"kind": "sweep", "input": event.sweep})
				case event.crossMint != nil:
					events = append(events, map[string]any{"kind": "cross_mint", "input": event.crossMint})
				default:
					events = append(events, map[string]any{"kind": "removal", "input": event.removal})
				}
			}
			if events == nil {
				events = []map[string]any{}
			}
			var want any
			decoder := json.NewDecoder(bytes.NewReader(c.Events))
			decoder.UseNumber()
			if err := decoder.Decode(&want); err != nil {
				t.Fatal(err)
			}
			if got := canonicalJSON(t, events); !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				t.Fatalf("policy events differ from Rust\n got %s\nwant %s", gotJSON, c.Events)
			}
		})
	}
	for _, set := range golden.EarnMax {
		settings, delegate := solana.MustPublicKeyFromBase58(set.Settings), solana.MustPublicKeyFromBase58(set.Delegate)
		topology, err := multiply.DeriveEarnMaxTopology(settings, set.PolicySeedBase)
		if err != nil {
			t.Fatal(err)
		}
		strategy, err := topology.Strategy(multiply.SyrupUsdcUsdc)
		if err != nil {
			t.Fatal(err)
		}
		monitor := &PolicyMonitor{cluster: "mainnet-beta", delegate: delegate}
		for index, family := range set.Families {
			if string(earnMaxFamilies[index]) != family.Family {
				t.Fatalf("family order %s differs from Rust %s", earnMaxFamilies[index], family.Family)
			}
			constraints, err := multiply.CanonicalConstraints(topology, earnMaxFamilies[index])
			if err != nil {
				t.Fatal(err)
			}
			policy := familyPolicy(strategy, earnMaxFamilies[index])
			update, err := sp.EncodeCompactPolicyUpdate(policy.Account, delegate, 0, constraints)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(update, family.Update.decode(t).Data) {
				t.Fatalf("%s %s canonical update bytes differ from Rust", set.Settings, family.Family)
			}
			actions, err := sp.DecodeSettingsActions(family.Create.decode(t))
			if err != nil || len(actions) != 1 {
				t.Fatalf("decode canonical create: %v %d", err, len(actions))
			}
			base, ok, err := monitor.earnMaxSeedBase(actions[0])
			if err != nil || !ok || base != set.PolicySeedBase {
				t.Fatalf("%s create seed base = %d %v %v, want %d", family.Family, base, ok, err, set.PolicySeedBase)
			}
		}
	}
}

const parityDumpSQL = "SELECT coalesce(jsonb_agg(r ORDER BY r::text), '[]'::jsonb) FROM (SELECT (SELECT coalesce(jsonb_object_agg(key, value), '{}'::jsonb) FROM jsonb_each(to_jsonb(t)) WHERE key NOT LIKE '%\\_at') AS r FROM loyal_yield.%s t) rows"

var parityTables = []string{
	"route_policies", "managed_vaults", "balance_sweep_targets", "autodeposit_reconciliation_requests",
	"cross_mint_swap_policies", "cross_mint_vault_opt_ins", "earn_reconciliation_jobs", "earn_chain_mutations",
	"user_yield_positions", "user_yield_position_deposits", "user_yield_position_withdrawals",
	"user_yield_position_holding_events", "vault_position_snapshots", "vault_position_snapshot_positions",
	"vault_reserve_positions_current", "vault_idle_token_balances_current", "earn_chain_refund_events",
	"earn_deposit_onboarding_attempts", "earn_max_policy_sets", "projection_offsets", "laserstream_replay_cursors",
	"multiply_route_states", "multiply_operations", "multiply_position_snapshots",
}

// rustOperation is MultiplyOperation's serde (camelCase) document.
type rustOperation struct {
	OperationID          string                   `json:"operationId"`
	RouteKey             string                   `json:"routeKey"`
	Cycle                uint64                   `json:"cycle"`
	Action               multiply.MultiplyAction  `json:"action"`
	Status               multiply.OperationStatus `json:"status"`
	IdempotencyKey       string                   `json:"idempotencyKey"`
	ExpectedEffects      multiply.ExpectedEffects `json:"expectedEffects"`
	TransactionSignature *string                  `json:"transactionSignature"`
	ConfirmedSlot        *uint64                  `json:"confirmedSlot"`
	ReconciliationSHA256 *string                  `json:"reconciliationSha256"`
	CreatedAt            time.Time                `json:"createdAt"`
}

func TestStoreRowsMatchRustStore(t *testing.T) {
	dsn := os.Getenv("EARN_PARITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("EARN_PARITY_TEST_DATABASE_URL is required")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.User == nil || endpoint.User.Username() != "workers_v2" || endpoint.Path != "/workers_v2_earn_parity" {
		t.Fatal("Earn parity requires the dedicated workers_v2 earn_parity fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// The dedicated database starts each run from empty tables and sequences.
	if _, err := pool.Exec(ctx, "TRUNCATE "+strings.Join(prefixed(parityTables), ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		t.Fatal(err)
	}
	multiplyStore, err := multiply.NewStoreFromPool(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	raw, err := os.ReadFile("../../../testdata/earn/store-script.json")
	if err != nil {
		t.Fatal(err)
	}
	var steps []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}
	decode := func(raw json.RawMessage, target any) {
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
	}
	var results []map[string]any
	for _, step := range steps {
		var op string
		decode(step["op"], &op)
		value, err := func() (any, error) {
			switch op {
			case "record_policy_match", "record_setup_policy_match":
				var input PolicyMatchInput
				decode(step["input"], &input)
				if op == "record_policy_match" {
					return nil, store.RecordPolicyMatch(ctx, input)
				}
				return nil, store.RecordSetupPolicyMatch(ctx, input)
			case "record_balance_sweep_policy_match":
				var input BalanceSweepPolicyMatchInput
				decode(step["input"], &input)
				return nil, store.RecordBalanceSweepPolicyMatch(ctx, input)
			case "record_cross_mint_swap_policy_manifest":
				var input CrossMintSwapPolicyManifestInput
				decode(step["input"], &input)
				return nil, store.RecordCrossMintSwapPolicyManifest(ctx, input)
			case "record_policy_removal":
				var input PolicyRemovalInput
				decode(step["input"], &input)
				return nil, store.RecordPolicyRemoval(ctx, input)
			case "record_recurring_delegation":
				var input RecurringDelegationObserved
				decode(step["input"], &input)
				return nil, store.RecordRecurringDelegation(ctx, input)
			case "project_earn_max_policy_set":
				var input EarnMaxPolicySetProjectionInput
				var consumer string
				decode(step["input"], &input)
				decode(step["consumer"], &consumer)
				return nil, store.ProjectEarnMaxPolicySet(ctx, consumer, input)
			case "project_earn_max_intent":
				var input EarnMaxIntentProjectionInput
				decode(step["input"], &input)
				return multiplyStore.ProjectIntent(ctx, projectionIntent(input))
			case "job":
				var consumer, key string
				var slot uint64
				var vault watch.Vault
				var mutation EarnMutation
				decode(step["consumer"], &consumer)
				decode(step["event_key"], &key)
				decode(step["durable_slot"], &slot)
				decode(step["vault"], &vault)
				decode(step["mutation"], &mutation)
				if _, err := store.Enqueue(ctx, consumer, key, slot, step["event_payload"], []watch.Vault{vault}, ""); err != nil {
					return nil, err
				}
				job, err := store.ClaimJob(ctx, consumer, "golden-owner", 120)
				if err != nil || job == nil {
					return "idle", err
				}
				return store.CompleteJob(ctx, job.ID, "golden-owner", mutation)
			case "sql":
				var statement string
				var args []any
				decode(step["statement"], &statement)
				decode(step["args"], &args)
				for i, arg := range args {
					if number, ok := arg.(float64); ok {
						args[i] = int64(number)
					}
				}
				_, err := pool.Exec(ctx, statement, args...)
				return nil, err
			case "admit_external":
				var routeKey string
				var route multiply.RouteState
				var document rustOperation
				decode(step["route_key"], &routeKey)
				decode(step["route"], &route)
				decode(step["operation"], &document)
				lease, err := multiplyStore.LeaseRoute(ctx, routeKey, "golden-owner", time.Now().Add(30*time.Second))
				if err != nil || lease == nil {
					return nil, err
				}
				inserted, err := multiplyStore.AdmitExternalOperation(ctx, lease, &route, &multiply.MultiplyOperation{
					OperationID: document.OperationID, RouteKey: document.RouteKey, Cycle: document.Cycle, EngineVersion: multiply.EngineVersion,
					Action: document.Action, Status: document.Status, IDempotencyKey: document.IdempotencyKey, ExpectedEffects: document.ExpectedEffects,
					TransactionSignature: document.TransactionSignature, ConfirmedSlot: document.ConfirmedSlot,
					ReconciliationSHA256: document.ReconciliationSHA256, CreatedAt: document.CreatedAt, UpdatedAt: document.CreatedAt,
				})
				if err != nil {
					return nil, err
				}
				_, err = multiplyStore.ReleaseLease(ctx, lease)
				return inserted, err
			}
			t.Fatalf("unknown op %s", op)
			return nil, nil
		}()
		if err != nil {
			results = append(results, map[string]any{"op": op, "ok": false})
			continue
		}
		results = append(results, map[string]any{"op": op, "ok": true, "value": value})
	}
	tables := map[string]json.RawMessage{}
	for _, table := range parityTables {
		var dump []byte
		if err := pool.QueryRow(ctx, strings.Replace(parityDumpSQL, "%s", table, 1)).Scan(&dump); err != nil {
			t.Fatal(err)
		}
		tables[table] = dump
	}
	goldenRaw, err := os.ReadFile("../../../testdata/earn/store-rows.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Steps  []map[string]any           `json:"steps"`
		Tables map[string]json.RawMessage `json:"tables"`
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		t.Fatal(err)
	}
	for index, want := range golden.Steps {
		got := results[index]
		if got["ok"] != want["ok"] || want["ok"] == true && !reflect.DeepEqual(canonicalJSON(t, got["value"]), canonicalJSON(t, want["value"])) {
			t.Errorf("step %d %v = %v, Rust %v", index, want["op"], got, want)
		}
	}
	for _, table := range parityTables {
		if got, want := canonicalJSON(t, tables[table]), canonicalJSON(t, golden.Tables[table]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s rows differ from Rust\n got %s\nwant %s", table, tables[table], golden.Tables[table])
		}
	}
}

func prefixed(tables []string) []string {
	out := make([]string, 0, len(tables))
	for _, table := range tables {
		out = append(out, "loyal_yield."+table)
	}
	return out
}
