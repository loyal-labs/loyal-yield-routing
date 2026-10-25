package webapp

import (
	"encoding/base64"
	"errors"
	"strconv"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Wire shapes for prepared Earn operations, ported byte-for-byte from
// The server never fetches a blockhash: the wire carries unsigned instructions
// and the client compiles them against a fresh one, so parity is JSON field
// order and the instruction bytes themselves.
type DepositWireAccountKey struct {
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
	Pubkey     string `json:"pubkey"`
}

type DepositWireInstruction struct {
	DataBase64 string                  `json:"dataBase64"`
	Keys       []DepositWireAccountKey `json:"keys"`
	ProgramID  string                  `json:"programId"`
}

type DepositWireLookupTableState struct {
	Addresses                  []string `json:"addresses"`
	Authority                  *string  `json:"authority"`
	DeactivationSlot           string   `json:"deactivationSlot"`
	LastExtendedSlot           uint64   `json:"lastExtendedSlot"`
	LastExtendedSlotStartIndex uint8    `json:"lastExtendedSlotStartIndex"`
}

type DepositWireLookupTableAccount struct {
	Key   string                      `json:"key"`
	State DepositWireLookupTableState `json:"state"`
}

// DepositWireSimulationDiagnostics mirrors
// client.ts:2310-2320 (withEarnPolicyCreateSimulationDiagnostics).
type DepositWireSimulationDiagnostics struct {
	IncludedPolicyAccounts []string `json:"includedPolicyAccounts"`
	Kind                   string   `json:"kind"`
	PolicyAccount          string   `json:"policyAccount"`
	PolicySeed             string   `json:"policySeed"`
	PolicyStage            string   `json:"policyStage"`
	ProgramID              string   `json:"programId"`
	SettingsPda            string   `json:"settingsPda"`
}

type DepositWirePreparedOperation struct {
	Instructions          []DepositWireInstruction          `json:"instructions"`
	LookupTableAccounts   []DepositWireLookupTableAccount   `json:"lookupTableAccounts"`
	Operation             string                            `json:"operation"`
	Payer                 string                            `json:"payer"`
	ProgramID             string                            `json:"programId"`
	RequiresConfirmation  bool                              `json:"requiresConfirmation"`
	SimulationDiagnostics *DepositWireSimulationDiagnostics `json:"simulationDiagnostics,omitempty"`
}

const depositEarnPolicyCreateMissingAccount = "earnPolicyCreateMissingAccount"

// depositWireOperation serializes one prepared instruction the way
// serializePreparedOperation does. lookupTableAccounts stays an empty array,
// never null.
func depositWireOperation(operation string, payer solana.PublicKey, ix squads.Instruction, diagnostics DepositWireSimulationDiagnostics) DepositWirePreparedOperation {
	keys := make([]DepositWireAccountKey, 0, len(ix.Accounts))
	for _, account := range ix.Accounts {
		keys = append(keys, DepositWireAccountKey{
			IsSigner:   account.IsSigner,
			IsWritable: account.IsWritable,
			Pubkey:     account.PublicKey.String(),
		})
	}
	return DepositWirePreparedOperation{
		Instructions: []DepositWireInstruction{{
			DataBase64: base64.StdEncoding.EncodeToString(ix.Data),
			Keys:       keys,
			ProgramID:  ix.ProgramID.String(),
		}},
		LookupTableAccounts:   []DepositWireLookupTableAccount{},
		Operation:             operation,
		Payer:                 payer.String(),
		ProgramID:             squads.ProgramID.String(),
		RequiresConfirmation:  false,
		SimulationDiagnostics: &diagnostics,
	}
}

func depositEarnPolicyCreateDiagnostics(policyAccount solana.PublicKey, seed uint64, stage string, settings solana.PublicKey, ix squads.Instruction) DepositWireSimulationDiagnostics {
	included := []string{}
	for _, account := range ix.Accounts {
		if account.PublicKey == policyAccount {
			included = append(included, policyAccount.String())
			break
		}
	}
	return DepositWireSimulationDiagnostics{
		IncludedPolicyAccounts: included,
		Kind:                   depositEarnPolicyCreateMissingAccount,
		PolicyAccount:          policyAccount.String(),
		PolicySeed:             strconv.FormatUint(seed, 10),
		PolicyStage:            stage,
		ProgramID:              squads.ProgramID.String(),
		SettingsPda:            settings.String(),
	}
}

// depositPacketBlockhash pins the all-ones blockhash the TS packet guard uses
// ; it is never sent anywhere.
var depositPacketBlockhash = solana.MustHashFromBase58("11111111111111111111111111111111")

const depositMaxPacketBytes = 1232

func depositPacketLength(payer solana.PublicKey, ix squads.Instruction) (int, error) {
	tx, err := earnprep.CompileV0(payer, depositPacketBlockhash, []squads.Instruction{ix}, nil)
	if err != nil {
		return 0, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return len(wire), nil
}

func depositPacketOverLimit(length int) bool { return length > depositMaxPacketBytes }

var errDepositPacketUnserializable = errors.New("prepared operation does not compile into a Solana packet")
