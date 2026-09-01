use std::{fmt, sync::Arc, time::Duration};

use chrono::{DateTime, Utc};
use olp_engine::{
    domain::ports::BoxFuture,
    inference::limits::{
        LimitBackend, LimitDimension, LimitError, LimitLease as LimitLeasePort, LimitRequest,
    },
};
use redis::{AsyncCommands, FromRedisValue, Script, aio::ConnectionManager};
use rust_decimal::Decimal;
use thiserror::Error;
use uuid::Uuid;

use crate::store::Store;

const RESERVE_SCRIPT: &str = include_str!("../scripts/reserve_limits.lua");
const RESERVE_COST_SCRIPT: &str = include_str!("../scripts/reserve_cost.lua");
const RECONCILE_COST_SCRIPT: &str = include_str!("../scripts/reconcile_cost.lua");
const RELEASE_SCRIPT: &str = include_str!("../scripts/release_concurrency.lua");
const RECONCILE_SCRIPT: &str = include_str!("../scripts/reconcile_limits.lua");
const SCRIPT_RESPONSE_VERSION: i64 = 1;
const FIXED_WINDOW_MS: i64 = 60_000;
const DAY_MS: i64 = 86_400_000;
const MAX_MONTH_MS: i64 = 31 * DAY_MS;
// Valkey executes Lua 5.1 scripts with IEEE-754 doubles. Keep every integer
// involved in a comparison or sorted-set score exactly representable.
const MAX_LUA_INTEGER: i64 = (1_i64 << 53) - 1;

#[derive(Clone)]
pub struct DistributedLimiter {
    connection: ConnectionManager,
    namespace: String,
}

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

impl DistributedLimiter {
    pub async fn connect(url: &str, namespace: impl Into<String>) -> Result<Self, LimitError> {
        let namespace = namespace.into();
        validate_namespace(&namespace)?;
        let client = redis::Client::open(url).map_err(LimitError::service)?;
        let connection = ConnectionManager::new(client)
            .await
            .map_err(LimitError::service)?;
        Ok(Self {
            connection,
            namespace,
        })
    }

    /// Performs the full reservation in one Valkey script. A hard-limited key
    /// must treat any returned infrastructure or state error as fail-closed.
    pub async fn reserve(
        &self,
        request: LimitRequest<'_>,
    ) -> Result<DistributedLimitLease, LimitError> {
        request.validate()?;
        validate_lua_integer_ranges(&request)?;
        validate_cost_limits(&request)?;
        let keys = self.keys_for(request.lookup_id, request.api_key_id);
        self.reserve_cost(&request, &keys, 0).await?;
        let lease_id = Uuid::now_v7().to_string();
        let ttl_ms = duration_ms(request.lease_ttl)?;

        let mut connection = self.connection.clone();
        let raw_response: redis::Value = Script::new(RESERVE_SCRIPT)
            .key(&keys.rate)
            .key(&keys.concurrency)
            .arg(request.requests_per_minute.unwrap_or(0))
            .arg(request.tokens_per_minute.unwrap_or(0))
            .arg(request.requested_tokens)
            .arg(request.max_concurrency.unwrap_or(0))
            .arg(&lease_id)
            .arg(ttl_ms)
            .invoke_async(&mut connection)
            .await
            .map_err(LimitError::service)?;

        match ReservationScriptResult::parse_value(&raw_response)? {
            ReservationScriptResult::Granted {
                window_id,
                concurrency_expires_at_ms,
            } if request.max_concurrency.is_some() == concurrency_expires_at_ms.is_some() => {
                Ok(DistributedLimitLease {
                    lease_id,
                    rate_key: keys.rate,
                    concurrency_key: keys.concurrency,
                    rate_window_id: window_id,
                    reserved_tokens: request.requested_tokens,
                    has_token_reservation: request.tokens_per_minute.is_some(),
                    concurrency_expires_at_ms,
                })
            }
            ReservationScriptResult::Granted { .. } | ReservationScriptResult::ScriptFailure => {
                Err(LimitError::UnexpectedResponse)
            }
            ReservationScriptResult::Rejected {
                dimension,
                retry_after_ms,
            } => Err(LimitError::Exceeded {
                dimension,
                retry_after: Duration::from_millis(retry_after_ms),
            }),
            ReservationScriptResult::MalformedState => Err(LimitError::MalformedState),
        }
    }

