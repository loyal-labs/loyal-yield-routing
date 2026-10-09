package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type artifactGolden struct {
	V0WireBase64, LookupKey                                                                                                                               string
	LookupAddresses, LoadedWritable, LoadedReadonly                                                                                                       []string
	SourceCommit, Settings, RootAuthority, Payer, DelegatedSigner, Wallet, Vault, WalletATA, VaultATA, Policy, SubscriptionAuthority, RecurringDelegation string
	PolicySeed, MaxAmountPerPeriod, Nonce, PeriodLength, StartTimestamp, ExpiryTimestamp                                                                  int64
	DataHex, PolicyDataHex, WeakenedPolicyDataHex, WeakenedCreatorDataHex, SettingsDataHex, WireBase64                                                    string
	DelegationWireBase64                                                                                                                                  string
	Accounts                                                                                                                                              []fleet.InstructionAccount
}

func artifactFixture(t *testing.T) (artifactGolden, ArtifactTarget, *SweepWireBuilder) {
	t.Helper()
	var f artifactGolden
	raw, e := os.ReadFile("../../testdata/autodeposit/canonical-policy-creator.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &f); e != nil {
		t.Fatal(e)
	}
	if f.SourceCommit != "45de590113312d54de7585a710620a8a80b504db" {
		t.Fatal("unexpected source pin")
	}
	target := ArtifactTarget{ControlTarget: ControlTarget{Cluster: mainnetCluster, TargetID: 1, SetupGeneration: 3, PolicySeed: f.PolicySeed, Settings: f.Settings, Wallet: f.Wallet, WalletTokenATA: f.WalletATA, Vault: f.Vault, VaultTokenATA: f.VaultATA, Mint: USDCMint, Policy: f.Policy, SubscriptionAuthority: f.SubscriptionAuthority, RecurringDelegation: f.RecurringDelegation, Nonce: &f.Nonce, MaxAmountPerPeriod: &f.MaxAmountPerPeriod, StartTimestamp: &f.StartTimestamp}, RootAuthority: f.RootAuthority, PeriodLength: &f.PeriodLength, ExpiryTimestamp: &f.ExpiryTimestamp}
	b, e := NewSweepWireBuilder(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, 32)), func(context.Context, int64, []string, ...string) (int64, []*chain.Account, error) {
		return 0, nil, errors.New("unexpected account read")
	})
	if e != nil {
		t.Fatal(e)
	}
	return f, target, b
}
func mustKeys(t *testing.T, addresses []string) []solana.PublicKey {
	t.Helper()
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		keys[i] = mustKey(address)
	}
	return keys
}
func goldenHex(t *testing.T, s string) []byte {
	t.Helper()
	v, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func artifactRequest(f artifactGolden) CanonicalSubscriptionPolicyRequest {
	return CanonicalSubscriptionPolicyRequest{Settings: f.Settings, RootAuthority: f.RootAuthority, Payer: f.Payer, DelegatedSigner: f.DelegatedSigner, PolicySeed: uint64(f.PolicySeed), Wallet: f.Wallet, Vault: f.Vault, MaxAmountPerPeriod: uint64(f.MaxAmountPerPeriod)}
}
func TestCanonicalSubscriptionCreatorOfficialSolitaABI(t *testing.T) {
	f, _, _ := artifactFixture(t)
	request := artifactRequest(f)
	ix, e := BuildCanonicalSubscriptionPolicy(request)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(ix.Data, goldenHex(t, f.DataHex)) || !reflect.DeepEqual(ix.Accounts, f.Accounts) {
		t.Fatalf("creator differs from independent Solita SDK golden: accounts=%+v", ix.Accounts)
	}
	if e = VerifyCanonicalSubscriptionPolicyAccount(request, &chain.Account{Owner: squads.ProgramID, Data: goldenHex(t, f.PolicyDataHex)}); e != nil {
		t.Fatalf("canonical advanced policy rejected: %v", e)
	}
	if e = VerifyCanonicalSubscriptionPolicyAccount(request, &chain.Account{Owner: squads.ProgramID, Data: goldenHex(t, f.WeakenedPolicyDataHex)}); e == nil {
		t.Fatal("dropping token-account owner constraint accepted")
	}
	for _, mutation := range []struct {
		name   string
		change func([]byte)
	}{{"threshold", func(d []byte) { binary.LittleEndian.PutUint16(d[102:104], 2) }}, {"signerPermissions", func(d []byte) { d[101] = 3 }}, {"signer", func(d []byte) { d[69] ^= 1 }}} {
		t.Run(mutation.name, func(t *testing.T) {
			d := goldenHex(t, f.PolicyDataHex)
			mutation.change(d)
			if e := VerifyCanonicalSubscriptionPolicyAccount(request, &chain.Account{Owner: squads.ProgramID, Data: d}); e == nil {
				t.Fatal("mutated policy accepted")
			}
		})
	}
}

// creatorReceipt is a confirmed receipt and the signature it was read by.
type creatorReceipt struct {
	signature solana.Signature
	chain.Receipt
}

func verifyCreator(t *testing.T, b *SweepWireBuilder, target ArtifactTarget, role ArtifactRole, r creatorReceipt) (VerifiedArtifactCreationProof, error) {
	t.Helper()
	return b.verifyArtifactCreator(t.Context(), target, role, r.signature, r.Receipt)
}

func goldenCreatorReceipt(t *testing.T, f artifactGolden) creatorReceipt {
	t.Helper()
	wire, e := base64.StdEncoding.DecodeString(f.WireBase64)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := solana.TransactionFromBytes(wire)
	if e != nil {
		t.Fatal(e)
	}
	r := creatorReceipt{tx.Signatures[0], chain.Receipt{Slot: 125, Wire: wire, PreLamports: make([]uint64, len(tx.Message.AccountKeys)), PostLamports: make([]uint64, len(tx.Message.AccountKeys))}}
	for i, k := range tx.Message.AccountKeys {
		r.PreLamports[i] = 1
		r.PostLamports[i] = 1
		if k.String() == f.Policy {
			r.PreLamports[i] = 0
			r.PostLamports[i] = 100
		}
	}
	return r
}
func resignArtifactReceipt(t *testing.T, r creatorReceipt, mutate func(*solana.Transaction), signer ed25519.PrivateKey) creatorReceipt {
	t.Helper()
	tx, e := solana.TransactionFromBytes(r.Wire)
	if e != nil {
		t.Fatal(e)
	}
	mutate(tx)
	key := solana.PrivateKey(signer)
	if _, e = tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k == key.PublicKey() {
			return &key
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	r.Wire = mustMarshalTransaction(t, tx)
	r.signature = tx.Signatures[0]
	return r
}
func TestArtifactCreatorRequiresExactCreationSignatureAndFullMatrix(t *testing.T) {
	f, target, b := artifactFixture(t)
	receipt := goldenCreatorReceipt(t, f)
	p, e := verifyCreator(t, b, target, ArtifactPolicy, receipt)
	if e != nil || p.account != target.Policy || p.signature != receipt.signature.String() || p.slot != 125 {
		t.Fatalf("official creator proof=%+v err=%v", p, e)
	}
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	for _, tc := range []struct {
		name   string
		mutate func(creatorReceipt) creatorReceipt
	}{
		{"existingAccountTouch", func(r creatorReceipt) creatorReceipt {
			r.PreLamports = append([]uint64(nil), r.PreLamports...)
			for i := range r.PreLamports {
				r.PreLamports[i] = 1
			}
			return r
		}},
		{"wrongReceiptSignature", func(r creatorReceipt) creatorReceipt { r.signature = solana.Signature{}; return r }},
		{"unsignedBytes", func(r creatorReceipt) creatorReceipt {
			r.Wire = append([]byte(nil), r.Wire...)
			r.Wire[3] ^= 1
			return r
		}},
		{"weakenedCreation", func(r creatorReceipt) creatorReceipt {
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data = goldenHex(t, f.WeakenedCreatorDataHex) }, root)
		}},
		{"updateInsteadOfCreate", func(r creatorReceipt) creatorReceipt {
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data[13] = 8 }, root)
		}},
		{"impostorRoot", func(r creatorReceipt) creatorReceipt {
			impostor := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{19}, 32))
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.AccountKeys[0] = solana.PrivateKey(impostor).PublicKey() }, impostor)
		}},
		{"extraneousLoadedAddresses", func(r creatorReceipt) creatorReceipt {
			r.LoadedReadonly = []solana.PublicKey{mustKey(target.Vault)}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := verifyCreator(t, b, target, ArtifactPolicy, tc.mutate(receipt)); e == nil {
				t.Fatal("non-creator accepted")
			}
		})
	}
}
func TestArtifactPersonalRootSeedAndAuthorityProof(t *testing.T) {
	f, target, _ := artifactFixture(t)
	a := testAccount{Address: f.Settings, Owner: squads.ProgramID.String(), Data: goldenHex(t, f.SettingsDataHex)}
	if e := verifyArtifactRoot(a.chainAccount(), target); e != nil {
		t.Fatal(e)
	}
	for _, offset := range []int{8, 24, 56, 58, 88, 93} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			changed := a
			changed.Data = append([]byte(nil), a.Data...)
			changed.Data[offset] ^= 1
			if e := verifyArtifactRoot(changed.chainAccount(), target); e == nil {
				t.Fatalf("changed settings byte %d accepted", offset)
			}
		})
	}
	other := target
	other.RootAuthority = f.DelegatedSigner
	if verifyArtifactRoot(a.chainAccount(), other) == nil {
		t.Fatal("foreign root authority accepted")
	}
}
func TestArtifactMemoRetainsExactSecurityPayload(t *testing.T) {
	expected := []byte{1, 2, 3, 0}
	if !canonicalSettingsCreatorData(expected, expected) {
		t.Fatal("no memo rejected")
	}
	actual := []byte{1, 2, 3, 1, 2, 0, 0, 0, 'o', 'k'}
	if !canonicalSettingsCreatorData(actual, expected) {
		t.Fatal("memo rejected")
	}
	actual[1] ^= 1
	if canonicalSettingsCreatorData(actual, expected) {
		t.Fatal("security payload changed")
	}
	actual[1] ^= 1
	actual[len(actual)-1] = 0xff
	if canonicalSettingsCreatorData(actual, expected) {
		t.Fatal("non-UTF8 memo accepted")
	}
}

