use chrono::{DateTime, Utc};
use sqlx::{Connection as _, PgConnection};

use super::{CostReconciliationError, CostReconciliationReport, DistributedLimiter};
use crate::{error::Error, store::Store};

const COST_RECONCILIATION_LOCK_ID: i64 = 0x4f4c_505f_4352; // "OLP_CR"

/// Owns the fleet lock on a detached session. Dropping it closes the session,
/// including on cancellation, instead of returning a locked connection to the pool.
pub struct CostReconciliationLeader {
    connection: PgConnection,
}

impl Store {
    pub async fn try_acquire_cost_reconciliation_leader(
        &self,
    ) -> Result<Option<CostReconciliationLeader>, Error> {
        let mut connection = self.pool().acquire().await?.detach();
        let acquired = sqlx::query_scalar!(
            "SELECT pg_try_advisory_lock($1) AS \"acquired!\"",
            COST_RECONCILIATION_LOCK_ID
        )
        .fetch_one(&mut connection)
        .await?;
        if !acquired {
            connection.close().await?;
            return Ok(None);
        }
        Ok(Some(CostReconciliationLeader { connection }))
    }
}

impl CostReconciliationLeader {
    pub async fn reconcile(
        &mut self,
        limiter: &DistributedLimiter,
        now: DateTime<Utc>,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        self.reconcile_at(limiter, now, 0).await
    }

    pub(super) async fn reconcile_at(
        &mut self,
        limiter: &DistributedLimiter,
        now: DateTime<Utc>,
        now_override_ms: i64,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        let snapshots = Store::cost_reconciliation_snapshots_on(&mut self.connection, now).await?;
        let report = limiter
            .apply_cost_snapshots(snapshots, now_override_ms)
            .await?;
        // A pass cannot report success after losing the session that owns its lock.
        self.connection.ping().await.map_err(Error::from)?;
        Ok(report)
    }
}
