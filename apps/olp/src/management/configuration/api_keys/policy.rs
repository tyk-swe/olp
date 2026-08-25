use std::{
    collections::BTreeSet,
    num::{NonZeroU32, NonZeroU64},
};

use chrono::{DateTime, Utc};
use olp_db::configuration::resources::{ApiKeyRecord, UpdateApiKeyInput};
use olp_engine::domain::{
    auth::{ApiKeyLimits, ApiKeyScope},
    ids::RouteSlug,
};

use crate::{public_http::problem::FieldErrors, public_http::problem::Problem};

use super::{create::CreateApiKeyRequest, manage::UpdateApiKeyRequest};

const MAX_NAME_CHARACTERS: usize = 100;
const MAX_U32_DATABASE_LIMIT: u32 = i32::MAX as u32;
const MAX_U64_DATABASE_LIMIT: u64 = i64::MAX as u64;

pub(super) struct RawApiKeyPolicy<'a> {
    name: &'a str,
    scopes: &'a [String],
    allowed_routes: &'a [String],
    requests_per_minute: Option<u32>,
    tokens_per_minute: Option<u64>,
    max_concurrency: Option<u32>,
    expires_at: Option<DateTime<Utc>>,
}

pub(super) enum ExpirationValidation {
    // Create must reach storage's idempotency replay boundary before this time-dependent check.
    DeferredToStorage,
    // A patch that leaves `expires_at` at its stored value must not be rejected
    // over an expiry the caller never touched: an already-expired key would
    // otherwise be uneditable.
    Unchanged,
    RequireFuture(DateTime<Utc>),
}

impl<'a> From<&'a CreateApiKeyRequest> for RawApiKeyPolicy<'a> {
    fn from(request: &'a CreateApiKeyRequest) -> Self {
        Self {
            name: &request.name,
            scopes: &request.scopes,
            allowed_routes: &request.allowed_routes,
            requests_per_minute: request.requests_per_minute,
            tokens_per_minute: request.tokens_per_minute,
            max_concurrency: request.max_concurrency,
            expires_at: request.expires_at,
        }
    }
}

/// The stored policy a `PATCH` merges into.
#[derive(Clone, Debug, PartialEq)]
pub(super) struct ApiKeyPolicySnapshot {
    pub name: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub expires_at: Option<DateTime<Utc>>,
}

impl From<&ApiKeyRecord> for ApiKeyPolicySnapshot {
    fn from(record: &ApiKeyRecord) -> Self {
        Self {
            name: record.name.clone(),
            scopes: record.scopes.clone(),
            allowed_routes: record.allowed_routes.clone(),
            requests_per_minute: record
                .requests_per_minute
                .and_then(|limit| u32::try_from(limit).ok()),
            tokens_per_minute: record
                .tokens_per_minute
                .and_then(|limit| u64::try_from(limit).ok()),
            max_concurrency: record
                .max_concurrency
                .and_then(|limit| u32::try_from(limit).ok()),
            expires_at: record.expires_at,
        }
    }
}

impl<'a> From<&'a ApiKeyPolicySnapshot> for RawApiKeyPolicy<'a> {
    fn from(policy: &'a ApiKeyPolicySnapshot) -> Self {
        Self {
            name: &policy.name,
            scopes: &policy.scopes,
            allowed_routes: &policy.allowed_routes,
            requests_per_minute: policy.requests_per_minute,
            tokens_per_minute: policy.tokens_per_minute,
            max_concurrency: policy.max_concurrency,
            expires_at: policy.expires_at,
        }
    }
}

/// Applies a merge patch to the stored policy. Fields the caller omitted keep
/// their stored value; an explicit `null` clears one. The flag reports whether
/// the caller asked for an expiry different from the stored one, which is the
/// only case where the expiry has to be in the future.
pub(super) fn merge_api_key_policy(
    stored: ApiKeyPolicySnapshot,
    request: &UpdateApiKeyRequest,
) -> (ApiKeyPolicySnapshot, bool) {
    let expiration_changed = request
        .expires_at
        .is_some_and(|expires_at| expires_at != stored.expires_at);
    let merged = ApiKeyPolicySnapshot {
        name: request.name.clone().unwrap_or(stored.name),
        scopes: request.scopes.clone().unwrap_or(stored.scopes),
        allowed_routes: request
            .allowed_routes
            .clone()
            .unwrap_or(stored.allowed_routes),
        requests_per_minute: request
            .requests_per_minute
            .unwrap_or(stored.requests_per_minute),
        tokens_per_minute: request
            .tokens_per_minute
            .unwrap_or(stored.tokens_per_minute),
        max_concurrency: request.max_concurrency.unwrap_or(stored.max_concurrency),
        expires_at: request.expires_at.unwrap_or(stored.expires_at),
    };
    (merged, expiration_changed)
}

