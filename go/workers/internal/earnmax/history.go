package earnmax

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"golang.org/x/sync/errgroup"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const (
	historyLimit    = 100
	historyFetchers = 8
	programData     = "Program data: "
	depositAmountAt = 8
)

type Flow struct {
	Signature solana.Signature
	Slot      uint64
	At        time.Time
	LPDelta   LPRaw
}

type Entry interface{ flow() Flow }

type Deposit struct {
	Flow
	Amount USDCRaw
}

type Request struct {
	Flow
	Requested *USDCRaw
}

type Claim struct {
	Flow
	Paid USDCRaw
}

func (e Deposit) flow() Flow { return e.Flow }
func (e Request) flow() Flow { return e.Flow }
func (e Claim) flow() Flow   { return e.Flow }

type History struct {
	Complete bool
	Entries  []Entry
}

func (r *Reader) History(ctx context.Context, a Authority) (History, error) {
	signatures, err := retryRateLimited(ctx, func() ([]chain.Signed, error) {
		return r.chain.History(ctx, a.key, historyLimit, solana.Signature{}, rpc.CommitmentConfirmed, 0)
	})
	if err != nil {
		return History{}, err
	}
	landed := make([]chain.Signed, 0, len(signatures))
	for _, s := range signatures {
		if !s.Failed {
			landed = append(landed, s)
		}
	}
	history := History{Complete: len(landed) < historyLimit}

	r.historyMu.Lock()
	cached := r.history[a.key]
	decoded := make(map[solana.Signature][]Entry, len(landed))
	var missing []chain.Signed
	for _, s := range landed {
		if entries, ok := cached[s.Signature]; ok {
			decoded[s.Signature] = entries
		} else if s.BlockTime != nil {
			missing = append(missing, s)
		}
	}
	r.historyMu.Unlock()

	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(historyFetchers)
	for _, s := range missing {
		group.Go(func() error {
			execution, err := retryRateLimited(groupCtx, func() (chain.Execution, error) {
				return r.chain.Execution(groupCtx, s.Signature, rpc.CommitmentConfirmed)
			})
			if errors.Is(err, chain.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			entries, err := decodeEntries(execution, s.Signature, *s.BlockTime, a.key)
			if err != nil {
				return fmt.Errorf("transaction %s: %w", s.Signature, err)
			}
			mu.Lock()
			decoded[s.Signature] = entries
			mu.Unlock()
			return nil
		})
	}
	err = group.Wait()
	r.historyMu.Lock()
	r.history[a.key] = decoded
	r.historyMu.Unlock()
	if err != nil {
		return History{}, err
	}

	for _, s := range landed {
		entries, ok := decoded[s.Signature]
		if !ok {
			history.Complete = false
			continue
		}
		history.Entries = append(history.Entries, entries...)
	}
	return history, nil
}

func decodeEntries(execution chain.Execution, signature solana.Signature, at time.Time, authority solana.PublicKey) ([]Entry, error) {
	keys := execution.Keys
	var instructions []solana.CompiledInstruction
	instructions = append(instructions, execution.Transaction.Message.Instructions...)
	for _, group := range execution.Inner {
		for _, ix := range group.Instructions {
			instructions = append(instructions, solana.CompiledInstruction{ProgramIDIndex: ix.ProgramIDIndex, Accounts: ix.Accounts, Data: ix.Data})
		}
	}
	var entries []Entry
	for _, ix := range instructions {
		discriminator, data, ok := voltrAction(ix, keys, authority)
		if !ok {
			continue
		}
		flow := Flow{Signature: signature, Slot: execution.Slot, At: at}
		if len(entries) == 0 {
			delta, err := lpDelta(execution, authority)
			if err != nil {
				return nil, err
			}
			flow.LPDelta = delta
		}
		switch discriminator {
		case voltr.DepositVaultDiscriminator:
			if len(data) < depositAmountAt+8 {
				return nil, errors.New("deposit_vault data is shorter than its amount")
			}
			amount, err := signed[USDCRaw](binary.LittleEndian.Uint64(data[depositAmountAt:]))
			if err != nil {
				return nil, err
			}
			entries = append(entries, Deposit{Flow: flow, Amount: amount})
		case voltr.RequestWithdrawVaultDiscriminator:
			request := Request{Flow: flow}
			if flow.LPDelta < 0 {
				request.Requested = requestedFromLogs(execution.Logs, uint64(-flow.LPDelta))
			}
			entries = append(entries, request)
		case voltr.WithdrawVaultDiscriminator:
			entries = append(entries, Claim{Flow: flow, Paid: -idleDelta(execution)})
		}
	}
	return entries, nil
}

func voltrAction(ix solana.CompiledInstruction, keys []solana.PublicKey, authority solana.PublicKey) ([8]byte, []byte, bool) {
	if int(ix.ProgramIDIndex) >= len(keys) || keys[ix.ProgramIDIndex] != voltr.ProgramID {
		return [8]byte{}, nil, false
	}
	var vault, user bool
	for _, index := range ix.Accounts {
		if int(index) >= len(keys) {
			return [8]byte{}, nil, false
		}
		vault = vault || keys[index] == Vault
		user = user || keys[index] == authority
	}
	if !vault || !user {
		return [8]byte{}, nil, false
	}
	for _, d := range [][8]byte{voltr.DepositVaultDiscriminator, voltr.RequestWithdrawVaultDiscriminator, voltr.WithdrawVaultDiscriminator} {
		if bytes.HasPrefix(ix.Data, d[:]) {
			return d, ix.Data, true
		}
	}
	return [8]byte{}, nil, false
}

func idleDelta(execution chain.Execution) USDCRaw {
	return USDCRaw(int64(execution.Post[IdleATA].Amount) - int64(execution.Pre[IdleATA].Amount))
}

func lpDelta(execution chain.Execution, authority solana.PublicKey) (LPRaw, error) {
	amount := func(balances map[solana.PublicKey]chain.TokenBalance) uint64 {
		for _, key := range execution.Keys {
			if b, ok := balances[key]; ok && b.Owner == authority && b.Mint == LPMint {
				return b.Amount
			}
		}
		return 0
	}
	post, err := signed[LPRaw](amount(execution.Post))
	if err != nil {
		return 0, err
	}
	pre, err := signed[LPRaw](amount(execution.Pre))
	return post - pre, err
}

func requestedFromLogs(logs []string, escrowedLP uint64) *USDCRaw {
	for _, line := range logs {
		encoded, ok := strings.CutPrefix(line, programData)
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		event, ok := voltr.DecodeRequestEvent(data)
		if !ok || event.EscrowedLPRaw != escrowedLP {
			continue
		}
		raw, fits := event.RequestedAsset.FloorRaw()
		requested, err := signed[USDCRaw](raw)
		if !fits || err != nil {
			continue
		}
		return &requested
	}
	return nil
}
