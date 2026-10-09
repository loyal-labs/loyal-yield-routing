package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// fixtureAccount is address's account as a test chain holds it.
func fixtureAccount(address, owner string, lamports uint64, data []byte) chain.Account {
	return chain.Account{Key: solana.MustPublicKeyFromBase58(address), Owner: solana.MustPublicKeyFromBase58(owner), Lamports: lamports, Data: data}
}

// testChain is the chain client against a local test endpoint.
func testChain(t *testing.T, url string) *chain.Client {
	t.Helper()
	client, err := chain.New(url, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The subprocess owns chain state for the lifetime of the connected scenario.
// Only initialization can inject accounts; subsequent RPC calls execute against
// that state. No failed simulation is replaced with a controlled success.
type connectedSVM struct {
	blockhash string
	mu        sync.Mutex
	input     io.WriteCloser
	output    *bufio.Reader
}

func startConnectedSVM(t *testing.T, ctx context.Context, accounts map[string]chain.Account) *connectedSVM {
	t.Helper()
	path := os.Getenv("KAMINO_CONNECTED_SVM_PATH")
	if path == "" {
		t.Fatal("connected execution requires KAMINO_CONNECTED_SVM_PATH")
	}
	processContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(processContext, path)
	command.Env = []string{"LC_ALL=C"}
	// Only local program artifacts are forwarded. The independent helper must
	// not inherit provider credentials or production network configuration.
	for _, name := range []string{"SQUADS_SMART_ACCOUNT_PROGRAM_SO", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO"} {
		if value := os.Getenv(name); value != "" {
			command.Env = append(command.Env, name+"="+value)
		}
	}
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
	fixture := make(map[string]any, len(accounts))
	for address, a := range accounts {
		fixture[address] = map[string]any{"Address": address, "Owner": a.Owner.String(), "Lamports": a.Lamports, "Executable": a.Executable, "Data": a.Data}
	}
	response, err := svm.call(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"accounts": fixture}})
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
func seedConnectedExecutionAccounts(t *testing.T, accounts map[string]chain.Account, positions []KaminoPositionAccounts, signer, vault string) {
	t.Helper()
	for _, address := range []string{signer, vault} {
		accounts[address] = fixtureAccount(address, "11111111111111111111111111111111", 1_000_000_000, []byte{})
	}
	for _, p := range positions {
		accounts[p.Market] = fixtureAccount(p.Market, kamino.ProgramID.String(), 100_000_000, make([]byte, 8))
		accounts[p.MarketAuthority] = fixtureAccount(p.MarketAuthority, "11111111111111111111111111111111", 100_000_000, []byte{})
		mint := fixtureAccount(p.CollateralMint, tokenProgram, 100_000_000, make([]byte, 82))
		binary.LittleEndian.PutUint32(mint.Data[:4], 1)
		fixtureKey(t, mint.Data, 4, p.MarketAuthority)
		binary.LittleEndian.PutUint64(mint.Data[36:44], 2_000_000_000_000)
		mint.Data[44] = 6
		mint.Data[45] = 1
		accounts[mint.Key.String()] = mint
		for _, token := range []struct{ address, mint string }{{p.LiquiditySupply, p.LiquidityMint}, {p.CollateralSupply, p.CollateralMint}} {
			account := fixtureAccount(token.address, tokenProgram, 100_000_000, make([]byte, 165))
			fixtureKey(t, account.Data, 0, token.mint)
			fixtureKey(t, account.Data, 32, p.MarketAuthority)
			binary.LittleEndian.PutUint64(account.Data[64:72], 2_000_000_000_000)
			account.Data[108] = 1
			accounts[account.Key.String()] = account
		}
	}
	for i, mint := range []string{USDCMint, USDTMint} {
		address := testPubkey(byte(34 + i))
		account := fixtureAccount(address, tokenProgram, 100_000_000, make([]byte, 165))
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
	if kind == "vault_shards" {
		// The vault's shard is bound to it through its sealed manifest.
		var manifestID int64
		if err = store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.lookup_table_manifests(family_id,subject_kind,subject_key,vault_id,desired_set_hash,address_count,source_slot,planner_version,catalog_version,sealed_at) VALUES($1,'vault',$2,$3,$4,$5,1000,'test','test',clock_timestamp()) RETURNING id`, familyID, table, vaultID, fmt.Sprintf("%x", hash.Sum(nil)), len(addresses)).Scan(&manifestID); err != nil {
			t.Fatal(err)
		}
		if _, err = store.pool.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_vault_bindings(vault_id,family_id,route_lookup_table_id,manifest_id,desired_head_revision,allocation_mode,reserved_capacity,lifecycle_state,active_from_slot,activated_at) VALUES($1,$2,$3,$4,1,'packed_shard',32,'active',1000,clock_timestamp())`, vaultID, familyID, tableID, manifestID); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *connectedSVM) call(request any) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.NewEncoder(s.input).Encode(request); err != nil {
		return nil, err
	}
	return s.output.ReadBytes('\n')
}
