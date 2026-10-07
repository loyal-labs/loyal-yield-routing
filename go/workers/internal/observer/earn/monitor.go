package earn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/multiply"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	sp "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
)

// PolicyMonitor projects confirmed Squads settings instructions, ported from
// loyal-squads-policy-monitor PolicyMonitor::process_policy_instructions with
// PostgresPolicyMatchSink and Earn MAX projection. Like the Rust monitor shared
// behind one mutex by the stream and every Earn consumer, it projects one
// transaction at a time and skips a signature it already projected: setup
// matches activate their vault without a slot order, so a lagging targeted
// replay of an older setup must not run after the stream applied a removal.
type PolicyMonitor struct {
	store    *Store
	rpc      *solanarpc.Client
	cluster  string
	delegate solana.PublicKey
	mu       sync.Mutex
	seen     map[string]struct{}
}

// NewPolicyMonitor binds the confirmed-commitment projection to one cluster
// spelling ("mainnet-beta" or "devnet") and the Earn MAX delegate.
func NewPolicyMonitor(store *Store, rpc *solanarpc.Client, cluster string, delegate solana.PublicKey) (*PolicyMonitor, error) {
	switch cluster {
	case "mainnet", "mainnet-beta":
		cluster = "mainnet-beta"
	case "devnet":
	default:
		return nil, fmt.Errorf("unsupported policy-monitor cluster %s", cluster)
	}
	if delegate.IsZero() {
		return nil, errors.New("EARN_MAX_DELEGATE is required")
	}
	return &PolicyMonitor{store: store, rpc: rpc, cluster: cluster, delegate: delegate, seen: map[string]struct{}{}}, nil
}

const confirmedCommitment = "confirmed"

// ProcessPolicyInstructions returns the number of projected events. Only the
// stream path waits (bounded) for an RPC behind the event slot; a durable job
// defers instead and the queue retries it.
func (m *PolicyMonitor) ProcessPolicyInstructions(ctx context.Context, signature string, slot uint64, instructions []sp.Instruction, waitBehind bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[signature]; ok {
		return 0, nil
	}
	emitted, err := m.process(ctx, signature, slot, instructions, waitBehind)
	if err == nil {
		m.seen[signature] = struct{}{}
	}
	return emitted, err
}

func (m *PolicyMonitor) process(ctx context.Context, signature string, slot uint64, instructions []sp.Instruction, waitBehind bool) (int, error) {
	emitted := 0
	earnMax := map[solana.PublicKey]uint64{}
	for _, instruction := range instructions {
		affected, err := m.affectedEarnMaxSettings(ctx, instruction)
		if err != nil {
			return emitted, err
		}
		for settings, base := range affected {
			earnMax[settings] = base
		}
		for _, event := range m.events(signature, slot, instruction) {
			if err := m.store.applyPolicyEvent(ctx, event); err != nil {
				return emitted, err
			}
			emitted++
		}
	}
	settings := make([]solana.PublicKey, 0, len(earnMax))
	for key := range earnMax {
		settings = append(settings, key)
	}
	sort.Slice(settings, func(i, j int) bool { return bytes.Compare(settings[i][:], settings[j][:]) < 0 })
	for _, key := range settings {
		if err := m.projectEarnMaxManifest(ctx, key, earnMax[key], signature, slot, waitBehind); err != nil {
			return emitted, err
		}
		emitted++
	}
	return emitted, nil
}

// policyEvent is one PolicyMonitorEvent: exactly one input is set.
type policyEvent struct {
	route, setup *PolicyMatchInput
	sweep        *BalanceSweepPolicyMatchInput
	crossMint    *CrossMintSwapPolicyManifestInput
	removal      *PolicyRemovalInput
}

