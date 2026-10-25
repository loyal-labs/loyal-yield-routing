package webapp

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

type withdrawAutodepositCloseInput struct {
	CloseSignature      string
	ConfirmedSlot       *big.Int
	DelegatedSigner     string
	PolicyAccount       string
	RecurringDelegation string
}

type withdrawReserveWithdrawalInput struct {
	AccountingReserve       string
	CollateralAta           string
	ExecutionMarket         string
	ExecutionReserve        string
	KaminoWithdrawAmountRaw string
	LiquidityMint           string
	Market                  *string
	Reserve                 string
	WithdrawnAmountRaw      string
}

// withdrawConfirmInput mirrors ConfirmedYieldWithdrawalInput
// as the parser produces it:
// optional strings collapse to nil, bigint fields stay arbitrary precision.
type withdrawConfirmInput struct {
	AutodepositClose           *withdrawAutodepositCloseInput
	Cluster                    string
	ConfirmedSlot              *big.Int
	DelegatedSigner            string
	LiquidityMint              string
	Market                     *string
	Mode                       string
	PolicyAccount              string
	PolicyID                   *big.Int
	PolicySeed                 *big.Int
	SetupPolicyAccount         *string
	SetupPolicyID              *big.Int
	SetupPolicySeed            *big.Int
	Settings                   string
	SmartAccountAddress        string
	AccountingReserve          *string
	ExecutionReserve           *string
	IsFinalStep                *bool
	ReserveWithdrawals         []withdrawReserveWithdrawalInput
	RequestedWithdrawAmountRaw *big.Int
	SourceAmountRaw            *big.Int
	SourceID                   *string
	SourceMetadata             map[string]any
	SourceMint                 *string
	SourceTokenAccount         *string
	SourceType                 *string
	StepCount                  *int
	StepIndex                  *int
	TargetReserve              string
	VaultIndex                 int
	VaultPubkey                string
	WalletAddress              string
	WithdrawalSignature        string
	WithdrawnAmountRaw         *big.Int
}

func withdrawReadRequiredString(record map[string]any, key string) (string, error) {
	value, isText := record[key].(string)
	if !isText || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string.", key)
	}
	return strings.TrimSpace(value), nil
}

func withdrawReadOptionalString(record map[string]any, key string) (*string, error) {
	value, present := record[key]
	if !present || value == nil {
		return nil, nil
	}
	text, isText := value.(string)
	if !isText {
		return nil, fmt.Errorf("%s must be a string when provided.", key)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	return &trimmed, nil
}

func withdrawReadBigIntString(record map[string]any, key string) (*big.Int, error) {
	text, err := withdrawReadRequiredString(record, key)
	if err != nil {
		return nil, err
	}
	return withdrawDigits(key, text)
}

func withdrawReadOptionalBigIntString(record map[string]any, key string) (*big.Int, error) {
	text, err := withdrawReadOptionalString(record, key)
	if err != nil || text == nil {
		return nil, err
	}
	return withdrawDigits(key, *text)
}

func withdrawDigits(key, text string) (*big.Int, error) {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return nil, fmt.Errorf("%s must be an unsigned integer string.", key)
		}
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	return value, nil
}

func withdrawReadVaultIndex(record map[string]any) (int, error) {
	value, isNumber := record["vaultIndex"].(float64)
	if !isNumber || value != math.Trunc(value) || value < 0 || value > 32767 {
		return 0, fmt.Errorf("vaultIndex must be an integer between 0 and 32767.")
	}
	return int(value), nil
}

func withdrawReadMode(record map[string]any) (string, error) {
	mode, err := withdrawReadRequiredString(record, "mode")
	if err != nil {
		return "", err
	}
	if mode != "partial" && mode != "full" {
		return "", fmt.Errorf("mode must be partial or full.")
	}
	return mode, nil
}

func withdrawReadOptionalBoolean(record map[string]any, key string) (*bool, error) {
	value, present := record[key]
	if !present || value == nil {
		return nil, nil
	}
	flag, isBool := value.(bool)
	if !isBool {
		return nil, fmt.Errorf("%s must be a boolean when provided.", key)
	}
	return &flag, nil
}

