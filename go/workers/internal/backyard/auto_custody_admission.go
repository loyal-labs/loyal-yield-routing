package backyard

// Binding of the durable shared-custody attribution proof
// (auto_custody_attribution.go) to the exact lifecycle phase, observation,
// lease and generation a spend decision is made under.
//
// PRODUCTION LIFECYCLE (Worker.Tick new-operation path) AND WHERE CUSTODY
// PROOF BINDS — the persisted state differs at every phase, so the API is
// phase-specific:
//
//  1. PRE-DECISION (after prepare* constructs the execution evidence with
//     its expected effects, BEFORE RecordDecision creates the row): the row
//     does not exist yet, so the ownership proof is STRICT — there is no
//     current operation to exclude and none may exist (any nonterminal row
//     holds). Call ObserveSharedCustodyOwnershipProof with the PREPARED
//     evidence's expected effects; the exact shared-custody debit gates the
//     probe (zero-spend operations such as ReportNAV skip even at a positive
//     balance; a positive spend is always probed, including at zero observed
//     balance, and then refused). The proof CARRIES Generation +
//     LeaseOwner/LeaseFencing + Digest.
//  2. BIND (bindOperation, right after RecordDecision): under the route lock
//     bindSharedCustodyAdmissionProofOnManifest re-validates
//     proof.BindsGeneration(generation, fence) and the exact prepared effects,
//     and the binding is recorded with the operation's intent. The build
//     uses that same in-memory evidence in the same tick; a crash before the
//     wire is signed fails the operation, it is never rebuilt.
//  3. PRE-BROADCAST SEND (Signed branch, CheckAndMarkBroadcastIntentOnManifest):
//     by now MarkBuilt has persisted the built expected effects and
//     PersistSignedUpdate the wire, digest AND signature.
//     ObserveSharedCustodySendProof re-runs the FULL custody walk and excludes
//     exactly the current signed operation, whose persisted row must carry the
//     claimed wire identity, the pinned delegate signature, persisted effects
//     equal to the caller's decoded built effects, and a positive
//     shared-custody debit. Decided/built/simulated rows are NEVER excludable
//     — a fresh proof for them happened in phase 1 before the row existed.
//
// MANIFEST: pass the SAME explicit reviewed manifest the calling lifecycle
// already threads. Both proofs run the planning read through
// readRoutePlanningStateOnManifest so a persisted candidate AUTO selector
// entry decodes under the same manifest that admits the candidate lane —
// never through the embedded manifest, and never by skipping entry
// validation.
//
// BINDING HONESTY: the planning read and the journal snapshot are two reads;
// neither proof claims one atomic unit across the RPC observation, the
// planning state and the journal. What they guarantee: the route lease was
// current for THIS database at proof time; the proof is deterministic over
// persisted rows (restart-identical); and Generation + lease fence are
// CARRIED so the locked bind validates BindsGeneration against the lock it
// actually holds. The phase-1 → phase-3 window is bounded by the send
// re-proof.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// sharedCustodyProofBinding is the durable, comparable form of a proof: what
// the bind persists into the per-operation phase3 record.
type sharedCustodyProofBinding struct {
	RouteKey      string `json:"routeKey"`
	Lane          string `json:"lane"`
	Custody       string `json:"custody"`
	Digest        string `json:"digest"`
	EffectsSHA256 string `json:"effectsSha256"`
	SpendRaw      uint64 `json:"spendRaw"`
	ObservedRaw   uint64 `json:"observedRaw"`
	ObservedSlot  int64  `json:"observedSlot"`
	Generation    int64  `json:"generation"`
	LeaseOwner    string `json:"leaseOwner"`
	LeaseFencing  int64  `json:"leaseFencing"`
}

func sharedCustodyProofBindingFrom(p sharedCustodyAdmissionProof) sharedCustodyProofBinding {
	return sharedCustodyProofBinding{
		RouteKey: p.RouteKey, Lane: p.Lane, Custody: p.Custody, Digest: p.Digest,
		EffectsSHA256: p.EffectsSHA256, SpendRaw: p.SpendRaw, ObservedRaw: p.Proof.ObservedRaw,
		ObservedSlot: p.ObservedSlot, Generation: p.Generation, LeaseOwner: p.LeaseOwner, LeaseFencing: p.LeaseFencing,
	}
}

// valid re-checks the persisted binding on its own: positive spend, a digest,
// a named lease owner, and a fenced generation — the shape BindsGeneration
// enforces on a live proof.
func (b sharedCustodyProofBinding) valid() bool {
	return b.SpendRaw > 0 && b.LeaseFencing != 0 && b.LeaseOwner != "" && b.Digest != "" &&
		sha256Pattern.MatchString(b.Digest) && sha256Pattern.MatchString(b.EffectsSHA256)
}

