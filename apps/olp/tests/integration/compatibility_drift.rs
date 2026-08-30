use olp::gateway::compatibility::{END_MARKER, START_MARKER, markdown};

const DOCUMENT: &str = include_str!("../../../../docs/compatibility.md");

#[test]
fn checked_in_compatibility_matrix_matches_the_endpoint_registry() {
    let generated = markdown();
    assert_eq!(
        generated_section(DOCUMENT),
        generated_section(&generated),
        "run `make compat` before committing endpoint or capability changes"
    );
}

/// The handwritten prose is spliced around the generated block, so a stray
/// second marker would silently truncate what `make compat` rewrites.
#[test]
fn the_document_carries_exactly_one_generated_block() {
    assert_eq!(DOCUMENT.matches(START_MARKER).count(), 1);
    assert_eq!(DOCUMENT.matches(END_MARKER).count(), 1);
}

fn generated_section(document: &str) -> &str {
    let start = document
        .find(START_MARKER)
        .expect("docs/compatibility.md opens a generated block");
    let end = document
        .find(END_MARKER)
        .expect("docs/compatibility.md closes a generated block");
    assert!(start < end, "the generated block markers are inverted");
    &document[start..end + END_MARKER.len()]
}
