use http::{StatusCode, header};
use olp_domain::{
    CanonicalResult, MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, Operation, ProviderOutput,
    ProviderRequest, Surface, TransportError,
};
use olp_protocols::openai::{
    OpenAiVideoDeleteResponse, OpenAiVideoListResponse, OpenAiVideoObject,
    decode_video_content_body, decode_video_delete_response, decode_video_list_response,
    decode_video_object, encode_video_create, encode_video_list,
};
use reqwest::{Method, multipart};

use super::{
    super::{OpenAiConnector, errors::*, media::*, streams::read_deadline_body},
    require_content_type,
};

pub(super) async fn execute(
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let Operation::Video(operation) = &request.operation else {
        unreachable!("checked by caller")
    };
    match operation {
        olp_domain::VideoOperation::Create(operation) => {
            let reference = if let Some(handle) = operation.input.as_ref() {
                let spool = request.media.as_ref().ok_or_else(|| {
                    protocol_body_error("OpenAI video input requires a bounded media spool")
                })?;
                Some(
                    bounded_part(
                        spool.as_ref(),
                        handle,
                        olp_protocols::openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
                    )
                    .await?,
                )
            } else {
                None
            };
            let mut reference_metadata = reference.clone();
            let wire = encode_video_create(operation, &request.attempt.upstream_model, |_| {
                reference_metadata.take().ok_or_else(|| {
                    olp_protocols::openai::VideoCodecError::Staging(
                        "video input spool metadata was unavailable".into(),
                    )
                })
            })
            .map_err(|error| protocol_encode_error("video create", error))?;
            let mut form = multipart::Form::new()
                .text("model", wire.model)
                .text("prompt", wire.prompt);
            form = add_optional_text(form, "seconds", wire.seconds);
            form = add_optional_text(form, "size", wire.size);
            if let Some(handle) = operation.input.as_ref() {
                let spool = request.media.as_ref().expect("validated above");
                form = form.part(
                    "input_reference",
                    multipart_part(spool.open(handle).await.map_err(map_spool_error)?)?,
                );
            }
            form = add_extra_fields(form, wire.extra);
            let response = connector
                .post_multipart_raw(&request, "videos", form)
                .await?;
            require_content_type(&response, "application/json")?;
            let bytes = read_deadline_body(
                response,
                connector.config.timeouts.idle,
                connector.config.response_limits.response_bytes,
            )
            .await?;
            let wire: OpenAiVideoObject = parse_wire("video create", &bytes)?;
            let result = decode_video_object(wire)
                .map_err(|error| protocol_decode_error("video create", error))?;
            Ok(ProviderOutput::Result(Box::new(CanonicalResult::VideoJob(
                result,
            ))))
        }
        olp_domain::VideoOperation::List(operation) => {
            let wire = encode_video_list(operation)
                .map_err(|error| protocol_encode_error("video list", error))?;
            let mut path = "videos".to_owned();
            let mut query = Vec::new();
            if let Some(after) = wire.after {
                query.push(("after", after));
            }
            if let Some(limit) = wire.limit {
                query.push(("limit", limit.to_string()));
            }
            if let Some(order) = wire.order {
                query.push(("order", order));
            }
            if !query.is_empty() {
                path.push('?');
                path.push_str(
                    &query
                        .into_iter()
                        .map(|(name, value)| format!("{name}={}", percent_encode(&value)))
                        .collect::<Vec<_>>()
                        .join("&"),
                );
            }
            let bytes = connector
                .request_json(&request, Method::GET, &path, None)
                .await?;
            let wire: OpenAiVideoListResponse = parse_wire("video list", &bytes)?;
            let result = decode_video_list_response(wire)
                .map_err(|error| protocol_decode_error("video list", error))?;
            Ok(ProviderOutput::Result(Box::new(
                CanonicalResult::VideoList(result),
            )))
        }
        olp_domain::VideoOperation::Get(operation) => {
            let path = video_job_path(&operation.job_id, None)?;
            let bytes = connector
                .request_json(&request, Method::GET, &path, None)
                .await?;
            let wire: OpenAiVideoObject = parse_wire("video get", &bytes)?;
            let result = decode_video_object(wire)
                .map_err(|error| protocol_decode_error("video get", error))?;
            Ok(ProviderOutput::Result(Box::new(CanonicalResult::VideoJob(
                result,
            ))))
        }
        olp_domain::VideoOperation::Content(operation) => {
            let variant = operation
                .extensions
                .values
                .get("/variant")
                .and_then(serde_json::Value::as_str);
            let path = video_job_path(&operation.job_id, Some("content"))?;
            let path = variant.map_or(path.clone(), |variant| {
                format!("{path}?variant={}", percent_encode(variant))
            });
            let response = connector
                .request_raw(&request, Method::GET, &path, None, "*/*")
                .await?;
            let content_type = response
                .headers()
                .get(header::CONTENT_TYPE)
                .and_then(|value| value.to_str().ok())
                .and_then(|value| value.split(';').next())
                .map(str::trim)
                .filter(|value| value.starts_with("video/") || value.starts_with("image/"))
                .ok_or_else(|| {
                    protocol_body_error("OpenAI video content used an invalid content type")
                })?
                .to_owned();
            let spool = request.media.as_ref().ok_or_else(|| {
                protocol_body_error("OpenAI video content requires a bounded media spool")
            })?;
            let maximum = u64::try_from(connector.config.response_limits.response_bytes)
                .unwrap_or(u64::MAX);
            let artifact = spool_response_body(
                response,
                spool,
                format!("video-content-{}.bin", operation.job_id),
                Some(content_type),
                maximum,
                connector.config.timeouts.idle,
            )
            .await?;
            Ok(ProviderOutput::Result(Box::new(
                CanonicalResult::VideoContent(decode_video_content_body(
                    olp_protocols::openai::BinaryMediaBody { media: artifact },
                )),
            )))
        }
        olp_domain::VideoOperation::Delete(operation) => {
            let path = video_job_path(&operation.job_id, None)?;
            let reconcile_missing = operation
                .extensions
                .values
                .get(MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION)
                .and_then(serde_json::Value::as_bool)
                .unwrap_or(false);
            let response = connector
                .request_raw_unchecked(&request, Method::DELETE, &path, None, "application/json")
                .await?;
            if response.status() == StatusCode::NOT_FOUND && reconcile_missing {
                return Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::VideoDelete(olp_domain::VideoDeleteResult {
                        id: operation.job_id.clone(),
                        deleted: true,
                        extensions: olp_domain::SourceExtensions::new(
                            Surface::OpenAi,
                            std::collections::BTreeMap::new(),
                        ),
                    }),
                )));
            }
            if !response.status().is_success() {
                return Err(connector
                    .map_error_response(response.response, response.attempt_deadline)
                    .await);
            }
            require_content_type(&response, "application/json")?;
            let bytes = read_deadline_body(
                response,
                connector.config.timeouts.idle,
                connector.config.response_limits.response_bytes,
            )
            .await?;
            let wire: OpenAiVideoDeleteResponse = parse_wire("video delete", &bytes)?;
            Ok(ProviderOutput::Result(Box::new(
                CanonicalResult::VideoDelete(decode_video_delete_response(wire)),
            )))
        }
    }
}

fn video_job_path(job_id: &str, suffix: Option<&str>) -> Result<String, TransportError> {
    if job_id.is_empty()
        || job_id.len() > 256
        || !job_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(protocol_body_error("OpenAI video job ID is invalid"));
    }
    Ok(suffix.map_or_else(
        || format!("videos/{job_id}"),
        |suffix| format!("videos/{job_id}/{suffix}"),
    ))
}

fn percent_encode(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            encoded.push(char::from(byte));
        } else {
            encoded.push('%');
            encoded.push(char::from(HEX[usize::from(byte >> 4)]));
            encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
        }
    }
    encoded
}
