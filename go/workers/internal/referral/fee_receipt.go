package referral

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type FeePoint struct {
	Signature string
	Slot      uint64
	Index     int
	At        time.Time
}

var feeAdmin = solana.MustPublicKeyFromBase58("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ")

type feeEvent struct {
	FeePoint
	kind                                           string
	supplyBefore, supplyAfter, fee, minted, burned uint64
	accBefore, accAfter, dead                      uint64 // deposit only
	owners                                         []feeOwner
}

type feeCall struct {
	program  solana.PublicKey
	accounts []uint16
	data     []byte
	depth    int
}

func feeDisc(data []byte, label string) bool {
	d := sha256.Sum256([]byte(label))
	return len(data) >= 8 && bytes.Equal(data[:8], d[:8])
}

func feeCalls(e chain.Execution) ([]feeCall, error) {
	inners := make(map[int][]feeCall, len(e.Inner))
	push := func(pid uint16, ix []uint16, data []byte, depth int) (feeCall, error) {
		if int(pid) >= len(e.Keys) {
			return feeCall{}, errors.New("invalid program index")
		}
		for _, i := range ix {
			if int(i) >= len(e.Keys) {
				return feeCall{}, errors.New("invalid account index")
			}
		}
		return feeCall{e.Keys[pid], ix, data, depth}, nil
	}
	for _, g := range e.Inner {
		if int(g.Index) >= len(e.Transaction.Message.Instructions) || inners[int(g.Index)] != nil {
			return nil, errors.New("invalid inner instruction group")
		}
		for _, ix := range g.Instructions {
			c, err := push(ix.ProgramIDIndex, ix.Accounts, ix.Data, int(ix.StackHeight))
			if err != nil {
				return nil, err
			}
			inners[int(g.Index)] = append(inners[int(g.Index)], c)
		}
	}
	var calls []feeCall
	for i, ix := range e.Transaction.Message.Instructions {
		c, err := push(ix.ProgramIDIndex, ix.Accounts, ix.Data, 1)
		if err != nil {
			return nil, err
		}
		calls = append(calls, c)
		calls = append(calls, inners[i]...)
	}
	return calls, nil
}

