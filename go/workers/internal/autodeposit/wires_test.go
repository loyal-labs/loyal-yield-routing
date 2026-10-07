package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	solwire "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/solana"
)

// fixedKey renders a deterministic, genuinely valid 32-byte public key for a
// test label. The system program's all-zero rendering cannot round-trip here,
// so every fixture key is a real base58 key derived from a digest.
func fixedKey(label string) string {
	sum := sha256.Sum256([]byte("autodeposit-lane-a:" + label))
	return base58Key(sum[:])
}

// testDelegationData builds a delegation account body at the official
// loyal-actions offsets, so the decoder is pinned to the real wire layout.
func testDelegationData(delegator, delegatee, mint string, perPeriod, pulled uint64) []byte {
	data := make([]byte, delegationDataLen)
	delegatorKey, delegateeKey, mintKey := mustKey(delegator), mustKey(delegatee), mustKey(mint)
	copy(data[delegationDelegatorOffset:], delegatorKey[:])
	copy(data[delegationDelegateeOffset:], delegateeKey[:])
	copy(data[delegationMintOffset:], mintKey[:])
	authority, err := subscriptionAuthorityKey(delegatorKey[:], mintKey[:])
	if err != nil {
		panic(err)
	}
	copy(data[delegationAuthorityOffset:], authority[:])
	binary.LittleEndian.PutUint64(data[delegationPerPeriodOffset:delegationPerPeriodOffset+8], perPeriod)
	binary.LittleEndian.PutUint64(data[delegationAmountPulledOffset:delegationAmountPulledOffset+8], pulled)
	data[delegationDiscriminatorOffset] = delegationDiscriminator
	return data
}

func TestRemainingDelegationAllowanceDecodesOfficialLayout(t *testing.T) {
	wallet, vault := fixedKey("wallet"), fixedKey("vault")
	data := testDelegationData(wallet, vault, USDCMint, 5_000_000, 1_200_000)
	allowance, err := RemainingDelegationAllowance(SubscriptionsProgramID, data, DelegationIdentity{
		Account: "delegation", Delegator: wallet, Delegatee: vault, Mint: USDCMint,
	})
	if err != nil {
		t.Fatalf("decode official delegation: %v", err)
	}
	if allowance != 3_800_000 {
		t.Fatalf("allowance %d, want 3800000", allowance)
	}
}

func TestRemainingDelegationAllowanceRejectsForeignAccounts(t *testing.T) {
	wallet, vault, foreign := fixedKey("wallet"), fixedKey("vault"), fixedKey("foreign-wallet")
	identity := DelegationIdentity{Account: "delegation", Delegator: wallet, Delegatee: vault, Mint: USDCMint}
	data := testDelegationData(wallet, vault, USDCMint, 5_000_000, 0)

	// A wrong program owner is never readable as an allowance.
	if _, err := RemainingDelegationAllowance(fixedKey("some-program"), data, identity); err == nil {
		t.Fatal("a foreign program owner must not decode")
	}
	// A delegator swap must not authorize this family's pull.
	copied := testDelegationData(foreign, vault, USDCMint, 5_000_000, 0)
	if _, err := RemainingDelegationAllowance(SubscriptionsProgramID, copied, identity); err == nil {
		t.Fatal("a foreign delegator must not decode")
	}
	// A pulled counter above its budget is corrupt, never a fresh allowance.
	corrupt := testDelegationData(wallet, vault, USDCMint, 5_000_000, 5_000_001)
	if _, err := RemainingDelegationAllowance(SubscriptionsProgramID, corrupt, identity); err == nil {
		t.Fatal("a corrupt pulled counter must not decode")
	}
	// A discriminator flip is not a delegation account.
	flipped := testDelegationData(wallet, vault, USDCMint, 5_000_000, 0)
	flipped[delegationDiscriminatorOffset] = 4
	if _, err := RemainingDelegationAllowance(SubscriptionsProgramID, flipped, identity); err == nil {
		t.Fatal("a wrong discriminator must not decode")
	}
}

