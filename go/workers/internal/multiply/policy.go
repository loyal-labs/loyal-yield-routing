package multiply

// Policy contracts ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/policy.rs: canonical KLend v2
// discriminators, per-family constraint indexes, and the Squads sync
// execution payload laid out by loyal-actions' Borsh serializers.

import (
	"encoding/binary"
	"errors"

	"github.com/gagliardetto/solana-go"
)

// KLend v2 discriminators (klend_interface::discriminators, as evidenced by
// the reviewed fleet/backyard builders: deposit/borrow/withdraw/repay below
// and the shared refresh tags).
var (
	DiscriminatorDepositCollateral   = [8]byte{216, 224, 191, 27, 204, 151, 102, 175}
	DiscriminatorBorrowDebt          = [8]byte{161, 128, 143, 245, 171, 199, 194, 6}
	DiscriminatorWithdrawCollateral  = [8]byte{235, 52, 119, 152, 149, 197, 20, 7}
	DiscriminatorRepayDebt           = [8]byte{116, 174, 213, 76, 180, 53, 210, 144}
	DiscriminatorRefreshReserve      = [8]byte{2, 218, 138, 235, 79, 201, 25, 102}
	DiscriminatorRefreshObligation   = [8]byte{33, 132, 147, 228, 151, 192, 72, 89}
	squadsExecuteSyncV2Discriminator = [8]byte{90, 81, 187, 81, 39, 70, 128, 78}
)

// PolicyFamily mirrors policy::PolicyFamily.
type PolicyFamily string

const (
	FamilyCollateral PolicyFamily = "CollateralLifecycle"
	FamilyDebt       PolicyFamily = "DebtLifecycle"
	FamilySwap       PolicyFamily = "SwapRoutes"
)

// FamilyForAction mirrors family_for_action.
func FamilyForAction(action MultiplyAction) (PolicyFamily, error) {
	switch action {
	case ActionDepositCollateral, ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		return FamilyCollateral, nil
	case ActionBorrowDebt, ActionRepayDebt:
		return FamilyDebt, nil
	case ActionSwapClaimToCollateral, ActionSwapDebtToCollateral,
		ActionSwapCollateralToDebt, ActionSwapCollateralToClaim:
		return FamilySwap, nil
	}
	return "", errors.New("action has no strategy policy family")
}

// ConstraintIndexes mirrors constraint_indexes: the terminal instruction must
// be the single policy-wrapped instruction, and its single constraint index is
// derived from the action and route shape.
func ConstraintIndexes(config StrategyConfig, action MultiplyAction, instructions []Instruction) ([]byte, error) {
	if len(instructions) != 1 {
		return nil, errors.New("policy execution must contain exactly one terminal instruction")
	}
	var index uint8
	switch action {
	case ActionDepositCollateral:
		index = 0
	case ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		index = 1
	case ActionBorrowDebt:
		index = 0
	case ActionRepayDebt:
		index = 1
	case ActionSwapClaimToCollateral:
		return routeConstraintIndex(instructions[0], claimToCollateralIndex(config.Key))
	case ActionSwapDebtToCollateral:
		return routeConstraintIndex(instructions[0], debtToCollateralIndex(config.Key))
	case ActionSwapCollateralToDebt:
		return routeConstraintIndex(instructions[0], collateralToDebtIndex(config.Key))
	case ActionSwapCollateralToClaim:
		return routeConstraintIndex(instructions[0], collateralToClaimIndex(config.Key))
	default:
		return nil, errors.New("action does not use a strategy policy")
	}
	return []byte{index}, nil
}

func routeConstraintIndex(instruction Instruction, index uint8) ([]byte, error) {
	if len(instruction.Data) < 13 || !equalBytes(instruction.Data[:8], JupiterSharedAccountsRouteDiscriminator[:]) {
		return nil, errors.New("Jupiter action is not SharedAccountsRoute")
	}
	routeCount := binary.LittleEndian.Uint32(instruction.Data[9:13])
	if routeCount < 1 || routeCount > 4 {
		return nil, errors.New("Jupiter route must contain one to four legs")
	}
	return []byte{index}, nil
}

func claimToCollateralIndex(key StrategyKey) uint8 {
	switch key {
	case OnycUsdc, OnycUsds, PrimeUsdc, PrimePyusd, PrimeUsds:
		return 0
	default:
		return 1
	}
}

func debtToCollateralIndex(key StrategyKey) uint8 {
	switch key {
	case PrimePyusd, SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 1
	default:
		return 0
	}
}

func collateralToDebtIndex(key StrategyKey) uint8 {
	switch key {
	case PrimePyusd, SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 3
	default:
		return 2
	}
}