// events classifies one instruction exactly as the Rust monitor emits:
// a generalized cross-mint policy, else removals, else recognized creates,
// else an incompatible update invalidating the policy.
func (m *PolicyMonitor) events(signature string, slot uint64, instruction sp.Instruction) []policyEvent {
	if event, ok := detectCrossMintPolicy(instruction); ok {
		event.Signature, event.Slot, event.Cluster, event.SourceCommitment = signature, slot, m.cluster, confirmedCommitment
		return []policyEvent{{crossMint: &event}}
	}
	if removals, err := sp.DetectPolicyRemovals(instruction); err == nil && len(removals) > 0 {
		events := make([]policyEvent, 0, len(removals))
		for _, removal := range removals {
			input := m.removal(signature, slot, removal)
			events = append(events, policyEvent{removal: &input})
		}
		return events
	}
	actions, err := sp.DecodeSettingsActions(instruction)
	if err != nil {
		return nil
	}
	var events []policyEvent
	for _, action := range actions {
		if event, ok := detectYieldRoute(action); ok {
			m.stamp(&event, signature, slot)
			events = append(events, policyEvent{route: &event})
		}
		if event, ok := detectYieldSetup(action); ok {
			m.stamp(&event, signature, slot)
			events = append(events, policyEvent{setup: &event})
		}
		if event, ok := detectBalanceSweep(action); ok {
			event.Signature, event.Slot, event.Cluster = signature, slot, m.cluster
			events = append(events, policyEvent{sweep: &event})
		}
	}
	if len(events) == 0 {
		if update, err := sp.DetectPolicyUpdateIdentity(instruction); err == nil && update != nil {
			input := m.removal(signature, slot, *update)
			events = append(events, policyEvent{removal: &input})
		}
	}
	return events
}

func (s *Store) applyPolicyEvent(ctx context.Context, event policyEvent) error {
	switch {
	case event.route != nil:
		return s.RecordPolicyMatch(ctx, *event.route)
	case event.setup != nil:
		return s.RecordSetupPolicyMatch(ctx, *event.setup)
	case event.sweep != nil:
		return s.RecordBalanceSweepPolicyMatch(ctx, *event.sweep)
	case event.crossMint != nil:
		return s.RecordCrossMintSwapPolicyManifest(ctx, *event.crossMint)
	}
	return s.RecordPolicyRemoval(ctx, *event.removal)
}

func (m *PolicyMonitor) stamp(event *PolicyMatchInput, signature string, slot uint64) {
	event.Signature, event.Slot, event.Cluster, event.SourceCommitment = signature, slot, m.cluster, confirmedCommitment
}

func (m *PolicyMonitor) removal(signature string, slot uint64, identity sp.PolicyIdentity) PolicyRemovalInput {
	return PolicyRemovalInput{Signature: signature, Slot: slot, Cluster: m.cluster, SourceCommitment: confirmedCommitment,
		Settings: identity.Settings.String(), Authority: identity.Authority.String(), PolicyAccount: identity.PolicyAccount.String()}
}

var earnMaxFamilies = []multiply.PolicyFamily{multiply.FamilyCollateral, multiply.FamilyDebt, multiply.FamilySwap}

func familyPolicy(strategy multiply.StrategyConfig, family multiply.PolicyFamily) multiply.PolicyConfig {
	switch family {
	case multiply.FamilyCollateral:
		return strategy.CollateralPolicy
	case multiply.FamilyDebt:
		return strategy.DebtPolicy
	}
	return strategy.SwapPolicy
}

func (m *PolicyMonitor) affectedEarnMaxSettings(ctx context.Context, instruction sp.Instruction) (map[solana.PublicKey]uint64, error) {
	out := map[solana.PublicKey]uint64{}
	if actions, err := sp.DecodeSettingsActions(instruction); err == nil {
		for _, action := range actions {
			base, ok, err := m.earnMaxSeedBase(action)
			if err != nil {
				return nil, err
			}
			if ok {
				out[action.Settings] = base
			}
		}
	}
	var identities []sp.PolicyIdentity
	if removals, err := sp.DetectPolicyRemovals(instruction); err == nil {
		identities = append(identities, removals...)
	}
	if update, err := sp.DetectPolicyUpdateIdentity(instruction); err == nil && update != nil {
		identities = append(identities, *update)
	}
	for _, identity := range identities {
		base, ok, err := m.store.EarnMaxPolicySeedBase(ctx, identity.Settings.String(), multiply.EarnMaxVaultIndex)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		topology, err := multiply.DeriveEarnMaxTopology(identity.Settings, base)
		if err != nil {
			return nil, err
		}
		strategy, err := topology.Strategy(multiply.SyrupUsdcUsdc)
		if err != nil {
			return nil, err
		}
		for _, family := range earnMaxFamilies {
			if familyPolicy(strategy, family).Account == identity.PolicyAccount {
				out[identity.Settings] = base
			}
		}
	}
	return out, nil
}

