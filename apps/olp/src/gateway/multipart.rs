use std::{collections::BTreeMap, sync::Arc, time::Duration};

use axum::extract::Multipart;
use olp_engine::domain::{canonical::requests::MediaHandle, ports::MediaSpool};
use olp_engine::protocols::openai::media::BoundedMediaPart;
use serde_json::Value;

use crate::{
    gateway::state::GatewayState,
    public_http::request_admission::multipart::{
        MultipartRequestAdmission, MultipartRouteAdmission,
    },
};

use super::error::InferenceError;
use crate::public_http::streaming_response::channel_stream;

const UTF8_BOM: &[u8] = &[0xEF, 0xBB, 0xBF];

pub(super) struct MultipartFormData {
    text: BTreeMap<String, Vec<String>>,
    files: BTreeMap<String, Vec<BoundedMediaPart>>,
    cleanup_spool: Arc<dyn MediaSpool>,
    pub(super) cleanup_handles: Vec<MediaHandle>,
    cleanup_armed: bool,
    // The parser reservation stays attached to the staged media until it is
    // either handed to request execution or deleted. This prevents a failed
    // validation or cancelled request from freeing fixed upload capacity
    // while its temporary files still consume spool space.
    cleanup_admission: Option<MultipartRequestAdmission>,
}

impl MultipartFormData {
    pub(super) fn new(
        cleanup_spool: Arc<dyn MediaSpool>,
        cleanup_admission: MultipartRequestAdmission,
    ) -> Self {
        Self {
            text: BTreeMap::new(),
            files: BTreeMap::new(),
            cleanup_spool,
            cleanup_handles: Vec::new(),
            cleanup_armed: true,
            cleanup_admission: Some(cleanup_admission),
        }
    }

    pub(super) fn disarm_cleanup(&mut self) {
        self.cleanup_armed = false;
        // Execution now owns every request-media handle. Its reservation no
        // longer needs to cover parser cleanup.
        if let Some(admission) = self.cleanup_admission.take() {
            admission.release();
        }
    }

    /// Remove staged request media before returning a parser failure. This is
    /// deliberately cancellation-safe: a handle remains in the vector until
    /// its removal attempt returns, so `Drop` can retry any work interrupted
    /// by request cancellation.
    async fn cleanup(&mut self) {
        if !self.cleanup_armed {
            if let Some(admission) = self.cleanup_admission.take() {
                admission.release();
            }
            return;
        }
        while let Some(handle) = self.cleanup_handles.last().cloned() {
            match self.cleanup_spool.remove(&handle).await {
                Ok(()) | Err(olp_engine::domain::ports::MediaSpoolError::NotFound) => {
                    self.cleanup_handles.pop();
                }
                Err(_) => {
                    // Leave the handle and reservation armed. `Drop` will
                    // schedule a final best-effort deletion while retaining
                    // capacity until that task completes.
                    return;
                }
            }
        }
        self.cleanup_armed = false;
        if let Some(admission) = self.cleanup_admission.take() {
            admission.release();
        }
    }

    pub(super) fn required(&mut self, name: &str) -> Result<String, InferenceError> {
        self.optional(name)?.ok_or_else(|| {
            InferenceError::invalid_request(format!("The {name} field is required."))
        })
    }

    pub(super) fn optional(&mut self, name: &str) -> Result<Option<String>, InferenceError> {
        let Some(mut values) = self.text.remove(name) else {
            return Ok(None);
        };
        if values.len() != 1 {
            return Err(InferenceError::invalid_request(format!(
                "The {name} field must appear at most once."
            )));
        }
        Ok(values.pop())
    }

    pub(super) fn optional_parse<T>(&mut self, name: &str) -> Result<Option<T>, InferenceError>
    where
        T: std::str::FromStr,
    {
        self.optional(name)?
            .map(|value| {
                value.parse().map_err(|_| {
                    InferenceError::invalid_request(format!("The {name} field is invalid."))
                })
            })
            .transpose()
    }

