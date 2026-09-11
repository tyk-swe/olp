//! Explicit contracts for the additional compatible vendors; model-specific facts
//! remain unknown until supplied by a reviewed catalog or the operator.
use crate::protocols::canonical::{
    identity::OperationKind,
    requests::{OPENAI_ENDPOINT_EXTENSION, Operation},
};
use crate::providers::catalog::{COHERE_UNSUPPORTED_PARAMETERS, chat_completions_only};
use crate::providers::options::ConnectionOptions;
use std::borrow::Cow;

pub(crate) fn operation<'a>(
    operation: &'a Operation,
    options: &ConnectionOptions,
    kind: crate::providers::runtime_model::ProviderKind,
) -> Cow<'a, Operation> {
    let mut result = Cow::Borrowed(operation);
    if chat_completions_only(options.vendor_id.as_deref())
        && crate::inference::selection::carries_endpoint_hint(operation)
    {
        crate::inference::selection::strip_endpoint_hint(result.to_mut());
    }
    if kind == crate::providers::runtime_model::ProviderKind::Anthropic
        && let Operation::Generation(request) = operation
        && request.parameters.max_output_tokens.is_none()
        && let Some(limit) = options
            .parameter_defaults
            .get("max_tokens")
            .and_then(serde_json::Value::as_u64)
            .and_then(|value| u32::try_from(value).ok())
        && let Operation::Generation(request) = result.to_mut()
    {
        request.parameters.max_output_tokens = Some(limit);
    }
    if options.vendor_id.as_deref() == Some("cohere")
        && let Operation::Generation(request) = operation
        && implicit_anthropic_candidate_count(request)
        && let Operation::Generation(request) = result.to_mut()
    {
        request.parameters.candidate_count = None;
    }
    result
}

fn implicit_anthropic_candidate_count(
    request: &crate::protocols::canonical::requests::GenerationRequest,
) -> bool {
    request.extensions.source == Some(crate::protocols::canonical::identity::Surface::Anthropic)
        && request.parameters.candidate_count == Some(1)
}

pub(crate) fn validate(operation: &Operation, options: &ConnectionOptions) -> Result<(), String> {
    let vendor = options.vendor_id.as_deref();
    if chat_completions_only(vendor)
        && let Operation::Generation(request) = operation
        && request
            .extensions
            .values
            .get(OPENAI_ENDPOINT_EXTENSION)
            .and_then(serde_json::Value::as_str)
            == Some("responses")
        && request
            .extensions
            .values
            .keys()
            .any(|path| !crate::protocols::canonical::requests::is_delivery_only_extension(path))
    {
        return Err("This vendor cannot preserve the supplied Responses-specific parameters through Chat Completions".into());
    }

    let supported = match vendor {
        Some("voyage") => operation.kind() == OperationKind::Embeddings,
        Some("cohere") => matches!(
            operation.kind(),
            OperationKind::Generation | OperationKind::Embeddings
        ),
        Some(_) if chat_completions_only(vendor) => operation.kind() == OperationKind::Generation,
        _ => true,
    };
    if !supported {
        return Err("Operation is outside the configured vendor contract".into());
    }
    if vendor == Some("cohere") {
        if let Operation::Generation(request) = operation
            && ((request.parameters.candidate_count.is_some()
                && !implicit_anthropic_candidate_count(request))
                || request.parameters.parallel_tool_calls.is_some())
        {
            return Err("Cohere does not support n or parallel_tool_calls".into());
        }
        if let Operation::Embeddings(request) = operation
            && request.dimensions.is_some()
        {
            return Err("Cohere's compatible endpoint does not support dimensions".into());
        }
        if operation.extensions().is_some_and(|e| {
            e.values.keys().any(|path| {
                path.strip_prefix('/')
                    .is_some_and(|name| COHERE_UNSUPPORTED_PARAMETERS.contains(&name))
            })
        }) {
            return Err("Cohere cannot represent a supplied parameter".into());
        }
    }
    if vendor == Some("voyage")
        && let Operation::Embeddings(request) = operation
        && request.input.iter().any(|input| {
            !matches!(
                input,
                crate::protocols::canonical::requests::EmbeddingInput::Text(_)
            )
        })
    {
        return Err("Voyage embeddings require text input".into());
    }
    Ok(())
}
