use futures::StreamExt;
use olp_domain::{CanonicalEvent, CanonicalEventKind, ProviderEventStream, RouteSlug};

use crate::{
    ApiState,
    gateway::{
        InferenceError, RoutedEventExecution, UsageCapture, emit_event_execution, release_limits,
    },
};

const MAX_COLLECTED_CANONICAL_EVENT_BYTES: usize = 16 * 1024 * 1024;

pub(crate) async fn collect_provider_events(
    first: CanonicalEvent,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
) -> Result<Vec<CanonicalEvent>, InferenceError> {
    collect_provider_events_with_limit(first, events, deadline, MAX_COLLECTED_CANONICAL_EVENT_BYTES)
        .await
}

async fn collect_provider_events_with_limit(
    first: CanonicalEvent,
    events: &mut ProviderEventStream,
    deadline: tokio::time::Instant,
    maximum_bytes: usize,
) -> Result<Vec<CanonicalEvent>, InferenceError> {
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

fn collected_event_bytes(
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

pub(crate) struct CompletedEventExecution {
    pub events: Vec<CanonicalEvent>,
    pub route_slug: RouteSlug,
    pub request_id: uuid::Uuid,
    completion: Option<EventExecutionCompletion>,
}

struct EventExecutionCompletion {
    state: ApiState,
    execution: RoutedEventExecution,
    usage: UsageCapture,
}

impl CompletedEventExecution {
    pub(crate) fn mark_success(&mut self) {
        if let Some(completion) = self.completion.take() {
            emit_event_execution(
                &completion.state,
                &completion.execution,
                &completion.usage,
                None,
            );
        }
    }
}

impl Drop for CompletedEventExecution {
    fn drop(&mut self) {
        let Some(completion) = self.completion.take() else {
            return;
        };
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider events were not representable on the client protocol.",
        );
        emit_event_execution(
            &completion.state,
            &completion.execution,
            &completion.usage,
            Some(&failure),
        );
    }
}

pub(crate) async fn collect_event_execution(
    state: &ApiState,
    mut execution: RoutedEventExecution,
) -> Result<CompletedEventExecution, InferenceError> {
    let result = collect_provider_events(
        execution.first.clone(),
        &mut execution.events,
        execution.deadline,
    )
    .await;
    let events = match result {
        Ok(events) => events,
        Err(failure) => {
            emit_event_execution(state, &execution, &UsageCapture::default(), Some(&failure));
            release_limits(state, execution.lease.as_ref()).await;
            return Err(failure);
        }
    };
    let mut usage = UsageCapture::default();
    for event in &events {
        usage.observe(event);
    }
    release_limits(state, execution.lease.as_ref()).await;
    Ok(CompletedEventExecution {
        events,
        route_slug: execution.route_slug.clone(),
        request_id: execution.request_id,
        completion: Some(EventExecutionCompletion {
            state: state.clone(),
            execution,
            usage,
        }),
    })
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use axum::http::StatusCode;
    use futures::stream;
    use olp_domain::{CanonicalEvent, CanonicalEventKind, ProviderEventStream};

    use super::collect_provider_events_with_limit;

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

        let error = collect_provider_events_with_limit(
            first,
            &mut events,
            tokio::time::Instant::now() + Duration::from_secs(1),
            maximum,
        )
        .await
        .unwrap_err();
        let problem = error.into_problem();
        assert_eq!(problem.status, StatusCode::BAD_GATEWAY.as_u16());
        assert_eq!(
            problem.problem_type.as_ref(),
            "https://openllmproxy.dev/problems/provider_protocol_error"
        );
    }
}
