package autodeposit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

func TestPersistedV0RetainsAllSignaturesAndResolvesPinnedLookup(t *testing.T) {
	payer := solana.PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)))
	delegate := solana.PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32)))
	lookupKey := mustKey(fixedKey("legacy-table"))
	lookupAccount := mustKey(fixedKey("legacy-loaded-account"))
	ix := solana.NewInstruction(kamino.ProgramID, solana.AccountMetaSlice{solana.NewAccountMeta(delegate.PublicKey(), false, true), solana.NewAccountMeta(lookupAccount, true, false)}, []byte{1, 2, 3})
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, solana.Hash(mustKey(fixedKey("legacy-blockhash"))), solana.TransactionPayer(payer.PublicKey()), solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{lookupKey: {lookupAccount}}))
	if err != nil {
		t.Fatal(err)
	}
	tx.Message.SetVersion(solana.MessageVersionV0)
	if _, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer.PublicKey() {
			return &payer
		}
		if key == delegate.PublicKey() {
			return &delegate
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	attempt := DurableAttempt{Signature: tx.Signatures[0].String(), SignedTransactionBase64: base64Std.EncodeToString(raw), SignedTransactionSHA256: hex.EncodeToString(mustSHA256(raw)), RecentBlockhash: tx.Message.RecentBlockhash.String()}
	parsed, err := persistedWireTransaction(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Signatures) != 2 {
		t.Fatal("legacy payer or executor signature was discarded")
	}
	if _, err = decodeSignedWireMessageTransaction(parsed); err == nil {
		t.Fatal("unresolved lookup was accepted as spend proof")
	}
	data := make([]byte, 88)
	binary.LittleEndian.PutUint32(data[:4], 1)
	binary.LittleEndian.PutUint64(data[4:12], ^uint64(0))
	binary.LittleEndian.PutUint64(data[12:20], 100)
	copy(data[56:], lookupAccount[:])
	builder := &SweepWireBuilder{read: fixtureReader(500, map[string]testAccount{lookupKey.String(): {Owner: solana.AddressLookupTableProgramID.String(), Data: data}})}
	if err = builder.resolvePersistedLookups(t.Context(), parsed); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSignedWireMessageTransaction(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.instructions) != 1 || decoded.instructions[0].accounts[1] != lookupAccount {
		t.Fatal("legacy lookup did not resolve to the pinned account")
	}
	if !bytes.Equal(raw, mustMarshalTransaction(t, parsed)) {
		t.Fatal("lookup proof rewrote the owned packet")
	}
	changed := attempt
	changed.Signature = parsed.Signatures[1].String()
	if _, err = persistedWireTransaction(changed); err == nil {
		t.Fatal("executor signature substituted for persisted first signature")
	}
	changed = attempt
	changed.RecentBlockhash = fixedKey("foreign-blockhash")
	if _, err = persistedWireTransaction(changed); err == nil {
		t.Fatal("blockhash substitution passed")
	}
	parsed, err = persistedWireTransaction(attempt)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(data[4:12], 499)
	if builder.resolvePersistedLookups(t.Context(), parsed) == nil {
		t.Fatal("deactivated lookup table passed")
	}
}
func mustMarshalTransaction(t *testing.T, tx *solana.Transaction) []byte {
	t.Helper()
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDelegationBindsSubscriptionAuthorityAndNonce(t *testing.T) {
	wallet, vault := fixedKey("wallet"), fixedKey("vault")
	nonce := uint64(7)
	walletKey, vaultKey, mint := mustKey(wallet), mustKey(vault), mustKey(USDCMint)
	authority, err := subscriptionAuthorityKey(walletKey[:], mint[:])
	if err != nil {
		t.Fatal(err)
	}
	address, err := delegationAccountKey(authority[:], walletKey[:], vaultKey[:], nonce)
	if err != nil {
		t.Fatal(err)
	}
	identity := DelegationIdentity{Account: base58Key(address[:]), Delegator: wallet, Delegatee: vault, Mint: USDCMint, Nonce: &nonce}
	data := testDelegationData(wallet, vault, USDCMint, 10, 2)
	if amount, err := RemainingDelegationAllowance(SubscriptionsProgramID, data, identity); err != nil || amount != 8 {
		t.Fatalf("valid frozen delegation %d %v", amount, err)
	}
	foreign := mustKey(fixedKey("foreign-subscription-authority"))
	copy(data[delegationAuthorityOffset:], foreign[:])
	if _, err = RemainingDelegationAllowance(SubscriptionsProgramID, data, identity); err == nil {
		t.Fatal("foreign subscription authority passed")
	}
	data = testDelegationData(wallet, vault, USDCMint, 10, 2)
	otherNonce := uint64(8)
	identity.Nonce = &otherNonce
	if _, err = RemainingDelegationAllowance(SubscriptionsProgramID, data, identity); err == nil {
		t.Fatal("another delegation nonce authorized this target")
	}
}