// earnMaxSeedBase is earn_max_policy_seed_base: a canonical create or update
// payload of one family at its seed base offset.
func (m *PolicyMonitor) earnMaxSeedBase(action sp.SettingsAction) (uint64, bool, error) {
	if action.Threshold != 1 || len(action.DelegatedSigners) != 1 || action.DelegatedSigners[0] != m.delegate {
		return 0, false, nil
	}
	topology, err := multiply.DeriveEarnMaxTopology(action.Settings, 0)
	if err != nil {
		return 0, false, err
	}
	for offset, family := range earnMaxFamilies {
		constraints, err := multiply.CanonicalConstraints(topology, family)
		if err != nil {
			return 0, false, err
		}
		if action.Payload.VaultIndex != 0 || len(action.Payload.SpendingLimits) != 0 || !sp.ConstraintsEqual(action.Payload.Constraints, constraints) {
			continue
		}
		if action.PolicySeed < uint64(offset) {
			return 0, false, nil
		}
		base := action.PolicySeed - uint64(offset)
		account, _, err := sp.ActionAccount(action.Settings, action.PolicySeed)
		if err != nil {
			return 0, false, err
		}
		if base > 0 && account == action.PolicyAccount {
			return base, true, nil
		}
	}
	return 0, false, nil
}

// Delays of the bounded wait for an RPC behind the LaserStream event slot.
var policyReloadDelays = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// readAtEventSlot reads accounts no older than the event slot. LaserStream can
// deliver a transaction before the separately configured RPC serves the same
// write; minContextSlot makes the RPC itself refuse an older view, and only
// that refusal waits (bounded) for the node to catch up.
func readAtEventSlot(ctx context.Context, rpc *solanarpc.Client, addresses []string, slot uint64, waitBehind bool) (solanarpc.AccountsResponse, error) {
	for attempt := 0; ; attempt++ {
		response, err := rpc.MultipleAccounts(ctx, addresses, confirmedCommitment, &slot)
		if !waitBehind || !solanarpc.IsBehind(err) || attempt == len(policyReloadDelays) {
			return response, err
		}
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case <-time.After(policyReloadDelays[attempt]):
		}
	}
}

func (m *PolicyMonitor) projectEarnMaxManifest(ctx context.Context, settings solana.PublicKey, base uint64, signature string, slot uint64, waitBehind bool) error {
	topology, err := multiply.DeriveEarnMaxTopology(settings, base)
	if err != nil {
		return err
	}
	strategy, err := topology.Strategy(multiply.SyrupUsdcUsdc)
	if err != nil {
		return err
	}
	type expected struct {
		family      multiply.PolicyFamily
		policy      multiply.PolicyConfig
		constraints []sp.InstructionConstraintView
		semantic    string
	}
	var families []expected
	addresses := make([]string, 0, len(earnMaxFamilies))
	for _, family := range earnMaxFamilies {
		constraints, err := multiply.CanonicalConstraints(topology, family)
		if err != nil {
			return err
		}
		policy := familyPolicy(strategy, family)
		update, err := sp.EncodeCompactPolicyUpdate(policy.Account, m.delegate, 0, constraints)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(update)
		families = append(families, expected{family, policy, constraints, hex.EncodeToString(digest[:])})
		addresses = append(addresses, policy.Account.String())
	}
	response, err := readAtEventSlot(ctx, m.rpc, addresses, slot, waitBehind)
	if err != nil {
		return err
	}
	var accounts, basis []map[string]any
	matched, present := 0, 0
	for index, family := range families {
		account := response.Accounts[index]
		entry := map[string]any{"family": string(family.family), "seed": family.policy.Seed, "account": family.policy.Account.String(), "semanticSha256": family.semantic}
		basis = append(basis, entry)
		state := map[string]any{"family": string(family.family), "seed": family.policy.Seed, "account": family.policy.Account.String(), "semanticSha256": family.semantic, "dataSha256": nil, "exists": false, "matches": false}
		if account != nil {
			present++
			matches := false
			if account.Owner == sp.Program.String() && !account.Executable {
				if matches, err = multiply.CurrentPolicyMatches(account.Data, family.policy, m.delegate, family.constraints, 0); err != nil {
					return err
				}
			}
			if matches {
				matched++
			}
			digest := sha256.Sum256(account.Data)
			state["dataSha256"], state["exists"], state["matches"] = hex.EncodeToString(digest[:]), true, matches
		}
		accounts = append(accounts, state)
	}
	manifest, err := json.Marshal(map[string]any{"version": multiply.ManifestVersion, "settings": settings.String(), "vaultIndex": topology.VaultIndex, "vault": topology.Vault.String(), "policies": basis})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(manifest)
	status := "incomplete"
	if matched == len(addresses) {
		status = "ready"
	} else if present == 0 {
		status = "removed"
	}
	policyAccounts, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	return m.store.ProjectEarnMaxPolicySet(ctx, PolicyProjectionConsumer, EarnMaxPolicySetProjectionInput{
		Settings: settings.String(), VaultIndex: topology.VaultIndex, Vault: topology.Vault.String(), ManifestVersion: multiply.ManifestVersion,
		ManifestSHA256: hex.EncodeToString(digest[:]), PolicySeedBase: base, Status: status, PolicyAccounts: policyAccounts,
		ObservedSignature: signature, ObservedSlot: slot, ObservedAt: time.Now().UTC(),
	})
}

