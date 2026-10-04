package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

func TestTopUpPreflightUsesOfficialBuilderAndActualPolicy(t *testing.T) {
	path := os.Getenv("KAMINO_TEST_KLEND_PROXY_PATH")
	if path == "" {
		t.Skip("requires the locally built official Rust KLend proxy")
	}
	binaryBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(binaryBytes)
	proxy, err := fleet.NewKLendProxy(path, hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	plan, _ := testPullPlan()
	plan.Target.VaultUsdcAta, err = deriveVaultATA(mustKey(plan.Target.VaultPubkey), mustKey(USDCMint), mustKey(splTokenID))
	if err != nil {
		t.Fatal(err)
	}
	plan.Target.VaultTokenAta = plan.Target.VaultUsdcAta
	obligation, err := vanillaObligationKey(mustKey(plan.Target.VaultPubkey), mustKey(plan.Market))
	if err != nil {
		t.Fatal(err)
	}
	reserve := make([]byte, reserveDataLength)
	disc, _ := hex.DecodeString(reserveDiscriminator)
	copy(reserve, disc)
	binary.LittleEndian.PutUint64(reserve[8:16], 1)
	binary.LittleEndian.PutUint64(reserve[16:24], 100)
	put := func(raw []byte, offset int, value string) { k := mustKey(value); copy(raw[offset:offset+32], k[:]) }
	for offset, value := range map[int]string{
		reserveMarketOffset: plan.Market, reserveLiquidityMintOffset: USDCMint,
		reserveLiquiditySupplyOffset: fixedKey("supply"), reserveCollateralMintOffset: fixedKey("collateral-mint"),
		reserveCollateralSupplyOffset: fixedKey("collateral-supply"), reserveLiquidityProgramOffset: splTokenID,
	} {
		put(reserve, offset, value)
	}
	obligationData := make([]byte, obligationDataLength)
	copy(obligationData, obligationDiscriminator[:])
	binary.LittleEndian.PutUint64(obligationData[16:24], 100)
	put(obligationData, 32, plan.Market)
	put(obligationData, 64, plan.Target.VaultPubkey)
	custody := make([]byte, 165)
	put(custody, 0, USDCMint)
	put(custody, 32, plan.Target.VaultPubkey)
	custody[108] = 1
	route := decodeReservePosition(plan.Reserve, reserve)
	route.Obligation = obligation
	route.Position.Obligation = obligation
	route.Position.VaultLiquidityATA = plan.Target.VaultUsdcAta
	official, err := proxy.BuildIdleDeposit(t.Context(), fleet.KaminoIdleDepositRequest{Vault: plan.Target.VaultPubkey, Target: route.Position, DepositLiquidityAmount: uint64(plan.AmountRaw)})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, solana.PrivateKey(key).PublicKey().String(), 1, official.Protected)
	if err != nil {
		t.Fatal(err)
	}
	byAddress := map[string]backyard.ConfirmedAccount{
		plan.Reserve:                   {Address: plan.Reserve, Owner: KLendProgramID, Data: reserve},
		plan.Market:                    {Address: plan.Market, Owner: KLendProgramID, Data: make([]byte, 8)},
		obligation:                     {Address: obligation, Owner: KLendProgramID, Data: obligationData},
		plan.Target.VaultUsdcAta:       {Address: plan.Target.VaultUsdcAta, Owner: splTokenID, Data: custody},
		plan.Target.RoutePolicyAccount: {Address: plan.Target.RoutePolicyAccount, Owner: squadsProgramID, Data: policy},
	}
	builder, err := NewSweepWireBuilder(proxy, key, func(ctx context.Context, addresses []string, optional ...string) (int64, []backyard.ConfirmedAccount, error) {
		out := make([]backyard.ConfirmedAccount, 0, len(addresses))
		for _, address := range addresses {
			out = append(out, byAddress[address])
		}
		return 100, out, nil
	})
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
