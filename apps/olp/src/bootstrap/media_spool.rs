use std::{
    collections::BTreeMap,
    future::Future,
    path::{Path, PathBuf},
    sync::{
        Arc, RwLock,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};

use bytes::Bytes;
use futures::{StreamExt as _, stream};
use olp_engine::domain::{
    canonical::{requests::MediaHandle, results::MediaArtifact},
    ports::{BoxFuture, MediaByteStream, MediaSpool, MediaSpoolError, MediaUpload, OpenedMedia},
};
use tokio::{
    fs::{self, File, OpenOptions},
    io::{AsyncReadExt as _, AsyncWriteExt as _},
    sync::Semaphore,
};
use tracing::warn;
use uuid::Uuid;

const READ_CHUNK_BYTES: usize = 64 * 1024;
const CLEANUP_CONCURRENCY: usize = 4;
const CLEANUP_INITIAL_BACKOFF: Duration = Duration::from_millis(25);
const CLEANUP_MAX_BACKOFF: Duration = Duration::from_secs(2);
pub(crate) const DEFAULT_CAPACITY_BYTES: u64 = 1024 * 1024 * 1024;
/// The smallest supported production spool. Multipart admission reserves
/// fixed worst-case endpoint budgets, so a smaller volume cannot safely serve
/// the public media API.
pub const MIN_MEDIA_SPOOL_CAPACITY_BYTES: u64 = 256 * 1024 * 1024;

#[derive(Clone, Debug)]
struct SpoolEntry {
    path: PathBuf,
    filename: String,
    content_type: Option<String>,
    content_length: u64,
}

/// Per-process, private filesystem spool for bounded request and response
/// media. Handles are random identifiers and never expose filesystem paths.
pub(crate) struct FileMediaSpool {
    root: PathBuf,
    entries: Arc<RwLock<BTreeMap<String, SpoolEntry>>>,
    used_bytes: Arc<AtomicU64>,
    janitor: Arc<SpoolJanitor>,
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

fn recovered_media_spool_bytes(base: &std::path::Path) -> std::io::Result<u64> {
    let mut total = 0_u64;

    let entries = match std::fs::read_dir(base) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(0);
        }
        Err(error) => return Err(error),
    };

    for entry in entries {
        let entry = match entry {
            Ok(entry) => entry,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error),
        };
        let file_type = match entry.file_type() {
            Ok(file_type) => file_type,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error),
        };
        if !file_type.is_dir()
            || !entry
                .file_name()
                .to_string_lossy()
                .starts_with("olp-media-")
        {
            continue;
        }

        let mut pending = vec![entry.path()];
        while let Some(directory) = pending.pop() {
            let children = match std::fs::read_dir(directory) {
                Ok(children) => children,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error),
            };
            for child in children {
                let child = match child {
                    Ok(child) => child,
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                    Err(error) => return Err(error),
                };
                let metadata = match child.path().symlink_metadata() {
                    Ok(metadata) => metadata,
                    Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                    Err(error) => return Err(error),
                };

                if metadata.file_type().is_dir() {
                    pending.push(child.path());
                } else if metadata.file_type().is_file() {
                    total = total.checked_add(metadata.len()).ok_or_else(|| {
                        std::io::Error::other("media spool byte accounting overflow")
                    })?;
                }
            }
        }
    }

    Ok(total)
}

impl FileMediaSpool {
    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn create() -> std::io::Result<Arc<Self>> {
        Self::create_fresh_at(&std::env::temp_dir(), DEFAULT_CAPACITY_BYTES)
    }

    fn create_at(base_dir: &Path, capacity_bytes: u64) -> std::io::Result<Arc<Self>> {
        Self::create_at_with_recovery(base_dir, capacity_bytes, true)
    }

    #[cfg(any(test, feature = "test-util"))]
    fn create_fresh_at(base_dir: &Path, capacity_bytes: u64) -> std::io::Result<Arc<Self>> {
        Self::create_at_with_recovery(base_dir, capacity_bytes, false)
    }

