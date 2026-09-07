use std::fmt;

use serde::Deserialize;
use serde::Serialize;
use sha2::Digest;
use sha2::Sha256;
use sqlx::Postgres;
use sqlx::Transaction;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::crypto::aad::idempotency_replay_scope;
use crate::crypto::envelope::EncryptedSecret;
use crate::crypto::envelope::MasterKey;

use crate::database::error::Error;

pub mod operations {
    pub const API_KEY_REVOKE: &str = "api_key.revoke";
    pub const INVITATION_REVOKE: &str = "invitation.revoke";
    pub const PROVIDER_ACTIVATE: &str = "provider.activate";
    pub const PROVIDER_DISABLE: &str = "provider.disable";
    pub const PROVIDER_RESTORE_AS_DRAFT: &str = "provider.restore_as_draft";
    pub const PROVIDER_REVOKE_CREDENTIAL: &str = "provider.revoke_credential";
    pub const PROVIDER_REVISION_RESTORE_AS_DRAFT: &str = "provider_revision.restore_as_draft";
    pub const ROUTE_ACTIVATE: &str = "route.activate";
    pub const ROUTE_RESTORE_AS_DRAFT: &str = "route.restore_as_draft";
}

const IDEMPOTENCY_REPLAY_VERSION: u8 = 1;
const MAX_IDEMPOTENCY_REPLAY_BODY_BYTES: usize = 1024 * 1024;
const MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES: usize = MAX_IDEMPOTENCY_REPLAY_BODY_BYTES * 4 + 4096;

