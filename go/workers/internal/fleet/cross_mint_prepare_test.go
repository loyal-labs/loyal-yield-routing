package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type crossMintPrepareTransport func(*http.Request) (*http.Response, error)

func (f crossMintPrepareTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func crossMintPrepareRPC(t *testing.T, call func(string, []json.RawMessage) any) *chain.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": call(request.Method, request.Params)})
	}))
	t.Cleanup(server.Close)
	return testChain(t, server.URL)
}

func crossMintPreparationFixture(t *testing.T) (CrossMintPreparationRequest, crossMintPlan, crossMintPreparationBank) {
	t.Helper()
	vault, settings, signer := testPubkey(91), testPubkey(92), testPubkey(93)
	_, withdraw := connectedPolicyHeader(t, settings, signer, 1)
	_, deposit := connectedPolicyHeader(t, settings, signer, 2)
	_, swap := connectedPolicyHeader(t, settings, signer, 3)
	b := CrossMintPolicyBindings{Settings: settings, VaultPubkey: vault, DelegatedSigner: signer,
		Withdraw: CrossMintEarnPolicyBinding{PolicyAccount: withdraw, ObservedSlot: 900, SourceCommitment: "finalized", ConstraintIndex: 0},
		Deposit:  CrossMintEarnPolicyBinding{PolicyAccount: deposit, ObservedSlot: 900, SourceCommitment: "finalized", ConstraintIndex: 0},
		Swap:     CrossMintSwapPolicyBinding{PolicyAccount: swap, ObservedSlot: 900, SourceCommitment: "finalized", MaxSlippageBPS: 50, DailySourceMintSpendingCap: 10_000_000, SourceShard: "classic", ManifestFingerprint: strings.Repeat("a", 64)}}
	plan := crossMintPlan{Kind: "cross_mint_jupiter", SourceMint: USDCMint, TargetMint: USDTMint, Amount: 999, ValueLoss: 50, Bindings: b}
	raw, _ := json.Marshal(plan)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["source_collateral_amount_raw"] = json.RawMessage(`999`)
	fields["source_recovery_anchor_collateral_raw"] = json.RawMessage(`1`)
	fields["redeemable_source_liquidity_amount_raw"] = json.RawMessage(`999`)
	raw, _ = json.Marshal(fields)
	m := CrossMintPreparationMovement{DecisionID: 1, OpportunityID: 2, OptimizerEpochID: 3, VaultID: 4, Cluster: "localnet", VaultPubkey: vault, SourceReserve: testPubkey(51), IntendedTargetReserve: testPubkey(61), ActiveTargetReserve: testPubkey(61), SourceMint: USDCMint, TargetMint: USDTMint, PlannedAmountRaw: 999, ExecutionPlan: raw, PreflightCertification: json.RawMessage(`{"kind":"retained-preflight"}`), CustodyMint: USDCMint, CustodyAmountRaw: 999, Phase: "source_reserve"}
	bank := crossMintPreparationBank{slot: 1000, observedAt: time.Now(), accounts: map[string]chain.Account{}}
	for i, id := range []ReserveIdentity{{m.SourceReserve, testPubkey(52), USDCMint}, {m.IntendedTargetReserve, testPubkey(62), USDTMint}} {
		a := reserveFixture(id, 1_000_000_000_000, 1_000_000_000_000)
		fixtureKey(t, a.Data, 408, tokenProgram)
		fixtureKey(t, a.Data, 2560, testPubkey(byte(71+i)))
		fixtureKey(t, a.Data, 160, testPubkey(byte(73+i)))
		fixtureKey(t, a.Data, 2600, testPubkey(byte(75+i)))
		binary.LittleEndian.PutUint64(a.Data[2592:2600], 2_000_000_000_000)
		bank.accounts[id.Address] = a
		p, err := decodeRouteReserve(&a, vault)
		if err != nil {
			t.Fatal(err)
		}
		obligation := fixtureAccount(p.Obligation, KLendProgram, 1_000_000, make([]byte, 3344))
		copy(obligation.Data, []byte{168, 206, 141, 106, 88, 76, 172, 167})
		fixtureKey(t, obligation.Data, 32, id.Market)
		fixtureKey(t, obligation.Data, 64, vault)
		if i == 0 {
			fixtureKey(t, obligation.Data, 96, id.Address)
			binary.LittleEndian.PutUint64(obligation.Data[128:136], 1000)
		}
		if _, err := decodeObligation(&obligation, id.Market, vault, "", &p.Position); err != nil {
			t.Fatal(err)
		}
		bank.accounts[p.Obligation] = obligation
		mint := fixtureAccount(id.Mint, tokenProgram, 1_000_000, make([]byte, 82))
		mint.Data[44], mint.Data[45] = 6, 1
		bank.accounts[id.Mint] = mint
		ata := fixtureAccount(p.Position.VaultLiquidityATA, tokenProgram, 1_000_000, make([]byte, 165))
		fixtureKey(t, ata.Data, 0, id.Mint)
		fixtureKey(t, ata.Data, 32, vault)
		ata.Data[108] = 1
		bank.accounts[p.Position.VaultLiquidityATA] = ata
		if i == 0 {
			bank.source = p
			bank.sourceCollateral = 1000
			m.CustodyAccount = p.Position.VaultLiquidityATA
		} else {
			bank.target = p
			bank.active = p
		}
	}
	certificate := CrossMintPreflightCertificate{Kind: "cross_mint_preflight", CertifiedAt: time.Now().Add(-time.Second), Cluster: m.Cluster, SourceMint: m.SourceMint, TargetMint: m.TargetMint, InputAmountRaw: "999", MinimumOutputAmountRaw: "998", EffectiveSlippageBPS: 1, EffectiveMaximumValueLossBPS: 50,
		FinalizedPolicyReadbacks: CrossMintCertificatePolicies{Withdraw: CrossMintCertificatePolicy{PolicyAccount: withdraw, ContextSlot: 999, DataSHA256: strings.Repeat("b", 64)}, Deposit: CrossMintCertificatePolicy{PolicyAccount: deposit, ContextSlot: 999, DataSHA256: strings.Repeat("c", 64)}, Swap: CrossMintCertificateSwapPolicy{CrossMintCertificatePolicy: CrossMintCertificatePolicy{PolicyAccount: swap, ContextSlot: 999, DataSHA256: strings.Repeat("d", 64)}, PolicySeed: "3", SourceShard: "classic", ManifestFingerprint: b.Swap.ManifestFingerprint, Dialect: "route_v2", ConstraintIndex: 0, DailySourceMintSpendingCap: "10000000"}},
		JupiterBuild:             CrossMintCertificateJupiter{ResponseSHA256: strings.Repeat("e", 64), RouteStepCount: 1, QuotedOutputAmountRaw: "999", ComputeUnitLimit: 200_000, PacketSizeBytes: 512, PacketDataSizeBytes: SolanaPacketLimit, FitsPacketDataSize: true, MessageSHA256: strings.Repeat("f", 64), LastValidBlockHeight: 2000, ObservedBlockHeight: 1900, InputPreBalanceRaw: "0", OutputPreBalanceRaw: "0", SimulationAttempted: true, SimulationUnits: 100_000, SimulationTopology: "withdraw_then_swap_atomic_preflight_only", SimulationLookupTables: []string{testPubkey(111)}, TargetDepositPolicyValidated: true, TargetReserve: m.IntendedTargetReserve, TargetObligation: bank.target.Obligation}}
	m.PreflightCertification, _ = json.Marshal(certificate)
	q := CrossMintPreparationRequest{Movement: m, Leg: "withdraw", Purpose: "optimize_yield", Generation: 1, RemainingFeeLamports: 10_000, ContinuationOwner: "worker", ContinuationFencingToken: 1, ControlGeneration: 2, ExpiresAt: time.Now().Add(time.Minute)}
	return q, plan, bank
}