    async fn reserve_cost(
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
        let snapshots = store.cost_reconciliation_snapshots(now).await?;
        let mut report = CostReconciliationReport::default();
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

    pub async fn release(&self, lease: &DistributedLimitLease) -> Result<(), LimitError> {
        if lease.concurrency_expires_at_ms.is_none() {
            return Ok(());
        }
        let mut connection = self.connection.clone();
        let _: i64 = Script::new(RELEASE_SCRIPT)
            .key(&lease.concurrency_key)
            .arg(&lease.lease_id)
            .invoke_async(&mut connection)
            .await
            .map_err(LimitError::service)?;
        Ok(())
    }

    pub async fn reconcile(
        &self,
        lease: &DistributedLimitLease,
        actual_tokens: i64,
    ) -> Result<(), LimitError> {
        if !lease.has_token_reservation {
            return Ok(());
        }
        if !(0..=MAX_LUA_INTEGER).contains(&actual_tokens) {
            return Err(LimitError::InvalidRequest(
                "actual_tokens must be a non-negative Lua-safe integer",
            ));
        }
        let adjustment = actual_tokens.saturating_sub(lease.reserved_tokens);

        if adjustment == 0 {
            return Ok(());
        }
        let mut connection = self.connection.clone();
        let _: i64 = Script::new(RECONCILE_SCRIPT)
            .key(&lease.rate_key)
            .arg(lease.rate_window_id)
            .arg(adjustment)
            .arg(&lease.lease_id)
            .invoke_async(&mut connection)
            .await
            .map_err(LimitError::service)?;
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), LimitError> {
        let mut connection = self.connection.clone();
        let pong: String = connection.ping().await.map_err(LimitError::service)?;
        if pong == "PONG" {
            Ok(())
        } else {
            Err(LimitError::UnexpectedResponse)
        }
    }

    fn keys_for(&self, lookup_id: &str, api_key_id: Uuid) -> LimitKeys {
        limit_keys(&self.namespace, lookup_id, api_key_id)
    }
}

fn validate_lua_integer_ranges(request: &LimitRequest<'_>) -> Result<(), LimitError> {
    for (name, value) in [
        ("requests_per_minute", request.requests_per_minute),
        ("tokens_per_minute", request.tokens_per_minute),
        ("max_concurrency", request.max_concurrency),
    ] {
        if value.is_some_and(|value| value > MAX_LUA_INTEGER) {
            return Err(LimitError::InvalidRequest(match name {
                "requests_per_minute" => "requests_per_minute exceeds the Valkey Lua integer range",
                "tokens_per_minute" => "tokens_per_minute exceeds the Valkey Lua integer range",
                _ => "max_concurrency exceeds the Valkey Lua integer range",
            }));
        }
    }
    if request.requested_tokens > MAX_LUA_INTEGER {
        return Err(LimitError::InvalidRequest(
            "requested_tokens exceeds the Valkey Lua integer range",
        ));
    }
    Ok(())
}

fn validate_cost_limits(request: &LimitRequest<'_>) -> Result<(), LimitError> {
    if request
        .daily_cost_limit
        .into_iter()
        .chain(request.monthly_cost_limit)
        .any(|value| !crate::valid_cost_limit(value))
    {
        return Err(LimitError::InvalidRequest(
            "cost limits must have at most 12 integer and 12 fractional digits",
        ));
    }
    Ok(())
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

struct ValkeyLimitLease {
    limiter: DistributedLimiter,
    lease: DistributedLimitLease,
}

impl LimitLeasePort for ValkeyLimitLease {
    fn reconcile(&self, actual_tokens: i64) -> BoxFuture<'_, Result<(), LimitError>> {
        Box::pin(self.limiter.reconcile(&self.lease, actual_tokens))
    }

    fn release(&self) -> BoxFuture<'_, Result<(), LimitError>> {
        Box::pin(self.limiter.release(&self.lease))
    }
}

impl LimitBackend for DistributedLimiter {
    fn reserve<'a>(
        &'a self,
        request: LimitRequest<'a>,
    ) -> BoxFuture<'a, Result<Arc<dyn LimitLeasePort>, LimitError>> {
        Box::pin(async move {
            let lease = DistributedLimiter::reserve(self, request).await?;
            Ok(Arc::new(ValkeyLimitLease {
                limiter: self.clone(),
                lease,
            }) as Arc<dyn LimitLeasePort>)
        })
    }

    fn ping(&self) -> BoxFuture<'_, Result<(), LimitError>> {
        Box::pin(DistributedLimiter::ping(self))
    }
}

