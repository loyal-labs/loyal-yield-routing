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
    let Some(slot) = status.slot else {
        return AuthoritativeConfirmationDecision::Pending;
    };
    if slot < 0 {
        return AuthoritativeConfirmationDecision::InvalidSlot;
    }
    if !status.satisfies_confirmed_commitment {
        return AuthoritativeConfirmationDecision::Pending;
    }
    if status.transaction_error {
        AuthoritativeConfirmationDecision::Failed { slot }
    } else {
        AuthoritativeConfirmationDecision::Confirmed { slot }
    }
}
