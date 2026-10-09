// Package spl holds the SPL Token, Token-2022, Associated Token, System and
// Compute Budget facts the workers use: token account and mint decoding,
// associated token addresses and the instructions we emit. Program IDs are
// solana-go's (solana.TokenProgramID, solana.Token2022ProgramID,
// solana.SPLAssociatedTokenAccountProgramID, solana.SystemProgramID,
// solana.ComputeBudget), and so are the builders, flattened to plain
// instructions.
package spl

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/solana-foundation/solana-go/v2"
	associatedtokenaccount "github.com/solana-foundation/solana-go/v2/programs/associated-token-account"
	computebudget "github.com/solana-foundation/solana-go/v2/programs/compute-budget"
	"github.com/solana-foundation/solana-go/v2/programs/system"
	"github.com/solana-foundation/solana-go/v2/programs/token"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// TokenAccount is an initialized or frozen SPL Token or Token-2022 account.
type TokenAccount struct {
	Program  solana.PublicKey // the owning token program
	Mint     solana.PublicKey
	Owner    solana.PublicKey // the token authority
	Amount   uint64           // raw base units of Mint
	Delegate *solana.PublicKey
	// DelegatedAmount is what Delegate may still move, in raw units.
	DelegatedAmount   uint64
	Frozen            bool
	IsNative          bool // a wrapped SOL account
	HasCloseAuthority bool
	Extensions        []Extension // Token-2022 only, in account order
}

// Mint is an initialized SPL Token or Token-2022 mint.
type Mint struct {
	Program    solana.PublicKey
	Decimals   uint8
	Extensions []Extension
}

// Extension is one Token-2022 TLV entry.
type Extension struct {
	Type  uint16
	Value []byte
}

// DecodeTokenAccount decodes a token account the way the token program
// unpacks it: a token program owner, the exact classic length, valid option
// tags and state, and for Token-2022 the account type and a well-formed TLV
// area without duplicate entries.
func DecodeTokenAccount(account *chain.Account) (TokenAccount, error) {
	extensions, err := decodeExtensions(account, 165, 2)
	if err != nil {
		return TokenAccount{}, err
	}
	data := account.Data
	if (data[108] != 1 && data[108] != 2) || binary.LittleEndian.Uint32(data[72:]) > 1 || binary.LittleEndian.Uint32(data[109:]) > 1 || binary.LittleEndian.Uint32(data[129:]) > 1 {
		return TokenAccount{}, fmt.Errorf("token account %s is uninitialized or has an invalid option tag", account.Key)
	}
	out := TokenAccount{
		Program: account.Owner, Mint: solana.PublicKeyFromBytes(data[0:32]), Owner: solana.PublicKeyFromBytes(data[32:64]),
		Amount: binary.LittleEndian.Uint64(data[64:72]), DelegatedAmount: binary.LittleEndian.Uint64(data[121:129]), Frozen: data[108] == 2,
		IsNative: data[109] == 1, HasCloseAuthority: data[129] == 1, Extensions: extensions,
	}
	if data[72] == 1 {
		delegate := solana.PublicKeyFromBytes(data[76:108])
		out.Delegate = &delegate
	}
	return out, nil
}

// UnrestrictedBalance reports whether the account's extensions leave all of
// its balance transferable: only ImmutableOwner, a TransferFeeAmount with
// nothing withheld and a TransferHookAccount outside a transfer.
func (a TokenAccount) UnrestrictedBalance() error {
	for _, extension := range a.Extensions {
		value := extension.Value
		switch {
		case extension.Type == 7 && len(value) == 0:
		case extension.Type == 2 && len(value) == 8 && binary.LittleEndian.Uint64(value) == 0:
		case extension.Type == 15 && len(value) == 1 && value[0] == 0:
		default:
			return fmt.Errorf("token account extension %d restricts its balance", extension.Type)
		}
	}
	return nil
}

// DecodeMint decodes an initialized mint with valid authority option tags;
// an extended Token-2022 mint pads its base to 165 bytes with zeros.
func DecodeMint(account *chain.Account) (Mint, error) {
	extensions, err := decodeExtensions(account, 82, 1)
	if err != nil {
		return Mint{}, err
	}
	data := account.Data
	if data[45] != 1 || binary.LittleEndian.Uint32(data[0:4]) > 1 || binary.LittleEndian.Uint32(data[46:50]) > 1 {
		return Mint{}, fmt.Errorf("mint %s is uninitialized or has an invalid authority tag", account.Key)
	}
	return Mint{Program: account.Owner, Decimals: data[44], Extensions: extensions}, nil
}