    fn create_at_with_recovery(
        base_dir: &Path,
        capacity_bytes: u64,
        recover_existing: bool,
    ) -> std::io::Result<Arc<Self>> {
        if capacity_bytes == 0 {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "media spool capacity must be greater than zero",
            ));
        }
        std::fs::create_dir_all(base_dir)?;
        let recovered_spool_bytes = if recover_existing {
            recovered_media_spool_bytes(base_dir)?
        } else {
            0
        };
        let root = base_dir.join(format!(
            "olp-media-{}-{}",
            std::process::id(),
            Uuid::now_v7()
        ));
        create_private_directory(&root)?;
        Ok(Arc::new(Self {
            root,
            entries: Arc::new(RwLock::new(BTreeMap::new())),
            used_bytes: Arc::new(AtomicU64::new(recovered_spool_bytes)),
            janitor: SpoolJanitor::new(),
            capacity_bytes,
        }))
    }

    fn try_reserve_capacity(&self, bytes: u64) -> bool {
        self.used_bytes
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(bytes)
                    .filter(|next| *next <= self.capacity_bytes)
            })
            .is_ok()
    }

    fn release_capacity(&self, bytes: u64) {
        release_used_bytes(&self.used_bytes, bytes);
    }

    fn lookup_entry(&self, handle: &MediaHandle) -> Result<SpoolEntry, MediaSpoolError> {
        validate_handle(handle.as_str())?;
        self.entries
            .read()
            .map_err(|_| MediaSpoolError::Unavailable)?
            .get(handle.as_str())
            .cloned()
            .ok_or(MediaSpoolError::NotFound)
    }
}

struct PendingSpoolWrite<'a> {
    spool: &'a FileMediaSpool,
    path: PathBuf,
    reserved: u64,
    committed: bool,
}

impl PendingSpoolWrite<'_> {
    fn reserve(&mut self, bytes: u64) -> Result<(), MediaSpoolError> {
        if !self.spool.try_reserve_capacity(bytes) {
            return Err(MediaSpoolError::Unavailable);
        }
        self.reserved += bytes;
        Ok(())
    }

    fn commit(mut self) {
        self.committed = true;
    }
}

impl Drop for PendingSpoolWrite<'_> {
    fn drop(&mut self) {
        if !self.committed {
            match std::fs::remove_file(&self.path) {
                Ok(()) => self.spool.release_capacity(self.reserved),
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    self.spool.release_capacity(self.reserved);
                }
                Err(error) => {
                    warn!(
                        %error,
                        "failed to remove an incomplete media spool write; scheduled for retry"
                    );
                    self.spool
                        .janitor
                        .enqueue_detached(SpoolCleanupJob::Partial(PartialSpoolRemoval {
                            path: self.path.clone(),
                            used_bytes: Arc::clone(&self.spool.used_bytes),
                            reserved: self.reserved,
                        }));
                }
            }
        }
    }
}

/// Owns removed bookkeeping in the unlink task, which continues after the
/// request awaiting removal is canceled.
struct PendingSpoolRemoval {
    entries: Arc<RwLock<BTreeMap<String, SpoolEntry>>>,
    used_bytes: Arc<AtomicU64>,
    handle: String,
    entry: Option<SpoolEntry>,
}

impl PendingSpoolRemoval {
    fn commit_removal(mut self) {
        let entry = self
            .entry
            .take()
            .expect("pending media removal always owns an entry");
        release_used_bytes(&self.used_bytes, entry.content_length);
    }
}

impl Drop for PendingSpoolRemoval {
    fn drop(&mut self) {
        let Some(entry) = self.entry.take() else {
            return;
        };
        if let Ok(mut entries) = self.entries.write() {
            entries.insert(self.handle.clone(), entry);
        }
    }
}

struct PartialSpoolRemoval {
    path: PathBuf,
    used_bytes: Arc<AtomicU64>,
    reserved: u64,
}

enum SpoolCleanupJob {
    Partial(PartialSpoolRemoval),
    Completed {
        path: PathBuf,
        pending: PendingSpoolRemoval,
    },
}

impl SpoolCleanupJob {
    fn path(&self) -> &Path {
        match self {
            Self::Partial(removal) => &removal.path,
            Self::Completed { path, .. } => path,
        }
    }

    fn commit(self) {
        match self {
            Self::Partial(removal) => {
                release_used_bytes(&removal.used_bytes, removal.reserved);
            }
            Self::Completed { pending, .. } => pending.commit_removal(),
        }
    }

