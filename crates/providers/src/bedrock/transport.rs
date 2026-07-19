use std::{
    collections::{HashMap, VecDeque},
    time::Duration,
};

use aws_sdk_bedrock::types::ModelModality;
use aws_sdk_bedrockruntime::{
    error::{ProvideErrorMetadata, SdkError},
    operation::converse_stream::ConverseStreamOutput as ConverseStreamResponse,
    types::{ContentBlockDelta, ContentBlockStart, ConverseStreamOutput, CountTokensInput},
};
use futures::stream;
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    DiscoveredProviderModel, MessageRole, Operation, ProviderEventStream, ProviderKind,
    ProviderOutput, ProviderRequest, ProviderTransport, TokenCountResult, TransportError,
    TransportMode, TransportPhase,
};
use tokio::time::{Instant, timeout};

use crate::bedrock::{
    BedrockCredentials, ConnectorConfig, sdk_config,
    translate::{
        decode_converse, decode_stop_reason, decode_usage, encode_generation, encode_token_count,
        protocol_body_error, protocol_error,
    },
};

pub struct BedrockConnector {
    runtime: aws_sdk_bedrockruntime::Client,
    control: aws_sdk_bedrock::Client,
    timeouts: crate::bedrock::ConnectorTimeouts,
}

impl BedrockConnector {
    pub async fn new(config: ConnectorConfig, credentials: BedrockCredentials) -> Self {
        let shared = sdk_config(&config, credentials).await;
        let mut runtime_config = aws_sdk_bedrockruntime::config::Builder::from(&shared);
        let mut control_config = aws_sdk_bedrock::config::Builder::from(&shared);
        if let Some(endpoint_url) = &config.endpoint_url {
            runtime_config = runtime_config.endpoint_url(endpoint_url);
            control_config = control_config.endpoint_url(endpoint_url);
        }
        Self {
            runtime: aws_sdk_bedrockruntime::Client::from_conf(runtime_config.build()),
            control: aws_sdk_bedrock::Client::from_conf(control_config.build()),
            timeouts: config.timeouts,
        }
    }

