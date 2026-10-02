//! Shared autocommit execution and catalog contract for the Earn history index.

use std::env;

use sqlx::PgPool;

use crate::OrchestratorError;

const INDEX_SCHEMA_SQL: &str = include_str!("earn_history_index_schema.sql");

#[derive(Debug, PartialEq, Eq)]
enum BuildMode {
    ExistingRole,
    BoundedTemp,
}

fn build_mode(value: Option<&str>) -> Result<BuildMode, OrchestratorError> {
    match value {
        Some("existing-role") => Ok(BuildMode::ExistingRole),
        Some("bounded-temp") => Ok(BuildMode::BoundedTemp),
        _ => Err(OrchestratorError::StoreInvariant(
            "Earn history index build is disabled by default; explicitly set EARN_HISTORY_INDEX_BUILD_MODE=existing-role or bounded-temp after the operational decision".to_owned(),
        )),
    }
}

pub struct EarnHistoryIndexPreflight {
    pub exists: bool,
    pub valid: bool,
    pub can_set_temp_file_limit: bool,
}

pub async fn preflight_earn_history_index(
    pool: &PgPool,
) -> Result<EarnHistoryIndexPreflight, OrchestratorError> {
    let (exists, can_set_temp_file_limit): (bool, bool) = sqlx::query_as(
        "SELECT to_regclass('loyal_yield.vault_position_snapshots_complete_history_idx') IS NOT NULL, has_parameter_privilege(current_user, 'temp_file_limit', 'SET')",
    )
    .fetch_one(pool)
    .await?;
    let valid = sqlx::query_scalar(INDEX_SCHEMA_SQL).fetch_one(pool).await?;
    Ok(EarnHistoryIndexPreflight {
        exists,
        valid,
        can_set_temp_file_limit,
    })
}

pub async fn validate_earn_history_index(pool: &PgPool) -> Result<(), OrchestratorError> {
    let valid: bool = sqlx::query_scalar(INDEX_SCHEMA_SQL).fetch_one(pool).await?;
    if !valid {
        return Err(OrchestratorError::StoreInvariant(
            "Earn history index is missing, invalid, or has the wrong keys/predicate; inspect the rollout runbook before recovery".to_owned(),
        ));
    }
    Ok(())
}

pub async fn apply_earn_history_index(
    pool: &PgPool,
    migration_sql: &str,
) -> Result<(), OrchestratorError> {
    let preflight = preflight_earn_history_index(pool).await?;
    if preflight.exists {
        // IF NOT EXISTS must not bless an invalid build or a name collision.
        // Never drop an existing index as an automatic recovery side effect.
        return validate_earn_history_index(pool).await;
    }
    let mode = build_mode(env::var("EARN_HISTORY_INDEX_BUILD_MODE").ok().as_deref())?;
    if mode == BuildMode::BoundedTemp && !preflight.can_set_temp_file_limit {
        return Err(OrchestratorError::StoreInvariant(
            "Earn history bounded-temp mode requires existing SET privilege for temp_file_limit; no DDL was attempted and no fallback is permitted".to_owned(),
        ));
    }
    eprintln!("Earn history index build mode: {mode:?}");

    // Keep these session settings and the concurrent CREATE on the same
    // physical connection. A multi-statement batch or BEGIN is not allowed.
    let mut connection = pool.acquire().await?;
    let previous: (String, String, String, String) = sqlx::query_as(
        "SELECT current_setting('lock_timeout'), current_setting('statement_timeout'), current_setting('max_parallel_maintenance_workers'), current_setting('temp_file_limit')",
    )
    .fetch_one(&mut *connection)
    .await?;
    let backend_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&mut *connection)
        .await?;
    eprintln!("Earn history index migration backend PID: {backend_pid}");
    let result = async {
        sqlx::query(
            "SELECT set_config('lock_timeout', '5s', false), set_config('statement_timeout', '30min', false), set_config('max_parallel_maintenance_workers', '0', false)",
        )
        .execute(&mut *connection)
        .await?;
        if mode == BuildMode::BoundedTemp {
            sqlx::query("SELECT set_config('temp_file_limit', '8GB', false)")
                .execute(&mut *connection)
                .await?;
        }
        sqlx::raw_sql(migration_sql).execute(&mut *connection).await?;
        Ok::<(), sqlx::Error>(())
    }
    .await;
    let restored = async {
        if mode == BuildMode::BoundedTemp {
            sqlx::query("SELECT set_config('temp_file_limit', $1, false)")
                .bind(&previous.3)
                .execute(&mut *connection)
                .await?;
        }
        sqlx::query(
            "SELECT set_config('lock_timeout', $1, false), set_config('statement_timeout', $2, false), set_config('max_parallel_maintenance_workers', $3, false)",
        )
        .bind(&previous.0)
        .bind(&previous.1)
        .bind(&previous.2)
        .execute(&mut *connection)
        .await
    }.await;
    if let Err(error) = restored {
        // A connection whose limits could not be restored must not reenter
        // the worker pool. Preserve the original CREATE failure if present.
        connection.close().await?;
        return Err(result.err().unwrap_or(error).into());
    }
    drop(connection);
    result?;
    validate_earn_history_index(pool).await
}

#[cfg(test)]
mod tests {
    use super::{build_mode, BuildMode};

    #[test]
    fn operator_build_modes_require_an_explicit_decision() {
        assert!(build_mode(None).is_err());
        assert!(build_mode(Some("")).is_err());
        assert!(build_mode(Some("automatic")).is_err());
        assert_eq!(
            build_mode(Some("existing-role")).unwrap(),
            BuildMode::ExistingRole
        );
        assert_eq!(
            build_mode(Some("bounded-temp")).unwrap(),
            BuildMode::BoundedTemp
        );
    }
}
