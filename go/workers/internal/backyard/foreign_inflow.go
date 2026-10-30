package backyard

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

type foreignCheckpoint struct {
	Signature  string `json:"signature"`
	Slot       uint64 `json:"slot"`
	BalanceRaw uint64 `json:"balanceRaw"`
	ForeignRaw uint64 `json:"foreignRaw"`
}

type foreignAccountingFault string

func (e foreignAccountingFault) Error() string { return string(e) }

type foreignTransfer struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	AmountRaw   uint64 `json:"amountRaw"`
	Voltr       bool   `json:"voltr"`
	Stage       bool   `json:"stage,omitempty"`
}

type foreignEvidence struct {
	Before    foreignCheckpoint `json:"before"`
	After     foreignCheckpoint `json:"after"`
	Transfers []foreignTransfer `json:"transfers"`
	Execution chain.Execution   `json:"execution"`
}

// Every transfer must explain the token metadata exactly; instruction ancestry,
// rather than transaction membership, distinguishes gifts from Voltr deposits.
func foreignAccountTransfers(v *Vault, address string, x chain.Execution) ([]foreignTransfer, error) {
	key := solanaKey(address)
	authority := v.idleAuth
	if address == v.strategyATA {
		authority = v.strategyAuth
	} else if address != v.idleATA {
		return nil, fmt.Errorf("unknown foreign-inflow account")
	}
	valid := func(b chain.TokenBalance) bool {
		return b.Program == solana.TokenProgramID && b.Mint == solanaKey(fleet.USDCMint) && b.Owner == solanaKey(authority) && b.Decimals == 6 && b.Amount <= math.MaxInt64
	}
	pre, preOK := x.Pre[key]
	post, postOK := x.Post[key]
	if x.Err != nil || x.Transaction == nil || !preOK || !postOK || !valid(pre) || !valid(post) {
		return nil, fmt.Errorf("foreign-inflow token identity or receipt incomplete")
	}
	balance := pre.Amount
	var transfers []foreignTransfer
	transientOwners := make(map[solana.PublicKey]solana.PublicKey)
	visit := func(program uint16, accounts []uint16, data []byte, parents []solana.PublicKey) error {
		if int(program) >= len(x.Keys) {
			return fmt.Errorf("foreign-inflow program index")
		}
		keys := make([]solana.PublicKey, len(accounts))
		for i, index := range accounts {
			if int(index) >= len(x.Keys) {
				return fmt.Errorf("foreign-inflow account index")
			}
			keys[i] = x.Keys[index]
		}
		if x.Keys[program] != solana.TokenProgramID || len(data) == 0 || len(keys) == 0 {
			return nil
		}
		// A sender can create, fund, drain and close a token account in one
		// transaction, leaving no pre/post token metadata for that source.
		if len(keys) >= 2 && keys[1] == solanaKey(fleet.USDCMint) {
			switch {
			case data[0] == 1 && len(data) == 1 && len(keys) >= 3:
				transientOwners[keys[0]] = keys[2]
			case (data[0] == 16 || data[0] == 18) && len(data) == 33:
				transientOwners[keys[0]] = solana.PublicKeyFromBytes(data[1:])
			}
		}
		if len(data) == 35 && data[0] == 6 && data[1] == 2 && data[2] == 1 {
			transientOwners[keys[0]] = solana.PublicKeyFromBytes(data[3:])
		}
		var source, destination solana.PublicKey
		if len(data) == 9 && data[0] == 3 && len(keys) >= 3 {
			source, destination = keys[0], keys[1]
		} else if len(data) == 10 && data[0] == 12 && len(keys) >= 4 && keys[1] == solanaKey(fleet.USDCMint) && data[9] == 6 {
			source, destination = keys[0], keys[2]
		} else {
			// Extra metas, queries and the classic immutable-owner no-op are public;
			// they cannot become a replacement dust-griefing vector.
			if data[0] >= 21 && data[0] <= 24 {
				return nil
			}
			if keys[0] == key || ((data[0] == 7 || data[0] == 14) && len(keys) > 1 && keys[1] == key) {
				return foreignAccountingFault("unsupported mutation of foreign-inflow custody")
			}
			return nil
		}
		if source != key && destination != key {
			return nil
		}
		amount := binary.LittleEndian.Uint64(data[1:9])
		if amount == 0 {
			return nil
		}
		if amount > math.MaxInt64 {
			return fmt.Errorf("foreign-inflow transfer overflow")
		}
		underVoltr := slices.Contains(parents, voltr.ProgramID)
		stage := source == solanaKey(v.cash) && destination == solanaKey(v.strategyATA) && slices.Contains(parents, squads.ProgramID)
		if source != destination {
			if destination == key {
				if balance > math.MaxInt64-amount {
					return fmt.Errorf("foreign-inflow balance overflow")
				}
				balance += amount
				if !underVoltr && !stage {
					b, ok := x.Pre[source]
					if !ok {
						b, ok = x.Post[source]
					}
					if owner, initialized := transientOwners[source]; initialized {
						b, ok = chain.TokenBalance{Owner: owner, Program: solana.TokenProgramID, Mint: solanaKey(fleet.USDCMint)}, true
					}
					if !ok {
						return fmt.Errorf("foreign-inflow source identity unavailable")
					}
					if b.Program != solana.TokenProgramID || b.Mint != solanaKey(fleet.USDCMint) ||
						b.Owner == solanaKey(v.squads) || b.Owner == solanaKey(v.idleAuth) || b.Owner == solanaKey(v.strategyAuth) {
						return foreignAccountingFault("foreign-inflow source is not external custody")
					}
				}
			} else {
				if balance < amount || !underVoltr {
					return foreignAccountingFault("unexplained foreign-inflow custody debit")
				}
				balance -= amount
				if address == v.strategyATA && destination != solanaKey(v.idleATA) && destination != solanaKey(v.cash) {
					return foreignAccountingFault("unexpected strategy custody destination")
				}
			}
			transfers = append(transfers, foreignTransfer{source.String(), destination.String(), amount, underVoltr, stage})
		}
		return nil
	}
	inner := make(map[uint16]bool)
	for i, outer := range x.Transaction.Message.Instructions {
		if err := visit(outer.ProgramIDIndex, outer.Accounts, outer.Data, nil); err != nil {
			return nil, err
		}
		for _, group := range x.Inner {
			if int(group.Index) != i {
				continue
			}
			if inner[group.Index] {
				return nil, fmt.Errorf("duplicate inner-instruction group")
			}
			inner[group.Index] = true
			parents := []solana.PublicKey{x.Keys[outer.ProgramIDIndex]}
			for _, instruction := range group.Instructions {
				height := int(instruction.StackHeight)
				if height < 2 || height > len(parents)+1 {
					return nil, fmt.Errorf("foreign-inflow CPI ancestry unavailable")
				}
				parents = parents[:height-1]
				if err := visit(instruction.ProgramIDIndex, instruction.Accounts, instruction.Data, parents); err != nil {
					return nil, err
				}
				parents = append(parents, x.Keys[instruction.ProgramIDIndex])
			}
		}
	}
	if len(inner) != len(x.Inner) || balance != post.Amount {
		return nil, foreignAccountingFault("unexplained foreign-inflow token balance change")
	}
	return transfers, nil
}

