//! Registry expansion for documentation export.
//!
//! `docs/compatibility.md` lists one row per addressable inference endpoint,
//! so the single [`Policy::GeminiAction`] row per Gemini API version expands
//! into the three actions its classifier accepts. Nothing here is used at
//! runtime; the export exists so the published table cannot drift from
//! [`ENDPOINTS`].

use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;

use crate::inference::http::endpoint_policy::classification::GEMINI_COUNT_TOKENS_SUFFIX;
use crate::inference::http::endpoint_policy::classification::GEMINI_GENERATE_SUFFIX;
use crate::inference::http::endpoint_policy::classification::GEMINI_STREAM_GENERATE_SUFFIX;
use crate::inference::http::endpoint_policy::registry::ENDPOINTS;
use crate::inference::http::endpoint_policy::registry::EndpointMethod;
use crate::inference::http::endpoint_policy::registry::EndpointSpec;
use crate::inference::http::endpoint_policy::registry::PathMatcher;
use crate::inference::http::endpoint_policy::registry::Policy;

/// One documented endpoint of the inference surface.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RegisteredOperation {
    /// Uppercase HTTP method.
    pub method: &'static str,
    /// Display form of the route path, with the Gemini remainder matcher
    /// written as `{model}` and the action suffix appended.
    pub path: String,
    /// Additional route paths serving the same endpoint.
    pub aliases: Vec<&'static str>,
    pub surface: Surface,
    pub operation: OperationKind,
}

/// Expands the endpoint registry into the documented operation rows, in
/// registry order.
#[must_use]
pub fn registered_operations() -> Vec<RegisteredOperation> {
    ENDPOINTS.iter().flat_map(expand).collect()
}

fn expand(spec: &'static EndpointSpec) -> Vec<RegisteredOperation> {
    let path = display_path(spec);
    match spec.policy {
        Policy::Fixed { operation, .. } => vec![row(spec, path, operation)],
        Policy::GeminiAction => [
            (GEMINI_GENERATE_SUFFIX, OperationKind::Generation),
            (GEMINI_STREAM_GENERATE_SUFFIX, OperationKind::Generation),
            (GEMINI_COUNT_TOKENS_SUFFIX, OperationKind::TokenCount),
        ]
        .into_iter()
        .map(|(suffix, operation)| row(spec, format!("{path}{suffix}"), operation))
        .collect(),
    }
}

fn row(spec: &'static EndpointSpec, path: String, operation: OperationKind) -> RegisteredOperation {
    RegisteredOperation {
        method: method_name(spec.method),
        path,
        aliases: spec.aliases.iter().map(|alias| alias.route_path).collect(),
        surface: spec.surface,
        operation,
    }
}

/// The remainder matcher captures `models/{model}` and, for actions, the
/// `:action` suffix; `{*resource}` is an axum matcher spelling rather than
/// something a caller writes, so the table names the model instead.
fn display_path(spec: &'static EndpointSpec) -> String {
    match spec.matcher {
        PathMatcher::Remainder { .. } => spec.route_path.replace("{*resource}", "{model}"),
        PathMatcher::Exact | PathMatcher::SingleSegment { .. } => spec.route_path.to_owned(),
    }
}

const fn method_name(method: EndpointMethod) -> &'static str {
    match method {
        EndpointMethod::Get => "GET",
        EndpointMethod::Post => "POST",
        EndpointMethod::Delete => "DELETE",
    }
}
