package fleetexec

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestCrossMintReceiptRequiresActualPrePostOwnerAndCanonicalProgram(t *testing.T) {
	f := mustSignedFixture(t)
	for _, mint := range []string{fleet.USDCMint, "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"} {
		program, err := canonicalCustodyTokenProgram(mint)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"valid", "owner_missing", "owner_changed", "program_missing", "wrong_program", "unknown_pre", "unknown_post", "wrong_direction", "mint_changed", "post_mint_changed"} {
			t.Run(mint+"/"+change, func(t *testing.T) {
				account, vault := sdk.MustPublicKeyFromBase58(f.SecondaryAccount), sdk.MustPublicKeyFromBase58(f.FeePayer)
				tokenMint, tokenProgram := sdk.MustPublicKeyFromBase58(mint), sdk.MustPublicKeyFromBase58(program)
				was := chain.TokenBalance{Mint: tokenMint, Owner: vault, Program: tokenProgram, Amount: 1000}
				is := chain.TokenBalance{Mint: tokenMint, Owner: vault, Program: tokenProgram, Amount: 5}
				switch change {
				case "owner_missing":
					was.Owner = sdk.PublicKey{}
				case "owner_changed":
					is.Owner = account
				case "program_missing":
					is.Program = sdk.PublicKey{}
				case "wrong_program":
					was.Program = sdk.SystemProgramID
				case "wrong_direction":
					is.Amount = 1001
				case "mint_changed":
					was.Mint, is.Mint = sdk.MustPublicKeyFromBase58(fleet.USDTMint), sdk.MustPublicKeyFromBase58(fleet.USDTMint)
				case "post_mint_changed":
					is.Mint = sdk.MustPublicKeyFromBase58(fleet.USDTMint)
				}
				receipt := chain.Receipt{Pre: map[sdk.PublicKey]chain.TokenBalance{account: was}, Post: map[sdk.PublicKey]chain.TokenBalance{account: is}}
				switch change {
				case "unknown_pre":
					delete(receipt.Pre, account)
				case "unknown_post":
					delete(receipt.Post, account)
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
	pages map[string][]chain.Signed
}

func (h addressHistory) History(_ context.Context, address sdk.PublicKey, _ int, _ sdk.Signature, _ rpc.CommitmentType, _ uint64) ([]chain.Signed, error) {
	return h.pages[address.String()], nil
}

func TestCrossMintPostBankUsesOneSlotAndSourceMinimumDepositDecoder(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	reader := postFixture(t, c, 1005, 1, 51)
	reserve := reader.accounts[c.source]
	market, obligation, _, program, err := reservePostIdentity(&reserve, c.mint, c.vault)
	if err != nil {
		t.Fatal(err)
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, program)
	if err != nil {
		t.Fatal(err)
	}
	pre := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: c.mint, TokenAccount: ata, AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: c.source, Market: market, Obligation: obligation, ObligationExists: true, CollateralRaw: 1001}}
	m := CrossMintMovement{VaultPubkey: c.vault, Phase: CrossMintSourceReserve, SourceMint: c.mint, CustodyMint: c.mint}
	history := addressHistory{pages: map[string][]chain.Signed{ata: {signed("receipt", 1000)}, obligation: {signed("receipt", 1000)}}}
	post, slot, err := observeCrossMintBank(context.Background(), reader, history, m, pre, 1000, 1000, recognizedSignatures("receipt"), true)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 1005 || post.Credit.AmountRaw != 51 || post.Position.CollateralRaw != 1 || post.Position.MinimumDepositAmountRaw == nil || *post.Position.MinimumDepositAmountRaw != 2 {
		t.Fatalf("incoherent bank/conversion: slot=%d %+v", slot, post)
	}
	// A later policy/ALT floor does not move the historical attribution anchor.
	if _, _, err = observeCrossMintBank(context.Background(), reader, history, m, pre, 1005, 1000, recognizedSignatures("receipt"), true); err != nil {
		t.Fatalf("source anchor shifted to current policy slot: %v", err)
	}
	for _, change := range []string{"stale_bank", "external_restoration", "missing_anchor", "noncanonical_ata", "wrong_obligation", "negative_fee_value"} {
		t.Run(change, func(t *testing.T) {
			fresh := postFixture(t, c, 1005, 1, 51)
			anchors := pre
			pages := addressHistory{pages: map[string][]chain.Signed{ata: {signed("receipt", 1000)}, obligation: {signed("receipt", 1000)}}}
			if change == "stale_bank" {
				fresh.slot = 999
			}
			if change == "external_restoration" {
				pages.pages[ata] = append([]chain.Signed{signed("external", 1002)}, pages.pages[ata]...)
			}
			if change == "missing_anchor" {
				pages.pages[ata] = nil
			}
			if change == "noncanonical_ata" {
				a := fresh.accounts[ata]
				a.Key = sdk.MustPublicKeyFromBase58(f.SecondaryAccount)
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
			if _, _, err = observeCrossMintBank(context.Background(), fresh, pages, m, anchors, 1000, 1000, recognizedSignatures("receipt"), true); err == nil {
				t.Fatalf("changed %s accepted", change)
			}
		})
	}
}

// A withdraw leg that empties the source makes KLend close the obligation in
// the same transaction. The finalized bank must record that as a closed,
// zero-collateral position rather than fail the whole proof forever; the
// reserve and token anchors stay required.
func TestCrossMintPostBankRecordsClosedSourceObligation(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, minimumSlot: 1000, sourceKind: "reserve_position"}
	source := postFixture(t, c, 1005, 0, 1051).accounts[c.source]
	market, obligation, _, program, err := reservePostIdentity(&source, c.mint, c.vault)
	if err != nil {
		t.Fatal(err)
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, program)
	if err != nil {
		t.Fatal(err)
	}
	pre := CrossMintBalanceAnchors{Credit: &CrossMintTokenAmount{Mint: c.mint, TokenAccount: ata, AmountRaw: 50}, Position: &CrossMintPositionAnchor{Reserve: c.source, Market: market, Obligation: obligation, ObligationExists: true, CollateralRaw: 1001}}
	m := CrossMintMovement{VaultPubkey: c.vault, Phase: CrossMintSourceReserve, SourceMint: c.mint, CustodyMint: c.mint}
	history := addressHistory{pages: map[string][]chain.Signed{ata: {signed("receipt", 1000)}, obligation: {signed("receipt", 1000)}}}
	for _, missing := range []string{obligation, c.source, ata} {
		reader := postFixture(t, c, 1005, 0, 1051)
		delete(reader.accounts, obligation)
		delete(reader.accounts, missing)
		post, _, err := observeCrossMintBank(context.Background(), reader, history, m, pre, 1000, 1000, recognizedSignatures("receipt"), true)
		if missing != obligation {
			if err == nil {
				t.Fatalf("absent required account %s accepted", missing)
			}
			continue
		}
		if err != nil {
			t.Fatalf("closed source obligation blocked the finalized bank: %v", err)
		}
		if post.Position == nil || post.Position.ObligationExists || post.Position.CollateralRaw != 0 || post.Credit.AmountRaw != 1051 {
			t.Fatalf("closed obligation not recorded as zero collateral: %+v", post)
		}
	}
}
