package backyardrwa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// UnwindIntentCommitRequest is the operator input for the bounded exit proof.
// BudgetScope and BudgetFamily are derived from the fixed production budget
// goal and the source lane, so the command cannot promise a different budget
// identity than the one the commit itself enforces.
type UnwindIntentCommitRequest struct {
	Lane             string
	Reason           string
	ObservationID    string
	MaxCollateralRaw int64
	MaxDebtRaw       int64
	CostBoundRaw     int64
	EvidenceID       string
}

// UnwindIntentCommitResult prints the exact intent a dry-run validated or an
// execute run committed.
type UnwindIntentCommitResult struct {
	DryRun bool         `json:"dryRun"`
	Intent UnwindIntent `json:"intent"`
}

// ErrUnwindIntentCommittedReleaseUnconfirmed reports that the intent IS
// durably committed and only the command's own short lease release could not
// be confirmed. The caller must not treat the intent as unsent or re-execute
// with new values.
var ErrUnwindIntentCommittedReleaseUnconfirmed = errors.New("unwind_intent_committed_lease_release_unconfirmed")

func unwindIntentFromRequest(req UnwindIntentCommitRequest, now time.Time) UnwindIntent {
	return UnwindIntent{
		SourceLane:       req.Lane,
		Reason:           req.Reason,
		ObservationID:    req.ObservationID,
		MaxCollateralRaw: req.MaxCollateralRaw,
		MaxDebtRaw:       req.MaxDebtRaw,
		CostBoundRaw:     req.CostBoundRaw,
		BudgetScope:      Phase3GoalID,
		BudgetFamily:     phase3BudgetFamilyForLane(req.Lane),
		EvidenceID:       req.EvidenceID,
		CreatedAt:        now,
	}
}

// RunUnwindIntentCommit is the operator exit seam: it commits the existing
// guarded unwind intent without any continuous worker. The default is a
// dry-run that validates the exact intent shape and the installed embedded
// manifest's source-lane authority and prints what execute would commit; it
// opens no database connection. Execute acquires its own short route lease and
// commits through CommitUnwindIntentOnManifest, so every fence stays shared
// verbatim: an already funded exit reservation, no nonterminal transaction, no
// unresolved capital recovery, and the reviewed binding's lane authority —
// which admits the candidate AUTO source only while the installed manifest
// carries it.
func RunUnwindIntentCommit(ctx context.Context, databaseURL string, req UnwindIntentCommitRequest, execute bool) (UnwindIntentCommitResult, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return UnwindIntentCommitResult{}, err
	}
	return runUnwindIntentCommitOnManifest(ctx, manifest, databaseURL, productionRouteKey, req, execute)
}

// runUnwindIntentCommitOnManifest is the identical command with the manifest
// and route row injectable, so tests can drive the exact command through a
// reviewed candidate binding without modifying the installed embedded form.
// The public wrapper keeps the embedded authority.
func runUnwindIntentCommitOnManifest(ctx context.Context, manifest RouteManifest, databaseURL, routeKey string, req UnwindIntentCommitRequest, execute bool) (result UnwindIntentCommitResult, err error) {
	intent := unwindIntentFromRequest(req, time.Now().UTC())
	if err = manifest.validateUnwindIntent(intent); err != nil {
		return UnwindIntentCommitResult{}, err
	}
	if !execute {
		return UnwindIntentCommitResult{DryRun: true, Intent: intent}, nil
	}
	if databaseURL == "" {
		return UnwindIntentCommitResult{}, budgetHold("invalid_unwind_intent_config")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		return UnwindIntentCommitResult{}, budgetHold("unwind_intent_database_unavailable")
	}
	defer db.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return UnwindIntentCommitResult{}, budgetHold("unwind_intent_owner_unavailable")
	}
	if _, err = db.AcquireRouteLease(ctx, routeKey, "unwind-intent:"+hex.EncodeToString(nonce[:]), 45*time.Second); err != nil {
		return UnwindIntentCommitResult{}, budgetHold("unwind_intent_lease_unavailable")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		released, releaseErr := db.ReleaseRouteLease(releaseCtx)
		// The named result carries the committed intent past this cleanup: a
		// release failure must stay visible without implying the commit is
		// missing or unsent.
		if err == nil && (releaseErr != nil || !released) {
			err = ErrUnwindIntentCommittedReleaseUnconfirmed
		}
	}()
	if err = db.CommitUnwindIntentOnManifest(ctx, manifest, routeKey, intent); err != nil {
		var hold *BudgetHold
		if errors.As(err, &hold) {
			return UnwindIntentCommitResult{}, hold
		}
		// SQL failures may embed credentials. Emit only a fixed stage error.
		return UnwindIntentCommitResult{}, budgetHold("unwind_intent_commit_unavailable")
	}
	result = UnwindIntentCommitResult{DryRun: false, Intent: intent}
	return result, nil
}
