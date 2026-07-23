use std::sync::Arc;

use base64::{Engine as _, engine::general_purpose::STANDARD};
use bytes::Bytes;
use futures::stream;
use olp_domain::{MediaHandle, MediaSpool, MediaSpoolError, MediaUpload, inline_media_marker};
use olp_protocols::{
    anthropic::{
        ContentBlock as AnthropicContentBlock, CountTokensRequest as AnthropicCountTokensRequest,
        Message as AnthropicMessage, MessageContent as AnthropicMessageContent,
        MessagesRequest as AnthropicMessagesRequest, ToolResultContent,
    },
    gemini::{
        Content as GeminiContent, CountTokensRequest as GeminiCountTokensRequest,
        GenerateContentRequest as GeminiGenerateContentRequest, Part as GeminiPart,
    },
    openai::{
        ChatCompletionRequest, ChatContentPart, ChatMessageContent, ResponseCreateRequest,
        ResponseInput, ResponseInputTokensRequest,
    },
};
use serde_json::Value;

use crate::{GatewayState, gateway::InferenceError};

pub(crate) const MAX_INLINE_MEDIA_ITEMS: usize = 4;
pub(crate) const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;
pub(crate) const MAX_INLINE_MEDIA_TOTAL_BYTES: usize = 2 * 1024 * 1024;

struct InlineMediaAdmission {
    spool: Arc<dyn MediaSpool>,
    handles: Vec<MediaHandle>,
    total_bytes: usize,
}

impl InlineMediaAdmission {
    fn new(state: &GatewayState) -> Self {
        Self {
            spool: state.media_spool.clone(),
            handles: Vec::new(),
            total_bytes: 0,
        }
    }

    async fn stage_base64(
        &mut self,
        encoded: &str,
        mime_type: &str,
        filename: String,
    ) -> Result<String, InferenceError> {
        if self.handles.len() >= MAX_INLINE_MEDIA_ITEMS {
            return Err(invalid_inline_media("Too many inline media items."));
        }
        let maximum_encoded = MAX_INLINE_MEDIA_BYTES.div_ceil(3).saturating_mul(4);
        if encoded.is_empty()
            || encoded.len() > maximum_encoded
            || encoded.bytes().any(|byte| byte.is_ascii_whitespace())
        {
            return Err(invalid_inline_media(
                "Inline media exceeds its encoded size limit.",
            ));
        }
        let decoded = STANDARD
            .decode(encoded)
            .map_err(|_| invalid_inline_media("Inline media is not valid canonical base64."))?;
        if decoded.is_empty() || decoded.len() > MAX_INLINE_MEDIA_BYTES {
            return Err(invalid_inline_media(
                "Inline media exceeds its decoded size limit.",
            ));
        }
        self.total_bytes = self.total_bytes.saturating_add(decoded.len());
        if self.total_bytes > MAX_INLINE_MEDIA_TOTAL_BYTES {
            return Err(invalid_inline_media(
                "Inline media exceeds the aggregate decoded size limit.",
            ));
        }
        let length = u64::try_from(decoded.len()).unwrap_or(u64::MAX);
        let artifact = self
            .spool
            .put(MediaUpload {
                filename,
                content_type: Some(mime_type.to_owned()),
                maximum_length: u64::try_from(MAX_INLINE_MEDIA_BYTES).unwrap_or(u64::MAX),
                bytes: Box::pin(stream::once(async move {
                    Ok::<Bytes, MediaSpoolError>(Bytes::from(decoded))
                })),
            })
            .await
            .map_err(|error| match error {
                olp_domain::MediaSpoolError::TooLarge { .. } => {
                    invalid_inline_media("Inline media exceeds its decoded size limit.")
                }
                _ => InferenceError::unavailable("media_spool_unavailable"),
            })?;
        if artifact.content_length != Some(length) {
            let _ = self.spool.remove(&artifact.handle).await;
            return Err(InferenceError::unavailable("media_spool_unavailable"));
        }
        self.handles.push(artifact.handle.clone());
        Ok(inline_media_marker(&artifact.handle))
    }

    fn into_handles(mut self) -> Vec<MediaHandle> {
        std::mem::take(&mut self.handles)
    }

