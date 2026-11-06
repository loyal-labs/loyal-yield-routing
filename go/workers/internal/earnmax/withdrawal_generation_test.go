package earnmax

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestWithdrawalGenerationRejectsRevokedOwnerWithUnchangedSeed(t *testing.T) {
	owner := solana.NewWallet().PublicKey()
	seed := uint64(3)
	rawSeed := [16]byte{16}
	settings, bump, err := squads.SettingsAddress(rawSeed)
	if err != nil {
		t.Fatal(err)
	}
	state := squads.Settings{Seed: rawSeed, Bump: bump, Threshold: 1, PolicySeed: &seed, Signers: []squads.Signer{{Key: owner, Permissions: squads.FullPermissions}}}
	if got, err := authorizedWithdrawalPolicySeed(state, settings, owner); err != nil || got != 3 {
		t.Fatalf("authorized owner: %d %v", got, err)
	}
	state.Signers[0].Permissions = squads.PermissionInitiate
	if _, err := authorizedWithdrawalPolicySeed(state, settings, owner); err == nil {
		t.Fatal("revoked owner retained withdrawal authority with unchanged seed")
	}
	state.Signers[0].Permissions = squads.FullPermissions
	state.Threshold = 2
	if _, err := authorizedWithdrawalPolicySeed(state, settings, owner); err == nil {
		t.Fatal("multi-owner authority unexpectedly admitted")
	}
}

type withdrawalGenerationFixture struct {
	pages      [][]chain.Signed
	executions map[solana.Signature]chain.Execution
	calls      int
}

func (f *withdrawalGenerationFixture) History(context.Context, solana.PublicKey, int, solana.Signature, rpc.CommitmentType, uint64) ([]chain.Signed, error) {
	f.calls++
	if f.calls > len(f.pages) {
		return nil, nil
	}
	return f.pages[f.calls-1], nil
}
func (f *withdrawalGenerationFixture) Execution(_ context.Context, s solana.Signature, _ rpc.CommitmentType) (chain.Execution, error) {
	return f.executions[s], nil
}

func withdrawalExecutionFixture(t *testing.T, a solana.PublicKey, signature solana.Signature, kinds ...[8]byte) chain.Execution {
	t.Helper()
	user, err := voltr.UserAccounts(Vault, LPMint, a)
	if err != nil {
		t.Fatal(err)
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, LPMint, solana.TokenProgramID)
	if err != nil {
		t.Fatal(err)
	}
	keys := []solana.PublicKey{voltr.ProgramID, a, Vault, LPMint, user.LPATA, escrow, user.Receipt, solana.TokenProgramID, solana.SystemProgramID}
	tx := &solana.Transaction{Signatures: []solana.Signature{signature}}
	for _, kind := range kinds {
		data := append([]byte(nil), kind[:]...)
		if kind == voltr.RequestWithdrawVaultDiscriminator {
			data = append(data, make([]byte, 10)...)
			binary.LittleEndian.PutUint64(data[8:], 100)
			data[16] = 1
		}
		tx.Message.Instructions = append(tx.Message.Instructions, solana.CompiledInstruction{ProgramIDIndex: 0, Accounts: []uint16{1, 1, 8, 2, 3, 4, 5, 6, 7, 8}, Data: data})
	}
	return chain.Execution{Receipt: chain.Receipt{Slot: 10, Keys: keys}, Transaction: tx}
}

func TestWithdrawalGenerationNeedsOnlyLatestRequestNotLifetimeHistory(t *testing.T) {
	a := solana.NewWallet().PublicKey()
	sig := solana.Signature{1}
	ex := withdrawalExecutionFixture(t, a, sig, voltr.RequestWithdrawVaultDiscriminator)
	f := &withdrawalGenerationFixture{pages: [][]chain.Signed{{{Signature: sig, Slot: 10}}}, executions: map[solana.Signature]chain.Execution{sig: ex}}
	got, err := ReadWithdrawalRequest(context.Background(), f, a, 10)
	if err != nil || got.RequestSignature != sig.String() || f.calls != 1 {
		t.Fatalf("latest generation: %#v %v calls%d", got, err, f.calls)
	}
	c := WithdrawalConsent{WithdrawalGeneration: got.WithdrawalGeneration, LPRaw: "100"}
	if err := verifyRequestInstruction(got.Execution, c, a); err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawalGenerationUsesFinalInvocationAndRejectsSettled(t *testing.T) {
	a := solana.NewWallet().PublicKey()
	sig := solana.Signature{2}
	ex := withdrawalExecutionFixture(t, a, sig, voltr.RequestWithdrawVaultDiscriminator, voltr.CancelRequestWithdrawVaultDiscriminator, voltr.RequestWithdrawVaultDiscriminator)
	f := &withdrawalGenerationFixture{pages: [][]chain.Signed{{{Signature: sig, Slot: 10}}}, executions: map[solana.Signature]chain.Execution{sig: ex}}
	got, err := ReadWithdrawalRequest(context.Background(), f, a, 10)
	if err != nil || got.RequestInstruction != 2 {
		t.Fatalf("wrong reused receipt generation: %#v %v", got, err)
	}
	c := WithdrawalConsent{WithdrawalGeneration: WithdrawalGeneration{RequestSignature: sig.String(), RequestInstruction: 0}, LPRaw: "100"}
	if verifyRequestInstruction(ex, c, a) == nil {
		t.Fatal("superseded request in same transaction accepted")
	}
	ex = withdrawalExecutionFixture(t, a, sig, voltr.RequestWithdrawVaultDiscriminator, voltr.WithdrawVaultDiscriminator)
	f.calls = 0
	f.executions[sig] = ex
	if _, err := ReadWithdrawalRequest(context.Background(), f, a, 10); err == nil {
		t.Fatal("settled receipt generation reused")
	}
}

func TestWithdrawalGenerationPaginatesReceiptSpamWithoutPnLHistory(t *testing.T) {
	a := solana.NewWallet().PublicKey()
	f := &withdrawalGenerationFixture{pages: make([][]chain.Signed, 2), executions: map[solana.Signature]chain.Execution{}}
	for i := 0; i < 100; i++ {
		sig := solana.Signature{byte(i + 1)}
		f.pages[0] = append(f.pages[0], chain.Signed{Signature: sig, Slot: 10})
		f.executions[sig] = withdrawalExecutionFixture(t, a, sig)
	}
	sig := solana.Signature{101}
	f.pages[1] = []chain.Signed{{Signature: sig, Slot: 10}}
	f.executions[sig] = withdrawalExecutionFixture(t, a, sig, voltr.RequestWithdrawVaultDiscriminator)
	got, err := ReadWithdrawalRequest(context.Background(), f, a, 10)
	if err != nil || got.RequestSignature != sig.String() || f.calls != 2 {
		t.Fatalf("paged generation: %#v %v calls%d", got, err, f.calls)
	}
}
