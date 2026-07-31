use std::{
    fmt::Write as _,
    io,
    path::{Path, PathBuf},
    sync::Arc,
};

use serde::{Deserialize, Serialize};
use sha2::{Digest as _, Sha256};
use tokio::{
    fs::{self, OpenOptions},
    io::AsyncWriteExt as _,
    sync::Mutex,
};
use uuid::Uuid;

const JOURNAL_DIRECTORY_PREFIX: &str = "olp-job-journal-";

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub(crate) struct MediaJobRecovery {
    pub job_id: Uuid,
    pub upstream_job_id: String,
}

#[derive(Debug)]
pub(crate) struct MediaJobJournal {
    root: PathBuf,
    scan: Mutex<Option<fs::ReadDir>>,
}

impl MediaJobJournal {
    pub(crate) fn open(base_dir: &Path, database_url: &str) -> io::Result<Arc<Self>> {
        std::fs::create_dir_all(base_dir)?;
        let database_url = url::Url::parse(database_url)
            .map_err(|error| io::Error::new(io::ErrorKind::InvalidInput, error))?;
        let socket_host = database_url
            .query_pairs()
            .find_map(|(key, value)| (key == "host").then_some(value));
        let database_identity = format!(
            "{}|{}|{}|{}",
            database_url.scheme(),
            database_url
                .host_str()
                .or(socket_host.as_deref())
                .unwrap_or(""),
            database_url.port_or_known_default().unwrap_or(5432),
            database_url.path()
        );
        let database_hash = Sha256::digest(database_identity.as_bytes());
        let database_hash = database_hash.iter().fold(
            String::with_capacity(database_hash.len() * 2),
            |mut encoded, byte| {
                write!(&mut encoded, "{byte:02x}").expect("writing to a String cannot fail");
                encoded
            },
        );
        let root = base_dir.join(format!("{JOURNAL_DIRECTORY_PREFIX}{database_hash}"));
        ensure_private_directory(&root)?;
        Ok(Arc::new(Self {
            root,
            scan: Mutex::new(None),
        }))
    }

