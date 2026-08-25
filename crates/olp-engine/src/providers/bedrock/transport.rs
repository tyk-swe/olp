use std::{
    collections::{HashMap, VecDeque},
    time::Duration,
};

use crate::domain::{
    canonical::{
        events::{Event, Kind},
        identity::TransportMode,
        requests::{MessageRole, Operation},
        results::{CanonicalResult, TokenCountResult},
    },
    ports::{
        AttemptFailureClass, DiscoveredProviderModel, ProviderEventStream, ProviderOutput,
        ProviderRequest, ProviderTransport, TransportError, TransportPhase, UpstreamSignal,
    },
    routing::provider::ProviderKind,
};
use aws_sdk_bedrock::types::ModelModality;
use aws_sdk_bedrockruntime::{
    error::{ProvideErrorMetadata, SdkError},
    operation::converse_stream::ConverseStreamOutput as ConverseStreamResponse,
    types::{ContentBlockDelta, ContentBlockStart, ConverseStreamOutput, CountTokensInput},
};
use aws_smithy_runtime_api::client::orchestrator::HttpResponse;
use aws_smithy_types::event_stream::RawMessage;
use futures::stream;
use tokio::time::{Instant, timeout};

use crate::providers::bedrock::{
    ConnectorConfig, Credentials, sdk_config,
    translate::{
        decode_converse, decode_stop_reason, decode_usage, encode_generation, encode_token_count,
        protocol_body_error, protocol_error,
    },
};

pub(in crate::providers) struct Connector {
    runtime: aws_sdk_bedrockruntime::Client,
    control: aws_sdk_bedrock::Client,
    timeouts: crate::providers::connector::Timeouts,
}

impl Connector {
    pub(in crate::providers) async fn new(
        config: ConnectorConfig,
        credentials: Credentials,
    ) -> Self {
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
    pub(in crate::providers) async fn discover_models(
        &self,
    ) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
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
        validate_model_id(&request.attempt.upstream_model)?;
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
                            .model_id(&request.attempt.upstream_model)
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
                        request.attempt.upstream_model.clone(),
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
                            .model_id(&request.attempt.upstream_model)
                            .set_messages(Some(encoded.messages))
                            .set_system((!encoded.system.is_empty()).then_some(encoded.system))
                            .inference_config(encoded.inference_config)
                            .set_tool_config(encoded.tool_config)
                            .send(),
                    )
                    .await
                    .map_err(|_| deadline_error(TransportPhase::Body, false))?
                    .map_err(|error| map_sdk_error(&error, TransportPhase::Body, false))?;
                    let events = decode_converse(response, &request.attempt.upstream_model)
                        .map_err(mark_uncommitted)?;
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
                        .model_id(&request.attempt.upstream_model)
                        .input(CountTokensInput::Converse(input))
                        .send(),
                )
                .await
                .map_err(|_| deadline_error(TransportPhase::Body, false))?
                .map_err(|error| map_sdk_error(&error, TransportPhase::Body, false))?;
                let input_tokens = u64::try_from(response.input_tokens())
                    .map_err(|_| {
                        protocol_body_error("Bedrock returned a negative input token count")
                    })
                    .map_err(mark_uncommitted)?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens,
                        extensions: crate::domain::canonical::requests::SourceExtensions::default(),
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

impl ProviderTransport for Connector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> crate::domain::ports::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(self.execute_request(request))
    }
}

struct StreamState {
    response: ConverseStreamResponse,
    pending: VecDeque<Result<Event, TransportError>>,
    sequence: u64,
    attempt_deadline: Instant,
    idle_timeout: Duration,
    saw_message_start: bool,
    saw_message_stop: bool,
    saw_metadata: bool,
    next_tool_index: u32,
    tool_indices: HashMap<u32, u32>,
    terminal: bool,

    open_content_blocks: std::collections::HashSet<u32>,
    stopped_content_blocks: std::collections::HashSet<u32>,
    invalid_event_order: bool,
}