    pub(super) fn take_repeated(&mut self, name: &str) -> Vec<String> {
        self.text
            .remove(name)
            .or_else(|| self.text.remove(&format!("{name}[]")))
            .unwrap_or_default()
    }

    pub(super) fn take_single_file(
        &mut self,
        name: &str,
    ) -> Result<Option<BoundedMediaPart>, InferenceError> {
        let Some(mut values) = self.files.remove(name) else {
            return Ok(None);
        };
        if values.len() != 1 {
            return Err(InferenceError::invalid_request(format!(
                "The {name} file must appear at most once."
            )));
        }
        Ok(values.pop())
    }

    pub(super) fn take_files_with_prefix(&mut self, prefix: &str) -> Vec<BoundedMediaPart> {
        let keys = self
            .files
            .keys()
            .filter(|name| *name == prefix || name.starts_with(&format!("{prefix}[")))
            .cloned()
            .collect::<Vec<_>>();
        keys.into_iter()
            .flat_map(|name| self.files.remove(&name).unwrap_or_default())
            .collect()
    }

    pub(super) fn take_extensions(&mut self) -> Result<BTreeMap<String, Value>, InferenceError> {
        if !self.files.is_empty() {
            return Err(InferenceError::invalid_request(
                "The multipart request contains an unsupported file field.",
            ));
        }
        std::mem::take(&mut self.text)
            .into_iter()
            .map(|(name, values)| {
                if values.len() != 1 {
                    return Err(InferenceError::invalid_request(format!(
                        "The unsupported {name} field cannot be repeated."
                    )));
                }
                Ok((
                    name,
                    Value::String(values.into_iter().next().unwrap_or_default()),
                ))
            })
            .collect()
    }
}

impl Drop for MultipartFormData {
    fn drop(&mut self) {
        if !self.cleanup_armed || self.cleanup_handles.is_empty() {
            return;
        }
        let spool = Arc::clone(&self.cleanup_spool);
        let handles = std::mem::take(&mut self.cleanup_handles);
        // Move the final lease owner into the detached cleanup task. On
        // cancellation, request-owned copies of the extension can disappear
        // immediately, but the semaphore reservation remains until these
        // staged artifacts have had their deletion attempts.
        let admission = self.cleanup_admission.take();
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                for handle in handles {
                    let _ = spool.remove(&handle).await;
                }
                if let Some(admission) = admission {
                    admission.release();
                }
            });
        }
    }
}

const MULTIPART_TOTAL_DEADLINE: Duration = Duration::from_secs(5 * 60);
const MAX_MULTIPART_TEXT_FIELD_BYTES: usize = 64 * 1024;
const MAX_MULTIPART_TEXT_TOTAL_BYTES: usize = 512 * 1024;

pub(super) async fn parse_multipart(
    state: &GatewayState,
    multipart: Multipart,
    maximum_file_bytes: u64,
    maximum_files: usize,
    admission: MultipartRequestAdmission,
) -> Result<MultipartFormData, InferenceError> {
    // This deadline deliberately covers the entire parser lifetime. The
    // existing request-body timeout protects stalled reads; without this
    // non-resetting cap, a peer that continues to trickle valid frames could
    // occupy an admission reservation indefinitely.
    // Keep ownership of the cleanup guard outside the timed parser future.
    // That lets timeout and parser-error paths synchronously remove any
    // completed staged files before their fixed admission reservation is
    // released back to another untrusted upload. On success it transfers the
    // reservation to the form, where it remains until execution takes the
    // media or cleanup finishes.
    let route_admission = admission.route.clone();
    let mut output = MultipartFormData::new(state.media_spool().clone(), admission);
    let result = tokio::time::timeout(
        MULTIPART_TOTAL_DEADLINE,
        parse_multipart_fields(
            state,
            multipart,
            maximum_file_bytes,
            maximum_files,
            &route_admission,
            &mut output,
        ),
    )
    .await;
    match result {
        Ok(Ok(())) => Ok(output),
        Ok(Err(error)) => {
            output.cleanup().await;
            Err(error)
        }
        Err(_) => {
            output.cleanup().await;
            Err(InferenceError::multipart_parser_timeout())
        }
    }
}

