package referral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type feeState struct {
	Minted, Accrued, Dead uint64
	// Opening accrued LP predates eligibility. Carry it until the next actual harvest.
	OpeningReserve uint64
	Head           string
}

func (s feeState) total() (uint64, error) {
	if ^uint64(0)-s.Minted < s.Accrued || ^uint64(0)-s.Minted-s.Accrued < s.Dead {
		return 0, errors.New("effective LP supply overflow")
	}
	return s.Minted + s.Accrued + s.Dead, nil
}

func (s *feeState) apply(e feeEvent) error {
	total, err := s.total()
	if err != nil {
		return err
	}
	switch e.kind {
	case "strategy":
		if total != e.supplyBefore || ^uint64(0)-s.Accrued < e.fee {
			return errors.New("strategy opening supply mismatch")
		}
		s.Accrued += e.fee
	case "deposit", "withdrawal":
		if total != e.supplyBefore || s.Accrued != e.accBefore || s.Dead != e.dead {
			return errors.New("user operation opening inventory mismatch")
		}
	case "request":
		if total != e.supplyBefore {
			return errors.New("withdrawal request supply mismatch")
		}
	case "harvest":
		if s.Accrued != e.minted {
			return errors.New("treasury harvest differs from explained admin accumulation")
		}
		s.Accrued = 0
		s.OpeningReserve = 0
	case "cancel":
	default:
		return errors.New("unsupported fee event")
	}
	if s.Minted < e.burned || ^uint64(0)-(s.Minted-e.burned) < e.minted {
		return errors.New("LP mint inventory overflow")
	}
	s.Minted = s.Minted - e.burned + e.minted
	after, err := s.total()
	if err != nil {
		return err
	}
	if (e.kind == "strategy" || e.kind == "deposit" || e.kind == "withdrawal") && after != e.supplyAfter {
		return errors.New("closing effective supply mismatch")
	}
	return nil
}

func feeSnapshot(ctx context.Context, c *chain.Client, minSlot uint64) (uint64, feeState, error) {
	var out feeState
	genesis, err := c.GenesisHash(ctx)
	if err != nil {
		return 0, out, err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return 0, out, errors.New("not Solana mainnet")
	}
	dataKey := solana.MustPublicKeyFromBase58(voltr.FeeProgramDataAddress)
	slot, acs, err := c.Accounts(ctx, []solana.PublicKey{earnmax.Vault, earnmax.LPMint, voltr.ProgramID, dataKey}, rpc.CommitmentFinalized, minSlot)
	if err != nil {
		return 0, out, err
	}
	if len(acs) != 4 || acs[0] == nil || acs[1] == nil || acs[2] == nil || acs[3] == nil {
		return slot, out, errors.New("fee checkpoint accounts missing")
	}
	program, data := acs[2], acs[3]
	loader := solana.BPFLoaderUpgradeableProgramID
	if program.Owner != loader || !program.Executable || len(program.Data) != 36 || binary.LittleEndian.Uint32(program.Data) != 2 || !bytes.Equal(program.Data[4:], dataKey[:]) ||
		data.Owner != loader || len(data.Data) < 45 || binary.LittleEndian.Uint32(data.Data) != 3 || binary.LittleEndian.Uint64(data.Data[4:12]) != voltr.FeeProgramDeploySlot {
		return slot, out, errors.New("Voltr executable identity changed")
	}
	digest := sha256.Sum256(data.Data[45:])
	if hex.EncodeToString(digest[:]) != voltr.FeeProgramDataSHA256 {
		return slot, out, errors.New("Voltr executable hash changed")
	}
	v, err := voltr.DecodeVault(acs[0])
	if err != nil {
		return slot, out, err
	}
	if len(acs[0].Data) != voltr.VaultSize || v.Admin != feeAdmin || v.AssetMint != earnmax.AssetMint || v.LPMint != earnmax.LPMint || v.AdminPerformanceFeeBPS != 1500 || v.ManagerPerformanceFeeBPS != 0 || v.ProtocolPerformanceFeeBPS != 0 || v.AdminManagementFeeBPS != 0 || v.ManagerManagementFeeBPS != 0 || v.ProtocolManagementFeeBPS != 0 || v.IssuanceFeeBPS != 40 || v.RedemptionFeeBPS != 0 || v.FeeAccumulatorManagerRaw != 0 || v.FeeAccumulatorProtocolRaw != 0 {
		return slot, out, errors.New("fee recipient or certified fee configuration changed")
	}
	mint, err := spl.DecodeMint(acs[1])
	if err != nil || mint.Program != solana.TokenProgramID {
		return slot, out, errors.New("invalid RWA LP mint")
	}
	auth, _, err := solana.FindProgramAddress([][]byte{[]byte("vault_lp_mint_auth"), earnmax.Vault[:]}, voltr.ProgramID)
	if err != nil || binary.LittleEndian.Uint32(acs[1].Data[:4]) != 1 || !bytes.Equal(acs[1].Data[4:36], auth[:]) {
		return slot, out, errors.New("LP mint authority mismatch")
	}
	out.Minted = binary.LittleEndian.Uint64(acs[1].Data[36:44])
	out.Accrued = v.FeeAccumulatorAdminRaw
	out.Dead = v.LPSupplyDeadWeightRaw
	_, err = out.total()
	return slot, out, err
}

