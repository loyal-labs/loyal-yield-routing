package earnmax

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	Signature       solana.Signature
	Slot            uint64
	At              time.Time
	LPDelta         LPRaw
	PoolLPBefore    uint64
	PoolLPAfter     uint64
	PoolValueBefore USDCRaw
	PoolValueAfter  USDCRaw
}

type Entry interface{ flow() Flow }

type Deposit struct {
	Flow
	Amount USDCRaw
}

type Escrow struct {
	Account solana.PublicKey
	LP      LPRaw
}

type Request struct {
	Flow
	Escrow    *Escrow
	Requested *USDCRaw
}

type Claim struct {
	Flow
	Escrow *Escrow
	Paid   *USDCRaw
}

type Cancel struct{ Flow }

type Missing struct{ Flow }

func (e Deposit) flow() Flow { return e.Flow }
func (e Request) flow() Flow { return e.Flow }
func (e Claim) flow() Flow   { return e.Flow }
func (e Cancel) flow() Flow  { return e.Flow }
func (e Missing) flow() Flow { return e.Flow }

type historyKey struct {
	signature solana.Signature
	slot      uint64
}

type History struct {
	Entries []Entry
}

func (h History) Complete() bool {
	for _, e := range h.Entries {
		if _, ok := e.(Missing); ok {
			return false
		}
	}
	return true
}

