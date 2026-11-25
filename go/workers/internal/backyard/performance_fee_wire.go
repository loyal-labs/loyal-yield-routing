package backyard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/solana-foundation/solana-go/v2"
)

const feeLighthouse = "L2TExMFKdjpN9kozasaurPirfHy9P8sbXoAN1qA3S95"

// The immutable Lighthouse ABI is also used by the official Voltr deposit wire.
// Check what can change after observation in the same bank as the publication.
type feeBytePin struct {
	Offset uint64
	Bytes  []byte
}
type feeReportWire struct {
	Entry                                              performanceFeeEntry
	PayoutPolicy                                       string
	VaultPins                                          []feeBytePin
	MintSupply, Idle, PriorStrategy, ReceiptTime, Cash uint64
	MaxSlot                                            int64
}

func feeAssertBytes(account string, pins ...feeBytePin) compiledInstruction {
	data := binary.AppendUvarint([]byte{3, 0}, uint64(len(pins)))
	for _, pin := range pins {
		data = binary.AppendUvarint(data, pin.Offset)
		data = append(data, 11)
		data = binary.AppendUvarint(data, uint64(len(pin.Bytes)))
		data = append(data, pin.Bytes...)
		data = append(data, 0)
	}
	return compiledInstruction{program: mustKey(feeLighthouse), accounts: []accountMeta{{key: mustKey(account)}}, data: data}
}

func feeU64(offset uint64, value uint64) feeBytePin {
	return feeBytePin{offset, binary.LittleEndian.AppendUint64(nil, value)}
}

func (w *feeReportWire) instructions(v *Vault, r BridgeBuildRequest, delegate publicKey, report compiledInstruction) ([]compiledInstruction, error) {
	e := w.Entry
	if v != looping || !e.valid() || uint64(e.Slot) != r.Report.ObservedSlot || w.MaxSlot < e.Slot ||
		uint64(e.After.PublishedStrategy) != r.Report.NAVAfterRaw || (Action(r.Action) == PayPerformanceFee) != (e.Paid > 0) ||
		(e.Paid > 0 && (r.AmountRaw != uint64(e.Paid) || w.PayoutPolicy == "")) || len(w.VaultPins) != 3 {
		return nil, fmt.Errorf("invalid Looping fee wire binding")
	}
	weekEnd := binary.LittleEndian.AppendUint64([]byte{15, 0, 4}, uint64(mondayCutoff(e.At)+7*86400-1))
	clock := binary.LittleEndian.AppendUint64([]byte{15, 0, 0}, uint64(w.MaxSlot))
	ops := []compiledInstruction{
		{program: mustKey(feeLighthouse), data: append(clock, 5)},
		{program: mustKey(feeLighthouse), data: append(weekEnd, 5)},
		feeAssertBytes(v.VoltrVault, w.VaultPins...),
		feeAssertBytes(v.lpMint, feeU64(36, w.MintSupply)),
		feeAssertBytes(v.idleATA, feeU64(64, w.Idle)),
		feeAssertBytes(v.cash, feeU64(64, w.Cash)),
		feeAssertBytes(v.strategyReceipt, feeU64(104, w.PriorStrategy), feeU64(112, w.ReceiptTime)),
	}
	if e.Paid > 0 {
		owner := mustKey(backyardSettingsSigner)
		ops = append(ops, feeAssertBytes(loopingTreasuryUSDC, feeBytePin{32, owner[:]}))
		move := sdkInstruction(spl.TransferChecked(solana.TokenProgramID, solanaKey(v.cash), solanaKey(fleet.USDCMint), solanaKey(loopingTreasuryUSDC), solanaKey(v.squads), uint64(e.Paid), 6))
		policy, err := decodeKey(w.PayoutPolicy)
		if err != nil {
			return nil, err
		}
		pay, err := wrapSquadsPolicyForDelegate(v, policy, delegate, delegate, []byte{0}, []compiledInstruction{move})
		if err != nil {
			return nil, err
		}
		ops = append(ops, pay)
	}
	ops = append(ops, report, feeAssertBytes(v.strategyReceipt, feeU64(104, r.Report.NAVAfterRaw)), feeAssertBytes(v.VoltrVault, feeU64(168, uint64(e.After.PublishedValue))))
	return ops, nil
}

func (e performanceFeeEntry) valid() bool {
	a, b := e.After, e.Before
	if r := e.Resolution; r != (feeCapitalResolution{}) && (!r.valid() || r.Signature != b.CapitalHold || r.Slot < b.CapitalSlot || r.Slot > e.Slot || e.Assessed != 0 || a.LastAssessment != e.At) {
		return false
	}
	if !a.valid() || !b.valid() || a.ActivationSlot != b.ActivationSlot || a.WeeklyCapRaw != b.WeeklyCapRaw || e.Slot < b.ActivationSlot || e.At <= 0 || e.At < b.LastAssessment || a.Version != b.Version+1 ||
		e.Assessed < 0 || e.OutsideCash < 0 || e.Paid < 0 || e.Paid > b.paymentRoom(e.At) || a.PaidThisWeek != b.paidSinceMonday(e.At)+e.Paid || b.Outstanding > math.MaxInt64-e.Assessed ||
		a.Outstanding != b.Outstanding+e.Assessed-e.Paid || a.Cutoff != mondayCutoff(e.At) || a.CapitalSlot > e.Slot || a.CapitalSlot < b.CapitalSlot ||
		(a.CapitalSlot != b.CapitalSlot && (a.CapitalSlot != e.Slot || a.CapitalHold != "")) || a.CapitalHold != "" && e.Assessed != 0 {
		return false
	}
	mature := b.matured(e.At)
	if e.At == a.Cutoff {
		mature += e.Assessed
	}
	return a.Payable == mature-e.Paid && (a.LastAssessment == b.LastAssessment && e.Assessed == 0 || a.LastAssessment == e.At && (b.Version == 0 && e.Assessed == 0 || b.Version > 0 && e.At-b.LastAssessment >= 3600))
}

