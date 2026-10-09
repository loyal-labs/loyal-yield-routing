package multiply

// Operation builder ported from
// 91694cd9^:crates/loyal-fleet-worker/src/multiply/builder.rs. KLend account vectors
// follow the reviewed in-repo KLend v2 templates (internal/fleet and
// internal/backyard): optional farm/referrer/placeholder slots carry the KLend
// program id, and the trailing reserve farm slot carries the Farms program id.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
)

// BuiltOperation mirrors builder::BuiltOperation.
type BuiltOperation struct {
	PreInstructions    []Instruction
	PolicyInstructions []Instruction
	LookupTables       []solana.PublicKey
	ExpectedEffects    ExpectedEffects
	QuoteContextSlot   *uint64
}

// QuoteClient is the consumer-defined Jupiter quote surface. The production
// client hits lite-api.jup.ag; tests bind fixtures. Live network access stays
// outside the offline decision path.
type QuoteClient interface {
	FetchQuote(ctx contextT, request QuoteRequest) (*QuoteResponse, error)
	FetchSwapInstructions(ctx contextT, quote *QuoteResponse, vault solana.PublicKey) (*SwapInstructionsResponse, error)
}

type contextT = context.Context

const (
	jupiterQuoteEndpoint            = "https://lite-api.jup.ag/swap/v1/quote"
	jupiterSwapInstructionsEndpoint = "https://lite-api.jup.ag/swap/v1/swap-instructions"
	jupiterSlippageBPS              = 50
	jupiterMaximumRouteLegs         = 4
	jupiterQuoteTimeout             = 15 * time.Second
)

// QuoteRequest is an ExactIn quote request.
type QuoteRequest struct {
	InputMint  string
	OutputMint string
	Amount     uint64
}

// QuoteResponse is the bounded subset of the Jupiter quote the builder trusts.
type QuoteResponse struct {
	RawJSON              json.RawMessage   `json:"-"`
	SlippageBPS          uint16            `json:"slippageBps"`
	InputMint            string            `json:"inputMint"`
	OutputMint           string            `json:"outputMint"`
	SwapMode             string            `json:"swapMode"`
	InAmount             string            `json:"inAmount"`
	OutAmount            string            `json:"outAmount"`
	OtherAmountThreshold string            `json:"otherAmountThreshold"`
	ContextSlot          uint64            `json:"contextSlot"`
	RoutePlan            []json.RawMessage `json:"routePlan"`
	PlatformFee          *json.RawMessage  `json:"platformFee"`
}

// Preserve the complete validated quote when reposting it to Jupiter. Its
// route metadata is provider input, not a new financial authorization.
func (q *QuoteResponse) UnmarshalJSON(raw []byte) error {
	type fields QuoteResponse
	var parsed fields
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	*q = QuoteResponse(parsed)
	q.RawJSON = append(json.RawMessage(nil), raw...)
	return nil
}

func (q QuoteResponse) MarshalJSON() ([]byte, error) {
	if len(q.RawJSON) != 0 {
		return append([]byte(nil), q.RawJSON...), nil
	}
	type fields QuoteResponse
	return json.Marshal(fields(q))
}

