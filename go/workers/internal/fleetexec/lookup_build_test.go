package fleetexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	sdk "github.com/gagliardetto/solana-go"
)

type lookupFixture struct {
	Manager, Table string
	RecentSlot     uint64 `json:"recent_slot"`
	Addresses      []string
	Accounts       map[string]json.RawMessage
	SignedPackets  map[string]string `json:"signed_packets"`
	Instructions   map[string]struct {
		Program, Data string
		Accounts      []struct {
			Address          string
			Signer, Writable bool
		}
	}
}

func lookupOfficialWire(t *testing.T, f lookupFixture, kind string) WireIdentity {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(f.SignedPackets[kind])
	if err != nil || len(raw) == 0 {
		t.Fatal("official Rust signed packet missing", kind, err)
	}
	tx, err := sdk.TransactionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	wh, mh := sha256.Sum256(raw), sha256.Sum256(message)
	return WireIdentity{SignedTransaction: raw, SignedTransactionHash: hex.EncodeToString(wh[:]), MessageHash: hex.EncodeToString(mh[:]), TransactionSignature: tx.Signatures[0].String(), RecentBlockhash: tx.Message.RecentBlockhash.String(), LastValidBlockHeight: 1150}
}
func TestLookupOriginalRustPacketsMatchExactGoIntentWithoutResigning(t *testing.T) {
	f := readLookupFixture(t)
	for _, kind := range []LookupKind{LookupCreate, LookupExtend, LookupDeactivate, LookupClose} {
		i := lookupFixtureIntent(f)
		i.Kind = kind
		if kind != LookupCreate {
			i.RecentSlot = nil
			i.Prefix = append([]string{}, f.Addresses[:2]...)
			i.Extension = append([]string{}, f.Addresses[2:]...)
		}
		if kind == LookupDeactivate || kind == LookupClose {
			i.Extension = nil
		}
		if kind == LookupClose {
			slot := uint64(1002)
			i.ExpectedDeactivationSlot = &slot
			i.Recipient = f.Manager
		}
		wire := lookupOfficialWire(t, f, string(kind))
		if err := proveLookupWire(i, wire); err != nil {
			t.Fatalf("original Rust %s wire compatibility: %v", kind, err)
		}
	}
}

func readLookupFixture(t *testing.T) lookupFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/lookups/official-alt.json")
	if err != nil {
		t.Fatal(err)
	}
	var f lookupFixture
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func lookupFixtureIntent(f lookupFixture) LookupIntent {
	return LookupIntent{Cluster: "localnet", OperationID: 1, FamilyID: 1, TableID: 1, Kind: LookupCreate, TableAddress: f.Table, Authority: f.Manager, Payer: f.Manager, RecentSlot: &f.RecentSlot, Extension: append([]string(nil), f.Addresses[:2]...)}
}
func TestLookupOfficialLockedInstructionGoldens(t *testing.T) {
	f := readLookupFixture(t)
	intent := lookupFixtureIntent(f)
	for _, kind := range []LookupKind{LookupCreate, LookupExtend, LookupDeactivate, LookupClose} {
		t.Run(string(kind), func(t *testing.T) {
			i := intent
			i.Kind = kind
			if kind != LookupCreate {
				i.RecentSlot = nil
				i.Prefix = f.Addresses[2:]
			}
			if kind == LookupDeactivate || kind == LookupClose {
				i.Extension = nil
			}
			if kind == LookupClose {
				s := uint64(1002)
				i.Recipient = f.Manager
				i.ExpectedDeactivationSlot = &s
			}
			ix, err := lookupInstructions(i)
			if err != nil {
				t.Fatal(err)
			}
			expected := f.Instructions[string(kind)]
			actual := ix[0]
			data, err := actual.Data()
			if err != nil {
				t.Fatal(err)
			}
			golden, err := base64.StdEncoding.DecodeString(expected.Data)
			if err != nil {
				t.Fatal(err)
			}
			if actual.ProgramID().String() != expected.Program || !bytes.Equal(data, golden) || len(actual.Accounts()) != len(expected.Accounts) {
				t.Fatal("Go ALT instruction differs from locked official Rust builder")
			}
			for n, meta := range actual.Accounts() {
				want := expected.Accounts[n]
				if meta.PublicKey.String() != want.Address || meta.IsSigner != want.Signer || meta.IsWritable != want.Writable {
					t.Fatalf("official account vector mismatch at %d", n)
				}
			}
		})
	}
}
func TestLookupOwnedPacketRejectsIntentAndSignatureMutation(t *testing.T) {
	f := readLookupFixture(t)
	intent := lookupFixtureIntent(f)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	hash := sdk.Hash([32]byte{42}).String()
	wire, err := signLookupMutation(intent, hash, 1150, key)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*LookupIntent, *WireIdentity){
		"suffix":    func(i *LookupIntent, w *WireIdentity) { i.Extension = []string{f.Addresses[2]} },
		"authority": func(i *LookupIntent, w *WireIdentity) { i.Authority = f.Addresses[2] },
		"signature": func(i *LookupIntent, w *WireIdentity) { w.TransactionSignature = sdk.Signature{}.String() },
		"message": func(i *LookupIntent, w *WireIdentity) {
			w.MessageHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		},
		"packet": func(i *LookupIntent, w *WireIdentity) {
			w.SignedTransaction = append([]byte(nil), w.SignedTransaction...)
			w.SignedTransaction[1] ^= 1
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			i, w := intent, wire
			mutate(&i, &w)
			if proveLookupWire(i, w) == nil {
				t.Fatal("mutated financial identity accepted")
			}
		})
	}
}
