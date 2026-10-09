package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"reflect"
	"testing"
)

func debtTopupCapitalFixture(t *testing.T, deposit bool) (Observation, Decision, RouteManifest, *RPCClient, *jupiterClient, []ConfirmedAccount) {
	t.Helper()
	o, d, _, m, rpc, client, accounts := debtTopupAllocationFixture(t)
	loan, err := observeTopupLoanOrigin(context.Background(), rpc)
	if err != nil {
		t.Fatal(err)
	}
	origin := sha256Bytes([]byte("allocation"))
	tranche := &topupTranche{Generation: 1, Loan: loan, Lane: o.Snapshot.RouteLane, OriginOperationID: origin, LastOperationID: origin, LastSlot: o.Snapshot.Slot, LastEffectsSHA256: sha256Bytes([]byte("receipt")), AllocatedUSDCRaw: uint64(d.AmountRaw), USDCRemainingRaw: uint64(d.AmountRaw), Stage: topupTrancheAllocated, CollateralRemainingRaw: 1, DepositQuantumRaw: 2}
	d.Action, d.Reason = SwapStableToCollateralStep, topupSwapReason
	o.Snapshot.SquadsIdleRaw = d.AmountRaw
	o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = 1, 1
	if deposit {
		tranche.Generation = 2
		tranche.Stage = topupTrancheCollateral
		tranche.LastOperationID = sha256Bytes([]byte("swap"))
		tranche.USDCRemainingRaw = 0
		tranche.CollateralRemainingRaw = 2_000_000_000
		o.Snapshot.SquadsIdleRaw = 0
		o.Snapshot.CollateralIdleRaw, o.Snapshot.PrimeIdleRaw = int64(tranche.CollateralRemainingRaw), int64(tranche.CollateralRemainingRaw)
		d.Action, d.Reason, d.AmountRaw = OpenRouteStep, topupDepositReason, o.Snapshot.CollateralIdleRaw
	}
	o.Snapshot.TopupTranche = tranche
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeSquadsATA).Data[64:72], uint64(o.Snapshot.SquadsIdleRaw))
	binary.LittleEndian.PutUint64(accountAt(accounts, autoAUTOPYUSD.CollateralCustody).Data[64:72], uint64(o.Snapshot.CollateralIdleRaw))
	return o, d, m, rpc, client, accounts
}

func TestDebtTopupSwapPricesActualLoanAndOwnedCarry(t *testing.T) {
	o, d, m, rpc, client, accounts := debtTopupCapitalFixture(t, false)
	e, err := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), 1, o.Snapshot.Slot)
	if err != nil {
		t.Fatal(err)
	}
	e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
	before := append([]byte(nil), accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data...)
	plan, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	current, effects, wire, err := plan.Input.decodeWithManifest(m)
	wantWire, wireErr := CompileJupiterMessage(e.Request)
	gotJSON, _ := json.Marshal(current)
	wantJSON, _ := json.Marshal(e.Request)
	if err != nil || wireErr != nil || !bytes.Equal(wire, wantWire) || !bytes.Equal(gotJSON, wantJSON) || !reflect.DeepEqual(effects, e.ExpectedEffects) || plan.PayoffRepayment == nil || plan.ExitAfterMicros <= 0 || !bytes.Equal(before, accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data) {
		t.Fatalf("current input or actual debt changed err=%v request=%v effects=%v payoff=%v cost=%v debt=%v", err, reflect.DeepEqual(current, e.Request), reflect.DeepEqual(effects, e.ExpectedEffects), plan.PayoffRepayment != nil, plan.ExitAfterMicros, bytes.Equal(before, accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data))
	}
	upper, _ := withdrawalUSDCExitEstimate(e.Request.QuotedOutputRaw)
	if plan.BorrowRelease != nil || plan.FundingSwap == nil {
		t.Fatal("owned idle collateral should fund the return without a position release")
	}
	funding, fundingEffects, _, fundingErr := plan.FundingSwap.Input.decodeWithManifest(m)
	if fundingErr != nil || funding.(JupiterSwapRequest).AmountRaw != upper+1 || fundingEffects.Accounts[0].BeforeRaw != upper+1 || fundingEffects.Accounts[0].AfterRaw != 0 {
		t.Fatal("swap output and carry not counted exactly once", fundingErr)
	}
	payoff, _, _, payoffErr := plan.PayoffRepayment.decodeWithManifest(m)
	if payoffErr != nil || payoff.(KaminoPrimeUSDCRequest).FullPayoff {
		t.Fatal("cost-only payoff granted execution authority", payoffErr)
	}
	if plan.Snapshot != o.Snapshot || plan.BorrowProjection != nil {
		t.Fatal("projection became current authority")
	}
	for name, mutate := range map[string]func(*Observation){
		"missing origin": func(o *Observation) { o.Snapshot.TopupTranche = nil },
		"foreign cash":   func(o *Observation) { o.Snapshot.SquadsIdleRaw++ },
		"foreign carry":  func(o *Observation) { o.Snapshot.CollateralIdleRaw++; o.Snapshot.PrimeIdleRaw++ },
		"withdrawal":     func(o *Observation) { o.Snapshot.WithdrawalDemandRaw = 1 },
		"risk":           func(o *Observation) { o.Snapshot.LTVBPS = 6000 },
		"unwind":         func(o *Observation) { o.Snapshot.Unwind = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := o
			mutate(&changed)
			if _, err := observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, changed, d, e); err == nil {
				t.Fatal("unsafe topup admitted")
			}
		})
	}
}

