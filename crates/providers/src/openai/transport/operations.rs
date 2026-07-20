mod audio;
mod generation;
mod http;
mod images;
mod results;
mod video;

use olp_domain::{
    AttemptFailureClass, Operation, ProviderKind, ProviderOutput, ProviderRequest, TransportError,
    TransportMode, TransportPhase,
};

use super::{AuthStyle, OpenAiConnector, errors::transport_error};

pub(super) use super::streams::require_content_type;

impl OpenAiConnector {
    pub(super) async fn execute_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let provider_kind_matches = match self.auth_style {
            AuthStyle::Bearer => matches!(
                request.attempt.provider_kind,
                ProviderKind::OpenAi | ProviderKind::OpenAiCompatible
            ),
            // Compatibility probes use OpenAiCompatible before the Azure
            // wrapper executes real attempts as AzureOpenAi.
            AuthStyle::ApiKeyHeader => matches!(
                request.attempt.provider_kind,
                ProviderKind::AzureOpenAi | ProviderKind::OpenAiCompatible
            ),
        };
        if !provider_kind_matches {
            return Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                "OpenAI connector received an attempt for another provider kind",
            ));
        }
        if request.metadata.operation != request.operation.kind() {
            return Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                "request metadata operation does not match the canonical operation",
            ));
        }
        validate_transport_mode(&request)?;

        match &request.operation {
            Operation::Generation(_) => generation::execute(self, request).await,
            Operation::Images(_) => images::execute(self, request).await,
            Operation::Speech(_) => audio::execute_speech(self, request).await,
            Operation::Transcription(_) => audio::execute_transcription(self, request).await,
            Operation::Video(_) => video::execute(self, request).await,
            Operation::Embeddings(_) | Operation::TokenCount(_) | Operation::Moderation(_) => {
                results::execute(self, request).await
            }
            Operation::Models(_) => Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                format!(
                    "OpenAI connector does not yet transport {:?}",
                    request.operation.kind()
                ),
            )),
        }
    }
}

pub(super) fn validate_transport_mode(request: &ProviderRequest) -> Result<(), TransportError> {
    let mode = request.metadata.mode;
    let streaming = mode == TransportMode::Streaming;
    let valid = match &request.operation {
        Operation::Generation(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.parameters.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Generation(operation)) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Edit(operation)) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Variation(_)) => mode == TransportMode::Unary,
        Operation::Speech(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Transcription(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Video(olp_domain::VideoOperation::Create(_)) => mode == TransportMode::Async,
        Operation::Video(
            olp_domain::VideoOperation::List(_)
            | olp_domain::VideoOperation::Get(_)
            | olp_domain::VideoOperation::Content(_)
            | olp_domain::VideoOperation::Delete(_),
        )
        | Operation::Embeddings(_)
        | Operation::TokenCount(_)
        | Operation::Moderation(_)
        | Operation::Models(_) => mode == TransportMode::Unary,
    };
    if valid {
        Ok(())
    } else {
        Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "canonical operation does not match the selected OpenAI transport mode",
        ))
    }
}