struct LimitKeys {
    rate: String,
    concurrency: String,
    daily_cost: String,
    monthly_cost: String,
}

fn limit_keys(namespace: &str, lookup_id: &str, api_key_id: Uuid) -> LimitKeys {
    let rate_prefix = format!("{namespace}:{{{lookup_id}}}");
    let cost_prefix = format!("{namespace}:{{{}}}:cost", api_key_id.simple());
    LimitKeys {
        rate: format!("{rate_prefix}:rate"),
        concurrency: format!("{rate_prefix}:concurrency:v2"),
        daily_cost: format!("{cost_prefix}:day"),
        monthly_cost: format!("{cost_prefix}:month"),
    }
}

#[cfg(any(test, feature = "test-util"))]
fn supported_timestamp_ms(at: DateTime<Utc>) -> Result<i64, LimitError> {
    let milliseconds = at.timestamp_millis();
    if !(1..=MAX_LUA_INTEGER).contains(&milliseconds) {
        return Err(LimitError::InvalidRequest(
            "cost limiter timestamp exceeds the supported range",
        ));
    }
    Ok(milliseconds)
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

#[derive(Clone)]
pub struct DistributedLimitLease {
    lease_id: String,
    concurrency_key: String,
    rate_key: String,
    rate_window_id: i64,
    reserved_tokens: i64,
    has_token_reservation: bool,
    concurrency_expires_at_ms: Option<i64>,
}

impl fmt::Debug for DistributedLimitLease {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DistributedLimitLease")
            .field("lease_id", &"[REDACTED]")
            .field("rate_key", &"[REDACTED]")
            .field("concurrency_key", &"[REDACTED]")
            .field("rate_window_id", &self.rate_window_id)
            .field("reserved_tokens", &self.reserved_tokens)
            .field("has_token_reservation", &self.has_token_reservation)
            .field("concurrency_expires_at_ms", &self.concurrency_expires_at_ms)
            .finish()
    }
}

enum ReservationScriptResult {
    Granted {
        window_id: i64,
        concurrency_expires_at_ms: Option<i64>,
    },
    Rejected {
        dimension: LimitDimension,
        retry_after_ms: u64,
    },
    MalformedState,
    ScriptFailure,
}

enum CostReservationScriptResult {
    Granted,
    Rejected {
        dimension: LimitDimension,
        retry_after_ms: u64,
    },
    MalformedState,
    ScriptFailure,
}