func TestArtifactDelegationCreatorBindsNonceBudgetPeriodAndWallet(t *testing.T) {
	f, target, b := artifactFixture(t)
	f.WireBase64 = f.DelegationWireBase64
	f.Policy = f.RecurringDelegation
	r := goldenCreatorReceipt(t, f)
	if p, e := verifyCreator(t, b, target, ArtifactDelegation, r); e != nil || p.account != target.RecurringDelegation {
		t.Fatalf("official delegation proof=%+v err=%v", p, e)
	}
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	for _, offset := range []int{1, 9, 17, 25, 33} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			changed := resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data[offset] ^= 1 }, root)
			if _, e := verifyCreator(t, b, target, ArtifactDelegation, changed); e == nil {
				t.Fatalf("delegation parameter byte %d changed", offset)
			}
		})
	}
}

// artifactHistoryFake serves history pages keyed by their before cursor.
type artifactHistoryFake struct {
	pages    map[solana.Signature][]chain.Signed
	receipts map[solana.Signature]chain.Receipt
	before   []solana.Signature
	calls    int
	address  solana.PublicKey
	limit    int
}

func (h *artifactHistoryFake) History(_ context.Context, address solana.PublicKey, limit int, before solana.Signature, _ rpc.CommitmentType, _ uint64) ([]chain.Signed, error) {
	h.address, h.limit = address, limit
	h.before = append(h.before, before)
	return h.pages[before], nil
}

