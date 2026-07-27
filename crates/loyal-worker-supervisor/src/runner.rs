//! The supervised run loop for long-running workers.
//!
//! The measured defect this replaces is a `?` on a dependency error inside a
//! worker's main loop: the process exits, and recovery is left to Render's
//! restart backoff, which was observed growing to 185 seconds during a single
//! sustained outage. Supervision keeps the same work inside one process with a
//! bounded retry schedule instead.

use std::{collections::BTreeMap, future::Future, time::Duration};

use loyal_observability::{
    FailureClass, OperationalError, ProcessGeneration, SupervisorCounters, SupervisorState,
    SupervisorStateEvent, WorkerDependency,
};
use tokio::time::Instant;

use crate::{
    backoff::{jittered, BackoffPolicy, Jitter},
    classify::SupervisedFailure,
};

/// Default time a supervised body must run before it counts as healthy.
pub const DEFAULT_HEALTHY_AFTER: Duration = Duration::from_secs(60);

/// Default time in `starting_degraded` before the process escalates.
pub const DEFAULT_STARTUP_DEGRADED_LIMIT: Duration = Duration::from_secs(600);

/// Why a supervised process is terminating.
///
/// Constructed only where the classification is [`FailureClass::FatalProcess`],
/// so a transient failure has no representation that ends a process.
#[derive(Debug)]
pub struct FatalExit<E> {
    /// The original error, for operator-facing output.
    pub error: E,
    /// The classification that permitted termination.
    pub failure: SupervisedFailure,
}

#[derive(Clone, Copy, Debug, Default)]
struct DependencyHealth {
    consecutive_failures: u32,
    degraded: bool,
}

/// Tracks per-dependency health and runs a worker body under supervision.
#[derive(Debug)]
pub struct WorkerSupervisor {
    generation: ProcessGeneration,
    policy: BackoffPolicy,
    jitter: Jitter,
    dependencies: BTreeMap<WorkerDependency, DependencyHealth>,
    counters: SupervisorCounters,
    healthy_after: Duration,
    startup_degraded_limit: Duration,
    reached_healthy: bool,
    escalated_startup: bool,
}

impl WorkerSupervisor {
    /// Creates a supervisor with the default bounded retry schedule.
    pub fn new() -> Self {
        Self {
            generation: ProcessGeneration::generate(),
            policy: BackoffPolicy::default(),
            jitter: Jitter::new(),
            dependencies: BTreeMap::new(),
            counters: SupervisorCounters::default(),
            healthy_after: DEFAULT_HEALTHY_AFTER,
            startup_degraded_limit: DEFAULT_STARTUP_DEGRADED_LIMIT,
            reached_healthy: false,
            escalated_startup: false,
        }
    }

    /// Overrides the retry schedule.
    pub fn with_backoff(mut self, policy: BackoffPolicy) -> Self {
        self.policy = policy;
        self
    }

    /// Overrides how long a body must run before it counts as healthy.
    pub fn with_healthy_after(mut self, healthy_after: Duration) -> Self {
        self.healthy_after = healthy_after;
        self
    }

    /// Overrides the bound on `starting_degraded`.
    pub fn with_startup_degraded_limit(mut self, limit: Duration) -> Self {
        self.startup_degraded_limit = limit;
        self
    }

    /// Returns this process lifetime's token.
    pub fn generation(&self) -> &ProcessGeneration {
        &self.generation
    }

    /// Returns progress counters for this process lifetime.
    pub fn counters(&self) -> SupervisorCounters {
        self.counters
    }