#[derive(Debug, PartialEq)]
pub(super) struct NormalizedApiKeyPolicy {
    pub name: String,
    pub scopes: Vec<ApiKeyScope>,
    pub allowed_routes: Vec<RouteSlug>,
    pub limits: ApiKeyLimits,
    pub expires_at: Option<DateTime<Utc>>,
}

impl NormalizedApiKeyPolicy {
    pub(super) fn into_update_input(self) -> UpdateApiKeyInput {
        UpdateApiKeyInput {
            name: self.name,
            scopes: self
                .scopes
                .into_iter()
                .map(|scope| scope.as_str().to_owned())
                .collect(),
            allowed_routes: self.allowed_routes.into_iter().map(Into::into).collect(),
            requests_per_minute: self.limits.requests_per_minute.map(NonZeroU32::get),
            tokens_per_minute: self.limits.tokens_per_minute.map(NonZeroU64::get),
            max_concurrency: self.limits.concurrency.map(NonZeroU32::get),
            expires_at: self.expires_at,
        }
    }
}

pub(super) fn normalize_api_key_policy(
    raw: RawApiKeyPolicy<'_>,
    expiration_validation: ExpirationValidation,
) -> Result<NormalizedApiKeyPolicy, Problem> {
    let mut errors = FieldErrors::new();
    let name = raw.name.trim().to_owned();
    if name.is_empty() || raw.name.chars().count() > MAX_NAME_CHARACTERS {
        errors.insert(
            "name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned()],
        );
    }

    let mut scopes = Vec::with_capacity(raw.scopes.len());
    let mut unknown_scope = None;
    for scope in raw.scopes {
        match scope.as_str() {
            "inference" => scopes.push(ApiKeyScope::Inference),
            "models_read" => scopes.push(ApiKeyScope::ModelsRead),
            _ if unknown_scope.is_none() => unknown_scope = Some(scope),
            _ => {}
        }
    }
    if raw.scopes.is_empty() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Select at least one scope.".to_owned()],
        );
    } else if let Some(scope) = unknown_scope {
        errors.insert("scopes".to_owned(), vec![format!("Unknown scope {scope}.")]);
    } else if scopes.iter().copied().collect::<BTreeSet<_>>().len() != scopes.len() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Scope entries must be unique.".to_owned()],
        );
    }

    let mut allowed_routes = Vec::with_capacity(raw.allowed_routes.len());
    let mut invalid_route = None;
    for route in raw.allowed_routes {
        match RouteSlug::parse(route.clone()) {
            Ok(route) => allowed_routes.push(route),
            Err(error) if invalid_route.is_none() => invalid_route = Some(error),
            Err(_) => {}
        }
    }
    if let Some(error) = invalid_route {
        errors.insert("allowed_routes".to_owned(), vec![error.to_string()]);
    } else if allowed_routes
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .len()
        != allowed_routes.len()
    {
        errors.insert(
            "allowed_routes".to_owned(),
            vec!["Route allowlist entries must be unique.".to_owned()],
        );
    }

    let requests_per_minute =
        normalize_u32_limit(&mut errors, "requests_per_minute", raw.requests_per_minute);
    let tokens_per_minute =
        normalize_u64_limit(&mut errors, "tokens_per_minute", raw.tokens_per_minute);
    let max_concurrency = normalize_u32_limit(&mut errors, "max_concurrency", raw.max_concurrency);

    if let ExpirationValidation::RequireFuture(now) = expiration_validation
        && raw.expires_at.is_some_and(|expiration| expiration <= now)
    {
        errors.insert(
            "expires_at".to_owned(),
            vec!["Expiration must be in the future or null.".to_owned()],
        );
    }

    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }

    Ok(NormalizedApiKeyPolicy {
        name,
        scopes,
        allowed_routes,
        limits: ApiKeyLimits {
            requests_per_minute,
            tokens_per_minute,
            concurrency: max_concurrency,
        },
        expires_at: raw.expires_at,
    })
}