func debtTopupDepositEvidence(t *testing.T, o Observation, d Decision, m RouteManifest, rpc *RPCClient, accounts []ConfirmedAccount) KaminoExecutionEvidence {
	t.Helper()
	route := autoAUTOPYUSD
	r, err := m.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, uint64(d.AmountRaw), LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, route.Lane)
	if err != nil {
		t.Fatal(err)
	}
	r.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	effects, err := boundedKaminoDepositEffects(accounts, route, o.Snapshot.Slot, r.AmountRaw)
	if err != nil {
		t.Fatal(err)
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	inner := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if err = json.Unmarshal(body, &call); err != nil {
			return nil, err
		}
		if call.Method != "simulateTransaction" {
			return inner.RoundTrip(request)
		}
		var encoded string
		var options struct{ Accounts struct{ Addresses []string } }
		json.Unmarshal(call.Params[0], &encoded)
		json.Unmarshal(call.Params[1], &options)
		wire, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(wire) < 65 || !allZero(wire[1:65]) || !bytes.Equal(wire[65:], message) {
			t.Fatal("unexpected simulation wire")
		}
		var rows []any
		moved := effects.Deposit.MinimumDebitRaw
		for _, address := range options.Accounts.Addresses {
			a := accountAt(accounts, address)
			a.Data = append([]byte(nil), a.Data...)
			switch address {
			case route.CollateralCustody:
				binary.LittleEndian.PutUint64(a.Data[64:72], uint64(o.Snapshot.CollateralIdleRaw)-moved)
			case route.CollateralLiquiditySupply:
				binary.LittleEndian.PutUint64(a.Data[64:72], binary.LittleEndian.Uint64(a.Data[64:72])+moved)
			case route.Kamino.Obligation:
				binary.LittleEndian.PutUint64(a.Data[128:136], uint64(o.Snapshot.PositionCollateralRaw)+moved/2)
			case route.Kamino.CollateralReserve:
				binary.LittleEndian.PutUint64(a.Data[224:232], binary.LittleEndian.Uint64(a.Data[224:232])+moved)
				binary.LittleEndian.PutUint64(a.Data[2592:2600], binary.LittleEndian.Uint64(a.Data[2592:2600])+moved/2)
			}
			rows = append(rows, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": o.Snapshot.Slot}, "value": map[string]any{"err": nil, "unitsConsumed": 200000, "accounts": rows}}})
		return response(string(payload)), nil
	})
	return KaminoExecutionEvidence{r, effects}
}

