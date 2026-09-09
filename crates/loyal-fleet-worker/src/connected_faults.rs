//! Test-only one-shot interruption at the real signed same-mint handoff.
//! Compiled out of every production binary. The owning connected test consumes
//! the actual prepared input to probe durable stale-owner rejection.
use super::*;
use std::sync::Mutex;

pub(crate) struct SameMintPreparedCrash {
    pub input: SameMintRebalanceInput,
    pub lease: RebalanceOpportunityLease,
    pub capacity: TargetCapacityReservationInput,
    pub submission: SignedRouteSubmissionInput,
}

static ARMED: Mutex<Option<i64>> = Mutex::new(None);
static CAPTURED: Mutex<Option<SameMintPreparedCrash>> = Mutex::new(None);

pub(crate) fn arm(opportunity_id: i64) {
    assert!(CAPTURED.lock().unwrap().is_none());
    assert!(ARMED.lock().unwrap().replace(opportunity_id).is_none());
}

pub(crate) fn interrupt(
    input: &SameMintRebalanceInput,
    handoff: &QueueSignedRouteHandoff,
    capacity: &TargetCapacityReservationInput,
) -> bool {
    let mut armed = ARMED.lock().unwrap();
    if *armed != Some(handoff.lease.opportunity.id) {
        return false;
    }
    *armed = None;
    *CAPTURED.lock().unwrap() = Some(SameMintPreparedCrash {
        input: input.clone(),
        lease: handoff.lease.clone(),
        capacity: capacity.clone(),
        submission: handoff.submission.clone(),
    });
    true
}

pub(crate) fn take() -> Option<SameMintPreparedCrash> {
    CAPTURED.lock().unwrap().take()
}