async fn store_multipart_file(
    state: &GatewayState,
    field: &mut axum::extract::multipart::Field<'_>,
    filename: String,
    content_type: Option<String>,
    name: String,
    maximum_file_bytes: u64,
    output: &mut MultipartFormData,
) -> Result<(), InferenceError> {
    let (sender, receiver) = tokio::sync::mpsc::channel(8);
    let stream = channel_stream(receiver);
    let put = state
        .media_spool()
        .put(olp_engine::domain::ports::MediaUpload {
            filename: filename.clone(),
            content_type: content_type.clone(),
            maximum_length: maximum_file_bytes,
            bytes: Box::pin(stream),
        });
    let produce = async move {
        while let Some(chunk) = field.chunk().await.transpose() {
            match chunk {
                Ok(chunk) => {
                    if sender.send(Ok(chunk)).await.is_err() {
                        return Ok::<(), InferenceError>(());
                    }
                }
                Err(error) => {
                    let _ = sender
                        .send(Err(olp_engine::domain::ports::MediaSpoolError::Unavailable))
                        .await;
                    return Err(InferenceError::invalid_request(format!(
                        "The multipart file is invalid: {error}"
                    )));
                }
            }
        }
        Ok(())
    };
    let (artifact, produced) = tokio::join!(put, produce);
    let artifact = match (artifact, produced) {
        (Ok(artifact), Ok(())) => artifact,
        (Ok(artifact), Err(error)) => {
            output.cleanup_handles.push(artifact.handle);
            return Err(error);
        }
        (Err(_), Err(error)) => return Err(error),
        (Err(error), Ok(())) => return Err(media_spool_error(error)),
    };
    output.cleanup_handles.push(artifact.handle.clone());
    let part = BoundedMediaPart::new(
        artifact.handle,
        filename,
        content_type,
        artifact.content_length.unwrap_or_default(),
        maximum_file_bytes,
    )
    .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    output.files.entry(name).or_default().push(part);
    Ok(())
}

async fn store_multipart_text(
    field: &mut axum::extract::multipart::Field<'_>,
    name: String,
    admission: &MultipartRouteAdmission,
    output: &mut MultipartFormData,
    text_bytes: &mut usize,
    authorized_model_seen: &mut bool,
) -> Result<(), InferenceError> {
    let mut bytes = Vec::new();
    while let Some(chunk) = field.chunk().await.map_err(|error| {
        InferenceError::invalid_request(format!("The multipart field is invalid: {error}"))
    })? {
        let next_field = bytes
            .len()
            .checked_add(chunk.len())
            .filter(|length| *length <= MAX_MULTIPART_TEXT_FIELD_BYTES)
            .ok_or_else(|| {
                InferenceError::invalid_request("A multipart text field exceeded 64 KiB.")
            })?;
        let next_total = text_bytes
            .checked_add(chunk.len())
            .filter(|length| *length <= MAX_MULTIPART_TEXT_TOTAL_BYTES)
            .ok_or_else(|| {
                InferenceError::invalid_request(
                    "Multipart text fields exceeded the 512 KiB aggregate limit.",
                )
            })?;
        bytes
            .try_reserve(chunk.len())
            .map_err(|_| InferenceError::unavailable("multipart_text_allocation_unavailable"))?;
        bytes.extend_from_slice(&chunk);
        debug_assert_eq!(bytes.len(), next_field);
        *text_bytes = next_total;
    }
    let text = String::from_utf8(bytes.strip_prefix(UTF8_BOM).unwrap_or(&bytes).to_vec()).map_err(
        |_| {
            InferenceError::invalid_request(format!(
                "The multipart field {name} is not valid UTF-8."
            ))
        },
    )?;
    if name == "model" {
        match admission {
            MultipartRouteAdmission::Expected(expected) if text != expected.as_str() => {
                return Err(InferenceError::invalid_request(
                    "X-OLP-Route must match the multipart model field.",
                ));
            }
            MultipartRouteAdmission::RequireAuthorizedModel(allowed_routes) => {
                let route =
                    olp_engine::domain::ids::RouteSlug::parse(text.as_str()).map_err(|_| {
                        InferenceError::invalid_request(
                            "The model field must contain a valid authorized route.",
                        )
                    })?;
                if !allowed_routes.contains(&route) {
                    return Err(InferenceError::forbidden(
                        "The API key is not authorized for the multipart model route.".to_owned(),
                    ));
                }
                *authorized_model_seen = true;
            }
            MultipartRouteAdmission::Expected(_) | MultipartRouteAdmission::Unrestricted => {
                *authorized_model_seen = true;
            }
        }
    }
    output.text.entry(name).or_default().push(text);
    Ok(())
}

