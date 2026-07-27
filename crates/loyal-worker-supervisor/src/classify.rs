//! Typed failure classification.
//!
//! Classification reads typed SQLSTATE codes, HTTP statuses, and
//! [`std::io::ErrorKind`] values. Raw message matching is deliberately absent:
//! the production `retryable` flag that this replaces was set by hand and
//! disagreed with itself across workers observing the same outage.

use std::io::ErrorKind;

use loyal_observability::{FailureClass, WorkerDependency};

/// A classified failure, ready for a supervisor decision.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SupervisedFailure {
    /// The dependency held responsible.
    pub dependency: WorkerDependency,
    /// How the worker must react.
    pub class: FailureClass,
}

impl SupervisedFailure {
    /// Creates a classified failure.
    pub const fn new(dependency: WorkerDependency, class: FailureClass) -> Self {
        Self { dependency, class }
    }
}

/// PostgreSQL SQLSTATE codes that mean the connection or server went away.
const CONNECTION_SQLSTATE_CLASS: &str = "08";
const ADMIN_SHUTDOWN: &str = "57P01";
const CRASH_SHUTDOWN: &str = "57P02";
const CANNOT_CONNECT_NOW: &str = "57P03";
const TOO_MANY_CONNECTIONS: &str = "53300";
const CONFIGURATION_LIMIT_EXCEEDED: &str = "53400";
const SERIALIZATION_FAILURE: &str = "40001";
const DEADLOCK_DETECTED: &str = "40P01";
const UNIQUE_VIOLATION: &str = "23505";
const LOCK_NOT_AVAILABLE: &str = "55P03";

/// SQLSTATE codes that mean the deployment is wrong, not the network.
const UNDEFINED_TABLE: &str = "42P01";
const UNDEFINED_COLUMN: &str = "42703";
const UNDEFINED_FUNCTION: &str = "42883";
const INVALID_CATALOG_NAME: &str = "3D000";
const INVALID_AUTHORIZATION: &str = "28000";
const INVALID_PASSWORD: &str = "28P01";
const INSUFFICIENT_PRIVILEGE: &str = "42501";

/// Classifies a `sqlx` failure against the database it came from.
///
/// The caller names the dependency because one process may hold both a Neon and
/// a TimescaleDB pool, and their outages must not share a backoff counter.
pub fn classify_sqlx(error: &sqlx::Error, dependency: WorkerDependency) -> SupervisedFailure {
    let class = match error {
        sqlx::Error::Io(io_error) => io_error_class(io_error.kind()),
        sqlx::Error::PoolTimedOut | sqlx::Error::PoolClosed | sqlx::Error::WorkerCrashed => {
            FailureClass::TransientIo
        }
        sqlx::Error::Tls(_) | sqlx::Error::Protocol(_) => FailureClass::TransientIo,
        sqlx::Error::Configuration(_) | sqlx::Error::InvalidArgument(_) => {
            FailureClass::FatalProcess
        }
        sqlx::Error::Database(database_error) => {
            database_error.code().map_or(FailureClass::PermanentItem, |code| sqlstate_class(&code))
        }
        // Schema drift between the deployed binary and the live database.
        sqlx::Error::TypeNotFound { .. }
        | sqlx::Error::ColumnNotFound(_)
        | sqlx::Error::ColumnIndexOutOfBounds { .. } => FailureClass::FatalProcess,
        sqlx::Error::RowNotFound
        | sqlx::Error::ColumnDecode { .. }
        | sqlx::Error::Decode(_)
        | sqlx::Error::Encode(_) => FailureClass::PermanentItem,
        _ => FailureClass::TransientIo,
    };
    SupervisedFailure::new(dependency, class)
}

fn sqlstate_class(code: &str) -> FailureClass {
    if code.starts_with(CONNECTION_SQLSTATE_CLASS) {
        return FailureClass::TransientIo;
    }
    match code {
        ADMIN_SHUTDOWN
        | CRASH_SHUTDOWN
        | CANNOT_CONNECT_NOW
        | TOO_MANY_CONNECTIONS
        | CONFIGURATION_LIMIT_EXCEEDED => FailureClass::TransientIo,
        SERIALIZATION_FAILURE | DEADLOCK_DETECTED | UNIQUE_VIOLATION | LOCK_NOT_AVAILABLE => {
            FailureClass::Contention
        }
        UNDEFINED_TABLE
        | UNDEFINED_COLUMN
        | UNDEFINED_FUNCTION
        | INVALID_CATALOG_NAME
        | INVALID_AUTHORIZATION
        | INVALID_PASSWORD
        | INSUFFICIENT_PRIVILEGE => FailureClass::FatalProcess,
        _ => FailureClass::PermanentItem,
    }
}

/// Classifies a `reqwest` failure against the HTTP dependency it came from.
pub fn classify_reqwest(error: &reqwest::Error, dependency: WorkerDependency) -> SupervisedFailure {
    let class = if error.is_timeout() || error.is_connect() || error.is_redirect() {
        FailureClass::TransientIo
    } else if let Some(status) = error.status() {
        if status.is_server_error()
            || status == reqwest::StatusCode::REQUEST_TIMEOUT
            || status == reqwest::StatusCode::TOO_MANY_REQUESTS
        {
            FailureClass::TransientIo
        } else {
            FailureClass::PermanentItem
        }
    } else if error.is_builder() {
        FailureClass::FatalProcess
    } else if error.is_decode() {
        FailureClass::PermanentItem
    } else {
        // A request that never produced a status generally failed in transport.
        FailureClass::TransientIo
    };
    SupervisedFailure::new(dependency, class)
}

