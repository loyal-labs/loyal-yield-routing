package autodeposit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// controlGolden is the deployed Autodeposit policy and delegation captured
// from Loyal Apps 45de590 (testdata/autodeposit/canonical-policy-creator.json).
type controlGolden struct {
	SourceCommit, Settings, RootAuthority, Wallet, Vault, WalletATA, VaultATA, Policy, SubscriptionAuthority, RecurringDelegation string
	PolicySeed, MaxAmountPerPeriod, Nonce, PeriodLength, StartTimestamp, ExpiryTimestamp                                          int64
	PolicyDataHex, SettingsDataHex                                                                                                string
}

func controlFixture(t *testing.T) (controlGolden, ControlTarget, *SweepWireBuilder) {
	t.Helper()
	var f controlGolden
	raw, err := os.ReadFile("../../testdata/autodeposit/canonical-policy-creator.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.SourceCommit != "45de590113312d54de7585a710620a8a80b504db" {
		t.Fatal("unexpected source pin")
	}
	target := ControlTarget{TargetID: 1, SetupGeneration: 3, PolicySeed: f.PolicySeed, Nonce: f.Nonce, Settings: f.Settings, Wallet: f.Wallet, WalletTokenATA: f.WalletATA, Vault: f.Vault, VaultTokenATA: f.VaultATA, Mint: USDCMint, Policy: f.Policy, SubscriptionAuthority: f.SubscriptionAuthority, RecurringDelegation: f.RecurringDelegation, MaxAmountPerPeriod: &f.MaxAmountPerPeriod, StartTimestamp: &f.StartTimestamp}
	b, err := NewSweepWireBuilder(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, 32)), func(context.Context, int64, []string, ...string) (int64, []*chain.Account, error) {
		return 0, nil, errors.New("unexpected account read")
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, target, b
}

func goldenHex(t *testing.T, s string) []byte {
	t.Helper()
	v, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// installControlSnapshot serves the golden policy, a delegation with the
// given per-period budget and a wallet token account approving the
// subscription authority.
func installControlSnapshot(t *testing.T, f controlGolden, b *SweepWireBuilder, perPeriod uint64) map[string]testAccount {
	t.Helper()
	token := make([]byte, 165)
	mint, wallet, authority := mustKey(USDCMint), mustKey(f.Wallet), mustKey(f.SubscriptionAuthority)
	copy(token[:32], mint[:])
	copy(token[32:64], wallet[:])
	token[108] = 1
	binary.LittleEndian.PutUint64(token[64:72], 9_000_000)
	binary.LittleEndian.PutUint32(token[72:76], 1)
	copy(token[76:108], authority[:])
	accounts := map[string]testAccount{f.Settings: {Address: f.Settings, Owner: squads.ProgramID.String(), Data: goldenHex(t, f.SettingsDataHex)}, f.Policy: {Address: f.Policy, Owner: squads.ProgramID.String(), Data: goldenHex(t, f.PolicyDataHex)}, f.SubscriptionAuthority: {Address: f.SubscriptionAuthority, Owner: SubscriptionsProgramID}, f.RecurringDelegation: {Address: f.RecurringDelegation, Owner: SubscriptionsProgramID, Data: testDelegationData(f.Wallet, f.Vault, USDCMint, perPeriod, 0)}, f.WalletATA: {Address: f.WalletATA, Owner: splTokenID, Data: token}}
	b.read = fixtureReader(200, accounts)
	return accounts
}

type countingControlReader struct {
	reader ControlReader
	calls  int
}

func (r *countingControlReader) ObserveControl(ctx context.Context, t ControlTarget, slot int64) (ControlObservation, error) {
	r.calls++
	return r.reader.ObserveControl(ctx, t, slot)
}

// seedControlRuntime seeds one mainnet target whose policy and delegation
// match the golden fixture, and a counting control reader over it.
func seedControlRuntime(t *testing.T, s *Store) (int64, *countingControlReader) {
	t.Helper()
	f, target, b := controlFixture(t)
	installControlSnapshot(t, f, b, uint64(f.MaxAmountPerPeriod))
	seeded := seedIntegrationTarget(t, s, "control")
	target.TargetID = seeded.TargetID
	seedProjectedSurplus(t, s, seeded, 1, 9_000_000)
	_, err := s.pool.Exec(t.Context(), `UPDATE loyal_yield.balance_sweep_targets SET settings=$2,authority=$3,policy_seed=$4,policy_account=$5,wallet=$6,wallet_usdc_ata=$7,wallet_token_ata=$7,vault_pubkey=$8,vault_usdc_ata=$9,vault_token_ata=$9,subscription_authority=$10,recurring_delegation=$11,recurring_delegation_nonce=$12,max_amount_per_period=$13,period_length_seconds=$14,start_timestamp=$15,recurring_delegation_expiry_timestamp=$16,setup_generation=$17 WHERE id=$1`, target.TargetID, target.Settings, f.RootAuthority, target.PolicySeed, target.Policy, target.Wallet, target.WalletTokenATA, target.Vault, target.VaultTokenATA, target.SubscriptionAuthority, target.RecurringDelegation, target.Nonce, *target.MaxAmountPerPeriod, f.PeriodLength, *target.StartTimestamp, f.ExpiryTimestamp, target.SetupGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.managed_vaults SET settings=$2,vault_pubkey=$3 WHERE id=$1`, seeded.ManagedVaultID, target.Settings, target.Vault); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE loyal_yield.route_policies SET settings=$2,authority=$3,vault_pubkey=$4 WHERE id=(SELECT active_policy_id FROM loyal_yield.managed_vaults WHERE id=$1)`, seeded.ManagedVaultID, target.Settings, f.RootAuthority, target.Vault); err != nil {
		t.Fatal(err)
	}
	seedTargetLiveReserve(t, s, target.TargetID, "control-reserve", "control-market", 1, time.Now())
	return target.TargetID, &countingControlReader{reader: b}
}

// The policy's per-period cap is an account-data constraint, which the
// envelope match ignores. A delegation whose budget exceeds it is a pull the
// policy refuses on chain, so the control is not active.
func TestControlRequiresThePolicyCapToCoverTheDelegationBudget(t *testing.T) {
	f, target, b := controlFixture(t)
	installControlSnapshot(t, f, b, uint64(f.MaxAmountPerPeriod))
	o, err := b.ObserveControl(t.Context(), target, 1)
	if err != nil || o.status() != "active" {
		t.Fatalf("delegation within the policy cap: %+v %v", o, err)
	}
	over := f.MaxAmountPerPeriod + 1
	installControlSnapshot(t, f, b, uint64(over))
	target.MaxAmountPerPeriod = &over
	o, err = b.ObserveControl(t.Context(), target, 1)
	if err != nil || o.PolicyValid || o.status() != "inconsistent" {
		t.Fatalf("delegation above the policy cap observed as %+v %v", o, err)
	}
}
