package fleetexec

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func TestCrossMintRuntimeReporterKeepsUnknownFrontierClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &CrossMintRuntime{}
	calls := 0
	r.SetRuntimeReporter(func(ready bool, slot uint64) {
		calls++
		if ready || slot != 0 {
			t.Fatalf("cancelled tick fabricated frontier: %v %d", ready, slot)
		}
	})
	if n, err := r.Tick(ctx); n != 0 || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("reporter cancellation: n=%d calls=%d err=%v", n, calls, err)
	}
}

func TestCrossMintReceiptRequiresActualPrePostOwnerAndCanonicalProgram(t *testing.T) {
	f := mustSignedFixture(t)
	for _, mint := range []string{fleet.USDCMint, "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"} {
		program, err := canonicalCustodyTokenProgram(mint)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"valid", "owner_missing", "owner_changed", "program_missing", "wrong_program", "unknown_pre", "duplicate_account", "wrong_direction", "mint_changed"} {
			t.Run(mint+"/"+change, func(t *testing.T) {
				before, after := uint64(1000), uint64(5)
				d := TokenDelta{Account: f.SecondaryAccount, Mint: mint, PreRaw: &before, PostRaw: &after, PreOwner: f.FeePayer, PostOwner: f.FeePayer, PreProgram: program, PostProgram: program}
				switch change {
				case "owner_missing":
					d.PreOwner = ""
				case "owner_changed":
					d.PostOwner = f.SecondaryAccount
				case "program_missing":
					d.PostProgram = ""
				case "wrong_program":
					d.PreProgram = sdk.SystemProgramID.String()
				case "unknown_pre":
					d.PreRaw = nil
				case "wrong_direction":
					after = 1001
				case "mint_changed":
					d.Mint = fleet.USDTMint
				}
				receipt := &TransactionReceipt{TokenDeltas: []TokenDelta{d}}
				if change == "duplicate_account" {
					receipt.TokenDeltas = append(receipt.TokenDeltas, d)
				}
				pre := CrossMintBalanceAnchors{Debit: &CrossMintTokenAmount{Mint: mint, TokenAccount: f.SecondaryAccount, AmountRaw: 1000}}
				effect, post, err := crossMintReceiptEffects(receipt, CrossMintMovement{VaultPubkey: f.FeePayer}, pre)
				if (err == nil) != (change == "valid") {
					t.Fatalf("change=%s error=%v", change, err)
				}
				if err == nil && (effect.Debit.AmountRaw != 995 || post.Debit.AmountRaw != 5) {
					t.Fatal("receipt aggregate used as attributable effect")
				}
			})
		}
	}
}

type addressHistory struct {
	pages map[string][]finalizedAddressSignature
}

func (h addressHistory) FinalizedAddressSignatures(_ context.Context, address, before string, _ int64) ([]finalizedAddressSignature, error) {
	return h.pages[address], nil
}

func TestCrossMintPostBankUsesOneSlotAndSourceMinimumDepositDecoder(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	reader := postFixture(t, c, 1005, 1, 51)
	reserve := reader.accounts[c.source]
	market, obligation, _, program, err := reservePostIdentity(reserve, c.mint, c.vault)
	if err != nil {
		t.Fatal(err)
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, program)
	if err != nil {
		t.Fatal(err)
	}
	pre := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: c.mint, TokenAccount: ata, AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: c.source, Market: market, Obligation: obligation, ObligationExists: true, CollateralRaw: 1001}}
	m := CrossMintMovement{VaultPubkey: c.vault, Phase: CrossMintSourceReserve, SourceMint: c.mint, CustodyMint: c.mint}
	history := addressHistory{pages: map[string][]finalizedAddressSignature{ata: {{Signature: "receipt", Slot: 1000, ConfirmationStatus: "finalized"}}, obligation: {{Signature: "receipt", Slot: 1000, ConfirmationStatus: "finalized"}}}}
	post, slot, err := observeCrossMintBank(context.Background(), reader, history, m, pre, 1000, 1000, map[string]bool{"receipt": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 1005 || post.Credit.AmountRaw != 51 || post.Position.CollateralRaw != 1 || post.Position.MinimumDepositAmountRaw == nil || *post.Position.MinimumDepositAmountRaw != 2 {
		t.Fatalf("incoherent bank/conversion: slot=%d %+v", slot, post)
	}
	// A later policy/ALT floor does not move the historical attribution anchor.
	if _, _, err = observeCrossMintBank(context.Background(), reader, history, m, pre, 1005, 1000, map[string]bool{"receipt": true}, true); err != nil {
		t.Fatalf("source anchor shifted to current policy slot: %v", err)
	}
	for _, change := range []string{"stale_bank", "external_restoration", "missing_anchor", "noncanonical_ata", "wrong_obligation", "negative_fee_value"} {
		t.Run(change, func(t *testing.T) {
			fresh := postFixture(t, c, 1005, 1, 51)
			anchors := pre
			pages := addressHistory{pages: map[string][]finalizedAddressSignature{ata: {{Signature: "receipt", Slot: 1000, ConfirmationStatus: "finalized"}}, obligation: {{Signature: "receipt", Slot: 1000, ConfirmationStatus: "finalized"}}}}
			if change == "stale_bank" {
				fresh.slot = 999
			}
			if change == "external_restoration" {
				pages.pages[ata] = append([]finalizedAddressSignature{{Signature: "external", Slot: 1002, ConfirmationStatus: "finalized"}}, pages.pages[ata]...)
			}
			if change == "missing_anchor" {
				pages.pages[ata] = nil
			}
			if change == "noncanonical_ata" {
				a := fresh.accounts[ata]
				a.Address = f.SecondaryAccount
				fresh.accounts[f.SecondaryAccount] = a
				copy := *pre.Credit
				copy.TokenAccount = f.SecondaryAccount
				anchors.Credit = &copy
			}
			if change == "wrong_obligation" {
				a := fresh.accounts[obligation]
				a.Data[64] ^= 1
				fresh.accounts[obligation] = a
			}
			if change == "negative_fee_value" {
				a := fresh.accounts[c.source]
				binary.LittleEndian.PutUint64(a.Data[344:352], ^uint64(0))
				binary.LittleEndian.PutUint64(a.Data[352:360], ^uint64(0))
				fresh.accounts[c.source] = a
			}
			if _, _, err = observeCrossMintBank(context.Background(), fresh, pages, m, anchors, 1000, 1000, map[string]bool{"receipt": true}, true); err == nil {
				t.Fatalf("changed %s accepted", change)
			}
		})
	}
}

func TestCrossMintRPCMetadataKeepsMissingValuesUnknown(t *testing.T) {
	pre := rpcTokenBalance{AccountIndex: 0, Mint: fleet.USDCMint, Owner: "owner", ProgramID: sdk.TokenProgramID.String()}
	pre.UITokenAmount.Amount = "1000"
	post := pre
	post.UITokenAmount.Amount = "1"
	post.Owner = ""
	post.ProgramID = ""
	deltas, err := receiptDeltas([]string{"account"}, []rpcTokenBalance{pre}, []rpcTokenBalance{post})
	if err != nil || len(deltas) != 1 || deltas[0].PreOwner != "owner" || deltas[0].PostOwner != "" || deltas[0].PostProgram != "" {
		t.Fatalf("unknown metadata fabricated: %+v %v", deltas, err)
	}
}
