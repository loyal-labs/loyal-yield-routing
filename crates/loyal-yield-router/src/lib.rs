//! Read-only data-lake SQL access for Loyal yield routing inputs.

pub mod timescale;

pub mod data_lake {
    pub use crate::timescale::{
        QueryOrder, ReserveHistoryQuery, ReserveStreamItem, ReserveUpdateCursor,
        ReserveUpdateFilter, ReserveUpdateNotification, ReserveUpdateRow, ReserveUpdateStream,
        ReserveWindowStats, ReserveWindowStatsQuery, SubscribeOptions,
        TimescaleRouterClient as DataLakeSqlClient,
        TimescaleRouterClientConfig as DataLakeSqlConfig,
    };
}