    pub(crate) async fn record(&self, job_id: Uuid, upstream_job_id: &str) -> io::Result<()> {
        if !olp_protocols::openai::valid_video_job_id(upstream_job_id) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "upstream video job ID is invalid",
            ));
        }
        let path = self.entry_path(job_id);
        match fs::read_to_string(&path).await {
            Ok(existing)
                if serde_json::from_str::<MediaJobRecovery>(&existing).is_ok_and(|entry| {
                    entry.job_id == job_id && entry.upstream_job_id == upstream_job_id
                }) =>
            {
                return sync_entry(path, self.root.clone()).await;
            }
            Ok(_) => {
                return Err(io::Error::new(
                    io::ErrorKind::AlreadyExists,
                    "media job journal identity conflicts with its existing entry",
                ));
            }
            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }

        let temporary = self.root.join(format!(".{job_id}.tmp-{}", Uuid::now_v7()));
        async {
            let mut options = OpenOptions::new();
            options.create_new(true).write(true);
            #[cfg(unix)]
            {
                options.mode(0o600);
            }
            let mut file = options.open(&temporary).await?;
            let entry = MediaJobRecovery {
                job_id,
                upstream_job_id: upstream_job_id.to_owned(),
            };
            file.write_all(&serde_json::to_vec(&entry).map_err(io::Error::other)?)
                .await?;
            file.sync_all().await?;
            drop(file);
            sync_directory(self.root.clone()).await?;
            match fs::hard_link(&temporary, &path).await {
                Ok(()) => {
                    sync_directory(self.root.clone()).await?;
                    fs::remove_file(&temporary).await?;
                    sync_directory(self.root.clone()).await
                }
                Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
                    let existing = fs::read_to_string(&path).await?;
                    if serde_json::from_str::<MediaJobRecovery>(&existing)
                        .is_ok_and(|existing| existing == entry)
                    {
                        fs::remove_file(&temporary).await?;
                        sync_entry(path, self.root.clone()).await
                    } else {
                        Err(io::Error::new(
                            io::ErrorKind::AlreadyExists,
                            "media job journal identity conflicts with its existing entry",
                        ))
                    }
                }
                Err(error) => Err(error),
            }
        }
        .await
    }

    pub(crate) async fn entries(
        &self,
        limit: usize,
        minimum_age: std::time::Duration,
    ) -> io::Result<Vec<MediaJobRecovery>> {
        let mut scan = self.scan.lock().await;
        if scan.is_none() {
            *scan = Some(fs::read_dir(&self.root).await?);
        }
        let mut entries = Vec::new();
        while entries.len() < limit {
            let next = match scan
                .as_mut()
                .expect("journal scan is initialized")
                .next_entry()
                .await
            {
                Ok(next) => next,
                Err(error) => {
                    *scan = None;
                    return Err(error);
                }
            };
            let Some(entry) = next else {
                *scan = None;
                break;
            };
            if !entry.file_type().await?.is_file() {
                continue;
            }
            if !entry
                .metadata()
                .await?
                .modified()
                .and_then(|modified| modified.elapsed().map_err(io::Error::other))
                .is_ok_and(|age| age >= minimum_age)
            {
                continue;
            }
            let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
                continue;
            };
            let (job_id, temporary) = match parse_entry_name(&name) {
                Some(entry) => entry,
                None => continue,
            };
            let recovery = match fs::read_to_string(entry.path()).await {
                Ok(value) => match serde_json::from_str::<MediaJobRecovery>(&value) {
                    Ok(recovery)
                        if recovery.job_id == job_id
                            && olp_protocols::openai::valid_video_job_id(
                                &recovery.upstream_job_id,
                            ) =>
                    {
                        recovery
                    }
                    _ => {
                        tracing::error!(%job_id, "media job recovery journal entry is invalid");
                        if temporary {
                            remove_temporary_entry(entry.path(), self.root.clone()).await;
                        }
                        continue;
                    }
                },
                Err(error) => {
                    tracing::error!(%job_id, %error, "failed to read media job recovery journal entry");
                    continue;
                }
            };
            if temporary {
                let canonical = self.entry_path(job_id);
                match fs::read_to_string(&canonical).await {
                    Ok(existing)
                        if serde_json::from_str::<MediaJobRecovery>(&existing)
                            .is_ok_and(|existing| existing == recovery) =>
                    {
                        match fs::remove_file(entry.path()).await {
                            Ok(()) => {}
                            Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                            Err(error) => {
                                tracing::error!(%job_id, %error, "failed to discard duplicate temporary media job identity");
                                continue;
                            }
                        }
                    }
                    Ok(_) => {
                        tracing::error!(%job_id, "temporary media job recovery identity conflicts with its journal entry");
                        continue;
                    }
                    Err(error) if error.kind() == io::ErrorKind::NotFound => {
                        match fs::rename(entry.path(), &canonical).await {
                            Ok(()) => {
                                if let Err(error) = sync_directory(self.root.clone()).await {
                                    tracing::error!(%job_id, %error, "failed to sync recovered media job identity");
                                    continue;
                                }
                            }
                            Err(error) => {
                                tracing::error!(%job_id, %error, "failed to promote temporary media job recovery identity");
                                continue;
                            }
                        }
                    }
                    Err(error) => {
                        tracing::error!(%job_id, %error, "failed to inspect media job recovery identity");
                        continue;
                    }
                }
            }
            sync_entry(self.entry_path(job_id), self.root.clone()).await?;
            entries.push(MediaJobRecovery {
                job_id,
                upstream_job_id: recovery.upstream_job_id,
            });
        }
        entries.sort_unstable_by_key(|entry| entry.job_id);
        Ok(entries)
    }

    pub(crate) async fn remove(&self, job_id: Uuid) -> io::Result<()> {
        match fs::remove_file(self.entry_path(job_id)).await {
            Ok(()) => sync_directory(self.root.clone()).await,
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(error),
        }
    }

    fn entry_path(&self, job_id: Uuid) -> PathBuf {
        self.root.join(format!("{job_id}.job"))
    }
}

fn parse_entry_name(name: &str) -> Option<(Uuid, bool)> {
    if let Some(job_id) = name.strip_suffix(".job") {
        return Uuid::parse_str(job_id).ok().map(|job_id| (job_id, false));
    }
    let (job_id, temporary_id) = name.strip_prefix('.')?.split_once(".tmp-")?;
    Uuid::parse_str(temporary_id).ok()?;
    Uuid::parse_str(job_id).ok().map(|job_id| (job_id, true))
}

#[cfg(unix)]
fn ensure_private_directory(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::{DirBuilderExt as _, PermissionsExt as _};

    let mut builder = std::fs::DirBuilder::new();
    match builder.mode(0o700).create(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {
            let metadata = std::fs::symlink_metadata(path)?;
            if !metadata.file_type().is_dir() || metadata.file_type().is_symlink() {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "media job journal path is not a directory",
                ));
            }
            std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
        }
        Err(error) => Err(error),
    }
}

#[cfg(not(unix))]
fn ensure_private_directory(path: &Path) -> io::Result<()> {
    match std::fs::create_dir(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists && path.is_dir() => Ok(()),
        Err(error) => Err(error),
    }
}

#[cfg(unix)]
async fn sync_directory(path: PathBuf) -> io::Result<()> {
    tokio::task::spawn_blocking(move || std::fs::File::open(path)?.sync_all())
        .await
        .map_err(io::Error::other)?
}

