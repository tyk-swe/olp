//! Shared fixtures for the integration-test binary. Per-test database
//! provisioning lives in `olp_db::test_support`.
#![allow(dead_code)]

pub(crate) mod route_fixtures;

/// First-owner setup input for an integration suite; `label` doubles as the
/// installation name and the owner email domain.
pub(crate) fn owner_setup(label: &str) -> olp_db::identity::InstallationSetupInput {
    olp_db::identity::InstallationSetupInput {
        installation_name: format!("{label} integration"),
        email: format!("owner@{label}.test"),
        display_name: "Owner".to_owned(),
        password_hash: "test-password-hash".to_owned(),
    }
}
