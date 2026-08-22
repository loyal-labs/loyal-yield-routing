//! Durable, value-prioritized fleet orchestration.
//!
//! The module keeps scheduling and queue semantics reusable by thin worker
//! binaries.  Chain-specific route construction remains at the integration
//! boundary rather than leaking into the economic planner.

pub mod confirmation;
pub mod health;
pub mod observation;
pub mod planner;
pub mod reconciliation;
pub mod resilience;
pub mod runtime_evidence;
pub mod source_evidence;
pub mod voltr_controller;
pub mod voltr_observation;
pub mod voltr_planning;

pub use confirmation::*;
pub use health::*;
pub use observation::*;
pub use planner::*;
pub use reconciliation::*;
pub use resilience::*;
pub use runtime_evidence::*;
pub use source_evidence::*;
pub use voltr_controller::*;
pub use voltr_observation::*;
pub use voltr_planning::*;

pub mod capacity {
    #[doc(inline)]
    pub use loyal_yield_store::fleet_orchestration::capacity::*;
}

pub mod domain {
    #[doc(inline)]
    pub use loyal_yield_store::fleet_orchestration::domain::*;
}

pub mod queue {
    #[doc(inline)]
    pub use loyal_yield_store::fleet_orchestration::queue::*;
}

pub use capacity::*;
pub use domain::*;
pub use queue::*;
