package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Public Settings from the retained finalized slot-444491195 program snapshot.
// SDK decoding below is independent of the Go envelope parser.
const setupSettingsFixture = "37OjvrHgQ606sAcAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/AQAAAJcaJGJKF/Wed7NyrW3KxJvGybD8BjcCj1JwIF/Ul3wIBwABiwAAAAAAAAAA"

func setupSettingsAccount(t *testing.T) ConfirmedAccount {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(setupSettingsFixture)
	if err != nil {
		t.Fatal(err)
	}
	return ConfirmedAccount{Address: bridgeSettings, Owner: bridgeSquadsProgram, Lamports: 2_060_160, Data: data}
}

func TestPolicySetupSettingsMatchesSDKAndRejectsAuthorityDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "testdata/policy-settings-oracle.mjs")
	cmd.Stdin = strings.NewReader(setupSettingsFixture)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Settings SDK oracle: %v %s", err, stderr.String())
	}
	var cases []struct{ Name, Data, Next string }
	if json.Unmarshal(out, &cases) != nil || len(cases) != 16 {
		t.Fatal("invalid Settings oracle output")
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			a := setupSettingsAccount(t)
			a.Data, err = base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				t.Fatal(err)
			}
			next, decodeErr := policySetupNextSeed(a)
			if c.Next == "" {
				assertBudgetHold(t, decodeErr, "policy_setup_settings_envelope_mismatch")
			} else if decodeErr != nil || strconv.FormatUint(next, 10) != c.Next {
				t.Fatalf("Settings disagrees with SDK: seed=%d error=%v", next, decodeErr)
			}
		})
	}
	for name, mutate := range map[string]func(*ConfirmedAccount){
		"address":       func(a *ConfirmedAccount) { a.Address = bridgeVault },
		"owner":         func(a *ConfirmedAccount) { a.Owner = classicTokenProgram },
		"executable":    func(a *ConfirmedAccount) { a.Executable = true },
		"empty":         func(a *ConfirmedAccount) { a.Lamports = 0 },
		"discriminator": func(a *ConfirmedAccount) { a.Data[0] ^= 1 },
		"bad-option":    func(a *ConfirmedAccount) { a.Data[78] = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			a := setupSettingsAccount(t)
			mutate(&a)
			_, err := policySetupNextSeed(a)
			assertBudgetHold(t, err, "policy_setup_settings_envelope_mismatch")
		})
	}
	a := setupSettingsAccount(t)
	for n := 0; n < len(a.Data); n++ {
		truncated := a
		truncated.Data = a.Data[:n]
		if _, err := policySetupNextSeed(truncated); err == nil {
			t.Fatalf("truncated Settings accepted at %d bytes", n)
		}
	}
}

type setupRPCScenario struct {
	rent                uint64
	drift               string
	blockhash           string
	fees, settingsReads int
}

