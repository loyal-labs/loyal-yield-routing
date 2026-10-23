package webapp

import (
	"encoding/base64"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

type dprepNativeSolItem struct {
	Account  *string `json:"account,omitempty"`
	Kind     string  `json:"kind"`
	Label    string  `json:"label"`
	Lamports string  `json:"lamports"`
	Stage    string  `json:"stage"`
}

type dprepNativeSolRequirement struct {
	BalanceLamports  string               `json:"balanceLamports"`
	BalanceSource    string               `json:"balanceSource"`
	CanProceed       bool                 `json:"canProceed"`
	DeficitLamports  string               `json:"deficitLamports"`
	Items            []dprepNativeSolItem `json:"items"`
	Payer            string               `json:"payer"`
	RequiredLamports string               `json:"requiredLamports"`
}

type dprepPersistence struct {
	Cluster              string                           `json:"cluster"`
	WalletAddress        string                           `json:"walletAddress"`
	DelegatedSigner      string                           `json:"delegatedSigner"`
	Settings             string                           `json:"settings"`
	VaultIndex           uint8                            `json:"vaultIndex"`
	VaultPubkey          string                           `json:"vaultPubkey"`
	PolicyID             string                           `json:"policyId"`
	PolicyAccount        string                           `json:"policyAccount"`
	PolicySeed           string                           `json:"policySeed"`
	SetupPolicyID        *string                          `json:"setupPolicyId,omitempty"`
	SetupPolicyAccount   *string                          `json:"setupPolicyAccount,omitempty"`
	SetupPolicySeed      *string                          `json:"setupPolicySeed,omitempty"`
	TargetReserve        string                           `json:"targetReserve"`
	Market               string                           `json:"market"`
	LiquidityMint        string                           `json:"liquidityMint"`
	DepositMint          string                           `json:"depositMint"`
	PrincipalAmountRaw   string                           `json:"principalAmountRaw"`
	PolicyInitialization string                           `json:"policyInitialization"`
	TargetSupplyApyBps   *string                          `json:"targetSupplyApyBps"`
	RiskProfile          string                           `json:"riskProfile"`
	UniversePreset       string                           `json:"universePreset"`
	RouteModes           []string                         `json:"routeModes"`
	StableMints          []string                         `json:"stableMints"`
	KaminoMarkets        []string                         `json:"kaminoMarkets"`
	KaminoLiquidityMints []string                         `json:"kaminoLiquidityMints"`
	SwapLanes            *[]EarnPolicySwapLanePersistence `json:"swapLanes,omitempty"`
	Threshold            *int                             `json:"threshold,omitempty"`
}

type dprepPolicyRef struct {
	Account                              string `json:"account"`
	ID                                   string `json:"id"`
	SameMintInstructionConstraintIndexes []int  `json:"sameMintInstructionConstraintIndexes"`
	Seed                                 string `json:"seed"`
}

type dprepTargetReserve struct {
	LiquidityMint         string  `json:"liquidityMint"`
	LiquidityTokenProgram string  `json:"liquidityTokenProgram"`
	Market                string  `json:"market"`
	Obligation            string  `json:"obligation"`
	Reserve               string  `json:"reserve"`
	SupplyApyBps          *string `json:"supplyApyBps"`
}

type dprepVault struct {
	AccountIndex  uint8   `json:"accountIndex"`
	CollateralAta *string `json:"collateralAta"`
	Pubkey        string  `json:"pubkey"`
	UsdcAta       string  `json:"usdcAta"`
}

type dprepPreparedDeposit struct {
	KaminoSetupAccountCount int                           `json:"kaminoSetupAccountCount"`
	KaminoSetupRentLamports string                        `json:"kaminoSetupRentLamports"`
	KaminoSetupRequired     bool                          `json:"kaminoSetupRequired"`
	NativeSolRequirement    dprepNativeSolRequirement     `json:"nativeSolRequirement"`
	Persistence             dprepPersistence              `json:"persistence"`
	PolicyFinalizePrepared  *DepositWirePreparedOperation `json:"policyFinalizePrepared"`
	Policy                  dprepPolicyRef                `json:"policy"`
	SetupPolicy             *EarnSetupPolicyRef           `json:"setupPolicy,omitempty"`
	PolicySetupPrepared     *DepositWirePreparedOperation `json:"policySetupPrepared"`
	Prepared                DepositWirePreparedOperation  `json:"prepared"`
	TargetReserve           dprepTargetReserve            `json:"targetReserve"`
	Vault                   dprepVault                    `json:"vault"`
}

type dprepResponse struct {
	Cluster             *string              `json:"cluster,omitempty"`
	ProgramID           *string              `json:"programId,omitempty"`
	SettingsPda         *string              `json:"settingsPda,omitempty"`
	SmartAccountAddress *string              `json:"smartAccountAddress,omitempty"`
	PreparedDeposit     dprepPreparedDeposit `json:"preparedDeposit"`
}

func dprepWireInstruction(ix squads.Instruction) DepositWireInstruction {
	keys := make([]DepositWireAccountKey, 0, len(ix.Accounts))
	for _, account := range ix.Accounts {
		keys = append(keys, DepositWireAccountKey{IsSigner: account.IsSigner, IsWritable: account.IsWritable, Pubkey: account.PublicKey.String()})
	}
	return DepositWireInstruction{DataBase64: base64.StdEncoding.EncodeToString(ix.Data), Keys: keys, ProgramID: ix.ProgramID.String()}
}

func dprepInstructionsOf(op DepositWirePreparedOperation) ([]squads.Instruction, error) {
	out := make([]squads.Instruction, 0, len(op.Instructions))
	for _, wire := range op.Instructions {
		data, err := base64.StdEncoding.DecodeString(wire.DataBase64)
		if err != nil {
			return nil, err
		}
		program, err := solana.PublicKeyFromBase58(wire.ProgramID)
		if err != nil {
			return nil, err
		}
		ix := squads.Instruction{ProgramID: program, Data: data}
		for _, key := range wire.Keys {
			pubkey, err := solana.PublicKeyFromBase58(key.Pubkey)
			if err != nil {
				return nil, err
			}
			ix.Accounts = append(ix.Accounts, solana.AccountMeta{PublicKey: pubkey, IsSigner: key.IsSigner, IsWritable: key.IsWritable})
		}
		out = append(out, ix)
	}
	return out, nil
}

func dprepPolicyRent(account solana.PublicKey, label string, op DepositWirePreparedOperation, stage string) dprepRentCandidate {
	data, _ := base64.StdEncoding.DecodeString(op.Instructions[0].DataBase64)
	return dprepRentCandidate{account: account, known: true, kind: "policy_rent", label: label, space: uint64(len(data)) + dprepPolicyAccountBytesOverCreate, stage: stage}
}

const dprepPolicyAccountBytesOverCreate = 81