func withdrawReadOptionalNonNegativeInteger(record map[string]any, key string) (*int, error) {
	value, present := record[key]
	if !present || value == nil {
		return nil, nil
	}
	number, isNumber := value.(float64)
	if !isNumber || number != math.Trunc(number) || number < 0 {
		return nil, fmt.Errorf("%s must be a non-negative integer when provided.", key)
	}
	parsed := int(number)
	return &parsed, nil
}

func withdrawReadOptionalReserveWithdrawals(record map[string]any) ([]withdrawReserveWithdrawalInput, error) {
	value, present := record["reserveWithdrawals"]
	if !present || value == nil {
		return nil, nil
	}
	entries, isArray := value.([]any)
	if !isArray {
		return nil, fmt.Errorf("reserveWithdrawals must be an array when provided.")
	}
	out := make([]withdrawReserveWithdrawalInput, 0, len(entries))
	for index, entry := range entries {
		record, isObject := entry.(map[string]any)
		if !isObject {
			// A JS array still passes the object check and then misses every
			// field; mirror that by reading an empty record.
			if _, isArrayEntry := entry.([]any); !isArrayEntry {
				return nil, fmt.Errorf("reserveWithdrawals[%d] must be an object.", index)
			}
			record = map[string]any{}
		}
		var parsed withdrawReserveWithdrawalInput
		var err error
		if parsed.AccountingReserve, err = withdrawReadRequiredString(record, "accountingReserve"); err != nil {
			return nil, err
		}
		if parsed.CollateralAta, err = withdrawReadRequiredString(record, "collateralAta"); err != nil {
			return nil, err
		}
		if parsed.ExecutionMarket, err = withdrawReadRequiredString(record, "executionMarket"); err != nil {
			return nil, err
		}
		if parsed.ExecutionReserve, err = withdrawReadRequiredString(record, "executionReserve"); err != nil {
			return nil, err
		}
		kaminoAmount, err := withdrawReadBigIntString(record, "kaminoWithdrawAmountRaw")
		if err != nil {
			return nil, err
		}
		parsed.KaminoWithdrawAmountRaw = kaminoAmount.String()
		if parsed.LiquidityMint, err = withdrawReadRequiredString(record, "liquidityMint"); err != nil {
			return nil, err
		}
		if parsed.Market, err = withdrawReadOptionalString(record, "market"); err != nil {
			return nil, err
		}
		if parsed.Reserve, err = withdrawReadRequiredString(record, "reserve"); err != nil {
			return nil, err
		}
		withdrawn, err := withdrawReadBigIntString(record, "withdrawnAmountRaw")
		if err != nil {
			return nil, err
		}
		parsed.WithdrawnAmountRaw = withdrawn.String()
		out = append(out, parsed)
	}
	return out, nil
}

func withdrawReadOptionalSourceType(record map[string]any) (*string, error) {
	value, err := withdrawReadOptionalString(record, "sourceType")
	if err != nil || value == nil {
		return nil, err
	}
	if *value != "reserve" && *value != "idle" {
		return nil, fmt.Errorf("sourceType must be reserve or idle when provided.")
	}
	return value, nil
}

func withdrawReadOptionalSourceMetadata(record map[string]any) (map[string]any, error) {
	value, present := record["sourceMetadata"]
	if !present || value == nil {
		return nil, nil
	}
	object, isObject := value.(map[string]any)
	if !isObject {
		return nil, fmt.Errorf("sourceMetadata must be an object when provided.")
	}
	return object, nil
}

func withdrawReadOptionalAutodepositClose(record map[string]any) (*withdrawAutodepositCloseInput, error) {
	value, present := record["autodepositClose"]
	if !present || value == nil {
		return nil, nil
	}
	object, isObject := value.(map[string]any)
	if !isObject {
		if _, isArray := value.([]any); isArray {
			// A JS array passes the object check and then misses every field.
			object = map[string]any{}
		} else {
			return nil, fmt.Errorf("autodepositClose must be an object when provided.")
		}
	}
	closeSignature, err := withdrawReadRequiredString(object, "closeSignature")
	if err != nil {
		return nil, err
	}
	confirmedSlot, err := withdrawReadBigIntString(object, "confirmedSlot")
	if err != nil {
		return nil, err
	}
	delegatedSigner, err := withdrawReadRequiredString(object, "delegatedSigner")
	if err != nil {
		return nil, err
	}
	policyAccount, err := withdrawReadRequiredString(object, "policyAccount")
	if err != nil {
		return nil, err
	}
	recurringDelegation, err := withdrawReadRequiredString(object, "recurringDelegation")
	if err != nil {
		return nil, err
	}
	return &withdrawAutodepositCloseInput{
		CloseSignature:      closeSignature,
		ConfirmedSlot:       confirmedSlot,
		DelegatedSigner:     delegatedSigner,
		PolicyAccount:       policyAccount,
		RecurringDelegation: recurringDelegation,
	}, nil
}