fn normalize_u32_limit(
    errors: &mut FieldErrors,
    field: &str,
    value: Option<u32>,
) -> Option<NonZeroU32> {
    match value {
        Some(0) => {
            errors.insert(
                field.to_owned(),
                vec!["Use a positive limit or null.".to_owned()],
            );
            None
        }
        Some(value) if value > MAX_U32_DATABASE_LIMIT => {
            errors.insert(
                field.to_owned(),
                vec![format!(
                    "Use a limit no greater than {MAX_U32_DATABASE_LIMIT} or null."
                )],
            );
            None
        }
        value => value.and_then(NonZeroU32::new),
    }
}

fn normalize_u64_limit(
    errors: &mut FieldErrors,
    field: &str,
    value: Option<u64>,
) -> Option<NonZeroU64> {
    match value {
        Some(0) => {
            errors.insert(
                field.to_owned(),
                vec!["Use a positive limit or null.".to_owned()],
            );
            None
        }
        Some(value) if value > MAX_U64_DATABASE_LIMIT => {
            errors.insert(
                field.to_owned(),
                vec![format!(
                    "Use a limit no greater than {MAX_U64_DATABASE_LIMIT} or null."
                )],
            );
            None
        }
        value => value.and_then(NonZeroU64::new),
    }
}

#[cfg(test)]
mod tests {
    use chrono::Duration;
    use olp_db::idempotency::fingerprint;
    use serde_json::json;

    use super::*;

    fn create_request() -> CreateApiKeyRequest {
        CreateApiKeyRequest {
            name: "  SDK key  ".to_owned(),
            scopes: vec!["inference".to_owned(), "models_read".to_owned()],
            allowed_routes: vec!["primary-route".to_owned()],
            requests_per_minute: Some(60),
            tokens_per_minute: Some(10_000),
            max_concurrency: Some(4),
            expires_at: None,
        }
    }

    fn update_request(request: &CreateApiKeyRequest) -> UpdateApiKeyRequest {
        UpdateApiKeyRequest {
            name: Some(request.name.clone()),
            scopes: Some(request.scopes.clone()),
            allowed_routes: Some(request.allowed_routes.clone()),
            requests_per_minute: Some(request.requests_per_minute),
            tokens_per_minute: Some(request.tokens_per_minute),
            max_concurrency: Some(request.max_concurrency),
            expires_at: Some(request.expires_at),
        }
    }

    fn stored_policy() -> ApiKeyPolicySnapshot {
        ApiKeyPolicySnapshot {
            name: "stored key".to_owned(),
            scopes: vec!["inference".to_owned()],
            allowed_routes: vec!["stored-route".to_owned()],
            requests_per_minute: Some(30),
            tokens_per_minute: Some(5_000),
            max_concurrency: Some(2),
            expires_at: None,
        }
    }

    fn normalize_create(request: &CreateApiKeyRequest) -> Result<NormalizedApiKeyPolicy, Problem> {
        normalize_api_key_policy(request.into(), ExpirationValidation::DeferredToStorage)
    }

    fn normalize_patch(
        stored: ApiKeyPolicySnapshot,
        request: &UpdateApiKeyRequest,
        now: DateTime<Utc>,
    ) -> Result<NormalizedApiKeyPolicy, Problem> {
        let (merged, expiration_changed) = merge_api_key_policy(stored, request);
        let validation = if expiration_changed {
            ExpirationValidation::RequireFuture(now)
        } else {
            ExpirationValidation::Unchanged
        };
        normalize_api_key_policy(RawApiKeyPolicy::from(&merged), validation)
    }

    fn normalize_update(
        request: &UpdateApiKeyRequest,
        now: DateTime<Utc>,
    ) -> Result<NormalizedApiKeyPolicy, Problem> {
        normalize_patch(stored_policy(), request, now)
    }

    #[test]
    fn create_and_update_normalize_the_same_valid_policy() {
        let now = Utc::now();
        let expiration = now + Duration::hours(1);
        let mut create = create_request();
        create.expires_at = Some(expiration);
        let update = update_request(&create);

        let create_policy = normalize_create(&create).unwrap();
        let update_policy = normalize_update(&update, now).unwrap();

        assert_eq!(create_policy, update_policy);
        assert_eq!(create_policy.name, "SDK key");
        assert_eq!(
            create_policy.scopes,
            vec![ApiKeyScope::Inference, ApiKeyScope::ModelsRead]
        );
        assert_eq!(create_policy.allowed_routes[0].as_str(), "primary-route");
        assert_eq!(
            create_policy
                .limits
                .requests_per_minute
                .map(NonZeroU32::get),
            Some(60)
        );
        assert_eq!(
            create_policy.limits.tokens_per_minute.map(NonZeroU64::get),
            Some(10_000)
        );
        assert_eq!(
            create_policy.limits.concurrency.map(NonZeroU32::get),
            Some(4)
        );
        assert_eq!(create_policy.expires_at, Some(expiration));
    }

