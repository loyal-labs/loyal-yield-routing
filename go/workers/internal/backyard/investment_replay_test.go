package backyard

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type offlineReplayInput struct {
	Action          string
	Lane            string
	Slot            int64
	Amount          uint64
	Accounts        []ConfirmedAccount
	Template        JupiterSwapRequest
	DebtTemplate    JupiterSwapRequest
	ResidueTemplate JupiterSwapRequest
	QuoteOutput     uint64
	AddedCollateral uint64
	AddedDebt       uint64
	QuoteToDebt     bool
	QuoteDebtToUSDC bool
	QuoteOutputs    map[string]uint64
}

func TestOfflineLeveragedReplayStep(t *testing.T) {
	path := os.Getenv("OFFLINE_REPLAY_INPUT")
	if path == "" {
		t.Skip("offline scenario driver required")
	}
	var input offlineReplayInput
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"mode": "offline", "networkRequests": 0}
	defer func() {
		if err != nil {
			report["error"] = shadowError(err)
		}
		raw, e := json.Marshal(report)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(os.Getenv("OFFLINE_REPLAY_OUTPUT"), raw, 0600); e != nil {
			t.Fatal(e)
		}
	}()
	v := *looping
	v.manifest.vault = &v
	m := v.manifest
	route, e := m.route(input.Lane)
	if e != nil {
		err = e
		return
	}
	st, e := readExitStepState(&v, input.Accounts, route, input.Slot)
	if e != nil {
		err = e
		return
	}
	prices, e := batchLanePrices(input.Accounts, route)
	if e != nil {
		err = e
		return
	}
	held, e := st.collateral.redeemLiquidityRaw(st.position.CollateralDepositedRaw)
	if e != nil {
		err = e
		return
	}
	value, e := prices.collateralMicros(held + st.collateralBalance)
	if e != nil {
		err = e
		return
	}
	report["position"], report["collateralRaw"], report["collateralValueRaw"] = st.position, held+st.collateralBalance, value
	report["collateralAccount"], report["cashAccount"], report["obligation"] = route.CollateralCustody, v.cash, route.Kamino.Obligation
	cash, e := vaultCustodyRaw(&v, input.Accounts, v.cash, fleet.USDCMint, bridgeTokenProgram)
	if e != nil {
		err = e
		return
	}
	report["cashRaw"], report["debtCashRaw"], report["debtCashAccount"] = cash, st.debtBalance, route.DebtCustody
	report["debtReserve"], report["debtLiquiditySupply"] = route.Kamino.DebtReserve, route.DebtLiquiditySupply
	if input.Action == "inspect" {
		return
	}
	installed := []squads.Installed{}
	for _, a := range input.Accounts {
		if a.Owner != squads.ProgramID.String() {
			continue
		}
		ca := chainAccount(a, a.Address)
		policy, e := squads.DecodeCanonicalPolicy(ca)
		if e == nil {
			installed = append(installed, squads.Installed{Account: solanaKey(a.Address), View: policy, Raw: ca})
		}
	}
	policies, e := findInstalledPolicies(&v, installed)
	if e != nil {
		err = e
		return
	}
	hash := LatestBlockhash{Blockhash: bridgeVault, LastValidBlockHeight: 99}
	var compile func(publicKey) ([]byte, error)
	switch input.Action {
	case "deposit", "borrow":
		leg, action := kaminoLegDeposit, OpenRouteStep
		if input.Action == "borrow" {
			leg = kaminoLegBorrow
		}
		request, e := m.kaminoPacketForRoute(policies, action, leg, input.Amount, hash, route.Lane)
		if e != nil {
			err = e
			return
		}
		request.ObligationReserves = st.topology
		compile = func(d publicKey) ([]byte, error) { return compileKaminoMessageForDelegate(&v, request, d) }
	case "withdraw", "quote", "residue", "screen":
		if input.Action == "withdraw" && held == 0 && st.collateralBalance == 0 {
			report["complete"] = st.position.DebtRaw == 0
			return
		}
		rpc := budgetBuildRPCWithAccounts(t, 5000, input.Slot, input.Accounts)
		client, e := fixtureJupiter(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/quote":
				q := r.URL.Query()
				a, _ := strconv.ParseUint(q.Get("amount"), 10, 64)
				template := input.Template
				if q.Get("outputMint") == route.Kamino.DebtMint && route.Kamino.DebtMint != fleet.USDCMint {
					template = input.DebtTemplate
				}
				if q.Get("inputMint") == route.Kamino.DebtMint && route.Kamino.DebtMint != fleet.USDCMint {
					template = input.ResidueTemplate
				}
				if template.AmountRaw == 0 {
					return nil, fmt.Errorf("missing captured route")
				}
				n := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(template.QuotedOutputRaw))
				out := n.Quo(n, new(big.Int).SetUint64(template.AmountRaw)).Uint64()
				if input.Action == "quote" {
					out = 2
				}
				if input.Action == "screen" {
					key := q.Get("inputMint") + ":" + q.Get("outputMint") + ":" + fmt.Sprint(a)
					var ok bool
					out, ok = input.QuoteOutputs[key]
					if !ok {
						report["quoteRequest"] = map[string]any{"key": key, "amount": a, "toDebt": q.Get("outputMint") != fleet.USDCMint, "fromDebt": q.Get("inputMint") == route.Kamino.DebtMint && route.Kamino.DebtMint != fleet.USDCMint}
						return nil, fmt.Errorf("missing simulated quote")
					}
				}
				bps, _ := strconv.ParseUint(q.Get("slippageBps"), 10, 16)
				raw, _ := json.Marshal(jupiter.Quote{InputMint: q.Get("inputMint"), OutputMint: q.Get("outputMint"), InAmount: fmt.Sprint(a), OutAmount: fmt.Sprint(out), OtherAmountThreshold: fmt.Sprint(jupiterOutputFloor(out, uint16(bps))), SwapMode: "ExactIn", SlippageBPS: uint16(bps), ContextSlot: uint64(input.Slot), RoutePlan: investmentTestPath(q.Get("inputMint"), q.Get("outputMint"))})
				return response(string(raw)), nil
			case "/swap-instructions":
				var req struct {
					QuoteResponse jupiter.Quote `json:"quoteResponse"`
				}
				if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
					return nil, e
				}
				template := input.Template
				if req.QuoteResponse.OutputMint == route.Kamino.DebtMint && route.Kamino.DebtMint != fleet.USDCMint {
					template = input.DebtTemplate
				}
				if req.QuoteResponse.InputMint == route.Kamino.DebtMint && route.Kamino.DebtMint != fleet.USDCMint {
					template = input.ResidueTemplate
				}
				ix := template.Instruction
				data, e := base64.StdEncoding.DecodeString(ix.Data)
				if e != nil {
					return nil, e
				}
				a, _ := strconv.ParseUint(req.QuoteResponse.InAmount, 10, 64)
				o, _ := strconv.ParseUint(req.QuoteResponse.OutAmount, 10, 64)
				binary.LittleEndian.PutUint64(data[jupiter.V2InAmountOffset:], a)
				binary.LittleEndian.PutUint64(data[jupiter.V2QuotedOutOffset:], o)
				binary.LittleEndian.PutUint16(data[jupiter.V2SlippageOffset:], req.QuoteResponse.SlippageBPS)
				ix.Data = base64.StdEncoding.EncodeToString(data)
				raw, _ := json.Marshal(map[string]any{"swapInstruction": ix})
				return response(string(raw)), nil
			default:
				return nil, fmt.Errorf("blocked offline endpoint %s", r.URL.Path)
			}
		}))
		if e != nil {
			err = e
			return
		}
		if input.Action == "screen" {
			o := Observation{ObservedAt: time.Now(), Snapshot: Snapshot{Slot: input.Slot}}
			result, e := (lanePricer{client: client, manifest: m}).priceInvestmentExit(context.Background(), o, route, input.Accounts, input.Slot, input.AddedCollateral, input.AddedDebt)
			report["screen"], report["eligible"] = result, e == nil
			if e != nil {
				report["screenReason"] = shadowError(e)
			}
			return
		}
		if input.Action == "residue" {
			amount, e := executionTokenLimit(prices, route, route.Kamino.DebtMint)
			if e != nil {
				err = e
				return
			}
			amount = min(amount, st.debtBalance)
			d := Decision{Action: SwapDebtToUSDCStep, StrategyKey: route.Lane, AmountRaw: int64(amount), MaxSlippageBPS: 20}
			x, e := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, policies, d, amount, 0, input.Slot)
			if e != nil {
				err = e
				return
			}
			value, e := valueBetweenTokenRaw(amount, prices.debtDecimals, 6, prices.debt, prices.usdc, false)
			if e != nil {
				err = e
				return
			}
			g, e := floorWithdrawalConversion(x.Request, ExpectedSale{}, investmentExitFloor(value))
			if e != nil {
				err = e
				return
			}
			request := g.(JupiterSwapRequest)
			report["amountRaw"], report["floorRaw"], report["referenceRaw"], report["kind"] = amount, request.MinimumOutputRaw, value, "residue"
			compile = func(d publicKey) ([]byte, error) { return compileJupiterMessageForDelegate(&v, request, d) }
		} else if input.Action == "quote" {
			d := Decision{Action: SwapCollateralToStableStep, StrategyKey: route.Lane, AmountRaw: int64(input.Amount), MaxSlippageBPS: 20}
			if input.QuoteToDebt {
				d.Action = SwapCollateralToDebtStep
			}
			if input.QuoteDebtToUSDC {
				d.Action = SwapDebtToUSDCStep
			}
			x, e := prepareJupiterQuoteEvidence(context.Background(), rpc, client, m, policies, d, input.Amount, 0, input.Slot)
			if e != nil {
				err = e
				return
			}
			// Pricing probe only: execute Jupiter directly, like the quote
			// service. Full worker replays retain the installed Squads policies.
			ix := x.Request.Instruction
			data, e := base64.StdEncoding.DecodeString(ix.Data)
			if e != nil {
				err = e
				return
			}
			var metas []accountMeta
			for _, a := range ix.Accounts {
				metas = append(metas, accountMeta{key: mustKey(a.Pubkey), signer: a.Pubkey == v.squads, writable: a.IsWritable})
			}
			message, e := compileV1Message(mustKey(v.squads), mustKey(bridgeVault), []compiledInstruction{{program: mustKey(ix.ProgramID), accounts: metas, data: data}}, swapBudget())
			if e != nil {
				err = e
				return
			}
			report["message"], report["pricingOnly"] = base64.StdEncoding.EncodeToString(message), true
			return
		} else {
			st.maxSlippageBPS = 20
			st.conversionCap, e = executionTokenLimit(prices, route, route.Kamino.CollateralMint)
			if e != nil {
				err = e
				return
			}
			st.cash = cash
			kind, open, convert := stepExit, st.position.CollateralDepositedRaw, held+st.collateralBalance
			if st.position.DebtRaw > 0 {
				kind = stepDown
				s := Snapshot{PositionDebtRaw: int64(st.position.DebtRaw), PositionCollateralRaw: int64(st.position.CollateralDepositedRaw)}
				plan, e := planDownStep(s, Decision{Action: DeleverRouteStep, Reason: debtZeroReason, StrategyKey: route.Lane}, st, st.position.DebtRaw)
				if e != nil {
					err = e
					return
				}
				open, convert = plan.withdrawReceiptsRaw, plan.convertCollateralRaw
			}
			if open == 0 {
				err = fmt.Errorf("no receipt amount for withdrawal")
				return
			}
			evidence, _, e := assembleStep(context.Background(), rpc, nil, client, m, policies, st, kind, open, convert, input.Slot)
			if e != nil {
				err = e
				return
			}
			swap := evidence.Request.Swap
			value, e := prices.collateralMicros(swap.AmountRaw)
			if e != nil {
				err = e
				return
			}
			floor := investmentExitFloor(uint64(value))
			sale := ExpectedSale{}
			if kind == stepDown && route.Kamino.DebtMint != fleet.USDCMint {
				sale.Proceeds = &saleProceedsMark{PriceSF: prices.debt, USDCSF: prices.usdc, Decimals: prices.debtDecimals}
			}
			guarded, e := floorWithdrawalConversion(*swap, sale, floor)
			if e != nil {
				err = e
				return
			}
			g := guarded.(JupiterSwapRequest)
			evidence.Request.Swap = &g
			report["kind"], report["amountRaw"], report["floorRaw"], report["referenceRaw"] = kind, swap.AmountRaw, g.MinimumOutputRaw, value
			if evidence.Request.Close != nil {
				report["repayRaw"] = evidence.Request.Close.AmountRaw
			}
			compile = func(d publicKey) ([]byte, error) { return compileStepMessageForDelegate(&v, evidence.Request, d) }
		}
	default:
		err = fmt.Errorf("invalid offline action")
		return
	}
	message, e := compile(mustKey(backyardDelegate))
	if e != nil {
		err = e
		return
	}
	_, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		err = e
		return
	}
	delegate := publicKeyFromBytes(private.Public().(ed25519.PublicKey))
	local, e := compile(delegate)
	if e != nil {
		err = e
		return
	}
	signed, e := signMessage(private, delegate, local, hash.Blockhash, hash.LastValidBlockHeight)
	if e != nil {
		err = e
		return
	}
	built, e := signed.BuildResult(input.Slot)
	if e != nil {
		err = e
		return
	}
	if e = built.validateForDelegate(delegate); e != nil {
		err = e
		return
	}
	report["message"], report["persistSignValidated"], report["at"] = message, true, time.Now().UTC()
}
