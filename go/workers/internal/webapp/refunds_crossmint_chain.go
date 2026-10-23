package webapp

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Ports client.assertEarnCrossMintCanonicalArtifacts
// (web/packages/smart-account-vaults/src/client.ts:8572-8646) and the pieces
// it leans on: createEarnCrossMintPolicyCreationPayload (client.ts:2052-2128),
// fetchRawPolicyAtAddress (client.ts:6232-6274) and assertCanonicalEarnPolicy
// (client.ts:6340-6406). The expected state is what
// createJupiterCrossMintPolicyPlan writes for the shard (loyal-actions
// sdk.ts:216-316), rebuilt from the enrolled spec.

const (
	crossMintJupiterV6ProgramID = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"

	jupiterSwapDiscriminator                  = "\xbb\x64\xfa\xcc\x31\xc4\xaf\x14"
	jupiterSharedAccountsRouteV2Discriminator = "\xd1\x98\x53\x93\x7c\xfe\xd8\xe9"

	// loyal-actions constants.ts:241-247.
	jupiterSwapSlippageBPSOffset           = 24
	jupiterSwapPlatformFeeBPSOffset        = 26
	jupiterSharedAccountsSlippageBPSOffset = 25
	jupiterSharedAccountsPlatformFeeOffset = 27

	// client.ts:6348 — Number(seed) loses precision past MAX_SAFE_INTEGER.
	crossMintMaxSafeSeed = uint64(1)<<53 - 1
)

// crossMintStablecoin is one entry of the six-mint Earn registry, in
// STABLECOINS order (constants.ts:40-47): the ATA list and the shard mints
// keep that order on the wire and in the policy bytes.
type crossMintStablecoin struct {
	Mint    solana.PublicKey
	Program solana.PublicKey
}

