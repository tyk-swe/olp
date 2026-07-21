use olp_domain::{
    AttemptPlan, ImageOperation, Operation, Provider, ProviderKind, RouteSlug, RoutingError,
    RuntimeSnapshot, Surface, Target, TransportMode, VideoOperation, select_attempts_filtered,
};
use olp_protocols::openai;

use crate::gateway::InferenceError;

/// Removes targets that cannot encode this concrete request without semantic
/// loss. Capability tuples are the coarse model boundary; this request-level
/// check covers structured output, tools, source-scoped vendor fields, and
/// media forms before credentials or transport are used.
#[cfg(test)]
pub(crate) fn select_representable_attempts(
    snapshot: &RuntimeSnapshot,
    route_slug: &RouteSlug,
    operation: &Operation,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
) -> Result<Vec<AttemptPlan>, InferenceError> {
    select_representable_attempts_filtered(
        snapshot,
        route_slug,
        operation,
        surface,
        mode,
        affinity_key,
        |_, _| true,
    )
}

/// Applies runtime eligibility (circuit state or an async-job target pin)
/// together with semantic validation before deterministic ordering and the
/// route's maximum-attempt truncation.
pub(crate) fn select_representable_attempts_filtered(
    snapshot: &RuntimeSnapshot,
    route_slug: &RouteSlug,
    operation: &Operation,
    surface: Surface,
    mode: TransportMode,
    affinity_key: &[u8],
    mut eligible: impl FnMut(&Provider, &Target) -> bool,
) -> Result<Vec<AttemptPlan>, InferenceError> {
    let mut capability_matched = false;
    let mut representable_matched = false;
    let selected = select_attempts_filtered(
        snapshot,
        route_slug,
        operation.kind(),
        surface,
        mode,
        affinity_key,
        |provider, target| {
            capability_matched = true;
            let operation = operation_for_provider(operation, provider.kind);
            if validate_for_provider(&operation, provider.kind, &target.upstream_model).is_err() {
                return false;
            }
            representable_matched = true;
            eligible(provider, target)
        },
    );
    match selected {
        Ok(attempts) => Ok(attempts),
        Err(RoutingError::NoEligibleTargets { .. })
            if capability_matched && !representable_matched =>
        {
            Err(InferenceError::invalid_request(
                "No route target can represent this request without semantic loss.",
            ))
        }
        Err(RoutingError::NoEligibleTargets { .. }) if representable_matched => {
            Err(InferenceError::unavailable("no_eligible_provider"))
        }
        Err(error) => Err(InferenceError::not_found(error.to_string())),
    }
}

/// Removes delivery-only hints before a canonical request crosses into a
/// different provider protocol. These hints choose an adapter endpoint; they
/// are not client semantics and must neither block nor leak into another
/// protocol encoder.
pub(crate) fn operation_for_provider(
    operation: &Operation,
    provider_kind: ProviderKind,
) -> Operation {
    let mut operation = operation.clone();
    if !matches!(
        provider_kind,
        ProviderKind::OpenAi | ProviderKind::AzureOpenAi | ProviderKind::OpenAiCompatible
    ) && let Operation::Generation(request) = &mut operation
    {
        request.extensions.values.remove("/__olp/openai_endpoint");
    }
    operation
}

#[cfg(test)]
fn retain_representable_attempts(
    operation: &Operation,
    attempts: &mut Vec<AttemptPlan>,
) -> Result<(), InferenceError> {
    attempts.retain(|attempt| {
        validate_for_provider(operation, attempt.provider_kind, &attempt.upstream_model).is_ok()
    });
    if attempts.is_empty() {
        return Err(InferenceError::invalid_request(
            "No route target can represent this request without semantic loss.",
        ));
    }
    Ok(())
}

fn validate_for_provider(
    operation: &Operation,
    provider_kind: ProviderKind,
    upstream_model: &str,
) -> Result<(), String> {
    match provider_kind {
        ProviderKind::OpenAi | ProviderKind::AzureOpenAi | ProviderKind::OpenAiCompatible => {
            validate_openai(operation, upstream_model)
        }
        ProviderKind::Anthropic => validate_anthropic(operation, upstream_model),
        ProviderKind::Gemini | ProviderKind::VertexAi => validate_gemini(operation, upstream_model),
        ProviderKind::Bedrock => {
            olp_providers::validate_bedrock_operation(operation).map_err(|error| error.to_string())
        }
    }
}

