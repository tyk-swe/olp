use std::collections::BTreeMap;

use axum::{
    Json, Router,
    extract::{State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::post,
};
use olp_domain::{
    CanonicalEventKind, ContentPart, FinishReason, GenerationParameters, GenerationRequest,
    Message, MessageRole, Operation, ResponseFormat, RouteSlug, SourceExtensions, Surface,
    ToolCall, ToolDefinition, Usage,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use utoipa::{OpenApi, ToSchema};
use uuid::Uuid;

use crate::{
    FieldErrors, ManagementState, Problem,
    gateway::{InferenceError, execute_session_generation},
    management_api::{Permission, json_payload, require_mutation_session, require_permission},
};

pub(crate) fn router() -> Router<ManagementState> {
    Router::new().route("/api/v1/playground", post(execute_playground))
}

#[derive(OpenApi)]
#[openapi(
    paths(execute_playground),
    components(schemas(
        PlaygroundRequest,
        PlaygroundToolRequest,
        PlaygroundResponseFormat,
        PlaygroundResponse,
        PlaygroundToolCall,
        PlaygroundUsage,
        Problem
    )),
    tags((name = "playground"))
)]
pub(crate) struct PlaygroundApiDoc;

#[derive(Deserialize, ToSchema)]
struct PlaygroundRequest {
    model: String,
    input: String,
    #[serde(default = "default_surface")]
    surface: String,
    #[serde(default)]
    tools: Vec<PlaygroundToolRequest>,
    response_format: Option<PlaygroundResponseFormat>,
    temperature: Option<f32>,
    max_output_tokens: Option<u32>,
}

fn default_surface() -> String {
    "openai".to_owned()
}

#[derive(Deserialize, ToSchema)]
struct PlaygroundToolRequest {
    name: String,
    description: Option<String>,
    input_schema: Value,
}

#[derive(Clone, Deserialize, ToSchema)]
#[serde(tag = "type", rename_all = "snake_case")]
enum PlaygroundResponseFormat {
    Text,
    JsonObject,
    JsonSchema {
        name: String,
        description: Option<String>,
        schema: Value,
        strict: Option<bool>,
    },
}

impl From<PlaygroundResponseFormat> for ResponseFormat {
    fn from(value: PlaygroundResponseFormat) -> Self {
        match value {
            PlaygroundResponseFormat::Text => Self::Text,
            PlaygroundResponseFormat::JsonObject => Self::JsonObject,
            PlaygroundResponseFormat::JsonSchema {
                name,
                description,
                schema,
                strict,
            } => Self::JsonSchema {
                name,
                description,
                schema,
                strict,
            },
        }
    }
}

#[derive(Serialize, ToSchema)]
struct PlaygroundResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    model: String,
    provider_model: Option<String>,
    output_text: String,
    refusal: Option<String>,
    tool_calls: Vec<PlaygroundToolCall>,
    structured_output: Option<Value>,
    usage: Option<PlaygroundUsage>,
    finish_reason: Option<String>,
    latency_ms: u64,
}

#[derive(Serialize, ToSchema)]
struct PlaygroundToolCall {
    id: String,
    name: String,
    arguments: String,
}

impl From<ToolCall> for PlaygroundToolCall {
    fn from(value: ToolCall) -> Self {
        Self {
            id: value.id,
            name: value.name,
            arguments: value.arguments,
        }
    }
}

#[derive(Serialize, ToSchema)]
struct PlaygroundUsage {
    input_tokens: u64,
    output_tokens: u64,
    total_tokens: u64,
    cached_input_tokens: Option<u64>,
    reasoning_tokens: Option<u64>,
}

impl From<Usage> for PlaygroundUsage {
    fn from(value: Usage) -> Self {
        Self {
            input_tokens: value.input_tokens,
            output_tokens: value.output_tokens,
            total_tokens: value.total_tokens,
            cached_input_tokens: value.cached_input_tokens,
            reasoning_tokens: value.reasoning_tokens,
        }
    }
}

#[derive(Default)]
struct ToolCallBuilder {
    id: Option<String>,
    name: Option<String>,
    arguments: String,
}