func TestDebtTopupDepositKeepsDebtAndCurrentWire(t *testing.T) {
	o, d, m, rpc, client, accounts := debtTopupCapitalFixture(t, true)
	e := debtTopupDepositEvidence(t, o, d, m, rpc, accounts)
	plan, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
	if err != nil {
		t.Fatal(err)
	}
	current, effects, _, err := plan.Input.decodeWithManifest(m)
	if err != nil || !reflect.DeepEqual(current, e.Request) || !reflect.DeepEqual(effects, e.ExpectedEffects) || plan.DepositProjection == nil || plan.BorrowProjection != nil || plan.Payoff == nil || plan.PayoffRepayment == nil || plan.ExitAfterMicros <= 0 {
		t.Fatal("missing debt-preserving projection/current input", err)
	}
	old, _ := decodeKaminoObligation(accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation), autoAUTOPYUSD.Kamino)
	post, _ := decodeKaminoObligation(accountAt(plan.DepositProjection.Accounts, autoAUTOPYUSD.Kamino.Obligation), autoAUTOPYUSD.Kamino)
	if post.debtRaw != old.debtRaw || post.collateralDepositedRaw <= old.collateralDepositedRaw {
		t.Fatal("deposit changed debt or omitted existing collateral")
	}
	wrong := e
	wrong.Request.ObligationReserves = []string{autoAUTOPYUSD.Kamino.DebtReserve, autoAUTOPYUSD.Kamino.CollateralReserve}
	if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, wrong); err == nil {
		t.Fatal("reordered reserves admitted")
	}
	bad := o
	bad.Snapshot.TopupTranche = nil
	if _, err := observePhase3DepositAdmission(context.Background(), rpc, client, m, bad, d, e); err == nil {
		t.Fatal("unowned deposit admitted")
	}
}

func topupSignedFixture(t *testing.T, p phase3BridgeAdmission) (phase3OperationAuthorization, PersistedOperation) {
	t.Helper()
	req, _, message, err := p.Input.decode()
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0] = 1
	intent, err := Phase3IntentDigest(req, p.Input.Effects)
	if err != nil {
		t.Fatal(err)
	}
	t0 := p.Snapshot.TopupTranche
	auth := phase3OperationAuthorization{PilotAuthorityID: pilotBudgetAuthorityID, GoalID: Phase3GoalID, IntentSHA256: intent, BuildInput: p.Input, BridgeAdmission: &p, SignedWireSHA256: sha256Bytes(wire), Topup: &topupTrancheBinding{Before: t0, Loan: t0.Loan, Lane: t0.Lane, OriginOperationID: t0.OriginOperationID, AllocatedUSDCRaw: t0.AllocatedUSDCRaw}}
	op := PersistedOperation{Status: Signed, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
	return auth, op
}