func TestCrossMintPreparationRejectsWrongAuthorityAndCustodyPhase(t *testing.T) {
	q, plan, _ := crossMintPreparationFixture(t)
	if _, err := validateCrossMintPreparationRequest(q, plan.Bindings.DelegatedSigner, "worker", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CrossMintPreparationRequest){
		func(q *CrossMintPreparationRequest) { q.ContinuationOwner = "another-worker" },
		func(q *CrossMintPreparationRequest) { q.ExpiresAt = time.Now().Add(time.Second) },
		func(q *CrossMintPreparationRequest) { q.Leg = "swap" },
		func(q *CrossMintPreparationRequest) { q.Movement.Phase = "target_idle" },
		func(q *CrossMintPreparationRequest) { q.Movement.CustodyVersion = 1 },
		func(q *CrossMintPreparationRequest) {
			q.Movement.ExecutionPlan = json.RawMessage(`{"kind":"same_mint"}`)
		},
		func(q *CrossMintPreparationRequest) { q.Movement.PreflightCertification = json.RawMessage(`{}`) },
	} {
		changed := q
		change(&changed)
		if _, err := validateCrossMintPreparationRequest(changed, plan.Bindings.DelegatedSigner, "worker", time.Now()); err == nil {
			t.Fatal("accepted wrong authority, plan or phase")
		}
	}
}

