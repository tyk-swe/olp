use std::fmt;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::security::{
    aad::idempotency_replay_scope,
    envelope::{EncryptedSecret, MasterKey},
};

use crate::error::Error;

const IDEMPOTENCY_REPLAY_VERSION: u8 = 1;
const MAX_IDEMPOTENCY_REPLAY_BODY_BYTES: usize = 1024 * 1024;
const MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES: usize = MAX_IDEMPOTENCY_REPLAY_BODY_BYTES * 4 + 4096;

/// Opaque HTTP replay material persisted only inside an authenticated
/// encryption envelope. Debug output deliberately never includes the body.
pub struct Response {
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    body: Zeroizing<Vec<u8>>,
}

impl Response {
    pub fn new(
        status: u16,
        content_type: Option<String>,
        etag: Option<String>,
        body: Vec<u8>,
    ) -> Result<Self, Error> {
        let response = Self {
            status,
            content_type,
            etag,
            body: Zeroizing::new(body),
        };
        response.validate()?;
        Ok(response)
    }

    pub fn json<T: Serialize>(status: u16, value: &T, etag: Option<String>) -> Result<Self, Error> {
        Self::new(
            status,
            Some("application/json".to_owned()),
            etag,
            serde_json::to_vec(value)?,
        )
    }

    #[must_use]
    pub fn into_parts(mut self) -> (u16, Option<String>, Option<String>, Vec<u8>) {
        let body = std::mem::take(&mut *self.body);
        (
            self.status,
            self.content_type.take(),
            self.etag.take(),
            body,
        )
    }

    fn validate(&self) -> Result<(), Error> {
        if !(200..=599).contains(&self.status)
            || self.body.len() > MAX_IDEMPOTENCY_REPLAY_BODY_BYTES
            || self
                .content_type
                .as_ref()
                .is_some_and(|value| !valid_replay_header(value))
            || self
                .etag
                .as_ref()
                .is_some_and(|value| !valid_replay_header(value))
        {
            return Err(Error::IdempotencyReplayUnavailable);
        }
        Ok(())
    }
}

impl fmt::Debug for Response {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Response")
            .field("status", &self.status)
            .field("content_type", &self.content_type)
            .field("etag", &self.etag)
            .field("body", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub enum Outcome<T> {
    Executed { value: T, response: Response },
    Replayed(Response),
}

#[derive(Clone, Copy)]
pub struct Replayable<'a> {
    request_fingerprint: [u8; 32],
    master_key: &'a MasterKey,
}

impl<'a> Replayable<'a> {
    #[must_use]
    pub const fn new(request_fingerprint: [u8; 32], master_key: &'a MasterKey) -> Self {
        Self {
            request_fingerprint,
            master_key,
        }
    }

    #[must_use]
    pub const fn request_fingerprint(&self) -> &[u8; 32] {
        &self.request_fingerprint
    }

    #[must_use]
    pub const fn master_key(&self) -> &'a MasterKey {
        self.master_key
    }
}