    /// Discovers text-output foundation model IDs using the official Bedrock
    /// control-plane SDK. The returned model ID is preserved byte-for-byte for
    /// route target configuration.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        let response = timeout(
            self.timeouts.first_byte,
            self.control
                .list_foundation_models()
                .by_output_modality(ModelModality::Text)
                .send(),
        )
        .await
        .map_err(|_| deadline_error(TransportPhase::FirstByte, false))?
        .map_err(|error| map_sdk_error(&error, TransportPhase::FirstByte, false))?;
        let mut models = Vec::with_capacity(response.model_summaries().len());
        for summary in response.model_summaries() {
            let id = summary.model_id().trim();
            validate_model_id(id)?;
            let supports_text_generation = summary
                .output_modalities()
                .iter()
                .any(|modality| modality.as_str() == ModelModality::Text.as_str());
            if !supports_text_generation {
                continue;
            }
            models.push(DiscoveredProviderModel {
                id: id.to_owned(),
                display_name: summary.model_name().unwrap_or(id).to_owned(),
            });
        }
        models.sort_by(|left, right| left.id.cmp(&right.id));
        models.dedup_by(|left, right| left.id == right.id);
        Ok(models)
    }

    async fn execute_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        validate_request(&request)?;
        validate_model_id(&request.attempt.provider_model)?;
        let attempt_deadline = Instant::now() + request.attempt.timeout.as_duration();
        match &request.operation {
            Operation::Generation(generation) => {
                let encoded = encode_generation(generation)?;
                if request.metadata.mode == TransportMode::Streaming {
                    let send_wait = first_byte_wait(attempt_deadline, self.timeouts.first_byte)?;
                    let response = timeout(
                        send_wait,
                        self.runtime
                            .converse_stream()
                            .model_id(&request.attempt.provider_model)
                            .set_messages(Some(encoded.messages))
                            .set_system((!encoded.system.is_empty()).then_some(encoded.system))
                            .inference_config(encoded.inference_config)
                            .set_tool_config(encoded.tool_config)
                            .send(),
                    )
                    .await
                    .map_err(|_| deadline_error(TransportPhase::FirstByte, false))?
                    .map_err(|error| map_sdk_error(&error, TransportPhase::FirstByte, false))?;
                    Ok(ProviderOutput::Events(stream_events(
                        response,
                        request.attempt.provider_model.clone(),
                        attempt_deadline,
                        self.timeouts.idle,
                    )))
                } else {
                    // The AWS SDK buffers unary response bodies before `.send()`
                    // resolves. Socket inactivity is bounded by the SDK read
                    // timeout; this outer bound is therefore the total attempt,
                    // not a misleading first-byte deadline.
                    let wait = remaining(attempt_deadline, TransportPhase::Body, false)?;
                    let response = timeout(
                        wait,
                        self.runtime
                            .converse()
                            .model_id(&request.attempt.provider_model)
                            .set_messages(Some(encoded.messages))
                            .set_system((!encoded.system.is_empty()).then_some(encoded.system))
                            .inference_config(encoded.inference_config)
                            .set_tool_config(encoded.tool_config)
                            .send(),
                    )
                    .await
                    .map_err(|_| deadline_error(TransportPhase::Body, false))?
                    .map_err(|error| map_sdk_error(&error, TransportPhase::Body, false))?;
                    let events = decode_converse(response, &request.attempt.provider_model)?;
                    Ok(ProviderOutput::Events(Box::pin(stream::iter(
                        events.into_iter().map(Ok),
                    ))))
                }
            }
            Operation::TokenCount(count) => {
                if request.metadata.mode != TransportMode::Unary {
                    return Err(protocol_error(
                        "Bedrock token counting supports unary mode only",
                    ));
                }
                let input = encode_token_count(count)?;
                let wait = remaining(attempt_deadline, TransportPhase::Body, false)?;
                let response = timeout(
                    wait,
                    self.runtime
                        .count_tokens()
                        .model_id(&request.attempt.provider_model)
                        .input(CountTokensInput::Converse(input))
                        .send(),
                )
                .await
                .map_err(|_| deadline_error(TransportPhase::Body, false))?
                .map_err(|error| map_sdk_error(&error, TransportPhase::Body, false))?;
                let input_tokens = u64::try_from(response.input_tokens()).map_err(|_| {
                    protocol_body_error("Bedrock returned a negative input token count")
                })?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens,
                        extensions: olp_domain::SourceExtensions::default(),
                    }),
                )))
            }
            operation => Err(protocol_error(format!(
                "Bedrock connector does not support {:?}",
                operation.kind()
            ))),
        }
    }
}

impl ProviderTransport for BedrockConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(self.execute_request(request))
    }
}

struct StreamState {
    response: ConverseStreamResponse,
    pending: VecDeque<Result<CanonicalEvent, TransportError>>,
    sequence: u64,
    attempt_deadline: Instant,
    idle_timeout: Duration,
    saw_message_start: bool,
    saw_message_stop: bool,
    saw_metadata: bool,
    next_tool_index: u32,
    tool_indices: HashMap<u32, u32>,
    terminal: bool,
}

fn stream_events(
    response: ConverseStreamResponse,
    provider_model: String,
    attempt_deadline: Instant,
    idle_timeout: Duration,
) -> ProviderEventStream {
    let pending = VecDeque::from([Ok(CanonicalEvent::new(
        0,
        CanonicalEventKind::ResponseStart {
            response_id: None,
            provider_model: Some(provider_model),
        },
    ))]);
    Box::pin(stream::unfold(
        StreamState {
            response,
            pending,
            sequence: 1,
            attempt_deadline,
            idle_timeout,
            saw_message_start: false,
            saw_message_stop: false,
            saw_metadata: false,
            next_tool_index: 0,
            tool_indices: HashMap::new(),
            terminal: false,
        },
        |mut state| async move {
            loop {
                if let Some(item) = state.pending.pop_front() {
                    return Some((item, state));
                }
                if state.terminal {
                    return None;
                }
                let wait = match remaining(state.attempt_deadline, TransportPhase::Body, true) {
                    Ok(wait) => wait.min(state.idle_timeout),
                    Err(error) => {
                        state.terminal = true;
                        return Some((Err(error), state));
                    }
                };
                let event = match timeout(wait, state.response.stream.recv()).await {
                    Ok(Ok(Some(event))) => event,
                    Ok(Ok(None)) => {
                        state.terminal = true;
                        if !state.saw_message_stop {
                            return Some((
                                Err(protocol_body_error(
                                    "Bedrock stream ended before message_stop",
                                )),
                                state,
                            ));
                        }
                        let done = CanonicalEvent::new(state.sequence, CanonicalEventKind::Done);
                        return Some((Ok(done), state));
                    }
                    Ok(Err(error)) => {
                        state.terminal = true;
                        return Some((
                            Err(map_sdk_error(&error, TransportPhase::Body, true)),
                            state,
                        ));
                    }
                    Err(_) => {
                        state.terminal = true;
                        return Some((Err(deadline_error(TransportPhase::Body, true)), state));
                    }
                };
                match map_stream_event(event, &mut state) {
                    Ok(kinds) => {
                        for kind in kinds {
                            state
                                .pending
                                .push_back(Ok(CanonicalEvent::new(state.sequence, kind)));
                            state.sequence = state.sequence.saturating_add(1);
                        }
                    }
                    Err(error) => {
                        state.terminal = true;
                        return Some((Err(error), state));
                    }
                }
            }
        },
    ))
}

