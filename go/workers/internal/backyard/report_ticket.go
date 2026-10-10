package backyard

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
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
	reportTicketArmWireLen  = 79
	voltrCapitalTailLen     = 70
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

func ticketedBridgeInstructions(request BridgeBuildRequest) ([]compiledInstruction, publicKey, []byte, error) {
	capital, policy, constraintIndex, err := bridgeInstruction(request)
	if err != nil {
		return nil, publicKey{}, nil, err
	}
	if request.Action == StageSquadsToVoltr {
		return []compiledInstruction{capital}, policy, []byte{constraintIndex}, nil
	}
	arm, err := armReportInstruction(request.Action, capital.data)
	if err != nil {
		return nil, publicKey{}, nil, err
	}
	if len(capital.accounts) != 17 {
		return nil, publicKey{}, nil, fmt.Errorf("Voltr capital account layout drifted before ticket append")
	}
	capital.accounts = append(capital.accounts, meta(reportTicketPDA, false, true))
	return []compiledInstruction{arm, capital}, policy, []byte{0, 1}, nil
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
	if len(data) != reportTicketArmWireLen {
		return compiledInstruction{}, fmt.Errorf("ArmReport wire length drifted")
	}
	var accounts []accountMeta
	for _, s := range armReportSlots(armReport[string]{Strategy: bridgeStrategy, Ticket: reportTicketPDA, Settings: bridgeSettings, Vault: bridgeVault},
		squads.ProgramID.String()) {
		accounts = append(accounts, meta(s.key, s.signer, s.writable))
	}
	return compiledInstruction{program: mustKey(bridgeAdaptorProgram), accounts: accounts, data: data}, nil
}

// armReport is the adaptor's ArmReport account set: the strategy-two adaptor
// config, the report ticket, the Squads settings and the vault that signs;
// the Squads program follows them. The builder and armReportAllowed read the
// same slot list.
type armReport[T any] struct {
	Strategy, Ticket, Settings, Vault T
}

type armSlot[T any] struct {
	key              T
	signer, writable bool
}

func armReportSlots[T any](a armReport[T], squadsProgram T) []armSlot[T] {
	return []armSlot[T]{{key: a.Strategy}, {key: a.Ticket, writable: true}, {key: a.Settings}, {key: a.Vault, signer: true}, {key: squadsProgram}}
}

// ArmReport data: discriminator, operation, then the 70-byte Voltr capital
// tail, its amount and the additional_args option carrying the report.
const (
	armAmountOffset     = 9
	armReportArgsOffset = 17
)

// armReportAllowed admits ArmReport of operation over the allowed accounts,
// with data predicates after its discriminator and operation. The Squads
// program slot is free: the adaptor checks it.
func armReportAllowed(a armReport[squads.Slot], operation byte, data ...squads.DataConstraintView) squads.InstructionConstraintView {
	var slots []squads.Slot
	for _, s := range armReportSlots(a, squads.Unpinned(squads.ProgramID)) {
		slots = append(slots, s.key)
	}
	leading := append(append([]byte(nil), armReportDiscriminator...), operation)
	return squads.AllowData(solanaKey(bridgeAdaptorProgram), append([]squads.DataConstraintView{squads.DataBytes(0, leading)}, data...), slots)
}

// Voltr outer data is:
// discriminator8 | amount8 | Some(discriminator)1+u32+8 |
// Some(additional_args)1+u32+ReportV1[57]. The ticket binds exactly the 70
// bytes that Voltr later forwards after selecting the adaptor discriminator.
func exactVoltrCapitalTail(data []byte) ([]byte, error) {
	const reportOffset = 34
	if len(data) != 91 || data[16] != 1 || binary.LittleEndian.Uint32(data[17:21]) != 8 ||
		(!bytes.Equal(data[21:29], adaptorDepositDiscriminator) && !bytes.Equal(data[21:29], adaptorWithdrawDiscriminator)) ||
		data[29] != 1 || binary.LittleEndian.Uint32(data[30:reportOffset]) != 57 || data[reportOffset] != 1 {
		return nil, fmt.Errorf("Voltr capital envelope cannot be bound to report ticket")
	}
	tail := append([]byte(nil), data[8:16]...)
	tail = append(tail, data[29:]...)
	if len(tail) != voltrCapitalTailLen {
		return nil, fmt.Errorf("Voltr capital tail length drifted")
	}
	return tail, nil
}