// bindsGeneration mirrors sharedCustodyAdmissionProof.BindsGeneration for the
// persisted form.
func (b sharedCustodyProofBinding) bindsGeneration(generation int64) bool {
	return b.SpendRaw > 0 && b.Generation == generation && b.LeaseFencing != 0
}

// expectedEffectsSHA256 binds a proof to the EXACT prepared/built effects
// bytes through the same deterministic encoder the store persists.
func expectedEffectsSHA256(effects ExpectedEffects) (string, error) {
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return "", err
	}
	return sha256Bytes(encoded), nil
}

// autoSharedCustodySpend resolves the shared-custody gate for one operation:
// only the candidate AUTO-PYUSD lane's POSITIVE custody debit applies. Every
// other lane — and any AUTO zero-spend operation — keeps installed behavior.
func autoSharedCustodySpend(lane, routeKey string, effects ExpectedEffects) (sharedCustodyAttributionConfig, uint64, bool) {
	if lane != autoAUTOPYUSD.Lane {
		return sharedCustodyAttributionConfig{}, 0, false
	}
	cfg := autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, routeKey)
	spend := sharedCustodySpendRaw(effects, cfg)
	return cfg, spend, spend > 0
}

// sharedCustodyAdmissionProof is the durable result carried to the locked
// bind and taken again at the send fence. SpendRaw == 0
// means the operation spends no shared custody and NO proof was taken (all
// other fields are zero).
type sharedCustodyAdmissionProof struct {
	Proof        sharedCustodyProof
	RouteKey     string
	Lane         string
	Custody      string
	Mint         string
	ObservedSlot int64
	SpendRaw     uint64
	// Generation and the lease fence are the planning-state values observed
	// at proof time, for BindsGeneration validation at the locked bind. They
	// are NOT claimed atomic with the journal snapshot or
	// the RPC observation.
	Generation   int64
	LeaseOwner   string
	LeaseFencing int64
	// ExcludedOperation is the one in-flight operation the reader validated
	// against its persisted row and excluded from the unresolved gate. Empty
	// for the pre-decision ownership proof (nothing exists to exclude).
	ExcludedOperation string
	// EffectsSHA256 binds this proof to the EXACT expected effects it was
	// taken over (deterministic store encoding), so the locked bind and the
	// send fence can refuse a proof taken over different effects.
	EffectsSHA256 string
	// Digest binds this proof (chain, observation bound, spend, fence, and
	// exclusion) for audit at the locked persistence.
	Digest string
}

// BindsGeneration reports whether this proof was taken under the named
// generation and lease fencing token. A mismatch at the locked bind or the
// send fence refuses the operation: the proof describes a
// decision made under a different lock.
func (p sharedCustodyAdmissionProof) BindsGeneration(generation int64, fencingToken int64) bool {
	return p.SpendRaw > 0 && p.Generation == generation && p.LeaseFencing == fencingToken && p.LeaseFencing != 0
}

// sharedCustodySignedSpend is the caller's send-phase claim that exactly one
// signed operation is the custody spend about to be broadcast: its exact
// persisted wire digest and signature. NEVER trusted — the reader validates
// the persisted row inside its own snapshot.
type sharedCustodySignedSpend struct {
	OperationID          string
	SignedWireSHA256     string
	TransactionSignature string
}

// ObserveSharedCustodyOwnershipProof proves — BEFORE the decision is
// recorded, while no operation row exists — that the freshly observed
// shared-custody residue belongs to cfg.Lane under the lease and generation
// the tick holds. expected is the PREPARED execution evidence's expected
// effects: the probe runs only when they positively debit the pinned
// custody. Strict gates: any nonterminal row on the route holds. This
// signature-stable form carries no RPC, so unknown terminal records can only
// be classified by their exact NAV wire bytes — the historical-expiry
// classification fails closed.
func (d *Database) ObserveSharedCustodyOwnershipProof(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64) (sharedCustodyAdmissionProof, error) {
	return d.ObserveSharedCustodyOwnershipProofWithRPC(ctx, manifest, cfg, expected, observedRaw, observedSlot, nil)
}