func decodeJupiterResponse(body io.Reader, target any) error {
	const maximumResponseBytes = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(body, maximumResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maximumResponseBytes {
		return errors.New("Jupiter response exceeds size bound")
	}
	return json.Unmarshal(raw, target)
}

// SwapInstructionsResponse is the bounded subset of the swap-instructions
// response. Any extra instruction the API introduces is a hard error.
type SwapInstructionsResponse struct {
	SetupInstructions           []json.RawMessage `json:"setupInstructions"`
	OtherInstructions           []json.RawMessage `json:"otherInstructions"`
	SwapInstruction             *RawInstruction   `json:"swapInstruction"`
	CleanupInstruction          *json.RawMessage  `json:"cleanupInstruction"`
	TokenLedgerInstruction      *json.RawMessage  `json:"tokenLedgerInstruction"`
	AddressLookupTableAddresses []string          `json:"addressLookupTableAddresses"`
}

// RawInstruction is one wire-level instruction from the Jupiter API.
type RawInstruction struct {
	ProgramID string           `json:"programId"`
	Accounts  []RawAccountMeta `json:"accounts"`
	Data      string           `json:"data"`
}

type RawAccountMeta struct {
	PubKey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}

// LiveQuoteClient is the production Jupiter client.
type LiveQuoteClient struct {
	HTTP *http.Client
}

// NewLiveQuoteClient bounds the HTTP client like the Rust builder.
func NewLiveQuoteClient() *LiveQuoteClient {
	return &LiveQuoteClient{HTTP: &http.Client{Timeout: jupiterQuoteTimeout}}
}

// FetchQuote implements QuoteClient.
func (c *LiveQuoteClient) FetchQuote(ctx contextT, request QuoteRequest) (*QuoteResponse, error) {
	query := url.Values{}
	query.Set("inputMint", request.InputMint)
	query.Set("outputMint", request.OutputMint)
	query.Set("amount", strconv.FormatUint(request.Amount, 10))
	query.Set("swapMode", "ExactIn")
	query.Set("slippageBps", strconv.Itoa(jupiterSlippageBPS))
	query.Set("maxAccounts", "32")
	request0, err := http.NewRequestWithContext(ctx, http.MethodGet, jupiterQuoteEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.HTTP.Do(request0)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jupiter quote returned status %d", response.StatusCode)
	}
	var quote QuoteResponse
	if err := decodeJupiterResponse(response.Body, &quote); err != nil {
		return nil, err
	}
	return &quote, nil
}

// FetchSwapInstructions implements QuoteClient.
func (c *LiveQuoteClient) FetchSwapInstructions(ctx contextT, quote *QuoteResponse, vault solana.PublicKey) (*SwapInstructionsResponse, error) {
	body, err := json.Marshal(map[string]any{
		"quoteResponse":           quote,
		"userPublicKey":           vault.String(),
		"useSharedAccounts":       true,
		"wrapAndUnwrapSol":        false,
		"dynamicComputeUnitLimit": false,
	})
	if err != nil {
		return nil, err
	}
	request0, err := http.NewRequestWithContext(ctx, http.MethodPost, jupiterSwapInstructionsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request0.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request0)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jupiter swap instructions returned status %d", response.StatusCode)
	}
	var parsed SwapInstructionsResponse
	if err := decodeJupiterResponse(response.Body, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

// BuildOperation mirrors build_operation for every plan the planner emits.
// The user-side actions stay rejected: they are admitted from confirmed
// wallet transactions, never built by the delegate.
func BuildOperation(plan *ActionPlan, observed *ObservedRoute, topology *EarnMaxTopology, quotes QuoteClient, ctx contextT) (*BuiltOperation, error) {
	if plan == nil || observed == nil || topology == nil {
		return nil, errors.New("multiply operation requires a plan, observations and topology")
	}
	var config StrategyConfig
	if plan.StrategyKey.Valid() {
		resolved, err := topology.Strategy(plan.StrategyKey)
		if err != nil {
			return nil, err
		}
		config = resolved
	}
	switch plan.Action {
	case ActionDepositCollateral:
		amount, err := exactAmount(plan.Amount)
		if err != nil {
			return nil, err
		}
		position := observed.Position(config.Key)
		instructions, err := depositInstructions(config, topology.Vault, amount,
			position.CollateralDepositedRaw > 0, position.DebtRaw > 0)
		if err != nil {
			return nil, err
		}
		return klendOperation(instructions,
			tokenEffect(config.CollateralCustody.String(), config.CollateralMint, -int64(amount)),
			obligationEffect(config, int64(amount), 0)), nil
	case ActionBorrowDebt:
		amount, err := resolveBorrow(plan.Amount, observed, config)
		if err != nil {
			return nil, err
		}
		position := observed.Position(config.Key)
		instructions, err := borrowInstructions(config, topology.Vault, amount,
			position.CollateralDepositedRaw > 0, position.DebtRaw > 0)
		if err != nil {
			return nil, err
		}
		if amount > math.MaxInt64 {
			return nil, errors.New("borrow amount exceeds BIGINT")
		}
		return klendOperation(instructions,
			tokenEffect(config.DebtCustody.String(), config.DebtMint, int64(amount)),
			obligationEffect(config, 0, int64(amount))), nil
	case ActionWithdrawCollateral, ActionWithdrawRemainingCollateral:
		position := observed.Position(config.Key)
		var wireCollateralAmount, expectedCollateralAmount uint64
		switch plan.Amount.Mode {
		case "exact":
			wireCollateralAmount, expectedCollateralAmount = plan.Amount.Exactly, plan.Amount.Exactly
		case "all":
			wireCollateralAmount, expectedCollateralAmount = position.CollateralDepositedRaw, position.CollateralDepositedRaw
		case "max_safe":
			wireCollateralAmount, expectedCollateralAmount = ^uint64(0), 1
		default:
			return nil, errors.New("invalid withdraw amount mode")
		}
		var liquidityAmount uint64
		if plan.Amount.Mode == "max_safe" {
			liquidityAmount = 1
		} else {
			var err error
			liquidityAmount, err = CollateralToLiquidityRaw(position, expectedCollateralAmount)
			if err != nil {
				return nil, err
			}
		}
		if liquidityAmount == 0 {
			return nil, errors.New("collateral withdrawal would redeem zero liquidity")
		}
		if expectedCollateralAmount > math.MaxInt64 || liquidityAmount > math.MaxInt64 {
			return nil, errors.New("withdrawal amount exceeds signed expected-effect range")
		}
		instructions, err := withdrawInstructions(config, topology.Vault, wireCollateralAmount, position.DebtRaw > 0)
		if err != nil {
			return nil, err
		}
		return klendOperation(instructions,
			tokenEffect(config.CollateralCustody.String(), config.CollateralMint, int64(liquidityAmount)),
			obligationEffect(config, -int64(expectedCollateralAmount), 0)), nil
	case ActionRepayDebt:
		debt := observed.Position(config.Key).DebtRaw
		var wireAmount, expectedAmount uint64
		switch {
		case plan.Amount.Mode == "all":
			wireAmount, expectedAmount = ^uint64(0), debt
		case plan.Amount.Mode == "exact" && plan.Amount.Exactly > 0 && plan.Amount.Exactly < debt:
			wireAmount, expectedAmount = plan.Amount.Exactly, plan.Amount.Exactly
		default:
			return nil, errors.New("repay requires an exact partial amount or repay-all")
		}
		instructions, err := repayInstructions(config, topology.Vault, wireAmount)
		if err != nil {
			return nil, err
		}
		if expectedAmount > math.MaxInt64 {
			return nil, errors.New("repay amount exceeds BIGINT")
		}
		return klendOperation(instructions,
			tokenEffect(config.DebtCustody.String(), config.DebtMint, -int64(expectedAmount)),
			obligationEffect(config, 0, -int64(expectedAmount))), nil
	case ActionSwapClaimToCollateral:
		return swapExactIn(quotes, ctx, USDCMint, config.CollateralMint, plan, topology.ClaimCustody, config.CollateralCustody, topology.Vault)
	case ActionSwapDebtToCollateral:
		return swapExactIn(quotes, ctx, config.DebtMint, config.CollateralMint, plan, config.DebtCustody, config.CollateralCustody, topology.Vault)
	case ActionSwapCollateralToDebt:
		return swapExactIn(quotes, ctx, config.CollateralMint, config.DebtMint, plan, config.CollateralCustody, config.DebtCustody, topology.Vault)
	case ActionSwapCollateralToClaim:
		return swapExactIn(quotes, ctx, config.CollateralMint, USDCMint, plan, config.CollateralCustody, topology.ClaimCustody, topology.Vault)
	case ActionClaim:
		return nil, errors.New("claim is owned by its authenticated root-wallet request")
	case ActionDepositClaimAsset:
		return nil, errors.New("user deposits are admitted from their confirmed wallet transaction")
	case ActionRequestWithdrawal, ActionCancelWithdrawal:
		return nil, errors.New("withdrawal intents are admitted from their confirmed wallet transaction")
	}
	return nil, fmt.Errorf("unknown multiply action %q", plan.Action)
}

// WalletClaimReceipt contains confirmed transaction metadata, not later balance
// snapshots. LookupTables resolve a v0 message's loaded accounts.
type WalletClaimReceipt struct {
	Signature                                                      string
	ConfirmationState                                              string
	ConfirmedSlot                                                  uint64
	TransactionError                                               *string
	Wire                                                           []byte
	LookupTables                                                   map[solana.PublicKey]solana.PublicKeySlice
	SourceBefore, SourceAfter, DestinationBefore, DestinationAfter TokenBalance
}

// ValidateWalletClaimReceipt is the Rust bridge's acceptance of a root-wallet
// Claim (balance-sweep-ata-monitor validate_earn_max_root_claim and
// validate_earn_max_claim), check for check: the one non-compute-budget
// instruction is the exact Squads root Transaction payload, for vault 0, of
// the saved amount from the claim custody to the saved destination, with an
// account table of exactly those five custody keys. Any number of signers;
// the Squads program enforces the authority's permission on chain.
func ValidateWalletClaimReceipt(route *RouteState, topology *EarnMaxTopology, requestID string, receipt *WalletClaimReceipt) error {
	if route == nil || topology == nil || receipt == nil || route.Withdrawal == nil {
		return errors.New("claim has no route, topology or receipt")
	}
	w := route.Withdrawal
	if route.Goal != GoalWithdraw || route.CurrentOperationID != nil || w.Status != WithdrawalClaimable || w.RequestID != requestID || requestID == "" {
		return errors.New("claim has no current wallet-owned request")
	}
	if receipt.TransactionError != nil || receipt.ConfirmedSlot == 0 || receipt.ConfirmedSlot < route.ObservedSlot || (receipt.ConfirmationState != "confirmed" && receipt.ConfirmationState != "finalized") {
		return errors.New("claim has no successful confirmed receipt")
	}
	if route.Settings != topology.Settings.String() || route.Vault != topology.Vault.String() || route.VaultIndex != 0 || topology.VaultIndex != 0 {
		return errors.New("claim route custody identity drifted")
	}
	tx, err := solana.TransactionFromBytes(receipt.Wire)
	if err != nil {
		return err
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, receipt.Wire) || len(tx.Signatures) == 0 || int(tx.Message.Header.NumRequiredSignatures) != len(tx.Signatures) {
		return errors.New("claim transaction is malformed")
	}
	if err := tx.VerifySignatures(); err != nil {
		return err
	}
	if tx.Signatures[0].String() != receipt.Signature {
		return errors.New("claim receipt does not bind the signed transaction")
	}
	source, vault, usdc, token := topology.ClaimCustody, topology.Vault, mustKey(USDCMint), mustKey(TokenProgram)
	destination, err := solana.PublicKeyFromBase58(w.DestinationAccount)
	if err != nil {
		return err
	}
	amount := w.AmountRaw
	for _, b := range []struct {
		balance TokenBalance
		account solana.PublicKey
	}{{receipt.SourceBefore, source}, {receipt.SourceAfter, source}, {receipt.DestinationBefore, destination}, {receipt.DestinationAfter, destination}} {
		if b.balance.Account != b.account.String() || b.balance.Mint != USDCMint || b.balance.TokenProgram != TokenProgram {
			return errors.New("claim receipt token identity drifted")
		}
	}
	if amount == 0 || source == destination || receipt.SourceBefore.AmountRaw < amount || receipt.SourceBefore.AmountRaw-amount != receipt.SourceAfter.AmountRaw ||
		receipt.DestinationBefore.AmountRaw+amount < amount || receipt.DestinationBefore.AmountRaw+amount != receipt.DestinationAfter.AmountRaw {
		return errors.New("claim receipt did not pay exact saved custody and amount")
	}
	static := len(tx.Message.AccountKeys)
	loadedWritable := 0
	if tx.Message.IsVersioned() {
		if err := tx.Message.SetAddressTables(receipt.LookupTables); err != nil {
			return err
		}
		if err := tx.Message.ResolveLookups(); err != nil {
			return errors.New("claim loaded account proof is incomplete")
		}
		for _, lookup := range tx.Message.AddressTableLookups {
			loadedWritable += len(lookup.WritableIndexes)
		}
	}
	keys := tx.Message.AccountKeys
	header := tx.Message.Header
	signers := int(header.NumRequiredSignatures)
	meta := func(index int) (*solana.AccountMeta, error) {
		if index >= len(keys) {
			return nil, errors.New("claim account index is invalid")
		}
		writable := index < static+loadedWritable
		switch {
		case index < signers:
			writable = index < signers-int(header.NumReadonlySignedAccounts)
		case index < static:
			writable = index < static-int(header.NumReadonlyUnsignedAccounts)
		}
		return &solana.AccountMeta{PublicKey: keys[index], IsSigner: index < signers, IsWritable: writable}, nil
	}
	found := false
	for _, compiled := range tx.Message.Instructions {
		if int(compiled.ProgramIDIndex) >= len(keys) {
			return errors.New("claim program index is invalid")
		}
		program := keys[compiled.ProgramIDIndex]
		if program == solana.ComputeBudget {
			continue
		}
		if found || program != mustKey(SquadsProgram) {
			return errors.New("claim has an unexpected outer instruction")
		}
		found = true
		accounts := make([]*solana.AccountMeta, len(compiled.Accounts))
		for i, index := range compiled.Accounts {
			if accounts[i], err = meta(int(index)); err != nil {
				return err
			}
		}
		if len(accounts) < 3 || !accounts[2].IsSigner {
			return errors.New("claim authority did not sign the transaction")
		}
		// SDK compilers may order the five inner accounts differently: keep the
		// proven order but require exactly the custody key set.
		table := make([]solana.AccountMeta, 0, len(accounts)-3)
		for _, a := range accounts[3:] {
			table = append(table, *a)
		}
		if len(table) != 5 {
			return errors.New("claim account table has unrelated or missing custody")
		}
		for _, key := range []solana.PublicKey{source, usdc, destination, vault, token} {
			count := 0
			for _, a := range table {
				if a.PublicKey == key {
					count++
				}
			}
			if count != 1 {
				return errors.New("claim account table has unrelated or missing custody")
			}
		}
		position := func(key solana.PublicKey, writable, signer bool) byte {
			for i := range table {
				if table[i].PublicKey == key {
					table[i].IsWritable = table[i].IsWritable || writable
					table[i].IsSigner = table[i].IsSigner || signer
					return byte(i)
				}
			}
			return 0
		}
		transfer := appendU64Instruction([]byte{12}, amount)
		transfer = append(transfer, 6)
		inner := []byte{position(source, true, false), position(usdc, false, false), position(destination, true, false), position(vault, false, true)}
		payload := append([]byte{1, position(token, false, false), 4}, inner...)
		payload = binary.LittleEndian.AppendUint16(payload, uint16(len(transfer)))
		payload = append(payload, transfer...)
		data := append(append([]byte{}, squadsExecuteSyncV2Discriminator[:]...), 0, 1, 0)
		data = binary.LittleEndian.AppendUint32(data, uint32(len(payload)))
		data = append(data, payload...)
		expected := []solana.AccountMeta{{PublicKey: topology.Settings, IsWritable: true}, {PublicKey: mustKey(SquadsProgram)}, {PublicKey: accounts[2].PublicKey, IsSigner: true}}
		for _, a := range table {
			expected = append(expected, solana.AccountMeta{PublicKey: a.PublicKey, IsWritable: a.IsWritable})
		}
		if !bytes.Equal(compiled.Data, data) || len(accounts) != len(expected) {
			return errors.New("claim must be the exact root Transaction payload for the saved request")
		}
		for i, want := range expected {
			if accounts[i].PublicKey != want.PublicKey || want.IsWritable && !accounts[i].IsWritable || want.IsSigner && !accounts[i].IsSigner {
				return errors.New("claim must be the exact root Transaction payload for the saved request")
			}
		}
	}
	if !found {
		return errors.New("claim root instruction is absent")
	}
	return nil
}

func klendOperation(instructions []Instruction, tokenDelta TokenDelta, obligationDelta ObligationDelta) *BuiltOperation {
	terminal := instructions[len(instructions)-1]
	return &BuiltOperation{
		PreInstructions:    instructions[:len(instructions)-1],
		PolicyInstructions: []Instruction{terminal},
		ExpectedEffects: ExpectedEffects{
			TokenDeltas:     []TokenDelta{tokenDelta},
			ObligationDelta: &obligationDelta,
		},
	}
}

func tokenEffect(account, mint string, rawDelta int64) TokenDelta {
	return TokenDelta{Account: account, Mint: mint, RawDelta: rawDelta}
}

func obligationEffect(config StrategyConfig, collateralRawDelta, debtRawDelta int64) ObligationDelta {
	return ObligationDelta{
		Obligation: config.Obligation.String(), CollateralRawDelta: collateralRawDelta, DebtRawDelta: debtRawDelta,
	}
}

func exactAmount(amount PlannedAmount) (uint64, error) {
	if amount.Mode == "exact" && amount.Exactly > 0 && amount.Exactly <= math.MaxInt64 {
		return amount.Exactly, nil
	}
	return 0, errors.New("operation requires a positive exact amount")
}

func resolveBorrow(amount PlannedAmount, observed *ObservedRoute, config StrategyConfig) (uint64, error) {
	if amount.Mode == "exact" {
		return exactAmount(amount)
	}
	if amount.Mode != "to_target_ltv" {
		return 0, errors.New("invalid borrow amount mode")
	}
	position := observed.Position(config.Key)
	if !validMarketValues(position) || position.DebtMintFactor == 0 {
		return 0, errors.New("position valuation is unknown or invalid")
	}
	targetValueSF := saturatingU128(new(big.Int).Mul(position.CollateralValueSF, new(big.Int).SetUint64(uint64(config.TargetLTVBPS))))
	targetValueSF.Div(targetValueSF, big.NewInt(10_000))
	additionalValueSF := new(big.Int).Sub(targetValueSF, position.DebtValueSF)
	if additionalValueSF.Sign() < 0 {
		additionalValueSF = big.NewInt(0)
	}
	if position.DebtMarketPriceSF.Sign() == 0 {
		return 0, errors.New("debt reserve price is zero")
	}
	raw := saturatingU128(new(big.Int).Mul(additionalValueSF, new(big.Int).SetUint64(position.DebtMintFactor)))
	raw.Div(raw, position.DebtMarketPriceSF)
	if !raw.IsInt64() {
		return 0, errors.New("borrow amount exceeds signed expected-effect range")
	}
	if raw.Uint64() == 0 {
		return 0, errors.New("position is already at target LTV")
	}
	return raw.Uint64(), nil
}

// Instruction graph construction. Account vectors follow the reviewed KLend
// v2 templates; the farm/referrer/placeholder semantics are documented at
// each helper.

func keyMeta(key solana.PublicKey, signer, writable bool) AccountMeta {
	return AccountMeta{PubKey: key, IsSigner: signer, IsWritable: writable}
}

func klendPlaceholder() AccountMeta { return keyMeta(mustKey(KlendProgram), false, false) }
func farmsPlaceholder() AccountMeta { return keyMeta(mustKey(FarmsProgram), false, false) }
func instructionsSysvar() AccountMeta {
	return keyMeta(mustKey("Sysvar1nstructions1111111111111111111111111"), false, false)
}

func farmUserMeta(farm *solana.PublicKey) AccountMeta {
	if farm == nil {
		return klendPlaceholder()
	}
	return keyMeta(*farm, false, true)
}

func farmStateMeta(farm *solana.PublicKey) AccountMeta {
	if farm == nil {
		return klendPlaceholder()
	}
	return keyMeta(*farm, false, true)
}

func refreshReserveInstruction(config StrategyConfig, reserve solana.PublicKey) Instruction {
	return Instruction{
		ProgramID: mustKey(KlendProgram),
		Accounts: []AccountMeta{
			keyMeta(reserve, false, true),
			keyMeta(config.Market, false, false),
			klendPlaceholder(), klendPlaceholder(), klendPlaceholder(),
			keyMeta(config.Oracle, false, false),
		},
		Data: append([]byte(nil), DiscriminatorRefreshReserve[:]...),
	}
}

func refreshObligationInstruction(config StrategyConfig, includeCollateral, includeDebt bool) Instruction {
	accounts := []AccountMeta{
		keyMeta(config.Market, false, false),
		keyMeta(config.Obligation, false, true),
	}
	if includeCollateral {
		accounts = append(accounts, keyMeta(config.CollateralReserve, false, true))
	}
	if includeDebt {
		accounts = append(accounts, keyMeta(config.DebtReserve, false, true))
	}
	return Instruction{
		ProgramID: mustKey(KlendProgram),
		Accounts:  accounts,
		Data:      append([]byte(nil), DiscriminatorRefreshObligation[:]...),
	}
}

func appendU64Instruction(prefix []byte, amount uint64) []byte {
	data := append([]byte(nil), prefix...)
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], amount)
	return append(data, raw[:]...)
}

