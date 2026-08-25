//! Canonical event collection shared by HTTP adapters and control-plane use cases.

use crate::domain::{
    canonical::events::{Event, Kind},
    ports::ProviderEventStream,
};
use futures::StreamExt;

use crate::inference::error::Error as InferenceError;

pub const MAX_COLLECTED_CANONICAL_EVENT_BYTES: usize = 16 * 1024 * 1024;

pub async fn collect(
    first: Event,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
) -> Result<Vec<Event>, InferenceError> {
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
    first: Event,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
    maximum_bytes: usize,
    observe: &mut (dyn FnMut(&Event) + Send),
) -> Result<Vec<Event>, InferenceError> {
    observe(&first);
    if let Kind::Error { error } = &first.kind {
        return Err(InferenceError::from_canonical(error));
    }
    let mut collected_bytes = collected_event_bytes(0, &first, maximum_bytes)?;
    let mut collected = vec![first];
    while !matches!(collected.last().map(|event| &event.kind), Some(Kind::Done)) {
        let next = tokio::time::timeout_at(deadline, events.next())
            .await
            .map_err(|_| InferenceError::timeout())?;
        match next {
            Some(Ok(event)) => {
                observe(&event);
                if let Kind::Error { error } = &event.kind {
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
    event: &Event,
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

    use crate::domain::{
        canonical::events::{Error, ErrorClass, Event, Kind},
        ports::{AttemptFailureClass, ProviderEventStream, TransportError, TransportPhase},
    };
    use futures::{StreamExt, stream};

    use super::{collect_provider_events_with_observer, collected_event_bytes};

    fn start() -> Event {
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: None,
                provider_model: Some("model".to_owned()),
            },
        )
    }

    fn canonical_error(sequence: u64) -> Event {
        Event::new(
            sequence,
            Kind::Error {
                error: Error {
                    class: ErrorClass::Upstream,
                    message: "safe failure".to_owned(),
                    provider_code: Some("rejected".to_owned()),
                    retryable: false,
                },
            },
        )
    }

    async fn collect(
        first: Event,
        items: Vec<Result<Event, TransportError>>,
    ) -> Result<Vec<Event>, super::InferenceError> {
        let mut events: ProviderEventStream = Box::pin(stream::iter(items));
        collect_provider_events_with_observer(
            first,
            &mut events,
            tokio::time::Instant::now() + Duration::from_secs(1),
            usize::MAX,
            &mut |_| {},
        )
        .await
    }

    #[tokio::test]
    async fn collection_stops_at_done_and_observes_only_consumed_events() {
        let done = Event::new(1, Kind::Done);
        let trailing = Event::new(2, Kind::Done);
        let mut events: ProviderEventStream =
            Box::pin(stream::iter([Ok(done.clone()), Ok(trailing.clone())]));
        let mut observed = Vec::new();

        let collected = collect_provider_events_with_observer(
            start(),
            &mut events,
            tokio::time::Instant::now() + Duration::from_secs(1),
            usize::MAX,
            &mut |event| observed.push(event.sequence),
        )
        .await
        .unwrap();

        assert_eq!(observed, [0, 1]);
        assert_eq!(collected, [start(), done]);
        assert_eq!(events.next().await.unwrap().unwrap(), trailing);
    }

    #[tokio::test]
    async fn collection_normalizes_each_terminal_failure() {
        let transport_error = TransportError {
            upstream: Default::default(),
            phase: TransportPhase::Body,
            class: AttemptFailureClass::UpstreamServer,
            response_committed: true,
            message: "safe upstream failure".to_owned(),
        };
        let cases = [
            (
                collect(canonical_error(0), vec![]).await.unwrap_err(),
                "upstream_error",
                "safe failure",
            ),
            (
                collect(start(), vec![Ok(canonical_error(1))])
                    .await
                    .unwrap_err(),
                "upstream_error",
                "safe failure",
            ),
            (
                collect(start(), vec![Err(transport_error)])
                    .await
                    .unwrap_err(),
                "upstream_unavailable",
                "safe upstream failure",
            ),
            (
                collect(start(), vec![]).await.unwrap_err(),
                "provider_protocol_error",
                "The provider response ended without a terminal event.",
            ),
        ];

        for (error, expected_code, expected_message) in cases {
            assert_eq!(error.code(), expected_code);
            assert_eq!(error.message(), expected_message);
        }
    }

    #[tokio::test(start_paused = true)]
    async fn collection_enforces_its_absolute_deadline() {
        let mut events: ProviderEventStream = Box::pin(stream::pending());
        let error = collect_provider_events_with_observer(
            start(),
            &mut events,
            tokio::time::Instant::now() + Duration::from_millis(1),
            usize::MAX,
            &mut |_| {},
        )
        .await
        .unwrap_err();

        assert_eq!(error.code(), "gateway_timeout");
    }

    #[tokio::test]
    async fn collection_bounds_each_event_and_the_aggregate() {
        let first = start();
        let maximum = serde_json::to_vec(&first).unwrap().len();
        let mut events: ProviderEventStream =
            Box::pin(stream::iter([Ok(Event::new(1, Kind::Done))]));

        let error = collect_provider_events_with_observer(
            first.clone(),
            &mut events,
            tokio::time::Instant::now() + Duration::from_secs(1),
            maximum,
            &mut |_| {},
        )
        .await
        .unwrap_err();
        assert_eq!(error.code(), "provider_protocol_error");

        assert_eq!(collected_event_bytes(0, &first, maximum).unwrap(), maximum);
        for (current, limit) in [(1, maximum), (usize::MAX, usize::MAX)] {
            assert_eq!(
                collected_event_bytes(current, &first, limit)
                    .unwrap_err()
                    .code(),
                "provider_protocol_error"
            );
        }
    }
}
