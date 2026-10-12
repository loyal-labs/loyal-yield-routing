package backyard

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// hastraPriceAccount is a StakePriceConfig at price (wYLDS per AUTO, scale
// 1e18) verified for timestamp, with Hastra's 3600 s staleness limit.
func hastraPriceAccount(price *big.Int, timestamp int64) ConfirmedAccount {
	data := make([]byte, 177)
	copy(data, hastra.StakePriceConfigDiscriminator[:])
	low := new(big.Int).And(price, new(big.Int).SetUint64(^uint64(0)))
	binary.LittleEndian.PutUint64(data[136:], low.Uint64())
	binary.LittleEndian.PutUint64(data[144:], new(big.Int).Rsh(price, 64).Uint64())
	binary.LittleEndian.PutUint64(data[152:], 1_000_000_000_000_000_000)
	binary.LittleEndian.PutUint64(data[160:], uint64(timestamp))
	binary.LittleEndian.PutUint64(data[168:], 3600)
	return ConfirmedAccount{Address: hastra.StakePriceConfig.String(), Owner: hastra.StakeProgramID.String(), Lamports: 2_123_280, Data: data}
}

// hastraTestView is an account view without a stream, complete through slot.
func hastraTestView(slot int64, accounts ...ConfirmedAccount) *View {
	v := &View{now: time.Now, accounts: map[string]viewAccount{}, receipts: map[string]uint64{}, advanced: make(chan struct{}), through: uint64(slot)}
	for _, a := range accounts {
		v.accounts[a.Address] = viewAccount{ConfirmedAccount: a, slot: uint64(slot)}
	}
	return v
}

func hastraTestChain(t *testing.T) *chain.Client {
	t.Helper()
	return newFakeChain(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":{"blockhash":"` + bridgeSettings + `","lastValidBlockHeight":99}}}`), nil
	}))
}

// hastraTestPolicies holds the Hastra literal at a stand-in account: it is
// not installed until the parent installs it.
func hastraTestPolicies() (installedPolicies, string) {
	account := solana.MustPublicKeyFromBase58("HastraPo1icy1111111111111111111111111111111")
	return installedPolicies{policyKey{hastra: hastraName()}: {Installed: squads.Installed{Account: account}, matches: 1}}, account.String()
}

// hastraTestPrice is about the mainnet price the canary saw: 1.023948 wYLDS
// per AUTO, scaled by 1e18.
var hastraTestPrice, _ = new(big.Int).SetString("1023948000000000000", 10)

// A Hastra lane's debt conversions are Hastra's, by its mints; its
// collateral and USDC conversions stay on Jupiter's catalog edges, and its
// debt never converts into USDC. AUTO/AUTO/PYUSD converts exactly as before,
// every conversion on the installed catalog policies it always used.
func TestConversionVenueSplitsTheWYLDSLaneAndLeavesPYUSD(t *testing.T) {
	t.Parallel()
	wylds := map[Action]byte{SwapDebtToCollateralStep: hastraStakeDepositLeg, SwapCollateralToDebtStep: hastraStakeRedeemLeg, SwapUSDCToDebtStep: hastraMintDepositLeg}
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		leg, hastraVenue := hastraConversionLeg(action, autoAUTOWYLDSLane)
		want, wantHastra := wylds[action]
		if hastraVenue != wantHastra || leg != want {
			t.Errorf("wYLDS %s: Hastra leg %d %t, want %d %t", action, leg, hastraVenue, want, wantHastra)
		}
		if _, pyusdHastra := hastraConversionLeg(action, autoAUTOPYUSD.Lane); pyusdHastra {
			t.Errorf("AUTO/PYUSD %s converts through Hastra", action)
		}
		key, index, err := jupiterPolicyLeg(autoAUTOWYLDSLane, action)
		switch action {
		case SwapStableToCollateralStep, SwapCollateralToStableStep:
			if err != nil || key.swaps != "USDC->AUTO/AUTO->USDC/AUTO->PYUSD" || index != map[Action]byte{SwapStableToCollateralStep: 0, SwapCollateralToStableStep: 1}[action] {
				t.Errorf("wYLDS %s swaps through %+v leg %d (%v), want AUTO's USDC catalog edge", action, key, index, err)
			}
		default:
			if err == nil {
				t.Errorf("wYLDS %s has a Jupiter edge %+v", action, key)
			}
		}
	}
	for action, swaps := range map[Action]string{
		SwapStableToCollateralStep: "USDC->AUTO/AUTO->USDC/AUTO->PYUSD", SwapCollateralToStableStep: "USDC->AUTO/AUTO->USDC/AUTO->PYUSD",
		SwapCollateralToDebtStep: "USDC->AUTO/AUTO->USDC/AUTO->PYUSD", SwapDebtToCollateralStep: "PYUSD->AUTO/PYUSD->USDe",
		SwapDebtToUSDCStep: "PYUSD->USDC/PYUSD->USDS",
	} {
		key, _, err := jupiterPolicyLeg(autoAUTOPYUSD.Lane, action)
		if err != nil || key.swaps != swaps || testPolicies(t)[key].matches != 1 {
			t.Errorf("AUTO/PYUSD %s swaps through %+v (%v), want the installed %s", action, key, err, swaps)
		}
	}
	if _, _, err := jupiterPolicyLeg(autoAUTOPYUSD.Lane, SwapUSDCToDebtStep); err == nil {
		t.Error("AUTO/PYUSD gained a USDC into PYUSD edge")
	}
	if !autoCollateralLane(autoAUTOWYLDSLane) || !autoCollateralLane(autoAUTOPYUSD.Lane) || autoCollateralLane(primePRIMEPYUSD.Lane) {
		t.Error("the AUTO special cases are not keyed on AUTO collateral")
	}
}

