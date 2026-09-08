package backyardrwa

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This fixture is deliberately test-only. It describes the independently
// captured SDK/reserve graph, not a production registration or signing path.
func onreLendingParityRoute() RuntimeRoute {
	return RuntimeRoute{
		Lane: "OnRe/ONyc/USDC", Protocol: "OnRe", CollateralSymbol: "ONyc", DebtSymbol: "USDC",
		Kamino: KaminoObservationConfig{
			Program: kaminoProgram, Vault: bridgeVault,
			Market:            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
			MarketAuthority:   "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss",
			Obligation:        "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei",
			CollateralReserve: "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
			DebtReserve:       "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
			CollateralMint:    "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5", DebtMint: bridgeUSDC,
		},
		CollateralCustody: "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3", DebtCustody: bridgeSquadsATA,
		CollateralLiquiditySupply: "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG",
		CollateralReceiptMint:     "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd",
		CollateralReceiptSupply:   "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo",
		DebtLiquiditySupply:       "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
		DebtFeeReceiver:           "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
		CollateralTokenProgram:    classicTokenProgram, DebtTokenProgram: classicTokenProgram,
		DebtFarm:           "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF",
		ObligationDebtFarm: "nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD",
		KaminoPolicies:     map[kaminoPrimeUSDCLeg]kaminoPolicyBinding{},
	}
}

func TestOnReConnectedLendingMatchesGoWithoutRegistration(t *testing.T) {
	dir, name := os.Getenv("PHASE3_ONRE_PROBE_DIR"), os.Getenv("PHASE3_JUPITER_PROBE_RESULT")
	if dir == "" || name == "" {
		t.Skip("explicit connected OnRe execution required")
	}
	read := func(path string, target any) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, target) != nil {
			t.Fatal("invalid retained OnRe input", path, err)
		}
		return data
	}
	var plan struct {
		Lane, Delegate string
		Broadcast      bool
		Candidate      struct {
			Policies []struct{ Edge, Policy string }
		}
	}
	planBytes := read(filepath.Join(dir, "plan.json"), &plan)
	type capturedAccount struct {
		Address, DataBase64, DataSHA256 string
		Present                         bool
	}
	var result struct {
		PlanSHA256, SnapshotSHA256 string
		Broadcast, SignatureProof  bool
		LendingSteps               []struct {
			Leg, WireBase64, WireSHA256 string
			AmountRaw                   uint64
			Pass                        bool
			Before                      []capturedAccount
		}
	}
	read(filepath.Join(dir, name), &result)
	snapshot, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	route := onreLendingParityRoute()
	if err != nil || result.PlanSHA256 != sha256Bytes(planBytes) || result.SnapshotSHA256 != sha256Bytes(snapshot) ||
		plan.Lane != route.Lane || plan.Delegate != bridgeDelegate || plan.Broadcast || result.Broadcast || result.SignatureProof || len(result.LendingSteps) != 6 {
		t.Fatal("connected execution identity or scope mismatch")
	}
	var retained struct {
		Preflight struct {
			Bindings struct {
				Data struct {
					Lanes []struct {
						Lane       string
						Operations []struct {
							Operation, Policy string
							Accounts          KaminoPrimeUSDCAccounts
						}
					}
				}
			}
		}
	}
	read("../../../../docs/evidence/backyard-rwa-go/phase3/setup-feasibility-2026-09-04.json", &retained)
	accounts := map[string]KaminoPrimeUSDCAccounts{}
	policies := map[string]string{}
	for _, lane := range retained.Preflight.Bindings.Data.Lanes {
		if lane.Lane == route.Lane {
			for _, op := range lane.Operations {
				accounts[op.Operation], policies[op.Operation] = op.Accounts, op.Policy
			}
		}
	}
	for _, candidate := range plan.Candidate.Policies {
		switch candidate.Edge {
		case "OnRe/borrow":
			policies["borrow"] = candidate.Policy
		case "OnRe/repay":
			policies["repay"] = candidate.Policy
		}
	}
	for i, step := range result.LendingSteps {
		if step.Leg != []string{"deposit", "borrow", "funding-swap", "redeposit", "repay", "withdraw"}[i] || !step.Pass {
			t.Fatal("incomplete connected order")
		}
		if step.Leg == "funding-swap" {
			continue
		}
		t.Run(step.Leg, func(t *testing.T) {
			op, leg, action, disc := step.Leg, kaminoLegDeposit, OpenRouteStep, kaminoDepositCollateral
			switch step.Leg {
			case "redeposit":
				op = "deposit"
			case "borrow":
				leg, disc = kaminoLegBorrow, kaminoBorrowUSDC
			case "repay":
				leg, action, disc = kaminoLegRepay, DeleverRouteStep, kaminoRepayUSDC
			case "withdraw":
				leg, action, disc = kaminoLegWithdraw, DeleverRouteStep, kaminoWithdrawCollateral
			}
			policyHash := ""
			for _, a := range step.Before {
				if a.Address != policies[op] {
					continue
				}
				data, err := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
				if err != nil || !a.Present || sha256Bytes(data) != a.DataSHA256 {
					t.Fatal("policy capture mismatch")
				}
				policyHash = a.DataSHA256
			}
			if !validSHA256(policyHash) {
				t.Fatal("missing executed policy")
			}
			route.KaminoPolicies[leg] = kaminoPolicyBinding{policies[op], policyHash}
			request := KaminoPrimeUSDCRequest{Action: action, AmountRaw: step.AmountRaw,
				Policy: policies[op], PolicyAccountDataSHA256: policyHash, Accounts: accounts[op],
				RouteLane: route.Lane, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
				Data: append(append([]byte{}, disc...), make([]byte, 8)...)}
			binary.LittleEndian.PutUint64(request.Data[8:], step.AmountRaw)
			if step.Leg == "redeposit" {
				request.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
			}
			message, err := compileResolvedKaminoMessage(request, mustKey(bridgeDelegate), route)
			wire, decodeErr := base64.StdEncoding.Strict().DecodeString(step.WireBase64)
			if err != nil || decodeErr != nil || len(wire) <= 65 || len(wire) > solanaPacketBytes || wire[0] != 1 || !allZero(wire[1:65]) || sha256Bytes(wire) != step.WireSHA256 {
				t.Fatalf("Go differs from executed %s wire: Go=%d SBF=%d err=%v", step.Leg, len(message), len(wire)-65, err)
			}
			// web3.js inserts program keys before account metas; Go inserts them
			// after. Compare every resolved instruction, key privilege, payer and
			// blockhash, not those equivalent readonly-key index assignments.
			if !reflect.DeepEqual(onreResolvedMessage(t, message), onreResolvedMessage(t, wire[65:])) {
				t.Fatal("Go message semantics differ from actual-SBF executed SDK wire")
			}
			for _, mutate := range []func([]byte){
				func(m []byte) { m[2]-- },                 // readonly account promoted to writable
				func(m []byte) { m[4] ^= 1 },              // sole signer identity
				func(m []byte) { m[4+int(m[3])*32] ^= 1 }, // blockhash
				func(m []byte) { m[len(m)-1] ^= 1 },       // inner amount
			} {
				mutant := append([]byte(nil), message...)
				mutate(mutant)
				if reflect.DeepEqual(onreResolvedMessage(t, mutant), onreResolvedMessage(t, wire[65:])) {
					t.Fatal("parity comparison concealed a changed privilege, signer, blockhash or amount")
				}
			}
			// Production stays closed; neither compiler nor signer resolves OnRe.
			if _, err := CompileKaminoMessage(request); err == nil {
				t.Fatal("OnRe became production enabled")
			}
			for index := range request.Accounts {
				for _, field := range []string{"address", "signer", "writable"} {
					mutant := request
					mutant.Accounts = append(KaminoPrimeUSDCAccounts(nil), request.Accounts...)
					a := &mutant.Accounts[index]
					switch field {
					case "address":
						a.Address = bridgeSettings
					case "signer":
						a.Signer = !a.Signer
					case "writable":
						a.Writable = !a.Writable
					}
					if _, err := compileResolvedKaminoMessage(mutant, mustKey(bridgeDelegate), route); err == nil {
						t.Fatalf("accepted account %d %s mutation", index, field)
					}
				}
			}
			for _, mutate := range []func(*KaminoPrimeUSDCRequest){
				func(r *KaminoPrimeUSDCRequest) { r.RouteLane = SelectedRouteID },
				func(r *KaminoPrimeUSDCRequest) { r.Policy = bridgeAllocationPolicy },
				func(r *KaminoPrimeUSDCRequest) { r.PolicyAccountDataSHA256 = strings.Repeat("0", 64) },
				func(r *KaminoPrimeUSDCRequest) { r.AmountRaw++ },
				func(r *KaminoPrimeUSDCRequest) { r.PolicyConstraintIndex = 1 },
				func(r *KaminoPrimeUSDCRequest) { r.ObligationReserves = []string{route.Kamino.DebtReserve} },
			} {
				mutant := request
				mutate(&mutant)
				if _, err := compileResolvedKaminoMessage(mutant, mustKey(bridgeDelegate), route); err == nil {
					t.Fatal("accepted request boundary mutation")
				}
			}
		})
	}
	if _, err := runtimeRoute(route.Lane); err == nil {
		t.Fatal("test installed OnRe")
	}
}

