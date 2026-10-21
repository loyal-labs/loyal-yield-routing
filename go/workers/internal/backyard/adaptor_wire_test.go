package backyard

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

type adaptorWireFixture struct {
	ReportV1 struct {
		Sequence       uint64 `json:"sequence"`
		ObservedSlot   uint64 `json:"observed_slot"`
		NAVAfterRaw    uint64 `json:"nav_after_raw"`
		SnapshotDigest string `json:"snapshot_digest"`
		Wire           string `json:"wire"`
	} `json:"report_v1"`
	VoltrDeposit struct {
		Amount uint64 `json:"amount"`
		Data   string `json:"data"`
	} `json:"voltr_deposit"`
	ArmReport struct {
		Data     string `json:"data"`
		Accounts []struct {
			Name     string `json:"name"`
			Signer   bool   `json:"signer"`
			Writable bool   `json:"writable"`
		} `json:"accounts"`
	} `json:"arm_report"`
	Discriminators map[string]string `json:"discriminators"`
	Errors         map[string]uint32 `json:"errors"`
	ReportTicket   struct {
		Seed         string `json:"seed"`
		Program      string `json:"program"`
		Strategy     string `json:"strategy"`
		PDA          string `json:"pda"`
		Bump         byte   `json:"bump"`
		StateLength  int    `json:"state_length"`
		StateVersion byte   `json:"state_version"`
	} `json:"report_ticket"`
}

func unhex(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdaptorWireMatchesRustFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../crates/loyal-voltr-rwa-nav-adaptor/fixtures/wire-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f adaptorWireFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}

	report := BridgeReport{Sequence: f.ReportV1.Sequence, ObservedSlot: f.ReportV1.ObservedSlot, NAVAfterRaw: f.ReportV1.NAVAfterRaw, SnapshotDigest: navDigest(f.ReportV1.SnapshotDigest)}
	wire, err := encodeBridgeReport(report)
	if err != nil || !bytes.Equal(wire, unhex(t, f.ReportV1.Wire)) {
		t.Fatalf("report wire drifted: %x err=%v", wire, err)
	}

	request := bridgeTestRequest(VoltrAllocateToSquads, f.VoltrDeposit.Amount)
	request.Report = report
	envelope, err := bridgeInstruction(rwa, request)
	if err != nil || !bytes.Equal(envelope.data, unhex(t, f.VoltrDeposit.Data)) {
		t.Fatalf("Voltr deposit envelope drifted: %x err=%v", envelope.data, err)
	}

	arm, err := armReportInstruction(rwa, VoltrAllocateToSquads, envelope.data)
	if err != nil || !bytes.Equal(arm.data, unhex(t, f.ArmReport.Data)) {
		t.Fatalf("ArmReport data drifted: %x err=%v", arm.data, err)
	}
	keys := map[string]string{"config": rwa.Strategy, "ticket": reportTicketPDA, "squads_settings": backyardSettings, "squads_vault": rwa.squads, "squads_program": squads.ProgramID.String()}
	if len(arm.accounts) != len(f.ArmReport.Accounts) {
		t.Fatalf("ArmReport has %d accounts", len(arm.accounts))
	}
	for i, want := range f.ArmReport.Accounts {
		got := arm.accounts[i]
		if solana.PublicKey(got.key).String() != keys[want.Name] || got.signer != want.Signer || got.writable != want.Writable {
			t.Fatalf("ArmReport account %d is not %s", i, want.Name)
		}
	}

	for name, got := range map[string][]byte{"adaptor_deposit": adaptorDepositDiscriminator, "adaptor_withdraw": adaptorWithdrawDiscriminator,
		"arm_report": armReportDiscriminator, "report_ticket_state": reportTicketStateDiscriminator} {
		if !bytes.Equal(got, unhex(t, f.Discriminators[name])) {
			t.Fatalf("%s discriminator drifted", name)
		}
	}
	if f.Errors["ReportSlot"] != adaptorErrorReportSlot || f.Errors["TicketReplay"] != adaptorErrorTicketReplay {
		t.Fatalf("adaptor error numbers drifted: %v", f.Errors)
	}

	ticket := f.ReportTicket
	pda, bump, err := solana.FindProgramAddress([][]byte{[]byte(ticket.Seed), solanaKey(ticket.Strategy).Bytes()}, solanaKey(ticket.Program))
	if err != nil || pda.String() != ticket.PDA || bump != ticket.Bump {
		t.Fatalf("report ticket PDA %s bump %d err=%v", pda, bump, err)
	}
	if ticket.Program != rwa.Adaptor || ticket.Strategy != rwa.Strategy || ticket.PDA != reportTicketPDA || ticket.Bump != reportTicketBump ||
		ticket.StateLength != reportTicketStateLength || ticket.StateVersion != reportTicketVersion {
		t.Fatal("report ticket identity or state layout drifted")
	}
}