async fn parse_multipart_fields(
    state: &GatewayState,
    mut multipart: Multipart,
    maximum_file_bytes: u64,
    maximum_files: usize,
    admission: &MultipartRouteAdmission,
    output: &mut MultipartFormData,
) -> Result<(), InferenceError> {
    let mut field_count = 0_usize;
    let mut file_count = 0_usize;
    let mut text_bytes = 0_usize;
    let mut authorized_model_seen = false;
    while let Some(mut field) = multipart.next_field().await.map_err(|error| {
        InferenceError::invalid_request(format!("The multipart request is invalid: {error}"))
    })? {
        field_count = field_count.saturating_add(1);
        if field_count > 128 {
            return Err(InferenceError::invalid_request(
                "The multipart request contains too many fields.",
            ));
        }
        let name = field
            .name()
            .filter(|name| !name.is_empty())
            .ok_or_else(|| InferenceError::invalid_request("A multipart field has no name."))?
            .to_owned();
        if let Some(filename) = field.file_name().map(str::to_owned) {
            file_count = file_count.saturating_add(1);
            if file_count > maximum_files {
                return Err(InferenceError::invalid_request(
                    "The multipart request contains too many files.",
                ));
            }
            let content_type = field.content_type().map(str::to_owned);
            store_multipart_file(
                state,
                &mut field,
                filename,
                content_type,
                name,
                maximum_file_bytes,
                output,
            )
            .await?;
        } else {
            store_multipart_text(
                &mut field,
                name,
                admission,
                output,
                &mut text_bytes,
                &mut authorized_model_seen,
            )
            .await?;
        }
    }
    if admission.requires_authorized_model() && !authorized_model_seen {
        // Every spooled file is already registered for cleanup by the caller.
        return Err(InferenceError::invalid_request(
            "A route-restricted multipart request must include a model field naming an \
             authorized route.",
        ));
    }
    Ok(())
}