    /// Runs `attempt` until it completes cleanly or fails fatally.
    ///
    /// A body that returns an error classified as anything other than
    /// [`FailureClass::FatalProcess`] is retried after a bounded, jittered
    /// delay. This is the only place a supervised worker can terminate, and it
    /// can do so only on that one classification.
    ///
    /// Configuration, argument, and identity validation belong *outside* this
    /// call. Everything inside it is treated as recoverable I/O unless
    /// classification proves otherwise.
    pub async fn run_supervised<E, F, Fut>(
        &mut self,
        classify: impl Fn(&E) -> SupervisedFailure,
        mut attempt: F,
    ) -> Result<(), FatalExit<E>>
    where
        F: FnMut() -> Fut,
        Fut: Future<Output = Result<(), E>>,
    {
        self.emit(
            SupervisorState::Starting,
            WorkerDependency::ProcessLocal,
            None,
            0,
            0,
        );
        let degraded_since = Instant::now();

        loop {
            self.counters.attempts = self.counters.attempts.saturating_add(1);
            let outcome = self.run_one_attempt(&mut attempt).await;

            let error = match outcome {
                Ok(()) => {
                    self.counters.successes = self.counters.successes.saturating_add(1);
                    self.emit(
                        SupervisorState::Stopping,
                        WorkerDependency::ProcessLocal,
                        None,
                        0,
                        0,
                    );
                    return Ok(());
                }
                Err(error) => error,
            };

            let failure = classify(&error);
            if !failure.class.is_survivable() {
                self.emit(
                    SupervisorState::Fatal,
                    failure.dependency,
                    Some(failure.class),
                    0,
                    0,
                );
                return Err(FatalExit { error, failure });
            }

            let delay = self.record_failure(failure);
            if !self.reached_healthy
                && !self.escalated_startup
                && degraded_since.elapsed() >= self.startup_degraded_limit
            {
                self.escalated_startup = true;
                OperationalError::new(
                    "worker_startup_degraded_limit_exceeded",
                    "start_supervised_worker",
                    "Worker has not reached a healthy state since startup and is still retrying",
                )
                .retryable(true)
                .recovery_required(true)
                .dependency(failure.dependency)
                .failure_class(failure.class)
                .emit();
            }
            tokio::time::sleep(delay).await;
        }
    }

    /// Runs one attempt, marking the process healthy once it has survived long
    /// enough for its dependencies to be considered working.
    async fn run_one_attempt<E, F, Fut>(&mut self, attempt: &mut F) -> Result<(), E>
    where
        F: FnMut() -> Fut,
        Fut: Future<Output = Result<(), E>>,
    {
        let body = attempt();
        tokio::pin!(body);
        let healthy_at = tokio::time::sleep(self.healthy_after);
        tokio::pin!(healthy_at);
        let mut marked_healthy = false;

        loop {
            tokio::select! {
                result = &mut body => return result,
                () = &mut healthy_at, if !marked_healthy => {
                    marked_healthy = true;
                    self.mark_healthy();
                }
            }
        }
    }

    /// Records a dependency success across the board.
    ///
    /// A body that ran past the healthy threshold exercised every dependency it
    /// needs, so all counters reset together. Per-dependency counters still
    /// diverge between resets, which is what keeps one dependency's recovery
    /// from clearing another's failure history mid-outage.
    fn mark_healthy(&mut self) {
        let was_degraded = self.dependencies.values().any(|health| health.degraded);
        if was_degraded {
            self.counters.recoveries = self.counters.recoveries.saturating_add(1);
        }
        for health in self.dependencies.values_mut() {
            *health = DependencyHealth::default();
        }
        self.reached_healthy = true;
        self.escalated_startup = false;
        self.counters.successes = self.counters.successes.saturating_add(1);
        let state = if was_degraded {
            SupervisorState::Recovering
        } else {
            SupervisorState::Healthy
        };
        self.emit(state, WorkerDependency::ProcessLocal, None, 0, 0);
    }

    /// Records a survivable failure and returns the delay before the next try.
    fn record_failure(&mut self, failure: SupervisedFailure) -> Duration {
        let health = self.dependencies.entry(failure.dependency).or_default();
        health.consecutive_failures = health.consecutive_failures.saturating_add(1);
        health.degraded = true;
        let consecutive_failures = health.consecutive_failures;

        let delay = jittered(self.policy.delay_after(consecutive_failures), &mut self.jitter);
        let state = if self.reached_healthy {
            SupervisorState::DependencyDegraded
        } else {
            SupervisorState::StartingDegraded
        };
        self.emit(
            state,
            failure.dependency,
            Some(failure.class),
            consecutive_failures,
            u64::try_from(delay.as_millis()).unwrap_or(u64::MAX),
        );
        delay
    }

    fn emit(
        &self,
        state: SupervisorState,
        dependency: WorkerDependency,
        failure_class: Option<FailureClass>,
        consecutive_failures: u32,
        retry_in_milliseconds: u64,
    ) {
        let degraded_dependencies = self
            .dependencies
            .values()
            .filter(|health| health.degraded)
            .count();
        SupervisorStateEvent {
            state,
            dependency,
            failure_class,
            consecutive_failures,
            retry_in_milliseconds,
            degraded_dependencies: u32::try_from(degraded_dependencies).unwrap_or(u32::MAX),
            counters: self.counters,
            process_generation: &self.generation,
        }
        .emit();
    }
}

impl Default for WorkerSupervisor {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use std::{
        cell::Cell,
        io::ErrorKind,
        rc::Rc,
        sync::atomic::{AtomicU32, Ordering},
    };

    use super::*;

