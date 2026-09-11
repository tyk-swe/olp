use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::Operation;
use crate::providers::runtime_model::ProviderKind;

use crate::providers::openai::transport::AuthStyle;
use crate::providers::openai::transport::Connector;
use crate::providers::transport_common::transport_error;

impl Connector {
    pub(crate) async fn execute_request(
        &self,
        mut request: ProviderRequest,
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
        let translated = crate::providers::profiles::operation(
            &request.operation,
            &self.options,
            request.attempt.provider_kind,
        );
        crate::providers::profiles::validate(&request.operation, &self.options)
            .map_err(crate::providers::transport_common::protocol_error)?;
        if let std::borrow::Cow::Owned(operation) = translated {
            request.operation = std::sync::Arc::new(operation);
        }
        let model = self.dispatched_model(&request.attempt.upstream_model);
        if model != request.attempt.upstream_model {
            request.attempt.upstream_model = model.to_owned();
        }

        // This dispatcher owns the operation narrowing contract used by each
        // operation module's infallible destructuring below.
        match &*request.operation {
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

pub(crate) fn validate_transport_mode(request: &ProviderRequest) -> Result<(), TransportError> {
    let mode = request.metadata.mode;
    let streaming = mode == TransportMode::Streaming;
    let valid = match &*request.operation {
        Operation::Generation(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.parameters.stream == streaming
        }
        Operation::Images(crate::protocols::canonical::requests::ImageOperation::Generation(
            operation,
        )) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(crate::protocols::canonical::requests::ImageOperation::Edit(
            operation,
        )) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(crate::protocols::canonical::requests::ImageOperation::Variation(_)) => {
            mode == TransportMode::Unary
        }
        Operation::Speech(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Transcription(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Video(crate::protocols::canonical::requests::VideoOperation::Create(_)) => {
            mode == TransportMode::Async
        }
        Operation::Video(
            crate::protocols::canonical::requests::VideoOperation::List(_)
            | crate::protocols::canonical::requests::VideoOperation::Get(_)
            | crate::protocols::canonical::requests::VideoOperation::Content(_)
            | crate::protocols::canonical::requests::VideoOperation::Delete(_),
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

pub mod audio;

pub mod generation;

pub mod http;

pub mod images;

pub mod results;

pub mod video;