// The Hastra literal converts between exactly Backyard's custodies: its
// stake accounts are the AUTO lanes' shared AUTO custody and the wYLDS
// lane's debt custody, its mint source the vault's USDC custody.
func TestHastraLiteralConvertsBetweenBackyardCustodies(t *testing.T) {
	t.Parallel()
	stake, err := hastra.OwnStake(kaminoKey(bridgeVault))
	if err != nil {
		t.Fatal(err)
	}
	mint, err := hastra.OwnMint(kaminoKey(bridgeVault))
	if err != nil {
		t.Fatal(err)
	}
	if stake.UserAUTO.String() != autoAUTOWYLDS.CollateralCustody || stake.UserAUTO.String() != autoAUTOPYUSD.CollateralCustody ||
		stake.UserWYLDS.String() != autoAUTOWYLDS.DebtCustody || mint.UserUSDC.String() != bridgeSquadsATA || mint.UserWYLDS != stake.UserWYLDS {
		t.Fatalf("Hastra accounts %+v %+v are not Backyard's custodies", stake, mint)
	}
}

// Every Hastra leg compiles, as production signs it, into the heap frame and
// one Squads execute that the exact legacy decoder admits, executing at its
// own constraint of the Hastra literal and at no other; with a foreign
// destination no constraint admits it.
func TestEveryHastraLegExecutesAtItsOwnConstraint(t *testing.T) {
	t.Parallel()
	literal, err := hastraPolicy()
	if err != nil {
		t.Fatal(err)
	}
	_, policy := hastraTestPolicies()
	price := hastra.StakePrice{Price: hastraTestPrice, Scale: 1_000_000_000_000_000_000, Timestamp: kaminoFixtureUnix}
	signer := solana.NewWallet().PrivateKey
	delegate := publicKey(signer.PublicKey())
	for _, action := range []Action{SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep} {
		t.Run(string(action), func(t *testing.T) {
			leg, _ := hastraConversionLeg(action, autoAUTOWYLDSLane)
			request := HastraConversionRequest{Action: action, AmountRaw: 1_000_000, Policy: policy, RecentBlockhash: bridgeSettings, LastValidBlockHeight: 99, RouteLane: autoAUTOWYLDSLane}
			if leg != hastraMintDepositLeg {
				request.Price, request.PriceScale, request.PriceTimestamp = price.Price.String(), price.Scale, price.Timestamp
			}
			if request.OutputRaw, err = hastraOutput(leg, price, request.AmountRaw); err != nil {
				t.Fatal(err)
			}
			message, err := compileHastraMessageForDelegate(request, delegate)
			if err != nil {
				t.Fatal(err)
			}
			signature, err := signer.Sign(message)
			if err != nil {
				t.Fatal(err)
			}
			wire := signedWire(message, signature[:])
			if _, _, _, _, err := decodeExactWire(wire); err != nil {
				t.Fatal("the exact decoder refuses the Hastra wire:", err)
			}
			execution := decodeTestExecution(t, message)
			if execution.Policy.String() != policy || len(execution.Inner) != 1 || fmt.Sprint(execution.ConstraintIndexes) != fmt.Sprint([]byte{leg}) {
				t.Fatalf("executes %+v, want one instruction at leg %d of %s", execution, leg, policy)
			}
			ix := execution.Inner[0]
			for index, constraint := range literal.Constraints {
				if admits := squads.Admits(constraint, ix, vaultObligation); admits != (index == int(leg)) {
					t.Errorf("constraint %d admits leg %d: %t", index, leg, admits)
				}
			}
			destination := map[byte]int{hastraStakeDepositLeg: 9, hastraStakeRedeemLeg: 6, hastraMintDepositLeg: 7}[leg]
			changed := squads.Instruction{ProgramID: ix.ProgramID, Accounts: slices.Clone(ix.Accounts), Data: ix.Data}
			changed.Accounts[destination].PublicKey = solanaKey(bridgeDelegate)
			for index, constraint := range literal.Constraints {
				if squads.Admits(constraint, changed, vaultObligation) {
					t.Errorf("constraint %d admits a foreign destination", index)
				}
			}
			// The persisted build input recompiles to the same message.
			input, err := encodePhase3BuildInput(request, []byte(`{}`))
			if err != nil || input.Kind != "hastra" {
				t.Fatal(input, err)
			}
			mismatch := request
			mismatch.OutputRaw++
			if _, err := compileHastraMessageForDelegate(mismatch, delegate); err == nil {
				t.Fatal("an output other than the program's at the quoted price compiled")
			}
		})
	}
}

