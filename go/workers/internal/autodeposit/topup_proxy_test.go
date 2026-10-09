package autodeposit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

func TestTopUpPreflightUsesOfficialBuilderAndActualPolicy(t *testing.T) {
	var err error
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	plan, _ := testPullPlan()
	ata, err := spl.AssociatedTokenAddress(mustKey(plan.Target.VaultPubkey), mustKey(USDCMint), solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Target.VaultUsdcAta = ata.String()
	plan.Target.VaultTokenAta = plan.Target.VaultUsdcAta
	reserve, route := testReserve(t, plan, "")
	obligation := route.Obligation
	put := func(raw []byte, offset int, value string) { k := mustKey(value); copy(raw[offset:offset+32], k[:]) }
	obligationData := make([]byte, kamino.ObligationSize)
	copy(obligationData, kamino.ObligationDiscriminator[:])
	binary.LittleEndian.PutUint64(obligationData[16:24], 100)
	put(obligationData, 32, plan.Market)
	put(obligationData, 64, plan.Target.VaultPubkey)
	custody := make([]byte, 165)
	put(custody, 0, USDCMint)
	put(custody, 32, plan.Target.VaultPubkey)
	custody[108] = 1
	official, err := fleet.BuildIdleDeposit(fleet.KaminoIdleDepositRequest{Vault: plan.Target.VaultPubkey, Target: route.Position, DepositLiquidityAmount: uint64(plan.AmountRaw)})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, solana.PrivateKey(key).PublicKey().String(), 1, official.Protected)
	if err != nil {
		t.Fatal(err)
	}
	byAddress := map[string]testAccount{
		plan.Reserve:                   reserve,
		plan.Market:                    {Address: plan.Market, Owner: kamino.ProgramID.String(), Data: make([]byte, 8)},
		obligation:                     {Address: obligation, Owner: kamino.ProgramID.String(), Lamports: 1, Data: obligationData},
		plan.Target.VaultUsdcAta:       {Address: plan.Target.VaultUsdcAta, Owner: splTokenID, Data: custody},
		plan.Target.RoutePolicyAccount: {Address: plan.Target.RoutePolicyAccount, Owner: squads.ProgramID.String(), Data: policy},
	}
	builder, err := NewSweepWireBuilder(key, fixtureReader(100, byAddress))
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := builder.ConfirmTopUpRoute(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	built, err := builder.BuildTopUp(t.Context(), TopUpWireRequest{Plan: plan, RecentBlockhash: fixedKey("blockhash"), LastValidBlockHeight: 900})
	if err != nil {
		t.Fatal(err)
	}
	attempt := DurableAttempt{AmountRaw: plan.AmountRaw, Signature: built.Signature, SignedTransactionBase64: built.SignedTransactionBase64, SignedTransactionSHA256: built.SignedTransactionSHA256, RecentBlockhash: built.RecentBlockhash, LastValidBlockHeight: built.LastValidBlockHeight}
	if err := builder.ProveTopUpWire(plan, attempt, confirmed); err != nil {
		t.Fatal(err)
	}
	// An actual policy that permits another amount is not executable even when
	// the reserve, obligation and token accounts otherwise remain valid.
	foreign := official.Protected[0]
	foreign.Data = append([]byte(nil), foreign.Data...)
	binary.LittleEndian.PutUint64(foreign.Data[8:], uint64(plan.AmountRaw+1))
	badPolicy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, solana.PrivateKey(key).PublicKey().String(), 1, []fleet.RouteInstruction{foreign})
	if err != nil {
		t.Fatal(err)
	}
	bad := byAddress[plan.Target.RoutePolicyAccount]
	bad.Data = badPolicy
	byAddress[bad.Address] = bad
	if _, err := builder.ConfirmTopUpRoute(t.Context(), plan); err == nil {
		t.Fatal("unexecutable deposit policy accepted before wallet pull")
	}
}
