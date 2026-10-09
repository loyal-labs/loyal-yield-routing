package autodeposit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type svmAccount struct {
	Address, Owner string
	Lamports       uint64
	Data           []byte
	Executable     bool
}
type svmAutodepositFixture struct {
	SchemaVersion                                                                                                        int
	Settings, Wallet, Vault, WalletATA, VaultATA, SubscriptionAuthority, RecurringDelegation, Policy, ExecutorSeedBase64 string
	PolicySeed, Nonce, BudgetRaw, AmountRaw, WalletBeforeRaw, PeriodLength, StartTimestamp, ExpiryTimestamp              int64
	Accounts                                                                                                             map[string]svmAccount
	SeedGapRejected                                                                                                      bool
	Provenance                                                                                                           struct{ SquadsProgramSha256, SubscriptionsProgramSha256, MockProgramSha256, InitializationRentProfile, Scope string }
	MockTopUp                                                                                                            struct {
		Scope, Reserve, Market, Obligation, RoutePolicy, LiquiditySupply, CollateralMint, CollateralSupply string
		RoutePolicySeed                                                                                    int64
	}
	PolicyCreator struct {
		Signature                 string
		Slot                      int64
		WireBase64                string
		PreLamports, PostLamports []uint64
		Logs                      []string
	}
}