// EarnMaxPolicySeedBase is load_earn_max_policy_seed_base.
func (s *Store) EarnMaxPolicySeedBase(ctx context.Context, settings string, vaultIndex uint8) (uint64, bool, error) {
	var value int64
	err := s.pool.QueryRow(ctx, `
            SELECT policy_seed_base
            FROM loyal_yield.earn_max_policy_sets
            WHERE settings = $1 AND vault_index = $2
            LIMIT 1`, settings, int16(vaultIndex)).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if value < 0 {
		return 0, false, errors.New("Earn MAX policy seed base is negative")
	}
	return uint64(value), true, nil
}

func isLowerHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ProjectEarnMaxPolicySet is project_earn_max_policy_set: the slot-ordered
// manifest row, the terminal route roll on ready, and the projection offset
// commit in one transaction.
func (s *Store) ProjectEarnMaxPolicySet(ctx context.Context, consumer string, input EarnMaxPolicySetProjectionInput) error {
	if input.Settings == "" || input.Vault == "" || input.ManifestVersion == "" || input.ObservedSignature == "" || input.PolicySeedBase == 0 ||
		(input.Status != "incomplete" && input.Status != "ready" && input.Status != "removed") || !isLowerHex64(input.ManifestSHA256) ||
		len(input.PolicyAccounts) == 0 || input.PolicyAccounts[0] != '[' {
		return errors.New("Earn MAX policy projection is malformed")
	}
	slot, err := slotBigint(input.ObservedSlot)
	if err != nil {
		return err
	}
	seedBase, err := policySeedBigint(input.PolicySeedBase)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		cursor, err := lockProjectionOffset(ctx, tx, consumer)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO loyal_yield.earn_max_policy_sets (
                settings, vault_index, vault, manifest_version, manifest_sha256,
                policy_seed_base, status, policy_accounts, observed_signature,
                observed_slot, observed_at, updated_at
            ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
            ON CONFLICT (settings, vault_index) DO UPDATE SET
                vault = EXCLUDED.vault,
                manifest_version = EXCLUDED.manifest_version,
                manifest_sha256 = EXCLUDED.manifest_sha256,
                policy_seed_base = EXCLUDED.policy_seed_base,
                status = EXCLUDED.status,
                policy_accounts = EXCLUDED.policy_accounts,
                observed_signature = EXCLUDED.observed_signature,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                updated_at = now()
            WHERE EXCLUDED.observed_slot >= loyal_yield.earn_max_policy_sets.observed_slot`,
			input.Settings, int16(input.VaultIndex), input.Vault, input.ManifestVersion, input.ManifestSHA256, seedBase, input.Status,
			[]byte(input.PolicyAccounts), input.ObservedSignature, slot, input.ObservedAt); err != nil {
			return err
		}
		if input.Status == "ready" {
			if err := multiply.ProjectReadyPolicySetInTx(ctx, tx, input.Settings, input.VaultIndex, input.PolicySeedBase, input.ObservedSlot, input.ObservedAt); err != nil {
				return err
			}
		}
		if slot > cursor {
			_, err = tx.Exec(ctx, `
                UPDATE loyal_yield.projection_offsets
                SET last_event_id = $2, updated_at = now()
                WHERE consumer_name = $1`, consumer, slot)
		}
		return err
	})
}

func lockProjectionOffset(ctx context.Context, tx pgx.Tx, consumer string) (int64, error) {
	var last, locked int64
	if err := tx.QueryRow(ctx, `
        INSERT INTO loyal_yield.projection_offsets (consumer_name, last_event_id)
        VALUES ($1, 0)
        ON CONFLICT (consumer_name) DO UPDATE
        SET consumer_name = EXCLUDED.consumer_name
        RETURNING last_event_id`, consumer).Scan(&last); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `
        SELECT last_event_id
        FROM loyal_yield.projection_offsets
        WHERE consumer_name = $1
        FOR UPDATE`, consumer).Scan(&locked); err != nil {
		return 0, err
	}
	return max(locked, last), nil
}
