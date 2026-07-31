use std::{fmt, time::Duration};

use redis::{AsyncCommands, FromRedisValue, Script, aio::ConnectionManager};
use thiserror::Error;
use uuid::Uuid;

use olp_domain::MAX_TOKENS_PER_MINUTE;

const RESERVE_SCRIPT: &str = include_str!("../scripts/reserve_limits.lua");
const RELEASE_SCRIPT: &str = include_str!("../scripts/release_concurrency.lua");
const RECONCILE_SCRIPT: &str = include_str!("../scripts/reconcile_limits.lua");
const SCRIPT_RESPONSE_VERSION: i64 = 1;
const FIXED_WINDOW_MS: i64 = 60_000;
// Valkey executes Lua 5.1 scripts with IEEE-754 doubles. Keep every integer
// involved in a comparison or sorted-set score exactly representable.
const MAX_LUA_INTEGER: i64 = MAX_TOKENS_PER_MINUTE as i64;

#[derive(Clone)]
pub struct DistributedLimiter {
    connection: ConnectionManager,
    namespace: String,
}

impl DistributedLimiter {
    pub async fn connect(url: &str, namespace: impl Into<String>) -> Result<Self, LimitError> {
        let namespace = namespace.into();
        validate_namespace(&namespace)?;
        let client = redis::Client::open(url)?;
        let mut connection = ConnectionManager::new(client).await?;
        if !crate::valkey::supports_hash_field_expiration(&mut connection).await? {
            return Err(LimitError::UnsupportedServer);
        }
        Ok(Self {
            connection,
            namespace,
        })
    }

    /// Performs the full reservation in one Valkey script. A hard-limited key
    /// must treat any returned infrastructure or state error as fail-closed.
    pub async fn reserve(&self, request: LimitRequest<'_>) -> Result<LimitLease, LimitError> {
        request.validate()?;
        let keys = self.keys_for(request.lookup_id);
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
            .await?;

        match ReservationScriptResult::parse_value(&raw_response)? {
            ReservationScriptResult::Granted {
                window_id,
                concurrency_expires_at_ms,
            } if request.max_concurrency.is_some() == concurrency_expires_at_ms.is_some() => {
                Ok(LimitLease {
                    lease_id,
                    rate_key: keys.rate,
                    concurrency_key: keys.concurrency,
                    rate_window_id: window_id,
                    reserved_tokens: request.requested_tokens,
                    has_token_reservation: request.tokens_per_minute.is_some(),
                    concurrency_expires_at_ms,
                })
            }
            ReservationScriptResult::Granted { .. } => Err(LimitError::UnexpectedResponse),
            ReservationScriptResult::Rejected {
                dimension,
                retry_after_ms,
            } => Err(LimitError::Exceeded {
                dimension,
                retry_after: Duration::from_millis(retry_after_ms),
            }),
            ReservationScriptResult::MalformedState => Err(LimitError::MalformedState),
            ReservationScriptResult::ScriptFailure => Err(LimitError::UnexpectedResponse),
        }
    }

    pub async fn release(&self, lease: &LimitLease) -> Result<(), LimitError> {
        if lease.concurrency_expires_at_ms.is_none() {
            return Ok(());
        }
        let mut connection = self.connection.clone();
        let _: i64 = Script::new(RELEASE_SCRIPT)
            .key(&lease.concurrency_key)
            .arg(&lease.lease_id)
            .invoke_async(&mut connection)
            .await?;
        Ok(())
    }

    pub async fn reconcile(
        &self,
        lease: &LimitLease,
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
        // An exact reservation has nothing to apply, so the script would only
        // rewrite `tpm` to its current value and leave a retained
        // `reconciled:<lease>` marker behind. Skipping it keeps the shared rate
        // hash from growing by one field per request for the retention window.
        if actual_tokens == lease.reserved_tokens {
            return Ok(());
        }
        let mut connection = self.connection.clone();
        let result: i64 = Script::new(RECONCILE_SCRIPT)
            .key(&lease.rate_key)
            .arg(lease.rate_window_id)
            .arg(&lease.lease_id)
            .arg(lease.reserved_tokens)
            .arg(actual_tokens)
            .invoke_async(&mut connection)
            .await?;
        match result {
            0 | 1 => Ok(()),
            _ => Err(LimitError::MalformedState),
        }
    }

    pub async fn ping(&self) -> Result<(), LimitError> {
        let mut connection = self.connection.clone();
        let pong: String = connection.ping().await?;
        if pong == "PONG" {
            Ok(())
        } else {
            Err(LimitError::UnexpectedResponse)
        }
    }

    fn keys_for(&self, lookup_id: &str) -> LimitKeys {
        limit_keys(&self.namespace, lookup_id)
    }
}

struct LimitKeys {
    rate: String,
    concurrency: String,
}

