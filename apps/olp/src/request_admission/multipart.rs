use std::{
    collections::BTreeSet,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
};

use axum::http::{HeaderMap, HeaderName};
use olp_domain::{ApiKey, OperationKind, RouteSlug, authorize_api_key};

use crate::{MAX_MEDIA_BODY_BYTES, Problem, gateway};

pub(crate) const IMAGE_VARIATION_BODY_BYTES: usize = 55 * 1024 * 1024;
pub(crate) const TRANSCRIPTION_BODY_BYTES: usize = 30 * 1024 * 1024;
pub(crate) const VIDEO_CREATE_BODY_BYTES: usize = 25 * 1024 * 1024;

/// The route information authenticated before a multipart body is read. A
/// route-restricted key must either supply the header it was pre-authorized
/// for, or place `model` before every file part so the parser can authorize it
/// before creating a spool file.
#[derive(Clone, Debug)]
pub(crate) enum MultipartRouteAdmission {
    Unrestricted,
    RequireModelBeforeFile(BTreeSet<RouteSlug>),
    Expected(RouteSlug),
}

impl MultipartRouteAdmission {
    pub(crate) const fn requires_model_before_file(&self) -> bool {
        matches!(self, Self::RequireModelBeforeFile(_))
    }
}

#[derive(Clone)]
pub(crate) struct MultipartRequestAdmission {
    pub(crate) route: MultipartRouteAdmission,
    pub(super) lease: Option<MultipartParserLease>,
}

impl MultipartRequestAdmission {
    #[cfg(test)]
    pub(crate) const fn unrestricted() -> Self {
        Self {
            route: MultipartRouteAdmission::Unrestricted,
            lease: None,
        }
    }

    pub(crate) fn release(&self) {
        if let Some(lease) = &self.lease {
            lease.release();
        }
    }
}

#[derive(Clone)]
pub(crate) struct MultipartAdmissionState {
    inner: Arc<MultipartAdmissionInner>,
}

struct MultipartAdmissionInner {
    /// Only half the total spool is ever promised to untrusted parsers. The
    /// spool itself continues to enforce byte-accurate accounting for all
    /// request and response media.
    budget_bytes: u64,
    reserved_bytes: AtomicU64,
    active_keys: Mutex<BTreeSet<uuid::Uuid>>,
}

#[derive(Clone)]
pub(crate) struct MultipartParserLease {
    inner: Arc<MultipartParserLeaseInner>,
}

struct MultipartParserLeaseInner {
    admission: MultipartAdmissionState,
    api_key_id: uuid::Uuid,
    reservation_bytes: u64,
    released: AtomicBool,
}

impl MultipartAdmissionState {
    pub(crate) fn new(capacity_bytes: u64) -> Self {
        Self {
            inner: Arc::new(MultipartAdmissionInner {
                budget_bytes: capacity_bytes / 2,
                reserved_bytes: AtomicU64::new(0),
                active_keys: Mutex::new(BTreeSet::new()),
            }),
        }
    }

    pub(crate) fn try_admit(
        &self,
        api_key_id: uuid::Uuid,
        reservation_bytes: u64,
    ) -> Option<MultipartParserLease> {
        if reservation_bytes == 0 || reservation_bytes > self.inner.budget_bytes {
            return None;
        }
        let mut active_keys = self.inner.active_keys.lock().ok()?;
        if active_keys.contains(&api_key_id) {
            return None;
        }
        let reserved = self
            .inner
            .reserved_bytes
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |current| {
                current
                    .checked_add(reservation_bytes)
                    .filter(|next| *next <= self.inner.budget_bytes)
            })
            .is_ok();
        if !reserved {
            return None;
        }
        active_keys.insert(api_key_id);
        Some(MultipartParserLease {
            inner: Arc::new(MultipartParserLeaseInner {
                admission: self.clone(),
                api_key_id,
                reservation_bytes,
                released: AtomicBool::new(false),
            }),
        })
    }
}

impl MultipartParserLease {
    pub(crate) fn release(&self) {
        self.inner.release();
    }
}

