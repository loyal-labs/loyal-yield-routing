package backyardrwa

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func pilotFlatFixture(t *testing.T) pilotFlatEvidence {
	t.Helper()
	addresses, _, err := pilotFlatAddresses()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]ConfirmedAccount{}
	values[bridgeVoltrVault] = voltrVaultFixture(t, 23)
	values[bridgeLPMint] = voltrLPMintFixture(t, 0)
	values[bridgeStrategyReceipt] = strategyReceiptFixture(t, 0)
	values[bridgeIdleATA] = tokenAccountFixture(t, bridgeIdleATA, bridgeUSDC, bridgeIdleAuthority, 23)
	values[bridgeStrategyATA] = tokenAccountFixture(t, bridgeStrategyATA, bridgeUSDC, bridgeStrategyAuth, 0)
	values[bridgeSquadsATA] = tokenAccountFixture(t, bridgeSquadsATA, bridgeUSDC, bridgeVault, 0)
	ticket := ConfirmedAccount{Address: reportTicketPDA, Owner: bridgeAdaptorProgram, Lamports: 1, Data: make([]byte, reportTicketStateLength)}
	copy(ticket.Data, reportTicketStateDiscriminator)
	ticket.Data[8] = reportTicketVersion
	ticket.Data[9] = reportTicketBump
	putKey(t, ticket.Data[16:48], bridgeStrategy)
	values[reportTicketPDA] = ticket
	for _, lane := range pilotTransitionLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		obligation := ConfirmedAccount{Address: route.Kamino.Obligation, Owner: kaminoProgram, Lamports: 1, Data: make([]byte, kaminoObligationLength)}
		copy(obligation.Data, kaminoObligationDiscriminator[:])
		putKey(t, obligation.Data[32:64], route.Kamino.Market)
		putKey(t, obligation.Data[64:96], bridgeVault)
		values[obligation.Address] = obligation
	}
	e := pilotFlatEvidence{Genesis: mainnetGenesisHash, Commitment: "finalized", Slot: 447400000}
	for _, address := range addresses {
		a, ok := values[address]
		if !ok {
			a = ConfirmedAccount{Address: address}
		}
		e.Accounts = append(e.Accounts, a)
	}
	if err = validatePilotFlatEvidence(e); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestPilotTransitionFlatEvidenceIncludesRetiredLanes(t *testing.T) {
	for _, lane := range pilotTransitionLanes {
		for _, kind := range []string{"debt", "collateral", "custody"} {
			t.Run(lane+"/"+kind, func(t *testing.T) {
				e := pilotFlatFixture(t)
				route, err := runtimeRoute(lane)
				if err != nil {
					t.Fatal(err)
				}
				for i := range e.Accounts {
					a := &e.Accounts[i]
					if a.Address == route.Kamino.Obligation {
						if kind == "debt" {
							a.Data[1296] = 1
						}
						if kind == "collateral" {
							a.Data[128] = 1
						}
					}
					if a.Address == route.CollateralCustody && kind == "custody" {
						*a = tokenAccountFixture(t, a.Address, route.Kamino.CollateralMint, bridgeVault, 1)
					}
				}
				if err = validatePilotFlatEvidence(e); err == nil {
					t.Fatal("hidden historical exposure admitted")
				}
			})
		}
	}
	for _, kind := range []string{"confirmed", "missing", "lp", "nav", "book", "armed"} {
		t.Run(kind, func(t *testing.T) {
			e := pilotFlatFixture(t)
			switch kind {
			case "confirmed":
				e.Commitment = "confirmed"
			case "missing":
				e.Accounts = e.Accounts[1:]
			default:
				for i := range e.Accounts {
					a := &e.Accounts[i]
					if kind == "lp" && a.Address == bridgeLPMint {
						binary.LittleEndian.PutUint64(a.Data[36:44], 1)
					}
					if kind == "nav" && a.Address == bridgeStrategyReceipt {
						a.Data[104] = 1
					}
					if kind == "book" && a.Address == bridgeVoltrVault {
						a.Data[168] = 24
					}
					if kind == "armed" && a.Address == reportTicketPDA {
						a.Data[10] = 1
						a.Data[56] = 1
						a.Data[64] = 1
					}
				}
			}
			if err := validatePilotFlatEvidence(e); err == nil {
				t.Fatal("invalid finalized flat state admitted")
			}
		})
	}
}

