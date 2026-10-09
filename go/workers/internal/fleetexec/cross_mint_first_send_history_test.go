package fleetexec

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
)

func TestCrossMintFirstSendRequiresActualCustodyAnchorWithoutInventingDestinationHistory(t *testing.T) {
	owner := sdk.MustPublicKeyFromBase58(mustSignedFixture(t).FeePayer)
	reader := fixtureAccounts{slot: 1005, accounts: map[string]chain.Account{}}
	makeToken := func(mint string, amount uint64) *CrossMintTokenAmount {
		address, err := associatedCustodyAccount(owner.String(), mint, sdk.TokenProgramID.String())
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 165)
		key := sdk.MustPublicKeyFromBase58(mint)
		copy(data[:32], key[:])
		copy(data[32:64], owner[:])
		binary.LittleEndian.PutUint64(data[64:72], amount)
		data[108] = 1
		reader.accounts[address] = fixtureAccount(address, sdk.TokenProgramID.String(), 1, data)
		return &CrossMintTokenAmount{Mint: mint, TokenAccount: address, AmountRaw: int64(amount)}
	}
	pre := CrossMintBalanceAnchors{Debit: makeToken(fleet.USDCMint, 1000), Credit: makeToken(fleet.USDTMint, 0)}
	movement := CrossMintMovement{VaultPubkey: owner.String(), Phase: CrossMintSourceIdle, CustodyAccount: pre.Debit.TokenAccount, CustodyMint: pre.Debit.Mint, CustodyReconciledSlot: crossRuntimeInt(1000)}
	history := addressHistory{pages: map[string][]chain.Signed{pre.Debit.TokenAccount: {signed("withdrawal", 1000)}}}
	known := recognizedSignatures("withdrawal")
	observed, slot, err := observeCrossMintFirstSendBank(context.Background(), reader, history, movement, pre, 1005, 1000, known)
	if err != nil || slot != 1005 || !sameCrossMintAnchorAmounts(observed, pre) {
		t.Fatalf("untouched destination required fabricated history: %v %d %v", observed, slot, err)
	}
	if _, _, err := observeCrossMintBank(context.Background(), reader, history, movement, pre, 1005, 1000, known, true); err == nil {
		t.Fatal("strict receipt/expiry all-account history policy weakened")
	}
	for _, change := range []string{"missing custody anchor", "destination external activity", "source external activity", "wrong custody identity"} {
		t.Run(change, func(t *testing.T) {
			badMovement := movement
			badHistory := addressHistory{pages: map[string][]chain.Signed{pre.Debit.TokenAccount: append([]chain.Signed{}, history.pages[pre.Debit.TokenAccount]...)}}
			switch change {
			case "missing custody anchor":
				badHistory.pages[pre.Debit.TokenAccount] = nil
			case "destination external activity":
				badHistory.pages[pre.Credit.TokenAccount] = []chain.Signed{signed("external", 1002)}
			case "source external activity":
				badHistory.pages[pre.Debit.TokenAccount] = append([]chain.Signed{signed("external", 1002)}, badHistory.pages[pre.Debit.TokenAccount]...)
			case "wrong custody identity":
				badMovement.CustodyAccount = owner.String()
			}
			if _, _, err := observeCrossMintFirstSendBank(context.Background(), reader, badHistory, badMovement, pre, 1005, 1000, known); err == nil {
				t.Fatal("changed source/destination history admitted")
			}
		})
	}
}