    async fn cleanup_and_fail<T>(mut self, error: InferenceError) -> Result<T, InferenceError> {
        let handles = std::mem::take(&mut self.handles);
        cleanup_handles_owned(self.spool.clone(), handles).await;
        Err(error)
    }
}

impl Drop for InlineMediaAdmission {
    fn drop(&mut self) {
        if self.handles.is_empty() {
            return;
        }
        let spool = self.spool.clone();
        let handles = std::mem::take(&mut self.handles);
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                cleanup_handles(&spool, handles).await;
            });
        }
    }
}

fn invalid_inline_media(message: impl Into<String>) -> InferenceError {
    InferenceError::invalid_request(message.into())
}

fn require_image_mime(value: &str) -> Result<&str, InferenceError> {
    if matches!(
        value,
        "image/jpeg" | "image/png" | "image/gif" | "image/webp"
    ) {
        Ok(value)
    } else {
        Err(invalid_inline_media(
            "Inline image MIME type is not supported.",
        ))
    }
}

fn require_audio_mime(value: &str) -> Result<&str, InferenceError> {
    if matches!(value, "audio/wav" | "audio/mpeg" | "audio/mp3") {
        Ok(value)
    } else {
        Err(invalid_inline_media(
            "Inline audio MIME type is not supported.",
        ))
    }
}

fn openai_audio_mime(format: &str) -> Result<&'static str, InferenceError> {
    match format {
        "wav" => Ok("audio/wav"),
        "mp3" => Ok("audio/mpeg"),
        _ => Err(invalid_inline_media(
            "OpenAI input_audio supports only wav or mp3.",
        )),
    }
}

async fn admit_anthropic_messages_inner(
    admission: &mut InlineMediaAdmission,
    messages: &mut [AnthropicMessage],
) -> Result<(), InferenceError> {
    for message in messages {
        let AnthropicMessageContent::Blocks(blocks) = &mut message.content else {
            continue;
        };
        for block in blocks {
            admit_anthropic_block(admission, block).await?;
        }
    }
    Ok(())
}

async fn admit_anthropic_block(
    admission: &mut InlineMediaAdmission,
    block: &mut AnthropicContentBlock,
) -> Result<(), InferenceError> {
    match block {
        AnthropicContentBlock::Image(image) if image.source.kind == "base64" => {
            let mime = image.source.media_type.as_deref().ok_or_else(|| {
                invalid_inline_media("Anthropic base64 image requires media_type")
            })?;
            require_image_mime(mime)?;
            let data = image
                .source
                .data
                .as_deref()
                .ok_or_else(|| invalid_inline_media("Anthropic base64 image requires data"))?;
            image.source.data = Some(
                admission
                    .stage_base64(
                        data,
                        mime,
                        format!("anthropic-image-{}.bin", admission.handles.len()),
                    )
                    .await?,
            );
        }
        AnthropicContentBlock::ToolResult(result) => {
            if let Some(ToolResultContent::Blocks(blocks)) = &mut result.content {
                for block in blocks {
                    Box::pin(admit_anthropic_block(admission, block)).await?;
                }
            }
        }
        _ => {}
    }
    Ok(())
}

