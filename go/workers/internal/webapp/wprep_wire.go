package webapp

import (
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// WireSmartAccountPreparedEarnUsdcWithdraw as serializePreparedEarnUsdcWithdraw
// emits it (wire/earn-prepare-wire.shared.ts:99-152, 369-444). The persistence
// objects are the SDK's own (client.ts:8876-8921, 10000-10036, 10125-10164),
// echoed verbatim to confirm.
type EarnWithdrawPrepareResponse struct {
	PreparedWithdraw EarnWithdrawPrepareWire `json:"preparedWithdraw"`
}

type EarnWithdrawPrepareWire struct {
	AmountRaw                string                       `json:"amountRaw"`
	AutodepositClosePrepared *struct{}                    `json:"autodepositClosePrepared"`
	Mode                     string                       `json:"mode"`
	Persistence              EarnWithdrawPersistence      `json:"persistence"`
	Policy                   EarnWithdrawPolicyRef        `json:"policy"`
	SetupPolicy              *EarnWithdrawSetupPolicyRef  `json:"setupPolicy,omitempty"`
	Prepared                 DepositWirePreparedOperation `json:"prepared"`
	WithdrawSteps            []EarnWithdrawStepWire       `json:"withdrawSteps"`
	TargetReserve            *EarnPolicyTargetReserve     `json:"targetReserve"`
	Vault                    EarnWithdrawVault            `json:"vault"`
}

type EarnWithdrawPolicyRef struct {
	Account                              string   `json:"account"`
	ID                                   string   `json:"id"`
	SameMintInstructionConstraintIndexes [2]uint8 `json:"sameMintInstructionConstraintIndexes"`
	Seed                                 string   `json:"seed"`
	WithdrawInstructionConstraintIndex   uint8    `json:"withdrawInstructionConstraintIndex"`
}

type EarnWithdrawSetupPolicyRef struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	Seed    string `json:"seed"`
}

type EarnWithdrawVault struct {
	AccountIndex  uint8   `json:"accountIndex"`
	CollateralAta *string `json:"collateralAta"`
	Pubkey        string  `json:"pubkey"`
	UsdcAta       string  `json:"usdcAta"`
}

type EarnWithdrawAccountingReserve struct {
	LiquidityMint string `json:"liquidityMint"`
	Market        string `json:"market"`
	Obligation    string `json:"obligation"`
	Reserve       string `json:"reserve"`
}

type EarnWithdrawExecutionReserve struct {
	LiquidityMint string `json:"liquidityMint"`
	Market        string `json:"market"`
	Reserve       string `json:"reserve"`
}

type EarnWithdrawStepWire struct {
	AccountingReserve  *EarnWithdrawAccountingReserve `json:"accountingReserve"`
	AmountRaw          string                         `json:"amountRaw"`
	CollateralAta      *string                        `json:"collateralAta"`
	ExecutionReserve   *EarnWithdrawExecutionReserve  `json:"executionReserve"`
	Mode               string                         `json:"mode"`
	Persistence        EarnWithdrawPersistence        `json:"persistence"`
	Prepared           DepositWirePreparedOperation   `json:"prepared"`
	ReserveWithdrawals []EarnWithdrawReserveMetadata  `json:"reserveWithdrawals"`
	StepCount          int                            `json:"stepCount"`
	StepIndex          int                            `json:"stepIndex"`
}

type EarnWithdrawReserveMetadata struct {
	AccountingReserve       string `json:"accountingReserve"`
	CollateralAta           string `json:"collateralAta"`
	ExecutionMarket         string `json:"executionMarket"`
	ExecutionReserve        string `json:"executionReserve"`
	KaminoWithdrawAmountRaw string `json:"kaminoWithdrawAmountRaw"`
	LiquidityMint           string `json:"liquidityMint"`
	Market                  string `json:"market"`
	Reserve                 string `json:"reserve"`
	WithdrawnAmountRaw      string `json:"withdrawnAmountRaw"`
}

// EarnWithdrawSourceMetadata is the nested sourceMetadata:
// {mint, tokenAccount} for an idle source, {liquidityMint, market, reserve}
// for a reserve source.
type EarnWithdrawSourceMetadata struct {
	Mint          string `json:"mint,omitempty"`
	TokenAccount  string `json:"tokenAccount,omitempty"`
	LiquidityMint string `json:"liquidityMint,omitempty"`
	Market        string `json:"market,omitempty"`
	Reserve       string `json:"reserve,omitempty"`
}