func depositInstructions(config StrategyConfig, vault solana.PublicKey, amount uint64, includeCollateral, includeDebt bool) ([]Instruction, error) {
	return []Instruction{
		refreshReserveInstruction(config, config.CollateralReserve),
		refreshReserveInstruction(config, config.DebtReserve),
		refreshObligationInstruction(config, includeCollateral, includeDebt),
		{
			ProgramID: mustKey(KlendProgram),
			Accounts: []AccountMeta{
				keyMeta(vault, true, true),
				keyMeta(config.Obligation, false, true),
				keyMeta(config.Market, false, false),
				keyMeta(config.MarketAuthority, false, false),
				keyMeta(config.CollateralReserve, false, true),
				keyMeta(mustKey(config.CollateralMint), false, false),
				keyMeta(config.CollateralLiquiditySupply, false, true),
				keyMeta(config.CollateralReceiptMint, false, true),
				keyMeta(config.CollateralMintSupply, false, true),
				keyMeta(config.CollateralCustody, false, true),
				klendPlaceholder(),
				keyMeta(mustKey(TokenProgram), false, false),
				keyMeta(mustKey(TokenProgram), false, false),
				instructionsSysvar(),
				farmUserMeta(config.CollateralFarmUser),
				farmStateMeta(config.CollateralFarmState),
				farmsPlaceholder(),
			},
			Data: appendU64Instruction(DiscriminatorDepositCollateral[:], amount),
		},
	}, nil
}

