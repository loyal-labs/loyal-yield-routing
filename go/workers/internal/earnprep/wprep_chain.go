package earnprep

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// WithdrawChain is the chain surface withdraw-prepare reads and simulates
// through; *chain.Client satisfies it.
type WithdrawChain interface {
	RPC
	Blockhash(ctx context.Context, commitment rpc.CommitmentType, minContextSlot uint64) (solana.Hash, uint64, uint64, error)
	Simulate(ctx context.Context, wire []byte, opts rpc.SimulateTransactionOpts) (chain.Simulated, error)
}

// ErrWithdrawRequiredAccountMissing is EarnWithdrawRequiredAccountMissingError
// (CL:212-222): the prefix simulation answered AccountNotFound twice.
var ErrWithdrawRequiredAccountMissing = errors.New("A required Earn withdrawal transaction account is unavailable. Refresh Earn and prepare the withdrawal again.")

// WithdrawLookupTable is an AddressLookupTableAccount as web3.js deserializes
// it: the wire carries the whole state back to the client.
type WithdrawLookupTable struct {
	LookupTable
	Authority                  *solana.PublicKey
	DeactivationSlot           uint64
	LastExtendedSlot           uint64
	LastExtendedSlotStartIndex uint8
}

const lookupTableMetaSize = 56

// parseWithdrawLookupTable mirrors AddressLookupTableAccount.deserialize:
// type index 1, the meta header, then 32-byte addresses to the end.
func parseWithdrawLookupTable(key solana.PublicKey, data []byte) (WithdrawLookupTable, error) {
	if len(data) < lookupTableMetaSize || (len(data)-lookupTableMetaSize)%32 != 0 {
		return WithdrawLookupTable{}, errors.New("lookup table account has an invalid length")
	}
	if binary.LittleEndian.Uint32(data[0:4]) != 1 {
		return WithdrawLookupTable{}, errors.New("lookup table account has an invalid type")
	}
	table := WithdrawLookupTable{
		LookupTable:                LookupTable{Key: key},
		DeactivationSlot:           binary.LittleEndian.Uint64(data[4:12]),
		LastExtendedSlot:           binary.LittleEndian.Uint64(data[12:20]),
		LastExtendedSlotStartIndex: data[20],
	}
	if data[21] != 0 {
		authority := solana.PublicKeyFromBytes(data[22:54])
		table.Authority = &authority
	}
	for offset := lookupTableMetaSize; offset < len(data); offset += 32 {
		table.Addresses = append(table.Addresses, solana.PublicKeyFromBytes(data[offset:offset+32]))
	}
	return table, nil
}

// WithdrawLookupTables mirrors resolveKaminoLookupTableAccounts
// (CL:2227-2253): one read per unique address, a missing or unreadable table
// dropped, since a table only ever saves transaction size.
func WithdrawLookupTables(ctx context.Context, c RPC, addresses []solana.PublicKey) []WithdrawLookupTable {
	var tables []WithdrawLookupTable
	seen := map[solana.PublicKey]bool{}
	for _, address := range addresses {
		if seen[address] {
			continue
		}
		seen[address] = true
		_, accounts, err := c.Accounts(ctx, []solana.PublicKey{address}, rpc.CommitmentConfirmed, 0)
		if err != nil || len(accounts) != 1 || accounts[0] == nil {
			continue
		}
		table, err := parseWithdrawLookupTable(address, accounts[0].Data)
		if err != nil {
			continue
		}
		tables = append(tables, table)
	}
	return tables
}

// WithdrawTokenAmountOrZero mirrors getTokenAccountAmountOrZero (CL:3148): an
// absent token account holds zero.
func WithdrawTokenAmountOrZero(ctx context.Context, c RPC, tokenAccount solana.PublicKey) (uint64, error) {
	_, accounts, err := c.Accounts(ctx, []solana.PublicKey{tokenAccount}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return 0, err
	}
	if accounts[0] == nil {
		return 0, nil
	}
	if len(accounts[0].Data) < 72 {
		return 0, fmt.Errorf("%s is not a token account", tokenAccount)
	}
	return binary.LittleEndian.Uint64(accounts[0].Data[64:72]), nil
}

// SimulateWithdrawTokenAmount mirrors simulatePreparedTokenAccountAmount
// (CL:3327-3390) without the lamport account, which only a final exit asks
// for: compile against a fresh confirmed blockhash, simulate with the
// blockhash replaced, retry one AccountNotFound, and read the token amount of
// tokenAccount from the simulated post-state (zero when it does not exist).
func SimulateWithdrawTokenAmount(ctx context.Context, c WithdrawChain, payer solana.PublicKey, instructions []squads.Instruction, tables []WithdrawLookupTable, tokenAccount solana.PublicKey) (uint64, error) {
	lookups := make([]LookupTable, len(tables))
	for i, table := range tables {
		lookups[i] = table.LookupTable
	}
	simulate := func() (chain.Simulated, error) {
		blockhash, _, _, err := c.Blockhash(ctx, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return chain.Simulated{}, err
		}
		tx, err := CompileV0(payer, blockhash, instructions, lookups)
		if err != nil {
			return chain.Simulated{}, err
		}
		wire, err := tx.MarshalBinary()
		if err != nil {
			return chain.Simulated{}, err
		}
		return c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{
			Commitment:             rpc.CommitmentConfirmed,
			ReplaceRecentBlockhash: true,
			Accounts:               &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: []solana.PublicKey{tokenAccount}},
		})
	}
	simulated, err := simulate()
	if simulationAccountNotFound(err) {
		simulated, err = simulate()
		if simulationAccountNotFound(err) {
			return 0, ErrWithdrawRequiredAccountMissing
		}
	}
	var failed *chain.SimulationError
	if errors.As(err, &failed) {
		reason, _ := json.Marshal(failed.Err)
		return 0, fmt.Errorf("Earn full withdraw prefix simulation failed: %s\n%s", reason, strings.Join(failed.Logs, "\n"))
	}
	if err != nil {
		return 0, err
	}
	if len(simulated.Accounts) == 0 || simulated.Accounts[0] == nil {
		return 0, nil
	}
	data := simulated.Accounts[0].Data
	if len(data) < 72 {
		return 0, errors.New("simulated token account is shorter than a token account")
	}
	return binary.LittleEndian.Uint64(data[64:72]), nil
}

func simulationAccountNotFound(err error) bool {
	var failed *chain.SimulationError
	if !errors.As(err, &failed) {
		return false
	}
	reason, _ := json.Marshal(failed.Err)
	return string(reason) == `"AccountNotFound"`
}