    async fn run(self, concurrency: Arc<Semaphore>) {
        let path = self.path().to_owned();
        let mut backoff = CLEANUP_INITIAL_BACKOFF;
        let mut attempts = 0_u64;
        loop {
            let removal = {
                let permit = Arc::clone(&concurrency)
                    .acquire_owned()
                    .await
                    .expect("media spool janitor semaphore is never closed");
                let removal = fs::remove_file(&path).await;
                drop(permit);
                removal
            };
            match removal {
                Ok(()) => {
                    self.commit();
                    return;
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    self.commit();
                    return;
                }
                Err(error) => {
                    attempts = attempts.saturating_add(1);
                    if attempts == 1 || attempts.is_multiple_of(60) {
                        warn!(%error, attempts, "media spool janitor could not remove an artifact");
                    }
                    tokio::time::sleep(backoff).await;
                    backoff = backoff.saturating_mul(2).min(CLEANUP_MAX_BACKOFF);
                }
            }
        }
    }

    fn run_blocking(self) {
        let path = self.path().to_owned();
        let mut backoff = CLEANUP_INITIAL_BACKOFF;
        let mut attempts = 0_u64;
        loop {
            match std::fs::remove_file(&path) {
                Ok(()) => {
                    self.commit();
                    return;
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    self.commit();
                    return;
                }
                Err(error) => {
                    attempts = attempts.saturating_add(1);
                    if attempts == 1 || attempts.is_multiple_of(60) {
                        warn!(
                            %error,
                            attempts,
                            "media spool fallback janitor could not remove an artifact"
                        );
                    }
                    std::thread::sleep(backoff);
                    backoff = backoff.saturating_mul(2).min(CLEANUP_MAX_BACKOFF);
                }
            }
        }
    }
}

struct SpoolJanitor {
    concurrency: Arc<Semaphore>,
}

impl SpoolJanitor {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            concurrency: Arc::new(Semaphore::new(CLEANUP_CONCURRENCY)),
        })
    }

    fn enqueue_detached(&self, job: SpoolCleanupJob) {
        let Ok(runtime) = tokio::runtime::Handle::try_current() else {
            spawn_blocking_cleanup(job);
            return;
        };
        let concurrency = Arc::clone(&self.concurrency);
        runtime.spawn(job.run(concurrency));
    }
}

fn spawn_blocking_cleanup(job: SpoolCleanupJob) {
    if std::thread::Builder::new()
        .name("olp-media-janitor".to_owned())
        .spawn(move || job.run_blocking())
        .is_err()
    {
        warn!("failed to start the media spool fallback janitor");
    }
}

/// Creates a private, capacity-bounded filesystem spool below `base_dir`.
///
/// The bound is enforced atomically across concurrent uploads. Deployment
/// manifests should give the backing volume additional headroom for filesystem
/// metadata and writes already in flight at the operating-system boundary.
pub fn create(base_dir: &Path, capacity_bytes: u64) -> std::io::Result<Arc<dyn MediaSpool>> {
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
pub fn create_bounded_for_test() -> std::io::Result<Arc<dyn MediaSpool>> {
    FileMediaSpool::create().map(|spool| spool as Arc<dyn MediaSpool>)
}

impl MediaSpool for FileMediaSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        Some(self.capacity_bytes)
    }

    fn used_bytes(&self) -> Option<u64> {
        Some(self.used_bytes.load(Ordering::Acquire))
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
            let mut pending_write = PendingSpoolWrite {
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
                pending_write.reserve(length)?;
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
                SpoolEntry {
                    path,
                    filename,
                    content_type: upload.content_type.clone(),
                    content_length: written,
                },
            );
            pending_write.commit();
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
            let entry = self.lookup_entry(handle)?;
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
        let pending_removal = PendingSpoolRemoval {
            entries: Arc::clone(&self.entries),
            used_bytes: Arc::clone(&self.used_bytes),
            handle: handle.as_str().to_owned(),
            entry: Some(entry),
        };
        let janitor = Arc::clone(&self.janitor);
        // Tokio's filesystem work can continue in its blocking pool after an
        // awaiting request is canceled. Keep the entry guard in this detached
        // task so successful unlink and capacity release stay coupled.
        tokio::spawn(async move {
            match unlink(path.clone()).await {
                Ok(()) => {
                    pending_removal.commit_removal();
                    Ok(())
                }
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    pending_removal.commit_removal();
                    Err(MediaSpoolError::NotFound)
                }
                Err(error) => {
                    warn!(%error, "failed to remove a media spool artifact; scheduled for retry");
                    janitor.enqueue_detached(SpoolCleanupJob::Completed {
                        path,
                        pending: pending_removal,
                    });
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
mod tests;
