//! Atomic Valkey state transitions for distributed target circuits.

use std::{fmt, time::Duration};

use olp_domain::TargetId;
use redis::{Script, aio::ConnectionManager};
use thiserror::Error;
use uuid::Uuid;

const OBSERVE_SCRIPT: &str = include_str!("../scripts/observe_circuit.lua");
const ACQUIRE_SCRIPT: &str = include_str!("../scripts/acquire_circuit.lua");
const SUCCESS_SCRIPT: &str = include_str!("../scripts/record_circuit_success.lua");
const FAILURE_SCRIPT: &str = include_str!("../scripts/record_circuit_failure.lua");
const MAX_LUA_INTEGER: u64 = (1_u64 << 53) - 1;

#[derive(Clone)]
pub struct DistributedCircuitBreaker {
    connection: ConnectionManager,
    namespace: String,
}

impl DistributedCircuitBreaker {
    pub async fn connect(
        url: &str,
        namespace: impl Into<String>,
    ) -> Result<Self, CircuitStoreError> {
        let namespace = namespace.into();
        let connection = crate::valkey::valkey_connection(url).await?;
        Ok(Self {
            connection,
            namespace,
        })
    }

    pub async fn observe(&self, target: TargetId) -> Result<bool, CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: i64 = Script::new(OBSERVE_SCRIPT)
            .key(self.key(target))
            .invoke_async(&mut connection)
            .await?;
        parse_boolean_response(response)
    }

    pub async fn acquire(
        &self,
        target: TargetId,
        probe_lease: Duration,
        retention: Duration,
    ) -> Result<DistributedCircuitPermit, CircuitStoreError> {
        let token = Uuid::now_v7().to_string();
        let mut connection = self.connection.clone();
        let response: (String, String) = Script::new(ACQUIRE_SCRIPT)
            .key(self.key(target))
            .arg(duration_ms(probe_lease)?)
            .arg(duration_ms(retention)?)
            .arg(&token)
            .invoke_async(&mut connection)
            .await?;
        match response {
            (status, response_token) if status == "denied" && response_token.is_empty() => {
                Ok(DistributedCircuitPermit::Denied)
            }
            (status, response_token) if status == "closed" && response_token.is_empty() => {
                Ok(DistributedCircuitPermit::Acquired { probe_token: None })
            }
            (status, response_token) if status == "probe" && response_token == token => {
                Ok(DistributedCircuitPermit::Acquired {
                    probe_token: Some(token),
                })
            }
            _ => Err(CircuitStoreError::UnexpectedResponse),
        }
    }

    pub async fn record_success(
        &self,
        target: TargetId,
        probe_token: Option<&str>,
    ) -> Result<bool, CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: i64 = Script::new(SUCCESS_SCRIPT)
            .key(self.key(target))
            .arg(probe_token.unwrap_or(""))
            .invoke_async(&mut connection)
            .await?;
        parse_boolean_response(response)
    }

    pub async fn record_failure(
        &self,
        target: TargetId,
        probe_token: Option<&str>,
        failure_threshold: u32,
        open_duration: Duration,
        retention: Duration,
    ) -> Result<bool, CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: i64 = Script::new(FAILURE_SCRIPT)
            .key(self.key(target))
            .arg(probe_token.unwrap_or(""))
            .arg(failure_threshold.max(1))
            .arg(duration_ms(open_duration)?)
            .arg(duration_ms(retention)?)
            .invoke_async(&mut connection)
            .await?;
        parse_boolean_response(response)
    }

    pub async fn ping(&self) -> Result<(), CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: String = redis::cmd("PING").query_async(&mut connection).await?;
        if response == "PONG" {
            Ok(())
        } else {
            Err(CircuitStoreError::UnexpectedResponse)
        }
    }

    fn key(&self, target: TargetId) -> String {
        format!("{}:{}", self.namespace, target.as_uuid())
    }
}

impl fmt::Debug for DistributedCircuitBreaker {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DistributedCircuitBreaker")
            .field("namespace", &self.namespace)
            .finish_non_exhaustive()
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum DistributedCircuitPermit {
    Denied,
    Acquired { probe_token: Option<String> },
}

#[derive(Debug, Error)]
pub enum CircuitStoreError {
    #[error("Valkey circuit operation failed")]
    Service(#[from] redis::RedisError),
    #[error("invalid circuit configuration: {0}")]
    InvalidConfiguration(&'static str),
    #[error("Valkey returned an unexpected circuit response")]
    UnexpectedResponse,
}

fn duration_ms(duration: Duration) -> Result<u64, CircuitStoreError> {
    let milliseconds = u64::try_from(duration.as_millis()).unwrap_or(u64::MAX);
    if milliseconds == 0 || milliseconds > MAX_LUA_INTEGER {
        return Err(CircuitStoreError::InvalidConfiguration(
            "duration must be a positive Lua-safe integer",
        ));
    }
    Ok(milliseconds)
}

fn parse_boolean_response(response: i64) -> Result<bool, CircuitStoreError> {
    match response {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(CircuitStoreError::UnexpectedResponse),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_unsafe_durations() {
        assert!(duration_ms(Duration::ZERO).is_err());
    }
}
