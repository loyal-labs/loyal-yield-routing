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

// This file ports the Kamino deposit-instructions client half of
// web/packages/smart-account-vaults/src/client.ts that the Earn deposit
// family builds on: the amount formatting sent to the API (CL:2400-2416),
// the upstream error (CL:2635-2643) and, further down, the POST
// (CL:2691-2740) and the bundle decode (CL:2514-2579). The withdraw client
// and the browser response cache are not ported: the cache only ever runs in
// a browser window (CL:2659-2665), so the server path Go serves always
// fetched fresh.

// KaminoDepositInstructionsURL is KAMINO_DEPOSIT_INSTRUCTIONS_URL (CL:343-344),
// the endpoint the server side posts to; browsers use a Next proxy path
// instead (CL:347-350), which a Go server never needs.
const KaminoDepositInstructionsURL = "https://api.kamino.finance/ktx/klend/deposit-instructions"

// KaminoDepositUSDCDecimals is EARN_DEPOSIT_USDC_DECIMALS (CL:327), the
// decimals every Earn product mint has on the Kamino API wire today.
const KaminoDepositUSDCDecimals = 6

// KaminoAPIDepositDiscriminator is KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// (loyal-actions/src/constants.ts:219-221): mainnet KLend's
// deposit_reserve_liquidity_and_obligation_collateral_v2, the instruction the
// API answers with; the same bytes as kamino.DepositV2Discriminator.
var KaminoAPIDepositDiscriminator = kamino.DepositV2Discriminator

// KaminoAPIDevnetDepositDiscriminator is
// KAMINO_DEVNET_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
// (constants.ts:225-227): devnet KLend's older deposit_reserve_liquidity.
var KaminoAPIDevnetDepositDiscriminator = anchor.Instruction("deposit_reserve_liquidity")

// KaminoUpstreamError ports KaminoUpstreamError (CL:2635-2643): a non-2xx
// Kamino API reply carries its status so callers can tell upstream trouble
// from a shape the API should never send.
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
// (CL:2691-2698). Wallet is the Earn vault: the API body's "wallet" field is
// the vault, not the user's main wallet (CL:2703-2708).
type KaminoDepositInstructionArgs struct {
	AmountRaw            uint64
	DepositDiscriminator [8]byte
	LendProgramID        solana.PublicKey
	Market               solana.PublicKey
	Reserve              solana.PublicKey
	Wallet               solana.PublicKey
}

type kaminoDepositRequestBody struct {
	Wallet  string `json:"wallet"`
	Market  string `json:"market"`
	Reserve string `json:"reserve"`
	Amount  string `json:"amount"`
}

// KaminoDepositInstructions ports fetchKaminoDepositInstruction (CL:2691-2740):
// POST the request body to endpoint, fail non-2xx replies with the status
// message, decode the payload into the deposit bundle. The TS browser cache
// never runs server side (CL:2659-2665), so Go keeps no cache; ctx bounds the
// request. client is taken, not built, so tests inject a fixture server.
func KaminoDepositInstructions(ctx context.Context, client *http.Client, endpoint string, args KaminoDepositInstructionArgs) (KaminoInstructionBundle, error) {
	body, err := json.Marshal(kaminoDepositRequestBody{
		Wallet:  args.Wallet.String(),
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

// ErrKaminoDepositInstructionMissing is "Kamino did not return a deposit
// instruction." (CL:2561, label "deposit"): the payload held no KLend
// instruction with the deposit discriminator and an accounts array.
var ErrKaminoDepositInstructionMissing = errors.New("Kamino did not return a deposit instruction.")

// DecodeKaminoDepositBundle ports readKaminoInstructionBundle (CL:2514-2579)
// with the deposit label, decoding the raw response body bytes. Field checks
// stay per-entry like the TS runtime checks (CL:407-420): an entry that is
// not shaped like an instruction can never match, and only entries in the
// kept prefix get built strictly.
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

// matchesDeposit is the match predicate (CL:2521-2530): the entry's program
// address equals the lending program, its accounts field is an array, and its
// base64 data starts with the discriminator (CL:2430-2448).
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

// parseKaminoInstructionResponse reads the response body field by field, the
// way the TS per-field runtime checks do: instructions is a list of raw
// entries, and each entry's programAddress, data and accounts are kept only
// when they carry the expected JSON types.
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

// toKaminoInstruction ports toKaminoTransactionInstruction (CL:2485-2512)
// with the deposit label.
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

// parseKaminoInstructionRole ports parseKaminoInstructionRole (CL:2418-2428):
// upper-case the role string, then SIGNER marks a signer and WRITABLE marks
// writable; a role that is not a string is neither.
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
