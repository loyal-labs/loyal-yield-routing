package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"os"
	"testing"
)

func setupProxy(t *testing.T) *fleet.KLendProxy {
	t.Helper()
	path := os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	if path == "" {
		t.Skip("requires locally built official Rust KLend proxy")
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(binary)
	proxy, err := fleet.NewKLendProxy(path, hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}
func setupFixture(t *testing.T, stage SetupStage) (*SweepWireBuilder, DepositPlan, DestinationSetupPlan, map[string]backyard.ConfirmedAccount) {
	t.Helper()
	proxy := setupProxy(t)
	plan, _ := testPullPlan()
	vault := mustKey(plan.Target.VaultPubkey)
	ata, err := deriveVaultATA(vault, mustKey(USDCMint), mustKey(splTokenID))
	if err != nil {
		t.Fatal(err)
	}
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
	accounts := map[string]backyard.ConfirmedAccount{
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
	built, err := proxy.BuildDestinationSetup(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	setup := DestinationSetupPlan{Stage: stage, Account: address, Route: route, ObservedSlot: 500}
	if len(built.Protected) > 0 {
		policy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, delegate, 1, built.Protected)
		if err != nil {
			t.Fatal(err)
		}
		accounts[plan.Target.SetupPolicyAccount] = backyard.ConfirmedAccount{}
		accounts[plan.Target.RoutePolicyAccount] = backyard.ConfirmedAccount{Address: plan.Target.RoutePolicyAccount, Owner: squadsProgramID, Data: policy}
		setup.PolicyAccount = plan.Target.RoutePolicyAccount
		setup.RentTopUpLamports = 10_000_000
	}
	read := func(ctx context.Context, addresses []string, optional ...string) (int64, []backyard.ConfirmedAccount, error) {
		optionalSet := map[string]bool{}
		for _, address := range optional {
			optionalSet[address] = true
		}
		out := make([]backyard.ConfirmedAccount, 0, len(addresses))
		for _, address := range addresses {
			account, exists := accounts[address]
			if !exists {
				if !optionalSet[address] {
					return 0, nil, errors.New("required test account missing")
				}
				account = backyard.ConfirmedAccount{Address: address}
			}
			out = append(out, account)
		}
		return 500, out, nil
	}
	builder, err := NewSweepWireBuilderWithSetup(proxy, key, read, func(context.Context, int) (uint64, error) { return 20_000_000, nil })
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
func TestSetupReadbackRejectsForeignIdentity(t *testing.T) {
	builder, plan, setup, accounts := setupFixture(t, SetupMetadata)
	data := make([]byte, 1032)
	copy(data, accountDiscriminator("UserMetadata"))
	foreign := mustKey(fixedKey("foreign"))
	copy(data[80:112], foreign[:])
	accounts[setup.Account] = backyard.ConfirmedAccount{Address: setup.Account, Owner: KLendProgramID, Data: data}
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
