use std::{collections::BTreeMap, sync::Arc, time::Duration};

use axum::extract::Multipart;
use encoding_rs::{Encoding, UTF_8};
use futures::stream;
use olp_domain::{MediaHandle, MediaSpool};
use olp_protocols::openai::BoundedMediaPart;
use serde_json::Value;

use crate::{GatewayState, MultipartRequestAdmission, MultipartRouteAdmission};

use super::error::InferenceError;

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
                Ok(()) | Err(olp_domain::MediaSpoolError::NotFound) => {
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

    pub(super) fn take_repeated(&mut self, name: &str) -> Result<Vec<String>, InferenceError> {
        let plain = self.text.remove(name);
        let bracketed = self.text.remove(&format!("{name}[]"));
        if plain.is_some() && bracketed.is_some() {
            return Err(InferenceError::invalid_request(format!(
                "The {name} field must not mix bracketed and unbracketed forms."
            )));
        }
        Ok(plain.or(bracketed).unwrap_or_default())
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

    pub(super) fn take_files_with_prefix(
        &mut self,
        prefix: &str,
    ) -> Result<Vec<BoundedMediaPart>, InferenceError> {
        let plain = self.files.remove(prefix);
        let bracketed_name = format!("{prefix}[]");
        let bracketed = self.files.remove(&bracketed_name);
        let mut indexed = self
            .files
            .keys()
            .filter_map(|name| multipart_index(prefix, name).map(|index| (index, name.clone())))
            .collect::<Vec<_>>();
        let forms = usize::from(plain.is_some())
            + usize::from(bracketed.is_some())
            + usize::from(!indexed.is_empty());
        if forms > 1 {
            return Err(InferenceError::invalid_request(format!(
                "The {prefix} file must not mix indexed, bracketed, and unbracketed forms."
            )));
        }
        if let Some(files) = plain.or(bracketed) {
            return Ok(files);
        }
        indexed.sort_unstable_by_key(|(index, _)| *index);
        let mut output = Vec::with_capacity(indexed.len());
        let mut previous = None;
        for (index, name) in indexed {
            if previous == Some(index) {
                return Err(InferenceError::invalid_request(format!(
                    "The {prefix} file contains a duplicate index."
                )));
            }
            previous = Some(index);
            let mut files = self.files.remove(&name).unwrap_or_default();
            if files.len() != 1 {
                return Err(InferenceError::invalid_request(format!(
                    "The indexed {prefix} file must appear exactly once."
                )));
            }
            output.push(files.pop().expect("one indexed multipart file"));
        }
        Ok(output)
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

fn multipart_index(prefix: &str, name: &str) -> Option<usize> {
    let index = name
        .strip_prefix(prefix)?
        .strip_prefix('[')?
        .strip_suffix(']')?;
    (!index.is_empty() && index.bytes().all(|byte| byte.is_ascii_digit()))
        .then(|| index.parse().ok())
        .flatten()
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
    let mut output = MultipartFormData::new(state.media_spool.clone(), admission);
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
            if admission.requires_model_before_file() && !authorized_model_seen {
                return Err(InferenceError::invalid_request(
                    "A route-restricted multipart request must send model before any file part.",
                ));
            }
            file_count = file_count.saturating_add(1);
            if file_count > maximum_files {
                return Err(InferenceError::invalid_request(
                    "The multipart request contains too many files.",
                ));
            }
            let content_type = field.content_type().map(str::to_owned);
            let (sender, receiver) = tokio::sync::mpsc::channel(8);
            let stream = stream::unfold(receiver, |mut receiver| async move {
                receiver.recv().await.map(|item| (item, receiver))
            });
            let put = state.media_spool.put(olp_domain::MediaUpload {
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
                                .send(Err(olp_domain::MediaSpoolError::Unavailable))
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
                    // The spool may have completed while the producer noticed
                    // malformed input. Register it before returning so the
                    // outer parser cleanup (and cancellation-safe `Drop`)
                    // owns it even if this request is aborted immediately.
                    output.cleanup_handles.push(artifact.handle);
                    return Err(error);
                }
                // A malformed multipart body is a client error even if the
                // spool was told to stop by the producer. Prefer that
                // original parser error over the expected receiver-side
                // `Unavailable` result from `put`.
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
        } else {
            // Match multer's documented `text()` behavior (charset selection,
            // BOM sniffing, replacement of malformed sequences), but bound
            // the raw bytes before growing an allocation.
            let charset = field
                .content_type()
                .and_then(|content_type| content_type.parse::<mime::Mime>().ok())
                .and_then(|content_type| {
                    content_type
                        .get_param("charset")
                        .map(|value| value.as_str().to_owned())
                });
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
                bytes.try_reserve(chunk.len()).map_err(|_| {
                    InferenceError::unavailable("multipart_text_allocation_unavailable")
                })?;
                bytes.extend_from_slice(&chunk);
                debug_assert_eq!(bytes.len(), next_field);
                text_bytes = next_total;
            }
            let encoding = charset
                .as_deref()
                .and_then(|label| Encoding::for_label(label.as_bytes()))
                .unwrap_or(UTF_8);
            let text = encoding.decode(&bytes).0.into_owned();
            if name == "model" {
                match admission {
                    MultipartRouteAdmission::Expected(expected) if text != expected.as_str() => {
                        return Err(InferenceError::invalid_request(
                            "X-OLP-Route must match the multipart model field.",
                        ));
                    }
                    MultipartRouteAdmission::RequireModelBeforeFile(allowed_routes) => {
                        let route = olp_domain::RouteSlug::parse(text.as_str()).map_err(|_| {
                            InferenceError::invalid_request(
                                "The model field must contain a valid authorized route before file parts.",
                            )
                        })?;
                        if !allowed_routes.contains(&route) {
                            return Err(InferenceError::forbidden(
                                "The API key is not authorized for the multipart model route."
                                    .to_owned(),
                            ));
                        }
                        authorized_model_seen = true;
                    }
                    MultipartRouteAdmission::Expected(_)
                    | MultipartRouteAdmission::Unrestricted => {
                        authorized_model_seen = true;
                    }
                }
            }
            output.text.entry(name).or_default().push(text);
        }
    }
    Ok(())
}

pub(super) fn media_spool_error(error: olp_domain::MediaSpoolError) -> InferenceError {
    match error {
        olp_domain::MediaSpoolError::TooLarge { .. } => {
            InferenceError::payload_too_large("media_too_large")
        }
        olp_domain::MediaSpoolError::InvalidFilename
        | olp_domain::MediaSpoolError::InvalidHandle
        | olp_domain::MediaSpoolError::ZeroLimit => {
            InferenceError::invalid_request(error.to_string())
        }
        olp_domain::MediaSpoolError::NotFound | olp_domain::MediaSpoolError::Unavailable => {
            InferenceError::unavailable("media_spool_unavailable")
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn form() -> MultipartFormData {
        MultipartFormData::new(
            crate::media_spool::FileMediaSpool::create().unwrap(),
            MultipartRequestAdmission::unrestricted(),
        )
    }

    fn file(index: usize) -> BoundedMediaPart {
        BoundedMediaPart::new(
            MediaHandle::new(index.to_string()),
            format!("{index}.png"),
            Some("image/png".to_owned()),
            1,
            10,
        )
        .unwrap()
    }

    #[test]
    fn repeated_aliases_cannot_be_mixed() {
        let mut form = form();
        form.text.insert("include".to_owned(), vec!["a".to_owned()]);
        form.text
            .insert("include[]".to_owned(), vec!["b".to_owned()]);
        assert!(form.take_repeated("include").is_err());
    }

    #[test]
    fn indexed_files_require_exact_names_and_sort_numerically() {
        let mut form = form();
        form.files.insert("image[10]".to_owned(), vec![file(10)]);
        form.files.insert("image[2]".to_owned(), vec![file(2)]);
        form.files
            .insert("image[0]suffix".to_owned(), vec![file(0)]);

        let files = form.take_files_with_prefix("image").unwrap();
        assert_eq!(
            files
                .iter()
                .map(|file| file.handle.as_str())
                .collect::<Vec<_>>(),
            ["2", "10"]
        );
        assert!(form.take_extensions().is_err());
    }

    #[test]
    fn numerically_duplicate_file_indices_are_rejected() {
        let mut form = form();
        form.files.insert("image[1]".to_owned(), vec![file(1)]);
        form.files.insert("image[01]".to_owned(), vec![file(1)]);
        assert!(form.take_files_with_prefix("image").is_err());
    }
}