func borrowInstructions(config StrategyConfig, vault solana.PublicKey, amount uint64, includeCollateral, includeDebt bool) ([]Instruction, error) {
	return []Instruction{
		refreshReserveInstruction(config, config.CollateralReserve),
		refreshReserveInstruction(config, config.DebtReserve),
		refreshObligationInstruction(config, includeCollateral, includeDebt),
		{
			ProgramID: mustKey(KlendProgram),
			Accounts: []AccountMeta{
				keyMeta(vault, true, false),
				keyMeta(config.Obligation, false, true),
				keyMeta(config.Market, false, false),
				keyMeta(config.MarketAuthority, false, false),
				keyMeta(config.DebtReserve, false, true),
				keyMeta(mustKey(config.DebtMint), false, false),
				keyMeta(config.DebtLiquiditySupply, false, true),
				keyMeta(config.DebtFeeVault, false, true),
				keyMeta(config.DebtCustody, false, true),
				klendPlaceholder(),
				keyMeta(config.DebtTokenProgram, false, false),
				instructionsSysvar(),
				farmUserMeta(config.DebtFarmUser),
				farmStateMeta(config.DebtFarmState),
				farmsPlaceholder(),
			},
			Data: appendU64Instruction(DiscriminatorBorrowDebt[:], amount),
		},
	}, nil
}

