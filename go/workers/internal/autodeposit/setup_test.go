package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
)

func setupFixture(t *testing.T, stage SetupStage) (*SweepWireBuilder, DepositPlan, DestinationSetupPlan, map[string]testAccount) {
	t.Helper()
	plan, _ := testPullPlan()
	vault := mustKey(plan.Target.VaultPubkey)
	ataKey, err := spl.AssociatedTokenAddress(vault, mustKey(USDCMint), solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	ata := ataKey.String()
	plan.Target.VaultUsdcAta = ata
	plan.Target.VaultTokenAta = ata
	metadata, err := metadataKey(plan.Target.VaultPubkey)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := vanillaObligationKey(vault, mustKey(plan.Market))
	if err != nil {
		t.Fatal(err)
	}
	put := func(data []byte, offset int, value string) {
		key := mustKey(value)
		copy(data[offset:offset+32], key[:])
	}
	reserve := make([]byte, reserveDataLength)
	disc, _ := hex.DecodeString(reserveDiscriminator)
	copy(reserve, disc)
	for offset, value := range map[int]string{reserveMarketOffset: plan.Market, reserveLiquidityMintOffset: USDCMint, reserveLiquidityProgramOffset: splTokenID, reserveCollateralMintOffset: fixedKey("setup-cmint"), reserveCollateralSupplyOffset: fixedKey("setup-csupply"), reserveLiquiditySupplyOffset: fixedKey("setup-lsupply")} {
		put(reserve, offset, value)
	}
	if stage == SetupFarm {
		put(reserve, reserveFarmOffset, fixedKey("setup-farm"))
	}
	route := decodeReservePosition(plan.Reserve, reserve)
	route.Obligation = obligation
	route.Position.Obligation = obligation
	route.Position.VaultLiquidityATA = ata
	if route.Position.ReserveFarmState != "" {
		farm := mustKey(route.Position.ReserveFarmState)
		obl := mustKey(obligation)
		pda, err := findProgramAddress([][]byte{[]byte("user"), farm[:], obl[:]}, farmsProgramID)
		if err != nil {
			t.Fatal(err)
		}
		route.Position.ObligationFarmUserState = base58Key(pda[:])
	}
	custodyData := make([]byte, 165)
	put(custodyData, 0, USDCMint)
	put(custodyData, 32, plan.Target.VaultPubkey)
	custodyData[108] = 1
	metadataData := make([]byte, 1032)
	copy(metadataData, accountDiscriminator("UserMetadata"))
	put(metadataData, 80, plan.Target.VaultPubkey)
	obligationData := make([]byte, obligationDataLength)
	copy(obligationData, obligationDiscriminator[:])
	put(obligationData, 32, plan.Market)
	put(obligationData, 64, plan.Target.VaultPubkey)
	accounts := map[string]testAccount{
		plan.Reserve:            {Address: plan.Reserve, Owner: KLendProgramID, Data: reserve},
		plan.Market:             {Address: plan.Market, Owner: KLendProgramID},
		plan.Target.VaultPubkey: {Address: plan.Target.VaultPubkey, Owner: systemProgramZero, Lamports: 10_000_000},
		ata:                     {Address: ata, Owner: splTokenID, Data: custodyData},
		metadata:                {Address: metadata, Owner: KLendProgramID, Data: metadataData},
		obligation:              {Address: obligation, Owner: KLendProgramID, Data: obligationData},
	}
	address := map[SetupStage]string{SetupATA: ata, SetupMetadata: metadata, SetupObligation: obligation, SetupFarm: route.Position.ObligationFarmUserState}[stage]
	delete(accounts, address)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	delegate := solana.PrivateKey(key).PublicKey().String()
	request := fleet.DestinationSetupRequest{Stage: string(stage), Vault: plan.Target.VaultPubkey, Payer: delegate, Target: route.Position}
	built, err := fleet.BuildDestinationSetup(request)
	if err != nil {
		t.Fatal(err)
	}
	setup := DestinationSetupPlan{Stage: stage, Account: address, Route: route, ObservedSlot: 500}
	if len(built.Protected) > 0 {
		policy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, delegate, 1, built.Protected)
		if err != nil {
			t.Fatal(err)
		}
		accounts[plan.Target.RoutePolicyAccount] = testAccount{Owner: squads.ProgramID.String(), Data: policy}
		setup.PolicyAccount = plan.Target.RoutePolicyAccount
		setup.RentTopUpLamports = 10_000_000
	}
	builder, err := NewSweepWireBuilderWithSetup(key, fixtureReader(500, accounts), func(context.Context, int) (uint64, error) { return 20_000_000, nil })
	if err != nil {
		t.Fatal(err)
	}
	return builder, plan, setup, accounts
}

func TestDestinationSetupUsesOfficialBuilderBeforePull(t *testing.T) {
	for _, stage := range []SetupStage{SetupATA, SetupMetadata, SetupObligation, SetupFarm} {
		t.Run(string(stage), func(t *testing.T) {
			builder, plan, expected, _ := setupFixture(t, stage)
			setup, err := builder.InspectDestinationSetup(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if setup == nil || setup.Stage != stage || setup.Account != expected.Account {
				t.Fatalf("setup %v want %v", setup, expected)
			}
			wire, err := builder.BuildDestinationSetup(t.Context(), plan, *setup, fixedKey("setup-blockhash"), 900)
			if err != nil {
				t.Fatal(err)
			}
			if wire.Signature == "" || wire.LastValidBlockHeight != 900 {
				t.Fatalf("setup wire %+v", wire)
			}
			if err = builder.ReadbackDestinationSetup(t.Context(), plan, *setup, 500); err == nil {
				t.Fatal("missing setup account was confirmed")
			}
		})
	}
}

// Fleet same-mint withdrawals leave accrued interest in the vault's custody,
// so production custody is rarely zero. The TS executor creates the account
// only when it is missing and leaves the idle tolerance to its pre-pull check.
func TestDestinationSetupAcceptsCustodyResidue(t *testing.T) {
	builder, plan, expected, accounts := setupFixture(t, SetupObligation)
	custody := accounts[plan.Target.VaultUsdcAta]
	custody.Data = append([]byte(nil), custody.Data...)
	binary.LittleEndian.PutUint64(custody.Data[64:72], 1488)
	accounts[plan.Target.VaultUsdcAta] = custody
	setup, err := builder.InspectDestinationSetup(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if setup == nil || setup.Stage != SetupObligation || setup.Account != expected.Account {
		t.Fatalf("setup %v want the missing obligation", setup)
	}
}

func TestSetupReadbackRejectsForeignIdentity(t *testing.T) {
	builder, plan, setup, accounts := setupFixture(t, SetupMetadata)
	data := make([]byte, 1032)
	copy(data, accountDiscriminator("UserMetadata"))
	foreign := mustKey(fixedKey("foreign"))
	copy(data[80:112], foreign[:])
	accounts[setup.Account] = testAccount{Owner: KLendProgramID, Data: data}
	if err := builder.ReadbackDestinationSetup(t.Context(), plan, setup, 500); err == nil {
		t.Fatal("foreign metadata owner passed readback")
	}
	owner := mustKey(plan.Target.VaultPubkey)
	copy(data[80:112], owner[:])
	if err := builder.ReadbackDestinationSetup(t.Context(), plan, setup, 500); err != nil {
		t.Fatal(err)
	}
	if err := builder.ReadbackDestinationSetup(t.Context(), plan, setup, 501); err == nil {
		t.Fatal("readback older than confirmed transaction passed")
	}
}
