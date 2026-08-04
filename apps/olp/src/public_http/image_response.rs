use std::{io, sync::Arc};

use axum::{
    body::{Body, Bytes},
    http::{HeaderValue, header},
    response::Response,
};
use base64::{Engine as _, engine::general_purpose::STANDARD};
use futures::StreamExt as _;
use olp_domain::{ImagesResult, MediaHandle, MediaSpool, OpenedMedia};
use olp_protocols::openai::{OpenAiImagePayload, encode_image_response};
use uuid::Uuid;

use crate::gateway::InferenceError;

const MAX_IMAGE_BYTES: u64 = 50 * 1024 * 1024;
const MAX_TEMPLATE_BYTES: usize = 4 * 1024 * 1024;
const RAW_CHUNK_BYTES: usize = 48 * 1024;

/// Encodes image handles directly from the bounded spool into a chunked JSON
/// response. At most one small base64 chunk is resident at a time; the full
/// image and its full encoded representation are never copied into memory.
pub(crate) async fn streaming_image_json_response(
    spool: Arc<dyn MediaSpool>,
    result: &ImagesResult,
) -> Result<Response, InferenceError> {
    let mut staged = Vec::<(String, MediaHandle)>::new();
    let encoded = encode_image_response(result, |handle| {
        let marker = format!("__olp_streamed_media_{}__", Uuid::now_v7().simple());
        staged.push((marker.clone(), handle.clone()));
        Ok(OpenAiImagePayload::Base64Json(marker))
    });
    let wire = match encoded {
        Ok(wire) => wire,
        Err(error) => {
            cleanup_staged(&spool, &staged).await;
            return Err(InferenceError::bad_gateway(
                "provider_protocol_error",
                error.to_string(),
            ));
        }
    };
    let template = match serde_json::to_vec(&wire) {
        Ok(template) => template,
        Err(_) => {
            cleanup_staged(&spool, &staged).await;
            return Err(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider image metadata could not be encoded.",
            ));
        }
    };
    if template.len() > MAX_TEMPLATE_BYTES {
        cleanup_staged(&spool, &staged).await;
        return Err(InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider image metadata exceeded its bound.",
        ));
    }
    let pieces = match split_template(template, &staged) {
        Some(pieces) => pieces,
        None => {
            cleanup_staged(&spool, &staged).await;
            return Err(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider image response could not be streamed safely.",
            ));
        }
    };

    if staged.is_empty() {
        let mut response = Response::new(Body::from(pieces.into_iter().next().unwrap_or_default()));
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        return Ok(response);
    }

    let mut opened = Vec::with_capacity(staged.len());
    for (_, handle) in &staged {
        match spool.open(handle).await {
            Ok(media)
                if media
                    .artifact
                    .content_length
                    .is_none_or(|length| length <= MAX_IMAGE_BYTES) =>
            {
                opened.push((handle.clone(), media));
            }
            Ok(_) => {
                drop(opened);
                cleanup_staged(&spool, &staged).await;
                return Err(InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "A spooled image exceeded its declared bound.",
                ));
            }
            Err(_) => {
                drop(opened);
                cleanup_staged(&spool, &staged).await;
                return Err(InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "A spooled provider image was unavailable.",
                ));
            }
        }
    }

    let (sender, receiver) = tokio::sync::mpsc::channel(8);
    let producer_spool = Arc::clone(&spool);
    let cleanup = staged
        .into_iter()
        .map(|(_, handle)| handle)
        .collect::<Vec<_>>();
    tokio::spawn(async move {
        let mut pieces = pieces.into_iter();
        let mut completed = send_body_chunk(&sender, pieces.next().unwrap_or_default()).await;
        for (_, media) in opened {
            if !completed {
                break;
            }
            completed = stream_base64(&sender, media).await;
            if completed {
                completed = send_body_chunk(&sender, pieces.next().unwrap_or_default()).await;
            }
        }
        cleanup_handles(&producer_spool, &cleanup).await;
    });
    let body_stream = futures::stream::unfold(receiver, |mut receiver| async move {
        receiver.recv().await.map(|item| (item, receiver))
    });
    let mut response = Response::new(Body::from_stream(body_stream));
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/json"),
    );
    Ok(response)
}

fn split_template(template: Vec<u8>, staged: &[(String, MediaHandle)]) -> Option<Vec<Bytes>> {
    let mut pieces = Vec::with_capacity(staged.len() + 1);
    let mut offset = 0;
    for (marker, _) in staged {
        let marker = marker.as_bytes();
        if template
            .windows(marker.len())
            .filter(|candidate| *candidate == marker)
            .count()
            != 1
        {
            return None;
        }
        let relative = template[offset..]
            .windows(marker.len())
            .position(|candidate| candidate == marker)?;
        let start = offset + relative;
        pieces.push(Bytes::copy_from_slice(&template[offset..start]));
        offset = start + marker.len();
    }
    pieces.push(Bytes::copy_from_slice(&template[offset..]));
    Some(pieces)
}

