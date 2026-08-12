use olp_engine::domain::{
    ApiKey, OperationKind, RouteSlug, Surface, TransportMode, select_attempts,
};

use olp_engine::inference::runtime::RuntimeBundle;

use super::protocol_error::ProtocolError;

pub(super) fn visible_routes(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    surface: Surface,
) -> Vec<RouteSlug> {
    runtime
        .routes
        .keys()
        .filter(|slug| key.allowed_routes.is_empty() || key.allowed_routes.contains(*slug))
        .filter(|slug| route_is_visible(runtime, slug, surface))
        .cloned()
        .collect()
}

pub(super) fn visible_route(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    id: &str,
    surface: Surface,
) -> Result<RouteSlug, ProtocolError> {
    let slug = RouteSlug::parse(id.to_owned()).map_err(|_| {
        ProtocolError::not_found(
            surface,
            "The requested model does not exist or is unavailable.",
        )
    })?;
    if (!key.allowed_routes.is_empty() && !key.allowed_routes.contains(&slug))
        || !runtime.routes.contains_key(&slug)
        || !route_is_visible(runtime, &slug, surface)
    {
        return Err(ProtocolError::not_found(
            surface,
            "The requested model does not exist or is unavailable.",
        ));
    }
    Ok(slug)
}

pub(super) fn after_cursor_start(
    routes: &[RouteSlug],
    cursor: Option<&str>,
    surface: Surface,
    stale_message: &'static str,
) -> Result<usize, ProtocolError> {
    match cursor {
        Some(cursor) => routes
            .iter()
            .position(|slug| slug.as_str() == cursor)
            .map(|index| index.saturating_add(1))
            .ok_or_else(|| ProtocolError::invalid(surface, stale_message)),
        None => Ok(0),
    }
}

pub(super) fn before_cursor_end(
    routes: &[RouteSlug],
    cursor: Option<&str>,
    surface: Surface,
    stale_message: &'static str,
) -> Result<usize, ProtocolError> {
    match cursor {
        Some(cursor) => routes
            .iter()
            .position(|slug| slug.as_str() == cursor)
            .ok_or_else(|| ProtocolError::invalid(surface, stale_message)),
        None => Ok(routes.len()),
    }
}

pub(super) fn supported_operations(
    runtime: &RuntimeBundle,
    slug: &RouteSlug,
    surface: Surface,
) -> Vec<OperationKind> {
    [OperationKind::Generation, OperationKind::TokenCount]
        .into_iter()
        .filter(|operation| {
            let modes: &[TransportMode] = if *operation == OperationKind::Generation {
                &[TransportMode::Unary, TransportMode::Streaming]
            } else {
                &[TransportMode::Unary]
            };
            modes.iter().any(|mode| {
                select_attempts(runtime, slug, *operation, surface, *mode, &[0; 16]).is_ok()
            })
        })
        .collect()
}

fn route_is_visible(runtime: &RuntimeBundle, slug: &RouteSlug, surface: Surface) -> bool {
    !supported_operations(runtime, slug, surface).is_empty()
}

#[cfg(test)]
mod tests {
    use axum::response::IntoResponse as _;
    use http_body_util::BodyExt as _;
    use serde_json::json;

    use super::*;

    fn routes() -> Vec<RouteSlug> {
        ["alpha", "beta", "gamma"]
            .into_iter()
            .map(RouteSlug::parse)
            .collect::<Result<_, _>>()
            .unwrap()
    }

    #[test]
    fn cursor_windows_exclude_the_named_boundary() {
        let routes = routes();
        assert_eq!(
            after_cursor_start(&routes, None, Surface::OpenAi, "stale").unwrap(),
            0
        );
        assert_eq!(
            after_cursor_start(&routes, Some("beta"), Surface::OpenAi, "stale").unwrap(),
            2
        );
        assert_eq!(
            before_cursor_end(&routes, None, Surface::OpenAi, "stale").unwrap(),
            routes.len()
        );
        assert_eq!(
            before_cursor_end(&routes, Some("beta"), Surface::OpenAi, "stale").unwrap(),
            1
        );
    }

    #[tokio::test]
    async fn stale_cursor_errors_retain_the_callers_surface_and_message() {
        let cases = [
            (
                after_cursor_start(
                    &routes(),
                    Some("missing"),
                    Surface::Anthropic,
                    "after stale",
                )
                .unwrap_err(),
                json!({
                    "type": "error",
                    "error": {"type": "invalid_request_error", "message": "after stale"}
                }),
            ),
            (
                before_cursor_end(&routes(), Some("missing"), Surface::Gemini, "before stale")
                    .unwrap_err(),
                json!({
                    "error": {"code": 400, "message": "before stale", "status": "INVALID_ARGUMENT"}
                }),
            ),
        ];

        for (error, expected_body) in cases {
            let response = error.into_response();
            assert_eq!(response.status(), http::StatusCode::BAD_REQUEST);
            let body = response.into_body().collect().await.unwrap().to_bytes();
            assert_eq!(
                serde_json::from_slice::<serde_json::Value>(&body).unwrap(),
                expected_body
            );
        }
    }
}
