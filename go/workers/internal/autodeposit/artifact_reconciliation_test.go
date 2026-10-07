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

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
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
	b, e := NewSweepWireBuilder(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, 32)), func(context.Context, []string, ...string) (int64, []backyard.ConfirmedAccount, error) {
		return 0, nil, errors.New("unexpected account read")
	})
	if e != nil {
		t.Fatal(e)
	}
	return f, target, b
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
	if e = VerifyCanonicalSubscriptionPolicyAccount(request, goldenHex(t, f.PolicyDataHex)); e != nil {
		t.Fatalf("canonical advanced policy rejected: %v", e)
	}
	if e = VerifyCanonicalSubscriptionPolicyAccount(request, goldenHex(t, f.WeakenedPolicyDataHex)); e == nil {
		t.Fatal("dropping token-account owner constraint accepted")
	}
	for _, mutation := range []struct {
		name   string
		change func([]byte)
	}{{"threshold", func(d []byte) { binary.LittleEndian.PutUint16(d[102:104], 2) }}, {"signerPermissions", func(d []byte) { d[101] = 3 }}, {"signer", func(d []byte) { d[69] ^= 1 }}} {
		t.Run(mutation.name, func(t *testing.T) {
			d := goldenHex(t, f.PolicyDataHex)
			mutation.change(d)
			if e := VerifyCanonicalSubscriptionPolicyAccount(request, d); e == nil {
				t.Fatal("mutated policy accepted")
			}
		})
	}
}
func goldenCreatorReceipt(t *testing.T, f artifactGolden) ArtifactReceipt {
	t.Helper()
	wire, e := base64.StdEncoding.DecodeString(f.WireBase64)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := solana.TransactionFromBytes(wire)
	if e != nil {
		t.Fatal(e)
	}
	r := ArtifactReceipt{Signature: tx.Signatures[0].String(), Slot: 125, Wire: wire, PreLamports: make([]uint64, len(tx.Message.AccountKeys)), PostLamports: make([]uint64, len(tx.Message.AccountKeys))}
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
func resignArtifactReceipt(t *testing.T, r ArtifactReceipt, mutate func(*solana.Transaction), signer ed25519.PrivateKey) ArtifactReceipt {
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
	r.Signature = tx.Signatures[0].String()
	return r
}
func TestArtifactCreatorRequiresExactCreationSignatureAndFullMatrix(t *testing.T) {
	f, target, b := artifactFixture(t)
	receipt := goldenCreatorReceipt(t, f)
	p, e := b.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, receipt)
	if e != nil || p.account != target.Policy || p.signature != receipt.Signature || p.slot != 125 {
		t.Fatalf("official creator proof=%+v err=%v", p, e)
	}
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	for _, tc := range []struct {
		name   string
		mutate func(ArtifactReceipt) ArtifactReceipt
	}{
		{"existingAccountTouch", func(r ArtifactReceipt) ArtifactReceipt {
			r.PreLamports = append([]uint64(nil), r.PreLamports...)
			for i := range r.PreLamports {
				r.PreLamports[i] = 1
			}
			return r
		}},
		{"wrongReceiptSignature", func(r ArtifactReceipt) ArtifactReceipt { r.Signature = solana.Signature{}.String(); return r }},
		{"unsignedBytes", func(r ArtifactReceipt) ArtifactReceipt {
			r.Wire = append([]byte(nil), r.Wire...)
			r.Wire[3] ^= 1
			return r
		}},
		{"weakenedCreation", func(r ArtifactReceipt) ArtifactReceipt {
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data = goldenHex(t, f.WeakenedCreatorDataHex) }, root)
		}},
		{"updateInsteadOfCreate", func(r ArtifactReceipt) ArtifactReceipt {
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data[13] = 8 }, root)
		}},
		{"impostorRoot", func(r ArtifactReceipt) ArtifactReceipt {
			impostor := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{19}, 32))
			return resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.AccountKeys[0] = solana.PrivateKey(impostor).PublicKey() }, impostor)
		}},
		{"extraneousLoadedAddresses", func(r ArtifactReceipt) ArtifactReceipt { r.LoadedReadonly = []string{target.Vault}; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := b.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, tc.mutate(receipt)); e == nil {
				t.Fatal("non-creator accepted")
			}
		})
	}
}
func TestArtifactPersonalRootSeedAndAuthorityProof(t *testing.T) {
	f, target, _ := artifactFixture(t)
	a := backyard.ConfirmedAccount{Address: f.Settings, Owner: squadsProgramID, Data: goldenHex(t, f.SettingsDataHex)}
	if e := verifyArtifactRoot(a, target); e != nil {
		t.Fatal(e)
	}
	for _, offset := range []int{8, 24, 56, 58, 88, 93} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			changed := a
			changed.Data = append([]byte(nil), a.Data...)
			changed.Data[offset] ^= 1
			if e := verifyArtifactRoot(changed, target); e == nil {
				t.Fatalf("changed settings byte %d accepted", offset)
			}
		})
	}
	other := target
	other.RootAuthority = f.DelegatedSigner
	if verifyArtifactRoot(a, other) == nil {
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
	if p, e := b.verifyArtifactCreator(t.Context(), target, ArtifactDelegation, r); e != nil || p.account != target.RecurringDelegation {
		t.Fatalf("official delegation proof=%+v err=%v", p, e)
	}
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32))
	for _, offset := range []int{1, 9, 17, 25, 33} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			changed := resignArtifactReceipt(t, r, func(tx *solana.Transaction) { tx.Message.Instructions[0].Data[offset] ^= 1 }, root)
			if _, e := b.verifyArtifactCreator(t.Context(), target, ArtifactDelegation, changed); e == nil {
				t.Fatalf("delegation parameter byte %d changed", offset)
			}
		})
	}
}

