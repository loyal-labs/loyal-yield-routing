package backyard

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
)

// In-memory RPC only. The expected commitment is asserted at the actual native
// request boundary, so changing the origin reader to confirmed breaks the test.
func topupLoanRPC(t *testing.T, accounts []ConfirmedAccount, commitment string) *RPCClient {
	t.Helper()
	client, err := NewRPCClient("https://topup.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = 0
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var call struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(body, &call) != nil {
			t.Fatal("invalid RPC request")
		}
		optionsIndex := 0
		if call.Method == "getMultipleAccounts" {
			optionsIndex = 1
		}
		var options struct {
			Commitment     string
			MinContextSlot int64
		}
		if len(call.Params) <= optionsIndex || json.Unmarshal(call.Params[optionsIndex], &options) != nil || (commitment != "" && options.Commitment != commitment) || (options.Commitment != "confirmed" && options.Commitment != "finalized") {
			t.Fatal("wrong RPC commitment", call.Method, options.Commitment)
		}
		slot := int64(binary.LittleEndian.Uint64(accountAt(accounts, budgetClockAddress).Data[:8]))
		var result any = slot
		if call.Method == "getMultipleAccounts" {
			if options.MinContextSlot <= 0 || options.MinContextSlot > slot {
				t.Fatal("invalid minimum context slot", options.MinContextSlot, slot)
			}
			var addresses []string
			if json.Unmarshal(call.Params[0], &addresses) != nil {
				t.Fatal("invalid account set")
			}
			var values []any
			for _, address := range addresses {
				a := accountAt(accounts, address)
				if a.Address == "" {
					t.Fatal("unexpected missing fixture account", address)
				}
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": a.Executable, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			result = map[string]any{"context": map[string]any{"slot": slot}, "value": values}
		} else if call.Method != "getSlot" {
			t.Fatal("unexpected RPC method", call.Method)
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		return response(string(encoded)), nil
	})
	return client
}

func TestTopupPrincipalOriginDoesNotRenewAnExpiredAttempt(t *testing.T) {
	loan, accounts := topupLoanFixture(t)
	route := autoAUTOPYUSD
	initial, err := decodeKaminoPayoffWindow(accounts, route, 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	plan := phase3BridgeAdmission{DepositProjection: &phase3KaminoProjection{}, Payoff: &initial,
		Snapshot: Snapshot{RouteLane: route.Lane, PositionCollateralRaw: int64(loan.CollateralRaw), PositionDebtRaw: int64(initial.ObservedDebtRaw)}}
	request := KaminoPrimeUSDCRequest{RouteLane: route.Lane}
	rpc := topupLoanRPC(t, accounts, "confirmed")
	clock := accountAt(accounts, budgetClockAddress)
	binary.LittleEndian.PutUint64(clock.Data[:8], 43)
	binary.LittleEndian.PutUint64(clock.Data[32:40], uint64(initial.ThroughUnix+1))
	before := loan
	if err := loan.validatePrincipal(accounts, route, 43); err != nil {
		t.Fatal("immutable principal was tied to attempt expiry", err)
	}
	if _, err := validateRedepositAdmissionPrestate(context.Background(), rpc, request, &plan, 43); err == nil {
		t.Fatal("principal proof renewed an expired executable attempt")
	}
	fresh, err := decodeKaminoPayoffWindow(accounts, route, 43, 3)
	if err != nil {
		t.Fatal(err)
	}
	plan.Payoff = &fresh
	if _, err := validateRedepositAdmissionPrestate(context.Background(), rpc, request, &plan, 43); err != nil {
		t.Fatal("fresh bounded attempt rejected unchanged principal", err)
	}
	if loan != before {
		t.Fatal("fresh attempt rebased immutable origin")
	}
}

func TestTopupFinalizedLoanOriginRejectsUnreviewedProgram(t *testing.T) {
	_, accounts := topupLoanFixture(t)
	pin := reviewedTopupKaminoIdentity()
	binary.LittleEndian.PutUint64(accountAt(accounts, budgetClockAddress).Data[:8], uint64(pin.deploySlot+1))
	accounts = append(accounts,
		ConfirmedAccount{Address: pin.program, Owner: bpfLoaderProgramOwner, Lamports: 1, Data: make([]byte, programHeaderLength)},
		ConfirmedAccount{Address: pin.programData, Owner: bpfLoaderProgramOwner, Lamports: 1, Data: make([]byte, programDataExecutableOffset)})
	if _, err := observeTopupLoanOrigin(context.Background(), topupLoanRPC(t, accounts, "finalized")); err == nil {
		t.Fatal("unreviewed program granted timestamp capability")
	}
}

// The public 10MiB deployed ProgramData is deliberately not checked into tests.
// Supply the reviewed offline artifact to exercise the real full hash pin.
func topupReviewedProgramAccounts(t *testing.T) []ConfirmedAccount {
	t.Helper()
	path := os.Getenv("BACKYARD_KLEND_PROGRAMDATA_FIXTURE")
	if path == "" {
		t.Skip("set BACKYARD_KLEND_PROGRAMDATA_FIXTURE to the reviewed offline ProgramData; no RPC is used")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, base := topupLoanFixture(t)
	pin := reviewedTopupKaminoIdentity()
	slot := pin.deploySlot + 1
	binary.LittleEndian.PutUint64(accountAt(base, budgetClockAddress).Data[:8], uint64(slot))
	for _, address := range []string{autoAUTOPYUSD.Kamino.Obligation, autoAUTOPYUSD.Kamino.DebtReserve, autoAUTOPYUSD.Kamino.CollateralReserve} {
		binary.LittleEndian.PutUint64(accountAt(base, address).Data[16:24], uint64(slot))
	}
	program := ConfirmedAccount{Address: pin.program, Owner: bpfLoaderProgramOwner, Executable: true, Lamports: 1, Data: make([]byte, programHeaderLength)}
	binary.LittleEndian.PutUint32(program.Data[:4], programAccountDiscriminant)
	putKey(t, program.Data[4:36], pin.programData)
	base = append(base, program, ConfirmedAccount{Address: pin.programData, Owner: bpfLoaderProgramOwner, Lamports: 1, Data: data})
	return base
}

func TestTopupFinalizedLoanOriginReviewedImage(t *testing.T) {
	base := topupReviewedProgramAccounts(t)
	pin := reviewedTopupKaminoIdentity()
	slot := pin.deploySlot + 1
	loan, err := observeTopupLoanOrigin(context.Background(), topupLoanRPC(t, base, "finalized"))
	if err != nil || loan.ObservedSlot != slot || loan.BorrowedAtUnix != 999 {
		t.Fatal("reviewed finalized origin failed", err)
	}
	for name, mutate := range map[string]func([]ConfirmedAccount){
		"program data pointer":    func(a []ConfirmedAccount) { putKey(t, accountAt(a, pin.program).Data[4:36], bridgeVault) },
		"program discriminant":    func(a []ConfirmedAccount) { a[len(a)-2].Data[0]++ },
		"program owner":           func(a []ConfirmedAccount) { a[len(a)-2].Owner = classicTokenProgram },
		"executable program data": func(a []ConfirmedAccount) { a[len(a)-1].Executable = true },
		"program data owner":      func(a []ConfirmedAccount) { a[len(a)-1].Owner = classicTokenProgram },
		"deploy slot": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, pin.programData).Data[4:12], uint64(pin.deploySlot+1))
		},
		"image":              func(a []ConfirmedAccount) { accountAt(a, pin.programData).Data[programDataExecutableOffset] ^= 1 },
		"allocation padding": func(a []ConfirmedAccount) { d := accountAt(a, pin.programData).Data; d[len(d)-1] ^= 1 },
		"same timestamp origin": func(a []ConfirmedAccount) {
			binary.LittleEndian.PutUint64(accountAt(a, autoAUTOPYUSD.Kamino.Obligation).Data[1288:1296], 1000)
		},
		"clock owner": func(a []ConfirmedAccount) {
			for i := range a {
				if a[i].Address == budgetClockAddress {
					a[i].Owner = classicTokenProgram
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			accounts := append([]ConfirmedAccount(nil), base...)
			for i := range accounts {
				accounts[i].Data = append([]byte(nil), accounts[i].Data...)
			}
			mutate(accounts)
			if _, err := observeTopupLoanOrigin(context.Background(), topupLoanRPC(t, accounts, "finalized")); err == nil {
				t.Fatal("unproven origin accepted")
			}
		})
	}
}