fn validate_openai(operation: &Operation, upstream_model: &str) -> Result<(), String> {
    operation
        .extensions()
        .ok_or_else(|| "operation extensions are unavailable".to_owned())?
        .ensure_representable_on(Surface::OpenAi)
        .map_err(|error| error.to_string())?;
    match operation {
        Operation::Generation(request) => {
            let mut request = request.clone();
            let responses = request
                .extensions
                .values
                .remove("/__olp/openai_endpoint")
                .and_then(|value| value.as_str().map(str::to_owned))
                .is_some_and(|endpoint| endpoint == "responses");
            if responses {
                openai::encode_response_create(&request, upstream_model)
                    .map(|_| ())
                    .map_err(|error| error.to_string())
            } else {
                openai::encode_chat_completion(&request, upstream_model)
                    .map(|_| ())
                    .map_err(|error| error.to_string())
            }
        }
        Operation::Embeddings(request) => openai::encode_embedding_request(request, upstream_model)
            .map(|_| ())
            .map_err(|error| error.to_string()),
        Operation::TokenCount(request) => {
            openai::encode_response_input_tokens(request, upstream_model)
                .map(|_| ())
                .map_err(|error| error.to_string())
        }
        Operation::Images(ImageOperation::Generation(request)) => {
            openai::encode_image_generation(request, upstream_model)
                .map(|_| ())
                .map_err(|error| error.to_string())
        }
        Operation::Images(ImageOperation::Edit(request)) => {
            openai::encode_image_edit(request, upstream_model, |handle| {
                Ok(dummy_media_part(handle))
            })
            .map(|_| ())
            .map_err(|error| error.to_string())
        }
        Operation::Images(ImageOperation::Variation(request)) => {
            openai::encode_image_variation(request, upstream_model, |handle| {
                Ok(dummy_media_part(handle))
            })
            .map(|_| ())
            .map_err(|error| error.to_string())
        }
        Operation::Speech(request) => openai::encode_speech(request, upstream_model)
            .map(|_| ())
            .map_err(|error| error.to_string()),
        Operation::Transcription(request) => {
            openai::encode_transcription(request, upstream_model, |handle| {
                Ok(dummy_media_part(handle))
            })
            .map(|_| ())
            .map_err(|error| error.to_string())
        }
        Operation::Video(VideoOperation::Create(request)) => {
            openai::encode_video_create(request, upstream_model, |handle| {
                Ok(dummy_media_part(handle))
            })
            .map(|_| ())
            .map_err(|error| error.to_string())
        }
        Operation::Video(VideoOperation::List(request)) => openai::encode_video_list(request)
            .map(|_| ())
            .map_err(|error| error.to_string()),
        Operation::Video(
            VideoOperation::Get(_) | VideoOperation::Content(_) | VideoOperation::Delete(_),
        )
        | Operation::Models(_) => Ok(()),
        Operation::Moderation(request) => openai::encode_moderation(request, upstream_model)
            .map(|_| ())
            .map_err(|error| error.to_string()),
    }
}

fn validate_anthropic(operation: &Operation, upstream_model: &str) -> Result<(), String> {
    match operation {
        Operation::Generation(_) | Operation::TokenCount(_) => {
            olp_providers::validate_anthropic_operation(operation, upstream_model)
                .map_err(|error| error.to_string())
        }
        Operation::Models(_) => Ok(()),
        _ => Err("Anthropic does not represent this operation".to_owned()),
    }
}

fn validate_gemini(operation: &Operation, upstream_model: &str) -> Result<(), String> {
    match operation {
        Operation::Generation(_) | Operation::TokenCount(_) => {
            olp_providers::validate_gemini_operation(operation, upstream_model)
                .map_err(|error| error.to_string())
        }
        Operation::Models(_) => Ok(()),
        _ => Err("Gemini does not represent this operation".to_owned()),
    }
}

