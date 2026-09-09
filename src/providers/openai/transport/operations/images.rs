use std::collections::VecDeque;

use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::TransportError;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::ImageOperation;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::openai::images::DEFAULT_IMAGE_UPLOAD_LIMIT;
use crate::protocols::openai::images::OpenAiImageResponse;
use crate::protocols::openai::images::encode_image_edit;
use crate::protocols::openai::images::encode_image_generation;
use crate::protocols::openai::images::encode_image_variation;
use reqwest::multipart;

use crate::providers::openai::transport::Connector;
use crate::providers::openai::transport::errors::*;
use crate::providers::openai::transport::media::*;
use crate::providers::openai::transport::streams::read_deadline_body;
use crate::providers::openai::transport::streams::require_content_type;
use crate::providers::transport_common::protocol_body_error;

pub(crate) async fn execute(
    connector: &Connector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    // The operation dispatcher routes only image requests into this module.
    let Operation::Images(operation) = &*request.operation else {
        unreachable!("checked by caller")
    };
    let (path, body) = match operation {
        ImageOperation::Generation(operation) => {
            let wire = encode_image_generation(operation, &request.attempt.upstream_model)
                .map_err(|error| protocol_encode_error("image generation", error))?;
            (
                "images/generations",
                serialize_wire("image generation", &wire)?,
            )
        }
        ImageOperation::Edit(_) | ImageOperation::Variation(_) => {
            return execute_multipart(connector, &request, operation).await;
        }
    };
    let response = connector.post_raw_json(&request, path, body).await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(connector.raw_sse_response(response)));
    }
    require_content_type(&response, "application/json")?;
    let bytes = read_deadline_body(
        response,
        connector.config.timeouts.idle,
        connector.config.max_response_bytes,
    )
    .await?;
    let wire: OpenAiImageResponse = parse_wire("image", &bytes)?;
    let result = connector.decode_image_result(&request, wire).await?;
    Ok(ProviderOutput::Result(Box::new(CanonicalResult::Images(
        result,
    ))))
}

async fn execute_multipart(
    connector: &Connector,
    request: &ProviderRequest,
    operation: &ImageOperation,
) -> Result<ProviderOutput, TransportError> {
    let spool = request
        .media
        .as_ref()
        .ok_or_else(|| protocol_body_error("OpenAI image uploads require a bounded media spool"))?;
    let mut form = multipart::Form::new();
    let path;
    match operation {
        ImageOperation::Edit(operation) => {
            let mut parts = VecDeque::new();
            for handle in operation.images.iter().chain(operation.mask.iter()) {
                parts.push_back(
                    bounded_part(spool.as_ref(), handle, DEFAULT_IMAGE_UPLOAD_LIMIT).await?,
                );
            }
            let wire = encode_image_edit(operation, &request.attempt.upstream_model, |_| {
                parts.pop_front().ok_or_else(|| {
                    crate::protocols::openai::images::ImageCodecError::InvalidMediaPart(
                        "media spool metadata was unavailable".into(),
                    )
                })
            })
            .map_err(|error| protocol_encode_error("image edit", error))?;
            form = form
                .text("model", wire.model.clone())
                .text("prompt", wire.prompt.clone());
            for (index, handle) in operation.images.iter().enumerate() {
                let opened = spool.open(handle).await.map_err(map_spool_error)?;
                let field = if operation.images.len() == 1 {
                    "image".to_owned()
                } else {
                    format!("image[{index}]")
                };
                form = form.part(field, multipart_part(opened)?);
            }
            if let Some(mask) = &operation.mask {
                form = form.part(
                    "mask",
                    multipart_part(spool.open(mask).await.map_err(map_spool_error)?)?,
                );
            }
            form = add_image_edit_fields(form, &wire);
            path = "images/edits";
        }
        ImageOperation::Variation(operation) => {
            let metadata =
                bounded_part(spool.as_ref(), &operation.image, DEFAULT_IMAGE_UPLOAD_LIMIT).await?;
            let wire = encode_image_variation(operation, &request.attempt.upstream_model, |_| {
                Ok(metadata.clone())
            })
            .map_err(|error| protocol_encode_error("image variation", error))?;
            form = form.text("model", wire.model).part(
                "image",
                multipart_part(
                    spool
                        .open(&operation.image)
                        .await
                        .map_err(map_spool_error)?,
                )?,
            );
            form = add_optional_text(form, "n", wire.n.map(|value| value.to_string()));
            form = add_optional_text(form, "size", wire.size);
            form = add_optional_text(form, "response_format", wire.response_format);
            form = add_optional_text(form, "user", wire.user);
            form = add_extra_fields(form, wire.extra);
            path = "images/variations";
        }
        // The outer image dispatcher sends generation through the JSON path.
        ImageOperation::Generation(_) => {
            unreachable!("generation uses JSON transport")
        }
    }
    let response = connector.post_multipart_raw(&request, path, form).await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(connector.raw_sse_response(response)));
    }
    require_content_type(&response, "application/json")?;
    let response = read_deadline_body(
        response,
        connector.config.timeouts.idle,
        connector.config.max_response_bytes,
    )
    .await?;
    let wire: OpenAiImageResponse = parse_wire("image", &response)?;
    let result = connector.decode_image_result(&request, wire).await?;
    Ok(ProviderOutput::Result(Box::new(CanonicalResult::Images(
        result,
    ))))
}