type artifactHistoryFake struct {
	entries  []ArtifactHistoryEntry
	receipts map[string]ArtifactReceipt
	calls    int
	address  string
	limit    int
}

func (h *artifactHistoryFake) ArtifactHistory(_ context.Context, address string, limit int) ([]ArtifactHistoryEntry, error) {
	h.address = address
	h.limit = limit
	return h.entries, nil
}
func (h *artifactHistoryFake) ArtifactReceipt(_ context.Context, signature string) (ArtifactReceipt, error) {
	h.calls++
	return h.receipts[signature], nil
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
	accounts := map[string]backyard.ConfirmedAccount{f.Settings: {Address: f.Settings, Owner: squadsProgramID, Data: goldenHex(t, f.SettingsDataHex)}, f.Policy: {Address: f.Policy, Owner: squadsProgramID, Data: goldenHex(t, f.PolicyDataHex)}, f.SubscriptionAuthority: {Address: f.SubscriptionAuthority, Owner: SubscriptionsProgramID}, f.RecurringDelegation: {Address: f.RecurringDelegation, Owner: SubscriptionsProgramID, Data: testDelegationData(f.Wallet, f.Vault, USDCMint, uint64(f.MaxAmountPerPeriod), 0)}, f.WalletATA: {Address: f.WalletATA, Owner: splTokenID, Data: token}}
	b.read = func(_ context.Context, addresses []string, _ ...string) (int64, []backyard.ConfirmedAccount, error) {
		out := make([]backyard.ConfirmedAccount, 0, len(addresses))
		for _, address := range addresses {
			out = append(out, accounts[address])
		}
		return 200, out, nil
	}
}
func TestArtifactHistoryIsBoundedAndRoleFiltered(t *testing.T) {
	f, target, b := artifactFixture(t)
	installArtifactSnapshot(t, f, b)
	receipt := goldenCreatorReceipt(t, f)
	touch := receipt
	touch.Slot = 126
	touch.PreLamports = append([]uint64(nil), receipt.PostLamports...)
	history := &artifactHistoryFake{entries: []ArtifactHistoryEntry{{Signature: "failed", Slot: 127, Failed: true}, {Signature: receipt.Signature, Slot: 125}}, receipts: map[string]ArtifactReceipt{receipt.Signature: receipt}}
	reader := ArtifactProofReader{Wires: b, History: history}
	if _, e := reader.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); e != nil {
		t.Fatal(e)
	}
	if history.limit != 32 || history.calls != 1 || history.address != target.Policy {
		t.Fatalf("history bound/role mismatch: %+v", history)
	}
	history.entries = make([]ArtifactHistoryEntry, 33)
	if _, e := reader.FindCreationProof(t.Context(), target, ArtifactPolicy, 100); e == nil {
		t.Fatal("overlong history accepted")
	}
	history.entries = []ArtifactHistoryEntry{{Signature: receipt.Signature, Slot: 126}}
	history.receipts[receipt.Signature] = touch
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
	b.read = func(_ context.Context, addresses []string, _ ...string) (int64, []backyard.ConfirmedAccount, error) {
		if len(addresses) != 1 || addresses[0] != f.LookupKey {
			return 0, nil, errors.New("unexpected lookup")
		}
		return 500, []backyard.ConfirmedAccount{{Address: f.LookupKey, Owner: "AddressLookupTab1e1111111111111111111111111", Data: tableData}}, nil
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
	keys := make([]string, 0)
	for _, key := range tx.Message.AccountKeys {
		keys = append(keys, key.String())
	}
	keys = append(keys, f.LoadedWritable...)
	keys = append(keys, f.LoadedReadonly...)
	r := ArtifactReceipt{Signature: tx.Signatures[0].String(), Slot: 125, Wire: wire, PreLamports: make([]uint64, len(keys)), PostLamports: make([]uint64, len(keys)), LoadedWritable: f.LoadedWritable, LoadedReadonly: f.LoadedReadonly}
	for i, key := range keys {
		r.PreLamports[i] = 1
		r.PostLamports[i] = 1
		if key == target.Policy {
			r.PreLamports[i] = 0
			r.PostLamports[i] = 100
		}
	}
	if _, e = b.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, r); e != nil {
		t.Fatalf("official v0 creator rejected: %v", e)
	}
	changed := r
	changed.Signature = tx.Signatures[1].String()
	if _, e = b.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, changed); e == nil {
		t.Fatal("root signature substituted for receipt payer signature")
	}
	changed = r
	changed.LoadedWritable = append([]string(nil), r.LoadedWritable...)
	changed.LoadedWritable[0] = f.Wallet
	if _, e = b.verifyArtifactCreator(t.Context(), target, ArtifactPolicy, changed); e == nil {
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
	policyProof, e := b.verifyArtifactCreator(ctx, target, ArtifactPolicy, receipt)
	if e != nil {
		t.Fatal(e)
	}
	f.WireBase64 = f.DelegationWireBase64
	f.Policy = f.RecurringDelegation
	delegationReceipt := goldenCreatorReceipt(t, f)
	delegationProof, e := b.verifyArtifactCreator(ctx, target, ArtifactDelegation, delegationReceipt)
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
	if loaded.PolicySignature == nil || *loaded.PolicySignature != receipt.Signature || loaded.PolicyConfirmedSlot == nil || *loaded.PolicyConfirmedSlot != receipt.Slot || loaded.DelegationSignature == nil || *loaded.DelegationSignature != delegationReceipt.Signature {
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