func feeOpeningMarker(ctx context.Context, c *chain.Client, at uint64) (string, error) {
	rows, err := c.History(ctx, earnmax.Vault, 32, solana.Signature{}, rpc.CommitmentFinalized, at)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Slot > at {
			continue
		}
		block, _, err := c.FinalizedBlockSignatures(ctx, r.Slot)
		if err != nil {
			return "", err
		}
		if !slices.Contains(block, r.Signature) {
			return "", errors.New("opening vault marker absent from its finalized block")
		}
		return r.Signature.String(), nil
	}
	return "", errors.New("no finalized vault history marker at opening checkpoint")
}

// Enumerate all signatures mentioning the exact vault across a bounded retained
// finalized interval. A page budget or missing block stops progress at the last
// verified cursor. No current snapshot is substituted for missing evidence.
func feeRange(ctx context.Context, c *chain.Client, from, to uint64, oldHead string) ([]feeEvent, map[uint64][]solana.Signature, string, error) {
	floor, err := c.FirstAvailableBlock(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if floor > from+1 {
		return nil, nil, "", errors.New("vault history not retained")
	}
	if oldHead == "" {
		return nil, nil, "", errors.New("previous vault history marker missing")
	}
	newHead := ""
	matched := false
	var rows []chain.Signed
	var before solana.Signature
	seen := map[solana.Signature]bool{}
	last := ^uint64(0)
	for page := 0; ; page++ {
		found, err := c.History(ctx, earnmax.Vault, 64, before, rpc.CommitmentFinalized, to)
		if err != nil {
			return nil, nil, "", err
		}
		for _, x := range found {
			if x.Slot > last || seen[x.Signature] {
				return nil, nil, "", errors.New("nonmonotonic vault history")
			}
			if x.Slot <= to && newHead == "" {
				newHead = x.Signature.String()
			}
			if x.Signature.String() == oldHead {
				if x.Slot > from {
					return nil, nil, "", errors.New("previous marker moved across finalized checkpoint")
				}
				matched = true
			}
			seen[x.Signature] = true
			last = x.Slot
			if x.Slot > from && x.Slot <= to && !x.Failed {
				rows = append(rows, x)
			}
		}
		if matched || len(found) < 64 || last < from {
			break
		}
		if page >= 3 {
			return nil, nil, "", errors.New("vault history page limit")
		}
		before = found[len(found)-1].Signature
	}
	if !matched || newHead == "" {
		return nil, nil, "", errors.New("previous finalized vault marker missing from archive interval")
	}
	treasury, err := feeTreasury()
	if err != nil {
		return nil, nil, "", err
	}
	blocks := map[uint64][]solana.Signature{}
	blockTimes := map[uint64]time.Time{}
	var events []feeEvent
	for len(rows) > 0 {
		batch := rows[:min(12, len(rows))]
		rows = rows[len(batch):]
		sigs := make([]solana.Signature, len(batch))
		for i, v := range batch {
			sigs[i] = v.Signature
		}
		executions, err := c.Executions(ctx, sigs, rpc.CommitmentFinalized)
		if err != nil {
			return nil, nil, "", err
		}
		for i, v := range batch {
			e := executions[i]
			if e == nil || e.Transaction == nil || e.Err != nil || e.Slot != v.Slot || e.BlockTime == nil || len(e.Transaction.Signatures) == 0 || e.Transaction.Signatures[0] != v.Signature || !slices.Contains(e.Keys, earnmax.Vault) {
				return nil, nil, "", errors.New("vault transaction missing or mismatched")
			}
			if !e.Transaction.Message.IsWritableStatic(earnmax.Vault) && !slices.Contains(e.LoadedWritable, earnmax.Vault) {
				continue
			}
			if _, ok := blocks[v.Slot]; !ok {
				block, blockTime, err := c.FinalizedBlockSignatures(ctx, v.Slot)
				if err != nil {
					return nil, nil, "", err
				}
				blocks[v.Slot] = block
				blockTimes[v.Slot] = blockTime
			}
			idx := slices.Index(blocks[v.Slot], v.Signature)
			if idx < 0 || !e.BlockTime.Equal(blockTimes[v.Slot]) {
				return nil, nil, "", errors.New("transaction absent from finalized block")
			}
			p := FeePoint{Signature: v.Signature.String(), Slot: v.Slot, Index: idx, At: *e.BlockTime}
			ev, err := decodeFeeEvent(*e, p, treasury)
			if err != nil {
				return nil, nil, "", fmt.Errorf("slot %d tx %d: %w", v.Slot, idx, err)
			}
			if ev.kind != "unrelated" {
				events = append(events, ev)
			}
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Slot == events[j].Slot {
			return events[i].Index < events[j].Index
		}
		return events[i].Slot < events[j].Slot
	})
	return events, blocks, newHead, nil
}