fn map_stream_event(
    event: ConverseStreamOutput,
    state: &mut StreamState,
) -> Result<Vec<CanonicalEventKind>, TransportError> {
    match event {
        ConverseStreamOutput::MessageStart(start) => {
            if state.saw_message_start || state.saw_message_stop || state.saw_metadata {
                return Err(protocol_body_error(
                    "Bedrock stream returned an out-of-order message_start event",
                ));
            }
            if start.role().as_str() != "assistant" {
                return Err(protocol_body_error(
                    "Bedrock stream returned a non-assistant output role",
                ));
            }
            state.saw_message_start = true;
            Ok(vec![CanonicalEventKind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            }])
        }
        ConverseStreamOutput::ContentBlockStart(start) => {
            require_content_phase(state)?;
            let bedrock_index = content_block_index(start.content_block_index)?;
            match start.start {
                None => Ok(Vec::new()),
                Some(ContentBlockStart::ToolUse(tool)) => {
                    if state.tool_indices.contains_key(&bedrock_index) {
                        return Err(protocol_body_error(
                            "Bedrock stream returned a duplicate tool content block",
                        ));
                    }
                    let tool_index = state.next_tool_index;
                    state.next_tool_index = state.next_tool_index.saturating_add(1);
                    state.tool_indices.insert(bedrock_index, tool_index);
                    Ok(vec![CanonicalEventKind::ToolCallDelta {
                        output_index: 0,
                        tool_index,
                        id: Some(tool.tool_use_id),
                        name: Some(tool.name),
                        arguments_delta: String::new(),
                    }])
                }
                Some(_) => Err(protocol_body_error(
                    "Bedrock stream started content that cannot be represented canonically",
                )),
            }
        }
        ConverseStreamOutput::ContentBlockDelta(delta) => {
            require_content_phase(state)?;
            let bedrock_index = content_block_index(delta.content_block_index)?;
            match delta.delta {
                Some(ContentBlockDelta::Text(text)) => Ok(vec![CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text,
                }]),
                Some(ContentBlockDelta::ToolUse(tool)) => {
                    Ok(vec![CanonicalEventKind::ToolCallDelta {
                        output_index: 0,
                        tool_index: *state.tool_indices.get(&bedrock_index).ok_or_else(|| {
                            protocol_body_error(
                                "Bedrock stream returned a tool delta before its block start",
                            )
                        })?,
                        id: None,
                        name: None,
                        arguments_delta: tool.input,
                    }])
                }
                Some(_) => Err(protocol_body_error(
                    "Bedrock stream returned a delta that cannot be represented canonically",
                )),
                None => Err(protocol_body_error(
                    "Bedrock stream returned an empty delta",
                )),
            }
        }
        ConverseStreamOutput::ContentBlockStop(stop) => {
            require_content_phase(state)?;
            content_block_index(stop.content_block_index)?;
            Ok(Vec::new())
        }
        ConverseStreamOutput::MessageStop(stop) => {
            if !state.saw_message_start || state.saw_message_stop || state.saw_metadata {
                return Err(protocol_body_error(
                    "Bedrock stream returned an out-of-order message_stop event",
                ));
            }
            if stop.additional_model_response_fields.is_some() {
                return Err(protocol_body_error(
                    "Bedrock stream returned vendor semantics that cannot be represented canonically",
                ));
            }
            state.saw_message_stop = true;
            Ok(vec![CanonicalEventKind::Finish {
                output_index: 0,
                reason: decode_stop_reason(&stop.stop_reason),
            }])
        }
        ConverseStreamOutput::Metadata(metadata) => {
            if !state.saw_message_stop || state.saw_metadata {
                return Err(protocol_body_error(
                    "Bedrock stream returned an out-of-order metadata event",
                ));
            }
            if metadata.trace.is_some() {
                return Err(protocol_body_error(
                    "Bedrock stream returned guardrail semantics that cannot be represented canonically",
                ));
            }
            state.saw_metadata = true;
            metadata
                .usage
                .as_ref()
                .map(decode_usage)
                .transpose()
                .map(|usage| {
                    usage
                        .map(|usage| vec![CanonicalEventKind::Usage { usage }])
                        .unwrap_or_default()
                })
        }
        _ => Err(protocol_body_error(
            "Bedrock stream returned an unknown event variant",
        )),
    }
}

