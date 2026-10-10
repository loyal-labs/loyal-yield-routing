package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

func retainedJupiterLookups(t *testing.T) []LookupTableSnapshot {
	return readRetainedJupiterLookups(t, "jupiter-lookup-accounts-2026-09-04.json", 2)
}

func readRetainedJupiterLookups(t *testing.T, name string, count int) []LookupTableSnapshot {
	t.Helper()
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-go/phase3/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Genesis string
		Slot    int64
		Tables  []struct {
			Address, Owner, DataBase64, DataSHA256 string
			Lamports                               uint64
			Executable                             bool
		}
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" || len(evidence.Tables) != count {
		t.Fatal("lookup provenance invalid")
	}
	tables := []LookupTableSnapshot{}
	for _, a := range evidence.Tables {
		data, err := base64.StdEncoding.Strict().DecodeString(a.DataBase64)
		if err != nil || sha256Bytes(data) != a.DataSHA256 {
			t.Fatal("lookup account digest mismatch")
		}
		s := LookupTableSnapshot{Address: a.Address, Owner: a.Owner, Lamports: a.Lamports, Executable: a.Executable, Data: data, ObservedSlot: evidence.Slot}
		if _, err := decodeMessageLookupTable(s); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, s)
	}
	return tables
}

// assertV0SDKVector compares message with the bytes the installed Solana SDK
// compiled from the same instructions, lookup tables and capture keys.
func assertV0SDKVector(t *testing.T, name string, message []byte) {
	t.Helper()
	want, err := base64.StdEncoding.Strict().DecodeString(sdkVectors(t).MessageV0.Messages[name])
	if err != nil || len(want) == 0 || !bytes.Equal(message, want) {
		t.Fatalf("Go v0 message differs from installed SDK: Go %d bytes SDK %d", len(message), len(want))
	}
}

func TestVersionedMessageMatchesSDKAndRejectsInvalidLookupAccounts(t *testing.T) {
	t.Parallel()
	tables := retainedJupiterLookups(t)
	for name, mutate := range map[string]func(*LookupTableSnapshot){
		"owner":       func(s *LookupTableSnapshot) { s.Owner = bridgeTokenProgram },
		"executable":  func(s *LookupTableSnapshot) { s.Executable = true },
		"unfunded":    func(s *LookupTableSnapshot) { s.Lamports = 0 },
		"bad-key":     func(s *LookupTableSnapshot) { s.Address = "invalid" },
		"truncated":   func(s *LookupTableSnapshot) { s.Data = s.Data[:55] },
		"bad-length":  func(s *LookupTableSnapshot) { s.Data = s.Data[:len(s.Data)-1] },
		"bad-type":    func(s *LookupTableSnapshot) { s.Data[0] = 0 },
		"deactivated": func(s *LookupTableSnapshot) { s.Data[4] = 0 },
		"immature":    func(s *LookupTableSnapshot) { binary.LittleEndian.PutUint64(s.Data[12:20], uint64(s.ObservedSlot)) },
		"bad-option":  func(s *LookupTableSnapshot) { s.Data[21] = 2 },
		"future-slot": func(s *LookupTableSnapshot) { s.ObservedSlot = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			s := tables[0]
			s.Data = append([]byte(nil), s.Data...)
			mutate(&s)
			if _, err := decodeMessageLookupTable(s); err == nil {
				t.Fatal("invalid table accepted")
			}
		})
	}
	// Two tables, shared entries, signer/program also in a table, a writable
	// promotion and >127 repeated account indices exercise real encoding edges.
	first, _ := decodeMessageLookupTable(tables[0])
	second, _ := decodeMessageLookupTable(tables[1])
	payer, program := first.addresses[0], first.addresses[1]
	ix := compiledInstruction{program: program, accounts: []accountMeta{{key: first.addresses[2]}, {key: second.addresses[10], writable: true}, {key: first.addresses[3], signer: true}}, data: bytes.Repeat([]byte{7}, 130)}
	for range 130 {
		ix.accounts = append(ix.accounts, accountMeta{key: first.addresses[2], writable: true})
	}
	message, err := compileV0Message(payer, mustKey(bridgeVault), []compiledInstruction{ix}, tables)
	if err != nil {
		t.Fatal(err)
	}
	assertV0SDKVector(t, "twoTablesSharedEntries", message)
	if _, err := checkedUnsignedMessage(message); err == nil {
		t.Fatal("two-signer message accepted by one-signer worker")
	}
	if _, err := compileV0Message(payer, payer, []compiledInstruction{ix}, []LookupTableSnapshot{tables[0], tables[0]}); err == nil {
		t.Fatal("duplicate table accepted")
	}
	for _, header := range [][]byte{{0x81, 1, 0, 0}, {0x80, 2, 0, 0}, {0x80, 1, 0}} {
		if _, err := checkedUnsignedMessage(header); err == nil {
			t.Fatal("invalid message header accepted")
		}
	}
}

func TestVersionedCaptureMatchesSDKWithoutExtraInstructionsOrPrivileges(t *testing.T) {
	t.Parallel()
	tables := retainedJupiterLookups(t)
	table, _ := decodeMessageLookupTable(tables[0])
	payer, program := mustKey(bridgeDelegate), publicKey(kamino.ProgramID)
	ix := compiledInstruction{program: program, accounts: []accountMeta{{key: table.addresses[2], writable: true}}, data: []byte{1, 2, 3}}
	capture := []publicKey{table.addresses[5], mustKey(bridgeVoltrVault), table.addresses[2], table.addresses[5], payer, program}
	message, err := compileV0Message(payer, mustKey(bridgeUSDC), []compiledInstruction{ix}, tables, capture...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = checkedUnsignedMessage(message); err != nil {
		t.Fatal(err)
	}
	assertV0SDKVector(t, "captureOnlyKeys", message)
}
