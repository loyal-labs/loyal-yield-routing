//! Bounded vocabulary and events for long-running worker supervision.
//!
//! Production telemetry currently records that a worker exited but not why: the
//! `*_fatal` records carry only a code and an operation, so a fleet-wide restart
//! wave cannot be attributed to the dependency that caused it. The types here
//! close that gap.
//!
//! Every field exported from this module is either a number or a `&'static str`
//! produced by an enum in this file, so no runtime error text, URL, or
//! credential can reach the collector through a supervisor event. The one
//! runtime-valued field is [`ProcessGeneration`], which is a locally generated
//! random token carrying no external data.

use std::{
    collections::hash_map::RandomState,
    fmt::{self, Display, Formatter},
    hash::{BuildHasher, Hasher},
};

use tracing::Level;

/// The `tracing` target exported by the supervisor-state OTLP layer.
pub(crate) const SUPERVISOR_STATE_TARGET: &str = "loyal.observability.supervisor_state";

/// A controlled external dependency of a long-running worker.
///
/// Restart waves are attributed per dependency, so backoff and degradation are
/// tracked against these values rather than against the process as a whole.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum WorkerDependency {
    /// The Neon/PostgreSQL durable store.
    Neon,
    /// The TimescaleDB observation store.
    Timescale,
    /// Solana JSON-RPC over HTTP.
    SolanaRpc,
    /// Solana JSON-RPC over WebSocket, including LaserStream sources.
    SolanaWebsocket,
    /// The Kamino public API.
    KaminoApi,
    /// Failure inside the process itself rather than a named dependency.
    ProcessLocal,
}

impl WorkerDependency {
    /// Returns the stable low-cardinality name used in telemetry.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Neon => "neon",
            Self::Timescale => "timescale",
            Self::SolanaRpc => "solana_rpc",
            Self::SolanaWebsocket => "solana_websocket",
            Self::KaminoApi => "kamino_api",
            Self::ProcessLocal => "process_local",
        }
    }
}

impl Display for WorkerDependency {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// How a worker must react to a failure.
///
/// This supersedes the ad-hoc `retryable` flag on [`crate::OperationalError`],
/// which production data showed to be inconsistent: the same fleet-wide
/// dependency loss was recorded as `retryable=true` by some workers and
/// `retryable=false` by others.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum FailureClass {
    /// Dependency connectivity was lost or timed out. Retry with backoff.
    TransientIo,
    /// A lease, fence, or serialization conflict. Defer the affected item only.
    Contention,
    /// One durable item is unprocessable. Record a bounded failure, continue.
    PermanentItem,
    /// Configuration, schema, cluster, or identity is wrong. Exit nonzero.
    FatalProcess,
}

impl FailureClass {
    /// Returns the stable low-cardinality name used in telemetry.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TransientIo => "transient_io",
            Self::Contention => "contention",
            Self::PermanentItem => "permanent_item",
            Self::FatalProcess => "fatal_process",
        }
    }

    /// Returns whether this class permits the process to keep running.
    ///
    /// This is the single predicate that decides process survival. A supervisor
    /// may terminate only when this returns `false`.
    pub const fn is_survivable(self) -> bool {
        !matches!(self, Self::FatalProcess)
    }
}

impl Display for FailureClass {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// The observable lifecycle state of a supervised worker process.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SupervisorState {
    /// Configuration accepted, dependencies not yet connected.
    Starting,
    /// Startup is blocked on a dependency that is expected to return.
    StartingDegraded,
    /// All dependencies are connected and work is progressing.
    Healthy,
    /// At least one dependency is failing; retries are in progress.
    DependencyDegraded,
    /// A previously degraded dependency succeeded; backoff is resetting.
    Recovering,
    /// Graceful shutdown requested; leases are being released.
    Stopping,
    /// Unrecoverable condition; the process is exiting nonzero.
    Fatal,
}

impl SupervisorState {
    /// Returns the stable low-cardinality name used in telemetry.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Starting => "starting",
            Self::StartingDegraded => "starting_degraded",
            Self::Healthy => "healthy",
            Self::DependencyDegraded => "dependency_degraded",
            Self::Recovering => "recovering",
            Self::Stopping => "stopping",
            Self::Fatal => "fatal",
        }
    }
}

