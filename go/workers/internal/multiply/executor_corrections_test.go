package multiply

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func signedWireFixture(t *testing.T, dual bool) *SignedOperation {
	t.Helper()
	delegate := testDelegateSeed()
	payer := delegate
	if dual {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = 77
		payer = ed25519.NewKeyFromSeed(seed)
	}
	pk := solana.PublicKey(payer[32:])
	dk := solana.PublicKey(delegate[32:])
	ix := solana.NewInstruction(solana.SystemProgramID, solana.AccountMetaSlice{solana.Meta(dk).SIGNER()}, []byte{9, 8, 7})
	var hash solana.Hash
	if _, err := rand.Read(hash[:]); err != nil {
		t.Fatal(err)
	}
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, hash, solana.TransactionPayer(pk))
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		var keyBytes ed25519.PrivateKey
		if key == pk {
			keyBytes = payer
		} else if key == dk {
			keyBytes = delegate
		} else {
			return nil
		}
		sdk := solana.PrivateKey(keyBytes)
		return &sdk
	})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := NewSignedOperation(wire, tx.Signatures[0].String(), hash.String(), 331900000)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestRecoveryVerifiesDualSignaturesAndRejectsTrailingBytes(t *testing.T) {
	for _, dual := range []bool{false, true} {
		signed := signedWireFixture(t, dual)
		mh, err := MessageSHA256(signed.Wire)
		if err != nil {
			t.Fatal(err)
		}
		op := &MultiplyOperation{SignedWire: signed.Wire, SignedWireSHA256: &signed.WireSHA256, TransactionSignature: &signed.TransactionSignature, RecentBlockhash: &signed.RecentBlockhash, MessageSHA256: &mh}
		if _, err := PersistedTransaction(op); err != nil {
			t.Fatal(err)
		}
		op.SignedWire = append(append([]byte(nil), signed.Wire...), 0)
		digest := sha256.Sum256(op.SignedWire)
		hash := hexEncode(digest[:])
		op.SignedWireSHA256 = &hash
		if _, err := PersistedTransaction(op); err == nil {
			t.Fatal("trailing bytes accepted")
		}
	}
}