// Authenticate Program data against the actual invoked program, accounts,
// instruction discriminator and successful runtime call frame.
func decodeFeeEvent(e chain.Execution, p FeePoint, treasury solana.PublicKey) (feeEvent, error) {
	out := feeEvent{FeePoint: p}
	if e.Transaction == nil || e.Err != nil || e.Slot != p.Slot || len(e.Transaction.Signatures) == 0 || e.Transaction.Signatures[0].String() != p.Signature {
		return out, errors.New("unmatched finalized transaction")
	}
	calls, err := feeCalls(e)
	if err != nil {
		return out, err
	}
	var stack []int
	next, relevant, emitted := 0, -1, false
	var payload []byte
	for _, line := range e.Logs {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "Program" {
			switch f[2] {
			case "invoke":
				depth := len(stack) + 1
				if len(f) != 4 || f[3] != "["+strconv.Itoa(depth)+"]" || next >= len(calls) || calls[next].depth != depth || calls[next].program.String() != f[1] {
					return out, errors.New("unmatched invocation trace")
				}
				c := calls[next]
				if c.program == voltr.ProgramID && slices.ContainsFunc(c.accounts, func(i uint16) bool { return e.Keys[i] == earnmax.Vault }) {
					if relevant >= 0 {
						return out, errors.New("multiple vault invocations in one transaction")
					}
					relevant = next
				}
				stack = append(stack, next)
				next++
			case "success":
				if len(stack) == 0 || calls[stack[len(stack)-1]].program.String() != f[1] {
					return out, errors.New("unmatched invocation return")
				}
				stack = stack[:len(stack)-1]
			case "failed:":
				return out, errors.New("inner invocation failed")
			}
		}
		if strings.HasPrefix(line, "Log truncated") {
			return out, errors.New("truncated fee receipt")
		}
		if data, ok := strings.CutPrefix(line, "Program data: "); ok && len(stack) > 0 && stack[len(stack)-1] == relevant {
			if emitted {
				return out, errors.New("multiple vault events")
			}
			payload, err = base64.StdEncoding.DecodeString(data)
			if err != nil {
				return out, err
			}
			emitted = true
		}
	}
	if len(stack) != 0 || next != len(calls) {
		return out, errors.New("incomplete vault transaction invocation trace")
	}
	if relevant < 0 {
		// Only the owning program can change the data of this funded vault.
		out.kind = "unrelated"
		return out, nil
	}
	if !emitted {
		return out, errors.New("missing complete Earn Max invocation")
	}
	c := calls[relevant]
	key := func(pos int, want solana.PublicKey) bool {
		return len(c.accounts) > pos && e.Keys[c.accounts[pos]] == want
	}
	field := func(pos int, want solana.PublicKey) bool {
		return len(payload) >= pos+32 && bytes.Equal(payload[pos:pos+32], want[:])
	}
	u := func(offset int) uint64 { return binary.LittleEndian.Uint64(payload[offset:]) }
	switch {
	case feeDisc(c.data, "global:deposit_strategy") && feeDisc(payload, "event:DepositStrategyEvent"),
		feeDisc(c.data, "global:withdraw_strategy") && feeDisc(payload, "event:WithdrawStrategyEvent"):
		if len(payload) != 312 || !key(2, earnmax.Vault) || !key(9, earnmax.LPMint) || !field(40, earnmax.Vault) || !field(168, earnmax.AssetMint) {
			return out, errors.New("strategy event identity mismatch")
		}
		out.kind = "strategy"
		out.supplyBefore, out.supplyAfter = u(224), u(232)
		if out.supplyAfter < out.supplyBefore {
			return out, errors.New("unexpected strategy supply decrease")
		}
		out.fee = out.supplyAfter - out.supplyBefore
		if out.fee > 0 && (u(200) != 0 || u(216) <= u(208) || (u(264) < u(248) || (u(264) == u(248) && u(256) <= u(240)))) {
			return out, errors.New("performance assessment not isolated above high water mark")
		}
	case feeDisc(c.data, "global:deposit_vault") && feeDisc(payload, "event:DepositVaultEvent"):
		if len(payload) != 224 || !key(2, earnmax.Vault) || !key(4, earnmax.LPMint) || !field(56, earnmax.Vault) || !field(88, earnmax.AssetMint) {
			return out, errors.New("deposit event identity mismatch")
		}
		out.kind = "deposit"
		out.minted = u(48)
		out.supplyBefore, out.supplyAfter = u(136), u(144)
		out.accBefore, out.accAfter = u(152), u(160)
		out.dead = u(168)
		if out.accBefore != out.accAfter || out.dead != u(176) || out.supplyAfter < out.supplyBefore || out.supplyAfter-out.supplyBefore != out.minted {
			return out, errors.New("deposit has unclassified fee or supply effects")
		}
	case feeDisc(c.data, "global:request_withdraw_vault") && feeDisc(payload, "event:RequestWithdrawVaultEvent"):
		if len(payload) != 210 || !key(3, earnmax.Vault) || !key(4, earnmax.LPMint) || !field(8, earnmax.Vault) || !field(146, earnmax.AssetMint) {
			return out, errors.New("withdrawal request identity mismatch")
		}
		out.kind = "request"
		out.supplyBefore = u(194)
	case feeDisc(c.data, "global:cancel_request_withdraw_vault") && feeDisc(payload, "event:CancelRequestWithdrawVaultEvent"):
		if len(payload) != 160 || !key(2, earnmax.Vault) || !key(3, earnmax.LPMint) || !field(8, earnmax.Vault) {
			return out, errors.New("cancel identity mismatch")
		}
		out.kind = "cancel"
		out.burned = u(112)
	case feeDisc(c.data, "global:withdraw_vault") && feeDisc(payload, "event:WithdrawVaultEvent"):
		if len(payload) != 232 || !key(2, earnmax.Vault) || !key(4, earnmax.LPMint) || !field(56, earnmax.Vault) || !field(88, earnmax.AssetMint) {
			return out, errors.New("withdrawal identity mismatch")
		}
		out.kind = "withdrawal"
		out.burned = u(48)
		out.supplyBefore, out.supplyAfter = u(144), u(152)
		out.accBefore, out.accAfter = u(160), u(168)
		out.dead = u(176)
		if out.accBefore != out.accAfter || out.dead != u(184) || out.supplyBefore < out.supplyAfter || out.supplyBefore-out.supplyAfter != out.burned {
			return out, errors.New("withdrawal has unclassified fee or supply effects")
		}
	case feeDisc(c.data, "global:harvest_fee") && feeDisc(payload, "event:HarvestFeeEvent"):
		if len(payload) != 192 || !key(2, feeAdmin) || !key(5, earnmax.Vault) || !key(6, earnmax.LPMint) || !key(9, treasury) || !key(11, solana.TokenProgramID) || !field(40, earnmax.Vault) || !field(104, feeAdmin) || u(176) != 0 || u(184) != 0 {
			return out, errors.New("harvest recipient, vault or fee split mismatch")
		}
		out.kind = "harvest"
		out.minted = u(168)
		if err := verifyFeeMint(e, calls, treasury, out.minted); err != nil {
			return out, err
		}
	default:
		return out, errors.New("unsupported Earn Max instruction or event")
	}
	var minted, burned uint64
	for _, ix := range calls {
		if ix.program != solana.TokenProgramID || len(ix.data) == 0 {
			continue
		}
		mintAt := -1
		switch ix.data[0] {
		case 7, 14:
			mintAt = 0
		case 8, 15:
			mintAt = 1
		}
		if mintAt < 0 || len(ix.accounts) <= mintAt || e.Keys[ix.accounts[mintAt]] != earnmax.LPMint {
			continue
		}
		if len(ix.data) < 9 {
			return out, errors.New("truncated LP mint or burn")
		}
		target := &minted
		if mintAt == 1 {
			target = &burned
		}
		amount := binary.LittleEndian.Uint64(ix.data[1:9])
		if ^uint64(0)-*target < amount {
			return out, errors.New("LP amount overflow")
		}
		*target += amount
	}
	if minted != out.minted || burned != out.burned {
		return out, errors.New("LP mint/burn effects differ from event")
	}
	return out, nil
}

