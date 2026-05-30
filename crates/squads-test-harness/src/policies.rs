//! Squads policy builders grouped by policy family.
//!
//! Loyal route action construction lives in the `loyal-actions` crate. This
//! module owns lower-level Squads policy instructions for focused harness tests.

mod internal_fund_transfer;
mod lifecycle;
mod program_interaction;
mod spending_limits;

pub use internal_fund_transfer::*;
pub use lifecycle::*;
pub use program_interaction::*;
pub use spending_limits::*;
