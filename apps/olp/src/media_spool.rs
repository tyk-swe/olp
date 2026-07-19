use std::{
    collections::BTreeMap,
    future::Future,
    path::{Path, PathBuf},
    sync::{
        Arc, RwLock,
        atomic::{AtomicU64, Ordering},
    },
};

use bytes::Bytes;
use futures::{StreamExt as _, stream};
use olp_domain::{
    BoxFuture, MediaArtifact, MediaByteStream, MediaHandle, MediaSpool, MediaSpoolError,
    MediaUpload, OpenedMedia,
};
use tokio::{
    fs::{self, File, OpenOptions},
    io::{AsyncReadExt as _, AsyncWriteExt as _},
};
use uuid::Uuid;

const READ_CHUNK_BYTES: usize = 64 * 1024;
pub(crate) const DEFAULT_CAPACITY_BYTES: u64 = 1024 * 1024 * 1024;
/// The smallest supported production spool. Multipart admission reserves
/// fixed worst-case endpoint budgets, so a smaller volume cannot safely serve
/// the public media API.
pub const MIN_MEDIA_SPOOL_CAPACITY_BYTES: u64 = 256 * 1024 * 1024;

#[derive(Clone, Debug)]
struct Entry {
    path: PathBuf,
    filename: String,
    content_type: Option<String>,
    content_length: u64,
}

/// Per-process, private filesystem spool for bounded request and response
/// media. Handles are random identifiers and never expose filesystem paths.
pub(crate) struct FileMediaSpool {
    root: PathBuf,
    entries: Arc<RwLock<BTreeMap<String, Entry>>>,
    used_bytes: Arc<AtomicU64>,
    capacity_bytes: u64,
}

/// Returns reserved capacity to the shared accounting counter, asserting the
/// bookkeeping never underflows. Shared by direct releases and the detached
/// removal task so the two paths cannot drift apart.
fn release_used_bytes(used_bytes: &AtomicU64, bytes: u64) {
    if bytes != 0 {
        let previous = used_bytes.fetch_sub(bytes, Ordering::AcqRel);
        debug_assert!(previous >= bytes, "media spool accounting underflow");
    }
}

impl FileMediaSpool {
    pub(crate) fn create() -> std::io::Result<Arc<Self>> {
        Self::create_at(&std::env::temp_dir(), DEFAULT_CAPACITY_BYTES)
    }

    fn create_at(base_dir: &Path, capacity_bytes: u64) -> std::io::Result<Arc<Self>> {
        if capacity_bytes == 0 {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "media spool capacity must be greater than zero",
            ));
        }
        std::fs::create_dir_all(base_dir)?;
        let root = base_dir.join(format!(
            "olp-media-{}-{}",
            std::process::id(),
            Uuid::now_v7()
        ));
        create_private_directory(&root)?;
        Ok(Arc::new(Self {
            root,
            entries: Arc::new(RwLock::new(BTreeMap::new())),
            used_bytes: Arc::new(AtomicU64::new(0)),
            capacity_bytes,
        }))
    }

    fn reserve(&self, bytes: u64) -> bool {
        self.used_bytes
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(bytes)
                    .filter(|next| *next <= self.capacity_bytes)
            })
            .is_ok()
    }

    fn release(&self, bytes: u64) {
        release_used_bytes(&self.used_bytes, bytes);
    }

    fn entry(&self, handle: &MediaHandle) -> Result<Entry, MediaSpoolError> {
        validate_handle(handle.as_str())?;
        self.entries
            .read()
            .map_err(|_| MediaSpoolError::Unavailable)?
            .get(handle.as_str())
            .cloned()
            .ok_or(MediaSpoolError::NotFound)
    }
}

struct PendingWrite<'a> {
    spool: &'a FileMediaSpool,
    path: PathBuf,
    reserved: u64,
    committed: bool,
}

impl PendingWrite<'_> {
    fn reserve(&mut self, bytes: u64) -> Result<(), MediaSpoolError> {
        if !self.spool.reserve(bytes) {
            return Err(MediaSpoolError::Unavailable);
        }
        self.reserved += bytes;
        Ok(())
    }

    fn commit(mut self) {
        self.committed = true;
    }
}

impl Drop for PendingWrite<'_> {
    fn drop(&mut self) {
        if !self.committed {
            self.spool.release(self.reserved);
            let _ = std::fs::remove_file(&self.path);
        }
    }
}

