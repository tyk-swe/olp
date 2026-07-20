use http::{HeaderMap, HeaderValue, header};
use olp_domain::{
    AttemptFailureClass, Operation, ProviderOutput, ProviderRequest, Surface, TransportError,
    TransportMode, TransportPhase,
};
use olp_protocols::openai::{
    ChatCompletionRequest, encode_chat_completion, encode_response_create,
};
use tokio::time::{Instant, timeout};

use super::super::{OpenAiConnector, errors::*, media::*};

pub(super) async fn execute(
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let Operation::Generation(generation) = &request.operation else {
        unreachable!("checked by caller")
    };
    let mut generation = generation.clone();
    let responses_endpoint = generation
        .extensions
        .values
        .remove("/__olp/openai_endpoint")
        .and_then(|value| value.as_str().map(str::to_owned))
        .is_some_and(|endpoint| endpoint == "responses");
    generation
        .extensions
        .ensure_representable_on(Surface::OpenAi)
        .map_err(|error| {
            transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                error.to_string(),
            )
        })?;

    let streaming = request.metadata.mode == TransportMode::Streaming;
    let body = if responses_endpoint {
        let mut wire = encode_response_create(&generation, &request.attempt.provider_model)
            .map_err(|error| protocol_encode_error("Responses", error))?;
        hydrate_responses_media(&mut wire.input, request.media.as_ref()).await?;
        serialize_wire("Responses", &wire)?
    } else {
        let mut wire = encode_chat_completion(&generation, &request.attempt.provider_model)
            .map_err(|error| protocol_encode_error("chat", error))?;
        if streaming {
            require_stream_usage(&mut wire)?;
        }
        hydrate_chat_media(&mut wire, request.media.as_ref()).await?;
        serialize_wire("chat", &wire)?
    };

    let started = Instant::now();
    let attempt_deadline = started + request.attempt.timeout.as_duration();
    let connect_timeout = bounded_duration(
        connector.config.timeouts.connect,
        remaining(attempt_deadline, TransportPhase::Connect)?,
    );
    // Resolution is validated and pinned before any credential is copied
    // into an HTTP header or request object.
    let client = connector
        .config
        .endpoint
        .pinned_client(connect_timeout)
        .await
        .map_err(map_endpoint_error)?;
    let url = connector
        .config
        .endpoint
        .resource_url(if responses_endpoint {
            "responses"
        } else {
            "chat/completions"
        })
        .map_err(map_endpoint_error)?;

    let first_byte_deadline = Instant::now() + connector.config.timeouts.first_byte;
    let mut headers = HeaderMap::new();
    connector.attach_auth(&mut headers)?;
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    headers.insert(
        header::ACCEPT,
        HeaderValue::from_static(if streaming {
            "text/event-stream"
        } else {
            "application/json"
        }),
    );
    headers.insert(
        "x-request-id",
        HeaderValue::from_str(&request.metadata.request_id.to_string()).map_err(|_| {
            transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                "request ID cannot be represented as an HTTP header",
            )
        })?,
    );

    let send_wait =
        remaining_until(first_byte_deadline, attempt_deadline).ok_or_else(first_byte_timeout)?;
    let response = timeout(
        send_wait,
        client.post(url).headers(headers).body(body).send(),
    )
    .await
    .map_err(|_| first_byte_timeout())?
    .map_err(map_send_error)?;

    if !response.status().is_success() {
        return Err(connector
            .map_error_response(response, attempt_deadline)
            .await);
    }

    let events = if streaming {
        connector
            .streaming_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                responses_endpoint,
            )
            .await
    } else {
        connector
            .unary_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                responses_endpoint,
            )
            .await
    }?;
    Ok(ProviderOutput::Events(events))
}

fn require_stream_usage(request: &mut ChatCompletionRequest) -> Result<(), TransportError> {
    let options = request
        .extra
        .entry("stream_options".to_owned())
        .or_insert_with(|| serde_json::json!({}));
    let Some(options) = options.as_object_mut() else {
        return Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI stream_options extension must be an object",
        ));
    };
    options.insert("include_usage".to_owned(), serde_json::Value::Bool(true));
    Ok(())
}
