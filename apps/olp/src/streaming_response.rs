use std::convert::Infallible;

use axum::{
    body::{Body, Bytes},
    http::{HeaderValue, header},
    response::Response,
};
use futures::{StreamExt, stream};
use olp_domain::{CanonicalEvent, CanonicalEventKind};
use olp_protocols::sse::{SseEncodeError, SseFrame, encode_frame};

use crate::{
    ApiState,
    gateway::{
        InferenceError, RoutedEventExecution, UsageCapture, emit_event_execution, release_limits,
    },
};

pub(crate) fn encode_sse_frame(frame: &SseFrame) -> Result<Bytes, SseEncodeError> {
    encode_frame(frame).map(Bytes::from)
}

pub(crate) fn encode_server_sse_frame(frame: &SseFrame) -> Bytes {
    encode_sse_frame(frame).expect("server-generated SSE event fields are valid")
}

pub(crate) fn sse_response(
    receiver: tokio::sync::mpsc::Receiver<Result<Bytes, Infallible>>,
) -> Response {
    let body_stream = stream::unfold(receiver, |mut receiver| async move {
        receiver.recv().await.map(|item| (item, receiver))
    });
    let mut response = Response::new(Body::from_stream(body_stream));
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/event-stream; charset=utf-8"),
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
    response
}

pub(crate) async fn send_stream_chunk(
    sender: &tokio::sync::mpsc::Sender<Result<Bytes, Infallible>>,
    bytes: Bytes,
    deadline: tokio::time::Instant,
) -> bool {
    tokio::select! {
        () = sender.closed() => false,
        () = tokio::time::sleep_until(deadline) => false,
        result = sender.send(Ok(bytes)) => result.is_ok(),
    }
}

pub(crate) trait ProtocolStreamEncoder: Send + 'static {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String>;
    fn encode_error(&self, error: &InferenceError) -> Bytes;
}

pub(crate) fn protocol_streaming_response<E>(
    state: ApiState,
    mut execution: RoutedEventExecution,
    mut encoder: E,
) -> Response
where
    E: ProtocolStreamEncoder,
{
    let (sender, receiver) = tokio::sync::mpsc::channel::<Result<Bytes, Infallible>>(32);
    tokio::spawn(async move {
        let mut events = std::mem::replace(&mut execution.events, Box::pin(stream::empty()));
        let mut next = Some(Ok(execution.first.clone()));
        let mut usage = UsageCapture::default();
        let mut failure = None;
        let mut failure_encoded = false;
        let mut saw_done = false;
        while let Some(item) = next {
            let event = match item {
                Ok(event) => event,
                Err(error) => {
                    failure = Some(InferenceError::from_transport(error));
                    break;
                }
            };
            usage.observe(&event);
            let terminal = matches!(event.kind, CanonicalEventKind::Done);
            let canonical_failure = match &event.kind {
                CanonicalEventKind::Error { error } => Some(InferenceError::from_canonical(error)),
                _ => None,
            };
            match encoder.push(event) {
                Ok(chunks) => {
                    for chunk in chunks {
                        if !send_stream_chunk(&sender, chunk, execution.deadline).await {
                            failure = Some(if sender.is_closed() {
                                InferenceError::client_cancelled()
                            } else {
                                InferenceError::timeout()
                            });
                            break;
                        }
                    }
                    if let Some(canonical_failure) = canonical_failure {
                        failure = Some(canonical_failure);
                        failure_encoded = true;
                    }
                }
                Err(message) => {
                    failure = Some(InferenceError::bad_gateway(
                        "provider_protocol_error",
                        message,
                    ));
                }
            }
            if terminal {
                saw_done = true;
            }
            if terminal || failure.is_some() {
                break;
            }
            next = tokio::select! {
                () = sender.closed() => {
                    failure = Some(InferenceError::client_cancelled());
                    None
                }
                () = tokio::time::sleep_until(execution.deadline) => {
                    failure = Some(InferenceError::timeout());
                    None
                }
                next = events.next() => next,
            };
        }
        if !saw_done && failure.is_none() {
            failure = Some(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider stream ended without a terminal event.",
            ));
        }
        if let Some(error) = &failure
            && !failure_encoded
            && !sender.is_closed()
        {
            let _ = sender.try_send(Ok(encoder.encode_error(error)));
        }
        drop(events);
        emit_event_execution(&state, &execution, &usage, failure.as_ref());
        release_limits(&state, execution.lease.as_ref()).await;
    });
    sse_response(receiver)
}

#[cfg(test)]
mod tests {
    use std::{convert::Infallible, time::Duration};

    use axum::{
        body::Bytes,
        http::{StatusCode, header},
    };
    use http_body_util::BodyExt as _;

    use super::{encode_sse_frame, send_stream_chunk, sse_response};

    #[test]
    fn encode_sse_frame_preserves_event_id_and_data_line_bytes() {
        let encoded = encode_sse_frame(&olp_protocols::sse::SseFrame {
            event: Some("message".to_owned()),
            data: "first\nsecond".to_owned(),
            id: Some("event-7".to_owned()),
            retry_ms: None,
        })
        .unwrap();

        assert_eq!(
            encoded.as_ref(),
            b"event: message\nid: event-7\ndata: first\ndata: second\n\n"
        );
    }

    #[test]
    fn encode_sse_frame_includes_retry_and_empty_data() {
        let encoded = encode_sse_frame(&olp_protocols::sse::SseFrame {
            event: None,
            data: String::new(),
            id: Some("event-8".to_owned()),
            retry_ms: Some(250),
        })
        .unwrap();

        assert_eq!(encoded.as_ref(), b"id: event-8\nretry: 250\ndata: \n\n");
    }

    #[tokio::test]
    async fn sse_response_preserves_streamed_bytes_and_headers() {
        let (sender, receiver) = tokio::sync::mpsc::channel::<Result<Bytes, Infallible>>(1);
        let response = sse_response(receiver);
        assert!(
            send_stream_chunk(
                &sender,
                Bytes::from_static(b"data: payload\n\n"),
                tokio::time::Instant::now() + Duration::from_secs(1),
            )
            .await
        );
        drop(sender);

        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            response.headers()[header::CONTENT_TYPE],
            "text/event-stream; charset=utf-8"
        );
        assert_eq!(response.headers()[header::CACHE_CONTROL], "no-cache");
        assert_eq!(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .as_ref(),
            b"data: payload\n\n"
        );
    }
}
