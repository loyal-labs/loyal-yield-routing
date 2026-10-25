package earnprep

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

const KaminoDepositInstructionsURL = "https://api.kamino.finance/ktx/klend/deposit-instructions"

// KaminoDepositUSDCDecimals is EARN_DEPOSIT_USDC_DECIMALS (CL:327), the
// decimals every Earn product mint has on the Kamino API wire today.
const KaminoDepositUSDCDecimals = 6

// KaminoAPIDepositDiscriminator is KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// : mainnet KLend's
// deposit_reserve_liquidity_and_obligation_collateral_v2, the instruction the
// API answers with; the same bytes as kamino.DepositV2Discriminator.
var KaminoAPIDepositDiscriminator = kamino.DepositV2Discriminator

// KaminoAPIDevnetDepositDiscriminator is
// KAMINO_DEVNET_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// : devnet KLend's older deposit_reserve_liquidity.
var KaminoAPIDevnetDepositDiscriminator = anchor.Instruction("deposit_reserve_liquidity")

type KaminoUpstreamError struct {
	Status  int
	Message string
}

func (e *KaminoUpstreamError) Error() string { return e.Message }

// FormatRawTokenAmountForAPI ports formatRawTokenAmountForApi (CL:2400-2416):
// the raw integer amount as whole.fraction with the fraction zero-padded to
// decimals and its trailing zeros trimmed; no fraction at all when it is zero
// ("1000000", 6) -> "1"; (1500000, 6) -> "1.5").
func FormatRawTokenAmountForAPI(amountRaw uint64, decimals int) string {
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, fraction := new(big.Int).QuoRem(new(big.Int).SetUint64(amountRaw), base, new(big.Int))
	if fraction.Sign() == 0 {
		return whole.String()
	}
	digits := []byte(fraction.String())
	for len(digits) < decimals {
		digits = append([]byte{'0'}, digits...)
	}
	return whole.String() + "." + strings.TrimRight(string(digits), "0")
}

// KaminoDepositInstructionArgs carries fetchKaminoDepositInstruction's args
// (CL:2691-2698).
type KaminoDepositInstructionArgs struct {
	AmountRaw            uint64
	DepositDiscriminator [8]byte
	LendProgramID        solana.PublicKey
	Market               solana.PublicKey
	Reserve              solana.PublicKey
	Vault                solana.PublicKey
}

type kaminoDepositRequestBody struct {
	Vault   string `json:"wallet"`
	Market  string `json:"market"`
	Reserve string `json:"reserve"`
	Amount  string `json:"amount"`
}

func KaminoDepositInstructions(ctx context.Context, client *http.Client, endpoint string, args KaminoDepositInstructionArgs) (KaminoInstructionBundle, error) {
	body, err := json.Marshal(kaminoDepositRequestBody{
		Vault:   args.Vault.String(),
		Market:  args.Market.String(),
		Reserve: args.Reserve.String(),
		Amount:  FormatRawTokenAmountForAPI(args.AmountRaw, KaminoDepositUSDCDecimals),
	})
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return KaminoInstructionBundle{}, &KaminoUpstreamError{
			Status:  response.StatusCode,
			Message: fmt.Sprintf("Kamino deposit instruction request failed with status %d.", response.StatusCode),
		}
	}
	return DecodeKaminoDepositBundle(payload, args.LendProgramID, args.DepositDiscriminator)
}

// KaminoInstructionBundle ports KaminoInstructionBundle (CL:2573-2578):
// Instruction is the first KLend instruction whose data starts with the
// deposit discriminator, Instructions is every KLend instruction up to and
// including it (the prefix a deposit route embeds), MatchingInstructions is
// every discriminator match. The API's lutsByAddress (CL:416-419) is dropped:
// the Earn deposit op compiles with no lookup tables, and a missing table
// only costs size, so nothing downstream needs the addresses.
type KaminoInstructionBundle struct {
	Instruction          squads.Instruction
	Instructions         []squads.Instruction
	MatchingInstructions []squads.Instruction
}

var ErrKaminoDepositInstructionMissing = errors.New("Kamino did not return a deposit instruction.")

func DecodeKaminoDepositBundle(payload []byte, lendProgramID solana.PublicKey, discriminator [8]byte) (KaminoInstructionBundle, error) {
	entries, err := parseKaminoInstructionResponse(payload)
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	expectedProgram := lendProgramID.String()
	var matches []int
	for i, entry := range entries {
		if !entry.matchesDeposit(expectedProgram, discriminator) {
			continue
		}
		matches = append(matches, i)
	}
	if len(matches) == 0 {
		return KaminoInstructionBundle{}, ErrKaminoDepositInstructionMissing
	}
	match := matches[0]
	instruction, err := toKaminoInstruction(entries[match])
	if err != nil {
		return KaminoInstructionBundle{}, err
	}
	instructions := make([]squads.Instruction, 0, match+1)
	for _, entry := range entries[:match+1] {
		if entry.programAddress == nil || *entry.programAddress != expectedProgram {
			continue
		}
		built, err := toKaminoInstruction(entry)
		if err != nil {
			return KaminoInstructionBundle{}, err
		}
		instructions = append(instructions, built)
	}
	matching := make([]squads.Instruction, 0, len(matches))
	for _, index := range matches {
		built, err := toKaminoInstruction(entries[index])
		if err != nil {
			return KaminoInstructionBundle{}, err
		}
		matching = append(matching, built)
	}
	return KaminoInstructionBundle{Instruction: instruction, Instructions: instructions, MatchingInstructions: matching}, nil
}