async fn sync_entry(path: PathBuf, directory: PathBuf) -> io::Result<()> {
    OpenOptions::new()
        .read(true)
        .open(path)
        .await?
        .sync_all()
        .await?;
    sync_directory(directory).await
}

async fn remove_temporary_entry(path: PathBuf, directory: PathBuf) {
    match fs::remove_file(path).await {
        Ok(()) => {
            if let Err(error) = sync_directory(directory).await {
                tracing::error!(%error, "failed to sync malformed media job journal cleanup");
            }
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => {
            tracing::error!(%error, "failed to remove malformed media job journal entry");
        }
    }
}

#[cfg(not(unix))]
async fn sync_directory(_path: PathBuf) -> io::Result<()> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn recovery_entry_survives_reopen_until_removed() {
        let base = tempfile::tempdir().unwrap();
        let job_id = Uuid::now_v7();
        let journal = MediaJobJournal::open(base.path(), "postgresql://journal-test").unwrap();
        journal.record(job_id, "video_recover-123").await.unwrap();
        drop(journal);

        let reopened = MediaJobJournal::open(base.path(), "postgresql://journal-test").unwrap();
        assert_eq!(
            reopened
                .entries(8, std::time::Duration::ZERO)
                .await
                .unwrap(),
            [MediaJobRecovery {
                job_id,
                upstream_job_id: "video_recover-123".to_owned(),
            }]
        );
        reopened.remove(job_id).await.unwrap();
        assert!(
            reopened
                .entries(8, std::time::Duration::ZERO)
                .await
                .unwrap()
                .is_empty()
        );
    }

    #[tokio::test]
    async fn fsynced_temporary_entry_is_promoted_for_recovery() {
        let base = tempfile::tempdir().unwrap();
        let job_id = Uuid::now_v7();
        let journal = MediaJobJournal::open(base.path(), "postgresql://journal-test").unwrap();
        let partial = journal
            .root
            .join(format!(".{job_id}.tmp-{}", Uuid::now_v7()));
        std::fs::write(&partial, br#"{"job_id":"#).unwrap();
        assert!(
            journal
                .entries(8, std::time::Duration::ZERO)
                .await
                .unwrap()
                .is_empty()
        );
        assert!(!partial.exists());

        let temporary = journal
            .root
            .join(format!(".{job_id}.tmp-{}", Uuid::now_v7()));
        let mut file = std::fs::File::create(&temporary).unwrap();
        let recovery = MediaJobRecovery {
            job_id,
            upstream_job_id: "video_recover-after-fsync".to_owned(),
        };
        std::io::Write::write_all(&mut file, &serde_json::to_vec(&recovery).unwrap()).unwrap();
        file.sync_all().unwrap();
        drop(file);

        assert_eq!(
            journal.entries(8, std::time::Duration::ZERO).await.unwrap(),
            [MediaJobRecovery {
                job_id,
                upstream_job_id: "video_recover-after-fsync".to_owned(),
            }]
        );
        assert!(journal.entry_path(job_id).is_file());
        assert!(!temporary.exists());
    }

    #[tokio::test]
    async fn concurrent_recovery_identities_cannot_overwrite_each_other() {
        let base = tempfile::tempdir().unwrap();
        let job_id = Uuid::now_v7();
        let journal = MediaJobJournal::open(base.path(), "postgresql://journal-test").unwrap();

        let (first, second) = tokio::join!(
            journal.record(job_id, "video_first-123"),
            journal.record(job_id, "video_second-456")
        );
        assert!(first.is_ok() ^ second.is_ok());
        let conflict = first.err().or_else(|| second.err()).unwrap();
        assert_eq!(conflict.kind(), io::ErrorKind::AlreadyExists);

        let entries = journal.entries(8, std::time::Duration::ZERO).await.unwrap();
        assert_eq!(entries.len(), 1);
        assert!(
            matches!(
                entries[0].upstream_job_id.as_str(),
                "video_first-123" | "video_second-456"
            ),
            "the durable winner must remain intact"
        );
    }

    #[tokio::test]
    async fn bounded_scans_advance_past_unresolved_entries() {
        let base = tempfile::tempdir().unwrap();
        let journal = MediaJobJournal::open(base.path(), "postgresql://journal-test").unwrap();
        for suffix in 0..3 {
            journal
                .record(Uuid::now_v7(), &format!("video_job-{suffix}"))
                .await
                .unwrap();
        }

        let mut seen = std::collections::BTreeSet::new();
        for _ in 0..3 {
            let entries = journal.entries(1, std::time::Duration::ZERO).await.unwrap();
            assert_eq!(entries.len(), 1);
            assert!(seen.insert(entries[0].job_id));
        }
    }
}
