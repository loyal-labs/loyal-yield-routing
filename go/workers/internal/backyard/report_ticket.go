package backyard

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

// Report-ticket v1 is the narrow fallback for Voltr not forwarding the Squads
// vault signer through its adaptor CPI. The direct adaptor instruction and the
// consuming Voltr instruction must be the first and second instruction of one
// Squads sync payload; the ticket is never valid across transactions.
//
// Pinned to the strategy-two cutover: seed "report_ticket" plus the
// strategy-two adaptor config under the adaptor program (bump 255), observed
// disarmed with zero sequences at finalized slot 446295496. It replaces the
// retired v2 ticket C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5 (bump 254).
const (
	reportTicketPDA         = "8zdYvAsntUxgaSY4CBh2Kmqf5EhUYi13eAMK6yinyJiq"
	reportTicketStateLength = 96
	reportTicketVersion     = byte(1)
	reportTicketBump        = byte(255)
	reportTicketDeposit     = byte(0)
	reportTicketWithdraw    = byte(1)
)

var (
	reportTicketStateDiscriminator = []byte{0xf5, 0x68, 0xb6, 0xc5, 0x3a, 0xe7, 0x74, 0xed}
	armReportDiscriminator         = []byte{0xa4, 0xaf, 0xf6, 0x29, 0xb2, 0x8c, 0x23, 0x03}
)

type observedReportTicket struct {
	LastConsumedSequence uint64
	Armed                bool
}

func decodeObservedReportTicket(account ConfirmedAccount) (observedReportTicket, error) {
	if account.Address != reportTicketPDA || account.Owner != bridgeAdaptorProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) != reportTicketStateLength ||
		!bytes.Equal(account.Data[:8], reportTicketStateDiscriminator) || account.Data[8] != reportTicketVersion ||
		account.Data[9] != reportTicketBump || account.Data[10] > 1 || !allZero(account.Data[11:16]) ||
		!sameKey(account.Data[16:48], bridgeStrategy) {
		return observedReportTicket{}, fmt.Errorf("report ticket identity or layout drifted")
	}
	lastConsumed := binary.LittleEndian.Uint64(account.Data[48:56])
	activeSequence := binary.LittleEndian.Uint64(account.Data[56:64])
	armed := account.Data[10] == 1
	activeHashIsZero := allZero(account.Data[64:96])
	if (!armed && (activeSequence != 0 || !activeHashIsZero)) ||
		(armed && (activeSequence == 0 || activeHashIsZero)) {
		return observedReportTicket{}, fmt.Errorf("report ticket armed state is incoherent")
	}
	return observedReportTicket{LastConsumedSequence: lastConsumed, Armed: armed}, nil
}

// ticketedBridgeInstructions is the bridge action's inner instructions and
// the leg each executes under.
func ticketedBridgeInstructions(request BridgeBuildRequest) ([]compiledInstruction, []byte, error) {
	capital, err := bridgeInstruction(request)
	if err != nil {
		return nil, nil, err
	}
	if request.Action == StageSquadsToVoltr {
		return []compiledInstruction{capital}, []byte{bridgeStageLeg}, nil
	}
	arm, err := armReportInstruction(request.Action, capital.data)
	if err != nil {
		return nil, nil, err
	}
	return []compiledInstruction{arm, capital}, []byte{bridgeArmLeg, bridgeCapitalLeg}, nil
}

func armReportInstruction(action Action, voltrData []byte) (compiledInstruction, error) {
	operation := byte(0xff)
	switch action {
	case VoltrAllocateToSquads, ReportNAV:
		operation = reportTicketDeposit
	case VoltrRestoreIdle:
		operation = reportTicketWithdraw
	default:
		return compiledInstruction{}, fmt.Errorf("action %s cannot arm an adaptor report ticket", action)
	}
	tail, err := exactVoltrCapitalTail(voltrData)
	if err != nil {
		return compiledInstruction{}, err
	}
	data := append([]byte(nil), armReportDiscriminator...)
	data = append(data, operation)
	data = append(data, tail...)
	adaptor := solanaKey(bridgeAdaptorProgram)
	accounts := squads.Metas(adaptor, armReportSlots(armReport[solana.PublicKey]{Strategy: solanaKey(bridgeStrategy), Ticket: solanaKey(reportTicketPDA),
		Settings: solanaKey(bridgeSettings), Vault: solanaKey(bridgeVault)}, squads.ProgramID))
	return sdkInstruction(solana.NewInstruction(adaptor, accounts, data)), nil
}

// armReport is the adaptor's ArmReport account set: the strategy-two adaptor
// config, the report ticket, the Squads settings and the vault that signs;
// the Squads program follows them. The builder and armReportAllowed read the
// same slot list.
type armReport[T any] struct {
	Strategy, Ticket, Settings, Vault T
}

func armReportSlots[T any](a armReport[T], squadsProgram T) []squads.AccountSlot[T] {
	return []squads.AccountSlot[T]{squads.ReadOnly(a.Strategy), squads.Writable(a.Ticket), squads.ReadOnly(a.Settings), squads.Signing(a.Vault),
		squads.ReadOnly(squadsProgram)}
}

// ArmReport data: discriminator, operation, then the Voltr capital tail: its
// amount and the additional_args option carrying the report.
const (
	armAmountOffset        = 8 + 1
	armReportArgsOffset    = armAmountOffset + 8
	reportTicketArmWireLen = armReportArgsOffset + bridgeReportArgsPrefixLen + bridgeReportLen
)

// armReportAllowed admits ArmReport of operation over the allowed accounts,
// with data predicates after its discriminator and operation. The Squads
// program slot is free: the adaptor checks it.
func armReportAllowed(a armReport[squads.Slot], operation byte, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	adaptor := solanaKey(bridgeAdaptorProgram)
	leading := append(append([]byte(nil), armReportDiscriminator...), operation)
	return squads.Allow(adaptor, append([]squads.DataConstraintView{squads.DataBytes(0, leading)}, data...), squads.Slots(adaptor, armReportSlots(a, squads.Any)))
}

// Voltr outer data is:
// discriminator8 | amount8 | Some(discriminator)1+u32+8 |
// Some(additional_args)1+u32+ReportV1. The ticket binds exactly the amount
// and the additional_args that Voltr later forwards after selecting the
// adaptor discriminator.
func exactVoltrCapitalTail(data []byte) ([]byte, error) {
	const (
		call         = voltr.StrategyAdaptorCallOffset
		args         = call + 1 + 4 + 8
		reportOffset = args + bridgeReportArgsPrefixLen
	)
	if len(data) != reportOffset+bridgeReportLen || data[call] != 1 || binary.LittleEndian.Uint32(data[call+1:call+5]) != 8 ||
		(!bytes.Equal(data[call+5:args], adaptorDepositDiscriminator) && !bytes.Equal(data[call+5:args], adaptorWithdrawDiscriminator)) ||
		data[args] != 1 || binary.LittleEndian.Uint32(data[args+1:reportOffset]) != bridgeReportLen || data[reportOffset] != bridgeReportVersion {
		return nil, fmt.Errorf("Voltr capital envelope cannot be bound to report ticket")
	}
	tail := append([]byte(nil), data[voltr.StrategyAmountOffset:call]...)
	return append(tail, data[args:]...), nil
}