func TestPrepareContainsOnlyWrappedTerminalAndCanonicalBudget(t *testing.T) {
	executor, _, _ := testExecutor(t)
	built := &BuiltOperation{PolicyInstructions: []Instruction{{ProgramID: solana.SystemProgramID, Accounts: []AccountMeta{{PubKey: fixtureKey(44), IsWritable: true}}, Data: []byte{1, 2, 3}}}}
	signed, _, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(45), 1, []byte{0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := decodeVerifiedTransaction(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Message.Instructions) != 2 {
		t.Fatalf("raw terminal leaked: %d instructions", len(tx.Message.Instructions))
	}
	budget := tx.Message.Instructions[0].Data
	if len(budget) != 5 || budget[0] != 2 || binary.LittleEndian.Uint32(budget[1:]) != 400000 {
		t.Fatal("invalid compute limit")
	}
	terminal := tx.Message.Instructions[1].Data
	if terminal[12] != 1 || binary.LittleEndian.Uint32(terminal[13:17]) != 1 || terminal[17] != 0 {
		t.Fatal("constraint indexes omitted Borsh Some tag")
	}
}

// lookupTableAccount is an active lookup table holding addresses.
func lookupTableAccount(key solana.PublicKey, addresses ...solana.PublicKey) *chain.Account {
	data := make([]byte, 56, 56+32*len(addresses))
	binary.LittleEndian.PutUint32(data[:4], 1)
	binary.LittleEndian.PutUint64(data[4:12], math.MaxUint64)
	for _, address := range addresses {
		data = append(data, address[:]...)
	}
	return &chain.Account{Key: key, Owner: solana.AddressLookupTableProgramID, Lamports: 1, Data: data}
}

func TestVersionedDualSignerWireAndFeeCap(t *testing.T) {
	key := testDelegateSeed()
	seed := make([]byte, 32)
	seed[0] = 99
	payer := ed25519.NewKeyFromSeed(seed)
	tableKey, loaded := fixtureKey(61), fixtureKey(62)
	fake := &fakeChain{genesis: solana.MustHashFromBase58(mainnetGenesisHash), blockhash: solana.Hash(fixtureKey(9)), lastValid: 100, slot: 77, fee: 20000,
		accounts: map[solana.PublicKey]*chain.Account{tableKey: lookupTableAccount(tableKey, loaded)}}
	executor, err := NewExecutorWithFeePayer(fake, payer, key)
	if err != nil {
		t.Fatal(err)
	}
	built := &BuiltOperation{LookupTables: []solana.PublicKey{tableKey}, PolicyInstructions: []Instruction{{ProgramID: solana.SystemProgramID, Accounts: []AccountMeta{{PubKey: loaded, IsWritable: true}}, Data: []byte{4}}}}
	signed, _, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(45), 0, []byte{0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := decodeVerifiedTransaction(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Message.GetVersion() != solana.MessageVersionV0 || len(tx.Signatures) != 2 || len(tx.Message.GetAddressTableLookups()) != 1 {
		t.Fatal("versioned dual signer recipe was not preserved")
	}
	mh, err := MessageSHA256(signed.Wire)
	if err != nil {
		t.Fatal(err)
	}
	op := &MultiplyOperation{SignedWire: signed.Wire, SignedWireSHA256: &signed.WireSHA256, TransactionSignature: &signed.TransactionSignature, RecentBlockhash: &signed.RecentBlockhash, MessageSHA256: &mh}
	if _, err := PersistedTransaction(op); err != nil {
		t.Fatal(err)
	}
	fake.fee = 20001
	if _, _, err := executor.PrepareAndSign(context.Background(), built, fixtureKey(45), 0, []byte{0}, 0); err == nil {
		t.Fatal("fee above Rust cap accepted")
	}
}

func TestEffectsRejectOverspendMissingAnchorsAndAcceptBoundedSwap(t *testing.T) {
	topology := testTopology(t)
	config := topology.Strategies[SyrupUsdcUsdc]
	after := idleObserved(topology)
	var custody *TokenBalance
	for i := range after.CollateralCustodies {
		v := &after.CollateralCustodies[i].Balance
		if v.Account == config.CollateralCustody.String() {
			custody = v
		}
	}
	if custody == nil {
		t.Fatal("fixture custody absent")
	}
	effects := &ExpectedEffects{TokenDeltas: []TokenDelta{{Account: custody.Account, Mint: custody.Mint, RawDelta: -500}}}
	setRaw := func(raw uint64) {
		for i := range after.CollateralCustodies {
			v := &after.CollateralCustodies[i].Balance
			if v.Account == custody.Account && v.Mint == custody.Mint {
				v.AmountRaw = raw
			}
		}
	}
	setRaw(400)
	if err := VerifyExpectedEffects(effects, ActionDepositCollateral, nil, after, topology); err == nil {
		t.Fatal("missing persisted balance accepted")
	}
	effects.TokenAmountsBefore = []TokenAmountBefore{{Account: custody.Account, Mint: custody.Mint, AmountRaw: 1000}}
	if err := VerifyExpectedEffects(effects, ActionDepositCollateral, nil, after, topology); err == nil {
		t.Fatal("overspend accepted")
	}
	if err := VerifyExpectedEffects(effects, ActionSwapCollateralToDebt, nil, after, topology); err == nil {
		t.Fatal("swap exceeded maximum")
	}
	setRaw(600)
	if err := VerifyExpectedEffects(effects, ActionSwapCollateralToDebt, nil, after, topology); err != nil {
		t.Fatalf("bounded swap: %v", err)
	}
	effects.TokenAmountsBefore[0].Mint = USDCMint
	if err := VerifyExpectedEffects(effects, ActionSwapCollateralToDebt, nil, after, topology); err == nil {
		t.Fatal("wrong mint anchor accepted")
	}
}

func TestPolicyUnderAnotherOwnerIsRefused(t *testing.T) {
	executor, fake, _ := testExecutor(t)
	policy := fixtureKey(4)
	fake.accounts = map[solana.PublicKey]*chain.Account{policy: {Key: policy, Owner: solana.TokenProgramID, Lamports: 1, Data: []byte{0}}}
	if _, _, err := executor.policyAccount(context.Background(), policy, 0); err == nil {
		t.Fatal("policy under wrong owner accepted")
	}
}

func TestLookupTableRequiresActiveCompleteMetadata(t *testing.T) {
	executor, fake, _ := testExecutor(t)
	tableKey, key := fixtureKey(5), fixtureKey(6)
	active := lookupTableAccount(tableKey, key)
	fake.accounts = map[solana.PublicKey]*chain.Account{tableKey: active}
	tables, err := executor.lookupTables(context.Background(), []solana.PublicKey{tableKey})
	if err != nil || len(tables[tableKey]) != 1 || tables[tableKey][0] != key {
		t.Fatalf("complete active table %v %v", tables, err)
	}
	for _, bad := range []*chain.Account{
		nil,
		{Key: tableKey, Owner: solana.TokenProgramID, Data: active.Data},
		{Key: tableKey, Owner: solana.AddressLookupTableProgramID, Data: active.Data[:len(active.Data)-1]},
		{Key: tableKey, Owner: solana.AddressLookupTableProgramID, Data: make([]byte, len(active.Data))},
	} {
		fake.accounts[tableKey] = bad
		if _, err := executor.lookupTables(context.Background(), []solana.PublicKey{tableKey}); err == nil {
			t.Fatal("invalid lookup table accepted")
		}
	}
}

// Retained only for the legacy parity test's signature prefix inspection.
func decodeShortVec(data []byte) (int, int) {
	value := 0
	for i, b := range data {
		if i >= 3 {
			return 0, 0
		}
		value |= int(b&127) << (7 * i)
		if b&128 == 0 {
			return value, i + 1
		}
	}
	return 0, 0
}
