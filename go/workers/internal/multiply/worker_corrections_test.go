package multiply

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"math"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func reviewSF(value uint64) *big.Int {
	return new(big.Int).Lsh(new(big.Int).SetUint64(value), 60)
}

func putReviewSF(destination []byte, value *big.Int) {
	for index := range destination {
		destination[index] = 0
	}
	bytes := value.Bytes()
	for index := range bytes {
		destination[index] = bytes[len(bytes)-1-index]
	}
}

// These cases assert the reference's debt-custody branch, using PYUSD so
// debt custody and claim custody remain distinct. The deployed strategy
// selection and USDC custody alias order are unchanged.
func TestPlannerDebtCustodyThreshold(t *testing.T) {
	topology := testTopology(t)
	for _, test := range []struct {
		name   string
		amount uint64
		debt   uint64
		action MultiplyAction
		mode   string
	}{
		{"dust_partial", 1, 100_000, ActionRepayDebt, "exact"},
		{"boundary_partial", 50_000, 100_000, ActionRepayDebt, "exact"},
		{"boundary_all", 50_000, 50_000, ActionRepayDebt, "all"},
		{"swap_above_boundary", 50_001, 100_000, ActionSwapDebtToCollateral, "exact"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := idleObserved(topology)
			position := observed.Position(SyrupUsdcPyusd)
			position.CollateralDepositedRaw = 1_000_000
			position.DebtRaw = test.debt
			observed.DebtCustody(SyrupUsdcPyusd).AmountRaw = test.amount
			decision := planUp(observed, SyrupUsdcPyusd, topology)
			if decision.Kind != "execute" || decision.Plan.Action != test.action || decision.Plan.Amount.Mode != test.mode {
				t.Fatalf("unexpected decision %+v", decision)
			}
			if test.mode == "exact" && decision.Plan.Amount.Exactly != test.amount {
				t.Fatalf("amount %d, want %d", decision.Plan.Amount.Exactly, test.amount)
			}
		})
	}
}

func TestPlannerAndBorrowKeepFullScaledFractions(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	position := observed.Position(SyrupUsdcUsdc)
	position.CollateralDepositedRaw = 1_000_000_000
	position.CollateralValueSF = reviewSF(1_000)
	position.DebtValueSF = reviewSF(100)
	position.DebtRaw = 100_000_000
	position.DebtMarketPriceSF = reviewSF(1)
	position.DebtMintFactor = 1_000_000
	collateralBefore, debtBefore := position.CollateralValueSF.String(), position.DebtValueSF.String()
	decision := planUp(observed, SyrupUsdcUsdc, topology)
	if decision.Kind != "execute" || decision.Plan.Action != ActionBorrowDebt {
		t.Fatalf("normal position above $16 failed planning: %+v", decision)
	}
	config := topology.Strategies[SyrupUsdcUsdc]
	amount, err := resolveBorrow(decision.Plan.Amount, observed, config)
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(config.TargetLTVBPS)*100_000 - 100_000_000
	if amount != want {
		t.Fatalf("borrow amount %d, want %d", amount, want)
	}
	if position.CollateralValueSF.String() != collateralBefore || position.DebtValueSF.String() != debtBefore {
		t.Fatal("planner mutated confirmed evidence")
	}
	position.DebtValueSF = nil
	if decision := planUp(observed, SyrupUsdcUsdc, topology); decision.Kind != "invalid_observation" {
		t.Fatal("missing valuation became a complete or executable decision")
	}
	if _, err := resolveBorrow(AmountToTargetLTV, observed, config); err == nil {
		t.Fatal("unknown valuation produced a borrow amount")
	}
}

func reviewReserveAccount(config StrategyConfig, debt bool) *chain.Account {
	address, mint := config.CollateralReserve, config.CollateralMint
	if debt {
		address, mint = config.DebtReserve, config.DebtMint
	}
	data := make([]byte, reserveLength)
	copy(data[:8], reserveDiscriminator)
	binary.LittleEndian.PutUint64(data[8:16], 1)
	binary.LittleEndian.PutUint64(data[16:24], 500)
	copy(data[32:64], config.Market[:])
	mintKey := mustKey(mint)
	copy(data[128:160], mintKey[:])
	binary.LittleEndian.PutUint64(data[224:232], 1_000_000_000)
	putReviewSF(data[248:264], reviewSF(1))
	binary.LittleEndian.PutUint64(data[272:280], 6)
	binary.LittleEndian.PutUint64(data[2592:2600], 1_000_000_000)
	return &chain.Account{Key: address, Owner: mustKey(KlendProgram), Lamports: 1, Data: data}
}