func (h *artifactHistoryFake) Receipt(_ context.Context, signature solana.Signature, _ rpc.CommitmentType) (chain.Receipt, error) {
	h.calls++
	receipt, ok := h.receipts[signature]
	if !ok {
		return chain.Receipt{}, chain.ErrNotFound
	}
	return receipt, nil
}

// testSignature is a distinct signature for history entries no test reads.
func testSignature(n int) solana.Signature {
	var signature solana.Signature
	binary.LittleEndian.PutUint64(signature[:], uint64(n)+1)
	return signature
}
func installArtifactSnapshot(t *testing.T, f artifactGolden, b *SweepWireBuilder) {
	t.Helper()
	token := make([]byte, 165)
	mint, wallet, authority := mustKey(USDCMint), mustKey(f.Wallet), mustKey(f.SubscriptionAuthority)
	copy(token[:32], mint[:])
	copy(token[32:64], wallet[:])
	token[108] = 1
	binary.LittleEndian.PutUint64(token[64:72], 9_000_000)
	binary.LittleEndian.PutUint32(token[72:76], 1)
	copy(token[76:108], authority[:])
	accounts := map[string]testAccount{f.Settings: {Address: f.Settings, Owner: squads.ProgramID.String(), Data: goldenHex(t, f.SettingsDataHex)}, f.Policy: {Address: f.Policy, Owner: squads.ProgramID.String(), Data: goldenHex(t, f.PolicyDataHex)}, f.SubscriptionAuthority: {Address: f.SubscriptionAuthority, Owner: SubscriptionsProgramID}, f.RecurringDelegation: {Address: f.RecurringDelegation, Owner: SubscriptionsProgramID, Data: testDelegationData(f.Wallet, f.Vault, USDCMint, uint64(f.MaxAmountPerPeriod), 0)}, f.WalletATA: {Address: f.WalletATA, Owner: splTokenID, Data: token}}
	b.read = fixtureReader(200, accounts)
}
func TestArtifactHistoryIsBoundedAndRoleFiltered(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	touch := receipt
	touch.Slot = 126
	touch.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	history := &artifactHistoryFake{pages: map[solana.Signature][]chain.Signed{{}: {{Signature: testSignature(0), Slot: 127, Failed: true}, {Signature: receipt.signature, Slot: 125}}}, receipts: map[solana.Signature]chain.Receipt{receipt.signature: receipt.Receipt}}
	reader := ArtifactProofReader{Wires: b, History: history}
	if _, e := reader.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); e != nil {
		t.Fatal(e)
	}
	if history.limit != 32 || history.calls != 1 || history.address != mustKey(target.Policy) {
		t.Fatalf("history bound/role mismatch: %+v", history)
	}
	history.pages[solana.Signature{}] = make([]chain.Signed, 33)
	if _, e := reader.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); e == nil {
		t.Fatal("overlong history accepted")
	}
	history.pages[solana.Signature{}] = []chain.Signed{{Signature: receipt.signature, Slot: 126}}
	history.receipts[receipt.signature] = touch.Receipt
	if _, e := reader.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); !errors.Is(e, ErrArtifactCreationProofPending) {
		t.Fatalf("touch became creator: %v", e)
	}
}

