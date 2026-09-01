package backyardrwa

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func tokenAccountData(amount uint64) []byte {
	data := make([]byte, 165)
	binary.LittleEndian.PutUint64(data[64:72], amount)
	return data
}

func TestDecodeExpectedEffectsFromOperationEnvelope(t *testing.T) {
	mint, authority, address := testPublicKey(11), testPublicKey(44), testPublicKey(77)
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{{Address: address, Owner: classicTokenProgram, Mint: mint, Authority: authority}}}
	encoded, err := json.Marshal(map[string]any{"schema": "loyal-backyard-rwa-operation-evidence/v1", "expectedEffects": expected})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeExpectedEffects(encoded)
	if err != nil || len(decoded.Accounts) != 1 || decoded.Accounts[0].Address != address {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	if _, err := DecodeExpectedEffects([]byte(`{"schema":"loyal-backyard-rwa-operation-evidence/v1","expectedEffects":null}`)); err == nil {
		t.Fatal("unbuilt operation accepted as reconcilable effects")
	}
}

func TestExactTokenAccountReconciliation(t *testing.T) {
	mint, authority := testPublicKey(11), testPublicKey(44)
	mintKey, authorityKey := testKey(t, mint), testKey(t, authority)
	a, b := testPublicKey(77), testPublicKey(99)
	expected := ExpectedEffects{
		Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true,
		Accounts: []ExpectedAccountEffect{
			{Address: a, Owner: classicTokenProgram, Mint: mint, Authority: authority, BeforeRaw: 10, AfterRaw: 7},
			{Address: b, Owner: token2022Program, Mint: mint, Authority: authority, BeforeRaw: 1, AfterRaw: 4},
		},
	}
	reconciliation, evidence, err := ReconcileTokenAccounts(77, expected, []ConfirmedAccount{
		{Address: b, Owner: token2022Program, Lamports: 1, Data: custodyFixture(mintKey, authorityKey, 4, true)},
		{Address: a, Owner: classicTokenProgram, Lamports: 1, Data: custodyFixture(mintKey, authorityKey, 7, false)},
	})
	if err != nil || reconciliation.ConfirmedSlot != 77 || !reconciliation.Conserved ||
		len(reconciliation.EffectsSHA256) != 64 || !strings.Contains(string(evidence), "loyal-backyard-rwa-reconciled-effects/v1") {
		t.Fatalf("reconciliation=%+v evidence=%s err=%v", reconciliation, evidence, err)
	}
}

func TestExactTokenAccountReconciliationRejectsMismatch(t *testing.T) {
	mint, authority, address := testPublicKey(11), testPublicKey(44), testPublicKey(77)
	mintKey, authorityKey := testKey(t, mint), testKey(t, authority)
	expected := ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{{Address: address, Owner: classicTokenProgram, Mint: mint, Authority: authority, AfterRaw: 7}}}
	if _, _, err := ReconcileTokenAccounts(77, expected, []ConfirmedAccount{{Address: address, Owner: classicTokenProgram, Lamports: 1, Data: custodyFixture(mintKey, authorityKey, 8, false)}}); err == nil {
		t.Fatal("wrong postcondition accepted")
	}
}
