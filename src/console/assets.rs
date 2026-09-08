use std::io;
use std::path::{Component, Path};

use serde::Deserialize;
use sha2::{Digest as _, Sha256};

#[derive(Deserialize)]
struct Manifest {
    version: u16,
    files: Vec<Asset>,
}

#[derive(Deserialize)]
struct Asset {
    path: String,
    sha256: String,
}

pub(crate) fn validate(console_dir: &Path) -> io::Result<()> {
    let manifest_path = console_dir.join("asset-manifest.json");
    if std::fs::metadata(&manifest_path)?.len() > 1_048_576 {
        return Err(io::Error::other("console asset manifest exceeds its limit"));
    }
    let manifest: Manifest = serde_json::from_slice(&std::fs::read(manifest_path)?)
        .map_err(|_| io::Error::other("console asset manifest is invalid"))?;
    if manifest.version != 1
        || !manifest
            .files
            .iter()
            .any(|asset| asset.path == "index.html")
    {
        return Err(io::Error::other(
            "console asset manifest lacks a supported entry point",
        ));
    }
    for asset in manifest.files {
        if asset.path.is_empty()
            || Path::new(&asset.path)
                .components()
                .any(|part| !matches!(part, Component::Normal(_)))
        {
            return Err(io::Error::other("console asset path is invalid"));
        }
        let path = console_dir.join(asset.path);
        if !std::fs::symlink_metadata(&path)?.file_type().is_file() {
            return Err(io::Error::other("console asset is not a regular file"));
        }
        let actual = checksum(&std::fs::read(path)?);
        if actual != asset.sha256 {
            return Err(io::Error::other(
                "console asset checksum mismatch; deploy the complete console bundle",
            ));
        }
    }
    Ok(())
}

fn checksum(bytes: &[u8]) -> String {
    crate::crypto::key_material::hex_lower(&Sha256::digest(bytes))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn incomplete_or_corrupt_bundles_are_rejected() {
        let root = std::env::temp_dir().join(format!("olp-assets-{}", uuid::Uuid::now_v7()));
        std::fs::create_dir(&root).unwrap();
        assert!(validate(&root).is_err());
        let index = b"<!doctype html><script>bootstrap()</script>";
        std::fs::write(root.join("index.html"), index).unwrap();
        let manifest = serde_json::json!({"version":1,"files":[{
            "path":"index.html", "sha256": checksum(index)
        }]});
        std::fs::write(root.join("asset-manifest.json"), manifest.to_string()).unwrap();
        validate(&root).unwrap();
        std::fs::write(root.join("index.html"), "wrong deployment").unwrap();
        assert!(validate(&root).is_err());
        std::fs::remove_file(root.join("index.html")).unwrap();
        assert!(validate(&root).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
}
