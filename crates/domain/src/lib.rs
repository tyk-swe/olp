//! Infrastructure-free canonical types, authorization, capabilities, and routing.
//!
//! This crate deliberately has no HTTP, database, cache, or provider SDK
//! dependencies. Adapters implement the traits in [`ports`] and translate their
//! data at the crate boundary.

pub mod auth;
pub mod canonical;
pub mod ids;
pub mod ports;
pub mod provider;
pub mod routing;

pub use auth::{
    ApiKey, ApiKeyAuthorizationError, ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus,
    InvalidRole, OwnerInvariantError, Permission, Role, authorize_api_key, validate_owner_change,
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
    TranscriptionResult, TranscriptionSegment, TransportMode, Usage, VideoContentResult,
    VideoCreateRequest, VideoDeleteResult, VideoJobRequest, VideoJobResult, VideoListRequest,
    VideoListResult, VideoOperation, VideoStatus, inline_media_marker,
    media_handle_from_inline_marker, validate_event_sequence,
};
pub use ids::{
    ApiKeyId, ApiKeyLookupId, ApiKeyLookupIdError, AttemptId, CredentialVersionId, DurationMs,
    ProviderId, RequestId, RouteId, RouteSlug, RouteSlugError, RuntimeGenerationId, TargetId,
};
pub use ports::{
    AttemptFailureClass, BoxFuture, DiscoveredProviderModel, MediaByteStream, MediaSpool,
    MediaSpoolError, MediaUpload, OpenedMedia, ProviderEventStream, ProviderOutput,
    ProviderRequest, ProviderTransport, TransportError, TransportPhase,
};
pub use provider::{
    CapabilitySource, ClosedSetParseError, ProviderAuthMode, ProviderState, RouteDraftState,
};
pub use routing::{
    AttemptPlan, Capability, CapabilityKey, InvalidProviderKind, Provider, ProviderKind, Route,
    RouteValidationError, RoutingError, RuntimeGeneration, RuntimeSnapshot,
    SnapshotValidationError, Target, select_attempts, select_attempts_filtered,
    weighted_rendezvous_score,
};