    fn transient() -> SupervisedFailure {
        SupervisedFailure::new(WorkerDependency::Neon, FailureClass::TransientIo)
    }

    fn fatal() -> SupervisedFailure {
        SupervisedFailure::new(WorkerDependency::Neon, FailureClass::FatalProcess)
    }

    fn fast_supervisor() -> WorkerSupervisor {
        WorkerSupervisor::new().with_backoff(BackoffPolicy::new(
            Duration::from_millis(1),
            Duration::from_millis(4),
        ))
    }

    /// The whole point of the change: a transient dependency failure must not
    /// end the process, no matter how many times it repeats.
    #[tokio::test]
    async fn transient_failures_retry_in_process_instead_of_exiting() {
        static ATTEMPTS: AtomicU32 = AtomicU32::new(0);
        let result = fast_supervisor()
            .run_supervised(
                |_: &std::io::Error| transient(),
                || async {
                    if ATTEMPTS.fetch_add(1, Ordering::SeqCst) < 5 {
                        Err(std::io::Error::from(ErrorKind::ConnectionReset))
                    } else {
                        Ok(())
                    }
                },
            )
            .await;

        assert!(result.is_ok(), "a recoverable body must not terminate");
        assert_eq!(ATTEMPTS.load(Ordering::SeqCst), 6);
    }

    /// A wrong schema or bad credential must still stop the process, or a bad
    /// deploy would retry forever against a database it cannot satisfy.
    #[tokio::test]
    async fn fatal_classification_terminates_without_retrying() {
        let attempts = Rc::new(Cell::new(0_u32));
        let counted = Rc::clone(&attempts);
        let result = fast_supervisor()
            .run_supervised(
                |_: &std::io::Error| fatal(),
                || {
                    let counted = Rc::clone(&counted);
                    async move {
                        counted.set(counted.get() + 1);
                        Err(std::io::Error::from(ErrorKind::PermissionDenied))
                    }
                },
            )
            .await;

        let exit = result.expect_err("a fatal classification must terminate");
        assert_eq!(exit.failure.class, FailureClass::FatalProcess);
        assert_eq!(attempts.get(), 1, "a fatal body must not be retried");
    }

    /// Retry delay must stay under the ceiling however long the outage runs,
    /// which is the property Render's restart backoff does not provide.
    #[tokio::test(start_paused = true)]
    async fn retry_delay_stays_within_the_configured_ceiling() {
        let mut supervisor = WorkerSupervisor::new().with_backoff(BackoffPolicy::new(
            Duration::from_millis(250),
            Duration::from_secs(5),
        ));
        let mut observed = Vec::new();
        for _ in 0..12 {
            observed.push(supervisor.record_failure(transient()));
        }

        assert!(observed.iter().all(|delay| !delay.is_zero()));
        assert!(observed.iter().all(|delay| *delay <= Duration::from_secs(5)));
        assert!(
            observed.last().copied().unwrap() >= Duration::from_millis(2_500),
            "a long outage must still reach the ceiling band"
        );
    }

    /// Two dependencies failing must not share a counter, otherwise restoring
    /// one would silently reset the other's backoff mid-outage.
    #[tokio::test(start_paused = true)]
    async fn dependency_backoff_counters_stay_independent() {
        let mut supervisor = fast_supervisor();
        for _ in 0..4 {
            supervisor.record_failure(SupervisedFailure::new(
                WorkerDependency::Neon,
                FailureClass::TransientIo,
            ));
        }
        supervisor.record_failure(SupervisedFailure::new(
            WorkerDependency::Timescale,
            FailureClass::TransientIo,
        ));

        assert_eq!(
            supervisor.dependencies[&WorkerDependency::Neon].consecutive_failures,
            4
        );
        assert_eq!(
            supervisor.dependencies[&WorkerDependency::Timescale].consecutive_failures,
            1
        );
    }

    /// A body that survives the healthy window clears its failure history, so a
    /// flapping dependency cannot ratchet the delay upward forever.
    #[tokio::test(start_paused = true)]
    async fn surviving_the_healthy_window_resets_backoff_and_counts_a_recovery() {
        let mut supervisor = fast_supervisor().with_healthy_after(Duration::from_secs(30));
        supervisor.record_failure(transient());
        assert_eq!(
            supervisor.dependencies[&WorkerDependency::Neon].consecutive_failures,
            1
        );

        supervisor.mark_healthy();

        assert_eq!(
            supervisor.dependencies[&WorkerDependency::Neon].consecutive_failures,
            0
        );
        assert_eq!(supervisor.counters().recoveries, 1);
    }
}
