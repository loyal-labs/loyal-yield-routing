use std::time::Duration;

use crate::pipeline::{SubmitAction, SubmitObservation};

#[derive(Debug, Clone, Copy)]
pub struct SubmitWorker {
    base_backoff: Duration,
}

impl SubmitWorker {
    pub fn new(base_backoff: Duration) -> Self {
        Self { base_backoff }
    }

    pub fn next_action(
        &self,
        observation: SubmitObservation,
        blockhash_still_valid: bool,
    ) -> SubmitAction {
        match observation {
            SubmitObservation::Accepted | SubmitObservation::Unknown if blockhash_still_valid => {
                SubmitAction::Broadcast
            }
            SubmitObservation::RateLimited if blockhash_still_valid => {
                SubmitAction::Backoff(self.base_backoff)
            }
            SubmitObservation::Accepted
            | SubmitObservation::Unknown
            | SubmitObservation::RateLimited => SubmitAction::ExpireAndReconcile,
            SubmitObservation::Fatal(reason) => SubmitAction::Fail(reason),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn submit_worker_rebroadcasts_unknown_valid_transaction() {
        let worker = SubmitWorker::new(Duration::from_millis(250));

        assert_eq!(
            worker.next_action(SubmitObservation::Unknown, true),
            SubmitAction::Broadcast
        );
    }

    #[test]
    fn submit_worker_expires_after_blockhash_window() {
        let worker = SubmitWorker::new(Duration::from_millis(250));

        assert_eq!(
            worker.next_action(SubmitObservation::Unknown, false),
            SubmitAction::ExpireAndReconcile
        );
    }

    #[test]
    fn submit_worker_treats_429_as_backpressure() {
        let worker = SubmitWorker::new(Duration::from_millis(250));

        assert_eq!(
            worker.next_action(SubmitObservation::RateLimited, true),
            SubmitAction::Backoff(Duration::from_millis(250))
        );
    }
}
