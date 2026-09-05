use axum::{
    body::Body,
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::Response,
};
use olp_db::idempotency::{Outcome, Replayable, ReplayedMutation, fingerprint};
use serde::Serialize;
use std::future::Future;
use tracing::warn;
use uuid::Uuid;

use crate::{management::state::ManagementState, public_http::problem::Problem};

use super::{error_mapping::map_persistence, response_policy::prevent_sensitive_response_caching};

pub(crate) fn require_idempotency_key(headers: &HeaderMap) -> Result<&str, Problem> {
    let value = headers
        .get("idempotency-key")
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::bad_request(
                "idempotency_key_required",
                "An Idempotency-Key header is required.",
            )
        })?;
    if !(8..=128).contains(&value.len())
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(Problem::bad_request(
            "invalid_idempotency_key",
            "Idempotency-Key must be 8-128 URL-safe ASCII characters.",
        ));
    }
    Ok(value)
}

/// A mutation that requires `Idempotency-Key` and records its response so a
/// retry after a dropped connection replays it instead of being told the key
/// was already used.
///
/// The mutation keeps its own claim inside its transaction, which is what
/// makes concurrent duplicates safe; this records the HTTP response once that
/// claim has committed. A crash before the response is recorded therefore
/// leaves the retry exactly where it is today — with a 409 — rather than
/// blocking a mutation that never ran. Replay material is encrypted under the
/// master key, so an installation without one keeps the old behaviour instead
/// of losing the mutation.
pub(crate) struct ReplayableMutation<'a> {
    state: &'a ManagementState,
    actor: Uuid,
    operation: &'static str,
    key: String,
    replay: Option<Replayable<'a>>,
}

/// What a mutation answers with, before [`ReplayableMutation::run`] records it
/// for replay.
pub(crate) struct MutationReply<T> {
    pub(crate) status: StatusCode,
    pub(crate) body: T,
    pub(crate) etag: Option<Uuid>,
    pub(crate) location: Option<String>,
}

impl<'a> ReplayableMutation<'a> {
    pub(crate) fn new<T: Serialize>(
        state: &'a ManagementState,
        actor: Uuid,
        operation: &'static str,
        headers: &HeaderMap,
        request: &T,
    ) -> Result<Self, Problem> {
        let key = require_idempotency_key(headers)?.to_owned();
        let replay = match state.master_key.as_deref() {
            Some(master_key) => {
                let fingerprint = fingerprint(request).map_err(map_persistence)?;
                Some(Replayable::new(fingerprint, master_key))
            }
            None => None,
        };
        Ok(Self {
            state,
            actor,
            operation,
            key,
            replay,
        })
    }

    /// Runs the whole idempotent choreography: answer with the recorded
    /// response if an identical earlier request already produced one,
    /// otherwise execute `mutate` under this key and record its reply.
    pub(crate) async fn run<T, F, Fut>(self, mutate: F) -> Result<Response, Problem>
    where
        T: Serialize,
        F: FnOnce(String) -> Fut,
        Fut: Future<Output = Result<MutationReply<T>, Problem>>,
    {
        if let Some(replayed) = self.replayed().await? {
            return Ok(replayed);
        }
        let reply = mutate(self.key.clone()).await?;
        self.respond_at(reply.status, &reply.body, reply.etag, reply.location)
            .await
    }

    /// The response an earlier identical request already produced, if any.
    async fn replayed(&self) -> Result<Option<Response>, Problem> {
        let Some(replay) = self.replay else {
            return Ok(None);
        };
        let replayed = self
            .state
            .store()
            .replayed_mutation(self.actor, self.operation, &self.key, replay)
            .await
            .map_err(map_persistence)?;
        match replayed {
            ReplayedMutation::Absent => Ok(None),
            ReplayedMutation::Replayed(response) => {
                idempotency_http_response(Outcome::<()>::Replayed(response)).map(Some)
            }
            ReplayedMutation::Conflict => Err(Problem::conflict(
                "idempotency_key_reused",
                "This Idempotency-Key was already used for a different request.",
            )),
        }
    }

    /// Records the mutation's response for replay and returns it. Recording is
    /// best effort: the mutation has already committed, so a storage failure
    /// here must not turn a successful mutation into an error. `location`
    /// is set for a 201 that points at the resource it created.
    async fn respond_at<T: Serialize>(
        &self,
        status: StatusCode,
        body: &T,
        etag: Option<Uuid>,
        location: Option<String>,
    ) -> Result<Response, Problem> {
        let response = olp_db::idempotency::Response::json(
            status.as_u16(),
            body,
            etag.map(|etag| format!("\"{etag}\"")),
        )
        .map_err(map_persistence)?;
        let response = match location {
            Some(location) => response.with_location(location).map_err(map_persistence)?,
            None => response,
        };
        if let Some(replay) = self.replay
            && let Err(error) = self
                .state
                .store()
                .record_mutation_response(self.actor, self.operation, &self.key, replay, &response)
                .await
        {
            warn!(
                %error,
                operation = self.operation,
                "management response could not be recorded for idempotent replay"
            );
        }
        idempotency_http_response(Outcome::<()>::Replayed(response))
    }
}

pub(crate) fn idempotency_http_response<T>(outcome: Outcome<T>) -> Result<Response, Problem> {
    let replay = match outcome {
        Outcome::Executed { response, .. } | Outcome::Replayed(response) => response,
    };
    let location = replay.location().map(ToOwned::to_owned);
    let (status, content_type, etag, body) = replay.into_parts();
    let mut response = Response::new(Body::from(body));
    *response.status_mut() = StatusCode::from_u16(status).map_err(|_| Problem::internal())?;
    if let Some(content_type) = content_type {
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| Problem::internal())?,
        );
    }
    if let Some(etag) = etag {
        response.headers_mut().insert(
            header::ETAG,
            HeaderValue::from_str(&etag).map_err(|_| Problem::internal())?,
        );
    }
    if let Some(location) = location {
        response.headers_mut().insert(
            header::LOCATION,
            HeaderValue::from_str(&location).map_err(|_| Problem::internal())?,
        );
    }
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}
