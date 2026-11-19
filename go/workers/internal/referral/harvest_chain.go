package referral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/anchor"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

var (
	harvestManager        = solana.MustPublicKeyFromBase58("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh")
	harvestWorkerDelegate = solana.MustPublicKeyFromBase58("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5")
	protocolAdmin         = solana.MustPublicKeyFromBase58("G2FCNGgQQ7MYyJvkXw1du86YGR6vXXejuQG9LsjX1kEs")
	protocolTreasury      = solana.MustPublicKeyFromBase58("C7sE3MjSAqqF7TgXn1VsNQPWem1gdhqv3ZYV9TNfSjY9")
	harvestOwners         = [3]solana.PublicKey{harvestManager, feeAdmin, protocolTreasury}
	harvestDisc           = anchor.Instruction("harvest_fee")
	protocolDisc          = anchor.Account("Protocol")
)

type harvestAddresses struct {
	protocol, mintAuth solana.PublicKey
	protocolBump       byte
	atas               [3]solana.PublicKey
}

func harvestAccounts() (a harvestAddresses, err error) {
	a.protocol, a.protocolBump, err = solana.FindProgramAddress([][]byte{[]byte("protocol")}, voltr.ProgramID)
	if err != nil {
		return a, err
	}
	a.mintAuth, _, err = solana.FindProgramAddress([][]byte{[]byte("vault_lp_mint_auth"), earnmax.Vault[:]}, voltr.ProgramID)
	if err != nil {
		return a, err
	}
	for i, owner := range harvestOwners {
		a.atas[i], err = spl.AssociatedTokenAddress(owner, earnmax.LPMint, solana.TokenProgramID)
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func ordinaryHarvestPayer(payer solana.PublicKey) bool {
	return !payer.IsZero() && payer != feeAdmin && payer != harvestManager && payer != protocolAdmin &&
		payer != protocolTreasury && payer != harvestWorkerDelegate && solana.IsOnCurve(payer[:])
}

type HarvestQuote struct {
	Slot, AccruedRaw uint64
	payer            solana.PublicKey
	addresses        harvestAddresses
	state            feeState
	balances         [3]uint64
	missing          [3]bool
}

// ReadHarvestQuote certifies all inputs in one finalized bank. AccruedRaw is
// the entire accumulator Voltr can harvest, including any opening reserve.
func ReadHarvestQuote(ctx context.Context, c *chain.Client, payer solana.PublicKey, minSlot uint64) (HarvestQuote, error) {
	var q HarvestQuote
	if !ordinaryHarvestPayer(payer) {
		return q, errors.New("harvest requires a separate ordinary fee payer")
	}
	a, err := harvestAccounts()
	if err != nil {
		return q, err
	}
	extra := append([]solana.PublicKey{a.protocol, payer}, a.atas[:]...)
	slot, state, acs, err := feeSnapshotWithAccounts(ctx, c, minSlot, extra)
	if err != nil {
		return q, err
	}
	v, err := voltr.DecodeVault(acs[0])
	if err != nil || v.Manager != harvestManager || bytes.Equal(payer[:], acs[0].Data[336:368]) {
		return q, errors.New("unexpected Earn MAX harvest authority")
	}
	protocol := acs[4]
	// @voltr/vault-sdk 2.1.1 Protocol codec: admin at 8, bump at 44,
	// pending admin at 46, protocol fee treasury at 78.
	if protocol == nil || protocol.Owner != voltr.ProgramID || protocol.Executable || protocol.Lamports == 0 ||
		len(protocol.Data) != 110 || !bytes.HasPrefix(protocol.Data, protocolDisc[:]) || protocol.Data[44] != a.protocolBump ||
		!bytes.Equal(protocol.Data[8:40], protocolAdmin[:]) || !bytes.Equal(protocol.Data[78:110], protocolTreasury[:]) ||
		bytes.Equal(protocol.Data[46:78], payer[:]) {
		return q, errors.New("unexpected Voltr protocol or protocol treasury")
	}
	funding := acs[5]
	if funding == nil || funding.Executable || funding.Owner != solana.SystemProgramID || len(funding.Data) != 0 || funding.Lamports == 0 {
		return q, errors.New("harvest fee payer must be a funded plain system account")
	}
	q = HarvestQuote{Slot: slot, AccruedRaw: state.Accrued, payer: payer, addresses: a, state: state}
	for i, acc := range acs[6:] {
		if acc == nil || (acc.Owner == solana.SystemProgramID && len(acc.Data) == 0 && !acc.Executable) {
			// An address pre-funded with SOL is still eligible for idempotent ATA creation.
			q.missing[i] = true
			continue
		}
		q.balances[i], err = harvestBalance(acc, a.atas[i], harvestOwners[i])
		if err != nil {
			return HarvestQuote{}, err
		}
	}
	return q, nil
}

func harvestBalance(acc *chain.Account, ata, owner solana.PublicKey) (uint64, error) {
	t, err := spl.DecodeTokenAccount(acc)
	if err != nil || acc.Key != ata || t.Program != solana.TokenProgramID || t.Mint != earnmax.LPMint || t.Owner != owner || t.Frozen || t.IsNative {
		return 0, fmt.Errorf("invalid Earn MAX harvest token account %s", ata)
	}
	return t.Amount, nil
}

type HarvestWire struct {
	Message              []byte // Independent copy for the signer.
	canonicalMessage     []byte
	Blockhash            solana.Hash
	LastValidBlockHeight uint64
	quote                HarvestQuote
	minSlot              uint64
}

func (q HarvestQuote) Build(ctx context.Context, c *chain.Client) (HarvestWire, error) {
	if q.Slot == 0 || q.AccruedRaw == 0 || q.AccruedRaw != q.state.Accrued {
		return HarvestWire{}, errors.New("missing or empty certified harvest quote")
	}
	hash, height, slot, err := c.Blockhash(ctx, rpc.CommitmentFinalized, q.Slot)
	if err != nil {
		return HarvestWire{}, err
	}
	if slot < q.Slot {
		return HarvestWire{}, errors.New("harvest blockhash older than quote")
	}
	return q.wire(hash, height, slot)
}

func (q HarvestQuote) wire(hash solana.Hash, height, slot uint64) (HarvestWire, error) {
	var ix []solana.Instruction
	for i, owner := range harvestOwners {
		if q.missing[i] {
			ix = append(ix, spl.CreateIdempotentATA(q.payer, owner, earnmax.LPMint, solana.TokenProgramID))
		}
	}
	a := q.addresses
	// Order and permissions from the deployed SDK 2.1.1 harvestFee instruction.
	ix = append(ix, solana.NewInstruction(voltr.ProgramID, solana.AccountMetaSlice{
		solana.Meta(q.payer).SIGNER(), solana.Meta(harvestManager), solana.Meta(feeAdmin), solana.Meta(protocolTreasury), solana.Meta(a.protocol),
		solana.Meta(earnmax.Vault).WRITE(), solana.Meta(earnmax.LPMint).WRITE(), solana.Meta(a.mintAuth),
		solana.Meta(a.atas[0]).WRITE(), solana.Meta(a.atas[1]).WRITE(), solana.Meta(a.atas[2]).WRITE(), solana.Meta(solana.TokenProgramID),
	}, harvestDisc[:]))
	tx, err := solana.NewTransaction(ix, hash, solana.TransactionPayer(q.payer))
	if err != nil {
		return HarvestWire{}, err
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return HarvestWire{}, err
	}
	return HarvestWire{Message: bytes.Clone(message), canonicalMessage: message, Blockhash: hash, LastValidBlockHeight: height, quote: q, minSlot: slot}, nil
}

func harvestPacket(sig solana.Signature, message []byte) []byte {
	wire := make([]byte, 1+len(sig)+len(message))
	wire[0] = 1
	copy(wire[1:65], sig[:])
	copy(wire[65:], message)
	return wire
}

// Signed accepts only a signature, keeping key custody outside this package.
func (w HarvestWire) Signed(sig solana.Signature) ([]byte, error) {
	wire := harvestPacket(sig, w.canonicalMessage)
	if err := ValidateHarvestWire(wire, w.quote.payer, w.Blockhash); err != nil {
		return nil, err
	}
	return wire, nil
}

// ValidateHarvestWire reconstructs the permitted legacy transaction, including
// account flags and ATA creation order, before trusting a persisted attempt.
func ValidateHarvestWire(wire []byte, payer solana.PublicKey, hash solana.Hash) error {
	if len(wire) <= 65 || len(wire) > 1232 || wire[0] != 1 || hash.IsZero() || !ordinaryHarvestPayer(payer) {
		return errors.New("invalid signed harvest envelope")
	}
	if !solana.Signature(wire[1:65]).Verify(payer, wire[65:]) {
		return errors.New("invalid harvest payer signature")
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return err
	}
	ins := tx.Message.Instructions
	if len(ins) < 1 || len(ins) > 4 {
		return errors.New("unexpected harvest instruction count")
	}
	a, err := harvestAccounts()
	if err != nil {
		return err
	}
	q := HarvestQuote{payer: payer, addresses: a}
	for _, ix := range ins[:len(ins)-1] {
		if len(ix.Accounts) < 2 || int(ix.Accounts[1]) >= len(tx.Message.AccountKeys) {
			return errors.New("invalid harvest ATA instruction")
		}
		for i, ata := range a.atas {
			if ata == tx.Message.AccountKeys[ix.Accounts[1]] {
				q.missing[i] = true
			}
		}
	}
	expected, err := q.wire(hash, 0, 0)
	if err != nil {
		return err
	}
	if !bytes.Equal(wire[65:], expected.Message) {
		return errors.New("signed wire differs from canonical Earn MAX harvest")
	}
	return nil
}

func (w HarvestWire) Simulate(ctx context.Context, c *chain.Client, wire []byte) error {
	if err := ValidateHarvestWire(wire, w.quote.payer, w.Blockhash); err != nil {
		return err
	}
	if !bytes.Equal(wire[65:], w.canonicalMessage) {
		return errors.New("harvest wire differs from simulation quote")
	}
	return w.simulate(ctx, c, wire, true)
}

func (w HarvestWire) SimulateUnsigned(ctx context.Context, c *chain.Client) error {
	return w.simulate(ctx, c, harvestPacket(solana.Signature{}, w.canonicalMessage), false)
}

func (w HarvestWire) simulate(ctx context.Context, c *chain.Client, wire []byte, signed bool) error {
	keys := append([]solana.PublicKey{earnmax.Vault, earnmax.LPMint}, w.quote.addresses.atas[:]...)
	out, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{SigVerify: signed, Commitment: rpc.CommitmentFinalized, MinContextSlot: &w.minSlot,
		Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: keys}})
	if err != nil {
		return err
	}
	if err := w.quote.checkHarvestEvent(out.Logs); err != nil {
		return err
	}
	return w.quote.checkEffects(out.Accounts)
}

