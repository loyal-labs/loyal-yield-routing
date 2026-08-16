//! Pure confirmation authority and wakeup semantics.
//!
//! WebSocket notifications are intentionally represented only as scheduling
//! hints. They cannot produce a terminal route outcome; terminal state always
//! requires an authoritative `getSignatureStatuses` observation with a slot.

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConfirmationPollTrigger {
    SubscriptionHint,
    DurableRecoveryDeadline,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthoritativePollUrgency {
    Immediate,
    Scheduled,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AuthoritativeStatusPoll {
    pub urgency: AuthoritativePollUrgency,
}

/// Converts every wakeup into an authoritative status poll. There is no
/// terminal variant in this scheduling boundary by design.
pub fn schedule_authoritative_status_poll(
    trigger: ConfirmationPollTrigger,
) -> AuthoritativeStatusPoll {
    AuthoritativeStatusPoll {
        urgency: match trigger {
            ConfirmationPollTrigger::SubscriptionHint => AuthoritativePollUrgency::Immediate,
            ConfirmationPollTrigger::DurableRecoveryDeadline => AuthoritativePollUrgency::Scheduled,
        },
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AuthoritativeSignatureStatus {
    pub slot: Option<i64>,
    pub satisfies_confirmed_commitment: bool,
    pub transaction_error: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RequiredConfirmationCommitment {
    Confirmed,
    Finalized,
}

impl RequiredConfirmationCommitment {
    pub fn from_persisted(value: &str) -> Result<Self, &'static str> {
        match value {
            "confirmed" => Ok(Self::Confirmed),
            "finalized" => Ok(Self::Finalized),
            _ => Err("unsupported persisted confirmation commitment"),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthoritativeConfirmationDecision {
    Pending,
    Confirmed { slot: i64 },
    Failed { slot: i64 },
    InvalidSlot,
}

/// Classifies only authoritative HTTP RPC evidence. A missing status, an
/// unconfirmed status, or a status without a nonnegative slot stays
/// nonterminal.
pub fn classify_authoritative_signature_status(
    status: AuthoritativeSignatureStatus,
) -> AuthoritativeConfirmationDecision {
    classify_authoritative_signature_status_for_commitment(
        status,
        RequiredConfirmationCommitment::Confirmed,
        false,
    )
}

/// Classifies authoritative HTTP RPC evidence against the commitment required
/// by the persisted submission. `satisfies_finalized_commitment` is kept
/// separate from the legacy status shape so confirmed-only callers retain
/// their existing behavior and API.
pub fn classify_authoritative_signature_status_for_commitment(
    status: AuthoritativeSignatureStatus,
    required_commitment: RequiredConfirmationCommitment,
    satisfies_finalized_commitment: bool,
) -> AuthoritativeConfirmationDecision {
    let Some(slot) = status.slot else {
        return AuthoritativeConfirmationDecision::Pending;
    };
    if slot < 0 {
        return AuthoritativeConfirmationDecision::InvalidSlot;
    }
    let satisfies_required_commitment = match required_commitment {
        RequiredConfirmationCommitment::Confirmed => status.satisfies_confirmed_commitment,
        RequiredConfirmationCommitment::Finalized => satisfies_finalized_commitment,
    };
    if !satisfies_required_commitment {
        return AuthoritativeConfirmationDecision::Pending;
    }
    if status.transaction_error {
        AuthoritativeConfirmationDecision::Failed { slot }
    } else {
        AuthoritativeConfirmationDecision::Confirmed { slot }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn finalized_requirement_waits_for_finalized_rpc_evidence() {
        let confirmed_only = AuthoritativeSignatureStatus {
            slot: Some(42),
            satisfies_confirmed_commitment: true,
            transaction_error: false,
        };

        assert_eq!(
            classify_authoritative_signature_status_for_commitment(
                confirmed_only,
                RequiredConfirmationCommitment::Finalized,
                false,
            ),
            AuthoritativeConfirmationDecision::Pending
        );
        assert_eq!(
            classify_authoritative_signature_status_for_commitment(
                confirmed_only,
                RequiredConfirmationCommitment::Finalized,
                true,
            ),
            AuthoritativeConfirmationDecision::Confirmed { slot: 42 }
        );
    }

    #[test]
    fn confirmed_requirement_preserves_legacy_behavior() {
        let confirmed = AuthoritativeSignatureStatus {
            slot: Some(43),
            satisfies_confirmed_commitment: true,
            transaction_error: false,
        };

        assert_eq!(
            classify_authoritative_signature_status(confirmed),
            classify_authoritative_signature_status_for_commitment(
                confirmed,
                RequiredConfirmationCommitment::Confirmed,
                false,
            )
        );
        assert_eq!(
            classify_authoritative_signature_status(confirmed),
            AuthoritativeConfirmationDecision::Confirmed { slot: 43 }
        );
    }

    #[test]
    fn finalized_requirement_does_not_terminalize_forkable_error_evidence() {
        let confirmed_error = AuthoritativeSignatureStatus {
            slot: Some(44),
            satisfies_confirmed_commitment: true,
            transaction_error: true,
        };

        assert_eq!(
            classify_authoritative_signature_status_for_commitment(
                confirmed_error,
                RequiredConfirmationCommitment::Finalized,
                false,
            ),
            AuthoritativeConfirmationDecision::Pending
        );
        assert_eq!(
            classify_authoritative_signature_status_for_commitment(
                confirmed_error,
                RequiredConfirmationCommitment::Finalized,
                true,
            ),
            AuthoritativeConfirmationDecision::Failed { slot: 44 }
        );
    }
}