impl fmt::Debug for Replayable<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Replayable")
            .field("request_fingerprint", &"[SHA-256]")
            .field("master_key", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub(crate) enum ReplayableIdempotencyClaim {
    Execute,
    Replay(Response),
    Conflict,
    InProgress,
}

#[derive(Serialize)]
struct StoredIdempotencyResponseRef<'a> {
    version: u8,
    status: u16,
    content_type: &'a Option<String>,
    etag: &'a Option<String>,
    body: &'a [u8],
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredIdempotencyResponse {
    version: u8,
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    body: Vec<u8>,
}

/// Produces a stable SHA-256 fingerprint from a typed management request.
/// Callers should serialize only request semantics, never generated secrets.
pub fn fingerprint<T: Serialize>(request: &T) -> Result<[u8; 32], Error> {
    Ok(Sha256::digest(serde_json::to_vec(request)?).into())
}

/// Reduces a write-only request secret to a stable fingerprint component so
/// the plaintext never enters the serialized idempotency request envelope.
#[must_use]
pub fn secret_digest(secret: &[u8]) -> [u8; 32] {
    Sha256::digest(secret).into()
}

pub(crate) async fn claim_replayable_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    request_fingerprint: &[u8; 32],
    master_key: &MasterKey,
) -> Result<ReplayableIdempotencyClaim, Error> {
    let scope = idempotency_replay_scope(actor, operation, key);
    let locked: bool = sqlx::query_scalar!(
        "SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0)) AS \"value!\"",
        &scope
    )
    .fetch_one(&mut **transaction)
    .await?;
    if !locked {
        return Ok(ReplayableIdempotencyClaim::InProgress);
    }

    sqlx::query!(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
        actor,
        operation,
        key
    )
    .execute(&mut **transaction)
    .await?;

    let existing = sqlx::query!(
        "SELECT state, request_fingerprint, replay_ciphertext, replay_nonce, replay_key_version \
         FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3",
        actor,
        operation,
        key
    )
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(row) = existing {
        let stored_fingerprint: Option<Vec<u8>> = row.request_fingerprint;
        if stored_fingerprint.as_deref() != Some(request_fingerprint.as_slice()) {
            return Ok(ReplayableIdempotencyClaim::Conflict);
        }
        let state: String = row.state;
        if state == "in_progress" {
            return Ok(ReplayableIdempotencyClaim::InProgress);
        }
        if state != "completed" {
            return Err(Error::IdempotencyReplayUnavailable);
        }
        let ciphertext: Option<Vec<u8>> = row.replay_ciphertext;
        let nonce: Option<Vec<u8>> = row.replay_nonce;
        let key_version: Option<i32> = row.replay_key_version;
        let ciphertext = ciphertext.ok_or(Error::IdempotencyReplayUnavailable)?;
        if ciphertext.len() > MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES {
            return Err(Error::IdempotencyReplayUnavailable);
        }
        let nonce: [u8; 12] = nonce
            .ok_or(Error::IdempotencyReplayUnavailable)?
            .try_into()
            .map_err(|_| Error::IdempotencyReplayUnavailable)?;
        let key_version = u32::try_from(key_version.ok_or(Error::IdempotencyReplayUnavailable)?)
            .map_err(|_| Error::IdempotencyReplayUnavailable)?;
        let encrypted = EncryptedSecret {
            key_version,
            nonce,
            ciphertext,
        };
        let plaintext = master_key
            .open(&encrypted, scope.as_bytes())
            .map_err(|_| Error::IdempotencyReplayUnavailable)?;
        let stored: StoredIdempotencyResponse =
            serde_json::from_slice(&plaintext).map_err(|_| Error::IdempotencyReplayUnavailable)?;
        if stored.version != IDEMPOTENCY_REPLAY_VERSION {
            return Err(Error::IdempotencyReplayUnavailable);
        }
        let response = Response::new(stored.status, stored.content_type, stored.etag, stored.body)?;
        return Ok(ReplayableIdempotencyClaim::Replay(response));
    }

    sqlx::query!(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, request_fingerprint, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', $5, now() + interval '24 hours')",
        Uuid::now_v7(),
        actor,
        operation,
        key,
        request_fingerprint.as_slice()
    )
    .execute(&mut **transaction)
    .await?;
    Ok(ReplayableIdempotencyClaim::Execute)
}

pub(crate) async fn complete_replayable_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    request_fingerprint: &[u8; 32],
    master_key: &MasterKey,
    response: &Response,
) -> Result<(), Error> {
    response.validate()?;
    let scope = idempotency_replay_scope(actor, operation, key);
    let plaintext = Zeroizing::new(serde_json::to_vec(&StoredIdempotencyResponseRef {
        version: IDEMPOTENCY_REPLAY_VERSION,
        status: response.status,
        content_type: &response.content_type,
        etag: &response.etag,
        body: &response.body,
    })?);
    let encrypted = master_key
        .seal(&plaintext, scope.as_bytes())
        .map_err(|_| Error::IdempotencyReplayEncryption)?;
    let key_version =
        i32::try_from(encrypted.key_version).map_err(|_| Error::IdempotencyReplayEncryption)?;
    let result = sqlx::query!(
        "UPDATE idempotency_records \
         SET state = 'completed', replay_ciphertext = $1, \
             replay_nonce = $2, replay_key_version = $3 \
         WHERE actor_user_id = $4 AND operation = $5 AND idempotency_key = $6 \
           AND state = 'in_progress' AND request_fingerprint = $7",
        encrypted.ciphertext,
        encrypted.nonce.to_vec(),
        key_version,
        actor,
        operation,
        key,
        request_fingerprint.as_slice()
    )
    .execute(&mut **transaction)
    .await?;
    if result.rows_affected() != 1 {
        return Err(Error::IdempotencyReplayUnavailable);
    }
    Ok(())
}

