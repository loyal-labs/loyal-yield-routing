//! Durable, value-prioritized fleet orchestration.
//!
//! The module keeps scheduling and queue semantics reusable by thin worker
//! binaries.  Chain-specific route construction remains at the integration
//! boundary rather than leaking into the economic planner.

pub mod capacity;
pub mod confirmation;
pub mod domain;
pub mod health;
pub mod observation;
pub mod planner;
pub mod queue;
pub mod resilience;
pub mod runtime_evidence;
pub mod source_evidence;

pub use capacity::*;
pub use confirmation::*;
pub use domain::*;
pub use health::*;
pub use observation::*;
pub use planner::*;
pub use queue::*;
pub use resilience::*;
pub use runtime_evidence::*;
pub use source_evidence::*;