func setupObservationRPC(t *testing.T, s *setupRPCScenario) *RPCClient {
	t.Helper()
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpc.client.Transport
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal("invalid RPC request")
		}
		var result any
		handled := true
		switch body.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
			if s.drift == "genesis" {
				result = "other"
			}
		case "getSlot":
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &config)
			if config["commitment"] != "finalized" {
				t.Fatal("setup seed slot must be finalized")
			}
			result = 42
		case "getMinimumBalanceForRentExemption":
			var size int
			_ = json.Unmarshal(body.Params[0], &size)
			if size == 0 {
				result = uint64(890_880)
			} else if size == 1400 || size == 1250 {
				result = s.rent
			} else {
				t.Fatalf("unexpected allocation %d", size)
			}
		case "getMultipleAccounts":
			var addresses []string
			_ = json.Unmarshal(body.Params[0], &addresses)
			if addresses[0] != bridgeSettings {
				handled = false
				break
			}
			s.settingsReads++
			var config map[string]any
			_ = json.Unmarshal(body.Params[1], &config)
			commitment := "finalized"
			if s.settingsReads == 3 {
				commitment = "confirmed"
			}
			if config["commitment"] != commitment || config["minContextSlot"] != float64(42) {
				t.Fatalf("wrong setup read commitment/minimum: %+v", config)
			}
			settings := setupSettingsAccount(t)
			if s.drift == "settings" && s.settingsReads == 3 {
				settings.Data[56] = 2
			}
			values := []any{map[string]any{"owner": settings.Owner, "lamports": settings.Lamports, "data": []string{base64.StdEncoding.EncodeToString(settings.Data), "base64"}, "executable": false}}
			if len(addresses) == 3 {
				policy, _ := policySetupAddress(140)
				if addresses[1] != bridgeSettingsSigner || addresses[2] != encodeBase58(policy[:]) {
					t.Fatal("setup requested a different admin or target")
				}
				balance := uint64(1_000_000_000)
				if s.drift == "underfunded" {
					balance = 1
				}
				values = append(values, map[string]any{"owner": "11111111111111111111111111111111", "lamports": balance, "data": []string{"", "base64"}, "executable": false}, nil)
				if (s.drift == "target" || s.drift == "malformed-target") && s.settingsReads == 3 {
					owner := "11111111111111111111111111111111"
					if s.drift == "malformed-target" {
						owner = ""
					}
					values[2] = map[string]any{"owner": owner, "lamports": 890_880, "data": []string{"", "base64"}, "executable": false}
				}
			}
			slot := 42
			if s.drift == "stale" && s.settingsReads == 3 {
				slot = 75
			}
			result = map[string]any{"context": map[string]int{"slot": slot}, "value": values}
		case "getFeeForMessage":
			s.fees++
			handled = false
		case "getLatestBlockhash":
			if s.blockhash != "" {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": s.blockhash, "lastValidBlockHeight": 1234}}
			} else if s.drift == "blockhash" {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": "bad", "lastValidBlockHeight": 99}}
			} else {
				handled = false
			}
		default:
			handled = false
		}
		if !handled {
			return base.RoundTrip(request)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc
}

func TestPolicySetupObservationPrefersDirectAndPricesStagingFallback(t *testing.T) {
	for _, tc := range []struct {
		operation, mode string
		rent            uint64
		payments        int
	}{{"repay", "direct-create", 8_000_000, 1}, {"borrow", "prefund-then-create", 10_634_881, 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			s := &setupRPCScenario{rent: tc.rent}
			out, err := observePolicySetup(context.Background(), setupObservationRPC(t, s), tc.operation)
			if err != nil {
				t.Fatal(err)
			}
			if out.Mode != tc.mode || len(out.Payments) != tc.payments || s.fees != tc.payments || s.settingsReads != 3 || out.Request.Seed != 140 || out.FinalizedSettingsSlot != 42 || out.ProductionSetupAdmission {
				t.Fatalf("wrong setup selection: %+v %+v", out, s)
			}
			var rent uint64
			var total int64
			for _, cost := range out.Payments {
				rent += cost.SetupLamports
				total += cost.TotalMicros
				if cost.Fee.Lamports != 5000 || cost.TotalMicros > Phase3TransactionCapMicros {
					t.Fatal("uncapped or missing-fee payment")
				}
			}
			if rent != tc.rent || total != out.TotalMicros {
				t.Fatal("missing setup debit")
			}
			if tc.payments == 1 && out.CompletionReserveMicros != 0 {
				t.Fatal("unnecessary second payment")
			}
			if tc.payments == 2 && (out.CompletionReserveMicros != out.Payments[1].TotalMicros || total <= Phase3TransactionCapMicros) {
				t.Fatal("staged payment omitted completion reserve")
			}
		})
	}
}

func TestPolicySetupObservationRejectsChangedAuthorityTargetAndFunding(t *testing.T) {
	for drift, reason := range map[string]string{"genesis": "policy_setup_genesis_mismatch", "settings": "policy_setup_settings_changed", "target": "policy_setup_target_not_absent", "underfunded": "policy_setup_payer_underfunded", "stale": "policy_setup_valuation_expired", "blockhash": "policy_setup_blockhash_invalid"} {
		t.Run(drift, func(t *testing.T) {
			_, err := observePolicySetup(context.Background(), setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000, drift: drift}), "repay")
			assertBudgetHold(t, err, reason)
		})
	}
	_, err := observePolicySetup(context.Background(), setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000, drift: "malformed-target"}), "repay")
	if err == nil {
		t.Fatal("malformed present target adopted as absent")
	}
}
