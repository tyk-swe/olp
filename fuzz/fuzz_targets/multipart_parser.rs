#![no_main]

//! Oracles for multipart ingestion and the bounded media spool.
//!
//! The parser half is a smoke path — malformed framing must not panic. The
//! spool half carries real invariants: a successfully stored artifact must be
//! readable, must return exactly the bytes that were streamed into it, must
//! declare a length that matches those bytes, and must be unreadable once
//! removed. A spool that silently truncates an upload or leaves a handle
//! readable after removal is a correctness and retention defect, and neither
//! shows up as a panic.

use std::{convert::Infallible, sync::LazyLock};

use bytes::Bytes;
use futures::{StreamExt as _, stream};
use libfuzzer_sys::fuzz_target;
use olp_domain::{MediaSpoolError, MediaUpload};

const MAXIMUM_FILE_BYTES: u64 = 2 * 1024 * 1024;

static RUNTIME: LazyLock<tokio::runtime::Runtime> = LazyLock::new(|| {
    tokio::runtime::Builder::new_current_thread()
        .build()
        .expect("fuzz runtime must start")
});

async fn drive(payload: Vec<u8>) {
    let Ok(spool) = olp::test_support::create_bounded_media_spool_for_test() else {
        return;
    };
    let source = stream::once(async move { Ok::<Bytes, Infallible>(Bytes::from(payload)) });
    let mut multipart = multer::Multipart::new(source, "olp-fuzz-boundary");
    let mut fields = 0_usize;
    loop {
        let Ok(next) = multipart.next_field().await else {
            return;
        };
        let Some(mut field) = next else {
            return;
        };
        fields += 1;
        if fields > 128 {
            return;
        }
        if let Some(filename) = field.file_name().map(str::to_owned) {
            let content_type = field.content_type().map(ToString::to_string);
            let (sender, receiver) = tokio::sync::mpsc::channel(8);
            let bytes = stream::unfold(receiver, |mut receiver| async move {
                receiver.recv().await.map(|item| (item, receiver))
            });
            let put = spool.put(MediaUpload {
                filename,
                content_type,
                maximum_length: MAXIMUM_FILE_BYTES,
                bytes: Box::pin(bytes),
            });
            // Keep a copy of everything actually handed to the spool. `None`
            // means the upload was cut short, so the stored bytes are
            // legitimately undefined and fidelity cannot be asserted.
            let produce = async move {
                let mut sent = Vec::new();
                while let Some(chunk) = field.chunk().await.transpose() {
                    let Ok(bytes) = chunk else {
                        let _ = sender.send(Err(MediaSpoolError::Unavailable)).await;
                        return None;
                    };
                    sent.extend_from_slice(&bytes);
                    if sender.send(Ok(bytes)).await.is_err() {
                        return None;
                    }
                }
                Some(sent)
            };
            let (artifact, sent) = tokio::join!(put, produce);
            let Ok(artifact) = artifact else {
                continue;
            };

            if let Some(expected) = sent {
                if let Some(declared) = artifact.content_length {
                    assert_eq!(
                        declared,
                        u64::try_from(expected.len()).unwrap_or(u64::MAX),
                        "stored artifact declares a length that disagrees with the bytes written"
                    );
                }
                let mut opened = spool
                    .open(&artifact.handle)
                    .await
                    .expect("a successfully stored artifact must be readable");
                let mut read_back = Vec::new();
                while let Some(chunk) = opened.bytes.next().await {
                    let chunk = chunk.expect("reading back a stored artifact must not fail");
                    read_back.extend_from_slice(&chunk);
                }
                assert_eq!(
                    read_back, expected,
                    "the spool returned different bytes than were streamed into it"
                );
            }

            if spool.remove(&artifact.handle).await.is_ok() {
                assert!(
                    spool.open(&artifact.handle).await.is_err(),
                    "handle remained readable after the artifact was removed"
                );
            }
        } else {
            let mut bytes = 0_usize;
            while let Ok(Some(chunk)) = field.chunk().await {
                bytes = bytes.saturating_add(chunk.len());
                if bytes > 64 * 1024 {
                    return;
                }
            }
        }
    }
}

fuzz_target!(|data: &[u8]| {
    // Raw bodies exercise malformed framing. A framed body drives valid field
    // parsing while treating the fuzzer bytes as an arbitrary streamed value.
    RUNTIME.block_on(drive(data.to_vec()));
    let mut framed = b"--olp-fuzz-boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"fuzz.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n".to_vec();
    framed.extend_from_slice(data);
    framed.extend_from_slice(b"\r\n--olp-fuzz-boundary--\r\n");
    RUNTIME.block_on(drive(framed));
});