fn dummy_media_part(handle: &olp_domain::MediaHandle) -> openai::BoundedMediaPart {
    openai::BoundedMediaPart::new(handle.clone(), "bounded-media", None, 0, 1)
        .expect("fixed validation media metadata is valid")
}

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet},
        num::{NonZeroU16, NonZeroU32},
    };

    use chrono::Utc;
    use olp_domain::{
        Capability, ContentPart, DurationMs, GenerationParameters, GenerationRequest, MediaHandle,
        MediaSource, Message, MessageRole, Provider, ResponseFormat, Route, RouteId, RouteSlug,
        RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot, SourceExtensions, Target,
        TargetId, TokenCountRequest, ToolDefinition,
    };
    use serde_json::json;

    use super::*;

    fn generation(source: Surface) -> Operation {
        Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("public-route").unwrap(),
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: "metadata-free fixture".into(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                max_output_tokens: Some(32),
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: SourceExtensions::new(source, BTreeMap::new()),
        })
    }

    fn attempt(kind: ProviderKind) -> AttemptPlan {
        AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: olp_domain::ProviderId::new(),
            provider_kind: kind,
            upstream_model: "upstream-model".into(),
            timeout: DurationMs::new(1_000),
            priority: 0,
        }
    }

    #[test]
    fn filters_targets_that_would_drop_structured_output() {
        let mut operation = generation(Surface::OpenAi);
        let Operation::Generation(request) = &mut operation else {
            unreachable!()
        };
        request.response_format = Some(ResponseFormat::JsonSchema {
            name: "answer".into(),
            description: None,
            schema: json!({"type":"object"}),
            strict: Some(true),
        });
        let mut attempts = vec![
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::OpenAi),
        ];
        retain_representable_attempts(&operation, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_kind, ProviderKind::OpenAi);
    }

    #[test]
    fn rejects_reasoning_citation_safety_and_media_extensions_cross_protocol() {
        for path in [
            "/reasoning",
            "/citations",
            "/safetyRatings",
            "/messages/0/content/0/source",
        ] {
            let mut operation = generation(Surface::Anthropic);
            let Operation::Generation(request) = &mut operation else {
                unreachable!()
            };
            request
                .extensions
                .values
                .insert(path.into(), json!({"vendor":"semantic"}));
            let mut attempts = vec![attempt(ProviderKind::Gemini)];
            let error = retain_representable_attempts(&operation, &mut attempts).unwrap_err();
            assert_eq!(error.status(), axum::http::StatusCode::BAD_REQUEST);
        }
    }

    #[test]
    fn common_tool_semantics_remain_eligible_across_native_surfaces() {
        let mut operation = generation(Surface::OpenAi);
        let Operation::Generation(request) = &mut operation else {
            unreachable!()
        };
        request.tools.push(ToolDefinition {
            name: "lookup".into(),
            description: Some("lookup a value".into()),
            input_schema: json!({"type":"object","properties":{"id":{"type":"string"}}}),
        });
        let mut attempts = vec![
            attempt(ProviderKind::OpenAi),
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
        ];
        retain_representable_attempts(&operation, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 3);
    }

    #[test]
    fn token_count_uses_each_production_encoder_for_semantic_validation() {
        let operation = Operation::TokenCount(TokenCountRequest {
            route: RouteSlug::parse("public-route").unwrap(),
            input: vec![ContentPart::Text {
                text: "count this".into(),
            }],
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        });
        let mut attempts = vec![
            attempt(ProviderKind::OpenAi),
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
        ];
        retain_representable_attempts(&operation, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 3);

        let mut source_specific = operation;
        let Operation::TokenCount(request) = &mut source_specific else {
            unreachable!()
        };
        request
            .extensions
            .values
            .insert("/vendor_only".into(), json!(true));
        let mut attempts = vec![
            attempt(ProviderKind::OpenAi),
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
        ];
        retain_representable_attempts(&source_specific, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_kind, ProviderKind::OpenAi);
    }

    #[test]
    fn inline_media_is_eligible_only_on_its_exact_source_protocol() {
        let mut anthropic_image = generation(Surface::Anthropic);
        let Operation::Generation(request) = &mut anthropic_image else {
            unreachable!()
        };
        request.messages[0].content = vec![ContentPart::Image {
            source: MediaSource::Handle(MediaHandle::new("bounded-image")),
            detail: None,
        }];
        request.extensions.values.insert(
            "/messages/0/content/0/source/media_type".into(),
            json!("image/png"),
        );
        let mut attempts = vec![
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
            attempt(ProviderKind::OpenAi),
        ];
        retain_representable_attempts(&anthropic_image, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_kind, ProviderKind::Anthropic);

        let mut openai_audio = generation(Surface::OpenAi);
        let Operation::Generation(request) = &mut openai_audio else {
            unreachable!()
        };
        request.messages[0].content = vec![ContentPart::InputAudio {
            media: MediaHandle::new("bounded-audio"),
            format: "wav".into(),
        }];
        let mut attempts = vec![
            attempt(ProviderKind::OpenAi),
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
        ];
        retain_representable_attempts(&openai_audio, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_kind, ProviderKind::OpenAi);

        let mut openai_file = generation(Surface::OpenAi);
        let Operation::Generation(request) = &mut openai_file else {
            unreachable!()
        };
        request.messages[0].content = vec![ContentPart::InputFile {
            media: MediaHandle::new("bounded-file"),
            mime_type: "application/pdf".into(),
            filename: "brief.pdf".into(),
        }];
        request
            .extensions
            .values
            .insert("/__olp/openai_endpoint".into(), json!("responses"));
        let mut attempts = vec![
            attempt(ProviderKind::OpenAi),
            attempt(ProviderKind::Anthropic),
            attempt(ProviderKind::Gemini),
        ];
        retain_representable_attempts(&openai_file, &mut attempts).unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_kind, ProviderKind::OpenAi);
    }

    #[test]
    fn semantic_filter_runs_before_route_attempt_limit() {
        let mut operation = generation(Surface::OpenAi);
        let Operation::Generation(request) = &mut operation else {
            unreachable!()
        };
        request.response_format = Some(ResponseFormat::JsonObject);
        let route_slug = request.route.clone();
        let incompatible = olp_domain::ProviderId::new();
        let compatible = olp_domain::ProviderId::new();
        let capability = |model: &str| {
            BTreeSet::from([Capability::new(
                model,
                olp_domain::OperationKind::Generation,
                Surface::OpenAi,
                TransportMode::Unary,
            )])
        };
        let route = Route {
            id: RouteId::new(),
            routing_id: None,
            slug: route_slug.clone(),
            operations: BTreeSet::from([olp_domain::OperationKind::Generation]),
            overall_timeout: DurationMs::new(2_000),
            max_attempts: NonZeroU16::new(1).unwrap(),
            targets: vec![
                Target {
                    id: TargetId::new(),
                    routing_id: None,
                    provider_id: incompatible,
                    upstream_model: "claude".into(),
                    priority: 0,
                    weight: NonZeroU32::new(1).unwrap(),
                    timeout: DurationMs::new(1_000),
                },
                Target {
                    id: TargetId::new(),
                    routing_id: None,
                    provider_id: compatible,
                    upstream_model: "gpt".into(),
                    priority: 1,
                    weight: NonZeroU32::new(1).unwrap(),
                    timeout: DurationMs::new(1_000),
                },
            ],
        };
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 1,
                activated_at: Utc::now(),
            },
            providers: BTreeMap::from([
                (
                    incompatible,
                    Provider {
                        id: incompatible,
                        name: "incompatible".into(),
                        kind: ProviderKind::Anthropic,
                        enabled: true,
                        active_credential: None,
                        capabilities: capability("claude"),
                    },
                ),
                (
                    compatible,
                    Provider {
                        id: compatible,
                        name: "compatible".into(),
                        kind: ProviderKind::OpenAi,
                        enabled: true,
                        active_credential: None,
                        capabilities: capability("gpt"),
                    },
                ),
            ]),
            routes: BTreeMap::from([(route_slug.clone(), route)]),
            api_keys: BTreeMap::new(),
        };
        let attempts = select_representable_attempts(
            &snapshot,
            &route_slug,
            &operation,
            Surface::OpenAi,
            TransportMode::Unary,
            b"affinity",
        )
        .unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].provider_id, compatible);
    }
}