impl MultipartParserLeaseInner {
    fn release(&self) {
        if self.released.swap(true, Ordering::AcqRel) {
            return;
        }
        let previous = self
            .admission
            .inner
            .reserved_bytes
            .fetch_sub(self.reservation_bytes, Ordering::AcqRel);
        debug_assert!(previous >= self.reservation_bytes);
        if let Ok(mut active_keys) = self.admission.inner.active_keys.lock() {
            active_keys.remove(&self.api_key_id);
        }
    }
}

impl Drop for MultipartParserLeaseInner {
    fn drop(&mut self) {
        // A cancelled request can drop before the parser explicitly returns.
        // The final Arc owner performs the same cleanup exactly once.
        self.release();
    }
}

pub(crate) fn validate_multipart_boundary(content_type: &str) -> Result<(), Problem> {
    let boundary = content_type.split(';').skip(1).find_map(|parameter| {
        let (name, value) = parameter.trim().split_once('=')?;
        name.trim()
            .eq_ignore_ascii_case("boundary")
            .then(|| value.trim().trim_matches('"'))
    });
    if boundary.is_none_or(|value| {
        value.is_empty()
            || value.len() > 200
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_graphic() && byte != b'"' && byte != b'\\')
    }) {
        return Err(Problem::bad_request(
            "invalid_multipart_boundary",
            "A multipart/form-data request requires a valid boundary no longer than 200 bytes.",
        ));
    }
    Ok(())
}

pub(crate) fn multipart_endpoint(
    method: &axum::http::Method,
    path: &str,
) -> Option<(OperationKind, u64)> {
    if *method != axum::http::Method::POST {
        return None;
    }
    match path {
        // Reserve against the route's fixed body ceiling, never against an
        // attacker-controlled Content-Length. Individual file limits are
        // still enforced by the spool while streaming.
        "/openai/v1/images/edits" => Some((OperationKind::ImageEdit, MAX_MEDIA_BODY_BYTES as u64)),
        "/openai/v1/images/variations" => Some((
            OperationKind::ImageVariation,
            IMAGE_VARIATION_BODY_BYTES as u64,
        )),
        "/openai/v1/audio/transcriptions" => Some((
            OperationKind::Transcription,
            TRANSCRIPTION_BODY_BYTES as u64,
        )),
        "/openai/v1/videos" => Some((OperationKind::VideoCreate, VIDEO_CREATE_BODY_BYTES as u64)),
        _ => None,
    }
}

pub(super) fn preauthorize_multipart(
    headers: &HeaderMap,
    key: &ApiKey,
    method: &axum::http::Method,
    path: &str,
) -> Result<(MultipartRouteAdmission, u64), gateway::InferenceError> {
    let Some((operation, reservation_bytes)) = multipart_endpoint(method, path) else {
        return Ok((MultipartRouteAdmission::Unrestricted, 0));
    };
    authorize_api_key(key, None, operation, chrono::Utc::now())
        .map_err(|error| gateway::InferenceError::forbidden(error.to_string()))?;

    let route_header = HeaderName::from_static("x-olp-route");
    let values = headers.get_all(&route_header);
    if values.iter().count() > 1 {
        return Err(gateway::InferenceError::invalid_request(
            "X-OLP-Route must appear at most once.",
        ));
    }
    let supplied = values
        .iter()
        .next()
        .map(|value| {
            value
                .to_str()
                .map_err(|_| gateway::InferenceError::invalid_request("X-OLP-Route is invalid."))
        })
        .transpose()?;
    if key.allowed_routes.is_empty() {
        return Ok((MultipartRouteAdmission::Unrestricted, reservation_bytes));
    }
    if let Some(supplied) = supplied {
        let route = RouteSlug::parse(supplied)
            .map_err(|_| gateway::InferenceError::invalid_request("X-OLP-Route is invalid."))?;
        authorize_api_key(key, Some(&route), operation, chrono::Utc::now())
            .map_err(|error| gateway::InferenceError::forbidden(error.to_string()))?;
        Ok((MultipartRouteAdmission::Expected(route), reservation_bytes))
    } else {
        Ok((
            MultipartRouteAdmission::RequireModelBeforeFile(key.allowed_routes.clone()),
            reservation_bytes,
        ))
    }
}