// A concurrent permissionless harvest can produce the same closing balances
// even when this simulated transaction minted nothing. Require its own event.
func (q HarvestQuote) checkHarvestEvent(logs []string) error {
	var count int
	inHarvest := false
	voltrLog := "Program " + voltr.ProgramID.String()
	for _, line := range logs {
		switch {
		case line == voltrLog+" invoke [1]":
			inHarvest = true
		case line == voltrLog+" success":
			inHarvest = false
		case inHarvest && strings.HasPrefix(line, "Program data: "):
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "Program data: "))
			if err != nil || len(data) != 192 || !feeDisc(data, "event:HarvestFeeEvent") || !bytes.Equal(data[8:40], q.payer[:]) ||
				!bytes.Equal(data[40:72], earnmax.Vault[:]) || !bytes.Equal(data[104:136], feeAdmin[:]) ||
				!bytes.Equal(data[136:168], harvestManager[:]) || binary.LittleEndian.Uint64(data[168:176]) != q.AccruedRaw ||
				binary.LittleEndian.Uint64(data[176:184]) != 0 || binary.LittleEndian.Uint64(data[184:192]) != 0 {
				return errors.New("simulated harvest event does not match the quoted admin fee")
			}
			count++
		}
	}
	if inHarvest || count != 1 {
		return errors.New("missing or ambiguous simulated Voltr harvest event")
	}
	return nil
}

func (q HarvestQuote) checkEffects(accounts []*chain.Account) error {
	if len(accounts) != 5 {
		return errors.New("incomplete harvest simulation evidence")
	}
	state, err := certifiedFeeInventory(accounts[0], accounts[1])
	if err != nil {
		return err
	}
	if state.Accrued != 0 || state.Dead != q.state.Dead || state.Minted < q.state.Minted || state.Minted-q.state.Minted != q.AccruedRaw {
		return errors.New("harvest did not mint the expected LP supply and clear accrued fees")
	}
	for i, before := range q.balances {
		after, err := harvestBalance(accounts[2+i], q.addresses.atas[i], harvestOwners[i])
		if err != nil {
			return err
		}
		want := before
		if harvestOwners[i] == feeAdmin {
			if before > math.MaxUint64-q.AccruedRaw {
				return errors.New("harvest treasury overflow")
			}
			want += q.AccruedRaw
		}
		if after != want {
			return fmt.Errorf("unexpected harvested LP balance at %s", q.addresses.atas[i])
		}
	}
	return nil
}
