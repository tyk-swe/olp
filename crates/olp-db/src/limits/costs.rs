#[cfg(any(test, feature = "test-util"))]
use super::supported_timestamp_ms;
use super::{
    DistributedLimiter, LimitKeys, MAX_LUA_INTEGER, RECONCILE_COST_SCRIPT, RESERVE_COST_SCRIPT,
    SCRIPT_RESPONSE_VERSION, script_results::CostReservationScriptResult, validate_cost_limits,
};
use crate::store::Store;
use chrono::{DateTime, Utc};
use olp_engine::inference::limits::{LimitError, LimitRequest};
use redis::{Script, aio::ConnectionManager};
use rust_decimal::Decimal;
use std::time::Duration;
use thiserror::Error;
use uuid::Uuid;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CostSnapshot {
    pub api_key_id: Uuid,
    pub daily_window_id: i64,
    pub daily_accrued: Decimal,
    pub monthly_window_id: i64,
    pub monthly_accrued: Decimal,
    pub unpriced_attempts: u64,
}

impl CostSnapshot {
    fn validate(self) -> Result<(), LimitError> {
        if !(0..=MAX_LUA_INTEGER).contains(&self.daily_window_id)
            || !(0..=MAX_LUA_INTEGER).contains(&self.monthly_window_id)
        {
            return Err(LimitError::InvalidRequest(
                "cost snapshot window IDs exceed the Valkey Lua integer range",
            ));
        }
        if [self.daily_accrued, self.monthly_accrued]
            .into_iter()
            .any(|cost| cost.is_sign_negative() || cost.scale() > 12)
        {
            return Err(LimitError::InvalidRequest(
                "snapshot cost must be non-negative with at most 12 fractional digits",
            ));
        }
        if self.unpriced_attempts > MAX_LUA_INTEGER as u64 {
            return Err(LimitError::InvalidRequest(
                "unpriced attempt count exceeds the Valkey Lua integer range",
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CostReconciliationReport {
    pub lock_acquired: bool,
    pub keys_reconciled: u64,
    pub daily_windows_reconciled: u64,
    pub monthly_windows_reconciled: u64,
}

#[derive(Debug, Error)]
pub enum CostReconciliationError {
    #[error(transparent)]
    Persistence(#[from] crate::error::Error),
    #[error(transparent)]
    Limiter(#[from] LimitError),
}

#[derive(Debug, Error)]
#[error("cost state awaits PostgreSQL reconciliation for the current UTC window")]
struct UninitializedCostState;

impl DistributedLimiter {
    pub(super) async fn reserve_cost(
        &self,
        request: &LimitRequest<'_>,
        keys: &LimitKeys,
        now_override_ms: i64,
    ) -> Result<(), LimitError> {
        if request.daily_cost_limit.is_none() && request.monthly_cost_limit.is_none() {
            return Ok(());
        }
        let mut connection = self.connection.clone();
        let raw_response: redis::Value = Script::new(RESERVE_COST_SCRIPT)
            .key(&keys.daily_cost)
            .key(&keys.monthly_cost)
            .arg(canonical_optional_decimal(request.daily_cost_limit))
            .arg(canonical_optional_decimal(request.monthly_cost_limit))
            .arg(now_override_ms)
            .invoke_async(&mut connection)
            .await
            .map_err(LimitError::service)?;
        match CostReservationScriptResult::parse_value(&raw_response)? {
            CostReservationScriptResult::Granted => Ok(()),
            CostReservationScriptResult::UninitializedState => {
                Err(LimitError::service(UninitializedCostState))
            }
            CostReservationScriptResult::Rejected {
                dimension,
                retry_after_ms,
            } => Err(LimitError::Exceeded {
                dimension,
                retry_after: Duration::from_millis(retry_after_ms),
            }),
            CostReservationScriptResult::MalformedState => Err(LimitError::MalformedState),
            CostReservationScriptResult::ScriptFailure => Err(LimitError::UnexpectedResponse),
        }
    }

    #[cfg(any(test, feature = "test-util"))]
    pub async fn reserve_cost_at(
        &self,
        request: &LimitRequest<'_>,
        now: DateTime<Utc>,
    ) -> Result<(), LimitError> {
        request.validate()?;
        validate_cost_limits(request)?;
        let keys = self.keys_for(request.lookup_id, request.api_key_id);
        self.reserve_cost(request, &keys, supported_timestamp_ms(now)?)
            .await
    }

    pub async fn apply_cost_snapshot(&self, snapshot: &CostSnapshot) -> Result<(), LimitError> {
        self.apply_cost_snapshot_inner(snapshot, 0)
            .await
            .map(|_| ())
    }

    #[cfg(any(test, feature = "test-util"))]
    pub async fn apply_cost_snapshot_at(
        &self,
        snapshot: &CostSnapshot,
        now: DateTime<Utc>,
    ) -> Result<(), LimitError> {
        self.apply_cost_snapshot_inner(snapshot, supported_timestamp_ms(now)?)
            .await
            .map(|_| ())
    }

    async fn apply_cost_snapshot_inner(
        &self,
        snapshot: &CostSnapshot,
        now_override_ms: i64,
    ) -> Result<ReconciledCostWindows, LimitError> {
        snapshot.validate()?;
        let keys = self.keys_for("unused_lookup", snapshot.api_key_id);
        let mut connection = self.connection.clone();
        apply_cost_snapshot_on(&mut connection, &keys, snapshot, now_override_ms).await
    }

    pub async fn reconcile_costs(
        &self,
        store: &Store,
        now: DateTime<Utc>,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        self.reconcile_costs_inner(store, now, 0).await
    }

    #[cfg(any(test, feature = "test-util"))]
    pub async fn reconcile_costs_at(
        &self,
        store: &Store,
        now: DateTime<Utc>,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        let now_override_ms = supported_timestamp_ms(now)?;
        self.reconcile_costs_inner(store, now, now_override_ms)
            .await
    }

    async fn reconcile_costs_inner(
        &self,
        store: &Store,
        now: DateTime<Utc>,
        now_override_ms: i64,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        let Some(mut leader) = store.try_acquire_cost_reconciliation_leader().await? else {
            return Ok(CostReconciliationReport::default());
        };
        leader.reconcile_at(self, now, now_override_ms).await
    }

    pub(super) async fn apply_cost_snapshots(
        &self,
        snapshots: Vec<CostSnapshot>,
        now_override_ms: i64,
    ) -> Result<CostReconciliationReport, CostReconciliationError> {
        let mut report = CostReconciliationReport {
            lock_acquired: true,
            ..CostReconciliationReport::default()
        };
        let mut first_error = None;
        for snapshot in snapshots {
            match self
                .apply_cost_snapshot_inner(&snapshot, now_override_ms)
                .await
            {
                Ok(reconciled) => {
                    report.keys_reconciled += u64::from(reconciled.daily || reconciled.monthly);
                    report.daily_windows_reconciled += u64::from(reconciled.daily);
                    report.monthly_windows_reconciled += u64::from(reconciled.monthly);
                }
                Err(error) if first_error.is_none() => first_error = Some(error),
                Err(_) => {}
            }
        }
        if let Some(error) = first_error {
            return Err(error.into());
        }
        Ok(report)
    }
}

async fn apply_cost_snapshot_on(
    connection: &mut ConnectionManager,
    keys: &LimitKeys,
    snapshot: &CostSnapshot,
    now_override_ms: i64,
) -> Result<ReconciledCostWindows, LimitError> {
    let response: (i64, i64, String, i64, i64) = Script::new(RECONCILE_COST_SCRIPT)
        .key(&keys.daily_cost)
        .key(&keys.monthly_cost)
        .arg(snapshot.daily_window_id)
        .arg(canonical_decimal(snapshot.daily_accrued))
        .arg(snapshot.monthly_window_id)
        .arg(canonical_decimal(snapshot.monthly_accrued))
        .arg(snapshot.unpriced_attempts)
        .arg(now_override_ms)
        .invoke_async(connection)
        .await
        .map_err(LimitError::service)?;
    match response {
        (SCRIPT_RESPONSE_VERSION, 1, detail, daily, monthly)
            if detail == "ok" && matches!(daily, 0 | 1) && matches!(monthly, 0 | 1) =>
        {
            Ok(ReconciledCostWindows {
                daily: daily == 1,
                monthly: monthly == 1,
            })
        }
        (SCRIPT_RESPONSE_VERSION, -1, detail, 0, 0)
            if matches!(
                detail.as_str(),
                "malformed_daily_cost_state" | "malformed_monthly_cost_state"
            ) =>
        {
            Err(LimitError::MalformedState)
        }
        _ => Err(LimitError::UnexpectedResponse),
    }
}

fn canonical_optional_decimal(value: Option<Decimal>) -> String {
    value.map_or_else(String::new, canonical_decimal)
}

fn canonical_decimal(value: Decimal) -> String {
    value.normalize().to_string()
}

struct ReconciledCostWindows {
    daily: bool,
    monthly: bool,
}
