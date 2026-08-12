//! Infrastructure-free canonical types, authorization, capabilities, and routing.
//!
//! This module deliberately has no HTTP, database, cache, or provider SDK
//! dependencies. Sibling adapters implement the traits in [`ports`] and
//! translate their data at the module boundary.

/// Defines a closed, string-backed enum from one canonical variant-to-wire map.
///
/// Keeping `ALL`, serde/schema names, display, and parsing in this expansion
/// makes adding a variant a single edit instead of several synchronized edits.
macro_rules! closed_string_enum {
    (
        $visibility:vis enum $name:ident {
            $($variant:ident => $wire:literal),+ $(,)?
        }
        parse_error $error:ty => $invalid:expr;
    ) => {
        #[derive(
            Clone,
            Copy,
            Debug,
            serde::Deserialize,
            Eq,
            Hash,
            Ord,
            PartialEq,
            PartialOrd,
            serde::Serialize,
            utoipa::ToSchema,
        )]
        $visibility enum $name {
            $(
                #[serde(rename = $wire)]
                #[schema(rename = $wire)]
                $variant,
            )+
        }

        impl $name {
            $visibility const ALL: [Self; closed_string_enum!(@count $($variant),+)] =
                [$(Self::$variant),+];

            #[must_use]
            $visibility const fn as_str(self) -> &'static str {
                match self {
                    $(Self::$variant => $wire),+
                }
            }
        }

        impl AsRef<str> for $name {
            fn as_ref(&self) -> &str {
                self.as_str()
            }
        }

        impl std::fmt::Display for $name {
            fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str(self.as_str())
            }
        }

        impl std::str::FromStr for $name {
            type Err = $error;

            fn from_str(value: &str) -> Result<Self, Self::Err> {
                match value {
                    $($wire => Ok(Self::$variant),)+
                    _ => Err(($invalid)(value)),
                }
            }
        }
    };
    (@count $head:ident $(, $tail:ident)*) => {
        1usize + closed_string_enum!(@count $($tail),*)
    };
    (@count) => { 0usize };
}

pub mod auth;
pub mod canonical;
pub mod ids;
pub mod ports;
pub mod provider;
pub mod provider_configuration;
pub mod routing;

pub use auth::{
    ApiKey, ApiKeyAuthorizationError, ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus,
    GatewayCapability, InvalidRole, OwnerInvariantError, Permission, Role, authorize_api_key,
    gateway_capability_for_operation, validate_owner_change,
};
pub use canonical::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, CanonicalResult, ContentPart,
    EmbeddingInput, EmbeddingVector, EmbeddingsRequest, EmbeddingsResult, ErrorClass,
    EventSequenceError, EventSequenceValidator, ExtensionError, FinishReason, GenerationParameters,
    GenerationRequest, ImageArtifact, ImageEditRequest, ImageGenerationRequest, ImageOperation,
    ImageVariationRequest, ImagesResult, InvalidOperationKind, InvalidSurface,
    InvalidTransportMode, MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, MediaArtifact, MediaHandle,
    MediaSource, Message, MessageRole, ModelDescriptor, ModelListResult, ModelOperation,
    ModerationItem, ModerationRequest, ModerationResult, Operation, OperationKind, RequestMetadata,
    ResponseFormat, SourceExtensions, SpeechRequest, SpeechResult, Surface, TokenCountRequest,
    TokenCountResult, ToolCall, ToolChoice, ToolDefinition, TranscriptionRequest,
    TranscriptionResult, TranscriptionSegment, TransportMode, Usage, UsageObservation,
    VideoContentResult, VideoCreateRequest, VideoDeleteResult, VideoJobRequest, VideoJobResult,
    VideoListRequest, VideoListResult, VideoOperation, VideoStatus, inline_media_marker,
    media_handle_from_inline_marker, validate_event_sequence,
};
pub use ids::{
    ApiKeyId, ApiKeyLookupId, ApiKeyLookupIdError, AttemptId, CredentialVersionId, DurationMs,
    ProviderId, RequestId, RouteId, RouteSlug, RouteSlugError, RuntimeGenerationId, TargetId,
};
pub use ports::{
    AttemptFailureClass, BoxFuture, DiscoveredProviderModel, MAX_UPSTREAM_RETRY_AFTER,
    MediaByteStream, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia, ProviderEventStream,
    ProviderOutput, ProviderRequest, ProviderTransport, TransportError, TransportPhase,
};
pub use provider::{
    CapabilitySource, ClosedSetParseError, ProviderAuthMode, ProviderState, RouteDraftState,
};
pub use provider_configuration::{
    CredentialRequirement, ProviderAuthModeSpec, ProviderConfiguration, ProviderConfigurationField,
    ProviderConfigurationViolation, ProviderFieldSpec, ProviderKindSpec, ProviderPresetSpec,
    ProviderViolationCode, ProviderViolationField, provider_kind_spec, provider_kind_specs,
    validate_provider_configuration,
};
pub use routing::{
    AttemptPlan, Capability, InvalidProviderKind, Provider, ProviderKind, Route,
    RouteValidationError, RoutingError, RuntimeGeneration, RuntimeSnapshot,
    SnapshotValidationError, Target, select_attempts, select_attempts_filtered,
    weighted_rendezvous_score,
};
