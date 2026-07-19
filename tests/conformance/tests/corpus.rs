use std::{collections::BTreeSet, fs, path::Path};

use olp_conformance_fixtures::{MAX_FIXTURE_BYTES, fixture_root, read_fixture};

fn visit(directory: &Path, files: &mut Vec<std::path::PathBuf>) {
    for entry in fs::read_dir(directory)
        .unwrap_or_else(|error| panic!("failed to list {}: {error}", directory.display()))
    {
        let path = entry
            .expect("fixture directory entry must be readable")
            .path();
        if path.is_dir() {
            visit(&path, files);
        } else {
            files.push(path);
        }
    }
}

#[test]
fn corpus_is_bounded_and_all_json_is_well_formed() {
    let root = fixture_root();
    let mut files = Vec::new();
    visit(&root, &mut files);
    assert!(!files.is_empty(), "fixture corpus must not be empty");

    let allowed_extensions = BTreeSet::from(["json", "sse"]);
    for path in files {
        let relative = path.strip_prefix(&root).expect("fixture must be in root");
        let bytes = read_fixture(relative);
        assert!(
            bytes.len() as u64 <= MAX_FIXTURE_BYTES,
            "{} exceeds fixture limit",
            relative.display()
        );
        let extension = path
            .extension()
            .and_then(|value| value.to_str())
            .expect("fixture must have an extension");
        assert!(
            allowed_extensions.contains(extension),
            "unsupported fixture type: {}",
            relative.display()
        );
        if extension == "json" {
            serde_json::from_slice::<serde_json::Value>(&bytes)
                .unwrap_or_else(|error| panic!("invalid JSON in {}: {error}", relative.display()));
        } else {
            std::str::from_utf8(&bytes)
                .unwrap_or_else(|error| panic!("invalid UTF-8 in {}: {error}", relative.display()));
        }
    }
}
