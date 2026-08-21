use std::{sync::Arc, time::Duration};

use futures::stream;
use tokio::sync::Notify;

use super::*;

#[tokio::test]
async fn enforces_streamed_limit_and_never_exposes_a_path() {
    let spool = FileMediaSpool::create().unwrap();
    assert_eq!(
        safe_filename("image.png\r\nX-Injected: true").unwrap_err(),
        MediaSpoolError::InvalidFilename
    );
    let error = spool
        .put(MediaUpload {
            filename: "../../secret.png".into(),
            content_type: Some("image/png".into()),
            maximum_length: 3,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"four"))])),
        })
        .await
        .unwrap_err();
    assert_eq!(error, MediaSpoolError::TooLarge { maximum: 3 });

    let artifact = spool
        .put(MediaUpload {
            filename: "../../image.png".into(),
            content_type: Some("image/png".into()),
            maximum_length: 4,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"data"))])),
        })
        .await
        .unwrap();
    assert_eq!(artifact.handle.as_str().len(), 32);
    assert!(!artifact.handle.as_str().contains('/'));
    let mut opened = spool.open(&artifact.handle).await.unwrap();
    assert_eq!(opened.filename, "image.png");
    assert_eq!(opened.bytes.next().await.unwrap().unwrap(), b"data"[..]);
    spool.remove(&artifact.handle).await.unwrap();
}

#[tokio::test]
async fn atomically_enforces_capacity_and_releases_it_on_remove() {
    assert!(FileMediaSpool::create_fresh_at(&std::env::temp_dir(), 0).is_err());
    let spool = FileMediaSpool::create_fresh_at(&std::env::temp_dir(), 4).unwrap();
    let first = spool
        .put(MediaUpload {
            filename: "first.bin".into(),
            content_type: None,
            maximum_length: 4,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"data"))])),
        })
        .await
        .unwrap();
    let rejected = spool
        .put(MediaUpload {
            filename: "second.bin".into(),
            content_type: None,
            maximum_length: 1,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"x"))])),
        })
        .await
        .unwrap_err();
    assert_eq!(rejected, MediaSpoolError::Unavailable);

    spool.remove(&first.handle).await.unwrap();
    let second = spool
        .put(MediaUpload {
            filename: "second.bin".into(),
            content_type: None,
            maximum_length: 1,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"x"))])),
        })
        .await
        .unwrap();
    spool.remove(&second.handle).await.unwrap();
}

#[test]
fn startup_accounts_for_orphaned_spool_bytes() {
    let base = tempfile::tempdir().unwrap();
    let orphaned_root = base.path().join("olp-media-orphaned");
    create_private_directory(&orphaned_root).unwrap();
    std::fs::write(orphaned_root.join("artifact"), b"old").unwrap();

    let spool = FileMediaSpool::create_at(base.path(), 4).unwrap();
    assert_eq!(spool.used_bytes.load(Ordering::Acquire), 3);
    assert!(spool.try_reserve_capacity(1));
    assert!(!spool.try_reserve_capacity(1));
}