func reviewObligationAccount(config StrategyConfig, vault solana.PublicKey) *chain.Account {
	data := make([]byte, obligationLength)
	copy(data[:8], obligationDiscriminator)
	copy(data[32:64], config.Market[:])
	copy(data[64:96], vault[:])
	copy(data[96:128], config.CollateralReserve[:])
	binary.LittleEndian.PutUint64(data[128:136], 500_000_000)
	copy(data[1208:1240], config.DebtReserve[:])
	putReviewSF(data[1296:1312], reviewSF(100_000_000))
	// Pinned SDK fields: adjusted debt is not elevation_group; referrer is
	// not unhealthy value. Nonzero sentinels catch the old inferred reads.
	putReviewSF(data[2208:2224], reviewSF(100))
	putReviewSF(data[2256:2272], reviewSF(400))
	data[2288] = 99
	return &chain.Account{Key: config.Obligation, Owner: mustKey(KlendProgram), Lamports: 1, Data: data}
}

func TestPinnedObligationLayoutAndReserveMintIdentity(t *testing.T) {
	topology := testTopology(t)
	config := topology.Strategies[SyrupUsdcPyusd]
	debtAccount := reviewReserveAccount(config, true)
	debt, err := decodeReserve(debtAccount, config)
	if err != nil {
		t.Fatalf("valid debt reserve rejected against collateral mint: %v", err)
	}
	collateral, err := decodeReserve(reviewReserveAccount(config, false), config)
	if err != nil {
		t.Fatal(err)
	}
	account := reviewObligationAccount(config, topology.Vault)
	position, err := decodeObligation(account, config, topology.Vault, collateral, debt, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if position.UnhealthyValueSF.Cmp(reviewSF(400)) != 0 || position.CollateralValueSF.Cmp(reviewSF(500)) != 0 || position.DebtValueSF.Cmp(reviewSF(100)) != 0 {
		t.Fatalf("wrong full-width valuation: %+v", position)
	}
	if position.DebtRaw != 100_000_000 {
		t.Fatalf("debt raw %d", position.DebtRaw)
	}
	account.Data[2285] = 1
	if _, err := decodeObligation(account, config, topology.Vault, collateral, debt, 0, 0); err == nil {
		t.Fatal("nonzero elevation group accepted")
	}
	account.Data[2285] = 0
	foreign := fixtureKey(230)
	copy(account.Data[232:264], foreign[:]) // second deposit, zero amount
	if _, err := decodeObligation(account, config, topology.Vault, collateral, debt, 0, 0); err == nil {
		t.Fatal("foreign zero-amount reserve slot was ignored")
	}
	wrongMint := mustKey(config.CollateralMint)
	copy(debtAccount.Data[128:160], wrongMint[:])
	if _, err := decodeReserve(debtAccount, config); err == nil {
		t.Fatal("wrong debt reserve mint accepted")
	}
}

func TestValuationUsesUnsignedRawAndWideIntermediates(t *testing.T) {
	reserve := &decodedReserve{MarketPriceSF: reviewSF(1), TotalLiquiditySF: reviewSF(math.MaxUint64), CollateralMintSupply: math.MaxUint64}
	value, err := collateralMarketValueSF(reserve, math.MaxUint64)
	if err != nil || value.Cmp(reviewSF(math.MaxUint64)) != 0 {
		t.Fatalf("unsigned raw amount narrowed: %v, %v", value, err)
	}
	reserve.MarketPriceSF = new(big.Int).Lsh(big.NewInt(1), 127)
	if _, err := collateralMarketValueSF(reserve, math.MaxUint64); err == nil {
		t.Fatal("value outside u128 accepted")
	}
}

type shortReviewReader struct{}

func (shortReviewReader) Accounts(context.Context, []solana.PublicKey, rpc.CommitmentType, uint64) (uint64, []*chain.Account, error) {
	return 500, nil, nil
}

func TestObservationRejectsIncompleteRPCArray(t *testing.T) {
	if _, err := ObserveConfirmed(context.Background(), shortReviewReader{}, testTopology(t), nil); err == nil {
		t.Fatal("missing RPC entries became an empty observation")
	}
}

func TestBorrowAPYIncludesHostFixedInterest(t *testing.T) {
	config := testTopology(t).Strategies[SyrupUsdcUsdc]
	account := reviewReserveAccount(config, true)
	binary.LittleEndian.PutUint16(account.Data[reserveConfigOffset+2:reserveConfigOffset+4], 1_000)
	reserve, err := decodeReserve(account, config)
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := reserveAPYBPS(account.Data, reserve, false)
	if err != nil {
		t.Fatal(err)
	}
	supply, err := reserveAPYBPS(account.Data, reserve, true)
	if err != nil {
		t.Fatal(err)
	}
	if borrow != 1_052 || supply != 0 {
		t.Fatalf("host fixed interest: borrow=%d supply=%d", borrow, supply)
	}
	for index := 0; index < 11; index++ {
		offset := reserveConfigOffset + 64 + index*8
		binary.LittleEndian.PutUint32(account.Data[offset+4:offset+8], math.MaxUint32)
	}
	if _, err := reserveAPYBPS(account.Data, reserve, false); err == nil {
		t.Fatal("compounded APY overflow accepted")
	}
}

func TestSnapshotClaimUnitsIdleZerosAndUnknownEvidence(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	observed.Claim.AmountRaw = 30_000_000
	route := testRouteState(t, topology)
	idle, err := snapshotInput(route, observed)
	if err != nil || idle.CollateralRaw != "0" || idle.DebtRaw != "0" || idle.EquityUSD != "30000000" {
		t.Fatalf("idle snapshot cannot be stored consistently: %+v %v", idle, err)
	}
	position := observed.Position(SyrupUsdcUsdc)
	position.CollateralDepositedRaw, position.DebtRaw = 1_000_000_000, 500_000_000
	position.CollateralValueSF, position.DebtValueSF = reviewSF(1_000), reviewSF(500)
	position.UnhealthyValueSF = reviewSF(800)
	active, err := snapshotInput(route, observed)
	if err != nil || active.EquityUSD != "530000000" {
		t.Fatalf("claim raw units double-scaled: %+v %v", active, err)
	}
	position.CollateralSupplyAPYBPS, position.DebtBorrowAPYBPS = 1, 3
	active, err = snapshotInput(route, observed)
	if err != nil || active.ForecastAPYBPS == nil || *active.ForecastAPYBPS != "0" {
		t.Fatalf("signed forecast division did not truncate toward zero: %+v %v", active, err)
	}
	position.DebtValueSF = nil
	if _, err := snapshotInput(route, observed); err == nil {
		t.Fatal("missing valuation became a zero-valued snapshot")
	}
}

func TestWorkerAcceptsExecutorPrivateKeyAndDepositRejectsWrap(t *testing.T) {
	topology := testTopology(t)
	dependencies := WorkerDeps{Store: &Store{pool: &pgxpool.Pool{}}, Observer: shortReviewReader{},
		Executor: &Executor{Chain: &fakeChain{}, Signer: testDelegateSeed()}, Quotes: fakeQuoteClient{topology}, WorkerID: "review",
		Chain: &fakeChain{}, Facts: testFacts()}
	if _, err := NewWorker(dependencies); err != nil {
		t.Fatal(err)
	}
	dependencies.Executor = &Executor{Signer: make([]byte, ed25519.SeedSize)}
	if _, err := NewWorker(dependencies); err == nil {
		t.Fatal("seed-sized buffer accepted as an executor private key")
	}
	state := testRouteState(t, topology)
	before := state.Generation
	if err := state.AdmitDeposit(DepositEvidence{RequestID: "wrap", TransactionSignature: "fixture", WalletAccount: "fixture",
		AmountRaw: math.MaxUint64, WalletPostAmountRaw: 1, VaultPostAmountRaw: math.MaxUint64}); err == nil {
		t.Fatal("wrapped wallet delta admitted a deposit")
	}
	if state.Generation != before {
		t.Fatal("invalid deposit mutated route generation")
	}
}