func (e kaminoEntry) matchesDeposit(expectedProgram string, discriminator [8]byte) bool {
	if e.programAddress == nil || *e.programAddress != expectedProgram || !e.accountsIsArray {
		return false
	}
	data, ok := decodeKaminoBase64(e.data)
	if !ok || len(data) < len(discriminator) {
		return false
	}
	return bytes.Equal(data[:len(discriminator)], discriminator[:])
}

func parseKaminoInstructionResponse(payload []byte) ([]kaminoEntry, error) {
	var wire struct {
		Instructions []json.RawMessage `json:"instructions"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, fmt.Errorf("Kamino deposit instruction response is not a JSON object: %w", err)
	}
	entries := make([]kaminoEntry, 0, len(wire.Instructions))
	for _, rawEntry := range wire.Instructions {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(rawEntry, &fields)
		entry := kaminoEntry{}
		if raw, ok := fields["programAddress"]; ok {
			var address string
			if json.Unmarshal(raw, &address) == nil {
				entry.programAddress = &address
			}
		}
		if raw, ok := fields["data"]; ok {
			var data string
			if json.Unmarshal(raw, &data) == nil {
				entry.data = &data
			}
		}
		if raw, ok := fields["accounts"]; ok {
			var accounts []json.RawMessage
			if json.Unmarshal(raw, &accounts) == nil {
				entry.accountsIsArray = true
				entry.accounts = make([]kaminoAccountEntry, 0, len(accounts))
				for _, rawAccount := range accounts {
					var accountFields map[string]json.RawMessage
					_ = json.Unmarshal(rawAccount, &accountFields)
					account := kaminoAccountEntry{}
					if rawAddress, ok := accountFields["address"]; ok {
						var address string
						if json.Unmarshal(rawAddress, &address) == nil {
							account.address = &address
						}
					}
					if rawRole, ok := accountFields["role"]; ok {
						var role string
						if json.Unmarshal(rawRole, &role) == nil {
							account.role = &role
						}
					}
					entry.accounts = append(entry.accounts, account)
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

type kaminoEntry struct {
	programAddress  *string
	data            *string
	accounts        []kaminoAccountEntry
	accountsIsArray bool
}

type kaminoAccountEntry struct {
	address *string
	role    *string
}

func toKaminoInstruction(entry kaminoEntry) (squads.Instruction, error) {
	if entry.programAddress == nil {
		return squads.Instruction{}, errors.New("Kamino deposit instruction is missing a program address.")
	}
	programID, err := solana.PublicKeyFromBase58(*entry.programAddress)
	if err != nil {
		return squads.Instruction{}, err
	}
	data, ok := decodeKaminoBase64(entry.data)
	if !ok {
		return squads.Instruction{}, errors.New("Kamino deposit instruction data is not a base64 string.")
	}
	accounts := make([]solana.AccountMeta, 0, len(entry.accounts))
	for _, account := range entry.accounts {
		if account.address == nil {
			return squads.Instruction{}, errors.New("Kamino deposit instruction account is missing an address.")
		}
		pubkey, err := solana.PublicKeyFromBase58(*account.address)
		if err != nil {
			return squads.Instruction{}, err
		}
		signer, writable := parseKaminoInstructionRole(account.role)
		accounts = append(accounts, solana.AccountMeta{PublicKey: pubkey, IsSigner: signer, IsWritable: writable})
	}
	return squads.Instruction{ProgramID: programID, Accounts: accounts, Data: data}, nil
}

func parseKaminoInstructionRole(role *string) (signer, writable bool) {
	normalized := ""
	if role != nil {
		normalized = strings.ToUpper(*role)
	}
	return strings.Contains(normalized, "SIGNER"), strings.Contains(normalized, "WRITABLE")
}

// decodeKaminoBase64 mirrors Buffer.from(value, "base64") on a JSON string:
// padded or unpadded both decode. A field that was absent or not a string
// (instructionDataStartsWith's neither-string-nor-bytes case, CL:2434-2441)
// decodes to nothing.
func decodeKaminoBase64(data *string) ([]byte, bool) {
	if data == nil {
		return nil, false
	}
	if out, err := base64.StdEncoding.DecodeString(*data); err == nil {
		return out, true
	}
	if out, err := base64.RawStdEncoding.DecodeString(*data); err == nil {
		return out, true
	}
	return nil, false
}