func withdrawInstructions(config StrategyConfig, vault solana.PublicKey, amount uint64, debtAware bool) ([]Instruction, error) {
	result := []Instruction{refreshReserveInstruction(config, config.CollateralReserve)}
	if debtAware {
		result = append(result, refreshReserveInstruction(config, config.DebtReserve))
	}
	result = append(result, refreshObligationInstruction(config, true, debtAware),
		Instruction{
			ProgramID: mustKey(KlendProgram),
			Accounts: []AccountMeta{
				keyMeta(vault, true, true),
				keyMeta(config.Obligation, false, true),
				keyMeta(config.Market, false, false),
				keyMeta(config.MarketAuthority, false, false),
				keyMeta(config.CollateralReserve, false, true),
				keyMeta(mustKey(config.CollateralMint), false, false),
				keyMeta(config.CollateralMintSupply, false, true),
				keyMeta(config.CollateralReceiptMint, false, true),
				keyMeta(config.CollateralLiquiditySupply, false, true),
				keyMeta(config.CollateralCustody, false, true),
				klendPlaceholder(),
				keyMeta(mustKey(TokenProgram), false, false),
				keyMeta(mustKey(TokenProgram), false, false),
				instructionsSysvar(),
				farmUserMeta(config.CollateralFarmUser),
				farmStateMeta(config.CollateralFarmState),
				farmsPlaceholder(),
			},
			Data: appendU64Instruction(DiscriminatorWithdrawCollateral[:], amount),
		})
	return result, nil
}

