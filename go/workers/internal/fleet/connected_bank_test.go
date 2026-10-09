package fleet

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func fixtureKey(t *testing.T, data []byte, offset int, address string) {
	t.Helper()
	key, err := decodePublicKey(address)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[offset:offset+32], key[:])
}

func connectedPolicyHeader(t *testing.T, settings, signer string, seed uint64) ([]byte, string) {
	return connectedPolicyHeaderForIndex(t, settings, signer, seed, 0)
}

func connectedPolicyHeaderForIndex(t *testing.T, settings, signer string, seed uint64, vaultIndex uint8) ([]byte, string) {
	t.Helper()
	data, err := BuildExactPolicyFixture(settings, signer, vaultIndex, nil)
	if err != nil {
		t.Fatal(err)
	}
	address, bump, err := derivePolicyAccount(settings, seed)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[40:48], seed)
	data[48] = bump
	return data[:110], address
}

func connectedSwapPolicy(t *testing.T, binding CrossMintPolicyBindings, seed uint64) []byte {
	t.Helper()
	data, _ := connectedPolicyHeaderForIndex(t, binding.Settings, binding.DelegatedSigner, seed, binding.VaultIndex)
	data = appendU32x(data, 2)
	for i, disc := range [][]byte{jupiterRouteV2Discriminator, jupiterSharedV2Discriminator} {
		key, _ := decodePublicKey(jupiterProgram)
		data = append(data, key[:]...)
		data = appendU32x(data, 2)
		indexes := []byte{0, 2}
		offset := uint64(24)
		if i == 1 {
			indexes = []byte{1, 5}
			offset = 25
		}
		for j, index := range indexes {
			keys := []string{binding.VaultPubkey}
			if j == 1 {
				keys = nil
				for _, mint := range earnStableMints {
					ata, err := deriveATA(binding.VaultPubkey, mint, mustStableProgram(mint))
					if err != nil {
						t.Fatal(err)
					}
					keys = append(keys, ata)
				}
			}
			data = append(data, index, 0)
			data = appendU32x(data, uint32(len(keys)))
			for _, k := range keys {
				key, _ := decodePublicKey(k)
				data = append(data, key[:]...)
			}
			data = append(data, 0)
		}
		data = appendU32x(data, 3)
		data = appendU64x(data, 0)
		data = append(data, 5)
		data = appendU32x(data, uint32(len(disc)))
		data = append(data, disc...)
		data = append(data, 0)
		data = appendU64x(data, offset)
		data = append(data, 1)
		data = appendU16x(data, binding.Swap.MaxSlippageBPS)
		data = append(data, 5)
		data = appendU64x(data, offset+2)
		data = append(data, 0, 0, 0)
	}
	data = append(data, 0, 0)
	data = appendU32x(data, 3)
	for _, mint := range []string{USDCMint, USDTMint, USDSMint} {
		key, _ := decodePublicKey(mint)
		data = append(data, key[:]...)
		data = appendU64x(data, 0)
		data = append(data, 0, 1, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
		data = append(data, 0)
		data = appendU64x(data, binding.Swap.DailySourceMintSpendingCap)
		data = appendU64x(data, 0)
	}
	data = appendU64x(data, 0)
	data = append(data, 0)
	data = append(data, make([]byte, 32)...)
	return data
}

// connectedEarnPolicyData builds a deployed Earn policy account that pins every
// account of each instruction, accepting both route markets wherever a market
// is, as retained policy discovery requires.
func connectedEarnPolicyData(settings, signer string, instructions []RouteInstruction, positions []KaminoPositionAccounts) ([]byte, error) {
	return connectedEarnPolicyDataForIndex(settings, signer, 0, instructions, positions)
}

func connectedEarnPolicyDataForIndex(settings, signer string, vaultIndex uint8, instructions []RouteInstruction, positions []KaminoPositionAccounts) ([]byte, error) {
	b, err := BuildExactPolicyFixture(settings, signer, vaultIndex, nil)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(b[len(b)-4:], uint32(len(instructions)))
	for _, ix := range instructions {
		program, err := decodePublicKey(ix.Program)
		if err != nil {
			return nil, err
		}
		b = append(b, program[:]...)
		b = appendU32x(b, uint32(len(ix.Accounts)))
		for i, account := range ix.Accounts {
			keys := []string{account.Address}
			for _, p := range positions {
				if account.Address == p.Market {
					keys = []string{positions[0].Market, positions[1].Market}
				}
			}
			b = append(b, byte(i), 0)
			b = appendU32x(b, uint32(len(keys)))
			for _, key := range keys {
				decoded, err := decodePublicKey(key)
				if err != nil {
					return nil, err
				}
				b = append(b, decoded[:]...)
			}
			b = append(b, 0)
		}
		b = appendU32x(b, 1)
		b = appendU64x(b, 0)
		b = append(b, 5)
		b = appendU32x(b, uint32(len(ix.Data)))
		b = append(b, ix.Data...)
		b = append(b, 0)
	}
	return b, nil
}

// ConnectedKind selects the route a ConnectedBank plans and publishes.
type ConnectedKind int

const (
	// ConnectedSameMint moves USDC between two existing obligations.
	ConnectedSameMint ConnectedKind = iota
	// ConnectedSameMintSetup moves USDC into a market where the vault has no
	// obligation yet and holds no lamports; only its setup policy may
	// initialize obligations.
	ConnectedSameMintSetup
	// ConnectedCrossMint moves USDC collateral into a USDT reserve via Jupiter.
	ConnectedCrossMint
)

// ConnectedBank is a live local SVM (Squads plus the explicit mock KLend and
// Jupiter SBF programs) behind a loopback RPC that loses the response of the
// first executed send, with one classic Earn vault (index 1) funded in its
// source reserve, its policies and managed ALTs, and one opportunity the Go
// planner published. Only SBF execution changes balances after seeding.
type ConnectedBank struct {
	Pool                            *pgxpool.Pool
	Store                           *Store
	RPCURL, Cluster                 string
	OpportunityID, EpochID, VaultID int64
	Vault, Settings, SetupPolicy    string
	Signer                          ed25519.PrivateKey
	Source, Target                  KaminoPositionAccounts
	buildURL                        string
	buildTransport                  http.RoundTripper
	sends, lostResponses            atomic.Int64
}

// Sends counts sendTransaction calls; LostResponses those executed by the
// chain whose response the RPC dropped.
func (b *ConnectedBank) Sends() int64         { return b.sends.Load() }
func (b *ConnectedBank) LostResponses() int64 { return b.lostResponses.Load() }

// Revalidator is the Go route preparation bound to this bank's RPC and to a
// Jupiter client that trusts only the bank's TLS fixture.
func (b *ConnectedBank) Revalidator(owner string, fused bool) (*Revalidator, error) {
	client, err := chain.New(b.RPCURL, 15*time.Second)
	if err != nil {
		return nil, err
	}
	r, err := NewRevalidator(b.Store, client, RevalidatorConfig{Owner: owner, FusedExecute: fused, DelegatedSigner: encodeBase58(b.Signer.Public().(ed25519.PublicKey)), LeaseTTL: time.Minute, SlotDuration: 400 * time.Millisecond, CrossMintEnabled: true, CrossMintMaxValueLossBPS: 50, CrossMintMaxSlippageBPS: 50, JupiterBuildURL: b.buildURL})
	if err != nil {
		return nil, err
	}
	r.jupiter.client.Transport = b.buildTransport
	return r, nil
}

func NewConnectedBank(t *testing.T, kind ConnectedKind) *ConnectedBank {
	t.Helper()
	databaseURL := os.Getenv("FLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires disposable database")
	}
	if u, err := url.Parse(databaseURL); err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/fleet" {
		t.Fatal("requires disposable loopback /fleet database")
	}
	ctx := t.Context()
	store, err := OpenStore(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	signer := encodeBase58(key.Public().(ed25519.PublicKey))
	suffix := fmt.Sprint(time.Now().UnixNano())
	// Every durable identity is unique to this bank: earlier runs leave
	// their rows in the shared fixture database.
	unique := func(tag string) string {
		digest := sha256.Sum256([]byte(suffix + ":" + tag))
		return encodeBase58(digest[:])
	}
	settings := unique("settings")
	settingsKey := solana.MustPublicKeyFromBase58(settings)
	const vaultIndex = uint8(1) // Classic Earn; Multiply keeps its independent vault0.
	vaultKey, _, err := squads.SmartAccountAddress(settingsKey, vaultIndex)
	if err != nil {
		t.Fatal(err)
	}
	vault := vaultKey.String()
	// A cluster per bank keeps its queue, ALT families and controls apart
	// from every other run sharing the fixture database.
	cluster := "localnet-" + suffix
	source := ReserveIdentity{testIdentity(51), testIdentity(52), USDCMint}
	target := ReserveIdentity{testIdentity(61), testIdentity(62), USDTMint}
	sameMint := kind != ConnectedCrossMint
	if sameMint {
		target.Mint = USDCMint
	}
	const amount = uint64(1_000_000_000)
	accounts := map[string]chain.Account{}
	positions := []KaminoPositionAccounts{}
	states := map[string]ReserveState{}
	for i, identity := range []ReserveIdentity{source, target} {
		account := reserveFixture(identity, 1_000_000_000_000, 1_000_000_000_000)
		setCurveScale(account.Data, uint32(50+i*350))
		fixtureKey(t, account.Data, 408, tokenProgram)
		fixtureKey(t, account.Data, 2560, testIdentity(byte(90+i)))
		fixtureKey(t, account.Data, 160, testIdentity(byte(92+i)))
		fixtureKey(t, account.Data, 2600, testIdentity(byte(94+i)))
		binary.LittleEndian.PutUint64(account.Data[2592:2600], 2_000_000_000_000)
		accounts[identity.Address] = account
		decoded, err := decodeRouteReserve(&account, vault)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 || kind != ConnectedSameMintSetup {
			obligation := fixtureAccount(decoded.Obligation, kamino.ProgramID.String(), 1_000_000, make([]byte, kamino.ObligationSize))
			copy(obligation.Data, kamino.ObligationDiscriminator[:])
			fixtureKey(t, obligation.Data, 32, identity.Market)
			fixtureKey(t, obligation.Data, 64, vault)
			if i == 0 {
				fixtureKey(t, obligation.Data, 96, identity.Address)
				binary.LittleEndian.PutUint64(obligation.Data[128:136], amount)
			}
			if _, err = decodeObligation(&obligation, identity.Market, vault, &decoded.Position); err != nil {
				t.Fatal(err)
			}
			accounts[decoded.Obligation] = obligation
		}
		positions = append(positions, decoded.Position)
		state, err := DecodeKaminoReserve(&account, identity, 1000, 400*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		states[identity.Address] = state
		mint := fixtureAccount(identity.Mint, tokenProgram, 1_000_000, make([]byte, 82))
		mint.Data[44] = 6
		mint.Data[45] = 1
		binary.LittleEndian.PutUint64(mint.Data[36:44], 2_000_000_000_000)
		accounts[identity.Mint] = mint
		ata := fixtureAccount(decoded.Position.VaultLiquidityATA, tokenProgram, 1_000_000, make([]byte, 165))
		fixtureKey(t, ata.Data, 0, identity.Mint)
		fixtureKey(t, ata.Data, 32, vault)
		ata.Data[108] = 1
		accounts[decoded.Position.VaultLiquidityATA] = ata
	}
	body, _ := jupiterBuildForVault(t, vault, amount-1, amount-1, 1)
	var envelope rawJupiterBuild
	if err = json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	minimum := thresholdFor(amount-1, 1)
	installPolicy := func(seed uint64, instructions []RouteInstruction) string {
		policyData, err := connectedEarnPolicyDataForIndex(settings, signer, vaultIndex, instructions, positions)
		if err != nil {
			t.Fatal(err)
		}
		_, address := connectedPolicyHeader(t, settings, signer, seed)
		_, bump, _ := derivePolicyAccount(settings, seed)
		putPolicySeed(policyData, seed, bump)
		// Executable policy accounts include the full tail beyond constraints.
		accounts[address] = fixtureAccount(address, squads.ProgramID.String(), 1_000_000, append(policyData, make([]byte, 2+4+8+1+32)...))
		return address
	}
	// Deposit amounts are finalized custody, not the planned amount: policies
	// authorize the deposit discriminator; custody checks bound the amount.
	klend := func(p KaminoPositionAccounts) klendPosition {
		position, err := bindKLendPDAs(&p, vaultKey)
		if err != nil {
			t.Fatal(err)
		}
		return position
	}
	withdraw := withdrawV2(klend(positions[0]), amount)
	if !sameMint {
		withdraw = withdrawV2(klend(positions[0]), amount-1)
	}
	deposit := depositV2(klend(positions[1]), amount)
	deposit.Data = deposit.Data[:8]
	bank := &ConnectedBank{Store: store, Pool: store.pool, Cluster: cluster, Vault: vault, Settings: settings, Signer: key, Source: positions[0], Target: positions[1]}
	earnPolicy, targetPolicy := "", ""
	var routeBody []RouteInstruction
	switch kind {
	case ConnectedSameMint, ConnectedSameMintSetup:
		earnPolicy = installPolicy(1, []RouteInstruction{withdraw, deposit})
		input := KaminoSameMintRouteRequest{Vault: vault, Source: positions[0], Target: positions[1], WithdrawCollateralAmount: amount, DepositLiquidityAmount: amount, Payer: signer}
		if kind == ConnectedSameMintSetup {
			metadata, err := kamino.UserMetadataAddress(vaultKey)
			if err != nil {
				t.Fatal(err)
			}
			bank.SetupPolicy = installPolicy(2, []RouteInstruction{initObligation(vaultKey, klend(positions[1]), metadata)})
			// Any top-up amount yields the same accounts for ALT coverage.
			input.TargetObligationMissing, input.VaultRentTopUpLamports = true, 1
		}
		route, err := BuildSameMintRoute(input)
		if err != nil {
			t.Fatal(err)
		}
		if routeBody, _, err = wrapSameMintRoute(route, signer, vaultIndex, earnPolicy, squadsPolicyAccount(accounts[earnPolicy].Data), bank.SetupPolicy, squadsPolicyAccount(accounts[bank.SetupPolicy].Data)); err != nil {
			t.Fatal(err)
		}
	case ConnectedCrossMint:
		route, err := BuildCrossMintLegs(KaminoSameMintRouteRequest{Vault: vault, Source: positions[0], Target: positions[1], WithdrawCollateralAmount: amount - 1, DepositLiquidityAmount: minimum})
		if err != nil {
			t.Fatal(err)
		}
		// The source policy's deposit arm authorizes recovery to its source,
		// not the unrelated target. The target has its own two-arm policy.
		recovery, err := BuildIdleDeposit(KaminoIdleDepositRequest{Vault: vault, Target: positions[0], DepositLiquidityAmount: minimum})
		if err != nil {
			t.Fatal(err)
		}
		recovery.Protected[0].Data = recovery.Protected[0].Data[:8]
		targetWithdraw := withdrawV2(klend(positions[1]), amount-1)
		earnPolicy = installPolicy(1, []RouteInstruction{route.Protected[0], recovery.Protected[0]})
		targetPolicy = installPolicy(2, []RouteInstruction{targetWithdraw, deposit})
		wrapped, err := wrapSquadsPolicy(earnPolicy, signer, vaultIndex, []uint8{0}, []RouteInstruction{route.Protected[0]})
		if err != nil {
			t.Fatal(err)
		}
		routeBody = append(append([]RouteInstruction{}, route.Public...), wrapped)
	}
	_, swapPolicy := connectedPolicyHeader(t, settings, signer, 11)
	binding := CrossMintPolicyBindings{Settings: settings, VaultIndex: vaultIndex, VaultPubkey: vault, DelegatedSigner: signer, Withdraw: CrossMintEarnPolicyBinding{earnPolicy, 999, "local-withdraw", "finalized", 0}, Deposit: CrossMintEarnPolicyBinding{targetPolicy, 999, "local-deposit", "finalized", 1}, Swap: CrossMintSwapPolicyBinding{PolicyAccount: swapPolicy, SourceShard: "classic", EnrollmentGeneration: 1, ObservedSlot: 999, ObservedSignature: "local-swap", SourceCommitment: "finalized", MaxSlippageBPS: 50, DailySourceMintSpendingCap: 10_000_000_000}}
	binding.Swap.ManifestFingerprint = fingerprintCrossMintManifest(binding, []string{USDCMint, USDTMint, USDSMint}, tokenProgram)
	accounts[swapPolicy] = fixtureAccount(swapPolicy, squads.ProgramID.String(), 1_000_000, connectedSwapPolicy(t, binding, 11))
	// Managed ALT coverage: every address the route body needs, split into
	// the shared market catalog and the vault's own shard.
	coverage := map[string]bool{}
	for _, key := range requiredLookupTableAddresses(routeBody) {
		coverage[key] = true
	}
	if !sameMint {
		coverage[targetPolicy] = true
		coverage[envelope.SwapInstruction.ProgramID] = true
		for _, account := range envelope.SwapInstruction.Accounts {
			if !account.IsSigner {
				coverage[account.Pubkey] = true
			}
		}
	}
	covered := sortedKeys(coverage)
	providerTable := unique("provider-alt")
	table := fixtureAccount(providerTable, altProgram, 1_000_000, make([]byte, 56+32*len(covered)))
	binary.LittleEndian.PutUint32(table.Data[:4], 1)
	binary.LittleEndian.PutUint64(table.Data[4:12], ^uint64(0))
	binary.LittleEndian.PutUint64(table.Data[12:20], 900)
	table.Data[21] = 1
	fixtureKey(t, table.Data, 22, signer)
	for i, key := range covered {
		fixtureKey(t, table.Data, 56+32*i, key)
	}
	accounts[providerTable] = table
	sharedSet := map[string]bool{kamino.FarmsProgramID.String(): true, instructionsSysvar: true, tokenProgram: true, solana.SysVarRentPubkey.String(): true}
	for _, p := range positions {
		for _, key := range []string{p.Reserve, p.Market, p.MarketAuthority, p.LiquidityMint, p.CollateralMint, p.LiquiditySupply, p.CollateralSupply} {
			sharedSet[key] = true
		}
	}
	if !sameMint {
		// Jupiter infrastructure belongs to the managed shared catalog.
		sharedSet[envelope.SwapInstruction.ProgramID] = true
		vaultAccounts := map[string]bool{vault: true}
		for _, p := range positions {
			vaultAccounts[p.VaultLiquidityATA] = true
		}
		for _, account := range envelope.SwapInstruction.Accounts {
			if !vaultAccounts[account.Pubkey] {
				sharedSet[account.Pubkey] = true
			}
		}
	}
	vaultSet := map[string]bool{swapPolicy: true}
	if targetPolicy != "" {
		vaultSet[targetPolicy] = true
	}
	for _, key := range covered {
		if !sharedSet[key] {
			vaultSet[key] = true
		}
	}
	for _, p := range positions {
		vaultSet[p.Obligation] = true
		vaultSet[p.VaultLiquidityATA] = true
	}
	sharedAddresses, vaultAddresses := sortedKeys(sharedSet), sortedKeys(vaultSet)
	sharedTable, vaultTable := unique("shared-alt"), unique("vault-alt")
	for key, addresses := range map[string][]string{sharedTable: sharedAddresses, vaultTable: vaultAddresses} {
		data := make([]byte, 56+32*len(addresses))
		copy(data, table.Data[:56])
		for i, address := range addresses {
			fixtureKey(t, data, 56+32*i, address)
		}
		accounts[key] = fixtureAccount(key, altProgram, 100_000_000, data)
	}
	envelope.AddressesByLookupTableAddress = map[string][]string{providerTable: covered}
	seedConnectedExecutionAccounts(t, accounts, positions, signer, vault)
	if kind == ConnectedSameMintSetup {
		// The vault holds no lamports: the route must fund its obligation rent.
		delete(accounts, vault)
	}
	svm := startConnectedSVM(t, ctx, accounts)
	blockhash, err := decodePublicKey(svm.blockhash)
	if err != nil {
		t.Fatal(err)
	}
	envelope.BlockhashWithMetadata.Blockhash = blockhash[:]
	envelope.BlockhashWithMetadata.LastValidBlockHeight = 1150
	if body, err = json.Marshal(envelope); err != nil {
		t.Fatal(err)
	}
	// Jupiter's real wire format is a numeric byte array, not Go's default
	// base64 encoding for []byte.
	var wire map[string]any
	if err = json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	blockhashNumbers := make([]int, len(blockhash))
	for i, b := range blockhash {
		blockhashNumbers[i] = int(b)
	}
	wire["blockhashWithMetadata"].(map[string]any)["blockhash"] = blockhashNumbers
	if body, err = json.Marshal(wire); err != nil {
		t.Fatal(err)
	}
	confirmedHead := unique("confirmed-head-blockhash")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/build" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		var request struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		// All chain reads and writes share one SVM state.
		response, err := svm.call(request)
		if err != nil {
			t.Errorf("SVM transport: %v", err)
			http.Error(w, "SVM transport failed", 500)
			return
		}
		if request.Method == "getLatestBlockhash" {
			var config struct {
				Commitment string `json:"commitment"`
			}
			if len(request.Params) > 0 {
				_ = json.Unmarshal(request.Params[0], &config)
			}
			if config.Commitment != "finalized" {
				// The production RPC is load balanced: a confirmed blockhash
				// comes from a backend ahead of the one serving the next
				// simulation or fee quote, which has not seen it. Only the
				// finalized blockhash is known to every backend.
				var envelope map[string]any
				if json.Unmarshal(response, &envelope) == nil {
					if result, ok := envelope["result"].(map[string]any); ok {
						if value, ok := result["value"].(map[string]any); ok {
							value["blockhash"] = confirmedHead
							response, _ = json.Marshal(envelope)
						}
					}
				}
			}
		}
		if request.Method == "sendTransaction" {
			bank.sends.Add(1)
			if bank.lostResponses.CompareAndSwap(0, 1) {
				// The chain executed the persisted wire but the response was
				// lost: landing must recover it from the signature, unsigned.
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32000, "message": "injected response loss after local execution"}})
				return
			}
		}
		_, _ = w.Write(response)
	})
	build := httptest.NewTLSServer(handler)
	t.Cleanup(build.Close)
	rpc := httptest.NewServer(handler)
	t.Cleanup(rpc.Close)
	bank.RPCURL, bank.buildURL, bank.buildTransport = rpc.URL, build.URL+"/build", build.Client().Transport
	vaultID := seedWorkerVault(t, ctx, store, suffix, source.Market, source.Address)
	bank.VaultID = vaultID
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,policy_account=$4,vault_pubkey=$5,delegated_signers=ARRAY[$3]::text[],cluster=$6,source_commitment='finalized',finalized_eligible=true,vault_index=$11,stable_mints=ARRAY[$7,$8]::text[],kamino_markets=ARRAY[$9,$10]::text[],kamino_liquidity_mints=ARRAY[$7,$8]::text[] WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, vaultID, settings, signer, earnPolicy, vault, cluster, USDCMint, USDTMint, source.Market, target.Market, int16(vaultIndex))
	exec(`UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3,vault_index=$4 WHERE id=$1`, vaultID, settings, vault, int16(vaultIndex))
	exec(`UPDATE loyal_yield.vault_reserve_positions_current SET amount_raw=$2,planning_metadata=jsonb_build_object('amount_semantics','kamino_obligation_collateral_deposited_amount','redeemable_source_liquidity_amount_raw',$3::text,'idle_vault_liquidity_amount_raw','0') WHERE vault_id=$1`, vaultID, int64(amount), strconv.FormatUint(amount, 10))
	if kind != ConnectedSameMintSetup {
		// The finalized snapshot includes the empty target obligation the chain holds.
		exec(`INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) SELECT vault_id,$2,$3,$4,0,false,$5,snapshot_id,observed_slot,observed_at,'{"amount_semantics":"kamino_obligation_collateral_deposited_amount","redeemable_source_liquidity_amount_raw":"0"}'::jsonb FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1 AND reserve=$6`, vaultID, target.Address, target.Market, target.Mint, states[target.Address].SupplyAPYBPS, source.Address)
	}
	if kind == ConnectedSameMintSetup {
		// Earn records init authority as the vault's separate setup policy.
		var setupID int64
		if err := store.pool.QueryRow(ctx, `INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,2,$4,$5,$6,ARRAY[$3]::text[],1,ARRAY[]::text[],ARRAY[$7]::text[],ARRAY[$8]::text[],ARRAY[$7]::text[],'[]',true,'finalized',true,999,'local-setup') RETURNING id`, cluster, settings, signer, bank.SetupPolicy, int16(vaultIndex), vault, USDCMint, target.Market).Scan(&setupID); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE loyal_yield.managed_vaults SET setup_policy_id=$2 WHERE id=$1`, vaultID, setupID)
	}
	if !sameMint {
		// The target's own policy, projected as the observer would.
		exec(`INSERT INTO loyal_yield.route_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signers,threshold,route_modes,stable_mints,kamino_markets,kamino_liquidity_mints,swap_lanes,active,source_commitment,finalized_eligible,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,2,$4,$5,$6,ARRAY[$3]::text[],1,ARRAY['same_mint_kamino']::text[],ARRAY[$7]::text[],ARRAY[$8]::text[],ARRAY[$7]::text[],'[]',true,'finalized',true,$9,$10)`, cluster, settings, signer, targetPolicy, int16(vaultIndex), vault, target.Mint, target.Market, binding.Deposit.ObservedSlot, binding.Deposit.ObservedSignature)
		_, siblingPolicy := connectedPolicyHeader(t, settings, signer, 12)
		exec(`INSERT INTO loyal_yield.cross_mint_vault_opt_ins(cluster,settings,vault_index,vault_pubkey,enabled,classic_policy_account,classic_policy_seed,token_2022_policy_account,token_2022_policy_seed,max_slippage_bps,daily_source_mint_spending_cap,generation) VALUES($1,$2,$8,$3,true,$4,11,$5,12,$6,$7,1)`, cluster, settings, vault, swapPolicy, siblingPolicy, binding.Swap.MaxSlippageBPS, int64(binding.Swap.DailySourceMintSpendingCap), int16(vaultIndex))
		for _, shard := range []struct {
			name, account string
			seed          int64
		}{{"classic", swapPolicy, 11}, {"token_2022", siblingPolicy, 12}} {
			exec(`INSERT INTO loyal_yield.cross_mint_swap_policies(cluster,settings,authority,policy_seed,policy_account,vault_index,vault_pubkey,delegated_signer,source_shard,max_slippage_bps,daily_source_mint_spending_cap,manifest_fingerprint,active,start_eligible,last_mutation,source_commitment,last_seen_slot,last_seen_signature) VALUES($1,$2,$3,$4,$5,$11,$6,$3,$7,$8,$9,$10,true,true,'create','finalized',999,'local-swap')`, cluster, settings, signer, shard.seed, shard.account, vault, shard.name, binding.Swap.MaxSlippageBPS, int64(binding.Swap.DailySourceMintSpendingCap), binding.Swap.ManifestFingerprint, int16(vaultIndex))
		}
		exec(`INSERT INTO loyal_yield.cross_mint_movement_controls(cluster,start_new_movements,continue_or_recover_existing,generation,updated_by) VALUES($1,true,true,1,'connected-local-verifier')`, cluster)
	}
	seedConnectedLookupTable(t, ctx, store, cluster, signer, vaultID, sharedTable, sharedAddresses, "shared_market", "shared_market")
	seedConnectedLookupTable(t, ctx, store, cluster, signer, vaultID, vaultTable, vaultAddresses, "vault_shards", "vault_shard")
	position, err := store.LoadVaultPosition(ctx, cluster, vaultID, source, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := MarketSnapshot{Cluster: cluster, Slot: 1000, ObservedAt: now, Reserves: states}
	epoch := testImmutableMarketEpoch(t, snapshot, source, target)
	snapshot.Hash, snapshot.ExpiresAt = epoch.Fingerprint, epoch.ExpiresAt
	snapshot.MintExpiresAt = map[string]time.Time{USDCMint: epoch.OptimizerEnvelopeExpiresAt(), USDTMint: epoch.OptimizerEnvelopeExpiresAt()}
	if snapshot.OptimizerEpochID, err = store.EnsureOptimizerEpoch(ctx, cluster, epoch); err != nil {
		t.Fatal(err)
	}
	fleetVault := FleetVault{Position: position, CrossMintTargets: map[string]CrossMintPolicyBindings{target.Address: binding}, CrossMintMaxValueLossBPS: 50}
	if sameMint {
		fleetVault.CrossMintTargets, fleetVault.AllowedTargets = nil, []string{target.Address}
	}
	plan, err := PlanFleet(snapshot, []FleetVault{fleetVault})
	if err != nil || len(plan.Opportunities) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	published, err := store.Publish(ctx, cluster, epoch, position, plan.Opportunities[0].Decision)
	if err != nil || !published.Inserted {
		t.Fatalf("publish: %+v %v", published, err)
	}
	if err = store.RefreshTargetCapacity(ctx, cluster, target.Address, target.Mint, states[target.Address].TotalSupplyUSDMicros, 1000); err != nil {
		t.Fatal(err)
	}
	bank.OpportunityID, bank.EpochID = published.OpportunityID, published.EpochID
	return bank
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
