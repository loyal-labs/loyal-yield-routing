package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The subprocess owns chain state for the lifetime of the connected scenario.
// Only initialization can inject accounts; subsequent RPC calls execute against
// that state. No failed simulation is replaced with a controlled success.
type connectedSVM struct {
	blockhash string
	mu        sync.Mutex
	input     io.WriteCloser
	output    *bufio.Reader
}

func startConnectedSVM(t *testing.T, ctx context.Context, accounts map[string]Account) *connectedSVM {
	t.Helper()
	path := os.Getenv("KAMINO_CONNECTED_SVM_PATH")
	if path == "" {
		t.Fatal("connected execution requires KAMINO_CONNECTED_SVM_PATH")
	}
	processContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(processContext, path)
	command.Env = []string{"LC_ALL=C"}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("local SVM exited: %v: %s", err, stderr.String())
		}
	})
	svm := &connectedSVM{input: input, output: bufio.NewReader(output)}
	response, err := svm.call(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"accounts": accounts}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err = json.Unmarshal(response, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error) > 0 || len(envelope.Result) == 0 {
		t.Fatalf("SVM initialization failed: %s", response)
	}
	var initialized struct {
		Blockhash string `json:"blockhash"`
	}
	if err = json.Unmarshal(envelope.Result, &initialized); err != nil || initialized.Blockhash == "" {
		t.Fatalf("invalid SVM blockhash: %s", response)
	}
	svm.blockhash = initialized.Blockhash
	return svm
}

// Seed only initial protocol inventory. All subsequent balance changes must be
// performed by the SBF programs; this helper is never used after initialization.
func seedConnectedExecutionAccounts(t *testing.T, accounts map[string]Account, positions []KaminoPositionAccounts, signer, vault string) {
	t.Helper()
	for _, address := range []string{signer, vault} {
		accounts[address] = Account{Address: address, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000, Data: []byte{}}
	}
	for _, p := range positions {
		accounts[p.Market] = Account{Address: p.Market, Owner: KLendProgram, Lamports: 100_000_000, Data: make([]byte, 8)}
		accounts[p.MarketAuthority] = Account{Address: p.MarketAuthority, Owner: "11111111111111111111111111111111", Lamports: 100_000_000, Data: []byte{}}
		mint := Account{Address: p.CollateralMint, Owner: tokenProgram, Lamports: 100_000_000, Data: make([]byte, 82)}
		binary.LittleEndian.PutUint32(mint.Data[:4], 1)
		fixtureKey(t, mint.Data, 4, p.MarketAuthority)
		binary.LittleEndian.PutUint64(mint.Data[36:44], 2_000_000_000_000)
		mint.Data[44] = 6
		mint.Data[45] = 1
		accounts[mint.Address] = mint
		for _, token := range []struct{ address, mint string }{{p.LiquiditySupply, p.LiquidityMint}, {p.CollateralSupply, p.CollateralMint}} {
			account := Account{Address: token.address, Owner: tokenProgram, Lamports: 100_000_000, Data: make([]byte, 165)}
			fixtureKey(t, account.Data, 0, token.mint)
			fixtureKey(t, account.Data, 32, p.MarketAuthority)
			binary.LittleEndian.PutUint64(account.Data[64:72], 2_000_000_000_000)
			account.Data[108] = 1
			accounts[account.Address] = account
		}
	}
	for i, mint := range []string{USDCMint, USDTMint} {
		address := testPubkey(byte(34 + i))
		account := Account{Address: address, Owner: tokenProgram, Lamports: 100_000_000, Data: make([]byte, 165)}
		fixtureKey(t, account.Data, 0, mint)
		fixtureKey(t, account.Data, 32, jupiterEvent)
		binary.LittleEndian.PutUint64(account.Data[64:72], 2_000_000_000_000)
		account.Data[108] = 1
		accounts[address] = account
	}
	for key, account := range accounts {
		if account.Lamports < 100_000_000 {
			account.Lamports = 100_000_000
			accounts[key] = account
		}
	}
}

func seedConnectedLookupTable(t *testing.T, ctx context.Context, store *Store, cluster, signer string, vaultID int64, table string, addresses []string, kind, allocation string) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_rollout_controls(cluster,vault_id,rollout_mode,updated_by) VALUES($1,$2,'reusable_only','connected-local-verifier') ON CONFLICT DO NOTHING`, cluster, vaultID); err != nil {
		t.Fatal(err)
	}
	var familyID, tableID int64
	if err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_families(cluster,logical_name,kind,planner_version,catalog_version,active_generation,provisioning_authority,payer,hard_capacity,largest_atomic_expansion,safety_margin,allocation_high_water) VALUES($1,$3,$3,'test','test',0,$2,$2,256,1,1,254) RETURNING id`, cluster, signer, kind).Scan(&familyID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	for _, address := range addresses {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(address)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(address))
	}
	encoded, err := json.Marshal(addresses)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_lookup_tables(cluster,scope,table_address,authority,payer,status,durable,address_count,address_hash,addresses,last_extended_slot,warmup_slot,family_id,allocation_kind,generation,shard_ordinal,desired_state,accepting_allocations,allocation_high_water,reserved_address_count,usable_address_count,last_verified_slot,last_verified_at,mutation_epoch) VALUES($1,$8,$2,$3,$3,'active',true,$4,$5,$6::jsonb,900,901,$7,$8,0,0,'active',true,254,$4,$4,1000,clock_timestamp(),0) RETURNING id`, cluster, table, signer, len(addresses), fmt.Sprintf("%x", hash.Sum(nil)), string(encoded), familyID, allocation).Scan(&tableID); err != nil {
		t.Fatal(err)
	}
	for ordinal, address := range addresses {
		if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_addresses(route_lookup_table_id,address,ordinal,added_slot,usable_after_slot,last_verified_slot,last_verified_at) VALUES($1,$2,$3,900,901,1000,clock_timestamp())`, tableID, address, ordinal); err != nil {
			t.Fatal(err)
		}
	}
}

func runConnectedRustWorker(t *testing.T, ctx context.Context, database string, request map[string]any) {
	t.Helper()
	path := os.Getenv("KAMINO_CONNECTED_WORKER_PATH")
	if path == "" {
		t.Fatal("connected lifecycle requires compiled retained worker test binary")
	}
	input := filepath.Join(t.TempDir(), "go-handoff.json")
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, path, "--exact", "cross_mint::connected_e2e::consume_go_cross_mint_opportunity", "--ignored", "--nocapture")
	// The only key is the public deterministic local fixture signer. Never
	// inherit production credentials or endpoints into the retained worker.
	command.Env = []string{"KAMINO_CONNECTED_CONFIRMER_PATH=" + os.Getenv("KAMINO_CONNECTED_CONFIRMER_PATH"), "LC_ALL=C", "OBSERVABILITY_ENABLED=false", "FLEET_TEST_DATABASE_URL=" + database, "KAMINO_CONNECTED_REQUEST_PATH=" + input,
		"POLICY_KEYPAIR=" + encodeBase58(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))),
		"EARN_ROUTER_ENABLED_STABLE_MINTS=" + USDCMint + "," + USDTMint,
		"NO_PROXY=127.0.0.1,localhost", "HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("retained worker failed: %v\n%s", err, output)
	}
	t.Logf("retained worker: %s", output)
}

func (s *connectedSVM) call(request any) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.NewEncoder(s.input).Encode(request); err != nil {
		return nil, err
	}
	return s.output.ReadBytes('\n')
}