fn require_content_phase(state: &StreamState) -> Result<(), TransportError> {
    if !state.saw_message_start || state.saw_message_stop || state.saw_metadata {
        Err(protocol_body_error(
            "Bedrock stream returned an out-of-order content event",
        ))
    } else {
        Ok(())
    }
}

fn content_block_index(index: i32) -> Result<u32, TransportError> {
    u32::try_from(index)
        .map_err(|_| protocol_body_error("Bedrock stream returned a negative content block index"))
}

fn validate_request(request: &ProviderRequest) -> Result<(), TransportError> {
    if request.attempt.provider_kind != ProviderKind::Bedrock {
        return Err(protocol_error(
            "Bedrock connector received a different provider kind",
        ));
    }
    if request.metadata.operation != request.operation.kind() {
        return Err(protocol_error(
            "request metadata operation does not match the canonical operation",
        ));
    }
    match &request.operation {
        Operation::Generation(generation) => {
            let streaming = request.metadata.mode == TransportMode::Streaming;
            if generation.parameters.stream != streaming {
                return Err(protocol_error(
                    "canonical stream flag does not match the selected transport mode",
                ));
            }
            if !matches!(
                request.metadata.mode,
                TransportMode::Unary | TransportMode::Streaming
            ) {
                return Err(protocol_error(
                    "Bedrock generation does not support async mode",
                ));
            }
        }
        Operation::TokenCount(_) if request.metadata.mode != TransportMode::Unary => {
            return Err(protocol_error(
                "Bedrock token counting supports unary mode only",
            ));
        }
        _ => {}
    }
    Ok(())
}

fn validate_model_id(model: &str) -> Result<(), TransportError> {
    if model.is_empty()
        || model.len() > 2_048
        || model.trim() != model
        || model.chars().any(char::is_control)
        || model.chars().any(char::is_whitespace)
    {
        return Err(protocol_error("Bedrock model ID or ARN is invalid"));
    }
    Ok(())
}

fn first_byte_wait(
    attempt_deadline: Instant,
    configured: Duration,
) -> Result<Duration, TransportError> {
    remaining(attempt_deadline, TransportPhase::FirstByte, false).map(|wait| wait.min(configured))
}

fn remaining(
    deadline: Instant,
    phase: TransportPhase,
    committed: bool,
) -> Result<Duration, TransportError> {
    deadline
        .checked_duration_since(Instant::now())
        .filter(|remaining| !remaining.is_zero())
        .ok_or_else(|| deadline_error(phase, committed))
}

fn deadline_error(phase: TransportPhase, committed: bool) -> TransportError {
    TransportError {
        phase,
        class: AttemptFailureClass::Timeout,
        response_committed: committed,
        message: "Bedrock request deadline exceeded".to_owned(),
    }
}