fn valid_replay_header(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| (0x20..=0x7e).contains(&byte) && byte != b'\r' && byte != b'\n')
}

pub(crate) async fn claim_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
) -> Result<bool, sqlx::Error> {
    // Expired claims must not permanently reserve a key. Keeping cleanup in
    // the caller's transaction also serializes a retry with any concurrent
    // attempt using the same actor/operation/key tuple.
    sqlx::query!(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
        actor,
        operation,
        key
    )
    .execute(&mut **transaction)
    .await?;
    let result = sqlx::query!(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', now() + interval '24 hours') \
         ON CONFLICT (actor_user_id, operation, idempotency_key) DO NOTHING",
        Uuid::now_v7(),
        actor,
        operation,
        key
    )
    .execute(&mut **transaction)
    .await?;
    Ok(result.rows_affected() == 1)
}

pub(crate) async fn complete_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    resource_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query!(
        "UPDATE idempotency_records SET state = 'completed', resource_id = $1 \
         WHERE actor_user_id = $2 AND operation = $3 AND idempotency_key = $4",
        resource_id,
        actor,
        operation,
        key
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use crate::error::Error;

    use super::{MAX_IDEMPOTENCY_REPLAY_BODY_BYTES, Response, fingerprint};

    #[test]
    fn replay_response_debug_output_redacts_the_body() {
        let response = Response::json(
            201,
            &serde_json::json!({"secret": "one-time-secret"}),
            Some("\"etag\"".to_owned()),
        )
        .unwrap();
        assert!(!format!("{response:?}").contains("one-time-secret"));
    }

    #[test]
    fn typed_fingerprints_are_stable_and_request_bound() {
        let first = fingerprint(&serde_json::json!({
            "name": "key",
            "scopes": ["inference"]
        }))
        .unwrap();
        let identical = fingerprint(&serde_json::json!({
            "name": "key",
            "scopes": ["inference"]
        }))
        .unwrap();
        let changed = fingerprint(&serde_json::json!({
            "name": "changed",
            "scopes": ["inference"]
        }))
        .unwrap();
        assert_eq!(first, identical);
        assert_ne!(first, changed);
    }

    #[test]
    fn replay_response_accepts_protocol_boundaries_and_round_trips_parts() {
        for status in [200, 599] {
            assert!(Response::new(status, None, None, Vec::new()).is_ok());
        }

        let longest_header = "x".repeat(256);
        let response = Response::new(
            201,
            Some(longest_header.clone()),
            Some(longest_header.clone()),
            b"response".to_vec(),
        )
        .unwrap();
        assert_eq!(
            response.into_parts(),
            (
                201,
                Some(longest_header.clone()),
                Some(longest_header),
                b"response".to_vec(),
            )
        );

        assert!(
            Response::new(200, None, None, vec![0; MAX_IDEMPOTENCY_REPLAY_BODY_BYTES],).is_ok()
        );
    }

    #[test]
    fn replay_response_rejects_unsafe_or_unbounded_material() {
        for status in [199, 600] {
            assert!(matches!(
                Response::new(status, None, None, Vec::new()),
                Err(Error::IdempotencyReplayUnavailable)
            ));
        }

        for (content_type, etag) in [
            (Some(String::new()), None),
            (Some("text/plain\nset-cookie: secret".to_owned()), None),
            (Some("caf\u{e9}".to_owned()), None),
            (Some("x".repeat(257)), None),
            (None, Some("\r\n".to_owned())),
        ] {
            assert!(matches!(
                Response::new(200, content_type, etag, Vec::new()),
                Err(Error::IdempotencyReplayUnavailable)
            ));
        }

        assert!(matches!(
            Response::new(
                200,
                None,
                None,
                vec![0; MAX_IDEMPOTENCY_REPLAY_BODY_BYTES + 1],
            ),
            Err(Error::IdempotencyReplayUnavailable)
        ));
    }
}