async fn stream_base64(
    sender: &tokio::sync::mpsc::Sender<Result<Bytes, io::Error>>,
    mut media: OpenedMedia,
) -> bool {
    let mut total = 0_u64;
    let mut carry = Vec::with_capacity(2);
    while let Some(next) = media.bytes.next().await {
        let chunk = match next {
            Ok(chunk) => chunk,
            Err(_) => return send_body_error(sender).await,
        };
        total = match total.checked_add(u64::try_from(chunk.len()).unwrap_or(u64::MAX)) {
            Some(total) if total <= MAX_IMAGE_BYTES => total,
            _ => return send_body_error(sender).await,
        };
        let mut offset = 0;
        if !carry.is_empty() {
            let needed = 3 - carry.len();
            let take = needed.min(chunk.len());
            carry.extend_from_slice(&chunk[..take]);
            offset += take;
            if carry.len() == 3 {
                if !send_encoded(sender, &carry).await {
                    return false;
                }
                carry.clear();
            }
        }
        while chunk.len().saturating_sub(offset) >= 3 {
            let available = chunk.len() - offset;
            let take = available.min(RAW_CHUNK_BYTES);
            let take = take - (take % 3);
            if take == 0 || !send_encoded(sender, &chunk[offset..offset + take]).await {
                return false;
            }
            offset += take;
        }
        carry.extend_from_slice(&chunk[offset..]);
    }
    carry.is_empty() || send_encoded(sender, &carry).await
}

async fn send_encoded(
    sender: &tokio::sync::mpsc::Sender<Result<Bytes, io::Error>>,
    raw: &[u8],
) -> bool {
    sender
        .send(Ok(Bytes::from(STANDARD.encode(raw))))
        .await
        .is_ok()
}

async fn send_body_chunk(
    sender: &tokio::sync::mpsc::Sender<Result<Bytes, io::Error>>,
    chunk: Bytes,
) -> bool {
    chunk.is_empty() || sender.send(Ok(chunk)).await.is_ok()
}

async fn send_body_error(sender: &tokio::sync::mpsc::Sender<Result<Bytes, io::Error>>) -> bool {
    let _ = sender
        .send(Err(io::Error::other("bounded image response failed")))
        .await;
    false
}

async fn cleanup_staged(spool: &Arc<dyn MediaSpool>, staged: &[(String, MediaHandle)]) {
    for (_, handle) in staged {
        let _ = spool.remove(handle).await;
    }
}

async fn cleanup_handles(spool: &Arc<dyn MediaSpool>, handles: &[MediaHandle]) {
    for handle in handles {
        let _ = spool.remove(handle).await;
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use futures::stream;
    use olp_domain::{ImageArtifact, MediaSource, MediaUpload, SourceExtensions, Surface};

    use super::*;

    #[tokio::test]
    async fn image_json_is_incremental_valid_and_cleans_the_spool() {
        let spool = crate::create_bounded_media_spool_for_test().unwrap();
        let raw = vec![0x5a; 2 * 1024 * 1024 + 1];
        let artifact = spool
            .put(MediaUpload {
                filename: "large.png".into(),
                content_type: Some("image/png".into()),
                maximum_length: MAX_IMAGE_BYTES,
                bytes: Box::pin(stream::iter([Ok(Bytes::copy_from_slice(&raw))])),
            })
            .await
            .unwrap();
        let result = ImagesResult {
            created_at: Some(1),
            images: vec![ImageArtifact {
                source: MediaSource::Handle(artifact.handle.clone()),
                revised_prompt: Some("bounded".into()),
            }],
            usage: None,
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        };
        let response = streaming_image_json_response(Arc::clone(&spool), &result)
            .await
            .unwrap();
        let mut body = response.into_body().into_data_stream();
        let mut encoded = Vec::new();
        while let Some(chunk) = body.next().await {
            let chunk = chunk.unwrap();
            assert!(chunk.len() <= 70 * 1024, "response chunk was not bounded");
            encoded.extend_from_slice(&chunk);
        }
        let value: serde_json::Value = serde_json::from_slice(&encoded).unwrap();
        assert_eq!(
            STANDARD
                .decode(value["data"][0]["b64_json"].as_str().unwrap())
                .unwrap(),
            raw
        );
        assert!(spool.open(&artifact.handle).await.is_err());
    }
}
