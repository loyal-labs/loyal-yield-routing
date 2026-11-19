package earnmax

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"maps"
	"math/big"
	"strconv"
	"strings"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// Quantities are raw integers. Net deposit principal is the exact ratio of
// NetAssetNumerator to NetAssetDenominator in micro-USDC. The issuance fee
// and LP mint rounding are already reflected in the shares actually received.
type CapitalEvent struct {
	Kind                string `json:"kind"`
	UnixTime            int64  `json:"unixTime"`
	LP                  uint64 `json:"lp"`
	BurnedLP            uint64 `json:"burnedLp,omitempty"`
	Asset               uint64 `json:"asset,omitempty"`
	NetAssetNumerator   string `json:"netAssetNumerator,omitempty"`
	NetAssetDenominator uint64 `json:"netAssetDenominator,omitempty"`
}

type CapitalMovement struct {
	BoundWalletSigned bool           `json:"boundWalletSigned"`
	FreeBefore        uint64         `json:"freeBefore"`
	FreeAfter         uint64         `json:"freeAfter"`
	EscrowBefore      uint64         `json:"escrowBefore"`
	EscrowAfter       uint64         `json:"escrowAfter"`
	Events            []CapitalEvent `json:"events"`
}

// The offsets and sizes include Anchor's eight-byte event discriminator and
// follow @voltr/vault-sdk 2.1.1's generated vault events.
func capitalEvent(data []byte, owner, receipt solana.PublicKey) (CapitalEvent, bool, error) {
	var e CapitalEvent
	key := func(i int, p solana.PublicKey) bool { return bytes.Equal(data[i:i+32], p[:]) }
	u64 := func(i int) uint64 { return binary.LittleEndian.Uint64(data[i : i+8]) }
	for _, v := range []struct {
		name, kind string
		size       int
	}{
		{"DepositVaultEvent", "deposit", 224}, {"WithdrawVaultEvent", "claim", 232},
		{"RequestWithdrawVaultEvent", "request", 210}, {"CancelRequestWithdrawVaultEvent", "cancel", 160},
	} {
		disc := sha256.Sum256([]byte("event:" + v.name))
		if !bytes.HasPrefix(data, disc[:8]) {
			continue
		}
		if len(data) != v.size {
			return e, false, errors.New("unexpected Voltr capital event size")
		}
		e.Kind = v.kind
		e.UnixTime = int64(u64(len(data) - 8))
		switch e.Kind {
		case "deposit", "claim":
			if !key(8, owner) || !key(56, Vault) || !key(88, AssetMint) {
				return e, false, nil
			}
			e.Asset, e.LP = u64(40), u64(48)
			if e.Kind == "deposit" {
				e.NetAssetDenominator = u64(144)
				if e.NetAssetDenominator == 0 {
					return e, false, errors.New("missing deposit share supply")
				}
				net := new(big.Int).Mul(new(big.Int).SetUint64(e.LP), new(big.Int).SetUint64(u64(128)))
				gross := new(big.Int).Mul(new(big.Int).SetUint64(e.Asset), new(big.Int).SetUint64(e.NetAssetDenominator))
				if e.LP > e.NetAssetDenominator || net.Sign() == 0 || net.Cmp(gross) > 0 {
					return e, false, errors.New("unverified fee-net deposit amount")
				}
				e.NetAssetNumerator = net.String()
			}
		case "request":
			if !key(8, Vault) || !key(40, owner) || !key(82, receipt) || !key(146, AssetMint) {
				return e, false, nil
			}
			e.LP = u64(114)
		case "cancel":
			if !key(8, Vault) || !key(40, owner) || !key(72, receipt) {
				return e, false, nil
			}
			e.LP, e.BurnedLP = u64(104), u64(112)
		}
		if e.UnixTime <= 0 || (e.LP == 0 && e.BurnedLP == 0) {
			return e, false, errors.New("invalid Voltr capital event")
		}
		return e, true, nil
	}
	return e, false, nil
}