/// Owns removed bookkeeping in the unlink task, which continues after the
/// request awaiting removal is canceled.
struct PendingRemoval {
    entries: Arc<RwLock<BTreeMap<String, Entry>>>,
    used_bytes: Arc<AtomicU64>,
    handle: String,
    entry: Option<Entry>,
}

impl PendingRemoval {
    fn release(mut self) {
        let entry = self
            .entry
            .take()
            .expect("pending media removal always owns an entry");
        release_used_bytes(&self.used_bytes, entry.content_length);
    }

    fn restore(&mut self) -> Result<(), MediaSpoolError> {
        let mut entries = self
            .entries
            .write()
            .map_err(|_| MediaSpoolError::Unavailable)?;
        let entry = self
            .entry
            .take()
            .expect("pending media removal always owns an entry");
        entries.insert(self.handle.clone(), entry);
        Ok(())
    }
}

impl Drop for PendingRemoval {
    fn drop(&mut self) {
        let Some(entry) = self.entry.take() else {
            return;
        };
        if let Ok(mut entries) = self.entries.write() {
            entries.insert(self.handle.clone(), entry);
        }
    }
}

/// Creates a private, capacity-bounded filesystem spool below `base_dir`.
///
/// The bound is enforced atomically across concurrent uploads. Deployment
/// manifests should give the backing volume additional headroom for filesystem
/// metadata and writes already in flight at the operating-system boundary.
pub fn create_media_spool(
    base_dir: &Path,
    capacity_bytes: u64,
) -> std::io::Result<Arc<dyn MediaSpool>> {
    if capacity_bytes < MIN_MEDIA_SPOOL_CAPACITY_BYTES {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("media spool capacity must be at least {MIN_MEDIA_SPOOL_CAPACITY_BYTES} bytes"),
        ));
    }
    FileMediaSpool::create_at(base_dir, capacity_bytes).map(|spool| spool as Arc<dyn MediaSpool>)
}

/// Creates the production bounded filesystem spool for local conformance and
/// fuzz harnesses without exposing its private path or concrete type.
#[cfg(any(test, feature = "test-util"))]
pub fn create_bounded_media_spool_for_test() -> std::io::Result<Arc<dyn MediaSpool>> {
    FileMediaSpool::create().map(|spool| spool as Arc<dyn MediaSpool>)
}

impl MediaSpool for FileMediaSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        Some(self.capacity_bytes)
    }

    fn put<'a>(
        &'a self,
        mut upload: MediaUpload,
    ) -> BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async move {
            if upload.maximum_length == 0 {
                return Err(MediaSpoolError::ZeroLimit);
            }
            let filename = safe_filename(&upload.filename)?;
            let token = Uuid::now_v7().simple().to_string();
            let path = self.root.join(&token);
            // Declare the cleanup guard before the file so cancellation drops
            // the open handle first (required for removal on Windows too).
            let mut pending = PendingWrite {
                spool: self,
                path: path.clone(),
                reserved: 0,
                committed: false,
            };
            let mut file = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&path)
                .await
                .map_err(|_| MediaSpoolError::Unavailable)?;
            let mut written = 0_u64;
            while let Some(chunk) = upload.bytes.next().await {
                let chunk = chunk?;
                let length = u64::try_from(chunk.len()).unwrap_or(u64::MAX);
                let Some(next_written) = written.checked_add(length) else {
                    return Err(MediaSpoolError::TooLarge {
                        maximum: upload.maximum_length,
                    });
                };
                if next_written > upload.maximum_length {
                    return Err(MediaSpoolError::TooLarge {
                        maximum: upload.maximum_length,
                    });
                }
                pending.reserve(length)?;
                if file.write_all(&chunk).await.is_err() {
                    return Err(MediaSpoolError::Unavailable);
                }
                written = next_written;
            }
            if file.flush().await.is_err() {
                return Err(MediaSpoolError::Unavailable);
            }
            drop(file);
            let handle = MediaHandle::new(token.clone());
            let Ok(mut entries) = self.entries.write() else {
                return Err(MediaSpoolError::Unavailable);
            };
            entries.insert(
                token,
                Entry {
                    path,
                    filename,
                    content_type: upload.content_type.clone(),
                    content_length: written,
                },
            );
            pending.commit();
            Ok(MediaArtifact {
                handle,
                content_type: upload.content_type,
                content_length: Some(written),
            })
        })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        Box::pin(async move {
            let entry = self.entry(handle)?;
            let file = File::open(&entry.path)
                .await
                .map_err(|error| match error.kind() {
                    std::io::ErrorKind::NotFound => MediaSpoolError::NotFound,
                    _ => MediaSpoolError::Unavailable,
                })?;
            let bytes: MediaByteStream = Box::pin(stream::unfold(Some(file), |file| async move {
                let mut file = file?;
                let mut buffer = vec![0_u8; READ_CHUNK_BYTES];
                match file.read(&mut buffer).await {
                    Ok(0) => None,
                    Ok(read) => {
                        buffer.truncate(read);
                        Some((Ok(Bytes::from(buffer)), Some(file)))
                    }
                    Err(_) => Some((Err(MediaSpoolError::Unavailable), None)),
                }
            }));
            Ok(OpenedMedia {
                artifact: MediaArtifact {
                    handle: handle.clone(),
                    content_type: entry.content_type,
                    content_length: Some(entry.content_length),
                },
                filename: entry.filename,
                bytes,
            })
        })
    }

    fn remove<'a>(&'a self, handle: &'a MediaHandle) -> BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(self.remove_with(handle, fs::remove_file))
    }
}