/// Classifies a bare I/O failure.
pub fn classify_io(error: &std::io::Error, dependency: WorkerDependency) -> SupervisedFailure {
    SupervisedFailure::new(dependency, io_error_class(error.kind()))
}

fn io_error_class(kind: ErrorKind) -> FailureClass {
    match kind {
        ErrorKind::ConnectionReset
        | ErrorKind::ConnectionAborted
        | ErrorKind::ConnectionRefused
        | ErrorKind::NotConnected
        | ErrorKind::BrokenPipe
        | ErrorKind::TimedOut
        | ErrorKind::UnexpectedEof
        | ErrorKind::WouldBlock
        | ErrorKind::Interrupted
        | ErrorKind::AddrNotAvailable
        | ErrorKind::AddrInUse
        | ErrorKind::HostUnreachable
        | ErrorKind::NetworkUnreachable
        | ErrorKind::NetworkDown => FailureClass::TransientIo,
        ErrorKind::PermissionDenied | ErrorKind::InvalidInput => FailureClass::FatalProcess,
        _ => FailureClass::PermanentItem,
    }
}

/// Classifies an `anyhow` error by walking its source chain for a typed cause.
///
/// `primary` names the dependency to attribute an untyped failure to.
///
/// An error with no recognized typed cause is classified [`FailureClass::TransientIo`]
/// rather than fatal. Configuration, argument, and identity validation run
/// before supervision starts, so an unrecognized error inside the supervised
/// body is far more likely to be an unwrapped I/O failure than a permanent
/// fault. The bounded `starting_degraded` escalation is what stops a genuinely
/// misconfigured worker from retrying in silence.
pub fn classify_anyhow(error: &anyhow::Error, primary: WorkerDependency) -> SupervisedFailure {
    for cause in error.chain() {
        if let Some(sqlx_error) = cause.downcast_ref::<sqlx::Error>() {
            return classify_sqlx(sqlx_error, primary);
        }
        if let Some(reqwest_error) = cause.downcast_ref::<reqwest::Error>() {
            return classify_reqwest(reqwest_error, primary);
        }
        if let Some(io_error) = cause.downcast_ref::<std::io::Error>() {
            return classify_io(io_error, primary);
        }
    }
    SupervisedFailure::new(primary, FailureClass::TransientIo)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every code here was chosen because it appears during a Neon compute
    /// restart or failover, which is the outage shape the measurement found.
    #[test]
    fn connection_loss_sqlstates_are_transient() {
        for code in [
            "08000", "08003", "08006", ADMIN_SHUTDOWN, CRASH_SHUTDOWN, CANNOT_CONNECT_NOW,
            TOO_MANY_CONNECTIONS,
        ] {
            assert_eq!(
                sqlstate_class(code),
                FailureClass::TransientIo,
                "{code} must not terminate a worker"
            );
        }
    }

    /// Schema drift must still stop the process, otherwise a bad deploy would
    /// retry against a database it can never satisfy.
    #[test]
    fn schema_and_authorization_sqlstates_stay_fatal() {
        for code in [UNDEFINED_TABLE, UNDEFINED_COLUMN, INVALID_PASSWORD, INVALID_CATALOG_NAME] {
            assert_eq!(sqlstate_class(code), FailureClass::FatalProcess, "{code}");
        }
    }

    /// Contention must defer one item rather than degrade a whole dependency.
    #[test]
    fn lease_and_serialization_conflicts_are_contention() {
        for code in [SERIALIZATION_FAILURE, DEADLOCK_DETECTED, UNIQUE_VIOLATION] {
            assert_eq!(sqlstate_class(code), FailureClass::Contention, "{code}");
        }
    }

    /// Pool exhaustion under load previously exited the process; it is the
    /// single most common shape behind the measured restart waves.
    #[test]
    fn pool_timeout_is_transient_and_attributed_to_its_own_dependency() {
        let failure = classify_sqlx(&sqlx::Error::PoolTimedOut, WorkerDependency::Timescale);
        assert_eq!(failure.class, FailureClass::TransientIo);
        assert_eq!(failure.dependency, WorkerDependency::Timescale);
    }

    /// A dropped connection mid-query is the LaserStream and Neon failure shape.
    #[test]
    fn reset_connections_are_transient() {
        let error = sqlx::Error::Io(std::io::Error::from(ErrorKind::ConnectionReset));
        assert_eq!(
            classify_sqlx(&error, WorkerDependency::Neon).class,
            FailureClass::TransientIo
        );
    }

    /// An unwrapped I/O cause must still be found through an `anyhow` chain,
    /// because the worker bodies wrap errors with context at every layer.
    #[test]
    fn anyhow_chains_are_walked_for_a_typed_cause() {
        let error = anyhow::Error::new(std::io::Error::from(ErrorKind::BrokenPipe))
            .context("publish supported Kamino reserves")
            .context("monitor startup");
        let failure = classify_anyhow(&error, WorkerDependency::KaminoApi);
        assert_eq!(failure.class, FailureClass::TransientIo);
        assert_eq!(failure.dependency, WorkerDependency::KaminoApi);
    }

    /// Permission and argument mistakes are deployment faults, not outages.
    #[test]
    fn permission_denied_stays_fatal() {
        assert_eq!(
            io_error_class(ErrorKind::PermissionDenied),
            FailureClass::FatalProcess
        );
    }
}
