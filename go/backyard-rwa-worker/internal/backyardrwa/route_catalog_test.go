package backyardrwa

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Independent retained SDK/account-vector evidence, not output from this Go
// compiler. No RPC, signer, simulation or send is used by this witness.
func TestCatalogKaminoConstructionMatchesRetainedAUTOAndEthena(t *testing.T) {
	testCatalogKaminoConstruction(t, []string{"AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"})
}

func TestCatalogKaminoConstructionMatchesRetainedPrimeSiblings(t *testing.T) {
	testCatalogKaminoConstruction(t, []string{"Prime/PRIME/PYUSD", "Prime/PRIME/USDS"})
	// Catalog support does not expand the three new-family canary budget.
	for _, lane := range []string{"Prime/PRIME/PYUSD", "Prime/PRIME/USDS"} {
		if phase3BudgetFamilyForLane(lane) != "" {
			t.Fatal("sibling construction authorized an extra funded canary")
		}
	}
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
	policies := map[string]string{}
	for _, p := range retained.Preflight.Bindings.Data.Policies {
		if p.RetainedBytesMatch {
			policies[p.Address] = p.Hash
		}
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
					request, err := manifest.kaminoPacketForRoute(action, leg, 77,
						LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, lane.Lane)
					if err != nil {
						t.Fatal(err)
					}
					if request.Policy != op.Policy || !validSHA256(policies[op.Policy]) ||
						request.PolicyAccountDataSHA256 != policies[op.Policy] || !reflect.DeepEqual(request.Accounts, op.Accounts) {
						t.Fatal("Go packet diverges from independent installed-policy account vector")
					}
					message, err := CompileKaminoMessage(request)
					if err != nil || len(message) == 0 || len(message)+65 > solanaPacketBytes {
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
								account.Address = bridgeSettings
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
					for _, mutation := range []func(*KaminoPrimeUSDCRequest){
						func(r *KaminoPrimeUSDCRequest) { r.Policy = bridgeAllocationPolicy },
						func(r *KaminoPrimeUSDCRequest) { r.PolicyAccountDataSHA256 = strings.Repeat("0", 64) },
						func(r *KaminoPrimeUSDCRequest) { r.RouteLane = SelectedRouteID },
						func(r *KaminoPrimeUSDCRequest) { r.RouteLane = "unknown/asset/debt" },
						func(r *KaminoPrimeUSDCRequest) { r.PolicyConstraintIndex = 1 },
						func(r *KaminoPrimeUSDCRequest) { r.AmountRaw++ },
					} {
						mutant := request
						mutation(&mutant)
						if _, err := CompileKaminoMessage(mutant); err == nil {
							t.Fatal("accepted policy/lane/constraint/amount mutation")
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
	if _, err := manifest.kaminoPacketForRoute(OpenRouteStep, kaminoLegDeposit, 77,
		LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}, "unknown/asset/debt"); err == nil {
		t.Fatal("unknown lane fell back to Prime packet construction")
	}
}
