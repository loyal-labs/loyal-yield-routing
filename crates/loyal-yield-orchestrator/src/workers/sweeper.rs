use chrono::{DateTime, Utc};

use crate::pipeline::{LeaseState, QueueStatus, SweepAction};

#[derive(Debug, Default, Clone, Copy)]
pub struct SweeperWorker;

impl SweeperWorker {
    pub fn lease_action(now: DateTime<Utc>, lease: &LeaseState) -> SweepAction {
        if lease.attempt_count >= lease.max_attempts {
            return SweepAction::DeadLetter;
        }

        match (lease.status, lease.lease_expires_at) {
            (QueueStatus::Leased, Some(expires_at)) if expires_at <= now => {
                SweepAction::ReleaseLease
            }
            _ => SweepAction::Keep,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::{Duration, TimeZone};

    #[test]
    fn sweeper_releases_expired_leases() {
        let now = Utc.timestamp_opt(1_700_000_000, 0).unwrap();
        let lease = LeaseState {
            status: QueueStatus::Leased,
            lease_expires_at: Some(now - Duration::seconds(1)),
            attempt_count: 1,
            max_attempts: 3,
        };

        assert_eq!(
            SweeperWorker::lease_action(now, &lease),
            SweepAction::ReleaseLease
        );
    }

    #[test]
    fn sweeper_dead_letters_after_retry_budget() {
        let now = Utc.timestamp_opt(1_700_000_000, 0).unwrap();
        let lease = LeaseState {
            status: QueueStatus::Leased,
            lease_expires_at: Some(now - Duration::seconds(1)),
            attempt_count: 3,
            max_attempts: 3,
        };

        assert_eq!(
            SweeperWorker::lease_action(now, &lease),
            SweepAction::DeadLetter
        );
    }
}