func (r *Reader) History(ctx context.Context, a Authority, atLeast uint64) (History, error) {
	signatures, err := retryTransient(ctx, func() ([]chain.Signed, error) {
		return r.chain.History(ctx, a.key, historyLimit, solana.Signature{}, rpc.CommitmentConfirmed, atLeast)
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
	var history History
	user, err := voltr.UserAccounts(Vault, LPMint, a.key)
	if err != nil {
		return History{}, err
	}

	r.historyMu.Lock()
	cached := r.history[a.key]
	decoded := make(map[historyKey][]Entry, len(landed))
	var missing []chain.Signed
	for _, s := range landed {
		key := historyKey{s.Signature, s.Slot}
		if entries, ok := cached[key]; ok {
			decoded[key] = entries
		} else {
			missing = append(missing, s)
		}
	}
	r.historyMu.Unlock()

	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(historyFetchers)
	for _, s := range missing {
		group.Go(func() error {
			execution, err := retryTransient(groupCtx, func() (chain.Execution, error) {
				return r.chain.Execution(groupCtx, s.Signature, rpc.CommitmentConfirmed)
			})
			if errors.Is(err, chain.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(execution.Transaction.Signatures) == 0 || execution.Transaction.Signatures[0] != s.Signature {
				return nil
			}
			at := execution.BlockTime
			if at == nil {
				at = s.BlockTime
			}
			if at == nil {
				return nil
			}
			entries, err := decodeEntries(execution, s.Signature, *at, a.key, user.LPATA)
			if err != nil {
				return fmt.Errorf("transaction %s: %w", s.Signature, err)
			}
			mu.Lock()
			decoded[historyKey{s.Signature, s.Slot}] = entries
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
		entries, ok := decoded[historyKey{s.Signature, s.Slot}]
		if !ok {
			missing := Missing{Flow{Signature: s.Signature, Slot: s.Slot}}
			if s.BlockTime != nil {
				missing.At = *s.BlockTime
			}
			entries = []Entry{missing}
		}
		history.Entries = append(history.Entries, entries...)
	}
	if len(signatures) >= historyLimit {
		history.Entries = append(history.Entries, Missing{})
	}
	return history, nil
}

type call struct {
	program  uint16
	accounts []uint16
	data     []byte
}

type invocation struct {
	call
	inner []call
}

func invocations(execution chain.Execution) []invocation {
	groups := map[uint16][]rpc.CompiledInstruction{}
	for _, group := range execution.Inner {
		groups[group.Index] = append(groups[group.Index], group.Instructions...)
	}
	var out []invocation
	for i, ix := range execution.Transaction.Message.Instructions {
		group := groups[uint16(i)]
		calls := make([]call, len(group))
		nested := true
		for j, c := range group {
			calls[j] = call{c.ProgramIDIndex, c.Accounts, c.Data}
			nested = nested && c.StackHeight > 1
		}
		out = append(out, invocation{call{ix.ProgramIDIndex, ix.Accounts, ix.Data}, calls})
		for j, c := range group {
			end := j + 1
			for end < len(group) && group[end].StackHeight > c.StackHeight {
				end++
			}
			var inner []call
			if nested {
				inner = calls[j+1 : end]
			}
			out = append(out, invocation{calls[j], inner})
		}
	}
	return out
}

func decodeEntries(execution chain.Execution, signature solana.Signature, at time.Time, authority, lpATA solana.PublicKey) ([]Entry, error) {
	keys := execution.Keys
	var entries []Entry
	for _, ix := range invocations(execution) {
		discriminator, data, ok := voltrAction(ix.call, keys, authority)
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
		moves := tokenMoves(ix.inner, keys)
		switch discriminator {
		case voltr.DepositVaultDiscriminator:
			if len(data) < depositAmountAt+8 {
				return nil, errors.New("deposit_vault data is shorter than its amount")
			}
			amount, err := signed[USDCRaw](binary.LittleEndian.Uint64(data[depositAmountAt:]))
			if err != nil {
				return nil, err
			}
			flow.PoolLPBefore, flow.PoolLPAfter, flow.PoolValueBefore, flow.PoolValueAfter = eventSupply(execution.Logs, "DepositVaultEvent", authority, uint64(amount), uint64(max(flow.LPDelta, 0)))
			entries = append(entries, Deposit{Flow: flow, Amount: amount})
		case voltr.RequestWithdrawVaultDiscriminator:
			request := Request{Flow: flow, Escrow: escrowed(moves, lpATA)}
			if request.Escrow != nil {
				request.Requested = requestedFromLogs(execution.Logs, uint64(request.Escrow.LP))
			}
			entries = append(entries, request)
		case voltr.WithdrawVaultDiscriminator:
			claim := Claim{Flow: flow, Escrow: burned(moves), Paid: paid(moves)}
			if claim.Escrow != nil && claim.Paid != nil {
				claim.PoolLPBefore, claim.PoolLPAfter, claim.PoolValueBefore, claim.PoolValueAfter = eventSupply(execution.Logs, "WithdrawVaultEvent", authority, uint64(*claim.Paid), uint64(claim.Escrow.LP))
			}
			entries = append(entries, claim)
		case voltr.CancelRequestWithdrawVaultDiscriminator:
			entries = append(entries, Cancel{flow})
		}
	}
	return entries, nil
}

func voltrAction(ix call, keys []solana.PublicKey, authority solana.PublicKey) ([8]byte, []byte, bool) {
	if int(ix.program) >= len(keys) || keys[ix.program] != voltr.ProgramID {
		return [8]byte{}, nil, false
	}
	var vault, user bool
	for _, index := range ix.accounts {
		if int(index) >= len(keys) {
			return [8]byte{}, nil, false
		}
		vault = vault || keys[index] == Vault
		user = user || keys[index] == authority
	}
	if !vault || !user {
		return [8]byte{}, nil, false
	}
	for _, d := range [][8]byte{voltr.DepositVaultDiscriminator, voltr.RequestWithdrawVaultDiscriminator, voltr.WithdrawVaultDiscriminator, voltr.CancelRequestWithdrawVaultDiscriminator} {
		if bytes.HasPrefix(ix.data, d[:]) {
			return d, ix.data, true
		}
	}
	return [8]byte{}, nil, false
}

type tokenMove struct {
	source, destination, mint solana.PublicKey
	amount                    uint64
	burn                      bool
}

func tokenMoves(calls []call, keys []solana.PublicKey) []tokenMove {
	if calls == nil {
		return nil
	}
	moves := []tokenMove{}
	for _, c := range calls {
		if int(c.program) >= len(keys) || keys[c.program] != solana.TokenProgramID || len(c.data) == 0 {
			continue
		}
		var source, mint, destination int
		switch c.data[0] {
		case splTransfer:
			source, mint, destination = 0, -1, 1
		case splTransferChecked:
			source, mint, destination = 0, 1, 2
		case splBurn, splBurnChecked:
			source, mint, destination = 0, 1, -1
		default:
			continue
		}
		if len(c.data) < 9 || len(c.accounts) <= max(source, mint, destination) {
			return nil
		}
		key := func(at int) solana.PublicKey {
			if at < 0 || int(c.accounts[at]) >= len(keys) {
				return solana.PublicKey{}
			}
			return keys[c.accounts[at]]
		}
		moves = append(moves, tokenMove{
			source: key(source), destination: key(destination), mint: key(mint),
			amount: binary.LittleEndian.Uint64(c.data[1:9]), burn: destination < 0,
		})
	}
	return moves
}

const (
	splTransfer        = 3
	splBurn            = 8
	splTransferChecked = 12
	splBurnChecked     = 15
)

func onlyMove(moves []tokenMove, match func(tokenMove) bool) (tokenMove, bool) {
	var found []tokenMove
	for _, m := range moves {
		if match(m) {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		return tokenMove{}, false
	}
	return found[0], true
}

func escrow(account solana.PublicKey, amount uint64) *Escrow {
	lp, err := signed[LPRaw](amount)
	if err != nil {
		return nil
	}
	return &Escrow{Account: account, LP: lp}
}

func escrowed(moves []tokenMove, lpATA solana.PublicKey) *Escrow {
	m, ok := onlyMove(moves, func(m tokenMove) bool { return !m.burn && m.source == lpATA })
	if !ok || (!m.mint.IsZero() && m.mint != LPMint) {
		return nil
	}
	return escrow(m.destination, m.amount)
}

func burned(moves []tokenMove) *Escrow {
	m, ok := onlyMove(moves, func(m tokenMove) bool { return m.burn && m.mint == LPMint })
	if !ok {
		return nil
	}
	return escrow(m.source, m.amount)
}

func paid(moves []tokenMove) *USDCRaw {
	m, ok := onlyMove(moves, func(m tokenMove) bool { return !m.burn && m.source == IdleATA })
	if !ok || (!m.mint.IsZero() && m.mint != AssetMint) {
		return nil
	}
	amount, err := signed[USDCRaw](m.amount)
	if err != nil {
		return nil
	}
	return &amount
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

// Voltr's published events bind user, amount, LP and vault. Deposit uses the
// pre/post supply and value to cash flows (supply already includes fees).
func eventSupply(logs []string, event string, user solana.PublicKey, amount, lp uint64) (uint64, uint64, USDCRaw, USDCRaw) {
	at, size := 136, 224
	if event == "WithdrawVaultEvent" {
		at, size = 144, 232
	}
	discriminator := sha256.Sum256([]byte("event:" + event))
	var before, after uint64
	var valueBefore, valueAfter USDCRaw
	found := false
	for _, line := range logs {
		if !strings.HasPrefix(line, programData) {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, programData))
		if err != nil || len(b) != size || !bytes.Equal(b[:8], discriminator[:8]) {
			continue
		}
		if !bytes.Equal(b[8:40], user[:]) || !bytes.Equal(b[56:88], Vault[:]) || !bytes.Equal(b[88:120], AssetMint[:]) || binary.LittleEndian.Uint64(b[40:48]) != amount || binary.LittleEndian.Uint64(b[48:56]) != lp {
			continue
		}
		if found {
			return 0, 0, 0, 0
		}
		found = true
		before, after = binary.LittleEndian.Uint64(b[at:at+8]), binary.LittleEndian.Uint64(b[at+8:at+16])
		valueBefore, err = signed[USDCRaw](binary.LittleEndian.Uint64(b[at-16 : at-8]))
		if err != nil {
			return 0, 0, 0, 0
		}
		valueAfter, err = signed[USDCRaw](binary.LittleEndian.Uint64(b[at-8 : at]))
		if err != nil {
			return 0, 0, 0, 0
		}
	}
	return before, after, valueBefore, valueAfter
}
