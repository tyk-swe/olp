use std::time::Duration;

use olp_engine::inference::request_metadata::Receiver;
use redis::{Client, RedisError, aio::ConnectionManager};
use tokio::sync::watch;

use crate::valkey::Error;

const STREAM_WRITE_TIMEOUT: Duration = Duration::from_secs(1);

/// Establishes the initial Valkey connection without dropping the bounded
/// local queue when Valkey starts after the gateway. The connection manager
/// handles subsequent reconnects.
pub async fn run_connecting(
    mut receiver: Receiver,
    valkey_url: &str,
    stream: &str,
    mut shutdown: watch::Receiver<bool>,
) -> Result<(), Error> {
    let client = match Client::open(valkey_url) {
        Ok(client) => client,
        Err(error) => {
            receiver.abandon_and_drain(0).await;
            return Err(error.into());
        }
    };
    let mut backoff = Duration::from_millis(100);
    receiver.set_retrying(true);
    loop {
        let connection = tokio::select! {
            biased;
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    receiver.abandon_and_drain(0).await;
                    return Ok(());
                }
                continue;
            }
            connection = ConnectionManager::new(client.clone()) => connection,
        };
        if let Ok(connection) = connection {
            receiver.set_retrying(false);
            return run(receiver, connection, stream, shutdown)
                .await
                .map_err(Into::into);
        }
        tokio::select! {
            biased;
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    receiver.abandon_and_drain(0).await;
                    return Ok(());
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

/// Writes engine-owned request metadata events to a Valkey Stream with
/// bounded local buffering. On an outage the current event is retried, the
/// channel fills to its configured bound, and further loss is counted by the
/// engine emitter.
pub async fn run(
    mut receiver: Receiver,
    mut connection: ConnectionManager,
    stream: &str,
    mut shutdown: watch::Receiver<bool>,
) -> Result<(), RedisError> {
    let mut shutdown_open = true;
    loop {
        if *shutdown.borrow() {
            receiver.abandon_and_drain(0).await;
            return Ok(());
        }

        let event = if shutdown_open {
            tokio::select! {
                biased;
                changed = shutdown.changed() => {
                    if changed.is_err() {
                        shutdown_open = false;
                    }
                    continue;
                }
                event = receiver.recv_next() => event,
            }
        } else {
            receiver.recv_next().await
        };
        let Some(event) = event else {
            return Ok(());
        };

        let payload = match serde_json::to_string(&event) {
            Ok(payload) => payload,
            Err(error) => {
                receiver.abandon_and_drain(1).await;
                return Err(RedisError::from((
                    redis::ErrorKind::Client,
                    "request metadata event serialization failed",
                    error.to_string(),
                )));
            }
        };
        let mut backoff = Duration::from_millis(25);
        loop {
            let mut command = redis::cmd("XADD");
            command.arg(stream).arg("*").arg("event").arg(&payload);
            let write = command.query_async(&mut connection);
            let result: Result<String, RedisError> =
                match tokio::time::timeout(STREAM_WRITE_TIMEOUT, write).await {
                    Ok(result) => result,
                    Err(_) => Err(RedisError::from((
                        redis::ErrorKind::Io,
                        "request metadata stream write timed out",
                    ))),
                };
            match result {
                Ok(_) => {
                    receiver.mark_persisted();
                    break;
                }
                Err(_) => {
                    receiver.set_retrying(true);
                    if *shutdown.borrow() {
                        receiver.abandon_and_drain(1).await;
                        return Ok(());
                    }
                    if shutdown_open {
                        tokio::select! {
                            () = tokio::time::sleep(backoff) => {}
                            changed = shutdown.changed() => {
                                if changed.is_err() {
                                    shutdown_open = false;
                                } else if *shutdown.borrow() {
                                    receiver.abandon_and_drain(1).await;
                                    return Ok(());
                                }
                            }
                        }
                    } else {
                        tokio::time::sleep(backoff).await;
                    }
                    backoff = (backoff * 2).min(Duration::from_secs(5));
                }
            }
        }
        receiver.set_retrying(false);
    }
}

#[cfg(test)]
mod tests {
    use chrono::Utc;
    use olp_engine::{
        domain::canonical::identity::{OperationKind, Surface},
        inference::request_metadata::{Emitter, Event, RequestAttemptMetadata},
    };
    use uuid::Uuid;

    use super::*;

    fn event() -> Event {
        let observed_at = Utc::now();
        let provider_id = Uuid::now_v7();
        Event {
            event_id: Uuid::now_v7(),
            request_id: Uuid::now_v7(),
            runtime_generation_id: Uuid::now_v7(),
            api_key_id: Uuid::now_v7(),
            provider_id: Some(provider_id),
            route_slug: "default".into(),
            upstream_model: Some("mock-model".into()),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            request_started_at: observed_at - chrono::Duration::milliseconds(10),
            request_completed_at: observed_at,
            observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 10,
            first_byte_ms: Some(3),
            input_tokens: Some(1),
            output_tokens: Some(2),
            cached_input_tokens: None,
            media_units: None,
            usage_complete: true,
            unpriced: true,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".into(),
                started_at: observed_at - chrono::Duration::milliseconds(10),
                completed_at: observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 10,
                first_byte_ms: Some(3),
                usage: None,
            }],
        }
    }

    #[tokio::test]
    async fn invalid_valkey_configuration_accounts_for_queued_events() {
        let (emitter, receiver) = Emitter::bounded(2);
        emitter.emit(event()).unwrap();
        emitter.emit(event()).unwrap();
        let (_shutdown_sender, shutdown) = watch::channel(false);

        assert!(
            run_connecting(receiver, "://invalid", "request-metadata", shutdown,)
                .await
                .is_err()
        );
        let snapshot = emitter.snapshot();
        assert_eq!(snapshot.abandoned, 2);
        assert_eq!(snapshot.lost(), 2);
        assert!(!snapshot.complete());
    }
}
