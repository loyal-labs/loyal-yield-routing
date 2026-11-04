package earnmax

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

func shareTransfer(ex chain.Execution, signature solana.Signature, at time.Time, authority, account solana.PublicKey) (Entry, bool) {
	flow := Flow{Signature: signature, Slot: ex.Slot, At: at}
	amount := func(balances map[solana.PublicKey]chain.TokenBalance) (LPRaw, bool) {
		b, ok := balances[account]
		if !ok {
			return 0, true
		}
		n, err := signed[LPRaw](b.Amount)
		return n, err == nil && b.Owner == authority && b.Mint == LPMint && b.Program == solana.TokenProgramID
	}
	pre, preOK := amount(ex.Pre)
	post, postOK := amount(ex.Post)
	if !preOK || !postOK {
		return Missing{flow}, true
	}
	flow.LPDelta = post - pre
	if flow.LPDelta == 0 {
		return nil, false
	}
	var calls []call
	for _, ix := range invocations(ex) {
		calls = append(calls, ix.call)
	}
	net := new(big.Int)
	for _, move := range tokenMoves(calls, ex.Keys) {
		if move.burn || (!move.mint.IsZero() && move.mint != LPMint) {
			continue
		}
		n := new(big.Int).SetUint64(move.amount)
		if move.destination == account {
			net.Add(net, n)
		}
		if move.source == account {
			net.Sub(net, n)
		}
	}
	if !net.IsInt64() || net.Int64() != int64(flow.LPDelta) {
		return Missing{flow}, true
	}
	return Transfer{Flow: flow}, true
}

// A transfer moves principal at the last confirmed vault share value. Its
// later appreciation belongs to the recipient; the incoming value is not yield.
func (r *Reader) valueTransfer(ctx context.Context, transfer Transfer) Transfer {
	// shortcut: inspect eight prior vault transactions; widen this bound if non-pricing traffic hides the last share value.
	const priceHistoryLimit = 8
	rows, err := retryTransient(ctx, func() ([]chain.Signed, error) {
		return r.chain.History(ctx, Vault, priceHistoryLimit, transfer.Signature, rpc.CommitmentConfirmed, transfer.Slot)
	})
	if err != nil {
		return transfer
	}
	for _, row := range rows {
		if row.Slot >= transfer.Slot {
			return transfer
		}
		if row.Failed {
			continue
		}
		ex, err := retryTransient(ctx, func() (chain.Execution, error) {
			return r.chain.Execution(ctx, row.Signature, rpc.CommitmentConfirmed)
		})
		if err != nil || ex.Err != nil || ex.Slot != row.Slot || len(ex.Transaction.Signatures) == 0 || ex.Transaction.Signatures[0] != row.Signature || ex.BlockTime == nil || ex.BlockTime.After(transfer.At) {
			return transfer
		}
		touchesVault := false
		for _, ix := range invocations(ex) {
			if int(ix.program) >= len(ex.Keys) || ex.Keys[ix.program] != voltr.ProgramID {
				continue
			}
			for _, index := range ix.accounts {
				if int(index) < len(ex.Keys) && ex.Keys[index] == Vault {
					touchesVault = true
					if !pricesVault(ix.data) {
						// An unrecognized vault mutation makes older prices unsafe.
						return transfer
					}
				}
			}
		}
		if !touchesVault {
			continue
		}
		value, supply, ok := transferShareValue(ex.Logs)
		if !ok {
			return transfer
		}
		amount := new(big.Int).Mul(big.NewInt(int64(transfer.LPDelta)), big.NewInt(int64(value)))
		amount.Quo(amount, new(big.Int).SetUint64(supply))
		if !amount.IsInt64() {
			return transfer
		}
		n := USDCRaw(amount.Int64())
		transfer.Amount = &n
		transfer.PoolLPBefore, transfer.PoolLPAfter = supply, supply
		transfer.PoolValueBefore, transfer.PoolValueAfter = value, value
		return transfer
	}
	return transfer
}

func pricesVault(data []byte) bool {
	for _, discriminator := range [][8]byte{voltr.DepositStrategyDiscriminator, voltr.WithdrawStrategyDiscriminator, voltr.DepositVaultDiscriminator, voltr.WithdrawVaultDiscriminator, voltr.RequestWithdrawVaultDiscriminator} {
		if bytes.HasPrefix(data, discriminator[:]) {
			return true
		}
	}
	return false
}

// Layouts from voltrxyz/vault-sdk src/idl/voltr_vault.json; supply includes fees.
func transferShareValue(logs []string) (USDCRaw, uint64, bool) {
	var value USDCRaw
	var supply uint64
	var stack []string
	for _, line := range logs {
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[0] == "Program" {
			switch parts[2] {
			case "invoke":
				stack = append(stack, parts[1])
				if parts[1] == voltr.ProgramID.String() {
					value, supply = 0, 0
				}
			case "failed:":
				return 0, 0, false
			case "success":
				if len(stack) == 0 || stack[len(stack)-1] != parts[1] {
					return 0, 0, false
				}
				stack = stack[:len(stack)-1]
			}
		}
		encoded, ok := strings.CutPrefix(line, programData)
		if !ok || len(stack) == 0 || stack[len(stack)-1] != voltr.ProgramID.String() {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return 0, 0, false
		}
		for _, layout := range []struct {
			name                                 string
			size, vaultAt, mintAt, valueAt, lpAt int
		}{
			{"DepositStrategyEvent", 312, 40, 168, 216, 232},
			{"WithdrawStrategyEvent", 312, 40, 168, 216, 232},
			{"DepositVaultEvent", 224, 56, 88, 128, 144},
			{"WithdrawVaultEvent", 232, 56, 88, 136, 152},
			{"RequestWithdrawVaultEvent", 210, 8, 146, 186, 194},
		} {
			discriminator := sha256.Sum256([]byte("event:" + layout.name))
			if len(data) < 8 || !bytes.Equal(data[:8], discriminator[:8]) {
				continue
			}
			if len(data) != layout.size || !bytes.Equal(data[layout.vaultAt:layout.vaultAt+32], Vault[:]) || !bytes.Equal(data[layout.mintAt:layout.mintAt+32], AssetMint[:]) {
				return 0, 0, false
			}
			value, err = signed[USDCRaw](binary.LittleEndian.Uint64(data[layout.valueAt:]))
			supply = binary.LittleEndian.Uint64(data[layout.lpAt:])
			if err != nil || value < 0 || supply == 0 {
				return 0, 0, false
			}
		}
	}
	return value, supply, len(stack) == 0 && value >= 0 && supply > 0
}