#[utoipa::path(
    post,
    path = "/api/v1/playground",
    tag = "playground",
    request_body = PlaygroundRequest,
    responses(
        (status = 200, description = "Ephemeral session-authorized generation result", body = PlaygroundResponse),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "CSRF, origin, or role check failed", body = Problem),
        (status = 422, description = "Request or route is invalid", body = Problem),
        (status = 502, description = "Provider response is invalid", body = Problem),
        (status = 503, description = "No healthy eligible target", body = Problem)
    )
)]
async fn execute_playground(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<PlaygroundRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::UsePlayground)?;
    let request = json_payload(payload)?;
    let (operation, surface, structured) = playground_operation(request)?;
    let execution = execute_session_generation(&state, operation, surface)
        .await
        .map_err(InferenceError::into_problem)?;

    let mut output_text = String::new();
    let mut refusal = String::new();
    let mut tool_calls = BTreeMap::<(u32, u32), ToolCallBuilder>::new();
    let mut provider_model = None;
    let mut usage = None;
    let mut finish_reason = None;
    for event in execution.events {
        match event.kind {
            CanonicalEventKind::ResponseStart {
                provider_model: model,
                ..
            } => provider_model = model,
            CanonicalEventKind::TextDelta {
                output_index: 0,
                text,
            } => {
                output_text.push_str(&text);
            }
            CanonicalEventKind::RefusalDelta {
                output_index: 0,
                text,
            } => {
                refusal.push_str(&text);
            }
            CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index,
                id,
                name,
                arguments_delta,
            } if output_index == 0 => {
                let call = tool_calls.entry((output_index, tool_index)).or_default();
                if id.is_some() {
                    call.id = id;
                }
                if name.is_some() {
                    call.name = name;
                }
                call.arguments.push_str(&arguments_delta);
            }
            CanonicalEventKind::Usage { usage: value } => usage = Some(value.into()),
            CanonicalEventKind::Finish {
                output_index: 0,
                reason,
            } => finish_reason = Some(finish_reason_name(reason)),
            _ => {}
        }
    }
    let tool_calls = tool_calls
        .into_values()
        .map(|call| {
            Ok(PlaygroundToolCall::from(ToolCall {
                id: call.id.ok_or_else(|| {
                    provider_problem("The provider returned a tool call without an ID.")
                })?,
                name: call.name.ok_or_else(|| {
                    provider_problem("The provider returned a tool call without a name.")
                })?,
                arguments: call.arguments,
            }))
        })
        .collect::<Result<Vec<_>, Problem>>()?;
    let structured_output =
        if structured && !output_text.is_empty() {
            Some(serde_json::from_str(&output_text).map_err(|_| {
                provider_problem("The provider did not return valid structured JSON.")
            })?)
        } else {
            None
        };
    let mut response = Json(PlaygroundResponse {
        id: execution.request_id.as_uuid(),
        model: execution.route_slug.to_string(),
        provider_model,
        output_text,
        refusal: (!refusal.is_empty()).then_some(refusal),
        tool_calls,
        structured_output,
        usage,
        finish_reason,
        latency_ms: execution.latency_ms,
    })
    .into_response();
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}