func prepareFeeReport(m RouteManifest, d Decision, s Snapshot, nav RouteNAVSnapshot, accounts []ConfirmedAccount, post RouteNAVCustodies) (*feeReportWire, error) {
	v := m.vault
	if v != looping || !s.netKnown(time.Now()) || !nav.net.valid(time.Now(), s.Slot) || nav.Slot != s.Slot ||
		nav.Receipt.CustodyTrackedRaw != 0 || s.VoltrStrategyIdleRaw != 0 {
		return nil, staleHold("looping_fee_book_not_reconciled")
	}
	booked, err := feeBookedIdle(s.VoltrTotalValueRaw, s.PriorReportedNAVRaw, s.VoltrIdleRaw)
	if err != nil {
		return nil, err
	}
	unbooked := uint64(s.VoltrIdleRaw - booked)
	if post.VoltrIdleRaw < unbooked || nav.TotalVaultNAVRaw < unbooked {
		return nil, staleHold("looping_fee_unbooked_idle")
	}
	paid := int64(0)
	if d.Action == PayPerformanceFee {
		paid = d.AmountRaw
		if s.WithdrawalDemandRaw != 0 || s.feeAllowance == nil || s.feeAllowance.cap(paid) != paid || paid <= 0 ||
			paid > s.SquadsIdleRaw || paid > max(0, s.SquadsIdleRaw+booked-v.selectorPolicy().idleBufferRaw(s.TotalVaultNAVRaw)) {
			return nil, staleHold("looping_fee_liquidity_unavailable")
		}
	}
	entry, err := s.performanceFee.assess(s, int64(nav.TotalVaultNAVRaw-unbooked)+paid, int64(nav.StrategyNAVRaw), int64(post.SquadsUSDCraw), paid, d.Action == ReportNAV || d.Action == PayPerformanceFee)
	if err != nil {
		return nil, err
	}
	if uint64(entry.After.PublishedValue) != post.VoltrIdleRaw-unbooked+uint64(entry.After.PublishedStrategy) {
		return nil, staleHold("looping_fee_net_book_mismatch")
	}
	data := accountAt(accounts, v.VoltrVault).Data
	if len(data) < 688 {
		return nil, staleHold("looping_fee_vault_unavailable")
	}
	pins := []feeBytePin{{168, append([]byte(nil), data[168:176]...)}, {512, append([]byte(nil), data[512:528]...)}, {568, append([]byte(nil), data[568:688]...)}}
	return &feeReportWire{Entry: entry, VaultPins: pins, MintSupply: nav.LPSupplyRaw, Idle: uint64(s.VoltrIdleRaw), PriorStrategy: uint64(s.PriorReportedNAVRaw), ReceiptTime: nav.PriorReportUpdatedTS, Cash: uint64(s.SquadsIdleRaw), MaxSlot: nav.net.maxSlot()}, nil
}

func validateFeeEffects(v *Vault, x ExpectedEffects) error {
	var treasury, source *ExpectedAccountEffect
	for i := range x.Accounts {
		a := &x.Accounts[i]
		if a.Address == loopingTreasuryUSDC {
			treasury = a
		}
		if a.Address == v.cash {
			source = a
		}
	}
	e := x.PerformanceFee
	if e == nil {
		if treasury != nil {
			return fmt.Errorf("treasury transfer lacks fee accounting")
		}
		return nil
	}
	if v != looping || !e.valid() || x.Kind != effectsBridge || !x.Conserved || source == nil || source.Mint != fleet.USDCMint || source.Owner != classicTokenProgram || source.Authority != v.squads ||
		(treasury != nil) != (e.Paid > 0) {
		return fmt.Errorf("invalid Looping fee evidence")
	}
	if e.Paid > 0 && (treasury.Owner != classicTokenProgram || treasury.Mint != fleet.USDCMint || treasury.Authority != backyardSettingsSigner ||
		treasury.AfterRaw < treasury.BeforeRaw || treasury.AfterRaw-treasury.BeforeRaw != uint64(e.Paid) || source.BeforeRaw < source.AfterRaw || source.BeforeRaw-source.AfterRaw != uint64(e.Paid)) {
		return fmt.Errorf("Looping fee transfer is not exact")
	}
	return nil
}

// An RPC never sees the delegate signature until this exact simulated message
// is durable. Recheck the signer, blockhash and wire at that boundary.
func validateSignedFeeWire(s SignedTransaction, sim SimulationResult, delegate publicKey) error {
	message, sig, err := splitSignedWire(s.signedWire)
	if err != nil || len(s.signedWire) > maxTransactionBytes || !bytes.Equal(s.message, message) || sim.Slot <= 0 || s.lastValidBlockHeight <= 0 ||
		sim.MessageSHA256 != sha256Bytes(message) || s.messageSHA256 != sim.MessageSHA256 || s.signedWireSHA256 != sha256Bytes(s.signedWire) ||
		s.transactionSignature != encodeBase58(sig) || !ed25519.Verify(delegate[:], message, sig) {
		return fmt.Errorf("signed Looping fee wire differs from its simulation")
	}
	m, err := decodeV1Message(message)
	if err != nil || len(m.AccountKeys) == 0 || publicKey(m.AccountKeys[0]) != delegate || m.Header.NumRequiredSignatures != 1 || m.RecentBlockhash.String() != s.recentBlockhash {
		return fmt.Errorf("invalid Looping fee signer or blockhash")
	}
	return nil
}