// CapitalMovementAt reads execution evidence only; the caller obtains ex at
// finalized commitment and independently verifies the bound account identity.
func CapitalMovementAt(ex chain.Execution, signature solana.Signature, a Authority) (*CapitalMovement, error) {
	if ex.Err != nil || ex.Transaction == nil || len(ex.Transaction.Signatures) == 0 || ex.Transaction.Signatures[0] != signature || ex.BlockTime == nil || ex.BlockTime.Unix() <= 0 {
		return nil, errors.New("incomplete finalized transaction")
	}
	user, err := voltr.UserAccounts(Vault, LPMint, a.key)
	if err != nil {
		return nil, err
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, LPMint, solana.TokenProgramID)
	if err != nil {
		return nil, err
	}
	balance := func(rows map[solana.PublicKey]chain.TokenBalance, key, owner solana.PublicKey) (uint64, error) {
		v, ok := rows[key]
		if ok && (v.Owner != owner || v.Mint != LPMint || v.Program != solana.TokenProgramID) {
			return 0, errors.New("invalid LP account identity")
		}
		return v.Amount, nil
	}
	m := CapitalMovement{}
	if m.FreeBefore, err = balance(ex.Pre, user.LPATA, a.key); err != nil {
		return nil, err
	}
	if m.FreeAfter, err = balance(ex.Post, user.LPATA, a.key); err != nil {
		return nil, err
	}
	if m.EscrowBefore, err = balance(ex.Pre, escrow, user.Receipt); err != nil {
		return nil, err
	}
	if m.EscrowAfter, err = balance(ex.Post, escrow, user.Receipt); err != nil {
		return nil, err
	}
	var stack []string
	for _, line := range ex.Logs {
		p := strings.Fields(line)
		if len(p) >= 3 && p[0] == "Program" {
			switch p[2] {
			case "invoke":
				if len(p) != 4 || p[3] != "["+strconv.Itoa(len(stack)+1)+"]" {
					return nil, errors.New("incomplete invocation logs")
				}
				stack = append(stack, p[1])
			case "failed:":
				return nil, errors.New("failed inner invocation")
			case "success":
				if len(stack) == 0 || stack[len(stack)-1] != p[1] {
					return nil, errors.New("incomplete invocation logs")
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
			return nil, errors.New("invalid Voltr event encoding")
		}
		e, relevant, err := capitalEvent(data, a.key, user.Receipt)
		if err != nil {
			return nil, err
		}
		if relevant {
			m.Events = append(m.Events, e)
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("truncated invocation logs")
	}
	// Reconcile the events with the actual Voltr invocations and SPL transfers.
	// In particular, equal offsetting LP transfers do not establish lot ownership.
	expected := map[string]int{}
	observed := map[string]int{}
	var moves []tokenMove
	for _, ix := range invocations(ex) {
		moves = append(moves, tokenMoves([]call{ix.call}, ex.Keys)...)
		d, _, ok := voltrAction(ix.call, ex.Keys, a.key)
		if !ok {
			continue
		}
		switch d {
		case voltr.DepositVaultDiscriminator:
			expected["deposit"]++
		case voltr.WithdrawVaultDiscriminator:
			expected["claim"]++
		case voltr.RequestWithdrawVaultDiscriminator:
			expected["request"]++
		case voltr.CancelRequestWithdrawVaultDiscriminator:
			expected["cancel"]++
		}
	}
	var escrowMoves []tokenMove
	for _, e := range m.Events {
		observed[e.Kind]++
		switch e.Kind {
		case "request":
			escrowMoves = append(escrowMoves, tokenMove{source: user.LPATA, destination: escrow, amount: e.LP})
		case "cancel":
			if e.LP > 0 {
				escrowMoves = append(escrowMoves, tokenMove{source: escrow, destination: user.LPATA, amount: e.LP})
			}
		}
	}
	if !maps.Equal(expected, observed) {
		return nil, errors.New("Voltr invocation has incomplete event evidence")
	}
	var external []tokenMove
	for _, v := range moves {
		if v.burn || v.amount == 0 || v.source == v.destination {
			continue
		}
		if v.source != user.LPATA && v.destination != user.LPATA && v.source != escrow && v.destination != escrow {
			continue
		}
		if !v.mint.IsZero() && v.mint != LPMint {
			return nil, errors.New("invalid LP transfer mint")
		}
		if len(escrowMoves) > 0 && v.source == escrowMoves[0].source && v.destination == escrowMoves[0].destination && v.amount == escrowMoves[0].amount {
			escrowMoves = escrowMoves[1:]
			continue
		}
		external = append(external, v)
	}
	if len(escrowMoves) != 0 {
		return nil, errors.New("unverified escrow transfer")
	}
	if len(external) > 0 {
		if len(external) != 1 || len(m.Events) != 0 || external[0].source == escrow || external[0].destination == escrow {
			return nil, errors.New("ambiguous LP transfer")
		}
		v := external[0]
		remote := v.source
		if v.source == user.LPATA {
			remote = v.destination
		}
		other, ok := ex.Post[remote]
		if !ok {
			other, ok = ex.Pre[remote]
		}
		if !ok || other.Owner.IsZero() || other.Mint != LPMint || other.Program != solana.TokenProgramID ||
			other.Owner == a.key || other.Owner == user.Receipt {
			return nil, errors.New("unverified external LP account ownership")
		}
		e := CapitalEvent{Kind: "transfer_in", LP: v.amount, UnixTime: ex.BlockTime.Unix()}
		if v.source == user.LPATA {
			e.Kind = "transfer_out"
		}
		m.Events = append(m.Events, e)
	}
	free, held := new(big.Int).SetUint64(m.FreeBefore), new(big.Int).SetUint64(m.EscrowBefore)
	for _, e := range m.Events {
		lp := new(big.Int).SetUint64(e.LP)
		switch e.Kind {
		case "deposit", "transfer_in":
			free.Add(free, lp)
		case "transfer_out":
			free.Sub(free, lp)
		case "request":
			free.Sub(free, lp)
			held.Add(held, lp)
		case "claim":
			held.Sub(held, lp)
		case "cancel":
			free.Add(free, lp)
			held.Sub(held, lp)
			held.Sub(held, new(big.Int).SetUint64(e.BurnedLP))
		}
		if free.Sign() < 0 || held.Sign() < 0 {
			return nil, errors.New("capital event exceeds LP balance")
		}
	}
	if !free.IsUint64() || !held.IsUint64() || free.Uint64() != m.FreeAfter || held.Uint64() != m.EscrowAfter {
		return nil, errors.New("unexplained free or escrow LP change")
	}
	if len(m.Events) == 0 {
		return nil, nil
	}
	return &m, nil
}