func verifyFeeMint(e chain.Execution, calls []feeCall, treasury solana.PublicKey, amount uint64) error {
	pre, post := e.Pre[treasury], e.Post[treasury]
	for _, side := range []map[solana.PublicKey]chain.TokenBalance{e.Pre, e.Post} {
		if t, ok := side[treasury]; ok && (t.Program != solana.TokenProgramID || t.Mint != earnmax.LPMint || t.Owner != feeAdmin) {
			return errors.New("harvest treasury token identity mismatch")
		}
	}
	if post.Amount < pre.Amount || post.Amount-pre.Amount != amount {
		return errors.New("actual treasury credit differs from harvest")
	}
	mints := 0
	for _, c := range calls {
		if c.program != solana.TokenProgramID || len(c.data) == 0 || (c.data[0] != 7 && c.data[0] != 14) || len(c.accounts) < 2 || e.Keys[c.accounts[0]] != earnmax.LPMint {
			continue
		}
		if len(c.data) >= 9 && binary.LittleEndian.Uint64(c.data[1:9]) == 0 {
			continue
		}
		if e.Keys[c.accounts[1]] != treasury {
			return errors.New("unclassified LP mint during harvest")
		}
		if len(c.data) < 9 || binary.LittleEndian.Uint64(c.data[1:9]) != amount {
			return errors.New("treasury mint instruction amount mismatch")
		}
		mints++
	}
	if (amount > 0 && mints != 1) || (amount == 0 && mints > 1) {
		return fmt.Errorf("unexpected treasury mint instruction count %d", mints)
	}
	return nil
}

func feeTreasury() (solana.PublicKey, error) {
	return spl.AssociatedTokenAddress(feeAdmin, earnmax.LPMint, solana.TokenProgramID)
}