// ObserveSharedCustodyOwnershipProofWithRPC is the ownership proof with the
// caller's explicit RPC client. The client is used ONLY to resolve the
// proven zero-origin slot's finalized block height when unknown terminal
// records need the historical-expiry classification; it never reads
// environment or global config inside the database helper. nil keeps every
// non-RPC classification and fails closed on the expiry path.
func (d *Database) ObserveSharedCustodyOwnershipProofWithRPC(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64, rpc *chain.Client) (sharedCustodyAdmissionProof, error) {
	return d.observeSharedCustodySpendProof(ctx, manifest, cfg, expected, observedRaw, observedSlot, nil, sharedCustodyOriginHeightResolver(rpc))
}

// ObserveSharedCustodySendProof re-proves ownership at the pre-broadcast
// send fence. expected must be the operation's PERSISTED DECODED built
// effects (MarkBuilt wrote them); signed must be the exact persisted wire
// identity (PersistSignedUpdate wrote the signature together with the wire,
// before broadcast). The signed row is the one validated exclusion from the
// unresolved gate; every other state is refused, never exempted.
func (d *Database) ObserveSharedCustodySendProof(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64, signed sharedCustodySignedSpend) (sharedCustodyAdmissionProof, error) {
	return d.ObserveSharedCustodySendProofWithRPC(ctx, manifest, cfg, expected, observedRaw, observedSlot, signed, nil)
}

// ObserveSharedCustodySendProofWithRPC is the send proof with the caller's
// explicit RPC client for the origin block-height resolution, same contract
// as ObserveSharedCustodyOwnershipProofWithRPC.
func (d *Database) ObserveSharedCustodySendProofWithRPC(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64, signed sharedCustodySignedSpend, rpc *chain.Client) (sharedCustodyAdmissionProof, error) {
	if signed.OperationID == "" || signed.SignedWireSHA256 == "" || signed.TransactionSignature == "" {
		return sharedCustodyAdmissionProof{}, budgetHold("custody_attribution_current_operation_invalid")
	}
	return d.observeSharedCustodySpendProof(ctx, manifest, cfg, expected, observedRaw, observedSlot, &sharedCustodyCurrentOperation{
		OperationID: signed.OperationID, SignedWireSHA256: signed.SignedWireSHA256,
		TransactionSignature: signed.TransactionSignature, ExpectedEffects: expected,
	}, sharedCustodyOriginHeightResolver(rpc))
}

// sharedCustodyOriginHeightResolver adapts the explicit RPC client into the
// validator's origin block-height resolver: the finalized block height
// containing the proven zero-origin slot. nil yields nil — the expiry
// classification then holds fail-closed instead of silently passing.
func sharedCustodyOriginHeightResolver(rpc *chain.Client) func(context.Context, int64) (int64, error) {
	if rpc == nil {
		return nil
	}
	return func(ctx context.Context, slot int64) (int64, error) {
		start := time.Now()
		defer logStage("custody_proof_origin_height", start)
		if slot <= 0 {
			return 0, fmt.Errorf("slot is required")
		}
		height, err := rpc.FinalizedBlockHeightAt(ctx, uint64(slot))
		if err != nil {
			return 0, confirmedObservationUnavailable(err)
		}
		return int64(height), nil
	}
}

// sharedCustodyProofInputs is the journal side of a proof: the planning read
// (lease, generation) and the attribution evidence snapshot. It needs no
// custody balance, so it can be read while the balance's slot is still being
// fixed; finishSharedCustodySpendProof then binds both.
type sharedCustodyProofInputs struct {
	spend    uint64
	planning *routePlanningState
	probe    *sharedCustodyCurrentOperation
	evidence sharedCustodyAttributionEvidence
}

// observeSharedCustodySpendProof is the shared proof core: planning read
// under the caller's manifest, validated optional exclusion, journal walk,
// observation binding, intent-prestate binding, and the carried
// generation/fence.
func (d *Database) observeSharedCustodySpendProof(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64, current *sharedCustodyCurrentOperation, originBlockHeight func(context.Context, int64) (int64, error)) (sharedCustodyAdmissionProof, error) {
	inputs, err := d.gatherSharedCustodyProofInputs(ctx, manifest, cfg, expected, current)
	if err != nil || inputs.spend == 0 {
		return sharedCustodyAdmissionProof{}, err
	}
	return finishSharedCustodySpendProof(ctx, cfg, expected, inputs, observedRaw, observedSlot, originBlockHeight)
}

