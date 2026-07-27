//! In-process supervision for Loyal's long-running Render workers.
//!
//! # Why this exists
//!
//! Production telemetry for 2026-07-21 through 2026-07-27 recorded 57 process
//! exits across the fleet, roughly 8 per day. 58% of them fell into
//! multi-service waves in which up to six independent workers exited within
//! seconds of each other, which is the signature of a shared dependency going
//! away rather than of per-worker bugs.
//!
//! Recovery was left to Render, whose restart backoff was measured doubling
//! from 11 seconds to 185 seconds across a single seven-minute outage. The
//! repository's own listener reconnect policy caps at 5 seconds, so recovering
//! by exiting was about 37 times slower at the tail than recovering in place.
//!
//! # How it works
//!
//! Parse arguments and validate configuration *before* calling
//! [`WorkerSupervisor::run_supervised`]; a fault there is a deployment fault and
//! should still exit nonzero. Everything inside the supervised body is retried
//! with bounded, jittered backoff unless [`classify`] proves it fatal.
//!
//! The type-level guarantee is narrow and deliberate:
//! [`WorkerSupervisor::run_supervised`] returns [`FatalExit`] only where
//! [`loyal_observability::FailureClass::is_survivable`] is false, so no
//! transient failure has a representation that ends a process.

#![forbid(unsafe_code)]

pub mod backoff;
pub mod classify;
mod runner;

pub use backoff::{BackoffPolicy, DEFAULT_INITIAL_BACKOFF, DEFAULT_MAXIMUM_BACKOFF};
pub use classify::{
    classify_anyhow, classify_io, classify_reqwest, classify_sqlx, SupervisedFailure,
};
pub use loyal_observability::{FailureClass, WorkerDependency};
pub use runner::{
    FatalExit, WorkerSupervisor, DEFAULT_HEALTHY_AFTER, DEFAULT_STARTUP_DEGRADED_LIMIT,
};
