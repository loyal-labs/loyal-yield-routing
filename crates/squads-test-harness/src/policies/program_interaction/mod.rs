//! Raw Squads `ProgramInteraction` policy builders.
//!
//! This directory is the low-level constraint-encoding layer below
//! `yield_route`. Stable-swap, Kamino, and all-in-one route builders stay in
//! separate modules so protocol-specific constraints are easy to find.

mod common;
mod kamino;
mod route_bundles;
mod stable_swap;

pub use kamino::*;
pub use route_bundles::*;
pub use stable_swap::*;