fn limit_keys(namespace: &str, lookup_id: &str) -> LimitKeys {
    let prefix = format!("{namespace}:{{{lookup_id}}}");
    LimitKeys {
        rate: format!("{prefix}:rate"),
        concurrency: format!("{prefix}:concurrency:v2"),
    }
}

#[derive(Debug, Clone)]
pub struct LimitRequest<'a> {
    pub lookup_id: &'a str,
    pub requests_per_minute: Option<i64>,
    pub tokens_per_minute: Option<i64>,
    pub max_concurrency: Option<i64>,
    pub requested_tokens: i64,
    pub lease_ttl: Duration,
}

impl LimitRequest<'_> {
    pub fn has_hard_limits(&self) -> bool {
        self.requests_per_minute.is_some()
            || self.tokens_per_minute.is_some()
            || self.max_concurrency.is_some()
    }

    fn validate(&self) -> Result<(), LimitError> {
        if !(8..=40).contains(&self.lookup_id.len())
            || !self
                .lookup_id
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(LimitError::InvalidRequest(
                "API key lookup ID must be 8-40 ASCII letters, digits, or underscores",
            ));
        }

        for (name, value) in [
            ("requests_per_minute", self.requests_per_minute),
            ("tokens_per_minute", self.tokens_per_minute),
            ("max_concurrency", self.max_concurrency),
        ] {
            if value.is_some_and(|value| !(1..=MAX_LUA_INTEGER).contains(&value)) {
                return Err(LimitError::InvalidRequest(match name {
                    "requests_per_minute" => {
                        "requests_per_minute must be a positive Lua-safe integer"
                    }
                    "tokens_per_minute" => "tokens_per_minute must be a positive Lua-safe integer",
                    _ => "max_concurrency must be a positive Lua-safe integer",
                }));
            }
        }

        if !(0..=MAX_LUA_INTEGER).contains(&self.requested_tokens) {
            return Err(LimitError::InvalidRequest(
                "requested_tokens must be a non-negative Lua-safe integer",
            ));
        }
        if self.tokens_per_minute.is_some() && self.requested_tokens == 0 {
            return Err(LimitError::InvalidRequest(
                "requested_tokens must be positive when a token limit is configured",
            ));
        }
        if self.lease_ttl.is_zero() {
            return Err(LimitError::InvalidRequest(
                "concurrency lease TTL must be greater than zero",
            ));
        }
        Ok(())
    }
}

#[derive(Clone)]
pub struct LimitLease {
    lease_id: String,
    concurrency_key: String,
    rate_key: String,
    rate_window_id: i64,
    reserved_tokens: i64,
    has_token_reservation: bool,
    concurrency_expires_at_ms: Option<i64>,
}

impl fmt::Debug for LimitLease {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("LimitLease")
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

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LimitDimension {
    Requests,
    Tokens,
    Concurrency,
    Unknown,
}

#[derive(Debug, Error)]
pub enum LimitError {
    #[error("Valkey limit service failed")]
    Service(#[from] redis::RedisError),
    #[error("Valkey distributed limit state is malformed")]
    MalformedState,
    #[error("Valkey returned an unexpected response")]
    UnexpectedResponse,
    #[error("Valkey 9.0+ or Redis 7.4+ is required for hash-field expiration")]
    UnsupportedServer,
    #[error("invalid distributed limit request: {0}")]
    InvalidRequest(&'static str),
    #[error("{dimension:?} limit exceeded; retry after {retry_after:?}")]
    Exceeded {
        dimension: LimitDimension,
        retry_after: Duration,
    },
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
            lookup_id: "lookup_01",
            requests_per_minute: Some(60),
            tokens_per_minute: Some(1_000),
            max_concurrency: Some(4),
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
            assert!(matches!(
                invalid.validate(),
                Err(LimitError::InvalidRequest(_))
            ));
        }
    }

    #[test]
    fn stable_keys_share_one_cluster_hash_tag() {
        let first = limit_keys("olp:v2:limits", "lookup_01");
        let second = limit_keys("olp:v2:limits", "lookup_02");

        assert_eq!(hash_tag(&first.rate), Some("lookup_01"));
        assert_eq!(hash_tag(&first.concurrency), Some("lookup_01"));
        assert_ne!(hash_tag(&first.rate), hash_tag(&second.rate));
        assert!(!first.rate.contains(":rpm:"));
        assert!(!first.rate.contains(":tpm:"));
    }

    #[test]
    fn reservation_contract_uses_valkey_time_and_no_timestamp_argument() {
        assert!(RESERVE_SCRIPT.contains("redis.call(\"TIME\")"));
        assert!(RESERVE_SCRIPT.contains("#ARGV ~= 6"));
        assert!(!RESERVE_SCRIPT.contains("ARGV: now_ms"));
        assert!(!RESERVE_SCRIPT.contains("window_ttl_ms"));
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
        let lease = LimitLease {
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
