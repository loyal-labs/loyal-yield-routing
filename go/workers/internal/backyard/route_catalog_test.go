package backyard

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// Independent retained SDK/account-vector evidence, not output from this Go
// compiler. No RPC, signer, simulation or send is used by this witness.
func TestCatalogKaminoConstructionMatchesRetainedAUTOAndEthena(t *testing.T) {
	t.Parallel()
	testCatalogKaminoConstruction(t, []string{"AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"})
}

func TestCatalogKaminoConstructionMatchesRetainedPrimeSiblings(t *testing.T) {
	t.Parallel()
	testCatalogKaminoConstruction(t, []string{"Prime/PRIME/PYUSD", "Prime/PRIME/USDS"})
	// Owner 2026-10-10: both Prime siblings are funded registry lanes.
	for _, lane := range []string{"Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		if !earnActiveLane(lane) || !earnHeldLane(lane) {
			t.Fatal("Prime sibling is not a funded registry lane", lane)
		}
	}
}

// basicLaneIDs are the registry lanes the basic policy families serve, in
// registry order.
func basicLaneIDs() []string {
	var out []string
	for _, lane := range earnLaneIDs(true) {
		if basicLane(lane) {
			out = append(out, lane)
		}
	}
	return out
}

func testCatalogKaminoConstruction(t *testing.T, lanes []string) {
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json")
	if err != nil {
		t.Fatal(err)
	}
	var retained struct {
		Preflight struct {
			Bindings struct {
				Data struct {
					Policies []struct {
						Address, Hash      string
						RetainedBytesMatch bool
					}
					Lanes []struct {
						Lane           string
						CustodiesExact bool
						Obligation     struct {
							Address, Market string
							Exact           bool
						}
						FarmAccounts []json.RawMessage
						Operations   []struct {
							Operation, Policy                                              string
							ProgramMatches, RetainedPolicyBytesMatch, AccountVectorMatches bool
							Accounts                                                       KaminoPrimeUSDCAccounts
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(data, &retained); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, lane := range retained.Preflight.Bindings.Data.Lanes {
		if lane.Lane != lanes[0] && lane.Lane != lanes[1] {
			continue
		}
		if seen[lane.Lane] {
			t.Fatal("duplicate retained lane")
		}
		seen[lane.Lane] = true
		t.Run(lane.Lane, func(t *testing.T) {
			route, err := runtimeRoute(lane.Lane)
			if err != nil {
				t.Fatal(err)
			}
			if !lane.CustodiesExact || !lane.Obligation.Exact || len(lane.FarmAccounts) != 0 ||
				route.Kamino.Obligation != lane.Obligation.Address || route.Kamino.Market != lane.Obligation.Market {
				t.Fatal("binding lacks retained exact custody/obligation evidence")
			}
			operations := map[string]bool{}
			for _, op := range lane.Operations {
				if operations[op.Operation] {
					t.Fatal("duplicate retained operation")
				}
				operations[op.Operation] = true
				t.Run(op.Operation, func(t *testing.T) {
					leg, action := kaminoLegDeposit, OpenRouteStep
					switch op.Operation {
					case "deposit":
					case "borrow":
						leg = kaminoLegBorrow
					case "repay":
						leg, action = kaminoLegRepay, DeleverRouteStep
					case "withdraw":
						leg, action = kaminoLegWithdraw, DeleverRouteStep
					default:
						t.Fatal("unknown retained operation")
					}
					if !op.ProgramMatches || !op.RetainedPolicyBytesMatch || !op.AccountVectorMatches {
						t.Fatal("retained operation is not exact")
					}
					request, err := manifest.kaminoPacketForRoute(testPolicies(t), action, leg, 77,
						LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, lane.Lane)
					if err != nil {
						t.Fatal(err)
					}
					// The leg executes through its lane's installed literal
					// (AUTO's one policy, a split lane's per-leg policy).
					key, _ := kaminoPolicyLeg(route, leg)
					if request.Policy != testPolicyAccount(key) || !reflect.DeepEqual(request.Accounts, op.Accounts) {
						t.Fatal("Go packet diverges from independent installed-policy account vector")
					}
					message, err := CompileKaminoMessage(request)
					if err != nil || len(message) == 0 || len(message)+65 > maxTransactionBytes {
						t.Fatalf("unsigned packet construction failed: bytes=%d err=%v", len(message), err)
					}
					// Substituting any identity or privilege must fail before signing.
					for index := range request.Accounts {
						for _, field := range []string{"address", "signer", "writable"} {
							mutant := request
							mutant.Accounts = append(KaminoPrimeUSDCAccounts(nil), request.Accounts...)
							account := &mutant.Accounts[index]
							switch field {
							case "address":
								account.Address = backyardSettings
							case "signer":
								account.Signer = !account.Signer
							case "writable":
								account.Writable = !account.Writable
							}
							if _, err := CompileKaminoMessage(mutant); err == nil {
								t.Fatalf("accepted account %d %s mutation", index, field)
							}
						}
					}
					for _, mutation := range []func(*KaminoLegRequest){
						func(r *KaminoLegRequest) { r.RouteLane = SelectedRouteID },
						func(r *KaminoLegRequest) { r.RouteLane = "unknown/asset/debt" },
						func(r *KaminoLegRequest) { r.AmountRaw++ },
					} {
						mutant := request
						mutation(&mutant)
						if _, err := CompileKaminoMessage(mutant); err == nil {
							t.Fatal("accepted lane/amount mutation")
						}
					}
				})
			}
			if len(operations) != 4 {
				t.Fatal("incomplete retained four-leg evidence")
			}
		})
	}
	if len(seen) != 2 {
		t.Fatal("missing retained exact lane evidence")
	}
	if _, err := manifest.kaminoPacketForRoute(testPolicies(t), OpenRouteStep, kaminoLegDeposit, 77,
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, "unknown/asset/debt"); err == nil {
		t.Fatal("unknown lane fell back to Prime packet construction")
	}
}

// AUTO/AUTO/wYLDS is AUTO's lane with wYLDS debt: its derived accounts are
// the live reserve's (testdata/wylds-term-debt.json, slot 455559961), its
// debt custody is the vault's wYLDS account, and it shares AUTO/PYUSD's
// market, collateral reserve and collateral custody. It is AUTO/PYUSD's
// overflow lane: active and held like any registry lane, entered only by
// an overflow tranche.
func TestAUTOWYLDSRouteIsTheLiveReserveAndAUTOsOverflow(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../programs/kamino/testdata/wylds-term-debt.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Accounts map[string]struct {
			Owner    solana.PublicKey
			Lamports uint64
			Data     []byte
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	route, err := runtimeRoute(autoAUTOWYLDSLane)
	if err != nil {
		t.Fatal(err)
	}
	a := fixture.Accounts[route.Kamino.DebtReserve]
	reserve, err := kamino.DecodeReserve(&chain.Account{Key: kaminoKey(route.Kamino.DebtReserve), Owner: a.Owner, Lamports: a.Lamports, Data: a.Data})
	if err != nil {
		t.Fatal(err)
	}
	if reserve.LendingMarket.String() != route.Kamino.Market || reserve.LiquidityMint.String() != route.Kamino.DebtMint ||
		reserve.LiquiditySupply.String() != route.DebtLiquiditySupply || reserve.FeeReceiver.String() != route.DebtFeeReceiver ||
		reserve.LiquidityTokenProgram.String() != route.DebtTokenProgram || !reserve.FarmDebt.IsZero() {
		t.Fatalf("wYLDS route diverges from its live reserve: %+v", route)
	}
	if route.DebtCustody != "4yABxrtZ9kQqckcupQ32M6k9RzyiAk5stmTCFcTRZK8E" || route.CollateralCustody != autoAUTOPYUSD.CollateralCustody ||
		route.Kamino.CollateralReserve != autoAUTOPYUSD.Kamino.CollateralReserve || route.Kamino.Market != autoAUTOPYUSD.Kamino.Market ||
		route.Kamino.Obligation == autoAUTOPYUSD.Kamino.Obligation || route.DebtCustody == autoAUTOPYUSD.DebtCustody {
		t.Fatalf("wYLDS route custody or obligation: %+v", route)
	}
	// Its obligation is the one the lane's initializer creates.
	obligation, err := kamino.ObligationAddress(1, 0, kaminoKey(bridgeVault), kaminoKey(route.Kamino.Market),
		kaminoKey(route.Kamino.CollateralMint), kaminoKey(route.Kamino.DebtMint))
	if err != nil || obligation.String() != route.Kamino.Obligation {
		t.Fatal("wYLDS obligation is not the vault's tag-1 AUTO/wYLDS PDA", obligation, err)
	}
	if !earnHeldLane(autoAUTOWYLDSLane) || !earnActiveLane(autoAUTOWYLDSLane) || !earnInitializerLane(autoAUTOWYLDSLane) ||
		!slices.Contains(earnLaneIDs(true), autoAUTOWYLDSLane) {
		t.Fatal("wYLDS lane is not a registry lane")
	}
	if source, ok := overflowSourceOf(autoAUTOWYLDSLane); !ok || source != autoAUTOPYUSD.Lane ||
		selectorDestinationLane(autoAUTOWYLDSLane) || !selectorDestinationLane(autoAUTOPYUSD.Lane) {
		t.Fatal("wYLDS lane is not AUTO/PYUSD's overflow")
	}
}