func repayInstructions(config StrategyConfig, vault solana.PublicKey, amount uint64) ([]Instruction, error) {
	return []Instruction{
		refreshReserveInstruction(config, config.CollateralReserve),
		refreshReserveInstruction(config, config.DebtReserve),
		refreshObligationInstruction(config, true, true),
		{
			ProgramID: mustKey(KlendProgram),
			Accounts: []AccountMeta{
				keyMeta(vault, true, false),
				keyMeta(config.Obligation, false, true),
				keyMeta(config.Market, false, false),
				keyMeta(config.DebtReserve, false, true),
				keyMeta(mustKey(config.DebtMint), false, false),
				keyMeta(config.DebtLiquiditySupply, false, true),
				keyMeta(config.DebtCustody, false, true),
				keyMeta(config.DebtTokenProgram, false, false),
				instructionsSysvar(),
				farmUserMeta(config.DebtFarmUser),
				farmStateMeta(config.DebtFarmState),
				keyMeta(config.MarketAuthority, false, false),
				farmsPlaceholder(),
			},
			Data: appendU64Instruction(DiscriminatorRepayDebt[:], amount),
		},
	}, nil
}

// swapExactIn ports the Jupiter swap arm: quote, validate, and bind the
// vault custody before any effect estimate exists.
func swapExactIn(quotes QuoteClient, ctx contextT, inputMint, outputMint string, plan *ActionPlan, source, destination, vault solana.PublicKey) (*BuiltOperation, error) {
	if quotes == nil {
		return nil, errors.New("Jupiter quotes are required for swap actions")
	}
	amount, err := exactAmount(plan.Amount)
	if err != nil {
		return nil, err
	}
	quote, err := quotes.FetchQuote(ctx, QuoteRequest{InputMint: inputMint, OutputMint: outputMint, Amount: amount})
	if err != nil {
		return nil, err
	}
	if quote == nil || quote.ContextSlot == 0 || quote.SlippageBPS != jupiterSlippageBPS || quote.InputMint != inputMint || quote.OutputMint != outputMint || quote.SwapMode != "ExactIn" || quote.PlatformFee != nil {
		return nil, errors.New("Jupiter quote identity drifted")
	}
	if len(quote.RoutePlan) == 0 || len(quote.RoutePlan) > jupiterMaximumRouteLegs {
		return nil, errors.New("Jupiter quote must contain one to four route legs")
	}
	inputRaw, err := strconv.ParseUint(quote.InAmount, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("quote inAmount: %w", err)
	}
	outputRaw, err := strconv.ParseUint(quote.OutAmount, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("quote outAmount: %w", err)
	}
	thresholdRaw, err := strconv.ParseUint(quote.OtherAmountThreshold, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("quote threshold: %w", err)
	}
	if inputRaw != amount || inputRaw > math.MaxInt64 || outputRaw > math.MaxInt64 || thresholdRaw == 0 || outputRaw < thresholdRaw {
		return nil, errors.New("Jupiter quote amount or threshold drifted")
	}
	response, err := quotes.FetchSwapInstructions(ctx, quote, vault)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("Jupiter returned no instruction response")
	}
	if len(response.SetupInstructions) > 0 || len(response.OtherInstructions) > 0 ||
		response.CleanupInstruction != nil || response.TokenLedgerInstruction != nil {
		return nil, errors.New("Jupiter introduced extra instructions")
	}
	if response.SwapInstruction == nil {
		return nil, errors.New("swap instruction missing")
	}
	swap := *response.SwapInstruction
	if swap.ProgramID != JupiterProgram {
		return nil, errors.New("Jupiter program drifted")
	}
	accounts := make([]AccountMeta, 0, len(swap.Accounts))
	sourceBound, destinationBound, vaultSigner := false, false, false
	for _, entry := range swap.Accounts {
		key, err := solana.PublicKeyFromBase58(entry.PubKey)
		if err != nil {
			return nil, fmt.Errorf("swap key missing: %w", err)
		}
		accounts = append(accounts, keyMeta(key, entry.IsSigner, entry.IsWritable))
		if key == source && entry.IsWritable {
			sourceBound = true
		}
		if key == destination && entry.IsWritable {
			destinationBound = true
		}
		if key == vault && entry.IsSigner {
			vaultSigner = true
		}
	}
	if !sourceBound || !destinationBound || !vaultSigner {
		return nil, errors.New("Jupiter did not bind vault custody")
	}
	data, err := base64.StdEncoding.DecodeString(swap.Data)
	if err != nil {
		return nil, fmt.Errorf("swap data missing: %w", err)
	}
	if err := validateEarnMaxJupiterRoute(Instruction{ProgramID: mustKey(JupiterProgram), Accounts: accounts, Data: data},
		vault, source, destination, mustKey(inputMint), mustKey(outputMint), inputRaw, outputRaw, thresholdRaw); err != nil {
		return nil, err
	}
	lookupTables := make([]solana.PublicKey, 0, len(response.AddressLookupTableAddresses))
	for _, value := range response.AddressLookupTableAddresses {
		key, err := solana.PublicKeyFromBase58(value)
		if err != nil {
			return nil, errors.New("lookup table key is not text")
		}
		lookupTables = append(lookupTables, key)
	}
	contextSlot := quote.ContextSlot
	return &BuiltOperation{
		PolicyInstructions: []Instruction{{
			ProgramID: mustKey(JupiterProgram), Accounts: accounts, Data: data,
		}},
		LookupTables: lookupTables,
		ExpectedEffects: ExpectedEffects{
			TokenDeltas: []TokenDelta{
				tokenEffect(source.String(), inputMint, -int64(inputRaw)),
				tokenEffect(destination.String(), outputMint, int64(thresholdRaw)),
			},
		},
		QuoteContextSlot: &contextSlot,
	}, nil
}

