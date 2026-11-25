package backyard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func TestLoopingFeeActivationRequiresFinalizedZeroFeeAndExactPolicy(t *testing.T) {
	const cap = 10_000_000_000
	vault := voltrVaultFixture(t, 1_000_000_000)
	vault.Address = looping.VoltrVault
	putKey(t, vault.Data[136:168], looping.idleATA)
	putKey(t, vault.Data[272:304], looping.lpMint)
	putKey(t, vault.Data[368:400], looping.squads)
	clear(vault.Data[512:528])
	lp := voltrLPMintFixture(t, 1_000_000_000_000)
	lp.Address = looping.lpMint
	treasury := tokenAccountFixture(t, loopingTreasuryUSDC, fleet.USDCMint, backyardSettingsSigner, 0)
	want, err := PerformanceFeePolicy(looping.Name, cap)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := encodeTestPolicy(1, want)
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]ConfirmedAccount{vault.Address: vault, lp.Address: lp, treasury.Address: treasury,
		policy.address.String(): {Owner: squads.ProgramID.String(), Lamports: 1, Data: policy.data}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch req.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
		case "getMultipleAccounts":
			var keys []string
			var opts struct{ Commitment string }
			if err := json.Unmarshal(req.Params[0], &keys); err != nil {
				t.Error(err)
			}
			if err := json.Unmarshal(req.Params[1], &opts); err != nil || opts.Commitment != "finalized" {
				t.Errorf("activation read at %q: %v", opts.Commitment, err)
			}
			values := make([]any, len(keys))
			for i, key := range keys {
				if key == backyardSettings {
					values[i], _ = capturedPolicyAccount(key)
				} else if acc, ok := accounts[key]; ok {
					values[i] = map[string]any{"owner": acc.Owner, "lamports": acc.Lamports, "data": []string{base64.StdEncoding.EncodeToString(acc.Data), "base64"}, "executable": false}
				}
			}
			result = map[string]any{"context": map[string]int{"slot": 500}, "value": values}
		default:
			t.Errorf("unexpected RPC method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := ActivatePerformanceFee(t.Context(), &out, server.URL, "", cap, false); err != nil || !strings.Contains(out.String(), policy.address.String()) {
		t.Fatalf("valid rehearsal: %s, %v", out.String(), err)
	}
	checkRefused := func(weeklyCap int64, reason string) {
		t.Helper()
		out.Reset()
		err := ActivatePerformanceFee(t.Context(), &out, server.URL, "", weeklyCap, false)
		if err == nil || !strings.Contains(err.Error(), reason) || out.Len() != 0 {
			t.Fatalf("expected %s, got %v / %s", reason, err, out.String())
		}
	}
	checkRefused(cap-1, "payout policy")
	checkRefused(cap+1, "cap")
	binary.LittleEndian.PutUint16(vault.Data[514:], 1500)
	checkRefused(cap, "Voltr fee")
	binary.LittleEndian.PutUint16(vault.Data[514:], 0)
	putKey(t, treasury.Data[32:64], backyardDelegate)
	checkRefused(cap, "treasury")
}

func TestLoopingFeePayoutPassesPersistedWireGate(t *testing.T) {
	r, _ := feeWireFixture(t)
	key, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	delegate := publicKeyFromBytes(key)
	message, err := compileBridgeMessageForDelegate(looping, r, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(signer, delegate, message, r.RecentBlockhash, r.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	simulation := SimulationResult{Slot: r.fee.Entry.Slot, MessageSHA256: sha256Bytes(message)}
	if err := validateSignedFeeWire(signed, simulation, delegate); err != nil {
		t.Fatalf("the exact signed weekly payout failed the persistence gate: %v", err)
	}
}

func TestLoopingFeeActivationRetainsCheckpointOnRetry(t *testing.T) {
	dsn := os.Getenv("BACKYARD_FOREIGN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires a disposable local backyard_foreign_test database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || config.ConnConfig.Host != "127.0.0.1" || !strings.HasPrefix(config.ConnConfig.Database, "backyard_foreign_test") {
		t.Fatal("requires a disposable local backyard_foreign_test database")
	}
	ctx := t.Context()
	d, err := openOperatorWriter(ctx, dsn, looping.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := d.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA loyal_yield`)
	defer exec(`DROP SCHEMA loyal_yield CASCADE`)
	exec(`CREATE TABLE loyal_yield.multiply_route_states(route_key text PRIMARY KEY, state jsonb, updated_at timestamptz);
		CREATE TABLE loyal_yield.multiply_operations(route_key text,status text,action text,engine_version text,
			expected_effects jsonb,reconciled_effects jsonb,confirmed_slot bigint,signed_wire bytea);
		CREATE TABLE loyal_yield.backyard_manual_recovery_latches(route_key text,cleared_at timestamptz)`)
	exec(`INSERT INTO loyal_yield.multiply_route_states VALUES($1,'{"other":1}',now()),($2,'{"other":2}',now())`, looping.RouteKey, rwa.RouteKey)
	seed := performanceFeeState{ActivationSlot: 451, WeeklyCapRaw: 1_000_000}
	if _, _, err := d.initializePerformanceFee(ctx, seed); err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("missing fee journal was accepted: %v", err)
	}
	migration, err := os.ReadFile("../../../../migrations/yield/0150_backyard_looping_performance_fee.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	exec(`INSERT INTO loyal_yield.multiply_operations(route_key,status,action,engine_version) VALUES($1,'signed','REPORT_NAV','backyard_rwa_v1')`, looping.RouteKey)
	if _, _, err := d.initializePerformanceFee(ctx, seed); err == nil {
		t.Fatal("activation succeeded with an unresolved signed operation")
	}
	exec(`UPDATE loyal_yield.multiply_operations SET status='reconciled'`)
	got, status, err := d.initializePerformanceFee(ctx, seed)
	if err != nil || status != "initialized" || got != seed {
		t.Fatalf("first activation: %s %+v %v", status, got, err)
	}
	later := seed
	later.ActivationSlot++
	got, status, err = d.initializePerformanceFee(ctx, later)
	if err != nil || status != "already-initialized" || got != seed {
		t.Fatalf("retry replaced the original anchor: %s %+v %v", status, got, err)
	}
	later.WeeklyCapRaw++
	if _, _, err := d.initializePerformanceFee(ctx, later); err == nil {
		t.Fatal("activation silently changed the transfer authority")
	}
	var untouched bool
	if err := d.pool.QueryRow(ctx, `SELECT (SELECT state='{"other":2}'::jsonb FROM loyal_yield.multiply_route_states WHERE route_key=$1) AND
		(SELECT state-'performanceFee'='{"other":1}'::jsonb FROM loyal_yield.multiply_route_states WHERE route_key=$2)`, rwa.RouteKey, looping.RouteKey).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("other route fields changed: %v", err)
	}
	exec(`UPDATE loyal_yield.multiply_route_states SET state=state-'performanceFee' WHERE route_key=$1`, looping.RouteKey)
	exec(`UPDATE loyal_yield.multiply_operations SET expected_effects='{"expectedEffects":{"performanceFee":{"after":{"version":1}}}}' WHERE route_key=$1`, looping.RouteKey)
	if _, _, err := d.initializePerformanceFee(ctx, seed); err == nil || !strings.Contains(err.Error(), "existing fee journal") {
		t.Fatalf("lost checkpoint could be reset: %v", err)
	}
}