var crossMintStablecoins = func() []crossMintStablecoin {
	must := func(text string) solana.PublicKey {
		return solana.MustPublicKeyFromBase58(text)
	}
	return []crossMintStablecoin{
		{must("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH"), solana.Token2022ProgramID},
		{must("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH"), solana.Token2022ProgramID},
		{must("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"), solana.Token2022ProgramID},
		{must("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), solana.TokenProgramID},
		{must("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"), solana.TokenProgramID},
		{must("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"), solana.TokenProgramID},
	}
}()

// crossMintCanonicalVaultATAs builds the ATA list
// (createEarnCrossMintPolicyCreationPayload, client.ts:2058-2066): one ATA per
// registered stablecoin under its own token program.
func crossMintCanonicalVaultATAs(vault solana.PublicKey) ([]solana.PublicKey, error) {
	atas := make([]solana.PublicKey, 0, len(crossMintStablecoins))
	for _, stablecoin := range crossMintStablecoins {
		ata, err := spl.AssociatedTokenAddress(vault, stablecoin.Mint, stablecoin.Program)
		if err != nil {
			return nil, err
		}
		atas = append(atas, ata)
	}
	return atas, nil
}

func crossMintSourceMints(shard string) []solana.PublicKey {
	var mints []solana.PublicKey
	for _, stablecoin := range crossMintStablecoins {
		classic := stablecoin.Program == solana.TokenProgramID
		if shard == "classic" && classic || shard == "token_2022" && !classic {
			mints = append(mints, stablecoin.Mint)
		}
	}
	return mints
}

// crossMintJupiterConstraint ports the `constraint` closure
// (client.ts:2067-2101): one per swap dialect, constraining the vault as
// authority and the six canonical vault ATAs as output, the discriminator,
// the slippage u16 ceiling and a zero platform fee.
func crossMintJupiterConstraint(vault solana.PublicKey, canonicalATAs []solana.PublicKey, dialect string, maxSlippageBps int32) (squads.InstructionConstraintView, error) {
	jupiter, err := solana.PublicKeyFromBase58(crossMintJupiterV6ProgramID)
	if err != nil {
		return squads.InstructionConstraintView{}, err
	}
	layout := struct {
		authority     uint8
		output        uint8
		discriminator string
		slippage      uint64
		platformFee   uint64
	}{}
	switch dialect {
	case "route_v2":
		layout.authority, layout.output = 0, 2
		layout.discriminator = jupiterSwapDiscriminator
		layout.slippage, layout.platformFee = jupiterSwapSlippageBPSOffset, jupiterSwapPlatformFeeBPSOffset
	case "shared_accounts_route_v2":
		layout.authority, layout.output = 1, 5
		layout.discriminator = jupiterSharedAccountsRouteV2Discriminator
		layout.slippage, layout.platformFee = jupiterSharedAccountsSlippageBPSOffset, jupiterSharedAccountsPlatformFeeOffset
	default:
		return squads.InstructionConstraintView{}, fmt.Errorf("unknown cross-mint swap dialect %q", dialect)
	}
	return squads.InstructionConstraintView{
		ProgramID: jupiter,
		AccountConstraints: []squads.AccountConstraintView{
			{AccountIndex: layout.authority, Pubkeys: []solana.PublicKey{vault}},
			{AccountIndex: layout.output, Pubkeys: canonicalATAs},
		},
		DataConstraints: []squads.DataConstraintView{
			{DataOffset: 0, DataValue: squads.DataValueView{Kind: squads.ValueBytes, Bytes: []byte(layout.discriminator)}, Operator: squads.OpEquals},
			{DataOffset: layout.slippage, DataValue: squads.DataValueView{Kind: squads.ValueU16, U16: uint16(maxSlippageBps)}, Operator: squads.OpLessThanOrEqualTo},
			{DataOffset: layout.platformFee, DataValue: squads.DataValueView{Kind: squads.ValueU8, U8: 0}, Operator: squads.OpEquals},
		},
	}, nil
}

// crossMintExpectedPolicy ports createEarnCrossMintPolicyCreationPayload
// (client.ts:2052-2128): the two swap-dialect constraints plus one daily
// spending limit per shard source mint.
func crossMintExpectedPolicy(vault solana.PublicKey, shard string, maxSlippageBps int32, dailyCap uint64) (squads.Policy, error) {
	canonicalATAs, err := crossMintCanonicalVaultATAs(vault)
	if err != nil {
		return squads.Policy{}, err
	}
	routeV2, err := crossMintJupiterConstraint(vault, canonicalATAs, "route_v2", maxSlippageBps)
	if err != nil {
		return squads.Policy{}, err
	}
	sharedAccounts, err := crossMintJupiterConstraint(vault, canonicalATAs, "shared_accounts_route_v2", maxSlippageBps)
	if err != nil {
		return squads.Policy{}, err
	}
	var limits []squads.SpendingLimitView
	for _, mint := range crossMintSourceMints(shard) {
		limits = append(limits, squads.SpendingLimitView{Mint: mint, Period: squads.PeriodDaily, MaxPerPeriod: dailyCap})
	}
	return squads.Policy{VaultIndex: earnVaultIndex, Constraints: []squads.InstructionConstraintView{routeV2, sharedAccounts}, SpendingLimits: limits}, nil
}

// crossMintLimitsSorted orders spending limits the way
// programInteractionPolicySecurity does before comparing
// (program-interaction-policy-security.ts:113): mint base58 localeCompare,
// the collationLess order.
func crossMintLimitsSorted(limits []squads.SpendingLimitView) []squads.SpendingLimitView {
	sorted := append([]squads.SpendingLimitView(nil), limits...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && collationLess(sorted[j].Mint.String(), sorted[j-1].Mint.String()); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}

// crossMintPolicySecurityEquals ports programInteractionPolicySecurityEquals:
// the vault index, the instruction constraints and the mint-sorted spending
// limits. Start timestamps and usage counters are Squads' running state, not
// what the policy allows (squads.SpendingLimitsEqual skips them for the same
// reason).
func crossMintPolicySecurityEquals(actual squads.Policy, expected squads.Policy) bool {
	if actual.VaultIndex != expected.VaultIndex {
		return false
	}
	if !squads.ConstraintsEqual(actual.Constraints, expected.Constraints) {
		return false
	}
	return squads.SpendingLimitsEqual(crossMintLimitsSorted(actual.SpendingLimits), crossMintLimitsSorted(expected.SpendingLimits))
}

func crossMintPolicyLabel(shard string) string {
	return "Cross-mint " + shard + " policy"
}

// assertEarnCrossMintCanonicalArtifacts ports
// client.assertEarnCrossMintCanonicalArtifacts for the toggle route: every
// refusal message carries over, each becoming a 409 autoswap_toggle_failed.
func assertEarnCrossMintCanonicalArtifacts(ctx context.Context, client *chain.Client, settings solana.PublicKey, bound []crossMintBoundPolicy, dailyCap string, maxSlippageBps int32, signer solana.PublicKey) error {
	if len(bound) != 2 || bound[0].SourceShard != "classic" || bound[1].SourceShard != "token_2022" {
		return fmt.Errorf("Cross-mint enrollment requires one classic and one Token-2022 policy.")
	}
	cap, err := strconv.ParseUint(dailyCap, 10, 64)
	if err != nil {
		return fmt.Errorf("Autoswap daily source mint spending cap is invalid.")
	}
	vault, _, err := squads.SmartAccountAddress(settings, earnVaultIndex)
	if err != nil {
		return err
	}
	expected := map[string]squads.Policy{}
	for _, shard := range []string{"classic", "token_2022"} {
		policy, err := crossMintExpectedPolicy(vault, shard, maxSlippageBps, cap)
		if err != nil {
			return err
		}
		expected[shard] = policy
	}

	for _, bound := range bound {
		label := crossMintPolicyLabel(bound.SourceShard)
		expectedAccount, _, err := squads.PolicyAddress(settings, bound.Seed)
		if err != nil {
			return err
		}
		if bound.Account != expectedAccount.String() {
			return fmt.Errorf("%s account does not match its seed.", label)
		}
		_, accounts, err := client.Accounts(ctx, []solana.PublicKey{expectedAccount}, rpc.CommitmentFinalized, 0)
		if err != nil {
			return err
		}
		if accounts[0] == nil {
			return fmt.Errorf("%s is not finalized on-chain.", label)
		}
		if accounts[0].Owner != squads.ProgramID {
			return fmt.Errorf("%s %s has an unexpected owner.", label, bound.Account)
		}
		decoded, err := squads.DecodeProgramInteractionPolicy(accounts[0])
		if err != nil {
			return fmt.Errorf("%s %s is not a decodable policy account.", label, bound.Account)
		}
		if err := assertCanonicalCrossMintPolicy(decoded, settings, bound, expected[bound.SourceShard], signer, label); err != nil {
			return err
		}
	}
	return nil
}

// assertCanonicalCrossMintPolicy ports assertCanonicalEarnPolicy
// (client.ts:6340-6406), message for message.
func assertCanonicalCrossMintPolicy(decoded squads.DecodedPolicy, settings solana.PublicKey, bound crossMintBoundPolicy, expected squads.Policy, signer solana.PublicKey, label string) error {
	if bound.Seed > crossMintMaxSafeSeed {
		return fmt.Errorf("%s seed is too large for this client.", label)
	}
	expectedAccount, _, err := squads.PolicyAddress(settings, bound.Seed)
	if err != nil {
		return err
	}
	if bound.Account != expectedAccount.String() {
		return fmt.Errorf("%s account does not match its policy seed.", label)
	}
	if decoded.Settings != settings {
		return fmt.Errorf("%s belongs to another Settings account.", label)
	}
	if decoded.Seed != bound.Seed {
		return fmt.Errorf("%s seed does not match its account.", label)
	}
	if decoded.Threshold != 1 {
		return fmt.Errorf("%s threshold is not canonical.", label)
	}
	if decoded.TimeLock != 0 {
		return fmt.Errorf("%s timelock is not canonical.", label)
	}
	if len(decoded.Signers) != 1 {
		return fmt.Errorf("%s signer set is not canonical.", label)
	}
	if decoded.Signers[0] != signer {
		return fmt.Errorf("%s deployment signer does not match.", label)
	}
	if decoded.Permissions[0] != squads.FullPermissions {
		return fmt.Errorf("%s signer permissions are not canonical.", label)
	}
	// The TS side also proves the decoded payload had no hooks; the decoder
	// refuses hook payloads outright, so reaching here means none.
	if !crossMintPolicySecurityEquals(squads.Policy{
		VaultIndex:     decoded.VaultIndex,
		Constraints:    decoded.Constraints,
		SpendingLimits: decodedFullLimits(decoded),
	}, expected) {
		return fmt.Errorf("%s instruction constraints are not canonical.", label)
	}
	return nil
}

// decodedFullLimits flattens the spending limits the decoder kept; it never
// returns nil for a decoded cross-mint policy, but an empty set must compare
// against the expected three, not skip the check.
func decodedFullLimits(decoded squads.DecodedPolicy) []squads.SpendingLimitView {
	if decoded.Full == nil {
		return nil
	}
	return decoded.Full.Payload.SpendingLimits
}

// fetchCrossMintPolicyExistence ports the delete route's getMultipleAccountsInfo
// (cross-mint/delete/readiness/route.ts:125-131) at confirmed commitment.
func fetchCrossMintPolicyExistence(ctx context.Context, client *chain.Client, policies []string) ([]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	keys := make([]solana.PublicKey, len(policies))
	for i, policy := range policies {
		key, err := solana.PublicKeyFromBase58(policy)
		if err != nil {
			return nil, err
		}
		keys[i] = key
	}
	_, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, err
	}
	existing := make([]bool, len(policies))
	for i, account := range accounts {
		existing[i] = account != nil
	}
	return existing, nil
}
