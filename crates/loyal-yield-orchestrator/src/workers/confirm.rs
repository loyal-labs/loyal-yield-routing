use crate::pipeline::{ConfirmationAction, ConfirmationObservation};

#[derive(Debug, Default, Clone, Copy)]
pub struct ConfirmWorker;

impl ConfirmWorker {
    pub fn next_action(
        observation: ConfirmationObservation,
        blockhash_expired: bool,
    ) -> ConfirmationAction {
        match observation {
            ConfirmationObservation::Confirmed { slot } => {
                ConfirmationAction::MarkConfirmed { slot }
            }
            ConfirmationObservation::Failed { reason } => ConfirmationAction::MarkFailed { reason },
            ConfirmationObservation::Unknown if blockhash_expired => {
                ConfirmationAction::ExpireAndReconcile
            }
            ConfirmationObservation::Unknown => ConfirmationAction::KeepPolling,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn confirm_worker_keeps_polling_unknown_valid_signature() {
        assert_eq!(
            ConfirmWorker::next_action(ConfirmationObservation::Unknown, false),
            ConfirmationAction::KeepPolling
        );
    }

    #[test]
    fn confirm_worker_expires_unknown_signature_after_blockhash_expiry() {
        assert_eq!(
            ConfirmWorker::next_action(ConfirmationObservation::Unknown, true),
            ConfirmationAction::ExpireAndReconcile
        );
    }

    #[test]
    fn confirm_worker_marks_confirmed_status() {
        assert_eq!(
            ConfirmWorker::next_action(
                ConfirmationObservation::Confirmed { slot: Some(10) },
                false
            ),
            ConfirmationAction::MarkConfirmed { slot: Some(10) }
        );
    }
}