func (d *Database) gatherSharedCustodyProofInputs(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, current *sharedCustodyCurrentOperation) (sharedCustodyProofInputs, error) {
	spend := sharedCustodySpendRaw(expected, cfg)
	if spend == 0 {
		// The operation spends no shared custody: the attribution gate does
		// not apply, and unrelated recovery must not be blocked by a positive
		// balance alone.
		return sharedCustodyProofInputs{}, nil
	}
	var probe *sharedCustodyCurrentOperation
	if current != nil {
		claimed := *current
		claimed.ExpectedEffects = expected
		probe = &claimed
	}
	inputs, err := d.readSharedCustodyJournal(ctx, manifest, cfg, probe)
	inputs.spend = spend
	return inputs, err
}

// readSharedCustodyJournal is the journal side of a proof. It needs only the
// lease and the custody identity, so the pre-decision proof may read it while
// the spend is still being prepared.
func (d *Database) readSharedCustodyJournal(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, probe *sharedCustodyCurrentOperation) (sharedCustodyProofInputs, error) {
	var inputs sharedCustodyProofInputs
	if cfg.RouteKey == "" || cfg.Lane == "" {
		return inputs, fmt.Errorf("shared custody attribution config is incomplete")
	}
	// Planning read at proof time through the caller's explicit reviewed
	// manifest: refuses without THIS database's current lease on the route,
	// decodes the persisted selector entry under the SAME manifest the
	// caller's lifecycle uses (a candidate AUTO entry is rejected by the
	// embedded manifest's decode), and yields the generation + fence the
	// proof carries.
	proofStart := time.Now()
	planning, err := d.readRoutePlanningStateOnManifest(ctx, manifest, cfg.RouteKey, true)
	logStage("custody_proof_planning", proofStart)
	if err != nil {
		return inputs, err
	}
	if planning.lease == nil {
		return inputs, budgetHold("custody_attribution_lease_unavailable")
	}
	inputs.planning, inputs.probe = planning, probe
	inputs.evidence, err = d.observeSharedCustodyAttributionEvidence(ctx, *planning.lease, cfg, 0, probe)
	logStage("custody_proof_evidence", proofStart)
	return inputs, err
}

// prefetchSharedCustodyOwnershipProof starts the pre-decision journal read
// and returns the finisher that binds it to the prepared spend and the
// prepared observation. The proof is the same strict proof: a journal change
// after the read fails the balance/tip binding here or the generation check
// under the bind lock.
func (d *Database) prefetchSharedCustodyOwnershipProof(ctx context.Context, manifest RouteManifest, cfg sharedCustodyAttributionConfig, rpc *chain.Client) custodyProofFinisher {
	var inputs sharedCustodyProofInputs
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		inputs, readErr = d.readSharedCustodyJournal(ctx, manifest, cfg, nil)
	}()
	resolver := sharedCustodyOriginHeightResolver(rpc)
	return func(ctx context.Context, got sharedCustodyAttributionConfig, expected ExpectedEffects, observedRaw uint64, observedSlot int64) (sharedCustodyAdmissionProof, error) {
		<-done
		return finishPrefetchedOwnershipProof(ctx, cfg, got, expected, inputs, readErr, observedRaw, observedSlot, resolver)
	}
}

// finishPrefetchedOwnershipProof binds a journal read taken before prepare to
// the prepared spend and observation, with the same validator as the serial
// proof.
func finishPrefetchedOwnershipProof(ctx context.Context, cfg, got sharedCustodyAttributionConfig, expected ExpectedEffects, inputs sharedCustodyProofInputs, readErr error, observedRaw uint64, observedSlot int64, resolver func(context.Context, int64) (int64, error)) (sharedCustodyAdmissionProof, error) {
	if readErr != nil {
		return sharedCustodyAdmissionProof{}, readErr
	}
	if got != cfg {
		return sharedCustodyAdmissionProof{}, budgetHold("custody_attribution_proof_drift")
	}
	inputs.spend = sharedCustodySpendRaw(expected, cfg)
	if inputs.spend == 0 {
		return sharedCustodyAdmissionProof{}, nil
	}
	return finishSharedCustodySpendProof(ctx, cfg, expected, inputs, observedRaw, observedSlot, resolver)
}

type custodyProofFinisher func(context.Context, sharedCustodyAttributionConfig, ExpectedEffects, uint64, int64) (sharedCustodyAdmissionProof, error)

// custodyProofPrefetchAction lists the AUTO actions that can debit the shared
// PYUSD custody; only these pay for an early journal read.
func custodyProofPrefetchAction(d Decision, s Snapshot) bool {
	if d.StrategyKey != autoAUTOPYUSD.Lane || s.DebtIdleRaw <= 0 {
		return false
	}
	switch d.Action {
	case DeleverRouteStep, SwapDebtToCollateralStep, SwapDebtToUSDCStep:
		return true
	}
	return false
}

