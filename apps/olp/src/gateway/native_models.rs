use olp_domain::{ApiKey, OperationKind, RouteSlug, Surface, TransportMode, select_attempts};

use crate::RuntimeBundle;

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
