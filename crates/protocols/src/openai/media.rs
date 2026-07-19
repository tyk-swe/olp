use olp_domain::{MediaArtifact, MediaHandle};
use serde::{Deserialize, Serialize};
use thiserror::Error;

/// A file part that has already been admitted into a bounded spool.
///
/// The protocol layer carries only an opaque handle and metadata. HTTP and
/// connector adapters are responsible for streaming the bytes into and out of
/// the spool while enforcing the declared limit.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct BoundedMediaPart {
    pub handle: MediaHandle,
    pub filename: String,
    pub content_type: Option<String>,
    pub content_length: u64,
    pub maximum_length: u64,
}

impl BoundedMediaPart {
    pub fn new(
        handle: MediaHandle,
        filename: impl Into<String>,
        content_type: Option<String>,
        content_length: u64,
        maximum_length: u64,
    ) -> Result<Self, MediaPartError> {
        let filename = filename.into();
        if filename.trim().is_empty() {
            return Err(MediaPartError::EmptyFilename);
        }
        if maximum_length == 0 {
            return Err(MediaPartError::ZeroLimit);
        }
        if content_length > maximum_length {
            return Err(MediaPartError::TooLarge {
                actual: content_length,
                maximum: maximum_length,
            });
        }
        Ok(Self {
            handle,
            filename,
            content_type,
            content_length,
            maximum_length,
        })
    }

    #[must_use]
    pub fn artifact(&self) -> MediaArtifact {
        MediaArtifact {
            handle: self.handle.clone(),
            content_type: self.content_type.clone(),
            content_length: Some(self.content_length),
        }
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum MediaPartError {
    #[error("multipart file name cannot be empty")]
    EmptyFilename,
    #[error("multipart file limit must be greater than zero")]
    ZeroLimit,
    #[error("multipart file is {actual} bytes, exceeding the {maximum}-byte limit")]
    TooLarge { actual: u64, maximum: u64 },
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct BinaryMediaBody {
    pub media: MediaArtifact,
}
