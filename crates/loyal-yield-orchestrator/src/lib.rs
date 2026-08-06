pub mod fleet_orchestration;
pub mod lookup_table_alerts;
pub mod lookup_tables;
pub mod rpc_safety;
mod shared_market_catalog;
mod signer;
mod stable_mints;

pub use lookup_table_alerts::*;
pub use lookup_tables::*;
pub use loyal_yield_store::domain::*;
pub use loyal_yield_store::types::*;
pub use loyal_yield_store::{sqlx, OrchestratorError, RouteLookupTableProvisioningLock};
pub use shared_market_catalog::{
    decode_kamino_reserve_account, derive_shared_market_catalog,
    load_finalized_kamino_reserve_catalog, validate_supported_reserve, DerivedSharedMarketCatalog,
    FinalizedKaminoReserveCatalog, KaminoReserveCatalogAccount, SharedMarketCatalogError,
    SupportedKaminoReserve,
};
pub use signer::{
    keypair_from_env, keypair_from_hex, keypair_from_string, policy_keypair_from_env,
    route_fee_payer_keypairs_from_env, solana_testing_keypair_from_env,
    standard_policy_keypair_from_env, yield_router_keypair_from_env, PolicySignerError,
    POLICY_KEYPAIR_ENV, SOLANA_TESTING_PK_ENV, STANDARD_POLICY_AUTHORITY, YIELD_ROUTER_KEYPAIR_ENV,
    YIELD_ROUTE_FEE_PAYER_KEYPAIRS_ENV,
};
pub use stable_mints::{
    enabled_stable_mints_from_env, enabled_stable_mints_hash, resolve_enabled_stable_mints,
    supported_stable_mints, StableMintConfigError, ENABLED_STABLE_MINTS_ENV,
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
        opportunity_lease: &fleet_orchestration::RebalanceOpportunityLease,
        input: &fleet_orchestration::TargetCapacityReservationInput,
        compiled_fee_lamports: i64,
    ) -> Result<fleet_orchestration::TargetCapacityReservationRecord, OrchestratorError> {
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