impl CostReservationScriptResult {
    fn parse_value(value: &redis::Value) -> Result<Self, LimitError> {
        let (version, status, detail, retry_after_ms, day_window, month_window) =
            RawReservationScriptResponse::from_redis_value_ref(value)
                .map_err(|_| LimitError::UnexpectedResponse)?;
        if version != SCRIPT_RESPONSE_VERSION || day_window <= 0 || month_window <= 0 {
            return Err(LimitError::UnexpectedResponse);
        }
        match (status, detail.as_str()) {
            (1, "ok") if retry_after_ms == 0 => Ok(Self::Granted),
            (0, "daily_cost") if (1..=DAY_MS).contains(&retry_after_ms) => Ok(Self::Rejected {
                dimension: LimitDimension::DailyCost,
                retry_after_ms: retry_after_ms as u64,
            }),
            (0, "monthly_cost") if (1..=MAX_MONTH_MS).contains(&retry_after_ms) => {
                Ok(Self::Rejected {
                    dimension: LimitDimension::MonthlyCost,
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (-1, "malformed_daily_cost_state" | "malformed_monthly_cost_state")
                if retry_after_ms == 0 =>
            {
                Ok(Self::MalformedState)
            }
            (-1, "invalid_arguments" | "invalid_server_time") if retry_after_ms == 0 => {
                Ok(Self::ScriptFailure)
            }
            _ => Err(LimitError::UnexpectedResponse),
        }
    }
}

type RawReservationScriptResponse = (i64, i64, String, i64, i64, i64);

impl ReservationScriptResult {
    fn parse_value(value: &redis::Value) -> Result<Self, LimitError> {
        let response = RawReservationScriptResponse::from_redis_value_ref(value)
            .map_err(|_| LimitError::UnexpectedResponse)?;
        Self::parse(response)
    }

    fn parse(
        (version, status, detail, retry_after_ms, window_id, lease_expiry_ms): RawReservationScriptResponse,
    ) -> Result<Self, LimitError> {
        if version != SCRIPT_RESPONSE_VERSION
            || !(0..=MAX_LUA_INTEGER).contains(&window_id)
            || !(0..=MAX_LUA_INTEGER).contains(&lease_expiry_ms)
        {
            return Err(LimitError::UnexpectedResponse);
        }

        match (status, detail.as_str()) {
            (1, "ok") if retry_after_ms == 0 && window_id > 0 => Ok(Self::Granted {
                window_id,
                concurrency_expires_at_ms: (lease_expiry_ms > 0).then_some(lease_expiry_ms),
            }),
            (0, "rpm" | "tpm")
                if (1..=FIXED_WINDOW_MS).contains(&retry_after_ms)
                    && window_id > 0
                    && lease_expiry_ms == 0 =>
            {
                Ok(Self::Rejected {
                    dimension: if detail == "rpm" {
                        LimitDimension::Requests
                    } else {
                        LimitDimension::Tokens
                    },
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (0, "concurrency")
                if (1..=MAX_LUA_INTEGER).contains(&retry_after_ms)
                    && window_id > 0
                    && lease_expiry_ms == 0 =>
            {
                Ok(Self::Rejected {
                    dimension: LimitDimension::Concurrency,
                    retry_after_ms: retry_after_ms as u64,
                })
            }
            (-1, "malformed_rate_state" | "malformed_concurrency_state")
                if retry_after_ms == 0 && window_id > 0 && lease_expiry_ms == 0 =>
            {
                Ok(Self::MalformedState)
            }
            (-1, "invalid_arguments" | "invalid_server_time")
                if retry_after_ms == 0 && window_id == 0 && lease_expiry_ms == 0 =>
            {
                Ok(Self::ScriptFailure)
            }
            _ => Err(LimitError::UnexpectedResponse),
        }
    }
}

fn duration_ms(duration: Duration) -> Result<i64, LimitError> {
    let milliseconds = i64::try_from(duration.as_millis()).map_err(|_| {
        LimitError::InvalidRequest("concurrency lease TTL exceeds the supported range")
    })?;
    if milliseconds > MAX_LUA_INTEGER {
        return Err(LimitError::InvalidRequest(
            "concurrency lease TTL exceeds Valkey Lua's safe integer range",
        ));
    }
    Ok(milliseconds)
}

fn validate_namespace(namespace: &str) -> Result<(), LimitError> {
    if namespace.is_empty()
        || namespace.len() > 128
        || !namespace
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b':' | b'_' | b'-'))
    {
        return Err(LimitError::InvalidRequest(
            "Valkey namespace must be 1-128 ASCII letters, digits, colons, underscores, or hyphens",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid_request() -> LimitRequest<'static> {
        LimitRequest {
            api_key_id: Uuid::nil(),
            lookup_id: "lookup_01",
            requests_per_minute: Some(60),
            tokens_per_minute: Some(1_000),
            max_concurrency: Some(4),
            daily_cost_limit: Some(Decimal::new(1, 2)),
            monthly_cost_limit: Some(Decimal::ONE),
            requested_tokens: 1,
            lease_ttl: Duration::from_secs(10),
        }
    }

    #[test]
    fn identifies_keys_that_must_fail_closed() {
        let unlimited = LimitRequest {
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: None,
            daily_cost_limit: None,
            monthly_cost_limit: None,
            ..valid_request()
        };
        assert!(!unlimited.has_hard_limits());

        assert!(
            LimitRequest {
                requests_per_minute: Some(60),
                ..unlimited
            }
            .has_hard_limits()
        );
    }

    #[test]
    fn rejects_values_that_would_disable_or_bypass_hard_limits() {
        let valid = valid_request();
        assert!(valid.validate().is_ok());

        for invalid in [
            LimitRequest {
                requests_per_minute: Some(0),
                ..valid.clone()
            },
            LimitRequest {
                tokens_per_minute: Some(-1),
                ..valid.clone()
            },
            LimitRequest {
                requested_tokens: -1,
                ..valid.clone()
            },
            LimitRequest {
                requested_tokens: 0,
                ..valid.clone()
            },
            LimitRequest {
                max_concurrency: Some(MAX_LUA_INTEGER + 1),
                ..valid.clone()
            },
            LimitRequest {
                lease_ttl: Duration::ZERO,
                ..valid.clone()
            },
            LimitRequest {
                lookup_id: "bad}{slot",
                ..valid
            },
        ] {
            let result = invalid
                .validate()
                .and_then(|()| validate_lua_integer_ranges(&invalid));
            assert!(matches!(result, Err(LimitError::InvalidRequest(_))));
        }
    }

    #[test]
    fn stable_keys_share_one_cluster_hash_tag() {
        let api_key_id = Uuid::now_v7();
        let first = limit_keys("olp:v2:limits", "lookup_01", api_key_id);
        let second = limit_keys("olp:v2:limits", "lookup_02", api_key_id);

        assert_eq!(hash_tag(&first.rate), Some("lookup_01"));
        assert_eq!(hash_tag(&first.concurrency), Some("lookup_01"));
        assert_ne!(hash_tag(&first.rate), hash_tag(&second.rate));
        assert!(!first.rate.contains(":rpm:"));
        assert!(!first.rate.contains(":tpm:"));
        assert_eq!(first.daily_cost, second.daily_cost);
        assert_eq!(first.monthly_cost, second.monthly_cost);
        assert_eq!(hash_tag(&first.daily_cost), hash_tag(&first.monthly_cost));
    }

    #[test]
    fn reservation_contract_uses_valkey_time_and_no_timestamp_argument() {
        assert!(RESERVE_SCRIPT.contains("redis.call(\"TIME\")"));
        assert!(RESERVE_SCRIPT.contains("#ARGV ~= 6"));
        assert!(!RESERVE_SCRIPT.contains("ARGV: now_ms"));
        assert!(!RESERVE_SCRIPT.contains("window_ttl_ms"));
        assert!(RESERVE_COST_SCRIPT.contains("redis.call(\"TIME\")"));
        assert!(RESERVE_COST_SCRIPT.contains("#ARGV ~= 3"));
    }

    #[test]
    fn script_result_parser_rejects_malformed_responses() {
        assert!(matches!(
            ReservationScriptResult::parse_value(&redis::Value::Array(Vec::new())),
            Err(LimitError::UnexpectedResponse)
        ));
        for response in [
            (2, 1, "ok".to_owned(), 0, 1, 0),
            (1, 1, "ok".to_owned(), 1, 1, 0),
            (1, 0, "rpm".to_owned(), 0, 1, 0),
            (1, 0, "rpm".to_owned(), 60_001, 1, 0),
            (1, 0, "rpm".to_owned(), 1, 0, 0),
            (1, 0, "rpm".to_owned(), 1, 1, 1),
            (1, 0, "unknown".to_owned(), 1, 1, 0),
            (1, -1, "malformed_rate_state".to_owned(), 1, 1, 0),
        ] {
            assert!(matches!(
                ReservationScriptResult::parse(response),
                Err(LimitError::UnexpectedResponse)
            ));
        }
    }

    #[test]
    fn lease_debug_redacts_internal_identifiers() {
        let lease = DistributedLimitLease {
            lease_id: "private-lease".to_owned(),
            rate_key: "private-rate-key".to_owned(),
            concurrency_key: "private-concurrency-key".to_owned(),
            rate_window_id: 1,
            reserved_tokens: 2,
            has_token_reservation: true,
            concurrency_expires_at_ms: Some(3),
        };
        let debug = format!("{lease:?}");
        assert!(!debug.contains("private-lease"));
        assert!(!debug.contains("private-rate-key"));
        assert!(!debug.contains("private-concurrency-key"));
        assert!(debug.contains("[REDACTED]"));
    }

    #[test]
    fn namespaces_cannot_override_the_redis_cluster_hash_tag() {
        assert!(validate_namespace("olp:v2:limits").is_ok());
        assert!(validate_namespace("olp:{shared}").is_err());
        assert!(validate_namespace("").is_err());
    }

    fn hash_tag(key: &str) -> Option<&str> {
        let start = key.find('{')? + 1;
        let end = key[start..].find('}')? + start;
        (end > start).then_some(&key[start..end])
    }
}