fn playground_operation(request: PlaygroundRequest) -> Result<(Operation, Surface, bool), Problem> {
    let mut fields = FieldErrors::new();
    let route = RouteSlug::parse(request.model.trim().to_owned()).map_err(|error| {
        fields.insert("model".to_owned(), vec![error.to_string()]);
        Problem::validation(fields.clone())
    })?;
    if request.input.trim().is_empty() || request.input.len() > 128 * 1024 {
        fields.insert(
            "input".to_owned(),
            vec!["Input must contain 1-131072 bytes.".to_owned()],
        );
    }
    if request.tools.len() > 64 {
        fields.insert(
            "tools".to_owned(),
            vec!["At most 64 tools may be tested at once.".to_owned()],
        );
    }
    if request
        .temperature
        .is_some_and(|value| !value.is_finite() || !(0.0..=2.0).contains(&value))
    {
        fields.insert(
            "temperature".to_owned(),
            vec!["Temperature must be from 0 through 2.".to_owned()],
        );
    }
    if request
        .max_output_tokens
        .is_some_and(|value| value == 0 || value > 1_000_000)
    {
        fields.insert(
            "max_output_tokens".to_owned(),
            vec!["Maximum output tokens must be from 1 through 1000000.".to_owned()],
        );
    }
    for (index, tool) in request.tools.iter().enumerate() {
        if tool.name.is_empty()
            || tool.name.len() > 128
            || !tool
                .name
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
            || !tool.input_schema.is_object()
        {
            fields.insert(
                format!("tools.{index}"),
                vec!["Tool names and object input schemas are required.".to_owned()],
            );
        }
    }
    let surface = match request.surface.parse::<Surface>() {
        Ok(surface) => surface,
        Err(_) => {
            fields.insert(
                "surface".to_owned(),
                vec!["Surface must be openai, anthropic, or gemini.".to_owned()],
            );
            // A placeholder used only until the aggregated field error returns.
            Surface::OpenAi
        }
    };
    if !fields.is_empty() {
        return Err(Problem::validation(fields));
    }
    let structured = matches!(
        request.response_format.as_ref(),
        Some(PlaygroundResponseFormat::JsonObject | PlaygroundResponseFormat::JsonSchema { .. })
    );
    let response_format = request.response_format.map(Into::into);
    let tools = request
        .tools
        .into_iter()
        .map(|tool| ToolDefinition {
            name: tool.name,
            description: tool.description,
            input_schema: tool.input_schema,
        })
        .collect();
    Ok((
        Operation::Generation(GenerationRequest {
            route,
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: request.input,
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                max_output_tokens: request.max_output_tokens,
                temperature: request.temperature,
                ..GenerationParameters::default()
            },
            tools,
            tool_choice: None,
            response_format,
            extensions: SourceExtensions::new(surface, BTreeMap::new()),
        }),
        surface,
        structured,
    ))
}

fn finish_reason_name(reason: FinishReason) -> String {
    match reason {
        FinishReason::Stop => "stop".to_owned(),
        FinishReason::Length => "length".to_owned(),
        FinishReason::ToolCalls => "tool_calls".to_owned(),
        FinishReason::ContentFilter => "content_filter".to_owned(),
        FinishReason::Error => "error".to_owned(),
        FinishReason::Other(value) => value,
    }
}

fn provider_problem(detail: &str) -> Problem {
    Problem::new(
        StatusCode::BAD_GATEWAY,
        "provider_protocol_error",
        "Provider protocol error",
        detail,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request() -> PlaygroundRequest {
        PlaygroundRequest {
            model: "default".to_owned(),
            input: "Return a small JSON object.".to_owned(),
            surface: "anthropic".to_owned(),
            tools: vec![PlaygroundToolRequest {
                name: "lookup_weather".to_owned(),
                description: Some("Look up weather".to_owned()),
                input_schema: serde_json::json!({"type": "object"}),
            }],
            response_format: Some(PlaygroundResponseFormat::JsonObject),
            temperature: Some(0.2),
            max_output_tokens: Some(100),
        }
    }

    #[test]
    fn builds_a_content_bearing_operation_only_in_memory() {
        let (operation, surface, structured) = playground_operation(request()).unwrap();
        assert_eq!(surface, Surface::Anthropic);
        assert!(structured);
        let Operation::Generation(generation) = operation else {
            panic!("playground must build generation")
        };
        assert_eq!(generation.route.as_str(), "default");
        assert_eq!(generation.tools[0].name, "lookup_weather");
        assert!(matches!(
            generation.response_format,
            Some(ResponseFormat::JsonObject)
        ));
    }

    #[test]
    fn rejects_invalid_surface_tool_schema_and_limits_together() {
        let mut request = request();
        request.surface = "unknown".to_owned();
        request.input.clear();
        request.tools[0].input_schema = Value::String("not-a-schema".to_owned());
        request.temperature = Some(f32::NAN);
        let problem = playground_operation(request).unwrap_err();
        assert_eq!(problem.status, StatusCode::UNPROCESSABLE_ENTITY.as_u16());
        assert!(problem.errors.contains_key("input"));
        assert!(problem.errors.contains_key("tools.0"));
        assert!(problem.errors.contains_key("temperature"));
        assert!(problem.errors.contains_key("surface"));
    }
}
