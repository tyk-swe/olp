use olp_domain::{
    AttemptFailureClass, CanonicalResult, Operation, ProviderOutput, ProviderRequest,
    TransportError, TransportPhase,
};
use olp_protocols::openai::{
    EmbeddingResponse, OpenAiModerationResponse, ResponseInputTokensResponse,
    decode_embedding_response, decode_moderation_response, decode_response_input_tokens_result,
    encode_embedding_request, encode_moderation, encode_response_input_tokens,
};

use super::super::{OpenAiConnector, errors::*, media::hydrate_responses_media};

pub(super) async fn execute(
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let (path, body, result_kind) = match &request.operation {
        Operation::Embeddings(operation) => {
            let wire = encode_embedding_request(operation, &request.attempt.provider_model)
                .map_err(|error| protocol_encode_error("embeddings", error))?;
            (
                "embeddings",
                serialize_wire("embeddings", &wire)?,
                ResultKind::Embeddings,
            )
        }
        Operation::TokenCount(operation) => {
            let mut wire = encode_response_input_tokens(operation, &request.attempt.provider_model)
                .map_err(|error| protocol_encode_error("input-token count", error))?;
            hydrate_responses_media(&mut wire.input, request.media.as_ref()).await?;
            (
                "responses/input_tokens",
                serialize_wire("input-token count", &wire)?,
                ResultKind::TokenCount,
            )
        }
        Operation::Moderation(operation) => {
            let wire = encode_moderation(operation, &request.attempt.provider_model)
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
            let wire: OpenAiModerationResponse = parse_wire("moderation", &response)?;
            CanonicalResult::Moderation(decode_moderation_response(wire))
        }
    };
    Ok(ProviderOutput::Result(Box::new(result)))
}

enum ResultKind {
    Embeddings,
    TokenCount,
    Moderation,
}