func TestDebtTopupFreshBuildSendAndProjectionRejectPrincipalChange(t *testing.T) {
	for _, deposit := range []bool{false, true} {
		t.Run(map[bool]string{false: "swap", true: "deposit"}[deposit], func(t *testing.T) {
			o, d, m, rpc, client, accounts := debtTopupCapitalFixture(t, deposit)
			var plan phase3BridgeAdmission
			var err error
			if deposit {
				e := debtTopupDepositEvidence(t, o, d, m, rpc, accounts)
				leg, _, _, err := selectKaminoLeg(true, d, KaminoPosition{CollateralDepositedRaw: uint64(o.Snapshot.PositionCollateralRaw), DebtRaw: uint64(o.Snapshot.PositionDebtRaw)})
				if err != nil || leg != kaminoLegDeposit {
					t.Fatal("topup selected nondeposit", err)
				}
				refresh := kaminoPrimeUSDCRefreshInstructionsForRequest(leg, e.Request)
				if len(refresh) != 3 || len(refresh[2].accounts) != 4 || refresh[0].accounts[0].key != mustKey(autoAUTOPYUSD.Kamino.CollateralReserve) || refresh[1].accounts[0].key != mustKey(autoAUTOPYUSD.Kamino.DebtReserve) || refresh[2].accounts[2].key != mustKey(autoAUTOPYUSD.Kamino.CollateralReserve) || refresh[2].accounts[3].key != mustKey(autoAUTOPYUSD.Kamino.DebtReserve) {
					t.Fatal("deposit refresh reserve order changed")
				}
				plan, err = observePhase3DepositAdmission(context.Background(), rpc, client, m, o, d, e)
			} else {
				e, quoteErr := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, d, uint64(d.AmountRaw), 1, o.Snapshot.Slot)
				if quoteErr != nil {
					t.Fatal(quoteErr)
				}
				e.Request.EntryReturnReserved, e.Request.TopupReturnReserved = true, true
				plan, err = observePhase3EntrySwapAdmission(context.Background(), rpc, client, m, o, d, e)
			}
			if err != nil {
				t.Fatal(err)
			}
			auth, op := topupSignedFixture(t, plan)
			encoded, encodeErr := json.Marshal(auth)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			var restarted phase3OperationAuthorization
			if err := json.Unmarshal(encoded, &restarted); err != nil {
				t.Fatal(err)
			}
			auth = restarted
			if err := validateTopupCapitalAuthorization(context.Background(), rpc, auth, o.Snapshot.Slot); err != nil {
				t.Fatal("fresh build", err)
			}
			if _, err := m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err != nil {
				t.Fatal("fresh send", err)
			}
			for _, variant := range []string{"expired", "budget", "origin", "marker"} {
				changed := auth
				p := *auth.BridgeAdmission
				changed.BridgeAdmission = &p
				b := *auth.Topup
				changed.Topup = &b
				switch variant {
				case "expired":
					p.ValidThroughSlot = o.Snapshot.Slot - 1
				case "budget":
					p.Payoff = nil
				case "origin":
					b.OriginOperationID = sha256Bytes([]byte("foreign"))
				case "marker":
					b.Loan.BorrowedAtUnix++
				}
				if err := validateTopupCapitalAuthorization(context.Background(), rpc, changed, o.Snapshot.Slot); err == nil {
					t.Fatalf("%s accepted", variant)
				}
			}
			missing := auth
			missing.Topup = nil
			if _, err := m.revaluePhase3SignedInput(context.Background(), rpc, missing, op); err == nil {
				t.Fatal("missing binding allowed at send")
			}
			data := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data
			data[1296]++ // one SF bit, invisible to a rounded raw-debt comparison
			if err := validateTopupCapitalAuthorization(context.Background(), rpc, auth, o.Snapshot.Slot); err == nil {
				t.Fatal("build accepted changed principal")
			}
			if _, err := m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
				t.Fatal("send accepted changed principal")
			}
			data[1296]--
			if deposit {
				projected := plan.DepositProjection.Accounts
				data := accountAt(projected, autoAUTOPYUSD.Kamino.Obligation).Data
				data[1296]++
				if err := validateTopupDepositPrincipal(accounts, projected, autoAUTOPYUSD); err == nil {
					t.Fatal("projection accepted subraw principal change")
				}
				data[1296]--
			}
			pin := reviewedTopupKaminoIdentity()
			accountAt(accounts, pin.programData).Data[100] ^= 1
			if _, err := m.revaluePhase3SignedInput(context.Background(), rpc, auth, op); err == nil {
				t.Fatal("send accepted unreviewed program")
			}
		})
	}
}

func TestDebtTopupDepositExactAccruedFraction(t *testing.T) {
	_, _, _, _, _, accounts := debtTopupCapitalFixture(t, true)
	route := autoAUTOPYUSD
	old := accountAt(accounts, route.Kamino.Obligation).Data
	amount := new(big.Int).Add(littleInt(old[1296:1312]), big.NewInt(37))
	putScaledFraction(old[1296:1312], amount)
	after := make([]ConfirmedAccount, len(accounts))
	for i, a := range accounts {
		after[i] = a
		after[i].Data = append([]byte(nil), a.Data...)
	}
	former := littleInt(old[1240:1272])
	rate := new(big.Int).Add(former, new(big.Int).Quo(new(big.Int).Set(former), big.NewInt(1_000_000)))
	next := accountAt(after, route.Kamino.Obligation).Data
	putScaledFraction(next[1240:1272], rate)
	putScaledFraction(accountAt(after, route.Kamino.DebtReserve).Data[296:328], rate)
	want := new(big.Int).Quo(new(big.Int).Mul(amount, rate), former)
	putScaledFraction(next[1296:1312], want)
	if err := validateTopupDepositPrincipal(accounts, after, route); err != nil {
		t.Fatal("legitimate single refresh rejected", err)
	}
	putScaledFraction(next[1296:1312], new(big.Int).Add(want, big.NewInt(1)))
	if err := validateTopupDepositPrincipal(accounts, after, route); err == nil {
		t.Fatal("same-raw fractional principal change accepted")
	}
	putScaledFraction(next[1296:1312], want)
	binary.LittleEndian.PutUint64(next[1288:1296], binary.LittleEndian.Uint64(old[1288:1296])+1)
	if err := validateTopupDepositPrincipal(accounts, after, route); err == nil {
		t.Fatal("borrow marker change accepted")
	}
}
