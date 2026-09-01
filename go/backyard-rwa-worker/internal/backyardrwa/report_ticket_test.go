package backyardrwa

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestReportTicketV1ABIIsPinned(t *testing.T) {
	if reportTicketPDA != "C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5" ||
		reportTicketStateLength != 96 || reportTicketVersion != 1 || reportTicketBump != 254 ||
		!bytes.Equal(reportTicketStateDiscriminator, []byte{0xf5, 0x68, 0xb6, 0xc5, 0x3a, 0xe7, 0x74, 0xed}) ||
		!bytes.Equal(armReportDiscriminator, []byte{0xa4, 0xaf, 0xf6, 0x29, 0xb2, 0x8c, 0x23, 0x03}) {
		t.Fatal("report-ticket v1 identity or state layout drifted")
	}
}

func exactReportTicketAccount(t *testing.T, lastConsumed uint64) ConfirmedAccount {
	t.Helper()
	data := make([]byte, reportTicketStateLength)
	copy(data[:8], reportTicketStateDiscriminator)
	data[8] = reportTicketVersion
	data[9] = reportTicketBump
	config, err := decodeBase58PublicKey(bridgeStrategy)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[16:48], config[:])
	binary.LittleEndian.PutUint64(data[48:56], lastConsumed)
	return ConfirmedAccount{Address: reportTicketPDA, Owner: bridgeAdaptorProgram, Lamports: 1, Data: data}
}

func TestReportTicketObservationRejectsArmedAndLayoutDrift(t *testing.T) {
	account := exactReportTicketAccount(t, 100)
	ticket, err := decodeObservedReportTicket(account)
	if err != nil || ticket.Armed || ticket.LastConsumedSequence != 100 {
		t.Fatalf("ticket=%+v err=%v", ticket, err)
	}
	account.Data[10] = 1
	binary.LittleEndian.PutUint64(account.Data[56:64], 101)
	account.Data[64] = 1
	ticket, err = decodeObservedReportTicket(account)
	if err != nil || !ticket.Armed {
		t.Fatalf("coherent armed ticket was not decoded: ticket=%+v err=%v", ticket, err)
	}
	account.Data[11] = 1
	if _, err := decodeObservedReportTicket(account); err == nil {
		t.Fatal("ticket with nonzero reserved byte was accepted")
	}
}

func TestCapitalAndNAVBuildAtomicArmThenVoltrPayload(t *testing.T) {
	tests := []struct {
		action    Action
		amount    uint64
		operation byte
		policy    string
	}{
		{VoltrAllocateToSquads, 1_000_000, reportTicketDeposit, bridgeAllocationPolicy},
		{ReportNAV, 0, reportTicketDeposit, bridgeNAVPolicy},
		{VoltrRestoreIdle, 1_000_000, reportTicketWithdraw, bridgeWithdrawPolicy},
	}
	for _, test := range tests {
		t.Run(string(test.action), func(t *testing.T) {
			instructions, policy, constraints, err := ticketedBridgeInstructions(bridgeTestRequest(test.action, test.amount))
			if err != nil {
				t.Fatal(err)
			}
			if policy != mustKey(test.policy) || len(instructions) != 2 || !bytes.Equal(constraints, []byte{0, 1}) {
				t.Fatalf("atomic policy topology drifted: policy=%v instructions=%d constraints=%v", policy, len(instructions), constraints)
			}
			arm, capital := instructions[0], instructions[1]
			if arm.program != mustKey(bridgeAdaptorProgram) || len(arm.data) != reportTicketArmWireLen ||
				!bytes.Equal(arm.data[:8], armReportDiscriminator) || arm.data[8] != test.operation {
				t.Fatalf("ArmReport wire drifted: %x", arm.data)
			}
			tail, err := exactVoltrCapitalTail(capital.data)
			if err != nil || !bytes.Equal(arm.data[9:], tail) {
				t.Fatalf("ticket is not bound to exact consumed Voltr tail: %v", err)
			}
			wantArmAccounts := []struct {
				key      string
				signer   bool
				writable bool
			}{
				{bridgeStrategy, false, false}, {reportTicketPDA, false, true},
				{bridgeSettings, false, false}, {bridgeVault, true, false},
				{bridgeSquadsProgram, false, false},
			}
			if len(arm.accounts) != len(wantArmAccounts) {
				t.Fatalf("ArmReport account count=%d", len(arm.accounts))
			}
			for index, want := range wantArmAccounts {
				got := arm.accounts[index]
				if got.key != mustKey(want.key) || got.signer != want.signer || got.writable != want.writable {
					t.Fatalf("ArmReport account %d drifted: %+v", index, got)
				}
			}
			if len(capital.accounts) != 18 || capital.accounts[17].key != mustKey(reportTicketPDA) ||
				capital.accounts[17].signer || !capital.accounts[17].writable {
				t.Fatal("Voltr ticket account is not exact writable outer index 17")
			}
		})
	}
}

func TestStageRemainsSingleInstructionWithoutTicket(t *testing.T) {
	instructions, policy, constraints, err := ticketedBridgeInstructions(bridgeTestRequest(StageSquadsToVoltr, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if policy != mustKey(bridgeStagePolicy) || len(instructions) != 1 || !bytes.Equal(constraints, []byte{0}) ||
		instructions[0].program != mustKey(bridgeTokenProgram) {
		t.Fatal("SPL-only staging topology drifted")
	}
	for _, account := range instructions[0].accounts {
		if account.key == mustKey(reportTicketPDA) {
			t.Fatal("SPL-only staging unexpectedly included report ticket")
		}
	}
}
