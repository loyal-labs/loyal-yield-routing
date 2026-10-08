package fleet

import (
	"crypto/sha256"
	"testing"
)

// parityKey is a deterministic synthetic account: sha256 of a fixed label.
func parityKey(label string) string {
	hash := sha256.Sum256([]byte("loyal-fleet-manifest-parity:" + label))
	return encodeBase58(hash[:])
}

// A same-mint route that initializes its target obligation under the vault's
// setup policy, on two live Kamino USDC reserves. Vault, settings and both
// policies are synthetic; every vault account is derived from the synthetic
// vault. The expected fingerprint was produced by Rust's
// loyal-route-lookup-tables lookup_table_manifest_hash over
// LookupTableManifest::from_instructions for these same instructions and
// typed provenance. The method (the route's own instructions, no
// compute-budget program, the selected fee payer, the setup policy typed as a
// policy) was cross-checked against a request the Rust fleet worker recorded
// in production, whose fingerprint it reproduced exactly.
//
// Matching Rust keeps waiting-ALT demand and request idempotency
// (cluster, vault, requirements_fingerprint) interchangeable across the swap.
func TestSameMintManifestMatchesRustSetupPolicyFingerprint(t *testing.T) {
	const rustFingerprint = "5dc11717cdd62ab5d0d22007647de4de0f024006e0189197e6fa0f6b35e2ca96"
	const (
		payer = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"
		token = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
		scope = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"
	)
	vault, settings := parityKey("vault"), parityKey("settings")
	routePolicy, setupPolicy := parityKey("route-policy"), parityKey("setup-policy")
	// Kamino main-market and second-market USDC reserves with their own
	// market, supply, collateral and farm accounts.
	source := KaminoPositionAccounts{Reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59", Market: "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF", LiquidityMint: USDCMint,
		LiquiditySupply: "Bgq7trRgVMeq33yt235zM2onQ4bRDBsY5EWiTetF4qw6", CollateralMint: "B8V6WVjPxW1UGwVDfxH2d2r8SyT4cqn7dQRK6XneVa7D", CollateralSupply: "3DzjXRfxRm6iejfyyMynR4tScddaanrePJ1NJU2XnPPL",
		LiquidityTokenProgram: token, ScopePrices: scope, ReserveFarmState: "JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh"}
	target := KaminoPositionAccounts{Reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", Market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", LiquidityMint: USDCMint,
		LiquiditySupply: "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM", CollateralMint: "6M89FWrQaqcy3domy85J1a1wVMnviL86WeUqbqTXf1qb", CollateralSupply: "25x4aEFoJE3bk4sdNLgHrrmchyop1JvcmGA4ccA6tWWT",
		LiquidityTokenProgram: token, ScopePrices: scope, ReserveFarmState: "6Y9fzrWzGZaxdAJ2eWRg9UZpL3kqPDiVXAb67KJpWdUg"}
	for _, p := range []*KaminoPositionAccounts{&source, &target} {
		derived, err := DeriveVaultReserveAccounts(*p, vault)
		if err != nil {
			t.Fatal(err)
		}
		*p = derived
	}
	metadata, err := findProgramAddress(KLendProgram, []byte("user_meta"), mustKey(t, vault))
	if err != nil {
		t.Fatal(err)
	}
	type account struct {
		address  string
		writable bool
	}
	shared := []account{{"11111111111111111111111111111111", false}, {target.CollateralSupply, true}, {source.CollateralSupply, true}, {scope, false}, {target.CollateralMint, true}, {target.MarketAuthority, false}, {target.Market, false}, {target.ReserveFarmState, true}, {source.Market, false}, {source.MarketAuthority, false}, {target.Reserve, true}, {source.CollateralMint, true}, {target.LiquiditySupply, true}, {source.LiquiditySupply, true}, {source.Reserve, true}, {USDCMint, false}, {farmsProgram, false}, {source.ReserveFarmState, true}, {"Sysvar1nstructions1111111111111111111111111", false}, {"SysvarRent111111111111111111111111111111111", false}, {token, false}}
	vaultRows := []account{{setupPolicy, true}, {routePolicy, true}, {target.ObligationFarmUserState, true}, {metadata, false}, {source.VaultLiquidityATA, true}, {source.Obligation, true}, {target.Obligation, true}, {vault, true}, {source.ObligationFarmUserState, true}}
	// Two top-level programs: KLend refreshes and Squads policy executions
	// signed and paid by the delegated signer.
	refresh := RouteInstruction{Program: KLendProgram}
	execute := RouteInstruction{Program: SquadsProgram, Accounts: []InstructionAccount{{Address: payer, Signer: true, Writable: true}}}
	for _, a := range shared {
		refresh.Accounts = append(refresh.Accounts, InstructionAccount{Address: a.address, Writable: a.writable})
	}
	for _, a := range vaultRows {
		execute.Accounts = append(execute.Accounts, InstructionAccount{Address: a.address, Writable: a.writable})
	}
	input := KaminoSameMintRouteRequest{Vault: vault, Source: source, Target: target, TargetObligationMissing: true}
	manifest, err := sameMintRouteALTManifest(input, settings, routePolicy, setupPolicy, payer, []RouteInstruction{refresh, execute})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Fingerprint != rustFingerprint {
		t.Fatalf("Go requirements fingerprint %s differs from Rust's %s", manifest.Fingerprint, rustFingerprint)
	}
	want := map[string]bool{}
	for _, a := range vaultRows {
		want[a.address] = a.writable
	}
	if len(manifest.SharedAddresses) != len(shared) || len(manifest.VaultAddresses) != len(vaultRows) {
		t.Fatalf("durable vectors: shared=%d vault=%d", len(manifest.SharedAddresses), len(manifest.VaultAddresses))
	}
	for _, a := range manifest.VaultAddresses {
		if writable, ok := want[a.Address]; !ok || writable != a.Writable {
			t.Fatalf("vault row %+v is not the route's", a)
		}
	}
}