// validateEarnMaxJupiterRoute ports the actual bounded Rust validator in
// loyal-actions/src/earn_max.rs. It binds fixed accounts and privileges, the
// sole vault authority, exact amounts, slippage and zero platform fee.
func validateEarnMaxJupiterRoute(instruction Instruction, vault, source, destination, inputMint, outputMint solana.PublicKey, inputAmount, quotedOutput, minimumOutput uint64) error {
	if instruction.ProgramID != mustKey(JupiterProgram) || len(instruction.Data) < 32 || !equalBytes(instruction.Data[:8], JupiterSharedAccountsRouteDiscriminator[:]) {
		return errors.New("Jupiter action is not SharedAccountsRoute")
	}
	expected := []struct {
		index            int
		key              solana.PublicKey
		signer, writable bool
	}{{2, vault, true, false}, {3, source, false, true}, {6, destination, false, true}, {7, inputMint, false, false}, {8, outputMint, false, false}}
	for _, want := range expected {
		if want.index >= len(instruction.Accounts) {
			return errors.New("Jupiter required account missing")
		}
		got := instruction.Accounts[want.index]
		if got.PubKey != want.key || got.IsSigner != want.signer || got.IsWritable != want.writable {
			return errors.New("Jupiter custody, mint or authority drifted")
		}
	}
	signers := 0
	for _, account := range instruction.Accounts {
		if account.IsSigner {
			signers++
		}
	}
	if signers != 1 {
		return errors.New("Jupiter requires an unexpected authority")
	}
	routeCount := binary.LittleEndian.Uint32(instruction.Data[9:13])
	if routeCount < 1 || routeCount > jupiterMaximumRouteLegs {
		return errors.New("Jupiter route must contain one to four legs")
	}
	tail := instruction.Data[len(instruction.Data)-19:]
	if binary.LittleEndian.Uint64(tail[:8]) != inputAmount || binary.LittleEndian.Uint64(tail[8:16]) != quotedOutput || binary.LittleEndian.Uint16(tail[16:18]) != jupiterSlippageBPS || tail[18] != 0 {
		return errors.New("Jupiter exact amount, slippage or fee drifted")
	}
	// Keep multiplication wide rather than saturate the u64 product. All
	// inputs fit SQL BIGINT but the intermediate need not; flooring a capped
	// product would fabricate a much smaller output threshold.
	minimum := new(big.Int).Mul(new(big.Int).SetUint64(quotedOutput), big.NewInt(10_000-jupiterSlippageBPS))
	minimum.Add(minimum, big.NewInt(9_999)).Quo(minimum, big.NewInt(10_000))
	if !minimum.IsUint64() || minimum.Uint64() != minimumOutput || inputAmount == 0 || minimumOutput == 0 {
		return errors.New("Jupiter minimum output is not the exact quote threshold")
	}
	return nil
}
