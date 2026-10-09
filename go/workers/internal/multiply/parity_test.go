package multiply

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

// ---------------------------------------------------------------- fixtures

// fixtureKey derives a deterministic valid point from a single tag byte; the
// tests only need distinct, well-formed keys, not meaningful addresses.
func fixtureKey(tag byte) solana.PublicKey {
	var raw [32]byte
	raw[0], raw[31] = tag, tag
	return solana.PublicKeyFromBytes(raw[:])
}

func testTopology(t *testing.T) *EarnMaxTopology {
	t.Helper()
	topology, err := DeriveEarnMaxTopology(fixtureKey(1), 320)
	if err != nil {
		t.Fatalf("derive topology: %v", err)
	}
	return topology
}

func idleObserved(topology *EarnMaxTopology) *ObservedRoute {
	observed := &ObservedRoute{Slot: 331895401}
	observed.Claim = TokenBalance{
		Account: topology.ClaimCustody.String(), Mint: USDCMint,
		TokenProgram: TokenProgram, AmountRaw: 0,
	}
	for _, config := range topology.StrategyCatalog() {
		observed.Strategies = append(observed.Strategies, &StrategyObservation{
			StrategyKey: config.Key, DebtAmountSF: "0",
			CollateralValueSF: new(big.Int), DebtValueSF: new(big.Int),
			DebtMarketPriceSF: new(big.Int).SetUint64(fractionOneSF),
			UnhealthyValueSF:  new(big.Int), DebtMintFactor: 1_000_000,
		})
		observed.CollateralCustodies = append(observed.CollateralCustodies, CustodyBalance{
			StrategyKey: config.Key,
			Balance: TokenBalance{
				Account: config.CollateralCustody.String(), Mint: config.CollateralMint,
				TokenProgram: TokenProgram,
			},
		})
		observed.DebtCustodies = append(observed.DebtCustodies, CustodyBalance{
			StrategyKey: config.Key,
			Balance: TokenBalance{
				Account: config.DebtCustody.String(), Mint: config.DebtMint,
				TokenProgram: config.DebtTokenProgram.String(),
			},
		})
	}
	return observed
}

func activeObserved(topology *EarnMaxTopology, key StrategyKey, collateral, debt uint64,
	collateralValueSF, debtValueSF, priceSF uint64) (*ObservedRoute, *StrategyObservation) {
	observed := idleObserved(topology)
	position := &StrategyObservation{
		StrategyKey: key, CollateralDepositedRaw: collateral, DebtRaw: debt,
		CollateralValueSF: new(big.Int).SetUint64(collateralValueSF), DebtValueSF: new(big.Int).SetUint64(debtValueSF),
		DebtMarketPriceSF: new(big.Int).SetUint64(priceSF), DebtMintFactor: 1_000_000,
		UnhealthyValueSF: new(big.Int).Mul(new(big.Int).SetUint64(debtValueSF), big.NewInt(2)),
	}
	setObservedPosition(observed, position)
	observed.CollateralCustodies[0].Balance.AmountRaw = 0
	for index := range observed.CollateralCustodies {
		if observed.CollateralCustodies[index].StrategyKey == key {
			observed.CollateralCustodies[index].Balance.AmountRaw = collateral
		}
	}
	for index := range observed.DebtCustodies {
		if observed.DebtCustodies[index].StrategyKey == key {
			observed.DebtCustodies[index].Balance.AmountRaw = debt
		}
	}
	return observed, position
}

func setObservedPosition(observed *ObservedRoute, position *StrategyObservation) {
	for index, existing := range observed.Strategies {
		if existing.StrategyKey == position.StrategyKey {
			observed.Strategies[index] = position
			return
		}
	}
	observed.Strategies = append(observed.Strategies, position)
}

func testRouteState(t *testing.T, topology *EarnMaxTopology) *RouteState {
	t.Helper()
	state, err := NewRouteState(routeKeyFor(topology.Settings, topology.VaultIndex),
		topology.Settings.String(), topology.VaultIndex, topology.Vault.String(), 320,
		TokenBalance{
			Account: topology.ClaimCustody.String(), Mint: USDCMint,
			TokenProgram: TokenProgram, AmountRaw: 5_000_000,
		}, 331895401, time.Now().UTC())
	if err != nil {
		t.Fatalf("new route state: %v", err)
	}
	return state
}

// ------------------------------------------------------------ types parity

