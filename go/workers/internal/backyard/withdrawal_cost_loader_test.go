package backyard

import (
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

func withdrawalLoaderFixture(t *testing.T, d *Database, others ...ConfirmedAccount) (*withdrawalCostPreparation, Observation, *chain.Client, string, func()) {
	t.Helper()
	m := readyWorkerManifest(t)
	owner := solana.NewWallet()
	seed := [16]byte{1}
	settings, bump, err := squads.SettingsAddress(seed)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := earnmax.AuthorityOf(settings)
	if err != nil {
		t.Fatal(err)
	}
	user, err := voltr.UserAccounts(earnmax.Vault, earnmax.LPMint, authority.Key())
	if err != nil {
		t.Fatal(err)
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, earnmax.LPMint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	keys := []solana.PublicKey{solana.TokenProgramID, authority.Key(), solana.SystemProgramID, earnmax.Vault, earnmax.LPMint, user.LPATA, escrow, user.Receipt, solana.SysVarRentPubkey, owner.PublicKey()}
	metas := make(solana.AccountMetaSlice, len(keys))
	for i, key := range keys {
		metas[i] = solana.Meta(key)
	}
	data := make([]byte, 18)
	copy(data, voltr.RequestWithdrawVaultDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 100)
	data[16] = 1
	tx, err := solana.NewTransaction([]solana.Instruction{solana.NewInstruction(voltr.ProgramID, metas, data)}, solana.Hash{1}, solana.TransactionPayer(owner.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == owner.PublicKey() {
			return &owner.PrivateKey
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	address, receiptData := receiptFixture(t, m.vault.VoltrVault, authority.Key().String(), 100, 1000<<48)
	receipt := ConfirmedAccount{Address: address, Owner: voltr.ProgramID.String(), Lamports: 1, Data: receiptData}
	vault := voltrVaultFixture(t, 10000)
	mint := voltrLPMintFixture(t, 1000)
	settingsData := make([]byte, 136)
	copy(settingsData[:8], squads.SettingsDiscriminator[:])
	copy(settingsData[8:24], seed[:])
	binary.LittleEndian.PutUint16(settingsData[56:58], 1)
	settingsData[87] = bump
	binary.LittleEndian.PutUint32(settingsData[88:92], 1)
	copy(settingsData[92:124], owner.PublicKey().Bytes())
	settingsData[124] = squads.FullPermissions
	settingsData[126] = 1
	binary.LittleEndian.PutUint64(settingsData[127:135], 258)
	accounts := append([]ConfirmedAccount{vault, mint, receipt, {Address: settings.String(), Owner: squads.ProgramID.String(), Lamports: 1, Data: settingsData}}, others...)
	requestSignature := tx.Signatures[0].String()
	client := newFakeChain(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		var result any
		switch req.Method {
		case "getMultipleAccounts":
			var keys []string
			if err := json.Unmarshal(req.Params[0], &keys); err != nil {
				return nil, err
			}
			values := make([]any, len(keys))
			for i, key := range keys {
				a := accountAt(accounts, key)
				if a.Address != "" {
					values[i] = map[string]any{"owner": a.Owner, "lamports": a.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
				}
			}
			result = map[string]any{"context": map[string]any{"slot": 77}, "value": values}
		case "getSignaturesForAddress":
			result = []any{map[string]any{"signature": requestSignature, "slot": 70, "err": nil, "blockTime": 1700000000, "confirmationStatus": "confirmed"}}
		case "getTransaction":
			result = map[string]any{"slot": 70, "blockTime": 1700000000, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"}, "meta": map[string]any{"err": nil, "fee": 5000, "preBalances": make([]uint64, len(tx.Message.AccountKeys)), "postBalances": make([]uint64, len(tx.Message.AccountKeys)), "innerInstructions": []any{}, "logMessages": []string{}, "preTokenBalances": []any{}, "postTokenBalances": []any{}}}
		default:
			return nil, fmt.Errorf("unexpected generation RPC %s", req.Method)
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if err != nil {
			return nil, err
		}
		return response(string(encoded)), nil
	}))
	view := &View{accounts: map[string]viewAccount{}, receipts: map[string]uint64{address: 0}, through: 77, advanced: make(chan struct{})}
	for _, a := range others {
		view.receipts[a.Address] = 0
	}
	for _, a := range accounts {
		view.accounts[a.Address] = viewAccount{ConfirmedAccount: a, slot: 77}
	}
	basis := sha256.Sum256(vault.Data)
	c := earnmax.WithdrawalConsent{WithdrawalGeneration: earnmax.WithdrawalGeneration{RequestSignature: requestSignature, RequestInstruction: 0}, Version: "1", PolicySeed: "258", Owner: owner.PublicKey().String(), Cluster: earnmax.Cluster, Program: voltr.ProgramID.String(), Vault: earnmax.Vault.String(), Settings: settings.String(), Authority: authority.Key().String(), Destination: owner.PublicKey().String(), LPRaw: "100", ReferenceUSDC: "500", ReferenceSlot: "77", FeeBasisHash: hex.EncodeToString(basis[:]), MinClaimUSDC: "490", MaxUserFeeLamports: "0", ExpiresAt: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10), Nonce: strings.Repeat("a", 64)}
	message, _ := c.CanonicalMessage()
	c.OwnerSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(owner.PrivateKey), message))
	id := insertWithdrawalCostConsent(t, d, c)
	o := Observation{ObservedAt: time.Now(), routeBatch: &routeObservationBatch{Accounts: accounts}, Snapshot: Snapshot{Slot: 77, ChainUnix: time.Now().Unix(), TotalVaultNAVRaw: 10000, WithdrawalDemandRaw: 1000}}
	net := &certifiedNAV{vault: m.vault.squads, manifest: m.SHA256, slot: 77, validUntil: time.Now().Add(exitQuoteInterval)}
	o.Snapshot.net = net
	p := &withdrawalCostPreparation{Slot: 77, view: view, chain: client, manifest: m, net: net, MaxCost: 20, NetworkUSDC: withdrawalTestUSDC(1), NetworkLamports: 5000, MessageSHA256: strings.Repeat("b", 64)}
	regenerate := func() {
		tx.Message.RecentBlockhash = solana.Hash{2}
		tx.Signatures = nil
		if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			if key == owner.PublicKey() {
				return &owner.PrivateKey
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if wire, err = tx.MarshalBinary(); err != nil {
			t.Fatal(err)
		}
		requestSignature = tx.Signatures[0].String()
	}
	return p, o, client, id, regenerate
}

func TestWithdrawalCostDatabaseLoaderUsesActualGenerationAndEscrow(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	p, o, client, id, regenerate := withdrawalLoaderFixture(t, d)
	if err := p.load(ctx, d, client, o); err != nil {
		t.Fatal(err)
	}
	if len(p.Participants) != 1 || p.Participants[0].ID != id || p.Participants[0].EscrowLP != 100 || p.Participants[0].Payable != 500 || p.EffectiveLP != 2000 {
		t.Fatalf("request/effective supply attribution incorrect: %+v", p)
	}
	if err := p.allocate(20, 20); err != nil {
		t.Fatal(err)
	}
	if p.OtherHolders != 19 || p.Participants[0].MaxCost != 1 {
		t.Fatalf("other holders omitted: %+v", p)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES('loader','route','decided')`); err != nil {
		t.Fatal(err)
	}
	if err := d.reserveWithdrawalCostRecord(ctx, "loader", p); err != nil {
		t.Fatal(err)
	}
	// Reusing the same PDA and LP cannot authorize another request signature.
	regenerate()
	p.Participants = nil
	if err := p.load(ctx, d, client, o); err == nil {
		t.Fatal("same receipt PDA accepted a different request generation")
	}
}

func TestWithdrawalCostDatabaseIdleRequesterDoesNotStallAdmittedFunding(t *testing.T) {
	d := withdrawalCostTestDB(t)
	ctx := context.Background()
	m := readyWorkerManifest(t)
	idleAddress, idleData := receiptFixture(t, m.vault.VoltrVault, solana.NewWallet().PublicKey().String(), 50, 500<<48)
	p, o, client, id, _ := withdrawalLoaderFixture(t, d, ConfirmedAccount{Address: idleAddress, Owner: voltr.ProgramID.String(), Lamports: 1, Data: idleData})
	o.Snapshot.WithdrawalDemandRaw = 1500
	if err := p.load(ctx, d, client, o); err == nil || !strings.Contains(err.Error(), "withdrawal_demand_unadmitted") {
		t.Fatalf("unadmitted demand sized a cost-bearing sale: %v", err)
	}
	o.Snapshot.WithdrawalDemandRaw = 1000
	p.Participants = nil
	if err := p.load(ctx, d, client, o); err != nil {
		t.Fatalf("one idle requester stalled admitted funding: %v", err)
	}
	if err := p.allocate(40, 40); err != nil {
		t.Fatal(err)
	}
	var admitted, waiting *withdrawalCostParticipant
	for i := range p.Participants {
		if p.Participants[i].Receipt == idleAddress {
			waiting = &p.Participants[i]
		} else {
			admitted = &p.Participants[i]
		}
	}
	if len(p.Participants) != 2 || admitted == nil || admitted.ID != id || admitted.MaxCost != 2 || waiting == nil || waiting.Wait == "" || waiting.MaxCost != 1 || p.OtherHolders != 37 {
		t.Fatalf("waiting requester share moved or blocked: %+v", p)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status) VALUES('idle','route','decided')`); err != nil {
		t.Fatal(err)
	}
	if err := d.reserveWithdrawalCostRecord(ctx, "idle", p); err != nil {
		t.Fatal(err)
	}
	var wait string
	var cost int64
	if err := d.pool.QueryRow(ctx, `SELECT wait_reason,max_cost_usdc FROM loyal_yield.backyard_withdrawal_cost_reservations WHERE operation_id='idle' AND receipt=$1 AND consent_id IS NULL`, idleAddress).Scan(&wait, &cost); err != nil || wait != waiting.Wait || cost != 1 {
		t.Fatalf("waiting requester not journaled with its own reason: %q %d %v", wait, cost, err)
	}
}
