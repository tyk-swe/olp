use super::invitations::normalize_email;
use super::*;

#[test]
fn roles_are_a_closed_fixed_set() {
    for role in ["owner", "operator", "developer", "viewer"] {
        assert_eq!(role.parse::<Role>().unwrap().as_str(), role);
    }
    assert!("administrator".parse::<Role>().is_err());
}

#[test]
fn invitation_email_normalization_is_strict() {
    assert_eq!(
        normalize_email("  Person@Example.TEST ").unwrap(),
        "person@example.test"
    );
    assert!(normalize_email("not-an-email").is_err());
    assert!(normalize_email("@example.test").is_err());
}