func TestArtifactV0CreatorBindsExternalPayerAndPinnedLookup(t *testing.T) {
	f, target, b := artifactFixture(t)
	tableData := make([]byte, 56+32*len(f.LookupAddresses))
	binary.LittleEndian.PutUint32(tableData[:4], 1)
	binary.LittleEndian.PutUint64(tableData[4:12], ^uint64(0))
	binary.LittleEndian.PutUint64(tableData[12:20], 100)
	for i, address := range f.LookupAddresses {
		key := mustKey(address)
		copy(tableData[56+i*32:], key[:])
	}
	b.read = func(_ context.Context, _ int64, addresses []string, _ ...string) (int64, []*chain.Account, error) {
		if len(addresses) != 1 || addresses[0] != f.LookupKey {
			return 0, nil, errors.New("unexpected lookup")
		}
		return 500, []*chain.Account{testAccount{Address: f.LookupKey, Owner: solana.AddressLookupTableProgramID.String(), Data: tableData}.chainAccount()}, nil
	}
	wire, e := base64.StdEncoding.DecodeString(f.V0WireBase64)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := solana.TransactionFromBytes(wire)
	if e != nil {
		t.Fatal(e)
	}
	if len(tx.Signatures) != 2 {
		t.Fatal("external payer/root multisignature absent")
	}
	writable, readonly := mustKeys(t, f.LoadedWritable), mustKeys(t, f.LoadedReadonly)
	keys := append(append(append([]solana.PublicKey(nil), tx.Message.AccountKeys...), writable...), readonly...)
	r := creatorReceipt{tx.Signatures[0], chain.Receipt{Slot: 125, Wire: wire, PreLamports: make([]uint64, len(keys)), PostLamports: make([]uint64, len(keys)), LoadedWritable: writable, LoadedReadonly: readonly}}
	for i, key := range keys {
		r.PreLamports[i] = 1
		r.PostLamports[i] = 1
		if key == mustKey(target.Policy) {
			r.PreLamports[i] = 0
			r.PostLamports[i] = 100
		}
	}
	if _, e = verifyCreator(t, b, target, ArtifactPolicy, r); e != nil {
		t.Fatalf("official v0 creator rejected: %v", e)
	}
	changed := r
	changed.signature = tx.Signatures[1]
	if _, e = verifyCreator(t, b, target, ArtifactPolicy, changed); e == nil {
		t.Fatal("root signature substituted for receipt payer signature")
	}
	changed = r
	changed.LoadedWritable = append([]solana.PublicKey(nil), r.LoadedWritable...)
	changed.LoadedWritable[0] = mustKey(f.Wallet)
	if _, e = verifyCreator(t, b, target, ArtifactPolicy, changed); e == nil {
		t.Fatal("provider substituted loaded address")
	}
}