fn stream_events(
    response: ConverseStreamResponse,
    upstream_model: String,
    attempt_deadline: Instant,
    idle_timeout: Duration,
) -> ProviderEventStream {
    let pending = VecDeque::from([Ok(Event::new(
        0,
        Kind::ResponseStart {
            response_id: None,
            provider_model: Some(upstream_model),
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
            open_content_blocks: std::collections::HashSet::new(),
            stopped_content_blocks: std::collections::HashSet::new(),
            invalid_event_order: false,
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
                        if state.invalid_event_order
                            || !state.saw_message_stop
                            || !state.saw_metadata
                        {
                            return Some((
                                Err(protocol_body_error(
                                    "Bedrock stream ended before message_stop",
                                )),
                                state,
                            ));
                        }
                        let done = Event::new(state.sequence, Kind::Done);
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
                                .push_back(Ok(Event::new(state.sequence, kind)));
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
) -> Result<Vec<Kind>, TransportError> {
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
            Ok(vec![Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            }])
        }
        ConverseStreamOutput::ContentBlockStart(start) => {
            require_content_phase(state)?;
            let bedrock_index = content_block_index(start.content_block_index)?;
            // OLP ContentBlockStart lifecycle validation
            if state.stopped_content_blocks.contains(&bedrock_index)
                || !state.open_content_blocks.insert(bedrock_index)
            {
                state.invalid_event_order = true;
            }
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
                    Ok(vec![Kind::ToolCallDelta {
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
            // OLP ContentBlockDelta lifecycle validation
            if state.stopped_content_blocks.contains(&bedrock_index) {
                state.invalid_event_order = true;
            } else {
                // Text blocks are permitted to begin with a delta.
                state.open_content_blocks.insert(bedrock_index);
            }
            match delta.delta {
                Some(ContentBlockDelta::Text(text)) => Ok(vec![Kind::TextDelta {
                    output_index: 0,
                    text,
                }]),
                Some(ContentBlockDelta::ToolUse(tool)) => Ok(vec![Kind::ToolCallDelta {
                    output_index: 0,
                    tool_index: *state.tool_indices.get(&bedrock_index).ok_or_else(|| {
                        protocol_body_error(
                            "Bedrock stream returned a tool delta before its block start",
                        )
                    })?,
                    id: None,
                    name: None,
                    arguments_delta: tool.input,
                }]),
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
            let bedrock_index = content_block_index(stop.content_block_index)?;
            // OLP ContentBlockStop lifecycle validation
            if !state.open_content_blocks.remove(&bedrock_index) {
                state.invalid_event_order = true;
            }
            if !state.stopped_content_blocks.insert(bedrock_index) {
                state.invalid_event_order = true;
            }
            Ok(Vec::new())
        }
        ConverseStreamOutput::MessageStop(stop) => {
            // OLP MessageStop lifecycle validation
            if !state.open_content_blocks.is_empty() {
                state.invalid_event_order = true;
            }
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
            Ok(vec![Kind::Finish {
                output_index: 0,
                reason: decode_stop_reason(&stop.stop_reason),
            }])
        }
        ConverseStreamOutput::Metadata(metadata) => {
            // OLP Metadata lifecycle validation
            if !state.saw_message_stop {
                state.invalid_event_order = true;
            }
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
                        .map(|usage| vec![Kind::Usage { usage }])
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
        upstream: Default::default(),
        phase,
        class: AttemptFailureClass::Timeout,
        response_committed: committed,
        message: "Bedrock request deadline exceeded".to_owned(),
    }
}

fn mark_uncommitted(mut error: TransportError) -> TransportError {
    error.response_committed = false;
    error
}

trait BedrockSdkRawResponse {
    fn is_successful_response(&self) -> bool {
        false
    }

    /// Bedrock's event-stream frames carry no HTTP envelope, so the default is
    /// "nothing observed" and the service code supplies the status instead.
    fn upstream_signal(&self) -> UpstreamSignal {
        UpstreamSignal::default()
    }
}

impl BedrockSdkRawResponse for HttpResponse {
    fn is_successful_response(&self) -> bool {
        self.status().is_success()
    }

    fn upstream_signal(&self) -> UpstreamSignal {
        UpstreamSignal::from_status(self.status().as_u16()).with_retry_after(
            self.headers()
                .get("retry-after")
                .and_then(|value| value.trim().parse::<u64>().ok())
                .map(std::time::Duration::from_secs),
        )
    }
}

impl BedrockSdkRawResponse for RawMessage {}

fn map_sdk_error<E, R>(
    error: &SdkError<E, R>,
    phase: TransportPhase,
    committed: bool,
) -> TransportError
where
    E: ProvideErrorMetadata,
    R: BedrockSdkRawResponse,
{
    let class = match error {
        SdkError::TimeoutError(_) => AttemptFailureClass::Timeout,
        SdkError::DispatchFailure(failure) if failure.is_timeout() => AttemptFailureClass::Timeout,
        SdkError::DispatchFailure(failure) if failure.is_user() => AttemptFailureClass::Protocol,
        SdkError::DispatchFailure(_) => AttemptFailureClass::Connect,
        SdkError::ConstructionFailure(_) | SdkError::ResponseError(_) => {
            AttemptFailureClass::Protocol
        }
        SdkError::ServiceError(service) if service.raw().is_successful_response() => {
            AttemptFailureClass::Protocol
        }
        SdkError::ServiceError(service) => classify_service_code(service.err().code()),
        _ => AttemptFailureClass::UpstreamServer,
    };
    let upstream = match error {
        SdkError::ServiceError(service) => {
            let observed = service.raw().upstream_signal();
            UpstreamSignal {
                status: observed
                    .status
                    .filter(|status| *status >= 400)
                    .or_else(|| service_code_status(service.err().code())),
                retry_after: observed.retry_after,
            }
        }
        _ => UpstreamSignal::default(),
    };
    TransportError {
        upstream,
        phase,
        class,
        response_committed: committed,
        message: "Bedrock SDK request failed".to_owned(),
    }
}

/// Bedrock reports client faults as modeled exception codes rather than status
/// codes, so a code the SDK surfaced without an HTTP envelope still maps to the
/// public status the caller should see.
fn service_code_status(code: Option<&str>) -> Option<u16> {
    match code? {
        "AccessDeniedException" => Some(403),
        "UnrecognizedClientException" | "InvalidSignatureException" | "ExpiredTokenException" => {
            Some(401)
        }
        "ValidationException" => Some(400),
        "ResourceNotFoundException" => Some(404),
        "ConflictException" => Some(409),
        _ => None,
    }
}

fn classify_service_code(code: Option<&str>) -> AttemptFailureClass {
    match code {
        Some("ThrottlingException" | "ServiceQuotaExceededException") => {
            AttemptFailureClass::RateLimit
        }
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
mod tests;