func TestCrossMintCertificateRejectsStructuralAndAuthoritySubstitutions(t *testing.T) {
	q, _, _ := crossMintPreparationFixture(t)
	base, err := ValidateCrossMintPreflightCertificate(q.Movement, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CrossMintPreflightCertificate){
		func(c *CrossMintPreflightCertificate) { c.Cluster = "another-cluster" },
		func(c *CrossMintPreflightCertificate) { c.TargetMint = USDCMint },
		func(c *CrossMintPreflightCertificate) { c.InputAmountRaw = "1000" },
		func(c *CrossMintPreflightCertificate) { c.MinimumOutputAmountRaw = "1" },
		func(c *CrossMintPreflightCertificate) { c.FinalizedPolicyReadbacks.Withdraw.ContextSlot = 899 },
		func(c *CrossMintPreflightCertificate) { c.FinalizedPolicyReadbacks.Swap.PolicySeed = "4" },
		func(c *CrossMintPreflightCertificate) {
			c.FinalizedPolicyReadbacks.Deposit.PolicyAccount = testPubkey(4)
		},
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.SimulationAttempted = false },
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.SimulationTopology = "withdraw_swap_deposit" },
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.TargetDepositPolicyValidated = false },
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.TargetReserve = testPubkey(4) },
		func(c *CrossMintPreflightCertificate) {
			c.JupiterBuild.SimulationUnits = c.JupiterBuild.ComputeUnitLimit + 1
		},
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.PacketSizeBytes = SolanaPacketLimit + 1 },
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.SimulationLookupTables = nil },
		func(c *CrossMintPreflightCertificate) { c.JupiterBuild.MessageSHA256 = "not-a-message-hash" },
	} {
		c := base
		change(&c)
		m := q.Movement
		m.PreflightCertification, _ = json.Marshal(c)
		if _, err := ValidateCrossMintPreflightCertificate(m, time.Now()); err == nil {
			t.Fatal("accepted certificate without complete immutable verifier gates")
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{"kind":"cross_mint_preflight"}`), json.RawMessage(`{"kind":"cross_mint_preflight","policyReadbackCommitment":"finalized","simulationCommitment":"confirmed"}`), append(bytes.Clone(q.Movement.PreflightCertification), []byte(`{}`)...)} {
		m := q.Movement
		m.PreflightCertification = raw
		if _, err := ValidateCrossMintPreflightCertificate(m, time.Now()); err == nil {
			t.Fatal("accepted generic, older Go or trailing certificate shape")
		}
	}
}

func TestCrossMintPreparationAggregateIncludesUnattributedSurplus(t *testing.T) {
	q, _, bank := crossMintPreparationFixture(t)
	amount, slot := int64(1200), int64(999)
	q.Leg, q.Movement.Phase, q.Movement.CustodyVersion = "swap", "source_idle", 1
	q.Movement.CustodyObservedBalanceRaw, q.Movement.CustodyReconciledSlot = &amount, &slot
	account := bank.accounts[q.Movement.CustodyAccount]
	binary.LittleEndian.PutUint64(account.Data[64:72], 1200)
	bank.accounts[q.Movement.CustodyAccount] = account
	r := &Revalidator{}
	if err := r.checkCrossMintCustody(q, bank); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(account.Data[64:72], 999)
	if err := r.checkCrossMintCustody(q, bank); err == nil {
		t.Fatal("accepted attributed amount in place of anchored aggregate")
	}
	binary.LittleEndian.PutUint64(account.Data[64:72], 1200)
	account.Data[108] = 2
	if err := r.checkCrossMintCustody(q, bank); err == nil {
		t.Fatal("accepted frozen custody account")
	}
}

func TestCrossMintMissingALTResultCannotBeSigned(t *testing.T) {
	q, _, _ := crossMintPreparationFixture(t)
	calls := 0
	r := &Revalidator{signer: testPubkey(93), computeLimit: defaultComputeLimit, rpc: crossMintPrepareRPC(t, func(method string, _ []json.RawMessage) any {
		calls++
		if method != "getLatestBlockhash" {
			t.Fatalf("waiting leg called %s", method)
		}
		return map[string]any{"context": map[string]any{"slot": 1000}, "value": map[string]any{"blockhash": testPubkey(99), "lastValidBlockHeight": 2000}}
	})}
	ix := []RouteInstruction{{Step: "test", Program: KLendProgram, Accounts: []InstructionAccount{{Address: testPubkey(11), Writable: true}}, Data: []byte{1}}}
	p, height, missing, err := r.compileCrossMintIndependentLeg(context.Background(), q, ix, nil, 1000)
	if err != nil || len(missing) != 1 || height != 0 || len(p.Transaction.Message) != 0 || len(p.Transaction.UnsignedWire) != 0 || calls != 1 || !bytes.Equal(p.ExecutionPlan, q.Movement.ExecutionPlan) {
		t.Fatalf("missing ALT metadata became signable: %+v height=%d missing=%v err=%v", p, height, missing, err)
	}
	if p.RouteFingerprint == "" || p.RequirementsFingerprint == "" {
		t.Fatal("waiting ALT omitted exact route identity")
	}
}

func TestCrossMintSourceRecoveryDoesNotReadMissingTargetSetup(t *testing.T) {
	q, plan, bank := crossMintPreparationFixture(t)
	q.Leg, q.Purpose = "deposit", "recover_source"
	q.Movement.Phase, q.Movement.CustodyVersion = "source_idle", 1
	amount, slot := int64(1200), int64(999)
	q.Movement.CustodyObservedBalanceRaw, q.Movement.CustodyReconciledSlot = &amount, &slot
	bank.accounts[plan.Bindings.Withdraw.PolicyAccount] = fixtureAccount(plan.Bindings.Withdraw.PolicyAccount, squads.ProgramID.String(), 1, []byte{1})
	r := &Revalidator{slotDuration: 400 * time.Millisecond, rpc: crossMintPrepareRPC(t, func(method string, params []json.RawMessage) any {
		if method != "getMultipleAccounts" {
			t.Fatalf("unexpected method %s", method)
		}
		var addresses []string
		if err := json.Unmarshal(params[0], &addresses); err != nil {
			t.Fatal(err)
		}
		values := []any{}
		for _, name := range addresses {
			if name == q.Movement.IntendedTargetReserve || name == bank.target.Obligation || name == plan.Bindings.Deposit.PolicyAccount || name == plan.Bindings.Swap.PolicyAccount {
				t.Fatalf("source recovery depends on unavailable target: %s", name)
			}
			a, ok := bank.accounts[name]
			if !ok {
				t.Fatalf("unexpected source recovery dependency %s", name)
			}
			values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
		}
		return map[string]any{"context": map[string]any{"slot": 1000}, "value": values}
	})}
	fresh, err := r.loadCrossMintPreparationBank(context.Background(), q, plan, nil)
	if err != nil || fresh.sourceCollateral != 1000 || fresh.source.Obligation != bank.source.Obligation {
		t.Fatalf("source recovery read failed: %+v err=%v", fresh, err)
	}
}

func TestRealKLendIndependentCrossMintPolicyArms(t *testing.T) {
	q, plan, bank := crossMintPreparationFixture(t)
	r := &Revalidator{signer: plan.Bindings.DelegatedSigner}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	route, err := BuildCrossMintLegs(KaminoSameMintRouteRequest{Vault: q.Movement.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position, WithdrawCollateralAmount: 999, DepositLiquidityAmount: 999})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := BuildIdleDeposit(KaminoIdleDepositRequest{Vault: q.Movement.VaultPubkey, Target: bank.source.Position, DepositLiquidityAmount: 999})
	if err != nil {
		t.Fatal(err)
	}
	makePolicy := func(seed uint64, ixs []RouteInstruction) chain.Account {
		data, err := connectedEarnPolicyData(plan.Bindings.Settings, r.signer, ixs, []KaminoPositionAccounts{bank.source.Position, bank.target.Position})
		if err != nil {
			t.Fatal(err)
		}
		name, bump, err := derivePolicyAccount(plan.Bindings.Settings, seed)
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint64(data[40:48], seed)
		data[48] = bump
		return fixtureAccount(name, squads.ProgramID.String(), 1_000_000, data)
	}
	bank.accounts[plan.Bindings.Withdraw.PolicyAccount] = makePolicy(1, []RouteInstruction{route.Protected[0], recovery.Protected[0]})
	bank.accounts[plan.Bindings.Deposit.PolicyAccount] = makePolicy(2, []RouteInstruction{route.Protected[1]})
	instructions, effect, anchors, policy, err := r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
	if err != nil || policy != plan.Bindings.Withdraw.PolicyAccount || len(instructions) != 4 || effect.Debit != nil || effect.MinimumCredit == nil || *effect.MinimumCredit != 1 || anchors.Position.CollateralRaw != 1000 {
		t.Fatalf("withdraw not independently prepared: instruction count=%d err=%v", len(instructions), err)
	}
	for _, ix := range instructions {
		if ix.Step == "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2" {
			t.Fatal("withdrawal included deposit")
		}
	}
	q.Leg, q.Purpose, q.Movement.Phase, q.Movement.CustodyVersion = "deposit", "recover_source", "source_idle", 1
	amount, slot := int64(1200), int64(999)
	q.Movement.CustodyObservedBalanceRaw, q.Movement.CustodyReconciledSlot = &amount, &slot
	instructions, effect, anchors, policy, err = r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
	if err != nil || policy != plan.Bindings.Withdraw.PolicyAccount || len(instructions) != 3 || effect.Debit.AmountRaw != 999 || anchors.Debit.AmountRaw != 1200 || effect.CreditMint != nil {
		t.Fatalf("source recovery arm failed: %+v err=%v", effect, err)
	}
	// An apparently valid policy with its recovery arm at another index cannot
	// replace immutable constraint index 1.
	bank.accounts[plan.Bindings.Withdraw.PolicyAccount] = makePolicy(1, []RouteInstruction{recovery.Protected[0], route.Protected[0]})
	if _, _, _, _, err := r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank); err == nil {
		t.Fatal("accepted source recovery at substituted constraint index")
	}
	q.Purpose, q.Movement.Phase = "optimize_yield", "target_idle"
	q.Movement.CustodyMint, q.Movement.CustodyAccount = USDTMint, bank.target.Position.VaultLiquidityATA
	_, effect, anchors, policy, err = r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank)
	if err != nil || policy != plan.Bindings.Deposit.PolicyAccount || effect.Debit.Mint != USDTMint || anchors.Position.Reserve != q.Movement.IntendedTargetReserve {
		t.Fatalf("target deposit failed: %+v err=%v", effect, err)
	}
	q.Purpose = "fallback_target"
	bank.active.Position.Reserve = testPubkey(88)
	q.Movement.ActiveTargetReserve = bank.active.Position.Reserve
	if _, _, _, _, err := r.prepareCrossMintKaminoInstructions(ctx, q, plan, bank); err == nil {
		t.Fatal("fallback substituted reserve without exact policy authority")
	}
}

func TestCrossMintTransportFailureDoesNotAuthorizeRecovery(t *testing.T) {
	q, plan, bank := crossMintPreparationFixture(t)
	q.Leg, q.Movement.Phase, q.Movement.CustodyVersion = "swap", "source_idle", 1
	amount, slot := int64(1200), int64(999)
	q.Movement.CustodyObservedBalanceRaw, q.Movement.CustodyReconciledSlot = &amount, &slot
	bank.targetEconomics = ReserveState{SupplyAPYBPS: 400, TotalSupplyUSDMicros: minimumReserveSupplyUSDMicros + 1, EconomicLifetimeMillis: 1000}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(q.Movement.ExecutionPlan, &fields); err != nil {
		t.Fatal(err)
	}
	fields["holding_horizon_seconds"] = json.RawMessage(`31536000`)
	fields["estimated_execution_costs"] = json.RawMessage(`{"kind":"cross_mint_jupiter","jupiter_swap_usd_micros":1,"deposit_usd_micros":1}`)
	q.Movement.ExecutionPlan, _ = json.Marshal(fields)
	r := &Revalidator{crossMintMaxValueLossBPS: 50, crossMintMaxSlippageBPS: 50, jupiter: &JupiterBuildClient{url: "https://mock.invalid", client: &http.Client{Transport: crossMintPrepareTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport stopped") })}}}
	_, _, _, err := r.prepareCrossMintSwapInstructions(context.Background(), q, plan, &bank)
	if err == nil || errors.Is(err, ErrCrossMintQuoteUnavailable) || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("transport failure became alternate spending authority: %v", err)
	}
}