// testWireBuilder builds a SweepWireBuilder over a throwaway executor key and
// a source-shaped policy fixture: BuildPull checks the actual constraints.
func testWireBuilder(t *testing.T) *SweepWireBuilder {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate executor key: %v", err)
	}
	builder, err := NewSweepWireBuilder(private, func(ctx context.Context, addresses []string, optional ...string) (int64, []backyard.ConfirmedAccount, error) {
		plan, delegation := testPullPlan()
		wallet, mint := mustKey(plan.Target.Wallet), mustKey(USDCMint)
		authority, err := subscriptionAuthorityKey(wallet[:], mint[:])
		if err != nil {
			return 0, nil, err
		}
		event, err := subscriptionEventAuthorityKey()
		if err != nil {
			return 0, nil, err
		}
		data := make([]byte, 73)
		data[0] = 5
		binary.LittleEndian.PutUint64(data[1:9], uint64(plan.AmountRaw))
		copy(data[9:41], wallet[:])
		copy(data[41:], mint[:])
		inner := fleet.RouteInstruction{Program: SubscriptionsProgramID, Data: data, Accounts: []fleet.InstructionAccount{
			{Address: delegation, Writable: true}, {Address: base58Key(authority[:])}, {Address: plan.Target.WalletUsdcAta, Writable: true}, {Address: plan.Target.VaultUsdcAta, Writable: true}, {Address: USDCMint}, {Address: splTokenID}, {Address: plan.Target.VaultPubkey, Signer: true}, {Address: base58Key(event[:])}, {Address: SubscriptionsProgramID},
		}}
		policy, err := fleet.BuildExactPolicyFixture(plan.Target.Settings, solana.PrivateKey(private).PublicKey().String(), 1, []fleet.RouteInstruction{inner})
		if err != nil {
			return 0, nil, err
		}
		return 500, []backyard.ConfirmedAccount{{Address: addresses[0], Owner: squadsProgramID, Data: policy}}, nil
	})
	if err != nil {
		t.Fatalf("build wire builder: %v", err)
	}
	return builder
}

func testPullPlan() (DepositPlan, string) {
	wallet, policy := fixedKey("wallet"), fixedKey("policy")
	settings := mustKey(fixedKey("settings"))
	vaultKey, err := findProgramAddress([][]byte{[]byte("smart_account"), settings[:], []byte("smart_account"), {1}}, squadsProgramID)
	if err != nil {
		panic(err)
	}
	vault := base58Key(vaultKey[:])
	walletAta, custodyAta := fixedKey("wallet-ata"), fixedKey("custody-ata")
	plan := DepositPlan{
		Version:       DepositPlanVersion,
		AmountRaw:     1_234_567,
		Reserve:       fixedKey("reserve"),
		Market:        fixedKey("market"),
		LiquidityMint: USDCMint,
		Target: DepositPlanTarget{
			ID: 1, ManagedVaultID: 1, Settings: fixedKey("settings"), VaultIndex: 1,
			Wallet:             wallet,
			WalletUsdcAta:      walletAta,
			WalletTokenAta:     walletAta,
			VaultPubkey:        vault,
			VaultUsdcAta:       custodyAta,
			VaultTokenAta:      custodyAta,
			TokenMint:          USDCMint,
			SweepPolicyAccount: fixedKey("sweep-policy"),
			RoutePolicyAccount: policy,
			RoutePolicySeed:    7,
		},
	}
	return plan, fixedKey("delegation")
}