// decodeExtensions checks the owner and layout of a token program account
// whose base is baseLength bytes and returns its Token-2022 extensions. An
// extended account carries accountType at byte 165, is not the multisig
// length, and holds at least one four-byte TLV header.
func decodeExtensions(account *chain.Account, baseLength int, accountType byte) ([]Extension, error) {
	if account == nil {
		return nil, errors.New("token program account is absent")
	}
	data := account.Data
	if account.Executable || account.Owner != solana.TokenProgramID && account.Owner != solana.Token2022ProgramID {
		return nil, fmt.Errorf("account %s is not a token program account", account.Key)
	}
	if len(data) == baseLength {
		return nil, nil
	}
	if account.Owner == solana.TokenProgramID || len(data) < 170 || len(data) == 355 || data[165] != accountType || !allZero(data[baseLength:165]) {
		return nil, fmt.Errorf("token program account %s has an invalid %d-byte layout", account.Key, len(data))
	}
	var extensions []Extension
	seen := map[uint16]bool{}
	for tail := data[166:]; len(tail) > 0; {
		if len(tail) < 4 {
			return nil, fmt.Errorf("token program account %s has a truncated extension header", account.Key)
		}
		kind, length := binary.LittleEndian.Uint16(tail[0:2]), int(binary.LittleEndian.Uint16(tail[2:4]))
		if kind == 0 {
			if !allZero(tail) {
				return nil, fmt.Errorf("token program account %s has nonzero extension padding", account.Key)
			}
			break
		}
		if seen[kind] || length > len(tail)-4 {
			return nil, fmt.Errorf("token program account %s has a duplicate or truncated extension %d", account.Key, kind)
		}
		seen[kind] = true
		extensions = append(extensions, Extension{Type: kind, Value: tail[4 : 4+length]})
		tail = tail[4+length:]
	}
	return extensions, nil
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// AssociatedTokenAddress is owner's associated token account for mint under
// tokenProgram, which must be SPL Token or Token-2022.
func AssociatedTokenAddress(owner, mint, tokenProgram solana.PublicKey) (solana.PublicKey, error) {
	if tokenProgram != solana.TokenProgramID && tokenProgram != solana.Token2022ProgramID {
		return solana.PublicKey{}, fmt.Errorf("%s is not a token program", tokenProgram)
	}
	address, _, err := solana.FindAssociatedTokenAddressWithProgram(owner, mint, tokenProgram)
	return address, err
}

// CreateIdempotentATA creates owner's associated token account for mint,
// paid by payer, unless it exists.
func CreateIdempotentATA(payer, owner, mint, tokenProgram solana.PublicKey) *solana.GenericInstruction {
	return flatten(associatedtokenaccount.NewCreateIdempotentInstructionBuilder().
		SetPayer(payer).SetWallet(owner).SetMint(mint).SetTokenProgram(tokenProgram).Build())
}

// TransferChecked moves amount raw units of mint from source to destination
// under authority.
func TransferChecked(tokenProgram, source, mint, destination, authority solana.PublicKey, amount uint64, decimals uint8) *solana.GenericInstruction {
	return flatten(token.NewTransferCheckedInstruction(amount, decimals, source, mint, destination, authority, nil).Build().SetProgramID(tokenProgram))
}

// SystemTransfer moves lamports out of a signing account.
func SystemTransfer(from, to solana.PublicKey, lamports uint64) *solana.GenericInstruction {
	return flatten(system.NewTransferInstruction(lamports, from, to).Build())
}

// SetComputeUnitLimit caps the transaction's compute units.
func SetComputeUnitLimit(units uint32) *solana.GenericInstruction {
	return flatten(computebudget.NewSetComputeUnitLimitInstruction(units).Build())
}

// SetComputeUnitPrice sets the priority fee in micro-lamports per unit.
func SetComputeUnitPrice(microLamports uint64) *solana.GenericInstruction {
	return flatten(computebudget.NewSetComputeUnitPriceInstruction(microLamports).Build())
}

// RequestHeapFrame requests a transaction heap of bytes.
func RequestHeapFrame(bytes uint32) *solana.GenericInstruction {
	return flatten(computebudget.NewRequestHeapFrameInstruction(bytes).Build())
}

// flatten encodes a fixed-layout solana-go instruction, whose in-memory
// encoder cannot fail.
func flatten(ix solana.Instruction) *solana.GenericInstruction {
	data, err := ix.Data()
	if err != nil {
		panic(fmt.Sprintf("spl: encode %s instruction: %v", ix.ProgramID(), err))
	}
	return solana.NewInstruction(ix.ProgramID(), ix.Accounts(), data)
}
