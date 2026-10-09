package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// One original allocation funds every following admission. Receipts and RPC
// responses are offline fixtures; no production key, network or chain send.
func TestTopupRecoveryChainedDB(t *testing.T) {
	for _, mode := range []string{"allocated", "collateral", "normal", "allocation_100k_cost_hold"} {
		t.Run(mode, func(t *testing.T) { testTopupRecoveryChain(t, mode) })
	}
}

func testTopupRecoveryChain(t *testing.T, mode string) {
	if os.Getenv("PHASE3_TEST_DATABASE_URL") == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_topup_ledger_test", 5*time.Minute)
	defer cancel()
	// Match the lifecycle columns from migration 0070 omitted by the older test schema.
	if _, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations ADD COLUMN IF NOT EXISTS simulation_slot bigint CHECK (simulation_slot IS NULL OR simulation_slot>0), ADD COLUMN IF NOT EXISTS simulation_result jsonb`); err != nil {
		t.Fatal(err)
	}
	o, _, _, m, rpc, client, accounts := autoEmergencyFundingFixture(t)
	slot := o.Snapshot.Slot
	var withdrawalReceipts []programAccount
	var sawWithdrawal, sawWithdrawalDisappear bool
	readObservation := func() (Observation, []ConfirmedAccount, error) {
		reader, finalized := fixtureBatchRuntime(slot, accounts)
		return observeConfirmedRouteSnapshotWithAccounts(ctx, m, routeObservationRuntime{
			confirmedSlot: func(context.Context) (int64, error) { return slot, nil },
			receipts:      func(context.Context, int64) (int64, []programAccount, error) { return slot, withdrawalReceipts, nil },
			accounts:      reader, finalizedReceipt: finalized, now: func() time.Time { return time.Unix(kaminoFixtureUnix, 0).UTC() },
		})
	}
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, e := io.ReadAll(req.Body)
		if e != nil {
			return nil, e
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var call struct{ Method string }
		if e = json.Unmarshal(body, &call); e != nil {
			return nil, e
		}
		if call.Method == "getProgramAccounts" {
			values := make([]any, 0, len(withdrawalReceipts))
			for _, r := range withdrawalReceipts {
				values = append(values, map[string]any{"pubkey": r.Address, "account": map[string]any{"owner": r.Account.Owner, "lamports": r.Account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(r.Account.Data), "base64"}}})
			}
			raw, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": slot}, "value": values}})
			if e != nil {
				return nil, e
			}
			return response(string(raw)), nil
		}
		res, e := inner.RoundTrip(req)
		if e != nil {
			return nil, e
		}
		defer res.Body.Close()
		var payload map[string]any
		if e = json.NewDecoder(res.Body).Decode(&payload); e != nil {
			return nil, e
		}
		if call.Method == "getSlot" {
			payload["result"] = slot
		} else if result, ok := payload["result"].(map[string]any); ok {
			if c, ok := result["context"].(map[string]any); ok {
				c["slot"] = slot
			}
		}
		raw, e := json.Marshal(payload)
		if e != nil {
			return nil, e
		}
		return response(string(raw)), nil
	})
	route := autoAUTOPYUSD
	normal := strings.HasPrefix(mode, "normal")
	amount := uint64(20_000_000)
	if mode == "allocation_100k_cost_hold" {
		amount = uint64(PilotWorkingTrancheCapRaw)
	}
	price := accountAt(accounts, route.Kamino.CollateralReserve).Data[248:264]
	riskPrice := append([]byte(nil), price...)
	putScaledFraction(price, new(big.Int).Mul(littleInt(price), big.NewInt(2)))
	reference, e := pinnedKaminoObservationConfig()
	if e != nil {
		t.Fatal(e)
	}
	// Quote at the fixture's actual current reserve prices, including unequal decimals.
	client.http.Transport = autoJupiterTransport(t, route, func(in, destination string, amount uint64) (uint64, uint64) {
		priceFor := func(mint string) (uint8, [16]byte) {
			reserve := reference.DebtReserve
			if mint == route.Kamino.CollateralMint {
				reserve = route.Kamino.CollateralReserve
			}
			if mint == route.Kamino.DebtMint {
				reserve = route.Kamino.DebtReserve
			}
			var p [16]byte
			copy(p[:], accountAt(accounts, reserve).Data[248:264])
			return uint8(binary.LittleEndian.Uint64(accountAt(accounts, reserve).Data[272:280])), p
		}
		sourceDecimals, sourcePrice := priceFor(in)
		destinationDecimals, destinationPrice := priceFor(destination)
		out, e := valueBetweenTokenRaw(amount, sourceDecimals, destinationDecimals, sourcePrice, destinationPrice, false)
		if e != nil {
			t.Fatal(e)
		}
		return out, out * 9950 / 10000
	}, nil).http.Transport
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], amount)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], 0)
	binary.LittleEndian.PutUint64(accountAt(accounts, route.CollateralCustody).Data[64:72], 0)
	key := "chained-" + mode
	state := planningPilotState(t)
	budget := state["phase3"].(Phase3Budget)
	budget.Families["AUTO"] = FamilyBudget{SpentMicros: 10000, ExitMicros: 5000000}
	state["phase3"], state["generation"] = budget, 3
	raw, _ := json.Marshal(state)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,3)`, key, raw); err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireRouteLease(ctx, key, "chain", 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	var originalLoan topupLoan
	var recoveryReserve, recoverySpent int64
	var current *topupTranche
	lastNAV := int64(-1)
	lastSequence := int64(40)
	observe := func() Observation {
		t.Helper()
		fresh, _, err := readObservation()
		if err != nil {
			for _, a := range accounts {
				if a.Owner == kaminoProgram && (len(a.Data) == kaminoObligationLength || len(a.Data) == kaminoReserveLength) {
					t.Logf("account %s length %d slot %d", a.Address, len(a.Data), binary.LittleEndian.Uint64(a.Data[16:24]))
				}
			}
			t.Fatal("observe", err)
		}
		if lastNAV < 0 {
			lastNAV = fresh.Snapshot.StrategyNAVRaw
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyReceipt).Data[104:112], uint64(lastNAV))
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[168:176], uint64(lastNAV+fresh.Snapshot.VoltrIdleRaw))
			fresh, _, err = readObservation()
			if err != nil {
				t.Fatal(err)
			}
		}
		fresh.ObservedAt = time.Now().UTC()
		p, err := db.readRoutePlanningStateOnManifest(ctx, m, key, true)
		if err != nil {
			t.Fatal("planning", err)
		}
		current = p.topup
		s := &fresh.Snapshot
		if s.WithdrawalDemandRaw > 0 {
			sawWithdrawal = true
		} else if sawWithdrawal {
			sawWithdrawalDisappear = true
		}
		s.TopupTranche, s.PilotActive, s.PilotTrancheCapLane = current, true, route.Lane
		s.PolicyReady, s.ExitBuildable, s.ProgramIdentityKnown = true, true, true
		s.VoltrProgramDeploySlot, s.AdaptorProgramDeploySlot = voltrProgramDeploySlot, adaptorProgramDeploySlot
		s.TopupDepositRoomRaw = int64(amount)
		s.JournalSequenceKnown, s.JournalReconciledSequenceRaw = true, lastSequence
		if lastNAV < 0 {
			lastNAV = s.StrategyNAVRaw
		}
		s.PriorReportedNAVRaw, s.JournalArmedNAVRaw, s.JournalArmedNAVKnown = lastNAV, lastNAV, true
		s.CapitalMutated = s.StrategyNAVRaw != lastNAV
		journal, e := db.ReconciledBridgeJournal(ctx, key)
		if e != nil {
			t.Fatal(e)
		}
		s.StagedAmountRaw, s.StagedAmountKnown, s.StageTransient = journal.StagedAmountRaw, journal.StagedAmountKnown, journal.StageAfterTicket
		s.PostMutationNAVRequired, err = db.PostMutationNAVRequired(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = applyUnwindIntentWithLane(s, p.unwind, m.selectorEntryLaneAllowed); err != nil {
			t.Fatal(err)
		}
		return fresh
	}
	localKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, ed25519.SeedSize))
	localDelegate := publicKeyFromBytes(localKey.Public().(ed25519.PublicKey))
	var actions []Action
	for step := 0; step < 20; step++ {
		o = observe()
		if normal && current != nil && current.Stage == topupTrancheComplete && !o.Snapshot.PostMutationNAVRequired {
			break
		}
		d := m.DecideOnManifest(o.Snapshot)
		if d.Action == Hold && d.Reason == "unwind_complete" {
			break
		}
		if d.Action == Hold || d.Action == HoldManualRecovery {
			t.Fatalf("step %d held: %+v", step, d)
		}
		t.Logf("step %d: %s %s", step, d.Action, d.Reason)
		id := sha256Bytes([]byte(fmt.Sprintf("%s-%d", key, step)))
		var request any
		var effects ExpectedEffects
		var bridge BridgeExecutionEvidence
		var swap JupiterExecutionEvidence
		var kamino KaminoExecutionEvidence
		switch d.Action {
		case VoltrAllocateToSquads, ReportNAV, StageSquadsToVoltr, VoltrRestoreIdle:
			r := bridgeTestRequest(d.Action, uint64(d.AmountRaw))
			r.Report.ObservedSlot, r.Report.Sequence = uint64(slot), uint64(slot)
			nav := o.Snapshot.StrategyNAVRaw
			if d.Action == VoltrAllocateToSquads {
				nav += d.AmountRaw
			}
			if d.Action == StageSquadsToVoltr {
				nav -= d.AmountRaw
			}
			r.Report.NAVAfterRaw = uint64(nav)
			effects, _, _, err = bridgeExpectedEffects(d, uint64(o.Snapshot.VoltrIdleRaw), uint64(o.Snapshot.VoltrStrategyIdleRaw), uint64(o.Snapshot.SquadsIdleRaw))
			effects.Kind = "bridge"
			if d.Action != StageSquadsToVoltr {
				effects.ReturnData = expectedAdaptorReturnData(uint64(nav))
			}
			bridge = BridgeExecutionEvidence{r, effects}
			request = r
		case SwapStableToCollateralStep, SwapCollateralToDebtStep, SwapCollateralToStableStep, SwapDebtToUSDCStep:
			_, _, source, destination, edgeErr := jupiterEdgeForRoute(d.Action, route.Lane)
			if edgeErr != nil {
				t.Fatal(edgeErr)
			}
			sourceRaw := binary.LittleEndian.Uint64(accountAt(accounts, source).Data[64:72])
			destRaw := binary.LittleEndian.Uint64(accountAt(accounts, destination).Data[64:72])
			swap, err = prepareJupiterQuoteEvidence(ctx, rpc, client, m, d, sourceRaw, destRaw, slot)
			if d.Action == SwapStableToCollateralStep {
				swap.Request.EntryReturnReserved, swap.Request.TopupReturnReserved = true, true
			}
			if d.Action == SwapCollateralToDebtStep {
				swap.Request.FullPayoffFunding = true
			}
			request, effects = swap.Request, swap.ExpectedEffects
		case OpenRouteStep:
			if d.Reason != topupDepositReason {
				t.Fatal("unexpected capital action", d)
			}
			kamino = debtTopupDepositEvidence(t, o, d, m, rpc, accounts)
			request, effects = kamino.Request, kamino.ExpectedEffects
		case DeleverRouteStep:
			leg, amount := kaminoLegWithdraw, uint64(o.Snapshot.PositionCollateralRaw)
			if o.Snapshot.PositionDebtRaw > 0 {
				leg, amount = kaminoLegRepay, uint64(o.Snapshot.PayoffDebtRaw)
			}
			r, e := m.kaminoPacketForRoute(d.Action, leg, amount, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
			if e != nil {
				t.Fatal(e)
			}
			r.ObligationReserves = []string{route.Kamino.CollateralReserve}
			source, dest := kaminoLegCustodiesForRoute(leg, route)
			if leg == kaminoLegRepay {
				r.ObligationReserves = append(r.ObligationReserves, route.Kamino.DebtReserve)
				r.FullPayoff = true
				bound, e := decodeKaminoPayoffWindow(accounts, route, slot, 3)
				if e != nil {
					t.Fatal(e)
				}
				r, err = m.kaminoPacketForRoute(d.Action, leg, bound.UpperDebtRaw, LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
				if err != nil {
					t.Fatal(err)
				}
				r.FullPayoff = true
				r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
				effects, err = boundedKaminoRepaymentEffects(accounts, source, dest, bound.ObservedDebtRaw, bound.UpperDebtRaw)
			} else {
				reserve, e := decodeKaminoReserve(accountAt(accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
				if e != nil {
					t.Fatal(e)
				}
				redeemed, e := reserve.redeemLiquidityRaw(amount)
				if e != nil {
					t.Fatal(e)
				}
				effects, err = exactKaminoTokenEffects(accounts, source, dest, redeemed)
			}
			kamino = KaminoExecutionEvidence{r, effects}
			request = r
		default:
			t.Fatalf("unexpected action %s", d.Action)
		}
		if err != nil {
			t.Fatal("prepare", err)
		}
		// Read the same journal-derived ownership proof used before production decision persistence.
		if _, spend, shared := autoSharedCustodySpend(route.Lane, key, effects); shared && spend > 0 {
			proof, e := db.ObserveSharedCustodyOwnershipProof(ctx, m, autoSharedPYUSDAttributionConfig(route, key), effects, uint64(debtCashRaw(o.Snapshot)), slot)
			if e != nil {
				t.Fatal("ownership", e)
			}
			o.custodyProof = &proof
		}
		body, _ := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "decision": newDecisionEvidence(o, d, m.SHA256, *m.PolicyCatalog.SHA256), "topupTranche": current})
		if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(d.Action), d.StrategyKey, body); err != nil {
			t.Fatal(err)
		}
		runtime := productionTickRuntime(db, rpc, m, Credentials{})
		if d.Reason == topupRiskEntryReason {
			var originalState, before, after, beforeOperation, afterOperation []byte
			readState := func(state, operation *[]byte) {
				t.Helper()
				if e := db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(state); e != nil {
					t.Fatal(e)
				}
				if e := db.pool.QueryRow(ctx, `SELECT to_jsonb(o) FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, id).Scan(operation); e != nil {
					t.Fatal(e)
				}
			}
			readState(&originalState, &beforeOperation)
			if _, e := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,families,AUTO,exitMicros}','1'::jsonb) WHERE route_key=$1`, key); e != nil {
				t.Fatal(e)
			}
			readState(&before, &beforeOperation)
			assertBudgetHold(t, runtime.admitJupiter(ctx, id, o, d, swap), "recovery_exceeds_reserved_exit")
			readState(&after, &afterOperation)
			if !bytes.Equal(before, after) || !bytes.Equal(beforeOperation, afterOperation) {
				t.Fatal("insufficient reserve rejection mutated durable state")
			}
			if _, e := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=$2 WHERE route_key=$1`, key, originalState); e != nil {
				t.Fatal(e)
			}
		}
		switch request.(type) {
		case BridgeBuildRequest:
			err = runtime.admitBridge(ctx, id, o, d, bridge)
		case JupiterSwapRequest:
			err = runtime.admitJupiter(ctx, id, o, d, swap)
		case KaminoPrimeUSDCRequest:
			err = runtime.admitKamino(ctx, id, o, d, kamino)
		}
		if mode == "allocation_100k_cost_hold" {
			assertBudgetHold(t, err, "topup_entry_execution_cost_cap_exhausted")
			var admitted bool
			var stateAfter []byte
			if e := db.pool.QueryRow(ctx, `SELECT expected_effects ? 'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&admitted); e != nil {
				t.Fatal(e)
			}
			if e := db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&stateAfter); e != nil {
				t.Fatal(e)
			}
			var before, after any
			json.Unmarshal(raw, &before)
			json.Unmarshal(stateAfter, &after)
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if admitted || !bytes.Equal(beforeJSON, afterJSON) || current != nil || o.Snapshot.VoltrIdleRaw != int64(amount) || o.Snapshot.SquadsIdleRaw != 0 || len(actions) != 0 {
				t.Fatal("unaffordable allocation changed cash or durable authority")
			}
			plan, e := observePhase3TopupAllocationAdmission(ctx, rpc, client, m, o, d, bridge)
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("$100k refused before allocation: next swap execution bound %d versus cap %d", plan.Exit[0].Cost.ExecutionCost.TotalMicros, PilotEntryExecutionCostCapMicros)
			return
		}
		if err != nil {
			t.Fatal("admit", d.Action, err)
		}
		auth := loadAutoInitializerAuth(t, ctx, db, id)
		if step == 0 {
			originalLoan = auth.Topup.Loan
			recoveryReserve = auth.BridgeAdmission.ExitAfterMicros
		}
		if !normal && step > 0 && (auth.BridgeAdmission.CurrentCost.TotalMicros+auth.BridgeAdmission.ExitAfterMicros > recoveryReserve-recoverySpent) {
			t.Fatal("recovery exceeded original allocation reserve")
		}
		if step > 0 {
			recoverySpent += auth.BridgeAdmission.CurrentCost.TotalMicros
		}
		rawEffects, _ := json.Marshal(effects)
		if err = db.authorizePhase3BuildOnManifest(ctx, m, rpc, id, request, rawEffects, auth.BridgeAdmission.CurrentCost); err != nil {
			if auth.DebtClear != nil {
				check := m
				check.selectorObservation = true
				check.observationLane = route.Lane
				_, e := ObserveConfirmedRouteSnapshot(ctx, rpc, check)
				t.Log("fresh risk observation", e)
			}
			t.Fatal("build", err)
		}
		_, _, wire, err := auth.BuildInput.decodeWithManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.markBuiltOnManifest(ctx, m, id, sha256Bytes(wire), rawEffects); err != nil {
			t.Fatal("persist build", err)
		}
		var localWire []byte
		switch r := request.(type) {
		case BridgeBuildRequest:
			localWire, err = compileBridgeMessageForDelegate(r, localDelegate)
		case JupiterSwapRequest:
			localWire, err = m.compileJupiterMessage(r, localDelegate)
		case KaminoPrimeUSDCRequest:
			localWire, err = m.compileKaminoMessage(r, localDelegate)
		}
		if err != nil {
			t.Fatal(err)
		}
		signedCheck := signedTestBuildResult(t, localKey, localWire)
		signedCheck.RecentBlockhash = bridgeVault
		if err = signedCheck.validateForDelegate(localDelegate); err != nil {
			t.Fatal("persist/sign wire validation", err)
		}
		if err = db.MarkSimulated(ctx, id, SimulationResult{Slot: slot}); err != nil {
			t.Fatal("persist simulation", err)
		}
		// No production signature is available or used. Persist the compiled message's fixture envelope.
		signed := append(make([]byte, 65), wire...)
		signed[0] = 1
		hash := sha256Bytes(signed)
		tx, e := db.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if err = db.bindPhase3WireTx(ctx, tx, id, hash); err == nil {
			_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3 WHERE operation_id=$1`, id, signed, hash)
		}
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// Reopen the DB and reacquire the lease after every wire is persisted.
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
			t.Fatal(err)
		}
		db.Close()
		db, err = OpenDatabase(ctx, strings.Replace(os.Getenv("PHASE3_TEST_DATABASE_URL"), "/phase3_budget_test", "/phase3_topup_ledger_test", 1))
		if err != nil {
			t.Fatal(err)
		}
		lease, err = db.AcquireRouteLease(ctx, key, "chain", 4*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_ = lease
		auth = loadAutoInitializerAuth(t, ctx, db, id)
		op := PersistedOperation{Status: Signed, SignedWire: signed, SignedWireSHA256: hash, TransactionSignature: encodeBase58(signed[1:65]), RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
		cost, e := m.revaluePhase3SignedInput(ctx, rpc, auth, op)
		if e != nil {
			t.Fatal("restarted send valuation", e)
		}
		risk, e := verifyDebtClearEmergency(m, o, d, id, time.Now().UTC())
		if e != nil {
			t.Fatal(e)
		}
		tx, e = db.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if err = db.authorizePhase3SendTxOnManifest(ctx, m, tx, id, auth.IntentSHA256, hash, cost, slot, risk); err != nil {
			tx.Rollback(ctx)
			t.Fatal("send", err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		after := make([]uint64, len(effects.Accounts))
		for i, a := range effects.Accounts {
			after[i] = a.AfterRaw
			if effects.Deposit != nil {
				after[i] = binary.LittleEndian.Uint64(accountAt(auth.BridgeAdmission.DepositProjection.Accounts, a.Address).Data[64:72])
			}
		}
		receipt := topupReceipt(t, effects, slot, after...)
		receipt.Signature = sha256Bytes([]byte(id + "-receipt"))
		rec, actual, e := m.ReconcileConfirmedTransaction(effects, receipt)
		if e != nil {
			t.Fatal("receipt", e)
		}
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='reconciling',transaction_signature=$2,confirmed_slot=$3 WHERE operation_id=$1`, id, receipt.Signature, slot); err != nil {
			t.Fatal(err)
		}
		if err = db.markReconciledOnManifest(ctx, m, id, rec, actual, receipt); err != nil {
			t.Fatal("reconcile", err)
		}
		for _, a := range receipt.PostTokenBalances {
			binary.LittleEndian.PutUint64(accountAt(accounts, a.Address).Data[64:72], a.Raw)
		}
		if r, ok := request.(BridgeBuildRequest); ok && r.Action != StageSquadsToVoltr {
			lastNAV = int64(r.Report.NAVAfterRaw)
			lastSequence = int64(r.Report.Sequence)
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyReceipt).Data[104:112], r.Report.NAVAfterRaw)
			idle := binary.LittleEndian.Uint64(accountAt(accounts, bridgeIdleATA).Data[64:72])
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[168:176], idle+r.Report.NAVAfterRaw)
			binary.LittleEndian.PutUint64(accountAt(accounts, reportTicketPDA).Data[48:56], r.Report.Sequence)
		}
		if r, ok := request.(KaminoPrimeUSDCRequest); ok {
			_, leg, _ := kaminoPrimeUSDCInstruction(r)
			obligation := accountAt(accounts, route.Kamino.Obligation)
			if leg == kaminoLegDeposit {
				for _, a := range auth.BridgeAdmission.DepositProjection.Accounts {
					if a.Owner == kaminoProgram {
						copy(accountAt(accounts, a.Address).Data, a.Data)
					}
				}
			}
			if leg == kaminoLegRepay {
				for i := 1208; i < 1312; i++ {
					obligation.Data[i] = 0
				}
			}
			if leg == kaminoLegWithdraw {
				for i := 96; i < 136; i++ {
					obligation.Data[i] = 0
				}
			}
		}
		actions = append(actions, d.Action)
		slot++
		binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], uint64(slot))
		for _, a := range accounts {
			if a.Owner == kaminoProgram && (len(a.Data) == kaminoReserveLength || len(a.Data) == kaminoObligationLength) {
				binary.LittleEndian.PutUint64(a.Data[16:24], uint64(slot))
			}
		}
		if mode == "allocated" && step == 0 || mode == "collateral" && d.Reason == topupSwapReason {
			copy(price, riskPrice)
		}
		if d.Reason == topupRiskEntryReason {
			address, data := receiptFixture(t, bridgeVoltrProgram, bridgeVoltrVault, testPublicKey(83), 1, 1<<48)
			withdrawalReceipts = []programAccount{{Address: address, Account: ConfirmedAccount{Address: address, Owner: bridgeVoltrProgram, Lamports: 1, Data: data}}}
		}
		if k, ok := request.(KaminoPrimeUSDCRequest); ok && k.FullPayoff {
			withdrawalReceipts = nil
		}
		if d.Action == SwapCollateralToDebtStep {
			putScaledFraction(price, new(big.Int).Mul(littleInt(price), big.NewInt(2)))
		}
		current, err = db.LoadTopupTranche(ctx, key)
		if err != nil {
			t.Fatal("restart ancestry", err)
		}
		if current != nil && current.Loan != originalLoan {
			t.Fatal("loan origin rebased")
		}
	}
	o = observe()
	if normal {
		obligation, e := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
		if e != nil {
			t.Fatal(e)
		}
		if current == nil || current.Stage != topupTrancheComplete || o.Snapshot.PostMutationNAVRequired || obligation.collateralDepositedRaw <= originalLoan.CollateralRaw || o.Snapshot.SquadsIdleRaw != 0 {
			t.Fatal("normal topup incomplete")
		}
		marker, e := topupBorrowMarker(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
		// This fixture has no interest accrual; the deposit must change only collateral.
		if e != nil || marker != originalLoan.BorrowedAtUnix || obligation.debtAmountSF != originalLoan.DebtAmountSF || obligation.cumulativeBorrowRate != originalLoan.CumulativeBorrowRate || debtCashRaw(o.Snapshot) != 0 {
			t.Fatal("normal topup changed original principal", e)
		}
		for _, a := range actions {
			if a == DeleverRouteStep || a == SwapCollateralToDebtStep {
				t.Fatal("normal topup repaid debt")
			}
		}
		binary.LittleEndian.PutUint64(accountAt(accounts, bridgeIdleATA).Data[64:72], amount)
		book := accountAt(accounts, bridgeVoltrVault).Data[168:176]
		binary.LittleEndian.PutUint64(book, binary.LittleEndian.Uint64(book)+amount)
		next := observe().Snapshot
		if d := m.DecideOnManifest(next); d.Action != VoltrAllocateToSquads || d.Reason != topupAllocationReason || d.AmountRaw != int64(amount) {
			t.Fatal("completed topup stranded the next deposit", d)
		}
		t.Logf("normal topup completed with unchanged principal: %v", actions)
		return
	}
	if o.Snapshot.PositionDebtRaw != 0 || o.Snapshot.PositionCollateralRaw != 0 || o.Snapshot.SquadsIdleRaw != 0 || o.Snapshot.CollateralIdleRaw != 0 || debtCashRaw(o.Snapshot) != 0 || o.Snapshot.VoltrStrategyIdleRaw != 0 || current == nil || current.Stage != topupTrancheComplete {
		t.Fatalf("incomplete recovery after %v: %+v", actions, o.Snapshot)
	}
	if mode == "allocated" && (!sawWithdrawal || !sawWithdrawalDisappear) {
		t.Fatal("withdrawal arrival/disappearance not observed")
	}
	t.Logf("complete recovery under original reserve %d, cost %d: %v", recoveryReserve, recoverySpent, actions)
}
