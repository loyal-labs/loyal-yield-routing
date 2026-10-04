package multiply

// This proof executes current Go Worker.Tick against real Squads/SPL and the
// explicitly fixed-price mock KLend. It is not mature KLend/oracle acceptance.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

type multiplySVMBank struct {
	mu  sync.Mutex
	in  io.WriteCloser
	out *bufio.Scanner
	cmd *exec.Cmd
}

func (b *multiplySVMBank) call(method string, params any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := b.in.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	if !b.out.Scan() {
		return nil, fmt.Errorf("SVM response ended: %v", b.out.Err())
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if err := json.Unmarshal(b.out.Bytes(), &r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("actual SVM %s: %v", method, r.Error)
	}
	return r.Result, nil
}

type multiplySVMFixture struct {
	store                                        *Store
	state                                        *RouteState
	topology                                     *EarnMaxTopology
	bank                                         *multiplySVMBank
	rpc                                          *LiveRPCSurface
	reader                                       *LiveObservationReader
	executor                                     *Executor
	swapPrefix                                   []byte
	mu                                           sync.Mutex
	lostResponse, receiptMissing, corruptReceipt bool
	staleObservation                             bool
	sends                                        int
	ctx                                          context.Context
}

func newMultiplySVMFixture(t *testing.T) *multiplySVMFixture {
	return newMultiplySVMFixtureWithInitialAccounts(t, nil)
}

func newMultiplySVMFixtureWithInitialAccounts(t *testing.T, initialize func(*EarnMaxTopology, map[string]*Account)) *multiplySVMFixture {
	t.Helper()
	paths := []string{os.Getenv("MULTIPLY_SVM_BIN"), os.Getenv("MULTIPLY_FIXTURE_BIN"), os.Getenv("MULTIPLY_MOCK_PROGRAM")}
	if paths[0] == "" && paths[1] == "" && paths[2] == "" {
		t.Skip("explicit Multiply real Squads/mock KLend fixture executables not configured")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			t.Fatal("all configured SVM fixture paths must be absolute")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	store := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	seed := uint64(time.Now().UnixNano())
	var seedBytes [16]byte
	binary.LittleEndian.PutUint64(seedBytes[:8], seed)
	settings, _, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("settings"), seedBytes[:]}, mustKey(SquadsProgram))
	if err != nil {
		t.Fatal(err)
	}
	topology, err := DeriveEarnMaxTopology(settings, 1)
	if err != nil {
		t.Fatal(err)
	}
	accounts := multiplyInitialAccounts(t, topology)
	if initialize != nil {
		initialize(topology, accounts)
	}
	dir := t.TempDir()
	inputPath, outputPath := filepath.Join(dir, "bank-input.json"), filepath.Join(dir, "bank-output.json")
	input, err := json.Marshal(map[string]any{"smartAccountSeed": seed, "settings": settings.String(), "catalog": topology.StrategyCatalog(), "accounts": accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	producer := exec.CommandContext(ctx, paths[1], "--exact", "bank::export_current_go_multiply_initial_bank", "--nocapture")
	producer.Dir = "../../../.."
	producer.Env = []string{"PATH=/usr/bin:/bin", "WORKERS_V2_MULTIPLY_BANK_INPUT=" + inputPath, "WORKERS_V2_MULTIPLY_BANK_OUTPUT=" + outputPath}
	if output, err := producer.CombinedOutput(); err != nil {
		t.Fatalf("real Squads policy fixture: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Settings, Vault, Delegate, ProgramSHA256 string
		Accounts                                 map[string]*Account
		SwapPrefix                               []byte
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile("../../../../crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(program)
	if exported.ProgramSHA256 != hex.EncodeToString(hash[:]) || exported.Settings != settings.String() || exported.Vault != topology.Vault.String() || exported.Delegate != solana.PublicKey(testDelegateSeed()[32:]).String() {
		t.Fatal("independent Squads fixture identity/provenance mismatch")
	}
	cmd := exec.CommandContext(ctx, paths[0])
	cmd.Env = []string{"PATH=/usr/bin:/bin", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO=" + paths[2]}
	cmd.Dir = "../../../.."
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	bank := &multiplySVMBank{in: in, out: bufio.NewScanner(out), cmd: cmd}
	bank.out.Buffer(make([]byte, 4096), 1<<24)
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if _, err := bank.call("initialize", map[string]any{"accounts": exported.Accounts}); err != nil {
		t.Fatalf("bank initialization: %v %s", err, stderr.String())
	}
	f := &multiplySVMFixture{store: store, topology: topology, bank: bank, ctx: ctx, swapPrefix: exported.SwapPrefix}
	server := httptest.NewServer(http.HandlerFunc(f.serveRPC))
	t.Cleanup(server.Close)
	f.rpc = NewLiveRPCSurface(server.URL)
	f.reader, err = NewLiveObservationReader(f.rpc)
	if err != nil {
		t.Fatal(err)
	}
	// The local bank truthfully reports its local genesis. This proves worker
	// execution, not mainnet identity; the production constructor must refuse it.
	if _, err := NewExecutorWithFeePayerContext(ctx, f.rpc, testDelegateSeed(), testDelegateSeed()); err == nil {
		t.Fatal("local SVM pretended to be mainnet")
	}
	f.executor = &Executor{RPC: f.rpc, Signer: testDelegateSeed(), feePayer: testDelegateSeed()}
	for _, item := range []struct {
		family PolicyFamily
		policy PolicyConfig
	}{{FamilyCollateral, topology.Strategies[SyrupUsdcUsdc].CollateralPolicy}, {FamilyDebt, topology.Strategies[SyrupUsdcUsdc].DebtPolicy}, {FamilySwap, topology.Strategies[SyrupUsdcUsdc].SwapPolicy}} {
		data := exported.Accounts[item.policy.Account.String()].Data
		expected, err := CanonicalConstraints(topology, item.family)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := CurrentPolicyMatches(data, item.policy, solana.PublicKey(testDelegateSeed()[32:]), expected, 0); err != nil || !ok {
			t.Fatalf("actual independent Squads %s policy refused: %v %v", item.family, ok, err)
		}
		wrong := append([]byte(nil), data...)
		wrong[0] ^= 1
		if ok, err := CurrentPolicyMatches(wrong, item.policy, solana.PublicKey(testDelegateSeed()[32:]), expected, 0); err == nil && ok {
			t.Fatal("incorrect actual policy discriminator accepted")
		}
	}
	before, err := ObserveConfirmed(ctx, f.reader, topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.state, err = NewRouteState(routeKeyFor(settings, 0), settings.String(), 0, topology.Vault.String(), 1, before.Claim, before.Slot, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.state.Goal = GoalDeploy
	if _, err := store.Pool().Exec(ctx, `INSERT INTO loyal_yield.earn_max_policy_sets(settings,vault_index,vault,manifest_version,manifest_sha256,status,policy_accounts,observed_signature,observed_slot,observed_at,policy_seed_base) VALUES($1,0,$2,'earn-max-v2',$3,'ready','[]','actual-svm-initial-policy',1000,now(),1)`, settings.String(), topology.Vault.String(), PolicyDataHash(raw)); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.CreateRouteState(ctx, f.state); err != nil || !ok {
		t.Fatalf("create actual bank route %v %v", ok, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// Failed admission may leave an unsigned prepared attempt. Cancel only
		// through its real fence; immutable signed/evidence rows remain intact.
		saved, loadErr := store.LoadRouteState(cleanup, f.state.RouteKey)
		if loadErr == nil && saved != nil && saved.Operation != nil {
			lease, leaseErr := store.LeaseRoute(cleanup, f.state.RouteKey, "svm-fixture-cleanup", time.Now().Add(time.Minute))
			if leaseErr != nil {
				t.Error(leaseErr)
			} else if lease != nil {
				next := *saved.State
				next.Generation++
				next.CurrentOperationID = nil
				next.Goal = GoalIdle
				if saved.Operation.Status == StatusPrepared {
					if ok, err := store.CancelPreparedOperation(cleanup, lease, saved.Operation.OperationID, &next); err != nil || !ok {
						t.Errorf("fenced prepared cleanup: %v %v", ok, err)
					}
				} else {
					next.Goal = GoalManualRecovery
					reason := "failed task-owned local bank fixture; retained signed audit"
					next.ManualRecoveryReason = &reason
					if ok, err := store.MarkManualRecovery(cleanup, lease, saved.Operation.OperationID, &next); err != nil || !ok {
						t.Errorf("fenced signed cleanup: %v %v", ok, err)
					}
				}
				_, _ = store.ReleaseLease(cleanup, lease)
			}
		}
		_, err := store.Pool().Exec(cleanup, `DELETE FROM loyal_yield.earn_max_policy_sets WHERE settings=$1`, settings.String())
		if err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *multiplySVMFixture) serveRPC(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID     uint64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<22)).Decode(&q); err != nil {
		http.Error(w, "fixture request invalid", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.Method == "sendTransaction" {
		// Independently inspect durable publication before allowing bank execution.
		saved, err := f.store.LoadRouteState(r.Context(), f.state.RouteKey)
		if err != nil || saved == nil || saved.Operation == nil || saved.Operation.Status != StatusBroadcastIntent || len(saved.Operation.SignedWire) == 0 {
			http.Error(w, "bank send preceded durable intent/wire", 500)
			return
		}
		if prestate, err := f.store.LoadOperationPrestate(r.Context(), saved.Operation); err != nil || prestate == nil {
			http.Error(w, "bank send preceded immutable prestate", 500)
			return
		}
	}
	result, err := f.bank.call(q.Method, q.Params)
	if q.Method == "sendTransaction" {
		f.sends++
		if err == nil && f.lostResponse {
			f.lostResponse = false
			err = errors.New("test transport lost actual bank response")
		}
	}
	if q.Method == "getTransaction" && f.receiptMissing {
		result = json.RawMessage("null")
	}
	if q.Method == "getTransaction" && f.corruptReceipt && err == nil && string(result) != "null" {
		var receipt map[string]any
		_ = json.Unmarshal(result, &receipt)
		meta := receipt["meta"].(map[string]any)
		meta["postTokenBalances"] = []any{} // adversarial incomplete provider metadata
		result, _ = json.Marshal(receipt)
	}
	if q.Method == "getMultipleAccounts" && f.staleObservation && err == nil {
		var observed map[string]any
		_ = json.Unmarshal(result, &observed)
		observed["context"].(map[string]any)["slot"] = 999
		result, _ = json.Marshal(observed)
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
}

func (f *multiplySVMFixture) worker(t *testing.T, keyless bool) *Worker {
	t.Helper()
	executor := f.executor
	if keyless {
		executor = &Executor{RPC: f.rpc}
	} // real absence of both private keys
	deps := WorkerDeps{Store: f.store, Observer: f.reader, Executor: executor, WorkerID: "svm-go-owner", RouteKey: &f.state.RouteKey}
	if !keyless {
		deps.Quotes = multiplyBankQuoteClient{f}
	}
	var worker *Worker
	var err error
	if keyless {
		worker, err = NewRecoveryWorker(deps)
	} else {
		worker, err = NewWorker(deps)
	}
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func multiplyInitialAccounts(t *testing.T, topology *EarnMaxTopology) map[string]*Account {
	t.Helper()
	accounts := emptyBankReader(topology).accounts
	// This source-layout initial bank intentionally has no accrued interest or
	// oracle valuation claim. Actual token/obligation changes come from execution.
	for _, config := range topology.StrategyCatalog() {
		for _, reserve := range []solana.PublicKey{config.CollateralReserve, config.DebtReserve} {
			binary.LittleEndian.PutUint64(accounts[reserve.String()].Data[16:24], 1000)
		}
	}
	config := topology.Strategies[SyrupUsdcUsdc]
	accounts[topology.ClaimCustody.String()] = observedToken(topology.ClaimCustody, USDCMint, topology.Vault, 0)
	for _, key := range []solana.PublicKey{config.Market, config.MarketAuthority, config.Oracle} {
		accounts[key.String()] = &Account{Address: key.String(), Owner: KlendProgram, Lamports: 10_000_000, Data: []byte{0}}
	}
	obligation := make([]byte, obligationLength)
	copy(obligation[:8], obligationDiscriminator)
	copy(obligation[32:64], config.Market[:])
	copy(obligation[64:96], topology.Vault[:])
	binary.LittleEndian.PutUint64(obligation[16:24], 1000)
	accounts[config.Obligation.String()] = &Account{Address: config.Obligation.String(), Owner: KlendProgram, Lamports: 10_000_000, Data: obligation}
	reserve := accounts[config.CollateralReserve.String()]
	copy(reserve.Data[160:192], config.CollateralLiquiditySupply[:])
	copy(reserve.Data[2560:2592], config.CollateralReceiptMint[:])
	copy(reserve.Data[2600:2632], config.CollateralMintSupply[:])
	reserve.Data[reserveConfigOffset+16], reserve.Data[reserveConfigOffset+17] = 65, 80
	// The fixed-price mock and this source integer exchange ratio are 1:1.
	binary.LittleEndian.PutUint64(reserve.Data[224:232], 1_000_000_000)
	accounts[config.CollateralCustody.String()] = observedToken(config.CollateralCustody, config.CollateralMint, topology.Vault, 1_000_000)
	accounts[config.CollateralLiquiditySupply.String()] = observedToken(config.CollateralLiquiditySupply, config.CollateralMint, config.MarketAuthority, 1_000_000_000)
	accounts[config.CollateralMintSupply.String()] = observedToken(config.CollateralMintSupply, config.CollateralReceiptMint.String(), config.MarketAuthority, 1_000_000_000)
	debtReserve := accounts[config.DebtReserve.String()]
	copy(debtReserve.Data[160:192], config.DebtLiquiditySupply[:])
	copy(debtReserve.Data[192:224], config.DebtFeeVault[:])
	if config.DebtFarmState != nil {
		copy(debtReserve.Data[96:128], config.DebtFarmState[:])
	}
	accounts[config.DebtLiquiditySupply.String()] = observedToken(config.DebtLiquiditySupply, USDCMint, config.MarketAuthority, 1_000_000_000)
	accounts[config.DebtFeeVault.String()] = observedToken(config.DebtFeeVault, USDCMint, config.MarketAuthority, 0)
	if config.DebtFarmState != nil {
		accounts[config.DebtFarmState.String()] = &Account{Address: config.DebtFarmState.String(), Owner: FarmsProgram, Data: []byte{0}, Lamports: 10_000_000}
	}
	if config.DebtFarmUser != nil {
		accounts[config.DebtFarmUser.String()] = &Account{Address: config.DebtFarmUser.String(), Owner: FarmsProgram, Data: []byte{0}, Lamports: 10_000_000}
	}
	for _, mint := range []solana.PublicKey{mustKey(USDCMint), mustKey(config.CollateralMint)} {
		pool, err := DeriveAssociatedTokenAccount(multiplyBankSwapAuthority(), mint, mustKey(TokenProgram))
		if err != nil {
			t.Fatal(err)
		}
		accounts[pool.String()] = observedToken(pool, mint.String(), multiplyBankSwapAuthority(), 1_000_000_000)
	}
	accounts[fixtureKey(120).String()] = observedToken(fixtureKey(120), USDCMint, fixtureKey(121), 0)
	for _, mint := range []solana.PublicKey{mustKey(USDCMint), mustKey(config.CollateralMint), config.CollateralReceiptMint} {
		data := make([]byte, 82)
		binary.LittleEndian.PutUint32(data[:4], 1)
		copy(data[4:36], config.MarketAuthority[:])
		binary.LittleEndian.PutUint64(data[36:44], 1_000_000_000)
		data[44], data[45] = 6, 1
		accounts[mint.String()] = &Account{Address: mint.String(), Owner: TokenProgram, Lamports: 10_000_000, Data: data}
	}
	return accounts
}

func multiplyBankSwapAuthority() solana.PublicKey {
	key, _, err := solana.FindProgramAddress([][]byte{[]byte("jupiter-swap-authority")}, mustKey(JupiterProgram))
	if err != nil {
		panic(err)
	}
	return key
}

// Official v1 SharedAccountsRoute layout; only the local fixed1:1 bank's
// two source-pinned mints are quoted. No live price/provider/DEX is involved.
type multiplyBankQuoteClient struct{ f *multiplySVMFixture }

func (c multiplyBankQuoteClient) FetchQuote(ctx contextT, request QuoteRequest) (*QuoteResponse, error) {
	if !((request.InputMint == USDCMint && request.OutputMint == syrupMint) || (request.OutputMint == USDCMint && request.InputMint == syrupMint)) {
		return nil, errors.New("local model has no supported mint pair")
	}
	var slot uint64
	raw, err := c.f.bank.call("getSlot", []any{})
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &slot); err != nil {
		return nil, err
	}
	minimum := (request.Amount*9950 + 9999) / 10000
	return &QuoteResponse{InputMint: request.InputMint, OutputMint: request.OutputMint, SwapMode: "ExactIn", InAmount: strconv.FormatUint(request.Amount, 10), OutAmount: strconv.FormatUint(request.Amount, 10), OtherAmountThreshold: strconv.FormatUint(minimum, 10), SlippageBPS: 50, ContextSlot: slot, RoutePlan: []json.RawMessage{json.RawMessage(`{"swap":{"TokenSwap":{}}}`)}}, nil
}
func (c multiplyBankQuoteClient) FetchSwapInstructions(ctx contextT, quote *QuoteResponse, vault solana.PublicKey) (*SwapInstructionsResponse, error) {
	if len(c.f.swapPrefix) != 17 {
		return nil, errors.New("independent Rust IDL prefix missing")
	}
	input, err := strconv.ParseUint(quote.InAmount, 10, 64)
	if err != nil {
		return nil, err
	}
	output, err := strconv.ParseUint(quote.OutAmount, 10, 64)
	if err != nil {
		return nil, err
	}
	inputMint, outputMint := mustKey(quote.InputMint), mustKey(quote.OutputMint)
	source, err := DeriveAssociatedTokenAccount(vault, inputMint, mustKey(TokenProgram))
	if err != nil {
		return nil, err
	}
	destination, err := DeriveAssociatedTokenAccount(vault, outputMint, mustKey(TokenProgram))
	if err != nil {
		return nil, err
	}
	poolSource, _ := DeriveAssociatedTokenAccount(multiplyBankSwapAuthority(), inputMint, mustKey(TokenProgram))
	poolDestination, _ := DeriveAssociatedTokenAccount(multiplyBankSwapAuthority(), outputMint, mustKey(TokenProgram))
	event, _, err := solana.FindProgramAddress([][]byte{[]byte("__event_authority")}, mustKey(JupiterProgram))
	if err != nil {
		return nil, err
	}
	data := append([]byte(nil), c.f.swapPrefix...)
	var tail [19]byte
	binary.LittleEndian.PutUint64(tail[:8], input)
	binary.LittleEndian.PutUint64(tail[8:16], output)
	binary.LittleEndian.PutUint16(tail[16:18], 50)
	data = append(data, tail[:]...)
	keys := []solana.PublicKey{mustKey(TokenProgram), multiplyBankSwapAuthority(), vault, source, poolSource, poolDestination, destination, inputMint, outputMint, mustKey(JupiterProgram), mustKey(JupiterProgram), event, mustKey(JupiterProgram)}
	var metas []RawAccountMeta
	for i, key := range keys {
		metas = append(metas, RawAccountMeta{PubKey: key.String(), IsSigner: i == 2, IsWritable: i >= 3 && i <= 6})
	}
	return &SwapInstructionsResponse{SwapInstruction: &RawInstruction{ProgramID: JupiterProgram, Accounts: metas, Data: base64.StdEncoding.EncodeToString(data)}}, nil
}

func TestCurrentGoMultiplyFreshDepositAndActualReceipt(t *testing.T) {
	f := newMultiplySVMFixture(t)
	result, err := f.worker(t, false).Tick(f.ctx)
	if err != nil || result.Condition != "operation_reconciled" {
		t.Fatalf("actual fresh Worker.Tick: %v %v", result, err)
	}
	f.assertReconciled(t, 1)
}

func TestCurrentGoMultiplyLostResponseKeylessRecovery(t *testing.T) {
	f := newMultiplySVMFixture(t)
	f.mu.Lock()
	f.lostResponse = true
	f.mu.Unlock()
	if _, err := f.worker(t, false).Tick(f.ctx); err == nil {
		t.Fatal("actual response loss was hidden")
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.Operation == nil || saved.Operation.Status != StatusBroadcastIntent {
		t.Fatalf("ambiguous send lost journal: %v", err)
	}
	f.mu.Lock()
	f.receiptMissing = true
	f.mu.Unlock()
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "awaiting_confirmed_transaction_receipt" {
		t.Fatalf("missing receipt did not retain attempt: %v %v", result, err)
	}
	held, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || held.Operation == nil || !bytes.Equal(held.Operation.SignedWire, saved.Operation.SignedWire) || held.Operation.TransactionSignature == nil || *held.Operation.TransactionSignature != *saved.Operation.TransactionSignature {
		t.Fatal("receipt wait replaced/lost immutable attempt")
	}
	f.mu.Lock()
	f.receiptMissing = false
	f.mu.Unlock()
	result, err = f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "recovered_operation_reconciled" {
		t.Fatalf("keyless actual receipt recovery: %v %v", result, err)
	}
	f.assertReconciled(t, 1)
}

func TestCurrentGoMultiplyRejectsIncompleteActualReceipt(t *testing.T) {
	f := newMultiplySVMFixture(t)
	f.mu.Lock()
	f.lostResponse = true
	f.mu.Unlock()
	_, _ = f.worker(t, false).Tick(f.ctx)
	f.mu.Lock()
	f.corruptReceipt = true
	f.mu.Unlock()
	result, err := f.worker(t, true).Tick(f.ctx)
	if err != nil || result.Condition != "manual_recovery_required" {
		t.Fatalf("malformed actual provider receipt: %v %v", result, err)
	}
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil || saved.State.Goal != GoalManualRecovery {
		t.Fatal("malformed receipt became financial completion")
	}
	var n int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM loyal_yield.multiply_operation_evidence e JOIN loyal_yield.multiply_operations o USING(operation_id) WHERE o.route_key=$1 AND e.evidence_kind='reconciled_receipt'`, f.state.RouteKey).Scan(&n); err != nil || n != 0 {
		t.Fatalf("malformed receipt published: %d %v", n, err)
	}
}

func (f *multiplySVMFixture) assertReconciled(t *testing.T, sends int) {
	t.Helper()
	saved, err := f.store.LoadRouteState(f.ctx, f.state.RouteKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Operation != nil || saved.State.CurrentOperationID != nil {
		t.Fatal("actual receipt failed terminal route CAS")
	}
	var status, wireHash string
	var evidence []byte
	var slot int64
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT o.status,o.signed_wire_sha256,e.evidence,e.observed_slot FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_operation_evidence e USING(operation_id) WHERE o.route_key=$1 AND e.evidence_kind='reconciled_receipt' ORDER BY o.created_at DESC LIMIT 1`, f.state.RouteKey).Scan(&status, &wireHash, &evidence, &slot); err != nil {
		t.Fatal(err)
	}
	var receipt ReconciledReceiptEvidence
	if err := json.Unmarshal(evidence, &receipt); err != nil {
		t.Fatal(err)
	}
	if status != "reconciled" || slot != 1000 || receipt.WireSHA256 != wireHash || receipt.ConfirmedSlot != 1000 || len(receipt.Transaction) == 0 {
		t.Fatal("exact actual transaction evidence missing from atomic terminal")
	}
	after, err := ObserveConfirmed(f.ctx, f.reader, f.topology, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.CollateralCustody(SyrupUsdcUsdc).AmountRaw != 0 || after.Position(SyrupUsdcUsdc).CollateralDepositedRaw != 1_000_000 {
		t.Fatal("actual bank financial deposit disagrees with terminal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != sends {
		t.Fatalf("recovery resent or omitted actual signed packet: %d", f.sends)
	}
}
