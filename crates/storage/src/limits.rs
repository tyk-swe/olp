use std::time::{Duration, SystemTime, UNIX_EPOCH};

use redis::{AsyncCommands, Script, aio::ConnectionManager};
use thiserror::Error;
use uuid::Uuid;

const RESERVE_SCRIPT: &str = include_str!("../scripts/reserve_limits.lua");
const RELEASE_SCRIPT: &str = include_str!("../scripts/release_concurrency.lua");
// Valkey executes Lua 5.1 scripts with IEEE-754 doubles. Keep every integer
// involved in a comparison or sorted-set score exactly representable.
const MAX_LUA_INTEGER: i64 = (1_i64 << 53) - 1;

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
        let connection = ConnectionManager::new(client).await?;
        Ok(Self {
            connection,
            namespace,
        })
    }

    /// Performs the full reservation in one Valkey script. A hard-limited key
    /// must treat any returned infrastructure error as fail-closed.
    pub async fn reserve(&self, request: LimitRequest<'_>) -> Result<LimitLease, LimitError> {
        request.validate()?;
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| LimitError::Clock)?;
        let now_ms = i64::try_from(now.as_millis()).map_err(|_| LimitError::Clock)?;
        if now_ms > MAX_LUA_INTEGER {
            return Err(LimitError::Clock);
        }
        let window = now_ms / 60_000;
        let window_ttl_ms = 60_000 - now_ms.rem_euclid(60_000);
        let hash_tag = format!("{{{}}}", request.lookup_id);
        let rpm_key = format!("{}:{}:rpm:{window}", self.namespace, hash_tag);
        let tpm_key = format!("{}:{}:tpm:{window}", self.namespace, hash_tag);
        let concurrency_key = format!("{}:{}:concurrency", self.namespace, hash_tag);
        let lease_id = Uuid::now_v7().to_string();
        let ttl_ms = duration_ms(request.lease_ttl)?;
        if ttl_ms > MAX_LUA_INTEGER - now_ms {
            return Err(LimitError::InvalidRequest(
                "concurrency lease expiry is outside Valkey Lua's safe integer range",
            ));
        }

        let mut connection = self.connection.clone();
        let response: (i64, String, i64) = Script::new(RESERVE_SCRIPT)
            .key(rpm_key)
            .key(tpm_key)
            .key(&concurrency_key)
            .arg(now_ms)
            .arg(window_ttl_ms)
            .arg(request.requests_per_minute.unwrap_or(0))
            .arg(request.tokens_per_minute.unwrap_or(0))
            .arg(request.requested_tokens)
            .arg(request.max_concurrency.unwrap_or(0))
            .arg(&lease_id)
            .arg(ttl_ms)
            .invoke_async(&mut connection)
            .await?;

        match (response.0, response.1.as_str()) {
            (1, "ok") if response.2 == 0 => Ok(LimitLease {
                lease_id,
                concurrency_key,
                has_concurrency_reservation: request.max_concurrency.is_some(),
            }),
            (0, "rpm" | "tpm" | "concurrency") if response.2 > 0 => Err(LimitError::Exceeded {
                dimension: match response.1.as_str() {
                    "rpm" => LimitDimension::Requests,
                    "tpm" => LimitDimension::Tokens,
                    "concurrency" => LimitDimension::Concurrency,
                    _ => unreachable!("match arm accepts only known dimensions"),
                },
                retry_after: Duration::from_millis(response.2 as u64),
            }),
            _ => Err(LimitError::UnexpectedResponse),
        }
    }

    pub async fn release(&self, lease: &LimitLease) -> Result<(), LimitError> {
        if !lease.has_concurrency_reservation {
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

    pub async fn ping(&self) -> Result<(), LimitError> {
        let mut connection = self.connection.clone();
        let pong: String = connection.ping().await?;
        if pong == "PONG" {
            Ok(())
        } else {
            Err(LimitError::UnexpectedResponse)
        }
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

#[derive(Debug, Clone)]
pub struct LimitLease {
    lease_id: String,
    concurrency_key: String,
    has_concurrency_reservation: bool,
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
    #[error("the system clock is before the Unix epoch")]
    Clock,
    #[error("Valkey returned an unexpected response")]
    UnexpectedResponse,
    #[error("invalid distributed limit request: {0}")]
    InvalidRequest(&'static str),
    #[error("{dimension:?} limit exceeded; retry after {retry_after:?}")]
    Exceeded {
        dimension: LimitDimension,
        retry_after: Duration,
    },
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

    #[test]
    fn identifies_keys_that_must_fail_closed() {
        let unlimited = LimitRequest {
            lookup_id: "lookup_01",
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: None,
            requested_tokens: 1,
            lease_ttl: Duration::from_secs(10),
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
        let valid = LimitRequest {
            lookup_id: "lookup_01",
            requests_per_minute: Some(60),
            tokens_per_minute: Some(1_000),
            max_concurrency: Some(4),
            requested_tokens: 1,
            lease_ttl: Duration::from_secs(10),
        };
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
    fn namespaces_cannot_override_the_redis_cluster_hash_tag() {
        assert!(validate_namespace("olp:v2:limits").is_ok());
        assert!(validate_namespace("olp:{shared}").is_err());
        assert!(validate_namespace("").is_err());
    }
}