func TestRouteStateFixtureRoundTripDeniesUnknownFields(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/multiply/route_state_schema9.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var state RouteState
	if err := jsonUnmarshalStrict(raw, &state); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if state.SchemaVersion != 9 || state.EngineVersion != EngineVersion ||
		state.Position.Kind != "active" || state.Position.StrategyKey == nil ||
		*state.Position.StrategyKey != OnycUsdc {
		t.Fatalf("decoded fixture does not match the schema-9 contract: %+v", state)
	}
	// serde deny_unknown_fields: one extra field must be refused.
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	generic["surpriseField"] = json.RawMessage(`1`)
	tampered, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshalStrict(tampered, &RouteState{}); err == nil {
		t.Fatal("unknown field was silently accepted")
	}
	encoded, err := jsonMarshal(&state)
	if err != nil {
		t.Fatal(err)
	}
	var reparsed RouteState
	if err := jsonUnmarshalStrict(encoded, &reparsed); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if reparsed.RouteKey != state.RouteKey || reparsed.Generation != state.Generation {
		t.Fatal("round trip lost identity")
	}
}

func TestExpectedEffectsAlwaysSerializesTokenAmountsBefore(t *testing.T) {
	// Rust always serializes the empty vec as [] (serde default is
	// decode-only); an omitted field would break persisted-JSON compatibility.
	effects := ExpectedEffects{TokenDeltas: []TokenDelta{{
		Account: "a", Mint: "m", RawDelta: 5,
	}}}
	encoded, err := jsonMarshal(effects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"tokenAmountsBefore":[]`) {
		t.Fatalf("tokenAmountsBefore was omitted: %s", encoded)
	}
}

// --------------------------------------------------------------- topology

func TestTopologyDeterminismAndIdentity(t *testing.T) {
	topology := testTopology(t)
	again, err := DeriveEarnMaxTopology(topology.Settings, 320)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Vault != again.Vault || topology.ClaimCustody != again.ClaimCustody {
		t.Fatal("topology derivation is not deterministic")
	}
	for key, config := range topology.Strategies {
		other, err := topology.Strategy(key)
		if err != nil {
			t.Fatalf("strategy %s: %v", key, err)
		}
		if other.Obligation != config.Obligation {
			t.Fatalf("obligation drift for %s", key)
		}
		if solana.IsOnCurve(config.Obligation.Bytes()) || solana.IsOnCurve(config.CollateralCustody.Bytes()) {
			t.Fatalf("strategy %s custody/obligation is on-curve (not a PDA)", key)
		}
	}
	if _, err := topology.Strategy("not_a_strategy"); err == nil {
		t.Fatal("unknown strategy was accepted")
	}
}

// ---------------------------------------------------------------- planner

func TestPlannerDeploysThenLoops(t *testing.T) {
	topology := testTopology(t)
	state := testRouteState(t, topology)
	state.Goal = GoalDeploy
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 4_000_000
	decision := NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan == nil ||
		decision.Plan.Action != ActionSwapClaimToCollateral ||
		decision.Plan.Amount.Exactly != 4_000_000 {
		t.Fatalf("expected claim swap, got %+v", decision)
	}
	// After the swap, the collateral custody holds funds -> deposit.
	observed.Claim.AmountRaw = 0
	syrupCustody(observed, SyrupUsdcUsdc).Balance.AmountRaw = 3_900_000
	decision = NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionDepositCollateral {
		t.Fatalf("expected deposit, got %+v", decision)
	}
	// With collateral deposited and debt far below target -> borrow to LTV.
	// Value scale: 3.9 tokens valued at 3.9e12 SF-$; the reserve prices one
	// raw token at 1e12 SF-$ so the target value resolves back to raw units.
	syrupCustody(observed, SyrupUsdcUsdc).Balance.AmountRaw = 0
	config := topology.Strategies[SyrupUsdcUsdc]
	targetValue := uint64(config.TargetLTVBPS) * 3_900_000_000_000 / 10_000
	position := &StrategyObservation{
		StrategyKey: SyrupUsdcUsdc, CollateralDepositedRaw: 3_900_000,
		CollateralValueSF: big.NewInt(3_900_000_000_000), DebtValueSF: new(big.Int),
		DebtMarketPriceSF: big.NewInt(1_000_000_000_000), DebtMintFactor: 1_000_000,
		UnhealthyValueSF: big.NewInt(4_000_000_000_000),
	}
	setObservedPosition(observed, position)
	decision = NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionBorrowDebt ||
		decision.Plan.Amount.Mode != "to_target_ltv" {
		t.Fatalf("expected borrow to target LTV, got %+v", decision)
	}
	amount, err := resolveBorrow(decision.Plan.Amount, observed, config)
	if err != nil {
		t.Fatal(err)
	}
	// target-value raw units at the modeled reserve price.
	if want := targetValue / 1_000_000; amount != want {
		t.Fatalf("borrow amount %d, want %d (targetLTV %d bps)", amount, want, config.TargetLTVBPS)
	}
	// Within tolerance of target LTV -> complete.
	position.DebtValueSF = new(big.Int).SetUint64(targetValue)
	position.DebtRaw = targetValue / 1_000_000
	decision = NextAction(state, observed, topology)
	if decision.Kind != "complete" {
		t.Fatalf("expected completion inside tolerance, got %+v", decision)
	}
}

func syrupCustody(observed *ObservedRoute, key StrategyKey) *CustodyBalance {
	for index := range observed.CollateralCustodies {
		if observed.CollateralCustodies[index].StrategyKey == key {
			return &observed.CollateralCustodies[index]
		}
	}
	return nil
}

func syrupDebt(observed *ObservedRoute, key StrategyKey) *CustodyBalance {
	for index := range observed.DebtCustodies {
		if observed.DebtCustodies[index].StrategyKey == key {
			return &observed.DebtCustodies[index]
		}
	}
	return nil
}

func TestPlannerWithdrawsThenClaims(t *testing.T) {
	topology := testTopology(t)
	state := testRouteState(t, topology)
	state.Goal = GoalWithdraw
	observed, _ := activeObserved(topology, SyrupUsdcUsdc, 3_000_000, 1_000_000,
		3_000_000_000_000, 1_000_000_000_000, 1_000_000_000_000)
	decision := NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionRepayDebt {
		t.Fatalf("expected repay first, got %+v", decision)
	}
	if decision.Plan.Amount.Mode != "all" {
		t.Fatalf("repay of full custody must be repay-all, got %+v", decision.Plan.Amount)
	}
	observed, _ = activeObserved(topology, SyrupUsdcUsdc, 3_000_000, 1_000_000,
		3_000_000_000_000, 1_000_000_000_000, 1_000_000_000_000)
	syrupDebt(observed, SyrupUsdcUsdc).Balance.AmountRaw = 0
	syrupCustody(observed, SyrupUsdcUsdc).Balance.AmountRaw = 0
	decision = NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionWithdrawCollateral ||
		decision.Plan.Amount.Mode != "max_safe" {
		t.Fatalf("expected max-safe withdraw, got %+v", decision)
	}
	// No active position but collateral custody holds residue -> swap to claim.
	// A complete confirmed snapshot proves the obligation is now empty.
	// Omitting strategy evidence would be unknown, not a closed position.
	for _, position := range observed.Strategies {
		position.CollateralDepositedRaw = 0
		position.DebtRaw = 0
	}
	syrupCustody(observed, SyrupUsdcUsdc).Balance.AmountRaw = 500_000
	decision = NextAction(state, observed, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionSwapCollateralToClaim {
		t.Fatalf("expected close-to-claim swap, got %+v", decision)
	}
}

// ----------------------------------------------------------------- policy

func TestConstraintIndexesPerActionAndStrategy(t *testing.T) {
	topology := testTopology(t)
	cases := []struct {
		key    StrategyKey
		action MultiplyAction
		index  uint8
	}{
		{OnycUsdc, ActionDepositCollateral, 0},
		{OnycUsdc, ActionWithdrawCollateral, 1},
		{PrimePyusd, ActionBorrowDebt, 0},
		{SyrupUsdcUsdc, ActionRepayDebt, 1},
		{OnycUsdc, ActionSwapClaimToCollateral, 0},
		{SyrupUsdcPyusd, ActionSwapClaimToCollateral, 1},
		{PrimePyusd, ActionSwapDebtToCollateral, 1},
		{OnycUsdc, ActionSwapCollateralToDebt, 2},
		{SyrupUsdcUsdc, ActionSwapCollateralToDebt, 3},
		{OnycUsdc, ActionSwapCollateralToClaim, 2},
		{SyrupUsdcUsdc, ActionSwapCollateralToClaim, 3},
	}
	for _, testCase := range cases {
		config, err := topology.Strategy(testCase.key)
		if err != nil {
			t.Fatal(err)
		}
		route := Instruction{
			ProgramID: mustKey(JupiterProgram),
			Data:      append([]byte(nil), JupiterSharedAccountsRouteDiscriminator[:]...),
		}
		route.Data = append(route.Data, 0, 2, 0, 0, 0) // routing enum + 2 legs
		route.Data = append(route.Data, make([]byte, 24)...)
		indexes, err := ConstraintIndexes(config, testCase.action, []Instruction{route})
		if err != nil {
			t.Fatalf("%s/%s: %v", testCase.key, testCase.action, err)
		}
		if len(indexes) != 1 || indexes[0] != testCase.index {
			t.Fatalf("%s/%s: index %v, want [%d]", testCase.key, testCase.action, indexes, testCase.index)
		}
	}
	if _, err := FamilyForAction(ActionClaim); err == nil {
		t.Fatal("user-side actions must have no strategy family")
	}
}

func TestSquadsExecutePayloadLayout(t *testing.T) {
	topology := testTopology(t)
	config := topology.Strategies[OnycUsdc]
	terminal := Instruction{
		ProgramID: mustKey(KlendProgram),
		Data:      appendU64Instruction(DiscriminatorDepositCollateral[:], 42),
	}
	transactionAccounts := make([]AccountMeta, 0, 8)
	inner := CompileSquadsInnerInstruction(&transactionAccounts, terminal)
	if len(transactionAccounts) != 1 { // account then program
		t.Fatalf("index space built %d entries", len(transactionAccounts))
	}
	constraintIndexes := []byte{0}
	policy, _ := config.PolicyForAction(ActionDepositCollateral)
	execute := ExecuteProgramInteractionInstruction(policy.Account,
		fixtureKey(1), 0,
		[]CompiledInstruction{inner}, constraintIndexes, transactionAccounts)
	if execute.ProgramID != mustKey(SquadsProgram) {
		t.Fatal("terminal program drifted")
	}
	if !equalBytes(execute.Data[:8], []byte{90, 81, 187, 81, 39, 70, 128, 78}) {
		t.Fatal("sync v2 discriminator drifted")
	}
	// num_signers = 1, payload = Policy(1) = ProgramInteraction(1), Some(1).
	if execute.Data[8] != 0 || execute.Data[9] != 1 || execute.Data[10] != 1 || execute.Data[11] != 1 || execute.Data[12] != 1 {
		t.Fatalf("squads payload enum tags drifted: %x", execute.Data[8:16])
	}
	if !equalBytes(execute.Data[17:18], constraintIndexes) {
		t.Fatal("constraint indexes misplaced")
	}
}

// ---------------------------------------------------------------- builder

func TestBuildKlendOperationsWireShape(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 0
	syrupCustody(observed, SyrupUsdcUsdc).Balance.AmountRaw = 1_500_000
	plan := &ActionPlan{
		Action: ActionDepositCollateral, StrategyKey: SyrupUsdcUsdc,
		Amount: AmountExact(1_500_000),
	}
	built, err := BuildOperation(plan, observed, topology, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(built.PreInstructions) != 3 { // two reserve refreshes + obligation refresh
		t.Fatalf("deposit refresh chain has %d instructions", len(built.PreInstructions))
	}
	if len(built.PolicyInstructions) != 1 ||
		!equalBytes(built.PolicyInstructions[0].Data[:8], DiscriminatorDepositCollateral[:]) {
		t.Fatal("deposit terminal drifted")
	}
	var amountRaw uint64
	buf := binary.LittleEndian
	amountRaw = buf.Uint64(built.PolicyInstructions[0].Data[8:16])
	if amountRaw != 1_500_000 {
		t.Fatalf("deposit amount %d drifted", amountRaw)
	}
	if len(built.ExpectedEffects.TokenDeltas) != 1 ||
		built.ExpectedEffects.TokenDeltas[0].RawDelta != -1_500_000 ||
		built.ExpectedEffects.ObligationDelta == nil ||
		built.ExpectedEffects.ObligationDelta.CollateralRawDelta != 1_500_000 {
		t.Fatalf("deposit effects drifted: %+v", built.ExpectedEffects)
	}
	// Withdraw max-safe carries u64::MAX on the wire and expects 1.
	withdrawPlan := &ActionPlan{
		Action: ActionWithdrawCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountMaxSafe,
	}
	built, err = BuildOperation(withdrawPlan, observed, topology, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(built.PolicyInstructions[0].Data[8:16]); got != ^uint64(0) {
		t.Fatalf("max-safe wire amount %x", got)
	}
	// Repay-all carries u64::MAX and expects the observed debt.
	observed, _ = activeObserved(topology, SyrupUsdcUsdc, 0, 250_000, 0, 250_000_000, 1_000_000)
	repayPlan := &ActionPlan{Action: ActionRepayDebt, StrategyKey: SyrupUsdcUsdc, Amount: AmountAll}
	built, err = BuildOperation(repayPlan, observed, topology, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(built.PolicyInstructions[0].Data[8:16]); got != ^uint64(0) {
		t.Fatalf("repay-all wire amount %x", got)
	}
	if built.ExpectedEffects.TokenDeltas[0].RawDelta != -250_000 ||
		built.ExpectedEffects.ObligationDelta.DebtRawDelta != -250_000 {
		t.Fatalf("repay-all expected effects drifted: %+v", built.ExpectedEffects)
	}
}

// fakeQuoteClient is a fixture Jupiter surface; the route instruction it
// returns is a real SharedAccountsRoute wire, hand-assembled to the layout
// documented in crates/loyal-actions/src/earn_max.rs. Source and destination
// bind the actual topology custodies the builder hands the request.
type fakeQuoteClient struct{ topology *EarnMaxTopology }

func (f fakeQuoteClient) FetchQuote(ctx contextT, request QuoteRequest) (*QuoteResponse, error) {
	if request.InputMint == request.OutputMint {
		return nil, errors.New("fixture only supports real swaps")
	}
	output := request.Amount * 99 / 100
	minimum := (output*9_950 + 9_999) / 10_000
	return &QuoteResponse{
		InputMint: request.InputMint, OutputMint: request.OutputMint, SwapMode: "ExactIn",
		InAmount:             strconv.FormatUint(request.Amount, 10),
		OutAmount:            strconv.FormatUint(output, 10),
		OtherAmountThreshold: strconv.FormatUint(minimum, 10),
		SlippageBPS:          50,
		ContextSlot:          331895400,
		RoutePlan:            []json.RawMessage{json.RawMessage(`{"swap":{"a":1}}`)},
	}, nil
}

func (f fakeQuoteClient) FetchSwapInstructions(ctx contextT, quote *QuoteResponse, vault solana.PublicKey) (*SwapInstructionsResponse, error) {
	config := f.topology.Strategies[SyrupUsdcUsdc]
	source, destination := f.topology.ClaimCustody, config.CollateralCustody
	data := append([]byte(nil), JupiterSharedAccountsRouteDiscriminator[:]...)
	data = append(data, 0, 1, 0, 0, 0) // routing enum + one leg
	data = append(data, make([]byte, 11)...)
	input, _ := strconv.ParseUint(quote.InAmount, 10, 64)
	output, _ := strconv.ParseUint(quote.OutAmount, 10, 64)
	var tail [19]byte
	binary.LittleEndian.PutUint64(tail[0:8], input)
	binary.LittleEndian.PutUint64(tail[8:16], output)
	binary.LittleEndian.PutUint16(tail[16:18], 50)
	data = append(data, tail[:]...)
	return &SwapInstructionsResponse{
		SwapInstruction: &RawInstruction{
			ProgramID: JupiterProgram,
			Accounts: []RawAccountMeta{
				{PubKey: fixtureKey(7).String()},
				{PubKey: fixtureKey(8).String()},
				{PubKey: vault.String(), IsSigner: true},
				{PubKey: source.String(), IsWritable: true},
				{PubKey: fixtureKey(10).String()},
				{PubKey: fixtureKey(11).String()},
				{PubKey: destination.String(), IsWritable: true},
				{PubKey: USDCMint},
				{PubKey: quote.OutputMint},
			},
			Data: base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

func TestBuildSwapBindsCustodyAndChecksRoute(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 1_000_000
	plan := &ActionPlan{
		Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc,
		Amount: AmountExact(1_000_000),
	}
	built, err := BuildOperation(plan, observed, topology, fakeQuoteClient{topology}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(built.PolicyInstructions) != 1 || built.PolicyInstructions[0].ProgramID != mustKey(JupiterProgram) {
		t.Fatal("swap terminal drifted")
	}
	if built.ExpectedEffects.TokenDeltas[0].RawDelta != -1_000_000 ||
		built.ExpectedEffects.TokenDeltas[1].RawDelta != 985_050 {
		t.Fatalf("swap effects drifted: %+v", built.ExpectedEffects.TokenDeltas)
	}
	config := topology.Strategies[SyrupUsdcUsdc]
	if err := validateEarnMaxJupiterRoute(built.PolicyInstructions[0],
		topology.Vault, topology.ClaimCustody, config.CollateralCustody,
		mustKey(USDCMint), mustKey(config.CollateralMint), 1_000_000, 990_000, 985_050); err != nil {
		t.Fatalf("legitimate route refused: %v", err)
	}
	// A tampered wire (not SharedAccountsRoute) must be refused.
	tampered := built.PolicyInstructions[0]
	tampered.Data = append([]byte{0, 0, 0, 0, 0, 0, 0, 0}, tampered.Data[8:]...)
	if err := validateEarnMaxJupiterRoute(tampered,
		topology.Vault, topology.ClaimCustody, config.CollateralCustody,
		mustKey(USDCMint), mustKey(config.CollateralMint), 1_000_000, 990_000, 985_050); err == nil {
		t.Fatal("non-route swap wire was accepted")
	}
}

// --------------------------------------------------------------- executor

type fakeRPC struct {
	genesis  string
	hash     BlockhashAndHeight
	sent     [][]byte
	statuses []*SignatureObservation
	height   uint64
}

func (f *fakeRPC) GenesisHash(ctx context.Context) (string, error) { return f.genesis, nil }

func (f *fakeRPC) LatestBlockhash(ctx context.Context) (*BlockhashAndHeight, error) {
	return &f.hash, nil
}

func (f *fakeRPC) AccountAtConfirmed(ctx context.Context, key solana.PublicKey) ([]byte, uint64, error) {
	return nil, f.hash.ContextSlot, nil
}

func (f *fakeRPC) SimulateTransaction(ctx context.Context, wire []byte, minContextSlot uint64) (*SimulationOutcome, error) {
	return &SimulationOutcome{Logs: []string{"ok"}}, nil
}

func (f *fakeRPC) SendRawTransaction(ctx context.Context, wire []byte) (string, error) {
	f.sent = append(f.sent, wire)
	digest := sha256.Sum256(wire)
	signature := make([]byte, 64)
	signature[0] = digest[0]
	return solana.Signature(signature).String(), nil
}

func (f *fakeRPC) SignatureStatus(ctx context.Context, signature string) (*SignatureObservation, error) {
	if len(f.statuses) == 0 {
		return nil, nil
	}
	next := f.statuses[0]
	f.statuses = f.statuses[1:]
	return next, nil
}

func (f *fakeRPC) BlockHeight(ctx context.Context) (uint64, error) { return f.height, nil }

func testDelegateSeed() ed25519.PrivateKey {
	var seed [ed25519.SeedSize]byte
	copy(seed[:], []byte("multiply-test-delegate-key"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func testExecutor(t *testing.T) (*Executor, *fakeRPC, ed25519.PublicKey) {
	t.Helper()
	key := testDelegateSeed()
	rpc := &fakeRPC{
		genesis: mainnetGenesisHash,
		hash: BlockhashAndHeight{
			RecentBlockhash:      fixtureKey(9).String(),
			LastValidBlockHeight: 331_900_000, ContextSlot: 331_895_401,
		},
	}
	executor, err := NewExecutor(rpc, key)
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	return executor, rpc, key.Public().(ed25519.PublicKey)
}

func TestExecutorRejectsNonMainnet(t *testing.T) {
	_, rpc, _ := testExecutor(t)
	rpc.genesis = "EtWTRBDZvAw6t7WorldPlaceholder"
	if _, err := NewExecutor(rpc, testDelegateSeed()); err == nil {
		t.Fatal("non-mainnet genesis was accepted")
	}
}

func TestPrepareAndSignWireShapeAndCapability(t *testing.T) {
	executor, rpc, delegate := testExecutor(t)
	topology := testTopology(t)
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 1_500_000
	plan := &ActionPlan{
		Action: ActionSwapClaimToCollateral, StrategyKey: SyrupUsdcUsdc,
		Amount: AmountExact(1_500_000),
	}
	built, err := BuildOperation(plan, observed, topology, fakeQuoteClient{topology}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fake RPC returns no policy account, so the exact-policy check must
	// refuse to proceed; this is the fail-closed path under test.
	if _, policyErr := executor.EnsureExactPolicy(context.Background(), topology, plan, built); policyErr == nil ||
		!strings.Contains(policyErr.Error(), "policy") {
		t.Fatalf("policy check on an absent account must fail, got %v", policyErr)
	}
	policy := config_policy(t, topology, plan)
	constraintIndexes := []byte{0}
	signed, slot, err := executor.PrepareAndSign(context.Background(), built,
		policy.Account, 0, constraintIndexes, 331_895_400)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 331_895_401 {
		t.Fatalf("min context slot %d", slot)
	}
	if len(signed.Wire) > solanaPacketBytes {
		t.Fatalf("packet is %d bytes", len(signed.Wire))
	}
	count, consumed := decodeShortVec(signed.Wire)
	if count != 1 {
		t.Fatalf("wire carries %d signatures", count)
	}
	signature := signed.Wire[consumed : consumed+ed25519.SignatureSize]
	if !ed25519.Verify(delegate, signed.Wire[consumed+ed25519.SignatureSize:], signature) {
		t.Fatal("wire signature does not verify against the delegate")
	}
	message := signed.Wire[consumed+ed25519.SignatureSize:]
	foundSquads := false
	for offset := 0; offset+8 <= len(message); offset++ {
		if equalBytes(message[offset:offset+8], []byte{90, 81, 187, 81, 39, 70, 128, 78}) {
			foundSquads = true
			break
		}
	}
	if !foundSquads {
		t.Fatal("Squads sync v2 discriminator missing from the wire")
	}
	if len(rpc.sent) != 0 {
		t.Fatal("PrepareAndSign must never send")
	}
}

func config_policy(t *testing.T, topology *EarnMaxTopology, plan *ActionPlan) PolicyConfig {
	t.Helper()
	config, err := topology.Strategy(plan.StrategyKey)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := config.PolicyForAction(plan.Action)
	if !ok {
		t.Fatal("plan action has no policy")
	}
	return policy
}

func hasPrefix(message []byte, _ struct{}) bool { return false }

var instructionProgramAtEnd struct{}

func TestPersistedTransactionRefusesDrift(t *testing.T) {
	signed := signedWireFixture(t, false)
	messageHash, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	operation := &MultiplyOperation{SignedWire: signed.Wire, SignedWireSHA256: &signed.WireSHA256, TransactionSignature: &signed.TransactionSignature, RecentBlockhash: &signed.RecentBlockhash, MessageSHA256: &messageHash}
	if _, err := PersistedTransaction(operation); err != nil {
		t.Fatalf("identity check refused a faithful wire: %v", err)
	}
	wrongHash := strings.Repeat("ab", 32)
	operation.SignedWireSHA256 = &wrongHash
	if _, err := PersistedTransaction(operation); err == nil {
		t.Fatal("drifted wire hash accepted")
	}
}

func TestVerifyExpectedEffectsRules(t *testing.T) {
	topology := testTopology(t)
	key := SyrupUsdcUsdc
	config := topology.Strategies[key]
	before, _ := activeObserved(topology, key, 0, 0, 1, 1, 1)
	after, afterPosition := activeObserved(topology, key, 0, 0, 1, 1, 1)
	setCollateralAmount := func(observed *ObservedRoute, raw uint64) {
		for index := range observed.CollateralCustodies {
			if observed.CollateralCustodies[index].Balance.Account == config.CollateralCustody.String() {
				observed.CollateralCustodies[index].Balance.AmountRaw = raw
			}
		}
	}
	effects := &ExpectedEffects{TokenDeltas: []TokenDelta{{
		Account: config.CollateralCustody.String(), Mint: config.CollateralMint, RawDelta: -500,
	}}, TokenAmountsBefore: []TokenAmountBefore{{
		Account: config.CollateralCustody.String(), Mint: config.CollateralMint, AmountRaw: 5_000,
	}}}
	// Undershoot below the persisted spend must fail.
	setCollateralAmount(before, 5_000)
	setCollateralAmount(after, 4_900)
	if err := VerifyExpectedEffects(effects, ActionDepositCollateral, before, after, topology); err == nil {
		t.Fatal("undershoot was accepted")
	}
	// Faithful deposit: custody lost exactly the spend and the obligation
	// position gained the collateral.
	setCollateralAmount(after, 4_500)
	effects.ObligationDelta = &ObligationDelta{
		Obligation: config.Obligation.String(), CollateralRawDelta: 500,
	}
	effects.ObligationBefore = &ObligationBefore{Obligation: config.Obligation.String(), DebtAmountSF: "0"}
	if err := VerifyExpectedEffects(effects, ActionDepositCollateral, before, after, topology); err == nil {
		t.Fatal("positive obligation delta without position collateral must fail")
	}
	afterPosition.CollateralDepositedRaw = 500
	if err := VerifyExpectedEffects(effects, ActionDepositCollateral, before, after, topology); err != nil {
		t.Fatalf("faithful deposit rejected: %v", err)
	}
	// Repay-all must clear obligation debt; unspent custody residue is valid.
	// A pyusd debt custody is used because the usdc debt custody is the same
	// vault ATA as the claim custody by derivation.
	debtKey := SyrupUsdcPyusd
	debtConfig := topology.Strategies[debtKey]
	effects = &ExpectedEffects{TokenDeltas: []TokenDelta{{
		Account: debtConfig.DebtCustody.String(), Mint: debtConfig.DebtMint, RawDelta: -1_000,
	}}, TokenAmountsBefore: []TokenAmountBefore{{
		Account: debtConfig.DebtCustody.String(), Mint: debtConfig.DebtMint, AmountRaw: 1_000,
	}}, ObligationDelta: &ObligationDelta{Obligation: debtConfig.Obligation.String(), DebtRawDelta: -1_000},
		ObligationBefore: &ObligationBefore{Obligation: debtConfig.Obligation.String(), DebtRaw: 900, DebtAmountSF: "900"}}
	// The same vault ATA can serve several strategies with one debt mint, so
	// set every matching entry the way a real observation would.
	setObservedRaw := func(observed *ObservedRoute, account string, raw uint64) {
		for index := range observed.DebtCustodies {
			if observed.DebtCustodies[index].Balance.Account == account {
				observed.DebtCustodies[index].Balance.AmountRaw = raw
			}
		}
		for index := range observed.CollateralCustodies {
			if observed.CollateralCustodies[index].Balance.Account == account {
				observed.CollateralCustodies[index].Balance.AmountRaw = raw
			}
		}
	}
	setObservedRaw(before, debtConfig.DebtCustody.String(), 1_000)
	setObservedRaw(after, debtConfig.DebtCustody.String(), 100)
	after.Position(debtKey).DebtRaw = 1
	if err := VerifyExpectedEffects(effects, ActionRepayDebt, before, after, topology); err == nil {
		t.Fatal("repay-all retained obligation debt")
	}
	after.Position(debtKey).DebtRaw = 0
	if err := VerifyExpectedEffects(effects, ActionRepayDebt, before, after, topology); err != nil {
		t.Fatalf("repay-all with unspent custody residue rejected: %v", err)
	}
}

// ----------------------------------------------------------------- worker

func TestOperationIDIsStable(t *testing.T) {
	// sha256(EngineVersion || routeKey || cycle_le || generation_le || action)
	hash := sha256.New()
	hash.Write([]byte(EngineVersion))
	hash.Write([]byte("earn-max:x:1"))
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], 3)
	hash.Write(raw[:])
	binary.LittleEndian.PutUint64(raw[:], 9)
	hash.Write(raw[:])
	hash.Write([]byte("deposit_collateral"))
	want := "mul-" + hex.EncodeToString(hash.Sum(nil))[:32]
	if got := OperationID("earn-max:x:1", 3, 9, "deposit_collateral"); got != want {
		t.Fatalf("operation id %s, want %s", got, want)
	}
}

func TestSnapshotInputUnits(t *testing.T) {
	topology := testTopology(t)
	route := testRouteState(t, topology)
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 0
	// 1 << 60 SF = exactly 1.0 -> 1_000_000 micros.
	position := &StrategyObservation{
		StrategyKey: SyrupUsdcUsdc, CollateralDepositedRaw: 2_000_000, DebtRaw: 1_000_000,
		CollateralValueSF: big.NewInt(1 << 60), DebtValueSF: big.NewInt(1 << 60 >> 2),
		DebtMarketPriceSF:      big.NewInt(1 << 60),
		CollateralSupplyAPYBPS: 500, DebtBorrowAPYBPS: 1_000,
		UnhealthyValueSF: big.NewInt(1 << 60),
	}
	setObservedPosition(observed, position)
	input, err := snapshotInput(route, observed)
	if err != nil {
		t.Fatal(err)
	}
	if input.CollateralValueUSD != "1000000" {
		t.Fatalf("collateral value %s micros, want 1000000", input.CollateralValueUSD)
	}
	if input.DebtValueUSD != "250000" {
		t.Fatalf("debt value %s micros, want 250000", input.DebtValueUSD)
	}
	if input.EquityUSD != "750000" { // 1_000_000 - 250_000
		t.Fatalf("equity %s micros", input.EquityUSD)
	}
	if input.LeverageBPS == nil || *input.LeverageBPS != 13333 { // 1_000_000*10000/750_000
		t.Fatalf("leverage bps %v", input.LeverageBPS)
	}
	if input.LTVBPS == nil || *input.LTVBPS != 2500 {
		t.Fatalf("ltv bps %v", input.LTVBPS)
	}
	if input.HealthFactorPPM == nil || *input.HealthFactorPPM != 4_000_000 {
		t.Fatalf("health factor ppm %v", input.HealthFactorPPM)
	}
	if input.ForecastAPYBPS == nil || *input.ForecastAPYBPS != "333" {
		// (1_000_000*500 - 250_000*1000)/750_000 = 333
		t.Fatalf("forecast %v", input.ForecastAPYBPS)
	}
}

func TestBindBeforeRequiresObservedCustody(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	effects := &ExpectedEffects{TokenDeltas: []TokenDelta{{
		Account: topology.ClaimCustody.String(), Mint: USDCMint, RawDelta: -10,
	}}}
	if err := bindBefore(effects, observed, "", topology); err != nil {
		t.Fatal(err)
	}
	if len(effects.TokenAmountsBefore) != 1 || effects.TokenAmountsBefore[0].AmountRaw != observed.Claim.AmountRaw {
		t.Fatal("before-binding lost the confirmed balance")
	}
	effects = &ExpectedEffects{TokenDeltas: []TokenDelta{{
		Account: fixtureKey(12).String(), Mint: USDCMint, RawDelta: -10,
	}}}
	if err := bindBefore(effects, observed, "", topology); err == nil {
		t.Fatal("unobserved custody was bound")
	}
}