func TestArtifactBackfillRequiresLiveLeaseGenerationAndExactStagePair(t *testing.T) {
	f, target, b := artifactFixture(t)
	store := integrationStore(t)
	ctx := t.Context()
	seeded := seedIntegrationTarget(t, store, "artifact-backfill")
	target.TargetID = seeded.TargetID
	_, e := store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET settings=$2,authority=$3,policy_seed=$4,policy_account=$5,wallet=$6,wallet_usdc_ata=$7,wallet_token_ata=$7,vault_pubkey=$8,vault_usdc_ata=$9,vault_token_ata=$9,subscription_authority=$10,recurring_delegation=$11,recurring_delegation_nonce=$12,max_amount_per_period=$13,period_length_seconds=$14,start_timestamp=$15,recurring_delegation_expiry_timestamp=$16,setup_generation=$17 WHERE id=$1`, target.TargetID, target.Settings, target.RootAuthority, target.PolicySeed, target.Policy, target.Wallet, target.WalletTokenATA, target.Vault, target.VaultTokenATA, target.SubscriptionAuthority, target.RecurringDelegation, *target.Nonce, *target.MaxAmountPerPeriod, *target.PeriodLength, *target.StartTimestamp, *target.ExpiryTimestamp, target.SetupGeneration)
	if e != nil {
		t.Fatal(e)
	}
	loaded, e := store.LoadArtifactTarget(ctx, target.TargetID)
	if e != nil || loaded == nil {
		t.Fatalf("load %+v %v", loaded, e)
	}
	target = *loaded
	receipt := goldenCreatorReceipt(t, f)
	policyProof, e := verifyCreator(t, b, target, ArtifactPolicy, receipt)
	if e != nil {
		t.Fatal(e)
	}
	f.WireBase64 = f.DelegationWireBase64
	f.Policy = f.RecurringDelegation
	delegationReceipt := goldenCreatorReceipt(t, f)
	delegationProof, e := verifyCreator(t, b, target, ArtifactDelegation, delegationReceipt)
	if e != nil {
		t.Fatal(e)
	}
	request := claimControlRequest(t, store, target.TargetID, 200, "artifact-owner")
	if e = store.BackfillArtifactCreationProof(ctx, request, "wrong-owner", policyProof); !errors.Is(e, ErrOwnershipLost) {
		t.Fatalf("foreign lease: %v", e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", policyProof); e != nil {
		t.Fatal(e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", policyProof); e != nil {
		t.Fatalf("idempotent proof: %v", e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", delegationProof); e != nil {
		t.Fatalf("second role using same generation: %v", e)
	}
	loaded, e = store.LoadArtifactTarget(ctx, target.TargetID)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.PolicySignature == nil || *loaded.PolicySignature != receipt.signature.String() || loaded.PolicyConfirmedSlot == nil || *loaded.PolicyConfirmedSlot != int64(receipt.Slot) || loaded.DelegationSignature == nil || *loaded.DelegationSignature != delegationReceipt.signature.String() {
		t.Fatalf("exact role stage pairs missing: %+v", loaded)
	}
	var processed int64
	if e = store.pool.QueryRow(ctx, `SELECT processed_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&processed); e != nil || processed >= 200 {
		t.Fatalf("artifact writer consumed control demand: %d %v", processed, e)
	}
	if _, e = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET setup_generation=setup_generation+1 WHERE id=$1`, target.TargetID); e != nil {
		t.Fatal(e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", policyProof); e == nil {
		t.Fatal("stale generation accepted")
	}
	if _, e = store.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET setup_generation=$2,policy_confirmed_slot=NULL,policy_signature='conflicting-retained-proof' WHERE id=$1`, target.TargetID, target.SetupGeneration); e != nil {
		t.Fatal(e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", policyProof); e == nil {
		t.Fatal("partial conflicting stage pair combined")
	}
	if _, e = store.pool.Exec(ctx, `UPDATE loyal_yield.autodeposit_reconciliation_requests SET claim_expires_at=now()-interval '1 second' WHERE target_id=$1`, target.TargetID); e != nil {
		t.Fatal(e)
	}
	if e = store.BackfillArtifactCreationProof(ctx, request, "artifact-owner", policyProof); !errors.Is(e, ErrOwnershipLost) {
		t.Fatalf("expired lease: %v", e)
	}
}