// parseWithdrawConfirmBody reads the body fields in the literal order the TS
// object property list evaluates them, so the first invalid field names
// itself the way Next would.
func parseWithdrawConfirmBody(raw []byte) (withdrawConfirmInput, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return withdrawConfirmInput{}, err
	}
	record := map[string]any{}
	switch value := parsed.(type) {
	case map[string]any:
		record = value
	case []any:
	default:
		return withdrawConfirmInput{}, fmt.Errorf("Request body must be an object.")
	}
	var input withdrawConfirmInput
	if input.AutodepositClose, err = withdrawReadOptionalAutodepositClose(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.Cluster, err = withdrawReadRequiredString(record, "cluster"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.ConfirmedSlot, err = withdrawReadBigIntString(record, "confirmedSlot"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.DelegatedSigner, err = withdrawReadRequiredString(record, "delegatedSigner"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.LiquidityMint, err = withdrawReadRequiredString(record, "liquidityMint"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.Market, err = withdrawReadOptionalString(record, "market"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.Mode, err = withdrawReadMode(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.PolicyAccount, err = withdrawReadRequiredString(record, "policyAccount"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.PolicyID, err = withdrawReadBigIntString(record, "policyId"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.PolicySeed, err = withdrawReadBigIntString(record, "policySeed"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SetupPolicyAccount, err = withdrawReadOptionalString(record, "setupPolicyAccount"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SetupPolicyID, err = withdrawReadOptionalBigIntString(record, "setupPolicyId"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SetupPolicySeed, err = withdrawReadOptionalBigIntString(record, "setupPolicySeed"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.Settings, err = withdrawReadRequiredString(record, "settings"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SmartAccountAddress, err = withdrawReadRequiredString(record, "smartAccountAddress"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.AccountingReserve, err = withdrawReadOptionalString(record, "accountingReserve"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.ExecutionReserve, err = withdrawReadOptionalString(record, "executionReserve"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.IsFinalStep, err = withdrawReadOptionalBoolean(record, "isFinalStep"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.ReserveWithdrawals, err = withdrawReadOptionalReserveWithdrawals(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.RequestedWithdrawAmountRaw, err = withdrawReadOptionalBigIntString(record, "requestedWithdrawAmountRaw"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceAmountRaw, err = withdrawReadOptionalBigIntString(record, "sourceAmountRaw"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceID, err = withdrawReadOptionalString(record, "sourceId"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceMetadata, err = withdrawReadOptionalSourceMetadata(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceMint, err = withdrawReadOptionalString(record, "sourceMint"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceTokenAccount, err = withdrawReadOptionalString(record, "sourceTokenAccount"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.SourceType, err = withdrawReadOptionalSourceType(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.StepCount, err = withdrawReadOptionalNonNegativeInteger(record, "stepCount"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.StepIndex, err = withdrawReadOptionalNonNegativeInteger(record, "stepIndex"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.TargetReserve, err = withdrawReadRequiredString(record, "targetReserve"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.VaultIndex, err = withdrawReadVaultIndex(record); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.VaultPubkey, err = withdrawReadRequiredString(record, "vaultPubkey"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.WalletAddress, err = withdrawReadRequiredString(record, "walletAddress"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.WithdrawalSignature, err = withdrawReadRequiredString(record, "withdrawalSignature"); err != nil {
		return withdrawConfirmInput{}, err
	}
	if input.WithdrawnAmountRaw, err = withdrawReadBigIntString(record, "withdrawnAmountRaw"); err != nil {
		return withdrawConfirmInput{}, err
	}
	return input, nil
}
