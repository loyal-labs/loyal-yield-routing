package multiply

import (
	"encoding/base64"
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
		out.Accounts = append(out.Accounts, AccountMeta{PubKey: key, IsSigner: a.Signer, IsWritable: a.Writable})
	}
	return out
}
