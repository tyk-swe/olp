use std::collections::VecDeque;

use olp_domain::{
    CanonicalResult, Operation, ProviderOutput, ProviderRequest, TransportError, TransportMode,
};
use olp_protocols::openai::{
    OpenAiImageResponse, encode_image_edit, encode_image_generation, encode_image_variation,
};
use reqwest::multipart;

use super::{
    super::{OpenAiConnector, errors::*, media::*, streams::read_deadline_body},
    require_content_type,
};

pub(super) async fn execute(
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let Operation::Images(operation) = &request.operation else {
        unreachable!("checked by caller")
    };
    let (path, body) = match operation {
        olp_domain::ImageOperation::Generation(operation) => {
            let wire = encode_image_generation(operation, &request.attempt.upstream_model)
                .map_err(|error| protocol_encode_error("image generation", error))?;
            (
                "images/generations",
                serialize_wire("image generation", &wire)?,
            )
        }
        olp_domain::ImageOperation::Edit(_) | olp_domain::ImageOperation::Variation(_) => {
            return execute_multipart(connector, request).await;
        }
    };
    let response = connector.post_raw_json(&request, path, body).await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(
            connector.raw_sse_response(response)?,
        ));
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
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    let spool = request
        .media
        .as_ref()
        .ok_or_else(|| protocol_body_error("OpenAI image uploads require a bounded media spool"))?;
    let Operation::Images(operation) = &request.operation else {
        unreachable!("checked by caller")
    };
    let mut form = multipart::Form::new();
    let path;
    match operation {
        olp_domain::ImageOperation::Edit(operation) => {
            let mut parts = VecDeque::new();
            for handle in operation.images.iter().chain(operation.mask.iter()) {
                parts.push_back(bounded_part(spool.as_ref(), handle, 50 * 1024 * 1024).await?);
            }
            let wire = encode_image_edit(operation, &request.attempt.upstream_model, |_| {
                parts.pop_front().ok_or_else(|| {
                    olp_protocols::openai::ImageCodecError::InvalidMediaPart(
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
        olp_domain::ImageOperation::Variation(operation) => {
            let metadata = bounded_part(spool.as_ref(), &operation.image, 50 * 1024 * 1024).await?;
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
        olp_domain::ImageOperation::Generation(_) => {
            unreachable!("generation uses JSON transport")
        }
    }
    let response = connector.post_multipart_raw(&request, path, form).await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(
            connector.raw_sse_response(response)?,
        ));
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
