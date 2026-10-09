package squads

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

type goldenInstruction struct {
	ProgramID string `json:"programId"`
	Accounts  []struct {
		Address  string `json:"address"`
		Signer   bool   `json:"signer"`
		Writable bool   `json:"writable"`
	} `json:"accounts"`
	DataBase64 string `json:"dataBase64"`
}

func (g goldenInstruction) instruction(t *testing.T) Instruction {
	t.Helper()
	program, err := solana.PublicKeyFromBase58(g.ProgramID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(g.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	out := Instruction{ProgramID: program, Data: data}
	for _, a := range g.Accounts {
		key, err := solana.PublicKeyFromBase58(a.Address)
		if err != nil {
			t.Fatal(err)
		}
		out.Accounts = append(out.Accounts, solana.AccountMeta{PublicKey: key, IsSigner: a.Signer, IsWritable: a.Writable})
	}
	return out
}

func TestSquadsEnvelopeMatchesIndependentRustGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/rust-squads-envelope.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		RequestJSON string `json:"requestJson"`
		Provenance  struct {
			InputSHA256 string `json:"inputSha256"`
		} `json:"provenance"`
		Expected struct {
			SourceSHA256 string            `json:"sourceSha256"`
			Instruction  goldenInstruction `json:"instruction"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(fixture.RequestJSON))
	if hex.EncodeToString(digest[:]) != fixture.Provenance.InputSHA256 || fixture.Expected.SourceSHA256 != fixture.Provenance.InputSHA256 {
		t.Fatal("Rust golden producer input hash drifted")
	}
	var request struct {
		Policy      string              `json:"policy"`
		Delegate    string              `json:"delegatedSigner"`
		Index       uint8               `json:"accountIndex"`
		Constraints []int               `json:"constraintIndices"`
		Inner       []goldenInstruction `json:"inner"`
	}
	if err := json.Unmarshal([]byte(fixture.RequestJSON), &request); err != nil {
		t.Fatal(err)
	}
	constraints := make([]byte, len(request.Constraints))
	for i, n := range request.Constraints {
		if n < 0 || n > 255 {
			t.Fatal("invalid constraint index")
		}
		constraints[i] = byte(n)
	}
	policy, err := solana.PublicKeyFromBase58(request.Policy)
	if err != nil {
		t.Fatal(err)
	}
	delegate, err := solana.PublicKeyFromBase58(request.Delegate)
	if err != nil {
		t.Fatal(err)
	}
	var inner []Instruction
	for _, ix := range request.Inner {
		inner = append(inner, ix.instruction(t))
	}
	actual, err := ExecuteTransactionSyncV2(ExecuteSync{Policy: policy, Signer: delegate, AccountIndex: request.Index, ConstraintIndexes: constraints, Inner: inner})
	if err != nil {
		t.Fatal(err)
	}
	expected := fixture.Expected.Instruction.instruction(t)
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if string(actualJSON) != string(expectedJSON) {
		t.Fatalf("Squads envelope differs from Rust golden:\nactual %s\nRust %s", actualJSON, expectedJSON)
	}
	decoded, err := DecodeExecuteTransactionSyncV2(actual)
	if err != nil || decoded.Policy != policy || decoded.Signer != delegate || decoded.AccountIndex != request.Index || !bytes.Equal(decoded.ConstraintIndexes, constraints) || len(decoded.Inner) != len(inner) {
		t.Fatalf("envelope does not decode to its inputs: %v", err)
	}
	for i := range inner {
		if decoded.Inner[i].ProgramID != inner[i].ProgramID || !bytes.Equal(decoded.Inner[i].Data, inner[i].Data) || len(decoded.Inner[i].Accounts) != len(inner[i].Accounts) {
			t.Fatalf("inner %d does not decode to its input", i)
		}
	}
}