func advanceForeignCheckpoint(v *Vault, address, signature string, before foreignCheckpoint, x chain.Execution) (foreignEvidence, error) {
	transfers, err := foreignAccountTransfers(v, address, x)
	if err != nil {
		return foreignEvidence{}, err
	}
	if x.Slot < before.Slot || x.Pre[solanaKey(address)].Amount != before.BalanceRaw ||
		len(x.Transaction.Signatures) == 0 || x.Transaction.Signatures[0].String() != signature {
		return foreignEvidence{}, foreignAccountingFault("foreign-inflow history discontinuity")
	}
	after := foreignCheckpoint{Signature: signature, Slot: x.Slot, BalanceRaw: x.Post[solanaKey(address)].Amount, ForeignRaw: before.ForeignRaw}
	for _, t := range transfers {
		if t.Destination == address && !t.Voltr && !t.Stage {
			if after.ForeignRaw > math.MaxInt64-t.AmountRaw {
				return foreignEvidence{}, fmt.Errorf("foreign-inflow credit overflow")
			}
			after.ForeignRaw += t.AmountRaw
		}
		if address == v.strategyATA && t.Source == address && t.Destination == v.idleATA && t.Voltr {
			if t.AmountRaw < after.ForeignRaw {
				return foreignEvidence{}, foreignAccountingFault("partial foreign-custody sweep")
			}
			after.ForeignRaw = 0
		}
	}
	if address == v.strategyATA && after.ForeignRaw > after.BalanceRaw {
		return foreignEvidence{}, foreignAccountingFault("foreign-custody deficit")
	}
	return foreignEvidence{before, after, transfers, x}, nil
}
