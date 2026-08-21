use std::{
    pin::Pin,
    sync::{
        Arc,
        atomic::{AtomicU64, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};

use axum::{
    body::{Body, Bytes},
    extract::State,
    http::{HeaderValue, Request, header},
    middleware,
    response::{IntoResponse, Response},
};
use http_body::{Frame, SizeHint};
use tokio::sync::{OwnedSemaphorePermit, Semaphore};

use crate::{
    gateway::{self, endpoint_policy::classification::InferenceEndpoint},
    public_http::problem::Problem,
};

pub(crate) const DEFAULT_MAX_IN_FLIGHT_INFERENCE_REQUESTS: usize = 256;
pub(crate) const DEFAULT_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS: usize = 32;
pub(crate) const MAX_ADMISSION_CAPACITY: usize = 1_000_000;
const RETRY_AFTER_SECONDS: &str = "1";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum AdmissionSurface {
    Inference,
    Management,
}

#[derive(Clone)]
pub(crate) struct PublicAdmission {
    inner: Arc<AdmissionInner>,
}

struct AdmissionInner {
    inference: Arc<Semaphore>,
    management: Arc<Semaphore>,
    inference_capacity: usize,
    management_capacity: usize,
    inference_admitted: AtomicUsize,
    management_admitted: AtomicUsize,
    inference_rejections: AtomicU64,
    management_rejections: AtomicU64,
}

#[derive(Clone)]
pub(crate) struct PublicAdmissionMiddleware {
    admission: PublicAdmission,
    inference_enabled: bool,
}

struct AdmissionPermit {
    admission: PublicAdmission,
    surface: AdmissionSurface,
    _permit: OwnedSemaphorePermit,
}

struct AdmissionBody {
    inner: Pin<Box<Body>>,
    permit: Option<AdmissionPermit>,
}

impl Default for PublicAdmission {
    fn default() -> Self {
        Self::new(
            DEFAULT_MAX_IN_FLIGHT_INFERENCE_REQUESTS,
            DEFAULT_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS,
        )
    }
}

impl PublicAdmission {
    pub(crate) fn new(inference_capacity: usize, management_capacity: usize) -> Self {
        assert!(
            (1..=MAX_ADMISSION_CAPACITY).contains(&inference_capacity),
            "inference admission capacity must be between 1 and {MAX_ADMISSION_CAPACITY}"
        );
        assert!(
            (1..=MAX_ADMISSION_CAPACITY).contains(&management_capacity),
            "management admission capacity must be between 1 and {MAX_ADMISSION_CAPACITY}"
        );
        Self {
            inner: Arc::new(AdmissionInner {
                inference: Arc::new(Semaphore::new(inference_capacity)),
                management: Arc::new(Semaphore::new(management_capacity)),
                inference_capacity,
                management_capacity,
                inference_admitted: AtomicUsize::new(0),
                management_admitted: AtomicUsize::new(0),
                inference_rejections: AtomicU64::new(0),
                management_rejections: AtomicU64::new(0),
            }),
        }
    }

    fn try_acquire(&self, surface: AdmissionSurface) -> Result<AdmissionPermit, ()> {
        let permit = match surface {
            AdmissionSurface::Inference => Arc::clone(&self.inner.inference),
            AdmissionSurface::Management => Arc::clone(&self.inner.management),
        }
        .try_acquire_owned()
        .map_err(|_| {
            match surface {
                AdmissionSurface::Inference => &self.inner.inference_rejections,
                AdmissionSurface::Management => &self.inner.management_rejections,
            }
            .fetch_add(1, Ordering::Relaxed);
        })?;
        match surface {
            AdmissionSurface::Inference => &self.inner.inference_admitted,
            AdmissionSurface::Management => &self.inner.management_admitted,
        }
        .fetch_add(1, Ordering::AcqRel);
        Ok(AdmissionPermit {
            admission: self.clone(),
            surface,
            _permit: permit,
        })
    }

    pub(crate) fn metrics(&self) -> String {
        let inference_admitted = self.inner.inference_admitted.load(Ordering::Acquire);
        let management_admitted = self.inner.management_admitted.load(Ordering::Acquire);
        let inference_rejections = self.inner.inference_rejections.load(Ordering::Relaxed);
        let management_rejections = self.inner.management_rejections.load(Ordering::Relaxed);
        format!(
            concat!(
                "# HELP olp_http_admission_capacity Configured process-local HTTP request admission capacity.\n",
                "# TYPE olp_http_admission_capacity gauge\n",
                "olp_http_admission_capacity{{surface=\"inference\"}} {}\n",
                "olp_http_admission_capacity{{surface=\"management\"}} {}\n",
                "# HELP olp_http_admitted_requests Current process-local admitted HTTP requests whose responses have not completed.\n",
                "# TYPE olp_http_admitted_requests gauge\n",
                "olp_http_admitted_requests{{surface=\"inference\"}} {}\n",
                "olp_http_admitted_requests{{surface=\"management\"}} {}\n",
                "# HELP olp_http_admission_rejections_total HTTP requests rejected because the process-local admission pool was full.\n",
                "# TYPE olp_http_admission_rejections_total counter\n",
                "olp_http_admission_rejections_total{{surface=\"inference\"}} {}\n",
                "olp_http_admission_rejections_total{{surface=\"management\"}} {}\n",
            ),
            self.inner.inference_capacity,
            self.inner.management_capacity,
            inference_admitted,
            management_admitted,
            inference_rejections,
            management_rejections,
        )
    }

    #[cfg(test)]
    pub(crate) fn admitted(&self, surface: &'static str) -> usize {
        match surface {
            "inference" => self.inner.inference_admitted.load(Ordering::Acquire),
            "management" => self.inner.management_admitted.load(Ordering::Acquire),
            _ => panic!("unknown admission surface"),
        }
    }
}

impl PublicAdmissionMiddleware {
    pub(crate) const fn new(admission: PublicAdmission, inference_enabled: bool) -> Self {
        Self {
            admission,
            inference_enabled,
        }
    }
}

impl Drop for AdmissionPermit {
    fn drop(&mut self) {
        let admitted = match self.surface {
            AdmissionSurface::Inference => &self.admission.inner.inference_admitted,
            AdmissionSurface::Management => &self.admission.inner.management_admitted,
        };
        let previous = admitted.fetch_sub(1, Ordering::AcqRel);
        debug_assert!(previous > 0, "admission permit accounting underflow");
    }
}

impl AdmissionBody {
    fn new(inner: Body, permit: AdmissionPermit) -> Self {
        Self {
            inner: Box::pin(inner),
            permit: Some(permit),
        }
    }

    fn release(&mut self) {
        self.permit.take();
    }
}

impl http_body::Body for AdmissionBody {
    type Data = Bytes;
    type Error = axum::Error;

    fn poll_frame(
        self: Pin<&mut Self>,
        context: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Self::Data>, Self::Error>>> {
        let this = self.get_mut();
        match this.inner.as_mut().poll_frame(context) {
            Poll::Ready(None) => {
                this.release();
                Poll::Ready(None)
            }
            Poll::Ready(Some(Err(error))) => {
                this.release();
                Poll::Ready(Some(Err(error)))
            }
            other => other,
        }
    }

    fn is_end_stream(&self) -> bool {
        self.inner.is_end_stream()
    }

    fn size_hint(&self) -> SizeHint {
        self.inner.size_hint()
    }
}

pub(crate) async fn admit_public_request(
    State(state): State<PublicAdmissionMiddleware>,
    request: Request<Body>,
    next: middleware::Next,
) -> Response {
    let endpoint = state
        .inference_enabled
        .then(|| InferenceEndpoint::classify(request.method(), request.uri().path()))
        .flatten();
    let surface = if endpoint.is_some() {
        AdmissionSurface::Inference
    } else {
        AdmissionSurface::Management
    };
    let permit = match state.admission.try_acquire(surface) {
        Ok(permit) => permit,
        Err(()) => return overload_response(surface, endpoint, request.uri()),
    };

    let response = next.run(request).await;
    let (parts, body) = response.into_parts();
    Response::from_parts(parts, Body::new(AdmissionBody::new(body, permit)))
}

fn overload_response(
    admission_surface: AdmissionSurface,
    endpoint: Option<InferenceEndpoint>,
    uri: &axum::http::Uri,
) -> Response {
    let mut response = match (admission_surface, endpoint) {
        (AdmissionSurface::Inference, Some(endpoint)) => {
            gateway::protocol_error::inference_error_response(
                endpoint.surface(),
                gateway::error::InferenceError::overloaded(),
            )
        }
        (AdmissionSurface::Management, _) => Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "request_admission_overloaded",
            "Service unavailable",
            "The service is temporarily overloaded.",
        )
        .with_instance(uri)
        .into_response(),
        (AdmissionSurface::Inference, None) => {
            unreachable!("inference admission requires a classified endpoint")
        }
    };
    response.headers_mut().insert(
        header::RETRY_AFTER,
        HeaderValue::from_static(RETRY_AFTER_SECONDS),
    );
    response
}

#[cfg(test)]
mod tests;
