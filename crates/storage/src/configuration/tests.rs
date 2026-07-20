use super::providers::{database_version, stored_version};

#[test]
fn credential_versions_must_be_positive_database_integers() {
    assert_eq!(database_version(1).unwrap(), 1);
    assert_eq!(database_version(i32::MAX as u32).unwrap(), i32::MAX);
    assert!(database_version(0).is_err());
    assert!(database_version(i32::MAX as u32 + 1).is_err());

    assert_eq!(stored_version(1).unwrap(), 1);
    assert!(stored_version(0).is_err());
    assert!(stored_version(-1).is_err());
}