pub(crate) async fn admit_anthropic_messages(
    state: &GatewayState,
    request: &mut AnthropicMessagesRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    if let Err(error) = admit_anthropic_messages_inner(&mut admission, &mut request.messages).await
    {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

pub(crate) async fn admit_anthropic_count(
    state: &GatewayState,
    request: &mut AnthropicCountTokensRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    if let Err(error) = admit_anthropic_messages_inner(&mut admission, &mut request.messages).await
    {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

async fn admit_gemini_content(
    admission: &mut InlineMediaAdmission,
    contents: &mut [GeminiContent],
) -> Result<(), InferenceError> {
    for content in contents {
        for part in &mut content.parts {
            let GeminiPart::InlineData(part) = part else {
                continue;
            };
            let mime = part.inline_data.mime_type.as_str();
            if mime.starts_with("image/") {
                require_image_mime(mime)?;
            } else if mime.starts_with("audio/") {
                require_audio_mime(mime)?;
            } else {
                return Err(invalid_inline_media(
                    "Gemini inlineData supports bounded launch image/audio MIME types only.",
                ));
            }
            part.inline_data.data = admission
                .stage_base64(
                    &part.inline_data.data,
                    mime,
                    format!("gemini-inline-{}.bin", admission.handles.len()),
                )
                .await?;
        }
    }
    Ok(())
}

pub(crate) async fn admit_gemini_generate(
    state: &GatewayState,
    request: &mut GeminiGenerateContentRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    if let Err(error) = admit_gemini_content(&mut admission, &mut request.contents).await {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

pub(crate) async fn admit_gemini_count(
    state: &GatewayState,
    request: &mut GeminiCountTokensRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    let result = async {
        admit_gemini_content(&mut admission, &mut request.contents).await?;
        if let Some(generation) = &mut request.generate_content_request {
            admit_gemini_content(&mut admission, &mut generation.contents).await?;
        }
        Ok::<(), InferenceError>(())
    }
    .await;
    if let Err(error) = result {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

pub(crate) async fn admit_openai_chat(
    state: &GatewayState,
    request: &mut ChatCompletionRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    let result = async {
        for message in &mut request.messages {
            let Some(ChatMessageContent::Parts(parts)) = &mut message.content else {
                continue;
            };
            for part in parts {
                let ChatContentPart::InputAudio { input_audio, .. } = part else {
                    continue;
                };
                let mime = openai_audio_mime(&input_audio.format)?;
                input_audio.data = admission
                    .stage_base64(
                        &input_audio.data,
                        mime,
                        format!("openai-audio-{}.bin", admission.handles.len()),
                    )
                    .await?;
            }
        }
        Ok::<(), InferenceError>(())
    }
    .await;
    if let Err(error) = result {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

pub(crate) async fn admit_openai_responses(
    state: &GatewayState,
    request: &mut ResponseCreateRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    admit_openai_response_input(state, &mut request.input).await
}

pub(crate) async fn admit_openai_response_input_tokens(
    state: &GatewayState,
    request: &mut ResponseInputTokensRequest,
) -> Result<Vec<MediaHandle>, InferenceError> {
    admit_openai_response_input(state, &mut request.input).await
}

async fn admit_openai_response_input(
    state: &GatewayState,
    input: &mut ResponseInput,
) -> Result<Vec<MediaHandle>, InferenceError> {
    let mut admission = InlineMediaAdmission::new(state);
    let result = async {
        let ResponseInput::Items(items) = input else {
            return Ok::<(), InferenceError>(());
        };
        for item in items {
            let Some(content) = item.get_mut("content").and_then(Value::as_array_mut) else {
                continue;
            };
            for part in content {
                let Some(object) = part.as_object_mut() else {
                    continue;
                };
                match object.get("type").and_then(Value::as_str) {
                    Some("input_audio") => {
                        let audio = object
                            .get_mut("input_audio")
                            .and_then(Value::as_object_mut)
                            .ok_or_else(|| {
                                invalid_inline_media(
                                    "Responses input_audio must be an object.",
                                )
                            })?;
                        let format = audio
                            .get("format")
                            .and_then(Value::as_str)
                            .ok_or_else(|| {
                                invalid_inline_media("Responses input_audio requires format.")
                            })?;
                        let mime = openai_audio_mime(format)?;
                        let data = audio
                            .get("data")
                            .and_then(Value::as_str)
                            .ok_or_else(|| {
                                invalid_inline_media("Responses input_audio requires data.")
                            })?;
                        let marker = admission
                            .stage_base64(
                                data,
                                mime,
                                format!("responses-audio-{}.bin", admission.handles.len()),
                            )
                            .await?;
                        audio.insert("data".to_owned(), Value::String(marker));
                    }
                    Some("input_file") => {
                        if object.contains_key("file_id") || object.contains_key("file_url") {
                            return Err(invalid_inline_media(
                                "Responses file_id/file_url resolution is outside the launch contract; use inline PDF file_data.",
                            ));
                        }
                        let filename = object
                            .get("filename")
                            .and_then(Value::as_str)
                            .filter(|value| value.to_ascii_lowercase().ends_with(".pdf"))
                            .ok_or_else(|| {
                                invalid_inline_media(
                                    "Responses input_file requires a PDF filename.",
                                )
                            })?
                            .to_owned();
                        let file_data = object
                            .get("file_data")
                            .and_then(Value::as_str)
                            .ok_or_else(|| {
                                invalid_inline_media("Responses input_file requires file_data.")
                            })?;
                        let encoded = file_data
                            .strip_prefix("data:application/pdf;base64,")
                            .ok_or_else(|| {
                                invalid_inline_media(
                                    "Responses input_file requires an application/pdf data URL.",
                                )
                            })?;
                        let marker = admission
                            .stage_base64(encoded, "application/pdf", filename)
                            .await?;
                        object.insert("file_data".to_owned(), Value::String(marker));
                    }
                    _ => {}
                }
            }
        }
        Ok::<(), InferenceError>(())
    }
    .await;
    if let Err(error) = result {
        return admission.cleanup_and_fail(error).await;
    }
    Ok(admission.into_handles())
}

pub(crate) async fn cleanup_admitted(state: &GatewayState, handles: Vec<MediaHandle>) {
    cleanup_handles_owned(state.media_spool.clone(), handles).await;
}

async fn cleanup_handles_owned(spool: Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    if handles.is_empty() {
        return;
    }
    let cleanup = tokio::spawn(async move {
        cleanup_handles(&spool, handles).await;
    });
    let _ = cleanup.await;
}

async fn cleanup_handles(spool: &Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    for handle in handles {
        let _ = spool.remove(&handle).await;
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use base64::{Engine as _, engine::general_purpose::STANDARD};
    use futures::StreamExt as _;
    use olp_domain::{ContentPart, MediaSource, Operation, Surface};
    use olp_protocols::{
        anthropic::{MessagesRequest, decode_messages_request},
        gemini::{GenerateContentRequest, decode_generate_content_request},
        openai::{
            ChatCompletionRequest, ResponseCreateRequest, ResponseInputTokensRequest,
            decode_chat_completion, decode_response_create, decode_response_input_tokens,
            encode_response_input_tokens,
        },
    };

    use super::*;
    use crate::{ApiMode, RuntimeManager};

    fn state() -> GatewayState {
        GatewayState::new(
            ApiMode::Gateway,
            None,
            Arc::new(RuntimeManager::empty()),
            "https://olp.test",
            "console",
        )
    }

    async fn assert_spooled(state: &GatewayState, handle: &MediaHandle, expected: &[u8]) {
        let mut opened = state.media_spool.open(handle).await.unwrap().bytes;
        let mut actual = Vec::new();
        while let Some(chunk) = opened.next().await {
            actual.extend_from_slice(&chunk.unwrap());
        }
        assert_eq!(actual, expected);
    }

    #[tokio::test]
    async fn anthropic_base64_image_is_replaced_by_a_bounded_handle() {
        let state = state();
        let mut request: MessagesRequest = serde_json::from_value(serde_json::json!({
            "model":"route", "max_tokens":8,
            "messages":[{"role":"user","content":[{"type":"image","source":{
                "type":"base64","media_type":"image/png","data":"aGk="
            }}]}]
        }))
        .unwrap();
        let handles = admit_anthropic_messages(&state, &mut request)
            .await
            .unwrap();
        assert!(!serde_json::to_string(&request).unwrap().contains("aGk="));
        let operation = decode_messages_request(request).unwrap();
        let Operation::Generation(generation) = operation else {
            panic!("expected generation")
        };
        let ContentPart::Image {
            source: MediaSource::Handle(handle),
            ..
        } = &generation.messages[0].content[0]
        else {
            panic!("expected bounded image handle")
        };
        assert_spooled(&state, handle, b"hi").await;
        cleanup_admitted(&state, handles).await;
    }

    #[tokio::test]
    async fn gemini_inline_audio_and_openai_chat_audio_are_admitted() {
        let state = state();
        let mut gemini: GenerateContentRequest = serde_json::from_value(serde_json::json!({
            "contents":[{"role":"user","parts":[{"inlineData":{
                "mimeType":"audio/wav","data":"aGk="
            }}]}]
        }))
        .unwrap();
        let handles = admit_gemini_generate(&state, &mut gemini).await.unwrap();
        let Operation::Generation(generation) =
            decode_generate_content_request("route", gemini, false).unwrap()
        else {
            panic!("expected generation")
        };
        assert!(matches!(
            generation.messages[0].content[0],
            ContentPart::InputAudio { ref format, .. } if format == "audio/wav"
        ));
        cleanup_admitted(&state, handles).await;

        let mut chat: ChatCompletionRequest = serde_json::from_value(serde_json::json!({
            "model":"route", "messages":[{"role":"user","content":[{
                "type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}
            }]}]
        }))
        .unwrap();
        let handles = admit_openai_chat(&state, &mut chat).await.unwrap();
        let Operation::Generation(generation) = decode_chat_completion(chat).unwrap() else {
            panic!("expected generation")
        };
        assert!(matches!(
            generation.messages[0].content[0],
            ContentPart::InputAudio { ref format, .. } if format == "wav"
        ));
        cleanup_admitted(&state, handles).await;
    }

    #[tokio::test]
    async fn responses_inline_pdf_is_admitted_while_remote_files_fail_explicitly() {
        let state = state();
        let mut request: ResponseCreateRequest = serde_json::from_value(serde_json::json!({
            "model":"route", "input":[{"type":"message","role":"user","content":[{
                "type":"input_file","filename":"brief.pdf",
                "file_data":"data:application/pdf;base64,aGk="
            }]}]
        }))
        .unwrap();
        let handles = admit_openai_responses(&state, &mut request).await.unwrap();
        let Operation::Generation(generation) = decode_response_create(request).unwrap() else {
            panic!("expected generation")
        };
        assert!(matches!(
            generation.messages[0].content[0],
            ContentPart::InputFile { ref mime_type, .. } if mime_type == "application/pdf"
        ));
        cleanup_admitted(&state, handles).await;

        let mut remote: ResponseCreateRequest = serde_json::from_value(serde_json::json!({
            "model":"route", "input":[{"type":"message","role":"user","content":[{
                "type":"input_file","file_url":"https://example.test/private.pdf"
            }]}]
        }))
        .unwrap();
        assert!(admit_openai_responses(&state, &mut remote).await.is_err());
    }

    #[tokio::test]
    async fn response_input_tokens_admits_media_and_preserves_only_same_protocol_semantics() {
        let state = state();
        let mut request: ResponseInputTokensRequest = serde_json::from_value(serde_json::json!({
            "model":"count-route",
            "input":[{"type":"message","role":"user","content":[
                {"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}},
                {"type":"input_file","filename":"brief.pdf",
                 "file_data":"data:application/pdf;base64,aGk="}
            ]}]
        }))
        .unwrap();
        let handles = admit_openai_response_input_tokens(&state, &mut request)
            .await
            .unwrap();
        assert_eq!(handles.len(), 2);
        let serialized = serde_json::to_string(&request).unwrap();
        assert!(!serialized.contains("aGk="));
        assert!(serialized.contains("urn:olp:inline-media:"));

        let Operation::TokenCount(operation) = decode_response_input_tokens(request).unwrap()
        else {
            panic!("expected token count")
        };
        assert!(matches!(operation.input[0], ContentPart::InputAudio { .. }));
        assert!(matches!(operation.input[1], ContentPart::InputFile { .. }));
        operation
            .extensions
            .ensure_representable_on(Surface::Anthropic)
            .expect_err("Responses media must not be silently translated cross-protocol");
        let forwarded = encode_response_input_tokens(&operation, "gpt-upstream").unwrap();
        let forwarded = serde_json::to_value(forwarded).unwrap();
        assert_eq!(forwarded["model"], "gpt-upstream");
        assert!(
            forwarded["input"][0]["content"][0]["input_audio"]["data"]
                .as_str()
                .unwrap()
                .starts_with("urn:olp:inline-media:")
        );
        assert!(
            forwarded["input"][0]["content"][1]["file_data"]
                .as_str()
                .unwrap()
                .starts_with("urn:olp:inline-media:")
        );
        cleanup_admitted(&state, handles).await;
    }

    #[tokio::test]
    async fn malformed_and_oversized_inline_media_fail_before_spooling() {
        let state = state();
        for data in [
            "%%%".to_owned(),
            STANDARD.encode(vec![0_u8; MAX_INLINE_MEDIA_BYTES + 1]),
        ] {
            let mut request: ChatCompletionRequest = serde_json::from_value(serde_json::json!({
                "model":"route", "messages":[{"role":"user","content":[{
                    "type":"input_audio","input_audio":{"data":data,"format":"wav"}
                }]}]
            }))
            .unwrap();
            assert!(admit_openai_chat(&state, &mut request).await.is_err());
        }
    }
}