func TestPilotBudgetActivationDurableAndIdempotent(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer db.Close()
	resetManualRecoveryProductionRoute(t, ctx, db, "pilot-activation-test")
	defer db.ReleaseRouteLease(ctx)
	evidence := pilotFlatFixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "getGenesisHash":
			result = mainnetGenesisHash
		case "getSlot":
			var options map[string]any
			json.Unmarshal(request.Params[0], &options)
			if options["commitment"] != "finalized" {
				t.Error("nonfinalized minimum slot")
			}
			result = evidence.Slot
		case "getMultipleAccounts":
			var addresses []string
			var options map[string]any
			json.Unmarshal(request.Params[0], &addresses)
			json.Unmarshal(request.Params[1], &options)
			if options["commitment"] != "finalized" {
				t.Error("nonfinalized flat evidence")
			}
			values := make([]any, 0, len(addresses))
			for _, address := range addresses {
				account := accountAt(evidence.Accounts, address)
				if account.Lamports == 0 {
					values = append(values, nil)
				} else {
					values = append(values, map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": account.Executable, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}})
				}
			}
			result = map[string]any{"context": map[string]any{"slot": evidence.Slot}, "value": values}
			requests++
		default:
			t.Errorf("unexpected activation RPC: %s", request.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	rpc, err := NewRPCClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, withPrior := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[withPrior], func(t *testing.T) {
			old := emptyTestBudget()
			old.Families["OnRe"] = FamilyBudget{SpentMicros: 17_000_000}
			state := map[string]any{"generation": 1, "cycle": 1, "unrelated": "keep", "selectorUnwind": nil}
			if withPrior {
				state["phase3"] = old
				state["phase3Initialization"] = phase3Initialization{Phase3GoalID, 2, time.Now().UTC()}
				state["generation"] = 2
			}
			encoded, _ := json.Marshal(state)
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2::jsonb,state_version=($2::jsonb->>'generation')::bigint WHERE route_key=$1`, productionRouteKey, encoded); err != nil {
				t.Fatal(err)
			}
			// An old terminal hold still stops activation when no physical
			// latch exists. Activation cannot turn a missing latch into consent.
			if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,action,status,recovery_reason,expected_effects) VALUES('pilot-derived-hold',$1,'HOLD_MANUAL_RECOVERY','manual_recovery','operator_review_required','{}')`, productionRouteKey); err != nil {
				t.Fatal(err)
			}
			_, err = db.activatePilotBudget(ctx, rpc)
			assertBudgetHold(t, err, "unresolved_work_prevents_pilot_transition")
			if _, err = db.pool.Exec(ctx, `DELETE FROM loyal_yield.multiply_operations WHERE operation_id='pilot-derived-hold'`); err != nil {
				t.Fatal(err)
			}
			first, err := db.activatePilotBudget(ctx, rpc)
			if err != nil {
				t.Fatal(err)
			}
			beforeRequests := requests
			var before string
			if err = db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&before); err != nil {
				t.Fatal(err)
			}
			// Release/reacquire under a different process identity; the marker and
			// canonical previous budget survive PostgreSQL jsonb key reordering.
			if _, err = db.ReleaseRouteLease(ctx); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDatabase(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err = reopened.AcquireRouteLease(ctx, productionRouteKey, "pilot-restart", time.Minute); err != nil {
				t.Fatal(err)
			}
			repeated, err := reopened.activatePilotBudget(ctx, rpc)
			if err != nil {
				t.Fatal(err)
			}
			if repeated.Authority != first.Authority || requests != beforeRequests {
				t.Fatal("retry refreshed authority or chain snapshot")
			}
			var after string
			if err = reopened.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, productionRouteKey).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("retry rewrote budget")
			}
			var stored struct {
				Budget    Phase3Budget `json:"phase3"`
				Unrelated string       `json:"unrelated"`
			}
			if err = json.Unmarshal([]byte(after), &stored); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if withPrior {
				want = 17_000_000
			}
			if stored.Budget.Families["OnRe"].SpentMicros != want || stored.Unrelated != "keep" {
				t.Fatal("transition erased prior data")
			}
			for name, mutation := range map[string]string{
				"absence-flag":   `jsonb_set(state,'{phase3,pilot,previousBudgetWasAbsent}',to_jsonb(NOT (state->'phase3'->'pilot'->>'previousBudgetWasAbsent')::boolean),true)`,
				"missing-family": `state #- '{phase3,families,AUTO}'`,
				"flat-slot":      `jsonb_set(state,'{pilotBudgetActivation,flatEvidence,slot}','1')`,
			} {
				// Existing-budget authority omits a false absence flag;
				// use an explicit contradictory true value in that case.
				if name == "absence-flag" && withPrior {
					mutation = `jsonb_set(state,'{phase3,pilot,previousBudgetWasAbsent}','true',true)`
				}
				if _, err = reopened.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=`+mutation+` WHERE route_key=$1`, productionRouteKey); err != nil {
					t.Fatal(err)
				}
				if _, err = reopened.activatePilotBudget(ctx, rpc); err == nil {
					t.Fatal("accepted corrupted activation", name)
				}
				if _, err = reopened.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2::jsonb WHERE route_key=$1`, productionRouteKey, after); err != nil {
					t.Fatal(err)
				}
			}
			if withPrior {
				if _, err = reopened.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,families,OnRe,spentMicros}','0') WHERE route_key=$1`, productionRouteKey); err != nil {
					t.Fatal(err)
				}
				if _, err = reopened.activatePilotBudget(ctx, rpc); err == nil {
					t.Fatal("accepted erased historical spend")
				}
				if _, err = reopened.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2::jsonb WHERE route_key=$1`, productionRouteKey, after); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = reopened.ReleaseRouteLease(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = db.AcquireRouteLease(ctx, productionRouteKey, "pilot-activation-test", time.Minute); err != nil {
				t.Fatal(err)
			}
		})
	}
}