impl FileMediaSpool {
    async fn remove_with<F, Fut>(
        &self,
        handle: &MediaHandle,
        unlink: F,
    ) -> Result<(), MediaSpoolError>
    where
        F: FnOnce(PathBuf) -> Fut + Send + 'static,
        Fut: Future<Output = std::io::Result<()>> + Send + 'static,
    {
        validate_handle(handle.as_str())?;
        let entry = self
            .entries
            .write()
            .map_err(|_| MediaSpoolError::Unavailable)?
            .remove(handle.as_str())
            .ok_or(MediaSpoolError::NotFound)?;
        let path = entry.path.clone();
        let pending = PendingRemoval {
            entries: Arc::clone(&self.entries),
            used_bytes: Arc::clone(&self.used_bytes),
            handle: handle.as_str().to_owned(),
            entry: Some(entry),
        };
        // Tokio's filesystem work can continue in its blocking pool after an
        // awaiting request is canceled. Keep the entry guard in this detached
        // task so successful unlink and capacity release stay coupled.
        tokio::spawn(async move {
            let mut pending = pending;
            match unlink(path).await {
                Ok(()) => {
                    pending.release();
                    Ok(())
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    pending.release();
                    Err(MediaSpoolError::NotFound)
                }
                Err(_) => {
                    // Preserve both the handle and its capacity reservation so
                    // a transient filesystem error cannot silently overbook
                    // the configured spool budget.
                    pending.restore()?;
                    Err(MediaSpoolError::Unavailable)
                }
            }
        })
        .await
        .map_err(|_| MediaSpoolError::Unavailable)?
    }
}

impl Drop for FileMediaSpool {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

fn safe_filename(value: &str) -> Result<String, MediaSpoolError> {
    let filename = Path::new(value)
        .file_name()
        .and_then(|name| name.to_str())
        .filter(|name| !name.is_empty() && *name != "." && *name != "..")
        .ok_or(MediaSpoolError::InvalidFilename)?;
    if filename.as_bytes().contains(&0)
        || filename.chars().any(char::is_control)
        || filename.len() > 255
    {
        return Err(MediaSpoolError::InvalidFilename);
    }
    Ok(filename.to_owned())
}

fn validate_handle(value: &str) -> Result<(), MediaSpoolError> {
    if value.len() != 32 || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(MediaSpoolError::InvalidHandle);
    }
    Ok(())
}

#[cfg(unix)]
fn create_private_directory(path: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt as _;

    let mut builder = std::fs::DirBuilder::new();
    builder.mode(0o700).create(path)
}

#[cfg(not(unix))]
fn create_private_directory(path: &Path) -> std::io::Result<()> {
    std::fs::create_dir(path)
}

#[cfg(test)]
mod tests {
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
        assert!(FileMediaSpool::create_at(&std::env::temp_dir(), 0).is_err());
        let spool = FileMediaSpool::create_at(&std::env::temp_dir(), 4).unwrap();
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

    #[tokio::test]
    async fn cancelled_unlink_completes_bookkeeping_after_physical_deletion() {
        let spool = FileMediaSpool::create_at(&std::env::temp_dir(), 4).unwrap();
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
            Entry {
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
}