// A conversion's effects are the program's own: the source debited exactly
// the amount, the destination credited at least the stake price's output
// with the program's floor (1:1 for a mint deposit), and its executable
// debit is the source custody's.
func TestHastraEffectsQuoteTheProgramRounding(t *testing.T) {
	t.Parallel()
	view := hastraTestView(42, hastraPriceAccount(hastraTestPrice, kaminoFixtureUnix-60), clockFixture())
	rpc := hastraTestChain(t)
	policies, policy := hastraTestPolicies()
	scale := big.NewInt(1_000_000_000_000_000_000)
	floor := func(amount uint64, multiplier, divisor *big.Int) uint64 {
		out := new(big.Int).Mul(new(big.Int).SetUint64(amount), multiplier)
		return out.Quo(out, divisor).Uint64()
	}
	for _, row := range []struct {
		action                   Action
		source, destination      string
		sourceMint, destinationM string
		want                     uint64
	}{
		{SwapDebtToCollateralStep, autoAUTOWYLDS.DebtCustody, autoAUTOWYLDS.CollateralCustody, hastra.WYLDSMint.String(), hastra.AUTOMint.String(), floor(1_000_003, scale, hastraTestPrice)},
		{SwapCollateralToDebtStep, autoAUTOWYLDS.CollateralCustody, autoAUTOWYLDS.DebtCustody, hastra.AUTOMint.String(), hastra.WYLDSMint.String(), floor(1_000_003, hastraTestPrice, scale)},
		{SwapUSDCToDebtStep, bridgeSquadsATA, autoAUTOWYLDS.DebtCustody, bridgeUSDC, hastra.WYLDSMint.String(), 1_000_003},
	} {
		decision := Decision{Action: row.action, AmountRaw: 1_000_003, StrategyKey: autoAUTOWYLDSLane}
		evidence, err := prepareHastraQuoteEvidence(context.Background(), rpc, view, policies, decision, 5_000_000, 7, 42)
		if err != nil {
			t.Fatal(row.action, err)
		}
		r, e := evidence.Request, evidence.ExpectedEffects
		if r.OutputRaw != row.want || r.Policy != policy || r.RecentBlockhash != bridgeSettings || (row.action == SwapUSDCToDebtStep) != (r.Price == "") {
			t.Fatalf("%s request %+v, want output %d", row.action, r, row.want)
		}
		if _, err := DecodeExpectedEffects(mustMarshalEffects(t, e)); err != nil {
			t.Fatal(err)
		}
		source, destination := e.Accounts[0], e.Accounts[1]
		if source.Address != row.source || source.Mint != row.sourceMint || source.BeforeRaw != 5_000_000 || source.AfterRaw != 5_000_000-1_000_003 ||
			destination.Address != row.destination || destination.Mint != row.destinationM || destination.BeforeRaw != 7 || *destination.MinimumAfterRaw != 7+row.want {
			t.Fatalf("%s effects %+v", row.action, e)
		}
		debit, err := MeasureExecutableDebit(r, e)
		if err != nil || debit.Source != row.source || debit.Raw != 1_000_003 {
			t.Fatalf("%s debit %+v (%v)", row.action, debit, err)
		}
	}
}

func mustMarshalEffects(t *testing.T, e ExpectedEffects) []byte {
	t.Helper()
	raw, err := jsonMarshalExpectedEffects(e)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