func TestBuildPullProducesSignedSubscriptionsWire(t *testing.T) {
	builder := testWireBuilder(t)
	plan, delegation := testPullPlan()
	blockhash := base58Key(bytes.Repeat([]byte{9}, 32))
	wire, err := builder.BuildPull(t.Context(), PullWireRequest{
		Plan: plan, RecurringDelegation: delegation,
		RecentBlockhash: blockhash, LastValidBlockHeight: 900,
	})
	if err != nil {
		t.Fatalf("build pull: %v", err)
	}
	raw, err := base64StdDecode(wire.SignedTransactionBase64)
	if err != nil {
		t.Fatalf("wire is not base64: %v", err)
	}
	if len(raw) > solanaPacketBytes {
		t.Fatalf("pull wire is %d bytes, exceeds %d", len(raw), solanaPacketBytes)
	}
	if _, err := decodeSignedWireMessage(raw); err != nil {
		t.Fatalf("signed pull wire does not parse: %v", err)
	}
	// The digest is the sha256 of the exact bytes, per the shared contract.
	if wire.SignedTransactionSHA256 != hexOrPanic(raw) {
		t.Fatal("wire digest is not the sha256 of the decoded bytes")
	}
	// The persisted digest reproduces through OwnSignedWire.
	if _, err := solwire.OwnSignedWire(raw, wire.SignedTransactionSHA256); err != nil {
		t.Fatalf("wire fails the shared packet contract: %v", err)
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Signature != tx.Signatures[0].String() {
		t.Fatal("persisted signature differs from exact wire")
	}
	// The only instruction is the policy-wrapped subscriptions transfer.
	message, err := decodeSignedWireMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.instructions) != 1 {
		t.Fatalf("pull wire has %d instructions, want exactly one wrapped transfer", len(message.instructions))
	}
	wrapper := message.instructions[0]
	if !bytes.Equal(wrapper.data[:8], squadsExecuteSyncV2Discriminator[:]) {
		t.Fatal("pull instruction is not wrapped in the squads policy envelope")
	}
	inner, err := parseWrappedCompiledInstruction(wrapper)
	if err != nil {
		t.Fatalf("unwrap pull instruction: %v", err)
	}
	if !keyEqual(inner.program, SubscriptionsProgramID) {
		t.Fatalf("wrapped program is %s, want the subscriptions program", inner.program)
	}
	if len(inner.data) != 73 || inner.data[0] != subscriptionsTransferRecurring {
		t.Fatalf("pull data is %d bytes tag %d, want 73 bytes tag 5", len(inner.data), inner.data[0])
	}
	amount := binary.LittleEndian.Uint64(inner.data[1:9])
	if int64(amount) != plan.AmountRaw {
		t.Fatalf("pull wire amount %d, want the frozen %d", amount, plan.AmountRaw)
	}
	// Account order is subscription_recurring_sweep_constraint: delegation,
	// subscription authority, wallet ATA, vault custody, mint, token program,
	// vault (signer), event authority, program.
	if !keyEqual(inner.accounts[0], delegation) {
		t.Fatalf("pull account 0 is %s, want the delegation", inner.accounts[0])
	}
	if !keyEqual(inner.accounts[2], plan.Target.WalletUsdcAta) {
		t.Fatalf("pull account 2 is %s, want the wallet USDC ATA", inner.accounts[2])
	}
	if !keyEqual(inner.accounts[3], plan.Target.VaultUsdcAta) {
		t.Fatalf("pull account 3 is %s, want the frozen vault custody", inner.accounts[3])
	}
	if inner.accounts[6] != mustKey(plan.Target.VaultPubkey) {
		t.Fatalf("pull account 6 is %s, want the vault as the sole inner signer", inner.accounts[6])
	}
	if wrapper.accounts[0] != mustKey(plan.Target.SweepPolicyAccount) || wrapper.data[8] != 1 {
		t.Fatal("pull used the wrong policy or vault index")
	}
	if wrapper.accounts[2] != builder.delegate {
		t.Fatalf("wrapper signer is %s, want the injected executor", wrapper.accounts[2])
	}
}

func hexOrPanic(raw []byte) string {
	return hexPrefix(mustSHA256(raw))
}

func TestBuildPullRejectsNonUSDCAndMissingDelegation(t *testing.T) {
	builder := testWireBuilder(t)
	blockhash := base58Key(bytes.Repeat([]byte{7}, 32))
	plan, _ := testPullPlan()
	plan.Target.TokenMint = "not-usdc"
	if _, err := builder.BuildPull(t.Context(), PullWireRequest{
		Plan: plan, RecurringDelegation: fixedKey("delegation"), RecentBlockhash: blockhash,
		LastValidBlockHeight: 900,
	}); err == nil {
		t.Fatal("a non-USDC plan must never build a pull wire")
	}
	plan, _ = testPullPlan()
	if _, err := builder.BuildPull(t.Context(), PullWireRequest{
		Plan: plan, RecentBlockhash: blockhash, LastValidBlockHeight: 900,
	}); err == nil {
		t.Fatal("a pull without its recurring delegation must never be built")
	}
}
