mod audio;
mod chat;
mod client;
mod embeddings;
use crate::protocols::extensions;
mod images;
mod media;
mod moderation;
mod response;
mod responses;
mod video;

pub use audio::{
    AudioCodecError, DEFAULT_AUDIO_UPLOAD_LIMIT, OpenAiSpeechRequest, OpenAiSpeechStreamEvent,
    OpenAiTranscriptionJson, OpenAiTranscriptionRequest, OpenAiTranscriptionResponse,
    OpenAiTranscriptionSegment, OpenAiTranscriptionStreamDecoder, OpenAiTranscriptionStreamEncoder,
    OpenAiTranscriptionUsage, SpeechStreamUpdate, TranscriptionInputTokenDetails,
    TranscriptionResponseFormat, decode_speech, decode_speech_body, decode_speech_stream_event,
    decode_transcription, decode_transcription_response, encode_speech, encode_speech_body,
    encode_speech_stream_update, encode_transcription, encode_transcription_response,
};
pub use chat::{
    ChatCompletionRequest, ChatContentPart, ChatFunctionCall, ChatFunctionDefinition, ChatImageUrl,
    ChatInputAudio, ChatJsonSchema, ChatMessage, ChatMessageContent, ChatNamedFunction,
    ChatNamedToolChoice, ChatResponseFormat, ChatRole, ChatTool, ChatToolCall, ChatToolChoice,
    OpenAiDecodeError, OpenAiEncodeError, StopSequences, decode_chat_completion,
    encode_chat_completion,
};
pub use client::{OpenAiClientEncodeError, OpenAiResponsesStreamEncoder, encode_response_object};
pub use embeddings::{
    EmbeddingCodecError, EmbeddingData, EmbeddingRequest, EmbeddingResponse, EmbeddingUsage,
    EmbeddingWireInput, EmbeddingWireVector, decode_embedding_request, decode_embedding_response,
    encode_embedding_request, encode_embedding_response,
};
pub use images::{
    DEFAULT_IMAGE_UPLOAD_LIMIT, ImageCodecError, ImageStreamOperation, ImageStreamUpdate,
    OpenAiImageData, OpenAiImageEditRequest, OpenAiImageGenerationRequest, OpenAiImagePayload,
    OpenAiImageResponse, OpenAiImageStreamEvent, OpenAiImageUsage, OpenAiImageVariationRequest,
    decode_image_edit, decode_image_generation, decode_image_response, decode_image_stream_event,
    decode_image_variation, encode_image_edit, encode_image_generation, encode_image_response,
    encode_image_stream_update, encode_image_variation,
};
pub use media::{BinaryMediaBody, BoundedMediaPart, MediaPartError};
pub use moderation::{
    ModerationCodecError, OpenAiModerationRequest, OpenAiModerationResponse,
    OpenAiModerationResult, decode_moderation, decode_moderation_response, encode_moderation,
    encode_moderation_response,
};
pub(crate) use response::decode_compatible_chat_completion_response;
pub use response::{
    ChatChunkChoice, ChatCompletionChoice, ChatCompletionChunk, ChatCompletionResponse, ChatDelta,
    ChatFunctionCallDelta, ChatResponseMessage, ChatToolCallDelta, ChatUsage,
    CompletionTokenDetails, OpenAiChatStreamDecoder, OpenAiResponseError, OpenAiStreamError,
    PromptTokenDetails, decode_chat_completion_response,
};
pub use responses::{
    OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION, OpenAiResponsesStreamDecoder,
    ResponseCreateRequest, ResponseErrorBody, ResponseInput, ResponseInputTokenDetails,
    ResponseInputTokensRequest, ResponseInputTokensResponse, ResponseNamedToolChoice,
    ResponseObject, ResponseOutputTokenDetails, ResponseTextConfig, ResponseTextFormat,
    ResponseTool, ResponseToolChoice, ResponseUsage, ResponsesCodecError, decode_response_create,
    decode_response_input_tokens, decode_response_input_tokens_result, decode_response_object,
    encode_response_create, encode_response_input_tokens, encode_response_input_tokens_result,
};
pub use video::{
    DEFAULT_VIDEO_REFERENCE_LIMIT, MAX_VIDEO_PROMPT_LENGTH, OpenAiVideoContentQuery,
    OpenAiVideoCreateRequest, OpenAiVideoDeleteResponse, OpenAiVideoError, OpenAiVideoListQuery,
    OpenAiVideoListResponse, OpenAiVideoObject, VideoCodecError, decode_video_content,
    decode_video_content_body, decode_video_content_with_query, decode_video_create,
    decode_video_delete, decode_video_delete_response, decode_video_get, decode_video_list,
    decode_video_list_response, decode_video_object, encode_video_content_body,
    encode_video_create, encode_video_delete_response, encode_video_list,
    encode_video_list_response, encode_video_object,
};

/// One taxonomy for "this OpenAI error is an upstream rate limit" across the
/// unary Responses decoder, the Responses stream decoder, and the Chat
/// Completions decoder, so retryability cannot diverge per surface.
pub(in crate::protocols) fn error_signals_rate_limit(
    code: Option<&str>,
    kind: Option<&str>,
) -> bool {
    code.is_some_and(|code| code.contains("rate_limit"))
        || kind.is_some_and(|kind| kind.contains("rate_limit"))
}
