#![recursion_limit = "256"]

pub mod apy {
    pub use loyal_kamino_codec::apy::*;
}
pub mod cli;
pub mod source;
pub mod targets {
    pub use loyal_kamino_codec::{ReserveTarget, SupportedReserveRecord};
    pub use loyal_kamino_data::targets::*;
}
pub mod timescale {
    pub use loyal_kamino_data::timescale::*;
}
pub mod verification;
pub mod verification_schedule;

pub use apy::{
    diff_snapshot, snapshot_from_account, snapshot_from_account_at, ReserveDiff, ReserveSnapshot,
};
pub use targets::{resolve_loyal_targets, KaminoApi, ReserveTarget};
