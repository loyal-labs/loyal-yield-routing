package autodeposit

import (
	"context"
	"errors"
	"log"
	"time"
)

// DesiredReconciler owns off-chain intent application. Its request generation
// is separate from chain slots and setup generations; it never signs a packet.
type DesiredReconciler struct {
	Store                       *Store
	Reader                      ControlReader
	Artifacts                   *ArtifactProofReader
	RuntimeChain                ConfirmedSlotReader
	OnError                     func(error)
	PollInterval, LeaseDuration time.Duration
	runtimeReporter             func(bool, uint64)
}

func (r *DesiredReconciler) SetRuntimeReporter(report func(bool, uint64)) { r.runtimeReporter = report }

func (r *DesiredReconciler) Tick(ctx context.Context) (bool, error) {
	if r.Store == nil || r.Reader == nil || r.Artifacts == nil {
		return false, errors.New("desired reconciliation requires store, control reader and artifact proof reader")
	}
	if err := r.Store.enqueueSchedulingEligibilityChanges(ctx, 100); err != nil {
		return false, err
	}
	lease := r.LeaseDuration
	if lease <= 0 {
		lease = 120 * time.Second
	}
	if lease < time.Second {
		return false, errors.New("desired reconciliation lease shorter than one second")
	}
	owner, err := newClaimToken()
	if err != nil {
		return false, err
	}
	request, err := r.Store.claimDesiredRequest(ctx, owner, int64(lease/time.Second))
	if err != nil || request == nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, lease)
	defer cancel()
	target, err := r.Store.loadDesiredTarget(work, request.TargetID)
	var observation ControlObservation
	var proofs []VerifiedArtifactCreationProof
	var observedAt time.Time
	if err == nil && target.Active && target.ChainStatus == "closed" && target.ChainSlot <= 0 {
		err = errRuntimeProofUnavailable
	}
	if err == nil && target.Active && target.ChainStatus != "closed" {
		if target.Floor == nil || *target.Floor < 0 || !target.Eligible || target.Artifact == nil {
			err = errRuntimeProofUnavailable
		} else {
			observation, err = r.Reader.ObserveControl(work, target.Artifact.ControlTarget, target.MinimumSlot)
			observedAt = time.Now().UTC() // RPC read clock, not chain block time.
			if err == nil && observation.status() != "active" {
				err = errRuntimeProofUnavailable
			}
			if err == nil {
				for _, role := range []ArtifactRole{ArtifactPolicy, ArtifactDelegation} {
					if role == ArtifactPolicy && target.Artifact.PolicySignature != nil && target.Artifact.PolicyConfirmedSlot != nil || role == ArtifactDelegation && target.Artifact.DelegationSignature != nil && target.Artifact.DelegationConfirmedSlot != nil {
						continue
					}
					var proof VerifiedArtifactCreationProof
					proof, err = r.Artifacts.FindCreationProof(work, *target.Artifact, role, target.MinimumSlot)
					if err != nil {
						break
					}
					proofs = append(proofs, proof)
				}
			}
		}
	}
	if err == nil {
		err = r.Store.applyDesiredObservation(work, *request, owner, *target, observation, observedAt, proofs)
	}
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		cleanup, done := context.WithTimeout(ctx, runtimeHealthTimeout)
		defer done()
		retry := r.Store.retryDesiredRequest(cleanup, *request, owner)
		return true, errors.Join(err, retry)
	}
	return true, nil
}

func (r *DesiredReconciler) Run(ctx context.Context) error {
	if r.runtimeReporter != nil {
		r.runtimeReporter(false, 0)
		defer r.runtimeReporter(false, 0)
	}
	if r.Store == nil || r.Reader == nil || r.Artifacts == nil || (r.LeaseDuration > 0 && r.LeaseDuration < time.Second) {
		return errors.New("desired runtime dependencies or lease invalid")
	}
	if err := r.Store.RequireDesiredSchema(ctx); err != nil {
		return err
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, runtimeCycleTimeout)
		_, err := r.Tick(cycle)
		var slot uint64
		if err == nil && r.runtimeReporter != nil {
			slot, err = r.Store.desiredRuntimeHealth(cycle, r.RuntimeChain)
		}
		cancel()
		if r.runtimeReporter != nil {
			r.runtimeReporter(err == nil && slot > 0 && ctx.Err() == nil, slot)
		}
		if err != nil && ctx.Err() == nil {
			log.Print("autodeposit desired_control_proof_unavailable")
			if r.OnError != nil {
				r.OnError(errRuntimeProofUnavailable)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