func loadSVMAutodepositFixture(t *testing.T) svmAutodepositFixture {
	t.Helper()
	path := os.Getenv("AUTODEPOSIT_TEST_SVM_FIXTURE_PATH")
	helper := os.Getenv("AUTODEPOSIT_TEST_SVM_PATH")
	if path == "" && helper == "" {
		t.Skip("requires actual source-produced authorization fixture and local SVM helper")
	}
	if path == "" || helper == "" {
		t.Fatal("both AUTODEPOSIT_TEST_SVM_FIXTURE_PATH and AUTODEPOSIT_TEST_SVM_PATH are required")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var f svmAutodepositFixture
	if e = json.Unmarshal(data, &f); e != nil {
		t.Fatal(e)
	}
	if f.SchemaVersion != 1 || !f.SeedGapRejected || len(f.Accounts) < 10 || f.AmountRaw <= 0 || f.Provenance.InitializationRentProfile == "" {
		t.Fatal("SVM fixture provenance incomplete")
	}
	for _, program := range []struct{ name, defaultPath, want string }{{"SQUADS_SMART_ACCOUNT_PROGRAM_SO", "squads/squads_smart_account_program.so", f.Provenance.SquadsProgramSha256}, {"SUBSCRIPTIONS_PROGRAM_SO", "subscriptions/subscriptions_program.so", f.Provenance.SubscriptionsProgramSha256}} {
		path := os.Getenv(program.name)
		if path == "" {
			path = filepath.Join("../../../../crates/squads-test-harness/fixtures", program.defaultPath)
		}
		body, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != program.want {
			t.Fatalf("%s differs from source-produced fixture", program.name)
		}
	}
	mock, e := os.ReadFile(os.Getenv("MOCK_YIELD_PROTOCOLS_PROGRAM_SO"))
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(mock)
	if hex.EncodeToString(digest[:]) != f.Provenance.MockProgramSha256 || f.MockTopUp.RoutePolicySeed != 2 || f.MockTopUp.Scope == "" {
		t.Fatal("mock KLend SBF provenance or route policy incomplete")
	}
	return f
}

type autodepositSVM struct {
	mu        sync.Mutex
	input     io.WriteCloser
	output    *bufio.Reader
	server    *httptest.Server
	blockhash string
}

func startAutodepositSVM(t *testing.T, f svmAutodepositFixture) *autodepositSVM {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	command := exec.CommandContext(ctx, os.Getenv("AUTODEPOSIT_TEST_SVM_PATH"))
	command.Env = []string{"LC_ALL=C"}
	for _, name := range []string{"SQUADS_SMART_ACCOUNT_PROGRAM_SO", "SUBSCRIPTIONS_PROGRAM_SO", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO"} {
		if v := os.Getenv(name); v != "" {
			command.Env = append(command.Env, name+"="+v)
		}
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	input, e := command.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output, e := command.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = command.Start(); e != nil {
		t.Fatal(e)
	}
	svm := &autodepositSVM{input: input, output: bufio.NewReader(output)}
	t.Cleanup(func() {
		if svm.server != nil {
			svm.server.Close()
		}
		_ = input.Close()
		if e := command.Wait(); e != nil {
			t.Errorf("local SVM failed: %v\n%s", e, stderr.String())
		}
		cancel()
	})
	var initialized struct {
		Blockhash string
		Slot      int64
	}
	if e = svm.call("initialize", map[string]any{"accounts": f.Accounts}, &initialized); e != nil {
		t.Fatal(e)
	}
	if initialized.Slot != 1000 || initialized.Blockhash == "" {
		t.Fatal("invalid local initialization")
	}
	svm.blockhash = initialized.Blockhash
	svm.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		data, e := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if e != nil || json.Unmarshal(data, &request) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		// Existing local SVM serves exact base64 receipts. Production A uses
		// JSON account keys; convert only encoding, retaining actual SBF metadata.
		jsonReceipt := false
		if request["method"] == "getTransaction" {
			params := request["params"].([]any)
			config := params[1].(map[string]any)
			if config["encoding"] == "json" {
				jsonReceipt = true
				config["encoding"] = "base64"
				data, e = json.Marshal(request)
				if e != nil {
					http.Error(w, "invalid receipt request", 400)
					return
				}
			}
		}
		svm.mu.Lock()
		defer svm.mu.Unlock()
		if _, e = svm.input.Write(append(data, '\n')); e != nil {
			http.Error(w, "SVM input failed", 500)
			return
		}
		reply, e := svm.output.ReadBytes('\n')
		if e != nil {
			http.Error(w, "SVM output failed", 500)
			return
		}
		if jsonReceipt {
			var envelope map[string]any
			if json.Unmarshal(reply, &envelope) != nil {
				http.Error(w, "invalid SVM receipt", 500)
				return
			}
			if result, ok := envelope["result"].(map[string]any); ok {
				wire := result["transaction"].([]any)[0].(string)
				tx, err := solana.TransactionFromBase64(wire)
				if err != nil {
					http.Error(w, "invalid exact SVM wire", 500)
					return
				}
				keys := make([]string, len(tx.Message.AccountKeys))
				for i, key := range tx.Message.AccountKeys {
					keys[i] = key.String()
				}
				result["transaction"] = map[string]any{"message": map[string]any{"accountKeys": keys}}
				reply, e = json.Marshal(envelope)
				if e != nil {
					http.Error(w, "invalid SVM receipt encoding", 500)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	}))
	return svm
}
func (s *autodepositSVM) call(method string, params any, out any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if e != nil {
		return e
	}
	if _, e = s.input.Write(append(data, '\n')); e != nil {
		return e
	}
	reply, e := s.output.ReadBytes('\n')
	if e != nil {
		return e
	}
	var envelope struct {
		Result json.RawMessage
		Error  json.RawMessage
	}
	if e = json.Unmarshal(reply, &envelope); e != nil {
		return e
	}
	if len(envelope.Error) > 0 {
		return fmt.Errorf("local SVM rejected operation: %s", envelope.Error)
	}
	return json.Unmarshal(envelope.Result, out)
}
func svmPullPlan(f svmAutodepositFixture) DepositPlan {
	return DepositPlan{Version: 1, AmountRaw: f.AmountRaw, Reserve: f.MockTopUp.Reserve, Market: f.MockTopUp.Market, LiquidityMint: USDCMint, Target: DepositPlanTarget{ID: 1, ManagedVaultID: 1, Settings: f.Settings, VaultIndex: 1, Wallet: f.Wallet, WalletUsdcAta: f.WalletATA, WalletTokenAta: f.WalletATA, VaultPubkey: f.Vault, VaultUsdcAta: f.VaultATA, VaultTokenAta: f.VaultATA, TokenMint: USDCMint, SweepPolicyAccount: f.Policy, RoutePolicyAccount: f.MockTopUp.RoutePolicy, RoutePolicySeed: f.MockTopUp.RoutePolicySeed}}
}
func svmDurablePull(b BuiltWire, amount int64) DurableAttempt {
	return DurableAttempt{OperationKind: OperationPull, State: AttemptPrepared, AmountRaw: amount, Signature: b.Signature, SignedTransactionBase64: b.SignedTransactionBase64, SignedTransactionSHA256: b.SignedTransactionSHA256, RecentBlockhash: b.RecentBlockhash, LastValidBlockHeight: b.LastValidBlockHeight}
}

// svmChain is the production chain adapter over the local SVM's RPC.
func svmChain(t *testing.T, url string) *RPCChain {
	t.Helper()
	client, err := chain.New(url, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return NewRPCChain(client)
}

func TestSVMActualGoPullExecutesRealProgramsAndExactReceipt(t *testing.T) {
	f := loadSVMAutodepositFixture(t)
	svm := startAutodepositSVM(t, f)
	rpcChain := svmChain(t, svm.server.URL)
	seed, e := base64.StdEncoding.DecodeString(f.ExecutorSeedBase64)
	if e != nil || len(seed) != 32 {
		t.Fatal("invalid public test executor seed")
	}
	builder, e := NewSweepWireBuilder(ed25519.NewKeyFromSeed(seed), rpcChain.ReadAccounts)
	if e != nil {
		t.Fatal(e)
	}
	plan := svmPullPlan(f)
	route, e := builder.ConfirmTopUpRoute(t.Context(), plan)
	if e != nil {
		t.Fatalf("destination must be executable before the wallet pull: %v", e)
	}
	// The producer's canonical creator actually executed through real SBF. These
	// balances/signature are its real metadata, not a controlled RPC success.
	creatorWire, e := base64.StdEncoding.DecodeString(f.PolicyCreator.WireBase64)
	if e != nil {
		t.Fatal(e)
	}
	target := ArtifactTarget{ControlTarget: ControlTarget{Cluster: mainnetCluster, TargetID: 1, SetupGeneration: 1, PolicySeed: f.PolicySeed, Settings: f.Settings, Wallet: f.Wallet, WalletTokenATA: f.WalletATA, Vault: f.Vault, VaultTokenATA: f.VaultATA, Mint: USDCMint, Policy: f.Policy, SubscriptionAuthority: f.SubscriptionAuthority, RecurringDelegation: f.RecurringDelegation, Nonce: &f.Nonce, MaxAmountPerPeriod: &f.BudgetRaw, StartTimestamp: &f.StartTimestamp}, RootAuthority: f.Wallet, PeriodLength: &f.PeriodLength, ExpiryTimestamp: &f.ExpiryTimestamp}
	if e = builder.proveArtifactAccounts(t.Context(), target, 1000); e != nil {
		t.Fatalf("actual canonical source authorization accounts: %v", e)
	}
	if _, e = builder.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, solana.MustSignatureFromBase58(f.PolicyCreator.Signature), chain.Receipt{Slot: uint64(f.PolicyCreator.Slot), Wire: creatorWire, PreLamports: f.PolicyCreator.PreLamports, PostLamports: f.PolicyCreator.PostLamports}); e != nil {
		t.Fatalf("actual canonical creator receipt: %v", e)
	}
	nonce := uint64(f.Nonce)
	identity := DelegationIdentity{Account: f.RecurringDelegation, Delegator: f.Wallet, Delegatee: f.Vault, Mint: USDCMint, Nonce: &nonce, WalletTokenAccount: f.WalletATA}
	if allowance, err := rpcChain.RemainingDelegationAllowanceRaw(t.Context(), f.RecurringDelegation, identity); err != nil || allowance != f.BudgetRaw {
		t.Fatalf("actual allowance=%d err=%v", allowance, err)
	}
	nonce++
	if _, e = rpcChain.RemainingDelegationAllowanceRaw(t.Context(), f.RecurringDelegation, identity); e == nil {
		t.Fatal("frozen nonce mismatch admitted")
	}
	for _, tc := range []struct {
		name   string
		key    ed25519.PrivateKey
		amount int64
	}{{"unauthorized", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{20}, 32)), f.AmountRaw}, {"over-budget", ed25519.NewKeyFromSeed(seed), f.BudgetRaw + 1}} {
		t.Run(tc.name, func(t *testing.T) {
			b, e := NewSweepWireBuilder(tc.key, rpcChain.ReadAccounts)
			if e != nil {
				t.Fatal(e)
			}
			p := plan
			p.AmountRaw = tc.amount
			wire, e := b.BuildPull(t.Context(), PullWireRequest{Plan: p, RecurringDelegation: f.RecurringDelegation, RecentBlockhash: svm.blockhash, LastValidBlockHeight: 1150})
			if tc.name == "over-budget" && e != nil {
				t.Fatalf("budget test must reach the real Subscriptions program: %v", e)
			}
			if e == nil {
				e = rpcChain.SimulateExact(t.Context(), svmDurablePull(wire, tc.amount))
			}
			if e == nil {
				t.Fatal("unauthorized or over-budget pull passed actual program preflight")
			}
			if tc.name == "over-budget" {
				var actual struct {
					Value struct {
						Err  json.RawMessage
						Logs []string
					}
				}
				if err := svm.call("simulateTransaction", []any{wire.SignedTransactionBase64, map[string]any{"encoding": "base64", "sigVerify": true, "replaceRecentBlockhash": false}}, &actual); err != nil {
					t.Fatal(err)
				}
				if len(actual.Value.Err) == 0 || string(actual.Value.Err) == "null" || !strings.Contains(strings.Join(actual.Value.Logs, "\n"), "Program "+SubscriptionsProgramID+" invoke") {
					t.Fatalf("budget rejection must come from real Subscriptions CPI: err=%s logs=%v", actual.Value.Err, actual.Value.Logs)
				}
			}
			balance, e := rpcChain.ConfirmedTokenBalanceRaw(t.Context(), f.WalletATA, f.Wallet)
			if e != nil || balance != f.WalletBeforeRaw {
				t.Fatalf("failed preflight changed wallet: %d %v", balance, e)
			}
		})
	}
	wire, e := builder.BuildPull(t.Context(), PullWireRequest{Plan: plan, RecurringDelegation: f.RecurringDelegation, RecentBlockhash: svm.blockhash, LastValidBlockHeight: 1150})
	if e != nil {
		t.Fatal(e)
	}
	t.Run("actual-squads-unauthorized-signer", func(t *testing.T) {
		// An adversary can bypass the Go builder; the actual Squads program must
		// still reject a signed packet. This funded payer removes fee/rent as
		// an alternative reason for rejection.
		tx, err := solana.TransactionFromBase64(wire.SignedTransactionBase64)
		if err != nil {
			t.Fatal(err)
		}
		unauthorized := solana.PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{20}, 32)))
		for i, key := range tx.Message.AccountKeys {
			if key == builder.delegate {
				tx.Message.AccountKeys[i] = unauthorized.PublicKey()
			}
		}
		tx.Signatures = nil
		if _, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			if key == unauthorized.PublicKey() {
				return &unauthorized
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		packet, err := tx.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		var simulation struct {
			Value struct {
				Err  json.RawMessage
				Logs []string
			}
		}
		if err = svm.call("simulateTransaction", []any{base64.StdEncoding.EncodeToString(packet), map[string]any{"encoding": "base64", "sigVerify": true, "replaceRecentBlockhash": false}}, &simulation); err != nil {
			t.Fatal(err)
		}
		logs := strings.Join(simulation.Value.Logs, "\n")
		if len(simulation.Value.Err) == 0 || string(simulation.Value.Err) == "null" || !strings.Contains(logs, "Program "+squadsProgramID+" invoke") || strings.Contains(logs, "Program "+SubscriptionsProgramID+" invoke") {
			t.Fatalf("unauthorized signer must be rejected by real Squads before asset CPI: err=%s logs=%s", simulation.Value.Err, logs)
		}
		if balance, err := rpcChain.ConfirmedTokenBalanceRaw(t.Context(), f.WalletATA, f.Wallet); err != nil || balance != f.WalletBeforeRaw {
			t.Fatalf("adversarial simulation changed wallet: %d %v", balance, err)
		}
	})
	attempt := svmDurablePull(wire, f.AmountRaw)
	if e = rpcChain.SimulateExact(t.Context(), attempt); e != nil {
		t.Fatalf("actual Go pull simulation: %v", e)
	}
	if e = rpcChain.SendWire(t.Context(), svmWire(t, attempt), true); e != nil {
		t.Fatal(e)
	}
	observation, e := svmObserve(t.Context(), rpcChain, attempt)
	if e != nil || observation.State != AttemptConfirmed || observation.ConfirmedSlot == nil {
		t.Fatalf("actual confirmation=%+v %v", observation, e)
	}
	attempt.State = AttemptConfirmed
	attempt.ConfirmedSlot = observation.ConfirmedSlot
	receipt, e := rpcChain.ConfirmedReceipt(t.Context(), attempt.Signature)
	if e != nil {
		t.Fatal(e)
	}
	if e = validatePullReceipt(plan, attempt, receipt); e != nil {
		t.Fatalf("actual exact pull receipt: %v", e)
	}
	for _, expected := range []struct {
		account, owner string
		amount         int64
	}{{f.WalletATA, f.Wallet, f.WalletBeforeRaw - f.AmountRaw}, {f.VaultATA, f.Vault, f.AmountRaw}} {
		balance, e := rpcChain.ConfirmedTokenBalanceRaw(t.Context(), expected.account, expected.owner)
		if e != nil || balance != expected.amount {
			t.Fatalf("actual balance %s=%d want%d err=%v", expected.account, balance, expected.amount, e)
		}
	}
	if e = rpcChain.SendWire(t.Context(), svmWire(t, attempt), true); e != nil {
		t.Fatal(e)
	}
	balance, e := rpcChain.ConfirmedTokenBalanceRaw(t.Context(), f.VaultATA, f.Vault)
	if e != nil || balance != f.AmountRaw {
		t.Fatalf("exact retry executed twice: %d %v", balance, e)
	}
	tx, e := solana.TransactionFromBase64(wire.SignedTransactionBase64)
	if e != nil || tx.Signatures[0].String() != wire.Signature {
		t.Fatal("actual signed wire identity lost")
	}
	nonce = uint64(f.Nonce)
	if allowance, err := rpcChain.RemainingDelegationAllowanceRaw(t.Context(), f.RecurringDelegation, identity); err != nil || allowance != f.BudgetRaw-f.AmountRaw {
		t.Fatalf("actual post-pull allowance=%d err=%v", allowance, err)
	}
	topWire, e := builder.BuildTopUp(t.Context(), TopUpWireRequest{Plan: plan, RecentBlockhash: svm.blockhash, LastValidBlockHeight: 1150})
	if e != nil {
		t.Fatalf("official Go top-up builder: %v", e)
	}
	top := svmDurablePull(topWire, f.AmountRaw)
	top.OperationKind = OperationTopUp
	if e = builder.ProveTopUpWireContext(t.Context(), plan, top, route); e != nil {
		t.Fatalf("immutable official top-up wire: %v", e)
	}
	if e = rpcChain.SimulateExact(t.Context(), top); e != nil {
		t.Fatalf("actual official top-up simulation: %v", e)
	}
	if e = rpcChain.SendWire(t.Context(), svmWire(t, top), true); e != nil {
		t.Fatal(e)
	}
	topObservation, e := svmObserve(t.Context(), rpcChain, top)
	if e != nil || topObservation.State != AttemptConfirmed || topObservation.ConfirmedSlot == nil {
		t.Fatalf("actual top-up confirmation=%+v %v", topObservation, e)
	}
	top.State, top.ConfirmedSlot = AttemptConfirmed, topObservation.ConfirmedSlot
	if e = (&Controller{chain: rpcChain}).verifyTopUpEffects(t.Context(), plan, route, Settlement{Attempt: top}); e != nil {
		t.Fatalf("actual exact top-up receipt: %v", e)
	}
	if position, slot, err := rpcChain.ConfirmedVaultPositionRaw(t.Context(), plan, route, 0); err != nil || position != f.AmountRaw || slot != 1000 {
		t.Fatalf("actual mock obligation/collateral conversion=%d slot%d err=%v", position, slot, err)
	}
	if e = rpcChain.SendWire(t.Context(), svmWire(t, top), true); e != nil {
		t.Fatal(e)
	}
	for _, expected := range []struct {
		account, owner string
		amount         int64
	}{{f.WalletATA, f.Wallet, f.WalletBeforeRaw - f.AmountRaw}, {f.VaultATA, f.Vault, 0}, {f.MockTopUp.LiquiditySupply, route.Position.MarketAuthority, 1_000_000 + f.AmountRaw}} {
		balance, err := rpcChain.ConfirmedTokenBalanceRaw(t.Context(), expected.account, expected.owner)
		if err != nil || balance != expected.amount {
			t.Fatalf("actual post-top-up/retry balance %s=%d want%d err=%v", expected.account, balance, expected.amount, err)
		}
	}
	_, collateral, e := rpcChain.ReadAccounts(t.Context(), 0, []string{f.MockTopUp.CollateralMint, f.MockTopUp.CollateralSupply, USDCMint})
	if e != nil || len(collateral) != 3 {
		t.Fatalf("actual collateral readback: %v", e)
	}
	for _, account := range collateral {
		if account.Owner.String() != splTokenID {
			t.Fatalf("actual SPL owner changed: %s", account.Key)
		}
		switch account.Key.String() {
		case f.MockTopUp.CollateralMint:
			if len(account.Data) != 82 || binary.LittleEndian.Uint64(account.Data[36:44]) != uint64(1_000_000+f.AmountRaw) {
				t.Fatal("actual collateral mint supply did not increase exactly once")
			}
		case f.MockTopUp.CollateralSupply:
			if len(account.Data) != 165 || base58Key(account.Data[:32]) != f.MockTopUp.CollateralMint || binary.LittleEndian.Uint64(account.Data[64:72]) != uint64(1_000_000+f.AmountRaw) {
				t.Fatal("actual collateral destination did not credit exactly once")
			}
		case USDCMint:
			if len(account.Data) != 82 || binary.LittleEndian.Uint64(account.Data[36:44]) != uint64(f.WalletBeforeRaw+1_000_000) {
				t.Fatal("actual USDC total mint supply changed during pull/deposit")
			}
		}
	}
	t.Logf("official Go top-up through fixed-price KLend mock+real Squads/SPL: signature=%s slot=%d raw=%d; no KLend interest/oracle/mainnet correctness claim", top.Signature, *top.ConfirmedSlot, f.AmountRaw)
	t.Logf("real Squads+Subscriptions+SPL Go pull: signature=%s slot=%d raw=%d; fixture initialization rent profile retained, final financial execution uses default Rent", attempt.Signature, *attempt.ConfirmedSlot, f.AmountRaw)
}
