package multiply

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func TestSwapQuoteFrontierBoundsSigningAndSimulation(t *testing.T) {
	executor, rpc, _ := testExecutor(t)
	quoteSlot := rpc.hash.ContextSlot + 100
	built := &BuiltOperation{QuoteContextSlot: &quoteSlot, PolicyInstructions: []Instruction{{ProgramID: solana.SystemProgramID, Data: []byte{1}}}}
	_, slot, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(45), 0, []byte{0}, rpc.hash.ContextSlot)
	if err != nil || slot != quoteSlot {
		t.Fatalf("signing discarded quote frontier: %d %v", slot, err)
	}
	quoteSlot = 0
	if _, _, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(45), 0, []byte{0}, rpc.hash.ContextSlot); err == nil {
		t.Fatal("unknown quote frontier accepted")
	}
}

func TestWithdrawalCannotReverseExpectedEffectSigns(t *testing.T) {
	topology := testTopology(t)
	observed := idleObserved(topology)
	position := observed.Position(SyrupUsdcUsdc)
	position.CollateralDepositedRaw = uint64(math.MaxInt64) + 1
	position.CollateralTotalSupplyRaw = 1
	position.CollateralTotalLiquiditySF = new(big.Int).SetUint64(fractionOneSF)
	for _, amount := range []PlannedAmount{AmountAll, AmountExact(uint64(math.MaxInt64) + 1)} {
		if _, err := BuildOperation(&ActionPlan{Action: ActionWithdrawRemainingCollateral, StrategyKey: SyrupUsdcUsdc, Amount: amount}, observed, topology, fakeQuoteClient{topology}, context.Background()); err == nil {
			t.Fatal("withdrawal flipped signed obligation or custody effects")
		}
	}
	// A small receipt-token withdrawal can redeem more than BIGINT liquidity.
	position.CollateralDepositedRaw = 1
	position.CollateralTotalLiquiditySF = new(big.Int).Lsh(new(big.Int).SetUint64(uint64(math.MaxInt64)+1), 60)
	if _, err := BuildOperation(&ActionPlan{Action: ActionWithdrawRemainingCollateral, StrategyKey: SyrupUsdcUsdc, Amount: AmountExact(1)}, observed, topology, fakeQuoteClient{topology}, context.Background()); err == nil {
		t.Fatal("liquidity conversion flipped signed custody effect")
	}
}

type unavailableReceiptRPC struct{ fakeRPC }

func (*unavailableReceiptRPC) ConfirmedTransaction(context.Context, string) (json.RawMessage, error) {
	return nil, errReceiptUnavailable
}

func TestConfirmedStatusWithoutReceiptCannotBecomeConfirmation(t *testing.T) {
	rpc := &unavailableReceiptRPC{fakeRPC: fakeRPC{statuses: []*SignatureObservation{{Slot: 500, ConfirmationState: "confirmed"}}}}
	executor := &Executor{RPC: rpc}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if status, err := executor.WaitConfirmed(ctx, solana.Signature{}.String()); status != nil || !errors.Is(err, errReceiptUnavailable) {
		t.Fatalf("status cache promoted to receipt: %v %v", status, err)
	}
}

func TestKeylessRecoveryCannotAcquireSigningCapability(t *testing.T) {
	_, rpc, _ := testExecutor(t)
	executor, err := NewRecoveryExecutorContext(context.Background(), rpc)
	if err != nil {
		t.Fatal(err)
	}
	built := &BuiltOperation{PolicyInstructions: []Instruction{{ProgramID: solana.SystemProgramID, Data: []byte{1}}}}
	if _, _, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(46), 0, []byte{0}, 500); err == nil {
		t.Fatal("keyless recovery created a new signed wire")
	}
}
