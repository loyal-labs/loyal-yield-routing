package fleetexec

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

type fixtureAccounts struct {
	accounts map[string]fleet.Account
	slot     int64
}

func (f fixtureAccounts) FinalizedAccounts(_ context.Context, addresses []string, floor int64) (int64, []fleet.Account, error) {
	out := make([]fleet.Account, 0, len(addresses))
	for _, address := range addresses {
		a, ok := f.accounts[address]
		if !ok {
			return 0, nil, errors.New("account missing")
		}
		out = append(out, a)
	}
	return f.slot, out, nil
}
func TestObligationAbsenceProofUsesCollateralAndExactIdentity(t *testing.T) {
	fixture := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(fixture.FeePayer)
	market := sdk.MustPublicKeyFromBase58(fixture.SecondaryAccount)
	reserve := sdk.MustPublicKeyFromBase58(fixture.RecentBlockhash)
	a := fleet.Account{Owner: fleet.KaminoProgram, Lamports: 1, Data: make([]byte, 3344)}
	copy(a.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167})
	copy(a.Data[32:64], market[:])
	copy(a.Data[64:96], owner[:])
	copy(a.Data[96:128], reserve[:])
	binary.LittleEndian.PutUint64(a.Data[128:136], 42)
	got, err := obligationCollateral(a, market.String(), owner.String(), reserve.String())
	if err != nil || got != 42 {
		t.Fatalf("collateral read %d: %v", got, err)
	}
	if _, err := obligationCollateral(a, market.String(), reserve.String(), reserve.String()); err == nil {
		t.Fatal("wrong custody owner accepted")
	}
	copy(a.Data[232:264], reserve[:])
	if _, err := obligationCollateral(a, market.String(), owner.String(), reserve.String()); err == nil {
		t.Fatal("duplicate deposit accepted")
	}
}
func TestIdleAbsenceProofRejectsMissingChangedAndOldObservations(t *testing.T) {
	fixture := mustSignedFixture(t)
	owner := sdk.MustPublicKeyFromBase58(fixture.FeePayer)
	mint := sdk.MustPublicKeyFromBase58(fleet.USDCMint)
	b := noEffectBaseline{Vault: owner.String(), Mint: mint.String(), IdleAmount: 42}
	b.Plan.SourceKind = "idle_vault_usdc"
	b.Plan.IdleAccount = fixture.SecondaryAccount
	a := fleet.Account{Address: b.Plan.IdleAccount, Owner: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", Lamports: 1, Data: make([]byte, 165)}
	copy(a.Data[:32], mint[:])
	copy(a.Data[32:64], owner[:])
	a.Data[108] = 1
	binary.LittleEndian.PutUint64(a.Data[64:72], 42)
	rpc := fixtureAccounts{map[string]fleet.Account{a.Address: a}, 100}
	if slot, err := observeSameMintNoEffect(context.Background(), rpc, b, 100); err != nil || slot != 100 {
		t.Fatalf("unchanged idle observation rejected: %d %v", slot, err)
	}
	if _, err := observeSameMintNoEffect(context.Background(), rpc, b, 101); err == nil {
		t.Fatal("old account observation accepted")
	}
	b.IdleAmount = 41
	if _, err := observeSameMintNoEffect(context.Background(), rpc, b, 100); err == nil {
		t.Fatal("changed custody accepted")
	}
	b.IdleAmount = 0
	rpc.accounts = map[string]fleet.Account{}
	if _, err := observeSameMintNoEffect(context.Background(), rpc, b, 100); err == nil {
		t.Fatal("missing account treated as zero")
	}
}
func TestLeaseWorkDeadlineUsesDatabaseExpiryWithMargin(t *testing.T) {
	expires := time.Date(2026, 10, 2, 0, 0, 30, 0, time.UTC)
	if got := leaseWorkDeadline(expires, time.Minute); !got.Equal(expires.Add(-5 * time.Second)) {
		t.Fatalf("deadline %v outlives bounded database fence", got)
	}
	if got := leaseWorkDeadline(expires, time.Second); !got.Equal(expires.Add(-100 * time.Millisecond)) {
		t.Fatalf("short lease margin %v", got)
	}
}

type processedStatus struct{ err string }

func (p processedStatus) SignatureStatus(context.Context, string) (SignatureStatus, error) {
	return SignatureStatus{Found: true, Confirmed: false, Err: p.err, Slot: 100, ContextSlot: 120, BlockHeight: 5000}, nil
}
func (processedStatus) FinalizedTransaction(context.Context, string) (*TransactionReceipt, error) {
	return nil, nil
}
func TestSeenUnconfirmedNeverExpiresOrReleasesCustody(t *testing.T) {
	for _, chainError := range []string{"", `{"InstructionError":[0,"InvalidArgument"]}`} {
		worker := &Worker{status: processedStatus{chainError}}
		lease := SubmissionLease{Submission: SubmissionRecord{State: StateEffectAmbiguous, MovementLeg: LegRoute, LastValidBlockHeight: 1000, BroadcastCount: 1, ExpiryObservedBlockHeight: int64Ptr(5000), EffectCheckSlot: int64Ptr(90)}}
		// A nil store intentionally ensures this path cannot attempt any terminal
		// database transition, even with a seen error after blockhash expiry.
		if err := worker.recoverBySignature(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSameMintNoEffectAcceptsFinalizedAbsentZeroTargetOnly(t *testing.T) {
	f := mustSignedFixture(t)
	c := sameMintPostContract{vault: f.FeePayer, source: f.SecondaryAccount, target: f.RecentBlockhash, mint: fleet.USDCMint, sourceKind: "reserve_position", minimumSlot: 1000}
	rpc := postFixture(t, c, 1001, 7, 0)
	_, targetObligation, _, _, err := reservePostIdentity(rpc.accounts[c.target], c.mint, c.vault)
	if err != nil {
		t.Fatal(err)
	}
	rpc.accounts[targetObligation] = fleet.Account{Address: targetObligation}
	b := noEffectBaseline{Vault: c.vault, Source: c.source, Target: c.target, Mint: c.mint, SourceAmount: 7, TargetAmount: 0}
	b.Plan.SourceKind = "reserve_position"
	b.Plan.SourceSemantics = "kamino_obligation_collateral_deposited_amount"
	if slot, err := observeSameMintNoEffect(context.Background(), rpc, b, 1000); err != nil || slot != 1001 {
		t.Fatalf("absent zero target rejected: %d %v", slot, err)
	}
	b.TargetAmount = 1
	if _, err := observeSameMintNoEffect(context.Background(), rpc, b, 1000); err == nil {
		t.Fatal("missing funded target became no-effect proof")
	}
}
