//! Private health and metrics routing backed by asynchronously refreshed snapshots.

use std::time::Duration;

use axum::BoxError;
use axum::Router;
use axum::routing::get;
use tower::ServiceBuilder;

use crate::http::problem::Problem;
use crate::observability::metrics::metrics;
use crate::observability::readiness::live;
use crate::observability::readiness::ready;
use crate::observability::state::ObservabilityState;

const OBSERVABILITY_CONCURRENCY_LIMIT: usize = 8;
const OBSERVABILITY_REQUEST_TIMEOUT: Duration = Duration::from_secs(2);

/// Builds the private observability router. It exposes no console,
/// management, or inference routes.
pub fn router(state: ObservabilityState) -> Router {
    Router::new()
        .route("/health/live", get(live))
        .route("/health/ready", get(ready))
        .route("/metrics", get(metrics))
        .with_state(state)
        .layer(
            ServiceBuilder::new()
                .layer(axum::error_handling::HandleErrorLayer::new(
                    observability_service_error,
                ))
                .layer(tower::load_shed::LoadShedLayer::new())
                .layer(tower::limit::ConcurrencyLimitLayer::new(
                    OBSERVABILITY_CONCURRENCY_LIMIT,
                ))
                .layer(tower::timeout::TimeoutLayer::new(
                    OBSERVABILITY_REQUEST_TIMEOUT,
                )),
        )
}

async fn observability_service_error(error: BoxError) -> Problem {
    if error.is::<tower::timeout::error::Elapsed>() {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_timeout",
            "Observability unavailable",
            "The observability request exceeded its deadline.",
        )
    } else {
        Problem::new(
            axum::http::StatusCode::SERVICE_UNAVAILABLE,
            "observability_overloaded",
            "Observability unavailable",
            "The observability listener is temporarily overloaded.",
        )
    }
}

pub mod cache;

pub mod health_http;

pub mod metrics;

pub mod providers;

pub mod readiness;

pub mod state;

pub mod tracing;

pub mod workers;
