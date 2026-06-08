//! Raw Squads `ProgramInteraction` policy builders.
//!
//! This directory keeps low-level compact constraint helpers for focused
//! harness tests. Production route action construction lives in `loyal-actions`.

mod common;
mod kamino;
mod stable_swap;

pub use kamino::*;
pub use stable_swap::*;