// EarnWithdrawPersistence covers the four persistence shapes: idle top level
// and step, reserve step and top level. A field a shape lacks is omitted;
// AutodepositClose is the literal null where the shape carries the key.
type EarnWithdrawPersistence struct {
	Cluster                        string                        `json:"cluster"`
	WalletAddress                  string                        `json:"walletAddress"`
	DelegatedSigner                string                        `json:"delegatedSigner"`
	Settings                       string                        `json:"settings"`
	VaultIndex                     uint8                         `json:"vaultIndex"`
	VaultPubkey                    string                        `json:"vaultPubkey"`
	PolicyID                       string                        `json:"policyId"`
	PolicyAccount                  string                        `json:"policyAccount"`
	PolicySeed                     string                        `json:"policySeed"`
	SetupPolicyID                  string                        `json:"setupPolicyId,omitempty"`
	SetupPolicyAccount             string                        `json:"setupPolicyAccount,omitempty"`
	SetupPolicySeed                string                        `json:"setupPolicySeed,omitempty"`
	AccountingReserve              string                        `json:"accountingReserve,omitempty"`
	ExecutionReserve               string                        `json:"executionReserve,omitempty"`
	TargetReserve                  string                        `json:"targetReserve"`
	Market                         string                        `json:"market,omitempty"`
	LiquidityMint                  string                        `json:"liquidityMint"`
	RequestedWithdrawAmountRaw     string                        `json:"requestedWithdrawAmountRaw"`
	WithdrawnAmountRaw             string                        `json:"withdrawnAmountRaw"`
	Mode                           string                        `json:"mode"`
	SourceAmountRaw                string                        `json:"sourceAmountRaw"`
	SourceID                       string                        `json:"sourceId"`
	SourceMetadata                 EarnWithdrawSourceMetadata    `json:"sourceMetadata"`
	SourceMint                     string                        `json:"sourceMint"`
	SourceTokenAccount             string                        `json:"sourceTokenAccount,omitempty"`
	SourceType                     string                        `json:"sourceType"`
	KaminoWithdrawAmountRaw        string                        `json:"kaminoWithdrawAmountRaw,omitempty"`
	ReserveWithdrawals             []EarnWithdrawReserveMetadata `json:"reserveWithdrawals,omitempty"`
	VaultCollateralCleanupIncluded bool                          `json:"vaultCollateralCleanupIncluded"`
	VaultUsdcRemainderRaw          string                        `json:"vaultUsdcRemainderRaw"`
	WalletTransferAmountRaw        string                        `json:"walletTransferAmountRaw"`
	AutodepositClose               json.RawMessage               `json:"autodepositClose,omitempty"`
	IsFinalStep                    *bool                         `json:"isFinalStep,omitempty"`
	StepCount                      *int                          `json:"stepCount,omitempty"`
	StepIndex                      *int                          `json:"stepIndex,omitempty"`
}

var wprepNull = json.RawMessage("null")

const wprepOperation = "earnUsdcWithdraw"

func wprepWireOperation(payer solana.PublicKey, instructions []squads.Instruction, tables []earnprep.WithdrawLookupTable) DepositWirePreparedOperation {
	wire := DepositWirePreparedOperation{
		Instructions:         make([]DepositWireInstruction, 0, len(instructions)),
		LookupTableAccounts:  make([]DepositWireLookupTableAccount, 0, len(tables)),
		Operation:            wprepOperation,
		Payer:                payer.String(),
		ProgramID:            squads.ProgramID.String(),
		RequiresConfirmation: true,
	}
	for _, ix := range instructions {
		keys := make([]DepositWireAccountKey, 0, len(ix.Accounts))
		for _, account := range ix.Accounts {
			keys = append(keys, DepositWireAccountKey{IsSigner: account.IsSigner, IsWritable: account.IsWritable, Pubkey: account.PublicKey.String()})
		}
		wire.Instructions = append(wire.Instructions, DepositWireInstruction{
			DataBase64: base64.StdEncoding.EncodeToString(ix.Data),
			Keys:       keys,
			ProgramID:  ix.ProgramID.String(),
		})
	}
	for _, table := range tables {
		addresses := make([]string, 0, len(table.Addresses))
		for _, address := range table.Addresses {
			addresses = append(addresses, address.String())
		}
		var authority *string
		if table.Authority != nil {
			text := table.Authority.String()
			authority = &text
		}
		wire.LookupTableAccounts = append(wire.LookupTableAccounts, DepositWireLookupTableAccount{
			Key: table.Key.String(),
			State: DepositWireLookupTableState{
				Addresses:                  addresses,
				Authority:                  authority,
				DeactivationSlot:           strconv.FormatUint(table.DeactivationSlot, 10),
				LastExtendedSlot:           table.LastExtendedSlot,
				LastExtendedSlotStartIndex: table.LastExtendedSlotStartIndex,
			},
		})
	}
	return wire
}