func finishSharedCustodySpendProof(ctx context.Context, cfg sharedCustodyAttributionConfig, expected ExpectedEffects, inputs sharedCustodyProofInputs, observedRaw uint64, observedSlot int64, originBlockHeight func(context.Context, int64) (int64, error)) (sharedCustodyAdmissionProof, error) {
	if inputs.spend == 0 || inputs.planning == nil || inputs.planning.lease == nil || inputs.spend != sharedCustodySpendRaw(expected, cfg) {
		return sharedCustodyAdmissionProof{}, budgetHold("custody_attribution_lease_unavailable")
	}
	validateStart := time.Now()
	proof, err := validateSharedCustodyAttributionResolved(ctx, observedRaw, observedSlot, cfg, inputs.evidence, 0, originBlockHeight)
	logStage("custody_proof_validate", validateStart)
	if err != nil {
		return sharedCustodyAdmissionProof{}, err
	}
	// History binding alone is not enough: the spend intent itself must be
	// written against the custody prestate the observation just measured.
	if err := validateSharedCustodySpendIntent(expected, observedRaw, cfg); err != nil {
		return sharedCustodyAdmissionProof{}, err
	}
	effectsSHA256, err := expectedEffectsSHA256(expected)
	if err != nil {
		return sharedCustodyAdmissionProof{}, err
	}
	planning := inputs.planning
	out := sharedCustodyAdmissionProof{
		Proof: proof, RouteKey: cfg.RouteKey, Lane: cfg.Lane, Custody: cfg.Custody, Mint: cfg.Mint,
		ObservedSlot: observedSlot, SpendRaw: inputs.spend, EffectsSHA256: effectsSHA256,
		Generation: planning.generation, LeaseOwner: planning.lease.Owner, LeaseFencing: planning.lease.FencingToken,
	}
	if inputs.probe != nil {
		out.ExcludedOperation = inputs.probe.OperationID
	}
	out.Digest = sharedCustodyAdmissionDigest(out)
	return out, nil
}

// validateSharedCustodySpendIntent binds the spend intent's custody prestate
// to the FRESH observation the proof is taken under: exactly one pinned
// custody account in the effects, its BeforeRaw equal to observedRaw, and a
// spend that never exceeds that balance. Without this, a caller could prove a
// coherent journal history while spending from an intent written against a
// stale or invented prestate — e.g. expecting 8B where the custody holds 3B.
func validateSharedCustodySpendIntent(expected ExpectedEffects, observedRaw uint64, cfg sharedCustodyAttributionConfig) error {
	matches, before := 0, uint64(0)
	for _, effect := range expected.Accounts {
		if effect.Address != cfg.Custody || effect.Mint != cfg.Mint || effect.Authority != cfg.Authority || effect.Owner != cfg.Owner {
			continue
		}
		matches++
		before += effect.BeforeRaw
	}
	if matches != 1 || before != observedRaw || sharedCustodySpendRaw(expected, cfg) > observedRaw {
		return budgetHold("custody_attribution_intent_prestate_mismatch")
	}
	return nil
}