fn map_sdk_error<E, R>(
    error: &SdkError<E, R>,
    phase: TransportPhase,
    committed: bool,
) -> TransportError
where
    E: ProvideErrorMetadata,
{
    let class = match error {
        SdkError::TimeoutError(_) => AttemptFailureClass::Timeout,
        SdkError::DispatchFailure(failure) if failure.is_timeout() => AttemptFailureClass::Timeout,
        SdkError::DispatchFailure(failure) if failure.is_user() => AttemptFailureClass::Protocol,
        SdkError::DispatchFailure(_) => AttemptFailureClass::Connect,
        SdkError::ConstructionFailure(_) => AttemptFailureClass::Protocol,
        SdkError::ResponseError(_) => AttemptFailureClass::Protocol,
        SdkError::ServiceError(service) => classify_service_code(service.err().code()),
        _ => AttemptFailureClass::UpstreamServer,
    };
    TransportError {
        phase,
        class,
        response_committed: committed,
        message: "Bedrock SDK request failed".to_owned(),
    }
}

fn classify_service_code(code: Option<&str>) -> AttemptFailureClass {
    match code {
        Some("ThrottlingException" | "ServiceQuotaExceededException") => {
            AttemptFailureClass::RateLimit
        }
        Some(
            "InternalServerException"
            | "ServiceUnavailableException"
            | "ModelErrorException"
            | "ModelNotReadyException"
            | "ModelStreamErrorException",
        ) => AttemptFailureClass::UpstreamServer,
        Some("ModelTimeoutException") => AttemptFailureClass::Timeout,
        Some(
            "AccessDeniedException"
            | "UnrecognizedClientException"
            | "InvalidSignatureException"
            | "ExpiredTokenException",
        ) => AttemptFailureClass::UpstreamClient,
        Some("ValidationException" | "ResourceNotFoundException" | "ConflictException") => {
            AttemptFailureClass::UpstreamClient
        }
        _ => AttemptFailureClass::UpstreamServer,
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use aws_smithy_eventstream::frame::write_message_to;
    use aws_smithy_types::event_stream::{Header, HeaderValue, Message as EventMessage};
    use futures::StreamExt;
    use olp_domain::{
        AttemptPlan, DurationMs, GenerationParameters, GenerationRequest, Message, OperationKind,
        ProviderId, RequestId, RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId,
        SourceExtensions, Surface, TargetId, TokenCountRequest,
    };
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::TcpListener,
    };

    use super::*;
    use crate::bedrock::{BedrockCredentials, ConnectorTimeouts, StaticCredentials};

    fn provider_request() -> ProviderRequest {
        ProviderRequest {
            metadata: RequestMetadata {
                request_id: RequestId::new(),
                operation: OperationKind::Generation,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
            attempt: AttemptPlan {
                generation_id: RuntimeGenerationId::new(),
                route_id: RouteId::new(),
                target_id: TargetId::new(),
                provider_id: ProviderId::new(),
                provider_kind: ProviderKind::Bedrock,
                provider_model: "anthropic.claude-test-v1:0".to_owned(),
                timeout: DurationMs::new(2_000),
                priority: 0,
            },
            operation: Operation::Generation(GenerationRequest {
                route: RouteSlug::parse("chat").unwrap(),
                messages: vec![Message {
                    role: MessageRole::User,
                    content: vec![olp_domain::ContentPart::Text {
                        text: "hello".to_owned(),
                    }],
                    name: None,
                    tool_call_id: None,
                    tool_calls: vec![],
                }],
                parameters: GenerationParameters::default(),
                tools: vec![],
                tool_choice: None,
                response_format: None,
                extensions: SourceExtensions::default(),
            }),
            media: None,
        }
    }

    fn streaming_request() -> ProviderRequest {
        let mut request = provider_request();
        request.metadata.mode = TransportMode::Streaming;
        let Operation::Generation(generation) = &mut request.operation else {
            unreachable!();
        };
        generation.parameters.stream = true;
        request
    }

    fn token_count_request() -> ProviderRequest {
        let mut request = provider_request();
        request.metadata.operation = OperationKind::TokenCount;
        request.operation = Operation::TokenCount(TokenCountRequest {
            route: RouteSlug::parse("chat").unwrap(),
            input: vec![olp_domain::ContentPart::Text {
                text: "count this".to_owned(),
            }],
            extensions: SourceExtensions::default(),
        });
        request
    }

    #[test]
    fn connector_and_model_validation_is_explicit() {
        let request = provider_request();
        assert!(validate_request(&request).is_ok());
        assert!(validate_model_id("us.anthropic.claude-3-7-sonnet-20250219-v1:0").is_ok());
        assert!(
            validate_model_id("arn:aws:bedrock:us-east-1:123456789012:inference-profile/example")
                .is_ok()
        );
        assert!(validate_model_id("bad model").is_err());
    }

    #[test]
    fn service_error_taxonomy_is_retry_aware() {
        assert_eq!(
            classify_service_code(Some("ThrottlingException")),
            AttemptFailureClass::RateLimit
        );
        assert_eq!(
            classify_service_code(Some("ValidationException")),
            AttemptFailureClass::UpstreamClient
        );
        assert_eq!(
            classify_service_code(Some("ServiceUnavailableException")),
            AttemptFailureClass::UpstreamServer
        );
    }

    fn event_frame(event_type: &str, payload: &str) -> Vec<u8> {
        let message = EventMessage::new(payload.as_bytes().to_vec())
            .add_header(Header::new(
                ":message-type",
                HeaderValue::String("event".into()),
            ))
            .add_header(Header::new(
                ":event-type",
                HeaderValue::String(event_type.to_owned().into()),
            ))
            .add_header(Header::new(
                ":content-type",
                HeaderValue::String("application/json".into()),
            ));
        let mut encoded = Vec::new();
        write_message_to(&message, &mut encoded).unwrap();
        encoded
    }

    async fn serve_once(
        body: Vec<u8>,
        content_type: &'static str,
    ) -> (String, tokio::task::JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let task = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            let mut buffer = [0_u8; 4_096];
            let header_end = loop {
                let read = socket.read(&mut buffer).await.unwrap();
                assert_ne!(read, 0);
                request.extend_from_slice(&buffer[..read]);
                if let Some(position) = request.windows(4).position(|window| window == b"\r\n\r\n")
                {
                    break position + 4;
                }
            };
            let headers = String::from_utf8_lossy(&request[..header_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    line.to_ascii_lowercase()
                        .strip_prefix("content-length:")
                        .map(str::trim)
                        .and_then(|value| value.parse::<usize>().ok())
                })
                .unwrap_or(0);
            while request.len() < header_end + content_length {
                let read = socket.read(&mut buffer).await.unwrap();
                assert_ne!(read, 0);
                request.extend_from_slice(&buffer[..read]);
            }
            let response_headers = format!(
                "HTTP/1.1 200 OK\r\ncontent-type: {content_type}\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                body.len()
            );
            socket.write_all(response_headers.as_bytes()).await.unwrap();
            socket.write_all(&body).await.unwrap();
            socket.shutdown().await.unwrap();
            String::from_utf8_lossy(&request).into_owned()
        });
        (format!("http://{address}"), task)
    }

    async fn mock_connector(endpoint: &str) -> BedrockConnector {
        let config = ConnectorConfig::new("us-east-1")
            .unwrap()
            .with_timeouts(ConnectorTimeouts {
                connect: Duration::from_secs(1),
                first_byte: Duration::from_secs(1),
                idle: Duration::from_secs(1),
            })
            .unwrap()
            .with_endpoint_url(endpoint)
            .unwrap();
        let credentials = StaticCredentials::from_json(
            br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret"}"#,
        )
        .unwrap();
        BedrockConnector::new(config, BedrockCredentials::Static(credentials)).await
    }

    #[tokio::test]
    async fn official_sdk_decodes_local_converse_event_stream_and_signs_request() {
        let mut frames = Vec::new();
        for (kind, payload) in [
            ("messageStart", r#"{"role":"assistant"}"#),
            (
                "contentBlockDelta",
                r#"{"delta":{"text":"hello"},"contentBlockIndex":0}"#,
            ),
            ("contentBlockStop", r#"{"contentBlockIndex":0}"#),
            (
                "contentBlockStart",
                r#"{"start":{"toolUse":{"toolUseId":"call-1","name":"weather"}},"contentBlockIndex":1}"#,
            ),
            (
                "contentBlockDelta",
                r#"{"delta":{"toolUse":{"input":"{\"city\":\"Paris\"}"}},"contentBlockIndex":1}"#,
            ),
            ("contentBlockStop", r#"{"contentBlockIndex":1}"#),
            ("messageStop", r#"{"stopReason":"tool_use"}"#),
            (
                "metadata",
                r#"{"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":1}}"#,
            ),
        ] {
            frames.extend(event_frame(kind, payload));
        }
        let (endpoint, server) = serve_once(frames, "application/vnd.amazon.eventstream").await;
        let connector = mock_connector(&endpoint).await;
        let ProviderOutput::Events(events) = connector.execute(streaming_request()).await.unwrap()
        else {
            panic!("expected event stream");
        };
        let events: Vec<_> = events
            .collect::<Vec<_>>()
            .await
            .into_iter()
            .collect::<Result<_, _>>()
            .unwrap();
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::TextDelta { text, .. } if text == "hello"
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::ToolCallDelta {
                id: Some(id),
                name: Some(name),
                ..
            } if id == "call-1" && name == "weather"
        )));
        assert!(events.iter().any(|event| matches!(
            &event.kind,
            CanonicalEventKind::ToolCallDelta {
                arguments_delta,
                ..
            } if arguments_delta == "{\"city\":\"Paris\"}"
        )));
        let tool_indexes = events
            .iter()
            .filter_map(|event| match event.kind {
                CanonicalEventKind::ToolCallDelta { tool_index, .. } => Some(tool_index),
                _ => None,
            })
            .collect::<Vec<_>>();
        assert_eq!(tool_indexes, vec![0, 0]);
        assert!(
            events
                .iter()
                .any(|event| matches!(event.kind, CanonicalEventKind::Usage { .. }))
        );
        assert!(matches!(
            events.last().unwrap().kind,
            CanonicalEventKind::Done
        ));
        let request = server.await.unwrap().to_ascii_lowercase();
        assert!(request.starts_with("post /model/anthropic.claude-test-v1%3a0/converse-stream"));
        assert!(request.contains("authorization: aws4-hmac-sha256"));
        assert!(!request.contains("secret-secret-secret"));
    }

    #[tokio::test]
    async fn official_control_sdk_discovers_connector_specific_model_ids() {
        let body = br#"{"modelSummaries":[{"modelArn":"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-test","modelId":"anthropic.claude-test-v1:0","modelName":"Claude Test","providerName":"Anthropic","inputModalities":["TEXT"],"outputModalities":["TEXT"],"responseStreamingSupported":true,"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},{"modelArn":"arn:aws:bedrock:us-east-1::foundation-model/stability.image-test","modelId":"stability.image-test-v1:0","modelName":"Image Test","providerName":"Stability AI","inputModalities":["TEXT"],"outputModalities":["IMAGE"],"responseStreamingSupported":false,"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}}]}"#.to_vec();
        let (endpoint, server) = serve_once(body, "application/json").await;
        let connector = mock_connector(&endpoint).await;
        let models = connector.discover_models().await.unwrap();
        assert_eq!(models.len(), 1);
        assert_eq!(models[0].id, "anthropic.claude-test-v1:0");
        assert_eq!(models[0].display_name, "Claude Test");
        let request = server.await.unwrap().to_ascii_lowercase();
        assert!(request.starts_with("get /foundation-models?byoutputmodality=text"));
        assert!(request.contains("authorization: aws4-hmac-sha256"));
    }

    #[tokio::test]
    async fn official_runtime_sdk_returns_typed_token_count() {
        let (endpoint, server) =
            serve_once(br#"{"inputTokens":7}"#.to_vec(), "application/json").await;
        let connector = mock_connector(&endpoint).await;
        let ProviderOutput::Result(result) =
            connector.execute(token_count_request()).await.unwrap()
        else {
            panic!("expected typed token-count result");
        };
        let CanonicalResult::TokenCount(result) = *result else {
            panic!("expected canonical token-count result");
        };
        assert_eq!(result.input_tokens, 7);
        let request = server.await.unwrap().to_ascii_lowercase();
        assert!(request.starts_with("post /model/anthropic.claude-test-v1%3a0/count-tokens"));
        assert!(request.contains("authorization: aws4-hmac-sha256"));
    }

    #[tokio::test]
    async fn official_sdk_performs_no_hidden_retry() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let mut accepted = 0_u32;
            loop {
                let Ok(Ok((mut socket, _))) =
                    timeout(Duration::from_millis(150), listener.accept()).await
                else {
                    break;
                };
                accepted = accepted.saturating_add(1);
                let mut request = [0_u8; 8_192];
                let _ = socket.read(&mut request).await.unwrap();
                let body = br#"{"message":"temporarily unavailable"}"#;
                let response = format!(
                    "HTTP/1.1 503 Service Unavailable\r\ncontent-type: application/json\r\nx-amzn-errortype: ServiceUnavailableException\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                    body.len()
                );
                socket.write_all(response.as_bytes()).await.unwrap();
                socket.write_all(body).await.unwrap();
                socket.shutdown().await.unwrap();
            }
            accepted
        });
        let connector = mock_connector(&endpoint).await;
        let error = connector.execute(provider_request()).await.unwrap_err();
        assert_eq!(error.class, AttemptFailureClass::UpstreamServer);
        assert_eq!(server.await.unwrap(), 1);
    }

    #[tokio::test]
    async fn event_stream_idle_deadline_is_enforced_after_commit() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut request = [0_u8; 8_192];
            let _ = socket.read(&mut request).await.unwrap();
            let frame = event_frame("messageStart", r#"{"role":"assistant"}"#);
            let content_length = frame.len() + 1_000_000;
            socket
                .write_all(format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/vnd.amazon.eventstream\r\ncontent-length: {content_length}\r\nconnection: close\r\n\r\n"
                ).as_bytes())
                .await
                .unwrap();
            socket.write_all(&frame).await.unwrap();
            tokio::time::sleep(Duration::from_millis(250)).await;
        });
        let config = ConnectorConfig::new("us-east-1")
            .unwrap()
            .with_timeouts(ConnectorTimeouts {
                connect: Duration::from_secs(1),
                first_byte: Duration::from_secs(1),
                idle: Duration::from_millis(25),
            })
            .unwrap()
            .with_endpoint_url(&endpoint)
            .unwrap();
        let credentials = StaticCredentials::from_json(
            br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret"}"#,
        )
        .unwrap();
        let connector =
            BedrockConnector::new(config, BedrockCredentials::Static(credentials)).await;
        let ProviderOutput::Events(mut events) =
            connector.execute(streaming_request()).await.unwrap()
        else {
            panic!("expected event stream");
        };
        assert!(events.next().await.unwrap().is_ok());
        assert!(events.next().await.unwrap().is_ok());
        let error = events.next().await.unwrap().unwrap_err();
        assert_eq!(error.class, AttemptFailureClass::Timeout);
        assert!(error.response_committed);
        drop(events);
        server.await.unwrap();
    }

    #[tokio::test]
    #[ignore = "requires OLP_BEDROCK_LIVE_REGION and an AWS default credential chain"]
    async fn live_provider_discovers_models_with_default_chain() {
        let region = std::env::var("OLP_BEDROCK_LIVE_REGION")
            .expect("set OLP_BEDROCK_LIVE_REGION for the ignored live test");
        let connector = BedrockConnector::new(
            ConnectorConfig::new(region).unwrap(),
            BedrockCredentials::DefaultChain,
        )
        .await;
        assert!(!connector.discover_models().await.unwrap().is_empty());
    }

    #[tokio::test]
    #[ignore = "requires OLP_BEDROCK_LIVE_REGION, OLP_BEDROCK_LIVE_MODEL, and AWS credentials"]
    async fn live_provider_runs_converse_with_default_chain() {
        let region = std::env::var("OLP_BEDROCK_LIVE_REGION")
            .expect("set OLP_BEDROCK_LIVE_REGION for the ignored live test");
        let model = std::env::var("OLP_BEDROCK_LIVE_MODEL")
            .expect("set OLP_BEDROCK_LIVE_MODEL for the ignored live test");
        let connector = BedrockConnector::new(
            ConnectorConfig::new(region).unwrap(),
            BedrockCredentials::DefaultChain,
        )
        .await;
        let mut request = provider_request();
        request.attempt.provider_model = model;
        let ProviderOutput::Events(events) = connector.execute(request).await.unwrap() else {
            panic!("expected generation events");
        };
        let events = events.collect::<Vec<_>>().await;
        assert!(events.iter().all(Result::is_ok));
    }
}