    #[test]
    fn create_and_update_return_the_same_static_field_errors() {
        let now = Utc::now();
        let create = CreateApiKeyRequest {
            name: "   ".to_owned(),
            scopes: vec!["admin".to_owned()],
            allowed_routes: vec!["same-route".to_owned(), "same-route".to_owned()],
            requests_per_minute: Some(0),
            tokens_per_minute: Some(0),
            max_concurrency: Some(0),
            expires_at: None,
        };
        let update = update_request(&create);

        let create_problem = normalize_create(&create).unwrap_err();
        let update_problem = normalize_update(&update, now).unwrap_err();

        assert_eq!(create_problem.status, 422);
        assert_eq!(
            create_problem.problem_type.as_ref(),
            "https://openllmproxy.dev/problems/validation_failed"
        );
        assert_eq!(create_problem.errors, update_problem.errors);
        assert_eq!(
            create_problem.errors["name"],
            ["Use between 1 and 100 characters."]
        );
        assert_eq!(create_problem.errors["scopes"], ["Unknown scope admin."]);
        assert_eq!(
            create_problem.errors["allowed_routes"],
            ["Route allowlist entries must be unique."]
        );
        for field in [
            "requests_per_minute",
            "tokens_per_minute",
            "max_concurrency",
        ] {
            assert_eq!(
                create_problem.errors[field],
                ["Use a positive limit or null."]
            );
        }
    }

    #[test]
    fn create_defers_expiration_validation_to_the_storage_replay_boundary() {
        let now = Utc::now();
        let expiration = now - Duration::hours(1);
        let mut create = create_request();
        create.expires_at = Some(expiration);
        let update = update_request(&create);

        let create_policy = normalize_create(&create).unwrap();
        assert_eq!(create_policy.expires_at, Some(expiration));

        let update_problem = normalize_update(&update, now).unwrap_err();
        assert_eq!(update_problem.errors.len(), 1);
        assert_eq!(
            update_problem.errors["expires_at"],
            ["Expiration must be in the future or null."]
        );
    }

    #[test]
    fn raw_name_length_is_bounded_before_the_name_is_trimmed() {
        let now = Utc::now();
        let mut create = create_request();
        create.name = format!(" {} ", "a".repeat(99));
        let update = update_request(&create);

        for problem in [
            normalize_create(&create).unwrap_err(),
            normalize_update(&update, now).unwrap_err(),
        ] {
            assert_eq!(
                problem.errors["name"],
                ["Use between 1 and 100 characters."]
            );
        }

        create.name = "a".repeat(100);
        assert_eq!(normalize_create(&create).unwrap().name.len(), 100);
    }

    #[test]
    fn scopes_and_route_allowlists_are_parsed_and_must_be_unique() {
        let mut create = create_request();
        create.scopes = vec!["inference".to_owned(), "inference".to_owned()];
        assert_eq!(
            normalize_create(&create).unwrap_err().errors["scopes"],
            ["Scope entries must be unique."]
        );

        create.scopes = vec!["inference".to_owned()];
        create.allowed_routes = vec!["Invalid Route".to_owned()];
        assert_eq!(
            normalize_create(&create).unwrap_err().errors["allowed_routes"],
            [
                "route slug must contain lowercase ASCII letters, digits, and single internal hyphens"
            ]
        );

        create.allowed_routes.clear();
        create.scopes.clear();
        assert_eq!(
            normalize_create(&create).unwrap_err().errors["scopes"],
            ["Select at least one scope."]
        );
    }

    #[test]
    fn limits_must_fit_the_positive_database_range() {
        let now = Utc::now();
        let mut create = create_request();
        create.requests_per_minute = Some(MAX_U32_DATABASE_LIMIT);
        create.tokens_per_minute = Some(MAX_U64_DATABASE_LIMIT);
        create.max_concurrency = Some(MAX_U32_DATABASE_LIMIT);
        assert!(normalize_create(&create).is_ok());

        create.requests_per_minute = Some(MAX_U32_DATABASE_LIMIT + 1);
        create.tokens_per_minute = Some(MAX_U64_DATABASE_LIMIT + 1);
        create.max_concurrency = Some(MAX_U32_DATABASE_LIMIT + 1);
        let update = update_request(&create);
        let create_problem = normalize_create(&create).unwrap_err();
        let update_problem = normalize_update(&update, now).unwrap_err();

        assert_eq!(create_problem.errors, update_problem.errors);
        assert_eq!(
            create_problem.errors["requests_per_minute"],
            ["Use a limit no greater than 2147483647 or null."]
        );
        assert_eq!(
            create_problem.errors["tokens_per_minute"],
            ["Use a limit no greater than 9223372036854775807 or null."]
        );
        assert_eq!(
            create_problem.errors["max_concurrency"],
            ["Use a limit no greater than 2147483647 or null."]
        );
    }

