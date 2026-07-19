#![no_main]

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
    let Ok(spool) = olp::create_bounded_media_spool_for_test() else {
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
            let produce = async move {
                while let Some(chunk) = field.chunk().await.transpose() {
                    let item = chunk.map_err(|_| MediaSpoolError::Unavailable);
                    if sender.send(item).await.is_err() {
                        break;
                    }
                }
            };
            let (artifact, ()) = tokio::join!(put, produce);
            if let Ok(artifact) = artifact {
                if let Ok(mut opened) = spool.open(&artifact.handle).await {
                    let mut read = 0_u64;
                    while let Some(Ok(chunk)) = opened.bytes.next().await {
                        read = read.saturating_add(u64::try_from(chunk.len()).unwrap_or(u64::MAX));
                        if read > MAXIMUM_FILE_BYTES {
                            break;
                        }
                    }
                }
                let _ = spool.remove(&artifact.handle).await;
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