pub(super) fn media_spool_error(
    error: olp_engine::domain::ports::MediaSpoolError,
) -> InferenceError {
    match error {
        olp_engine::domain::ports::MediaSpoolError::TooLarge { .. } => {
            InferenceError::payload_too_large("media_too_large")
        }
        olp_engine::domain::ports::MediaSpoolError::InvalidFilename
        | olp_engine::domain::ports::MediaSpoolError::InvalidHandle
        | olp_engine::domain::ports::MediaSpoolError::ZeroLimit => {
            InferenceError::invalid_request(error.to_string())
        }
        olp_engine::domain::ports::MediaSpoolError::NotFound
        | olp_engine::domain::ports::MediaSpoolError::Unavailable => {
            InferenceError::unavailable("media_spool_unavailable")
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use olp_engine::domain::{
        canonical::results::MediaArtifact,
        ports::{BoxFuture, MediaSpoolError, MediaUpload, OpenedMedia},
    };

    struct UnusedSpool;

    impl MediaSpool for UnusedSpool {
        fn put(&self, _: MediaUpload) -> BoxFuture<'_, Result<MediaArtifact, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            _: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }

        fn remove<'a>(&'a self, _: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
            Box::pin(async { Err(MediaSpoolError::Unavailable) })
        }
    }

    fn form() -> MultipartFormData {
        MultipartFormData::new(
            Arc::new(UnusedSpool),
            MultipartRequestAdmission::unrestricted(),
        )
    }

    fn file(name: &str) -> BoundedMediaPart {
        BoundedMediaPart::new(
            MediaHandle::new(format!("{name}-handle")),
            name,
            Some("application/octet-stream".to_owned()),
            3,
            10,
        )
        .unwrap()
    }

    #[test]
    fn text_fields_enforce_cardinality_parsing_and_array_aliases() {
        let mut data = form();
        assert_eq!(data.optional("missing").unwrap(), None);
        assert_eq!(
            data.required("model").unwrap_err().message(),
            "The model field is required."
        );

        data.text.insert("count".to_owned(), vec!["7".to_owned()]);
        assert_eq!(data.optional_parse::<u16>("count").unwrap(), Some(7));
        data.text
            .insert("count".to_owned(), vec!["invalid".to_owned()]);
        assert_eq!(
            data.optional_parse::<u16>("count").unwrap_err().message(),
            "The count field is invalid."
        );
        data.text.insert(
            "model".to_owned(),
            vec!["first".to_owned(), "second".to_owned()],
        );
        assert_eq!(
            data.optional("model").unwrap_err().message(),
            "The model field must appear at most once."
        );

        data.text.insert(
            "include[]".to_owned(),
            vec!["usage".to_owned(), "logprobs".to_owned()],
        );
        assert_eq!(data.take_repeated("include"), ["usage", "logprobs"]);
        assert!(data.take_repeated("include").is_empty());
    }

    #[test]
    fn file_fields_are_selected_without_consuming_unrelated_uploads() {
        let mut data = form();
        assert!(data.take_single_file("missing").unwrap().is_none());

        data.files.insert("mask".to_owned(), vec![file("mask")]);
        assert_eq!(
            data.take_single_file("mask").unwrap().unwrap().filename,
            "mask"
        );
        data.files
            .insert("mask".to_owned(), vec![file("first"), file("second")]);
        assert_eq!(
            data.take_single_file("mask").unwrap_err().message(),
            "The mask file must appear at most once."
        );

        data.files.insert("image".to_owned(), vec![file("base")]);
        data.files
            .insert("image[1]".to_owned(), vec![file("second")]);
        data.files
            .insert("unrelated".to_owned(), vec![file("other")]);
        let selected = data.take_files_with_prefix("image");
        assert_eq!(
            selected
                .iter()
                .map(|part| part.filename.as_str())
                .collect::<Vec<_>>(),
            ["base", "second"]
        );
        assert_eq!(
            data.files.keys().map(String::as_str).collect::<Vec<_>>(),
            ["unrelated"]
        );
    }

    #[test]
    fn extensions_accept_single_text_values_and_reject_ambiguous_remainders() {
        let mut data = form();
        data.text
            .insert("vendor".to_owned(), vec!["value".to_owned()]);
        assert_eq!(
            data.take_extensions().unwrap(),
            BTreeMap::from([("vendor".to_owned(), Value::String("value".to_owned()))])
        );

        let mut repeated = form();
        repeated
            .text
            .insert("vendor".to_owned(), vec!["a".to_owned(), "b".to_owned()]);
        assert_eq!(
            repeated.take_extensions().unwrap_err().message(),
            "The unsupported vendor field cannot be repeated."
        );

        let mut with_file = form();
        with_file
            .files
            .insert("unexpected".to_owned(), vec![file("payload")]);
        assert_eq!(
            with_file.take_extensions().unwrap_err().message(),
            "The multipart request contains an unsupported file field."
        );
    }

    #[test]
    fn spool_failures_map_to_stable_public_error_classes() {
        let cases = [
            (MediaSpoolError::TooLarge { maximum: 1 }, "media_too_large"),
            (MediaSpoolError::InvalidFilename, "invalid_request"),
            (MediaSpoolError::InvalidHandle, "invalid_request"),
            (MediaSpoolError::ZeroLimit, "invalid_request"),
            (MediaSpoolError::NotFound, "media_spool_unavailable"),
            (MediaSpoolError::Unavailable, "media_spool_unavailable"),
        ];
        for (error, code) in cases {
            assert_eq!(media_spool_error(error).code(), code);
        }
    }
}