    #[test]
    fn create_defaults_scopes_and_a_patch_may_carry_only_the_fields_it_changes() {
        let create: CreateApiKeyRequest = serde_json::from_value(json!({ "name": "key" })).unwrap();
        assert_eq!(create.scopes, ["inference"]);

        let patch: UpdateApiKeyRequest =
            serde_json::from_value(json!({ "name": "renamed", "scopes": ["inference"] })).unwrap();
        assert_eq!(patch.name.as_deref(), Some("renamed"));
        assert_eq!(patch.allowed_routes, None);
        assert_eq!(patch.requests_per_minute, None);
        assert_eq!(patch.expires_at, None);
    }

    #[test]
    fn a_patch_never_widens_the_fields_it_omits() {
        let request: UpdateApiKeyRequest =
            serde_json::from_value(json!({ "name": "renamed", "scopes": ["inference"] })).unwrap();
        let (merged, expiration_changed) = merge_api_key_policy(stored_policy(), &request);

        assert!(!expiration_changed);
        assert_eq!(merged.name, "renamed");
        assert_eq!(merged.allowed_routes, ["stored-route"]);
        assert_eq!(merged.requests_per_minute, Some(30));
        assert_eq!(merged.tokens_per_minute, Some(5_000));
        assert_eq!(merged.max_concurrency, Some(2));

        let policy = normalize_patch(stored_policy(), &request, Utc::now()).unwrap();
        assert_eq!(policy.allowed_routes[0].as_str(), "stored-route");
        assert_eq!(
            policy.limits.requests_per_minute.map(NonZeroU32::get),
            Some(30)
        );
    }

    #[test]
    fn an_explicit_null_clears_a_limit_and_the_expiry() {
        let stored = ApiKeyPolicySnapshot {
            expires_at: Some(Utc::now() + Duration::hours(1)),
            ..stored_policy()
        };
        let request: UpdateApiKeyRequest = serde_json::from_value(json!({
            "requests_per_minute": null,
            "allowed_routes": [],
            "expires_at": null
        }))
        .unwrap();
        let (merged, expiration_changed) = merge_api_key_policy(stored, &request);

        assert!(expiration_changed);
        assert_eq!(merged.name, "stored key");
        assert_eq!(merged.requests_per_minute, None);
        assert_eq!(merged.tokens_per_minute, Some(5_000));
        assert!(merged.allowed_routes.is_empty());
        assert_eq!(merged.expires_at, None);
    }

    #[test]
    fn an_expired_key_stays_editable_until_the_expiry_itself_changes() {
        let expired = Utc::now() - Duration::hours(1);
        let stored = ApiKeyPolicySnapshot {
            expires_at: Some(expired),
            ..stored_policy()
        };
        let now = Utc::now();

        let renamed: UpdateApiKeyRequest =
            serde_json::from_value(json!({ "name": "renamed" })).unwrap();
        assert_eq!(
            normalize_patch(stored.clone(), &renamed, now).unwrap().name,
            "renamed"
        );

        let echoed = UpdateApiKeyRequest {
            name: Some("renamed".to_owned()),
            expires_at: Some(Some(expired)),
            ..UpdateApiKeyRequest::default()
        };
        assert_eq!(
            normalize_patch(stored.clone(), &echoed, now)
                .unwrap()
                .expires_at,
            Some(expired)
        );

        let moved = UpdateApiKeyRequest {
            expires_at: Some(Some(expired - Duration::minutes(1))),
            ..UpdateApiKeyRequest::default()
        };
        assert_eq!(
            normalize_patch(stored, &moved, now).unwrap_err().errors["expires_at"],
            ["Expiration must be in the future or null."]
        );
    }

    #[test]
    fn create_fingerprint_uses_raw_request_before_normalization() {
        let padded = create_request();
        let mut trimmed = create_request();
        trimmed.name = padded.name.trim().to_owned();

        assert_eq!(
            normalize_create(&padded).unwrap(),
            normalize_create(&trimmed).unwrap()
        );
        assert_ne!(
            fingerprint(&padded).unwrap(),
            fingerprint(&trimmed).unwrap()
        );
    }
}
