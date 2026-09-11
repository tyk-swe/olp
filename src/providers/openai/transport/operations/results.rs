use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::openai::embeddings::EmbeddingResponse;
use crate::protocols::openai::embeddings::decode_embedding_response;
use crate::protocols::openai::embeddings::encode_embedding_request;
use crate::protocols::openai::moderation::Response;
use crate::protocols::openai::moderation::decode_response;
use crate::protocols::openai::moderation::encode;
use crate::protocols::openai::responses::token_count::ResponseInputTokensResponse;
use crate::protocols::openai::responses::token_count::decode_response_input_tokens_result;
use crate::protocols::openai::responses::token_count::encode_response_input_tokens;

use crate::providers::openai::transport::Connector;
use crate::providers::openai::transport::errors::*;
use crate::providers::openai::transport::media::hydrate_responses_media;
use crate::providers::transport_common::transport_error;

pub(crate) async fn execute(
    connector: &Connector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let (path, body, result_kind) = match &*request.operation {
        Operation::Embeddings(operation) => {
            let wire = encode_embedding_request(operation, &request.attempt.upstream_model)
                .map_err(|error| protocol_encode_error("embeddings", error))?;
            (
                "embeddings",
                serialize_wire("embeddings", &wire)?,
                ResultKind::Embeddings,
            )
        }
        Operation::TokenCount(operation) => {
            let mut wire = encode_response_input_tokens(operation, &request.attempt.upstream_model)
                .map_err(|error| protocol_encode_error("input-token count", error))?;
            hydrate_responses_media(
                &mut wire.input,
                request.media.as_ref(),
                request.max_inline_media_bytes,
            )
            .await?;
            (
                "responses/input_tokens",
                serialize_wire("input-token count", &wire)?,
                ResultKind::TokenCount,
            )
        }
        Operation::Moderation(operation) => {
            let wire = encode(operation, &request.attempt.upstream_model)
                .map_err(|error| protocol_encode_error("moderation", error))?;
            (
                "moderations",
                serialize_wire("moderation", &wire)?,
                ResultKind::Moderation,
            )
        }
        operation => {
            return Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                format!(
                    "OpenAI connector does not yet transport {:?}",
                    operation.kind()
                ),
            ));
        }
    };
    let response = connector.post_unary_json(&request, path, body).await?;
    let result = match result_kind {
        ResultKind::Embeddings => {
            let response = if connector.options.vendor_id.as_deref() == Some("voyage") {
                let mut value: serde_json::Value = parse_wire("embeddings", &response)?;
                if let Some(usage) = value
                    .get_mut("usage")
                    .and_then(serde_json::Value::as_object_mut)
                    && let Some(tokens) = usage.get("total_tokens").cloned()
                {
                    usage.insert("prompt_tokens".into(), tokens);
                }
                serde_json::to_vec(&value)
                    .map_err(|error| protocol_encode_error("embeddings", error))?
            } else {
                response
            };
            let wire: EmbeddingResponse = parse_wire("embeddings", &response)?;
            CanonicalResult::Embeddings(
                decode_embedding_response(wire)
                    .map_err(|error| protocol_decode_error("embeddings", error))?,
            )
        }
        ResultKind::TokenCount => {
            let wire: ResponseInputTokensResponse = parse_wire("input-token count", &response)?;
            CanonicalResult::TokenCount(decode_response_input_tokens_result(wire))
        }
        ResultKind::Moderation => {
            let wire: Response = parse_wire("moderation", &response)?;
            CanonicalResult::Moderation(decode_response(wire))
        }
    };
    Ok(ProviderOutput::Result(Box::new(result)))
}

enum ResultKind {
    Embeddings,
    TokenCount,
    Moderation,
}
