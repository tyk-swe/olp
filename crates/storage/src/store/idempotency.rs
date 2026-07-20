use std::fmt;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use sqlx::{Postgres, Row, Transaction};
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::security::{EncryptedSecret, MasterKey, idempotency_replay_scope};

use super::PersistenceError;

const IDEMPOTENCY_REPLAY_VERSION: u8 = 1;
const MAX_IDEMPOTENCY_REPLAY_BODY_BYTES: usize = 1024 * 1024;
const MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES: usize = MAX_IDEMPOTENCY_REPLAY_BODY_BYTES * 4 + 4096;

/// Opaque HTTP replay material persisted only inside an authenticated
/// encryption envelope. Debug output deliberately never includes the body.
pub struct IdempotencyResponse {
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    body: Zeroizing<Vec<u8>>,
}

impl IdempotencyResponse {
    pub fn new(
        status: u16,
        content_type: Option<String>,
        etag: Option<String>,
        body: Vec<u8>,
    ) -> Result<Self, PersistenceError> {
        let response = Self {
            status,
            content_type,
            etag,
            body: Zeroizing::new(body),
        };
        response.validate()?;
        Ok(response)
    }

    pub fn json<T: Serialize>(
        status: u16,
        value: &T,
        etag: Option<String>,
    ) -> Result<Self, PersistenceError> {
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

    fn validate(&self) -> Result<(), PersistenceError> {
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
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        Ok(())
    }
}

impl fmt::Debug for IdempotencyResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("IdempotencyResponse")
            .field("status", &self.status)
            .field("content_type", &self.content_type)
            .field("etag", &self.etag)
            .field("body", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub enum IdempotencyOutcome<T> {
    Executed {
        value: T,
        response: IdempotencyResponse,
    },
    Replayed(IdempotencyResponse),
}

#[derive(Clone, Copy)]
pub struct ReplayableIdempotency<'a> {
    request_fingerprint: [u8; 32],
    master_key: &'a MasterKey,
}

impl<'a> ReplayableIdempotency<'a> {
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

impl fmt::Debug for ReplayableIdempotency<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReplayableIdempotency")
            .field("request_fingerprint", &"[SHA-256]")
            .field("master_key", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub(crate) enum ReplayableIdempotencyClaim {
    Execute,
    Replay(IdempotencyResponse),
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
pub fn idempotency_fingerprint<T: Serialize>(request: &T) -> Result<[u8; 32], PersistenceError> {
    Ok(Sha256::digest(serde_json::to_vec(request)?).into())
}

/// Reduces a write-only request secret to a stable fingerprint component so
/// the plaintext never enters the serialized idempotency request envelope.
#[must_use]
pub fn idempotency_secret_digest(secret: &[u8]) -> [u8; 32] {
    Sha256::digest(secret).into()
}

pub(crate) async fn claim_replayable_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    request_fingerprint: &[u8; 32],
    master_key: &MasterKey,
) -> Result<ReplayableIdempotencyClaim, PersistenceError> {
    let scope = idempotency_replay_scope(actor, operation, key);
    let locked: bool =
        sqlx::query_scalar("SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0))")
            .bind(&scope)
            .fetch_one(&mut **transaction)
            .await?;
    if !locked {
        return Ok(ReplayableIdempotencyClaim::InProgress);
    }

    sqlx::query(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;

    let existing = sqlx::query(
        "SELECT state, request_fingerprint, replay_ciphertext, replay_nonce, replay_key_version \
         FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(row) = existing {
        let stored_fingerprint: Option<Vec<u8>> = row.get("request_fingerprint");
        if stored_fingerprint.as_deref() != Some(request_fingerprint.as_slice()) {
            return Ok(ReplayableIdempotencyClaim::Conflict);
        }
        let state: String = row.get("state");
        if state == "in_progress" {
            return Ok(ReplayableIdempotencyClaim::InProgress);
        }
        if state != "completed" {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let ciphertext: Option<Vec<u8>> = row.get("replay_ciphertext");
        let nonce: Option<Vec<u8>> = row.get("replay_nonce");
        let key_version: Option<i32> = row.get("replay_key_version");
        let ciphertext = ciphertext.ok_or(PersistenceError::IdempotencyReplayUnavailable)?;
        if ciphertext.len() > MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let nonce: [u8; 12] = nonce
            .ok_or(PersistenceError::IdempotencyReplayUnavailable)?
            .try_into()
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let key_version =
            u32::try_from(key_version.ok_or(PersistenceError::IdempotencyReplayUnavailable)?)
                .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let encrypted = EncryptedSecret {
            key_version,
            nonce,
            ciphertext,
        };
        let plaintext = master_key
            .open(&encrypted, scope.as_bytes())
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let stored: StoredIdempotencyResponse = serde_json::from_slice(&plaintext)
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        if stored.version != IDEMPOTENCY_REPLAY_VERSION {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let response =
            IdempotencyResponse::new(stored.status, stored.content_type, stored.etag, stored.body)?;
        return Ok(ReplayableIdempotencyClaim::Replay(response));
    }

    sqlx::query(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, request_fingerprint, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', $5, now() + interval '24 hours')",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(request_fingerprint.as_slice())
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
    response: &IdempotencyResponse,
) -> Result<(), PersistenceError> {
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
        .map_err(|_| PersistenceError::IdempotencyReplayEncryption)?;
    let key_version = i32::try_from(encrypted.key_version)
        .map_err(|_| PersistenceError::IdempotencyReplayEncryption)?;
    let result = sqlx::query(
        "UPDATE idempotency_records \
         SET state = 'completed', replay_ciphertext = $1, \
             replay_nonce = $2, replay_key_version = $3 \
         WHERE actor_user_id = $4 AND operation = $5 AND idempotency_key = $6 \
           AND state = 'in_progress' AND request_fingerprint = $7",
    )
    .bind(encrypted.ciphertext)
    .bind(encrypted.nonce.to_vec())
    .bind(key_version)
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(request_fingerprint.as_slice())
    .execute(&mut **transaction)
    .await?;
    if result.rows_affected() != 1 {
        return Err(PersistenceError::IdempotencyReplayUnavailable);
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
    sqlx::query(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;
    let result = sqlx::query(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', now() + interval '24 hours') \
         ON CONFLICT (actor_user_id, operation, idempotency_key) DO NOTHING",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(operation)
    .bind(key)
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
    sqlx::query(
        "UPDATE idempotency_records SET state = 'completed', resource_id = $1 \
         WHERE actor_user_id = $2 AND operation = $3 AND idempotency_key = $4",
    )
    .bind(resource_id)
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