impl Display for SupervisorState {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

/// A random token identifying one process lifetime.
///
/// Emitted once at startup and unchanged until the process exits. A consumer
/// that observes the token change has observed a restart, which is what
/// distinguishes in-process recovery from a supervisor wrapper that merely
/// relaunches the worker.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ProcessGeneration(String);

impl ProcessGeneration {
    /// Generates a token from the OS-seeded hasher state.
    ///
    /// `RandomState` is seeded per process by the operating system, so this
    /// needs no random-number dependency and cannot collide across restarts in
    /// any way an operator would notice.
    pub fn generate() -> Self {
        let mut hasher = RandomState::new().build_hasher();
        hasher.write_u64(std::process::id().into());
        Self(format!("{:016x}", hasher.finish()))
    }

    /// Returns the token as a string.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl Display for ProcessGeneration {
    fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

/// Counters describing supervised progress for one process lifetime.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct SupervisorCounters {
    /// Work attempts admitted since startup.
    pub attempts: u64,
    /// Work attempts that completed without a dependency failure.
    pub successes: u64,
    /// Transitions out of a degraded state back into healthy operation.
    pub recoveries: u64,
}

/// A single supervisor-state record.
///
/// One record names exactly one dependency. Flat fields are used rather than a
/// nested map because the collector stores log attributes as a string map, and
/// flat keys stay directly queryable there.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SupervisorStateEvent<'generation> {
    /// The lifecycle state being entered.
    pub state: SupervisorState,
    /// The dependency this transition concerns.
    pub dependency: WorkerDependency,
    /// The failure class, when the transition was caused by a failure.
    pub failure_class: Option<FailureClass>,
    /// Consecutive failures recorded for this dependency.
    pub consecutive_failures: u32,
    /// Planned delay before the next attempt against this dependency.
    pub retry_in_milliseconds: u64,
    /// Dependencies currently degraded.
    pub degraded_dependencies: u32,
    /// Progress counters for this process lifetime.
    pub counters: SupervisorCounters,
    /// The process lifetime token.
    pub process_generation: &'generation ProcessGeneration,
}

impl SupervisorStateEvent<'_> {
    /// Emits this record to local logs and, when enabled, the filtered OTLP layer.
    pub fn emit(&self) {
        tracing::event!(
            name: "loyal.worker_supervisor_state",
            target: SUPERVISOR_STATE_TARGET,
            Level::INFO,
            state = self.state.as_str(),
            dependency = self.dependency.as_str(),
            failure_class = self.failure_class.map_or("", FailureClass::as_str),
            consecutive_failures = self.consecutive_failures,
            retry_in_ms = self.retry_in_milliseconds,
            degraded_dependencies = self.degraded_dependencies,
            attempts = self.counters.attempts,
            successes = self.counters.successes,
            recoveries = self.counters.recoveries,
            process_generation = self.process_generation.as_str(),
            message = "worker supervisor state",
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The supervisor's survival decision must depend on nothing but the class,
    /// so that no worker can reintroduce an exit on a transient failure.
    #[test]
    fn only_the_fatal_class_permits_process_termination() {
        assert!(!FailureClass::FatalProcess.is_survivable());
        for class in [
            FailureClass::TransientIo,
            FailureClass::Contention,
            FailureClass::PermanentItem,
        ] {
            assert!(class.is_survivable(), "{class} must not terminate a worker");
        }
    }

    /// Telemetry names are queried by dashboards and alerts, so they are part of
    /// the external contract rather than incidental strings.
    #[test]
    fn telemetry_names_are_stable_and_distinct() {
        let dependencies = [
            WorkerDependency::Neon,
            WorkerDependency::Timescale,
            WorkerDependency::SolanaRpc,
            WorkerDependency::SolanaWebsocket,
            WorkerDependency::KaminoApi,
            WorkerDependency::ProcessLocal,
        ];
        let names = dependencies.map(WorkerDependency::as_str);
        let mut unique = names.to_vec();
        unique.sort_unstable();
        unique.dedup();
        assert_eq!(unique.len(), names.len());
        assert_eq!(WorkerDependency::Neon.as_str(), "neon");
        assert_eq!(SupervisorState::StartingDegraded.as_str(), "starting_degraded");
        assert_eq!(FailureClass::TransientIo.as_str(), "transient_io");
    }

    /// A restart must be observable, so two lifetimes must not share a token.
    #[test]
    fn process_generations_differ_between_lifetimes() {
        assert_ne!(
            ProcessGeneration::generate().as_str(),
            ProcessGeneration::generate().as_str()
        );
    }
}