func onreResolvedMessage(t *testing.T, message []byte) any {
	t.Helper()
	if len(message) < 4 || message[0] != 1 || message[1] != 0 {
		t.Fatal("invalid single-payer header")
	}
	offset := 3
	count, err := decodeShortVec(message, &offset)
	if err != nil || count < 1 || int(message[2]) >= count || offset+count*32+32 >= len(message) {
		t.Fatal("invalid message keys")
	}
	keys := make([]publicKey, count)
	privileges := map[publicKey][2]bool{}
	for i := range keys {
		copy(keys[i][:], message[offset:offset+32])
		offset += 32
		if _, exists := privileges[keys[i]]; exists {
			t.Fatal("duplicate message key")
		}
		privileges[keys[i]] = [2]bool{i == 0, i < count-int(message[2])}
	}
	blockhash := append([]byte(nil), message[offset:offset+32]...)
	offset += 32
	n, err := decodeShortVec(message, &offset)
	if err != nil || n != 4 {
		t.Fatal("not exact four-instruction lending packet")
	}
	instructions := make([]decodedLegacyInstruction, n)
	for i := range instructions {
		instructions[i], offset, err = decodeLegacyInstruction(message, offset, keys)
		if err != nil {
			t.Fatal(err)
		}
		instructions[i].accountIndexes = nil // identities and order retained in accounts
	}
	if offset != len(message) {
		t.Fatal("trailing message bytes")
	}
	return struct {
		Payer        publicKey
		Blockhash    []byte
		Privileges   map[publicKey][2]bool
		Instructions []decodedLegacyInstruction
	}{keys[0], blockhash, privileges, instructions}
}
