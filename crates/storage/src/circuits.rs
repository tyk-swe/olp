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
const RESPONSE_VERSION: i64 = 1;
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
        validate_namespace(&namespace)?;
        let client = redis::Client::open(url)?;
        let connection = ConnectionManager::new(client).await?;
        Ok(Self {
            connection,
            namespace,
        })
    }

    pub async fn observe(&self, target: TargetId) -> Result<bool, CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: Vec<i64> = Script::new(OBSERVE_SCRIPT)
            .key(self.key(target))
            .invoke_async(&mut connection)
            .await?;
        parse_boolean_response(&response)
    }

    pub async fn acquire(
        &self,
        target: TargetId,
        probe_lease: Duration,
        retention: Duration,
    ) -> Result<DistributedCircuitPermit, CircuitStoreError> {
        let token = Uuid::now_v7().to_string();
        let mut connection = self.connection.clone();
        let response: Vec<String> = Script::new(ACQUIRE_SCRIPT)
            .key(self.key(target))
            .arg(duration_ms(probe_lease)?)
            .arg(duration_ms(retention)?)
            .arg(&token)
            .invoke_async(&mut connection)
            .await?;
        if response.len() != 3 || response[0] != RESPONSE_VERSION.to_string() {
            return Err(CircuitStoreError::UnexpectedResponse);
        }
        match response[1].as_str() {
            "denied" if response[2].is_empty() => Ok(DistributedCircuitPermit::Denied),
            "closed" if response[2].is_empty() => {
                Ok(DistributedCircuitPermit::Acquired { probe_token: None })
            }
            "probe" if response[2] == token => Ok(DistributedCircuitPermit::Acquired {
                probe_token: Some(token),
            }),
            _ => Err(CircuitStoreError::UnexpectedResponse),
        }
    }

    pub async fn record_success(
        &self,
        target: TargetId,
        probe_token: Option<&str>,
    ) -> Result<bool, CircuitStoreError> {
        let mut connection = self.connection.clone();
        let response: Vec<i64> = Script::new(SUCCESS_SCRIPT)
            .key(self.key(target))
            .arg(probe_token.unwrap_or(""))
            .invoke_async(&mut connection)
            .await?;
        parse_ack_response(&response)
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
        let response: Vec<i64> = Script::new(FAILURE_SCRIPT)
            .key(self.key(target))
            .arg(probe_token.unwrap_or(""))
            .arg(failure_threshold.max(1))
            .arg(duration_ms(open_duration)?)
            .arg(duration_ms(retention)?)
            .invoke_async(&mut connection)
            .await?;
        parse_ack_response(&response)
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

fn validate_namespace(namespace: &str) -> Result<(), CircuitStoreError> {
    if namespace.is_empty() || namespace.len() > 512 || namespace.chars().any(char::is_whitespace) {
        return Err(CircuitStoreError::InvalidConfiguration(
            "namespace must be 1..=512 non-whitespace bytes",
        ));
    }
    Ok(())
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

fn parse_boolean_response(response: &[i64]) -> Result<bool, CircuitStoreError> {
    match response {
        [RESPONSE_VERSION, 0] => Ok(false),
        [RESPONSE_VERSION, 1] => Ok(true),
        _ => Err(CircuitStoreError::UnexpectedResponse),
    }
}

fn parse_ack_response(response: &[i64]) -> Result<bool, CircuitStoreError> {
    match response {
        [RESPONSE_VERSION, applied @ (0 | 1)] => Ok(*applied == 1),
        _ => Err(CircuitStoreError::UnexpectedResponse),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_unsafe_namespaces_and_durations() {
        assert!(validate_namespace("").is_err());
        assert!(validate_namespace("has whitespace").is_err());
        assert!(duration_ms(Duration::ZERO).is_err());
    }
}