/// Opaque HTTP replay material persisted only inside an authenticated
/// encryption envelope. Debug output deliberately never includes the body.
pub struct Response {
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    location: Option<String>,
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
            location: None,
            body: Zeroizing::new(body),
        };
        response.validate()?;
        Ok(response)
    }

    /// Points a 201 at the resource it created. The header has to travel with
    /// the stored envelope: a replayed create must be byte-identical to the
    /// original, headers included.
    pub fn with_location(mut self, location: String) -> Result<Self, Error> {
        self.location = Some(location);
        self.validate()?;
        Ok(self)
    }

    #[must_use]
    pub fn location(&self) -> Option<&str> {
        self.location.as_deref()
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
            || self
                .location
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
            .field("location", &self.location)
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
    #[serde(skip_serializing_if = "Option::is_none")]
    location: &'a Option<String>,
    body: &'a [u8],
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredIdempotencyResponse {
    version: u8,
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    #[serde(default)]
    location: Option<String>,
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
    let locked: bool = sqlx::query_scalar::<_, bool>(
        "SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0)) AS \"value\"",
    )
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

    let existing = sqlx::query_as::<_, ClaimReplayableIdempotencyRow>(
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
        let response = open_stored_response(
            master_key,
            &scope,
            StoredEnvelope {
                ciphertext: row.replay_ciphertext,
                nonce: row.replay_nonce,
                key_version: row.replay_key_version,
            },
        )?;
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
    response: &Response,
) -> Result<(), Error> {
    let scope = idempotency_replay_scope(actor, operation, key);
    let encrypted = seal_response(master_key, &scope, response)?;
    let key_version =
        i32::try_from(encrypted.key_version).map_err(|_| Error::IdempotencyReplayEncryption)?;
    let result = sqlx::query(
        "UPDATE idempotency_records \
         SET state = 'completed', replay_ciphertext = $1, \
             replay_nonce = $2, replay_key_version = $3 \
         WHERE actor_user_id = $4 AND operation = $5 AND idempotency_key = $6 \
           AND state = 'in_progress' AND request_fingerprint = $7",
    )
    .bind(&encrypted.ciphertext)
    .bind(encrypted.nonce.to_vec())
    .bind(key_version)
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(request_fingerprint.as_slice())
    .execute(&mut **transaction)
    .await?;
    if result.rows_affected() != 1 {
        return Err(Error::IdempotencyReplayUnavailable);
    }
    Ok(())
}

struct StoredEnvelope {
    ciphertext: Option<Vec<u8>>,
    nonce: Option<Vec<u8>>,
    key_version: Option<i32>,
}

fn open_stored_response(
    master_key: &MasterKey,
    scope: &str,
    envelope: StoredEnvelope,
) -> Result<Response, Error> {
    let ciphertext = envelope
        .ciphertext
        .ok_or(Error::IdempotencyReplayUnavailable)?;
    if ciphertext.len() > MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES {
        return Err(Error::IdempotencyReplayUnavailable);
    }
    let nonce: [u8; 12] = envelope
        .nonce
        .ok_or(Error::IdempotencyReplayUnavailable)?
        .try_into()
        .map_err(|_| Error::IdempotencyReplayUnavailable)?;
    let key_version = u32::try_from(
        envelope
            .key_version
            .ok_or(Error::IdempotencyReplayUnavailable)?,
    )
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
    match stored.location {
        Some(location) => response.with_location(location),
        None => Ok(response),
    }
}

fn seal_response(
    master_key: &MasterKey,
    scope: &str,
    response: &Response,
) -> Result<EncryptedSecret, Error> {
    response.validate()?;
    let plaintext = Zeroizing::new(serde_json::to_vec(&StoredIdempotencyResponseRef {
        version: IDEMPOTENCY_REPLAY_VERSION,
        status: response.status,
        content_type: &response.content_type,
        etag: &response.etag,
        location: &response.location,
        body: &response.body,
    })?);
    master_key
        .seal(&plaintext, scope.as_bytes())
        .map_err(|_| Error::IdempotencyReplayEncryption)
}

/// What a retry of an already-claimed management mutation should do.
#[derive(Debug)]
pub enum ReplayedMutation {
    /// Nothing was recorded under this key: run the mutation.
    Absent,
    /// The identical request already ran; return this response again.
    Replayed(Response),
    /// The key was used for a different request.
    Conflict,
}

/// Looks up the response an earlier identical request recorded under this
/// idempotency key. Mutations that claim their key non-replayably answer a
/// dropped-connection retry with a bare 409, which leaves the client
/// unable to tell whether the revoke or activation happened; recording the
/// response after the mutation commits lets the retry replay it instead.
pub async fn replayed_mutation(
    pool: &sqlx::PgPool,
    actor: Uuid,
    operation: &str,
    key: &str,
    replay: Replayable<'_>,
) -> Result<ReplayedMutation, Error> {
    let record = sqlx::query_as::<_, ClaimReplayableIdempotencyRow>(
        "SELECT state, request_fingerprint, replay_ciphertext, replay_nonce, \
                    replay_key_version \
             FROM idempotency_records \
             WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
               AND expires_at > now()",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .fetch_optional(pool)
    .await?;
    let Some(record) = record else {
        return Ok(ReplayedMutation::Absent);
    };
    if record
        .request_fingerprint
        .as_deref()
        .is_some_and(|fingerprint| fingerprint != replay.request_fingerprint().as_slice())
    {
        return Ok(ReplayedMutation::Conflict);
    }
    if record.state != "completed" || record.replay_ciphertext.is_none() {
        // Either a concurrent attempt still holds the key, or the process
        // died before it could record its response. The mutation's own
        // claim decides what the retry gets.
        return Ok(ReplayedMutation::Absent);
    }
    let scope = idempotency_replay_scope(actor, operation, key);
    let response = open_stored_response(
        replay.master_key(),
        &scope,
        StoredEnvelope {
            ciphertext: record.replay_ciphertext,
            nonce: record.replay_nonce,
            key_version: record.replay_key_version,
        },
    )?;
    Ok(ReplayedMutation::Replayed(response))
}

/// Attaches the response envelope to the record the mutation just
/// completed. The mutation is already durable, so callers treat a failure
/// here as "not replayable" rather than as a failed request.
pub async fn record_mutation_response(
    pool: &sqlx::PgPool,
    actor: Uuid,
    operation: &str,
    key: &str,
    replay: Replayable<'_>,
    response: &Response,
) -> Result<(), Error> {
    let scope = idempotency_replay_scope(actor, operation, key);
    let encrypted = seal_response(replay.master_key(), &scope, response)?;
    let key_version =
        i32::try_from(encrypted.key_version).map_err(|_| Error::IdempotencyReplayEncryption)?;
    let result = sqlx::query(
        "UPDATE idempotency_records \
             SET request_fingerprint = $4, replay_ciphertext = $5, replay_nonce = $6, \
                 replay_key_version = $7, resource_id = NULL \
             WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
               AND state = 'completed' AND replay_ciphertext IS NULL",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(replay.request_fingerprint().as_slice())
    .bind(&encrypted.ciphertext)
    .bind(encrypted.nonce.to_vec())
    .bind(key_version)
    .execute(pool)
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

#[cfg(test)]
mod tests {
    use crate::database::error::Error;

    use crate::crypto::envelope::MasterKey;
    use crate::database::idempotency::MAX_IDEMPOTENCY_REPLAY_BODY_BYTES;
    use crate::database::idempotency::Response;
    use crate::database::idempotency::StoredEnvelope;
    use crate::database::idempotency::fingerprint;
    use crate::database::idempotency::open_stored_response;
    use crate::database::idempotency::seal_response;

    #[test]
    fn a_sealed_response_round_trips_its_location_header() {
        let master_key = MasterKey::new(1, [7; 32]);
        let scope = "idempotency:test";
        let response = Response::json(201, &serde_json::json!({"id": "abc"}), None)
            .unwrap()
            .with_location("/api/v3/api-keys/abc".to_owned())
            .unwrap();
        let sealed = seal_response(&master_key, scope, &response).unwrap();

        let opened = open_stored_response(
            &master_key,
            scope,
            StoredEnvelope {
                ciphertext: Some(sealed.ciphertext),
                nonce: Some(sealed.nonce.to_vec()),
                key_version: Some(i32::try_from(sealed.key_version).unwrap()),
            },
        )
        .unwrap();
        assert_eq!(opened.location(), Some("/api/v3/api-keys/abc"));
        assert_eq!(opened.into_parts().0, 201);
    }

    #[test]
    fn an_envelope_stored_before_locations_existed_still_opens() {
        let master_key = MasterKey::new(1, [9; 32]);
        let scope = "idempotency:test";
        let response = Response::json(200, &serde_json::json!({"ok": true}), None).unwrap();
        let sealed = seal_response(&master_key, scope, &response).unwrap();

        let opened = open_stored_response(
            &master_key,
            scope,
            StoredEnvelope {
                ciphertext: Some(sealed.ciphertext),
                nonce: Some(sealed.nonce.to_vec()),
                key_version: Some(i32::try_from(sealed.key_version).unwrap()),
            },
        )
        .unwrap();
        assert_eq!(opened.location(), None);
    }

    #[test]
    fn a_location_that_cannot_be_a_header_is_refused() {
        let response = Response::json(201, &serde_json::json!({}), None).unwrap();
        assert!(matches!(
            response.with_location("/api/v3/keys\r\nX-Injected: 1".to_owned()),
            Err(Error::IdempotencyReplayUnavailable)
        ));
    }

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

#[derive(sqlx::FromRow)]
struct ClaimReplayableIdempotencyRow {
    state: String,
    request_fingerprint: Option<Vec<u8>>,
    replay_ciphertext: Option<Vec<u8>>,
    replay_nonce: Option<Vec<u8>>,
    replay_key_version: Option<i32>,
}