#[tokio::test]
async fn failed_partial_cleanup_keeps_capacity_until_physical_deletion() {
    let spool = FileMediaSpool::create_fresh_at(&std::env::temp_dir(), 4).unwrap();
    let blocked_path = spool.root.join("blocked-partial");
    std::fs::create_dir(&blocked_path).unwrap();
    assert!(spool.try_reserve_capacity(4));
    drop(PendingSpoolWrite {
        spool: spool.as_ref(),
        path: blocked_path.clone(),
        reserved: 4,
        committed: false,
    });

    assert_eq!(spool.used_bytes.load(Ordering::Acquire), 4);
    assert_eq!(
        spool
            .put(MediaUpload {
                filename: "rejected.bin".into(),
                content_type: None,
                maximum_length: 1,
                bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"x"))])),
            })
            .await
            .unwrap_err(),
        MediaSpoolError::Unavailable
    );

    std::fs::remove_dir(blocked_path).unwrap();
    tokio::time::timeout(Duration::from_secs(1), async {
        while spool.used_bytes.load(Ordering::Acquire) != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("partial cleanup must release capacity after deletion succeeds");
}

#[tokio::test]
async fn failed_completed_cleanup_is_owned_by_the_janitor() {
    let spool = FileMediaSpool::create_fresh_at(&std::env::temp_dir(), 4).unwrap();
    let artifact = spool
        .put(MediaUpload {
            filename: "first.bin".into(),
            content_type: None,
            maximum_length: 4,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"data"))])),
        })
        .await
        .unwrap();

    assert_eq!(
        spool
            .remove_with(&artifact.handle, |_| async {
                Err(std::io::Error::new(
                    std::io::ErrorKind::PermissionDenied,
                    "injected unlink failure",
                ))
            })
            .await
            .unwrap_err(),
        MediaSpoolError::Unavailable
    );
    assert_eq!(
        spool.open(&artifact.handle).await.unwrap_err(),
        MediaSpoolError::NotFound
    );
    tokio::time::timeout(Duration::from_secs(1), async {
        while spool.used_bytes.load(Ordering::Acquire) != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("janitor must release capacity after retrying the unlink");

    let replacement = spool
        .put(MediaUpload {
            filename: "replacement.bin".into(),
            content_type: None,
            maximum_length: 4,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"data"))])),
        })
        .await
        .unwrap();
    spool.remove(&replacement.handle).await.unwrap();
}

#[tokio::test]
async fn cancelled_unlink_completes_bookkeeping_after_physical_deletion() {
    let spool = FileMediaSpool::create_fresh_at(&std::env::temp_dir(), 4).unwrap();
    let artifact = spool
        .put(MediaUpload {
            filename: "first.bin".into(),
            content_type: None,
            maximum_length: 4,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"data"))])),
        })
        .await
        .unwrap();
    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let entered_wait = entered.notified();
    let task = tokio::spawn({
        let spool = Arc::clone(&spool);
        let handle = artifact.handle.clone();
        let entered = Arc::clone(&entered);
        let release = Arc::clone(&release);
        async move {
            spool
                .remove_with(&handle, move |path| async move {
                    std::fs::remove_file(path)?;
                    entered.notify_one();
                    release.notified().await;
                    Ok(())
                })
                .await
        }
    });
    entered_wait.await;
    task.abort();
    let _ = task.await;

    assert_eq!(
        spool.open(&artifact.handle).await.unwrap_err(),
        MediaSpoolError::NotFound
    );
    let rejected = spool
        .put(MediaUpload {
            filename: "second.bin".into(),
            content_type: None,
            maximum_length: 1,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"x"))])),
        })
        .await
        .unwrap_err();
    assert_eq!(rejected, MediaSpoolError::Unavailable);
    release.notify_one();
    tokio::time::timeout(Duration::from_secs(1), async {
        while spool.used_bytes.load(Ordering::Acquire) != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("detached unlink must release capacity after caller cancellation");
    let second = spool
        .put(MediaUpload {
            filename: "second.bin".into(),
            content_type: None,
            maximum_length: 1,
            bytes: Box::pin(stream::iter([Ok(Bytes::from_static(b"x"))])),
        })
        .await
        .unwrap();
    spool.remove(&second.handle).await.unwrap();
}

#[cfg(unix)]
#[tokio::test]
async fn a_file_read_error_is_terminal() {
    let spool = FileMediaSpool::create().unwrap();
    let token = "a".repeat(32);
    spool.entries.write().unwrap().insert(
        token.clone(),
        SpoolEntry {
            path: spool.root.clone(),
            filename: "directory.bin".to_owned(),
            content_type: None,
            content_length: 1,
        },
    );

    let mut bytes = spool.open(&MediaHandle::new(token)).await.unwrap().bytes;
    assert!(matches!(
        bytes.next().await,
        Some(Err(MediaSpoolError::Unavailable))
    ));
    assert!(bytes.next().await.is_none());
}