// sharedCustodyAdmissionDigest binds the proof's complete content — chain
// steps, origin, observation bound, spend, fence, and exclusion — into one
// digest for the locked persistence to record.
func sharedCustodyAdmissionDigest(p sharedCustodyAdmissionProof) string {
	var b strings.Builder
	b.WriteString(p.RouteKey)
	b.WriteByte('|')
	b.WriteString(p.Lane)
	b.WriteByte('|')
	b.WriteString(p.Custody)
	b.WriteByte('|')
	b.WriteString(p.Mint)
	b.WriteByte('|')
	b.WriteString(strconv.FormatUint(p.SpendRaw, 10))
	b.WriteByte('|')
	b.WriteString(p.EffectsSHA256)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(p.ObservedSlot, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatUint(p.Proof.ObservedRaw, 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(p.Generation, 10))
	b.WriteByte('|')
	b.WriteString(p.LeaseOwner)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(p.LeaseFencing, 10))
	b.WriteByte('|')
	b.WriteString(p.ExcludedOperation)
	for _, step := range p.Proof.Steps {
		b.WriteByte('|')
		b.WriteString(step.OperationID)
		b.WriteByte(',')
		b.WriteString(step.Signature)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(step.Slot, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatUint(step.BeforeRaw, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatUint(step.AfterRaw, 10))
		b.WriteByte(',')
		b.WriteString(step.EffectsSHA256)
	}
	b.WriteString("|origin=")
	b.WriteString(p.Proof.Origin.Signature)
	b.WriteByte(',')
	b.WriteString(strconv.FormatInt(p.Proof.Origin.Slot, 10))
	return sha256Bytes([]byte(b.String()))
}

// bindSharedCustodyAdmissionProofOnManifest is the bind seam (doc 26 §2):
// for a POSITIVE prepared AUTO-PYUSD spend it requires the strict
// pre-decision proof carried on the bind's own observation, re-validates it
// against the CURRENT route lock row (generation/fence under FOR UPDATE), and
// returns the durable binding to persist beside the operation's intent. Every
// other lane and any AUTO zero-spend operation returns a zero binding and
// preserves installed behavior; a missing or drifted proof for a real spend
// holds — every operation passes through bindOperation before it is built, so
// there is no alternate-caller bypass. A bound row is never bound again.
func bindSharedCustodyAdmissionProofOnManifest(ctx context.Context, tx pgx.Tx, routeKey, lane string, observation Observation, effects ExpectedEffects) (sharedCustodyProofBinding, error) {
	cfg, _, applies := autoSharedCustodySpend(lane, routeKey, effects)
	if !applies {
		return sharedCustodyProofBinding{}, nil
	}
	proof := observation.carriedCustodyOwnershipProof()
	if proof == nil {
		return sharedCustodyProofBinding{}, budgetHold("custody_attribution_proof_missing")
	}
	// The same route lock RecordDecision uses: generation and lease identity
	// must still be the proof's at authorization time.
	var generation int64
	var owner string
	var fencing int64
	if err := tx.QueryRow(ctx, `SELECT state_version, COALESCE(lease_owner,''), fencing_token FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, routeKey).Scan(&generation, &owner, &fencing); err != nil {
		return sharedCustodyProofBinding{}, err
	}
	if !proof.BindsGeneration(generation, fencing) || proof.LeaseOwner != owner {
		return sharedCustodyProofBinding{}, budgetHold("custody_attribution_generation_drift")
	}
	binding := sharedCustodyProofBindingFrom(*proof)
	if !binding.valid() || binding.EffectsSHA256 != proof.EffectsSHA256 || binding.RouteKey != cfg.RouteKey ||
		binding.Lane != cfg.Lane || binding.Custody != cfg.Custody {
		return sharedCustodyProofBinding{}, budgetHold("custody_attribution_proof_drift")
	}
	// The carried proof must commit to its own content: the digest is
	// recomputed from the full proof exactly as the locked send boundary does.
	if sharedCustodyAdmissionDigest(*proof) != proof.Digest {
		return sharedCustodyProofBinding{}, budgetHold("custody_attribution_proof_drift")
	}
	// The proof must be over the exact prepared effects bound here and the
	// bind's own observation — never merely a positive balance.
	effectsSHA256, err := expectedEffectsSHA256(effects)
	if err != nil {
		return sharedCustodyProofBinding{}, err
	}
	observedRaw := uint64(0)
	if observation.Snapshot.DebtIdleRaw > 0 {
		observedRaw = uint64(observation.Snapshot.DebtIdleRaw)
	}
	if binding.EffectsSHA256 != effectsSHA256 || binding.SpendRaw != sharedCustodySpendRaw(effects, cfg) ||
		proof.ObservedSlot != observation.Snapshot.Slot || proof.Proof.ObservedRaw != observedRaw {
		return sharedCustodyProofBinding{}, budgetHold("custody_attribution_proof_drift")
	}
	return binding, nil
}

// observeConfirmedSharedCustodyRaw is the fresh confirmed custody observation
// for the final-send seam: one confirmed account read at the
// caller's minimum slot, decoded through the shared DecodeTokenCustody with
// the pinned owner program, mint and authority bytes.
func observeConfirmedSharedCustodyRaw(ctx context.Context, rpc *chain.Client, cfg sharedCustodyAttributionConfig, minimumSlot int64) (uint64, int64, error) {
	if rpc == nil || minimumSlot <= 0 {
		return 0, 0, budgetHold("custody_attribution_observation_invalid")
	}
	slot, accounts, err := confirmedAccounts(ctx, rpc, []string{cfg.Custody}, minimumSlot)
	if err != nil {
		return 0, 0, err
	}
	if slot < minimumSlot {
		return 0, 0, budgetHold("custody_attribution_observation_stale")
	}
	account := accountAt(accounts, cfg.Custody)
	// DecodeTokenCustody alone accepts EITHER token program; the attribution
	// config pins the route's exact debt token program, so the live account's
	// owner program must equal it (a missing account decodes to the zero
	// ConfirmedAccount and refuses here), and the account must be a real
	// non-executable data account.
	if cfg.Owner == "" || account.Owner != cfg.Owner {
		return 0, 0, budgetHold("custody_attribution_custody_invalid")
	}
	mint, err := decodeBase58PublicKey(cfg.Mint)
	if err != nil {
		return 0, 0, err
	}
	authority, err := decodeBase58PublicKey(cfg.Authority)
	if err != nil {
		return 0, 0, err
	}
	custody, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
	if err != nil || account.Executable {
		return 0, 0, budgetHold("custody_attribution_custody_invalid")
	}
	return custody.Raw, slot, nil
}

// sharedCustodySendProof splits the final-send proof so its journal read can
// run while the signed wire is revalued. The fresh custody balance is still
// read only in finish, at a slot no older than the revalued cost, and the
// same full walk then binds that balance to the journal snapshot.
type sharedCustodySendProof struct {
	applies bool
	cfg     sharedCustodyAttributionConfig
	inputs  sharedCustodyProofInputs
}

func (d *Database) gatherSharedCustodySendProof(ctx context.Context, manifest RouteManifest, operationID string, decoded ExpectedEffects, signed sharedCustodySignedSpend) (sharedCustodySendProof, error) {
	var routeKey, lane string
	if err := d.pool.QueryRow(ctx, `SELECT route_key, COALESCE(strategy_key,'') FROM loyal_yield.multiply_operations WHERE operation_id=$1`, operationID).Scan(&routeKey, &lane); err != nil {
		return sharedCustodySendProof{}, err
	}
	cfg, _, applies := autoSharedCustodySpend(lane, routeKey, decoded)
	if !applies {
		return sharedCustodySendProof{}, nil
	}
	if signed.OperationID == "" || signed.SignedWireSHA256 == "" || signed.TransactionSignature == "" {
		return sharedCustodySendProof{}, budgetHold("custody_attribution_current_operation_invalid")
	}
	inputs, err := d.gatherSharedCustodyProofInputs(ctx, manifest, cfg, decoded, &sharedCustodyCurrentOperation{
		OperationID: signed.OperationID, SignedWireSHA256: signed.SignedWireSHA256,
		TransactionSignature: signed.TransactionSignature, ExpectedEffects: decoded,
	})
	return sharedCustodySendProof{applies: true, cfg: cfg, inputs: inputs}, err
}

func (p sharedCustodySendProof) finish(ctx context.Context, rpc *chain.Client, decoded ExpectedEffects, minimumSlot int64) (*sharedCustodyAdmissionProof, error) {
	if !p.applies {
		return nil, nil
	}
	balanceStart := time.Now()
	raw, slot, err := observeConfirmedSharedCustodyRaw(ctx, rpc, p.cfg, minimumSlot)
	logStage("final_check_custody_balance", balanceStart)
	if err != nil {
		return nil, err
	}
	proof, err := finishSharedCustodySpendProof(ctx, p.cfg, decoded, p.inputs, raw, slot, sharedCustodyOriginHeightResolver(rpc))
	if err != nil {
		return nil, err
	}
	return &proof, nil
}

// validateSharedCustodySendProofOnBroadcastTx re-validates the fresh send
// proof INSIDE the locked broadcast-intent transaction, against the same
// route row lock (FOR UPDATE) and the PERSISTED built effects — not the
// caller's decode. The persisted effects decode under the SAME reviewed
// manifest the caller threads, so a candidate AUTO initializer (zero PYUSD
// spend) decodes exactly as it did at bind and keeps installed behavior.
// A proof naming another operation, missing, digest-inconsistent, observed
// before the decision's slot, or taken under a stale generation/fence/lease holds before broadcast intent
// is recorded.
func validateSharedCustodySendProofOnBroadcastTx(ctx context.Context, tx pgx.Tx, manifest RouteManifest, operationID string, custody *sharedCustodyAdmissionProof) error {
	var lane, routeKey string
	var effectsBytes []byte
	var generation, decisionSlot int64
	var leaseOwner string
	var fencing int64
	var leaseLive bool
	// lease_expires_at is NULL for a route that has never been leased; the
	// IS TRUE fold keeps the scan total so the zero-spend/other-lane skip
	// below still works there, while a positive spend still requires the
	// lease checks that follow.
	if err := tx.QueryRow(ctx, `SELECT COALESCE(op.strategy_key,''), op.route_key, op.expected_effects, route.state_version,
			COALESCE(route.lease_owner,''), route.fencing_token, (route.lease_expires_at>clock_timestamp()) IS TRUE,
			COALESCE((op.expected_effects->'decision'->>'observationSlot')::bigint,0)
		FROM loyal_yield.multiply_operations op JOIN loyal_yield.multiply_route_states route ON route.route_key=op.route_key
		WHERE op.operation_id=$1 FOR UPDATE OF route`, operationID).Scan(&lane, &routeKey, &effectsBytes, &generation, &leaseOwner, &fencing, &leaseLive, &decisionSlot); err != nil {
		return err
	}
	// Lane applicability first: an installed (non-candidate) lane never
	// decodes persisted effects here — its signed quote-expiry recovery keeps
	// the exact pre-custody behavior, and an unexpected carried proof is
	// refused rather than ignored. Only a candidate AUTO lane reaches the
	// effects decode below, where the persisted built effects are required to
	// decode and bind.
	if lane != autoAUTOPYUSD.Lane {
		if custody != nil {
			return budgetHold("custody_attribution_proof_drift")
		}
		return nil
	}
	decoded, err := decodeExpectedEffectsWithManifest(manifest, effectsBytes)
	if err != nil {
		return err
	}
	cfg, spend, applies := autoSharedCustodySpend(lane, routeKey, decoded)
	if !applies {
		if custody != nil {
			return budgetHold("custody_attribution_proof_drift")
		}
		return nil
	}
	if custody == nil {
		return budgetHold("custody_attribution_proof_missing")
	}
	binding := sharedCustodyProofBindingFrom(*custody)
	effectsSHA256, err := expectedEffectsSHA256(decoded)
	if err != nil {
		return err
	}
	// Self-consistency: the digest is recomputed from the carried content, so
	// a proof cannot pass the field checks below without committing to them.
	if binding.valid() && sharedCustodyAdmissionDigest(*custody) != custody.Digest {
		return budgetHold("custody_attribution_proof_drift")
	}
	// The proof must have been observed under the lease that is CURRENT under
	// this lock — same fencing token and owner, lease unexpired. A proof taken
	// before a lease release/re-acquire or expiry is stale and refuses.
	if !leaseLive {
		return budgetHold("custody_attribution_lease_stale")
	}
	if custody.LeaseOwner != leaseOwner || custody.LeaseFencing != fencing {
		return budgetHold("custody_attribution_generation_drift")
	}
	if !binding.valid() || !binding.bindsGeneration(generation) ||
		// The fresh custody observation is confirmed no earlier than the
		// decision it spends for.
		decisionSlot <= 0 || custody.ObservedSlot < decisionSlot ||
		binding.SpendRaw != spend || binding.EffectsSHA256 != effectsSHA256 ||
		binding.RouteKey != routeKey || binding.Lane != lane || binding.Custody != cfg.Custody ||
		custody.ExcludedOperation != operationID {
		return budgetHold("custody_attribution_proof_drift")
	}
	return nil
}

// observePreDecisionCustodyOwnershipProof is the Worker's pre-decision seam
// (doc 26 §1): for a prepared POSITIVE AUTO-PYUSD spend it runs the strict
// ownership proof under the current route lease and carries it as
// per-operation local data on the observation that goes to RecordDecision and
// the locked bind. Zero-spend and non-AUTO lanes preserve installed
// behavior exactly; the proof is never a mutable global.
func (w *Worker) observePreDecisionCustodyOwnershipProof(ctx context.Context, observation *Observation, decision Decision, effects ExpectedEffects, prefetched custodyProofFinisher) error {
	cfg, _, applies := autoSharedCustodySpend(decision.StrategyKey, w.routeKey, effects)
	if !applies {
		return nil
	}
	if w.runtime.custodyOwnershipProof == nil && prefetched == nil {
		return budgetHold("custody_attribution_ownership_proof_unavailable")
	}
	observedRaw := uint64(0)
	if observation.Snapshot.DebtIdleRaw > 0 {
		observedRaw = uint64(observation.Snapshot.DebtIdleRaw)
	}
	produce := custodyProofFinisher(func(ctx context.Context, cfg sharedCustodyAttributionConfig, effects ExpectedEffects, raw uint64, slot int64) (sharedCustodyAdmissionProof, error) {
		return w.runtime.custodyOwnershipProof(ctx, w.manifest, cfg, effects, raw, slot)
	})
	if prefetched != nil {
		produce = prefetched
	}
	proof, err := produce(ctx, cfg, effects, observedRaw, observation.Snapshot.Slot)
	if err != nil {
		return err
	}
	observation.custodyProof = &proof
	return nil
}