func collateralToClaimIndex(key StrategyKey) uint8 {
	switch key {
	case SyrupUsdcUsdc, SyrupUsdcPyusd:
		return 3
	default:
		return 2
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// CompiledInstruction is a Squads inner instruction in transaction-account
// index space (mirrors loyal_actions::SquadsCompiledInstruction).
type CompiledInstruction struct {
	ProgramIDIndex uint8
	Accounts       []byte
	Data           []byte
}

// CompileSquadsInnerInstruction mirrors compile_squads_inner_instruction:
// push-or-merge every account (signer flags cleared later) and the program.
func CompileSquadsInnerInstruction(transactionAccounts *[]AccountMeta, instruction Instruction) CompiledInstruction {
	accounts := make([]byte, 0, len(instruction.Accounts))
	for _, account := range instruction.Accounts {
		accounts = append(accounts, pushOrUpdateAccountMeta(transactionAccounts, account))
	}
	programIDIndex := pushOrUpdateAccountMeta(transactionAccounts, AccountMeta{PubKey: instruction.ProgramID})
	return CompiledInstruction{ProgramIDIndex: programIDIndex, Accounts: accounts, Data: instruction.Data}
}

func pushOrUpdateAccountMeta(accounts *[]AccountMeta, meta AccountMeta) uint8 {
	for index := range *accounts {
		if (*accounts)[index].PubKey == meta.PubKey {
			(*accounts)[index].IsSigner = (*accounts)[index].IsSigner || meta.IsSigner
			(*accounts)[index].IsWritable = (*accounts)[index].IsWritable || meta.IsWritable
			return uint8(index)
		}
	}
	*accounts = append(*accounts, meta)
	return uint8(len(*accounts) - 1)
}

// AccountMeta mirrors solana_sdk::Instruction's account entry.
type AccountMeta struct {
	PubKey     solana.PublicKey
	IsSigner   bool
	IsWritable bool
}

// Instruction is the multiply package's instruction value.
type Instruction struct {
	ProgramID solana.PublicKey
	Accounts  []AccountMeta
	Data      []byte
}

// ExecuteProgramInteractionInstruction mirrors
// loyal_actions::execute_program_interaction_policy_instruction with
// num_signers = 1 (SQUADS_SYNC_SIGNER_COUNT).
func ExecuteProgramInteractionInstruction(policy solana.PublicKey, signer solana.PublicKey, accountIndex uint8, compiled []CompiledInstruction, constraintIndexes []byte, transactionAccounts []AccountMeta) Instruction {
	for index := range transactionAccounts {
		transactionAccounts[index].IsSigner = false
	}
	accounts := make([]AccountMeta, 0, len(transactionAccounts)+3)
	accounts = append(accounts, AccountMeta{PubKey: policy, IsWritable: true})
	accounts = append(accounts, AccountMeta{PubKey: mustKey(SquadsProgram)})
	accounts = append(accounts, AccountMeta{PubKey: signer, IsSigner: true})
	accounts = append(accounts, transactionAccounts...)

	payload := squadsCompiledInstructionPayload(compiled)
	// Borsh layout from loyal-actions:
	// SquadsSyncTransactionArgs { account_index: u8, num_signers: u8,
	//   payload: SquadsSyncPayload::Policy(
	//     SquadsPolicyPayload::ProgramInteraction(
	//       SquadsProgramInteractionPayload {
	//         instruction_constraint_indices: Option<Vec<u8>>,
	//         transaction_payload: SyncTransaction(
	//           SquadsProgramInteractionSyncPayload { account_index: u8, instructions: Vec<u8> }) }) }) }
	data := make([]byte, 0, 64+len(payload))
	data = append(data, squadsExecuteSyncV2Discriminator[:]...)
	data = append(data, accountIndex, 1, 1, 1, 1)
	data = appendU32LE(data, uint32(len(constraintIndexes)))
	data = append(data, constraintIndexes...)
	data = append(data, 1, accountIndex)
	data = appendU32LE(data, uint32(len(payload)))
	data = append(data, payload...)
	return Instruction{ProgramID: mustKey(SquadsProgram), Accounts: accounts, Data: data}
}

func squadsCompiledInstructionPayload(instructions []CompiledInstruction) []byte {
	payload := make([]byte, 1, 32)
	payload[0] = uint8(len(instructions))
	for _, instruction := range instructions {
		payload = append(payload, instruction.ProgramIDIndex, uint8(len(instruction.Accounts)))
		payload = append(payload, instruction.Accounts...)
		payload = appendU16LE(payload, uint16(len(instruction.Data)))
		payload = append(payload, instruction.Data...)
	}
	return payload
}

func appendU16LE(dst []byte, value uint16) []byte {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], value)
	return append(dst, raw[:]...)
}

func appendU32LE(dst []byte, value uint32) []byte {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	return append(dst, raw[:]...)
}
