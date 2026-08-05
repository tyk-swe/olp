//! Canonical event collection shared by HTTP adapters and control-plane use cases.

use futures::StreamExt;
use olp_domain::{CanonicalEvent, CanonicalEventKind, ProviderEventStream};

use crate::InferenceError;

pub const MAX_COLLECTED_CANONICAL_EVENT_BYTES: usize = 16 * 1024 * 1024;

pub async fn collect_provider_events(
    first: CanonicalEvent,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
) -> Result<Vec<CanonicalEvent>, InferenceError> {
    collect_provider_events_with_observer(
        first,
        events,
        deadline,
        MAX_COLLECTED_CANONICAL_EVENT_BYTES,
        &mut |_| {},
    )
    .await
}

pub async fn collect_provider_events_with_observer(
    first: CanonicalEvent,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
    maximum_bytes: usize,
    observe: &mut (dyn FnMut(&CanonicalEvent) + Send),
) -> Result<Vec<CanonicalEvent>, InferenceError> {
    observe(&first);
    if let CanonicalEventKind::Error { error } = &first.kind {
        return Err(InferenceError::from_canonical(error));
    }
    let mut collected_bytes = collected_event_bytes(0, &first, maximum_bytes)?;
    let mut collected = vec![first];
    while !matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ) {
        let next = tokio::time::timeout_at(deadline, events.next())
            .await
            .map_err(|_| InferenceError::timeout())?;
        match next {
            Some(Ok(event)) => {
                observe(&event);
                if let CanonicalEventKind::Error { error } = &event.kind {
                    return Err(InferenceError::from_canonical(error));
                }
                collected_bytes = collected_event_bytes(collected_bytes, &event, maximum_bytes)?;
                collected.push(event);
            }
            Some(Err(error)) => return Err(InferenceError::from_transport(error)),
            None => {
                return Err(InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider response ended without a terminal event.",
                ));
            }
        }
    }
    Ok(collected)
}

pub fn collected_event_bytes(
    current: usize,
    event: &CanonicalEvent,
    maximum: usize,
) -> Result<usize, InferenceError> {
    let event_bytes = serde_json::to_vec(event).map_err(|_| {
        InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an event that could not be bounded.",
        )
    })?;
    current
        .checked_add(event_bytes.len())
        .filter(|total| *total <= maximum)
        .ok_or_else(|| {
            InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider response exceeded the collected-event limit.",
            )
        })
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use futures::stream;
    use olp_domain::{CanonicalEvent, CanonicalEventKind, ProviderEventStream};

    use super::collect_provider_events_with_observer;

    #[tokio::test]
    async fn unary_event_collection_has_an_aggregate_byte_limit() {
        let first = CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: Some("model".to_owned()),
            },
        );
        let maximum = serde_json::to_vec(&first).unwrap().len();
        let mut events: ProviderEventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
            1,
            CanonicalEventKind::Done,
        ))]));

        let error = collect_provider_events_with_observer(
            first,
            &mut events,
            tokio::time::Instant::now() + Duration::from_secs(1),
            maximum,
            &mut |_| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error.code(), "provider_protocol_error");
    }
}
