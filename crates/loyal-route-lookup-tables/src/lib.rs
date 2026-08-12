pub mod lookup_tables;

pub use lookup_tables::*;
pub use loyal_yield_store::domain::*;
pub use loyal_yield_store::types::*;
pub use loyal_yield_store::{sqlx, OrchestratorError, RouteLookupTableProvisioningLock};

use loyal_yield_store::fleet_orchestration::{
    RebalanceOpportunityLease, TargetCapacityReservationInput, TargetCapacityReservationRecord,
};
use sqlx::PgPool;
use std::ops::Deref;

#[derive(Clone)]
pub struct NeonSqlClient {
    inner: loyal_yield_store::NeonSqlClient,
}

pub type OrchestratorStore = NeonSqlClient;

impl NeonSqlClient {
    pub async fn connect(config: NeonSqlConfig) -> Result<Self, OrchestratorError> {
        loyal_yield_store::NeonSqlClient::connect(config)
            .await
            .map(|inner| Self { inner })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self {
            inner: loyal_yield_store::NeonSqlClient::from_pool(pool),
        }
    }

    pub fn pool(&self) -> &PgPool {
        self.inner.pool()
    }

    #[doc(hidden)]
    pub async fn reserve_target_capacity_in_connection(
        connection: &mut sqlx::PgConnection,
        opportunity_lease: &RebalanceOpportunityLease,
        input: &TargetCapacityReservationInput,
        compiled_fee_lamports: i64,
    ) -> Result<TargetCapacityReservationRecord, OrchestratorError> {
        loyal_yield_store::NeonSqlClient::reserve_target_capacity_in_connection(
            connection,
            opportunity_lease,
            input,
            compiled_fee_lamports,
        )
        .await
    }
}

impl Deref for NeonSqlClient {
    type Target = loyal_yield_store::NeonSqlClient;

    fn deref(&self) -> &Self::Target {
        &self.inner
    }
}
