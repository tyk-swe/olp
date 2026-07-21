use std::{
    collections::BTreeSet,
    num::{NonZeroU32, NonZeroU64},
};

use chrono::{DateTime, Utc};
use olp_domain::{ApiKeyLimits, ApiKeyScope, RouteSlug};
use olp_storage::UpdateApiKeyInput;

use crate::{FieldErrors, Problem};

use super::{CreateApiKeyRequest, UpdateApiKeyRequest};

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

impl<'a> From<&'a UpdateApiKeyRequest> for RawApiKeyPolicy<'a> {
    fn from(request: &'a UpdateApiKeyRequest) -> Self {
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

#[derive(Debug, PartialEq)]
pub(super) struct NormalizedApiKeyPolicy {
    pub name: String,
    pub scopes: Vec<ApiKeyScope>,
    pub allowed_routes: Vec<RouteSlug>,
    pub limits: ApiKeyLimits,
    pub expires_at: Option<DateTime<Utc>>,
}

impl NormalizedApiKeyPolicy {
    pub fn into_update_input(self) -> UpdateApiKeyInput {
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
    use olp_storage::idempotency_fingerprint;
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
            name: request.name.clone(),
            scopes: request.scopes.clone(),
            allowed_routes: request.allowed_routes.clone(),
            requests_per_minute: request.requests_per_minute,
            tokens_per_minute: request.tokens_per_minute,
            max_concurrency: request.max_concurrency,
            expires_at: request.expires_at,
        }
    }

    fn normalize_create(request: &CreateApiKeyRequest) -> Result<NormalizedApiKeyPolicy, Problem> {
        normalize_api_key_policy(request.into(), ExpirationValidation::DeferredToStorage)
    }

    fn normalize_update(
        request: &UpdateApiKeyRequest,
        now: DateTime<Utc>,
    ) -> Result<NormalizedApiKeyPolicy, Problem> {
        normalize_api_key_policy(request.into(), ExpirationValidation::RequireFuture(now))
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
    fn create_scope_default_and_update_scope_requirement_are_unchanged() {
        let create: CreateApiKeyRequest = serde_json::from_value(json!({ "name": "key" })).unwrap();
        assert_eq!(create.scopes, ["inference"]);

        assert!(serde_json::from_value::<UpdateApiKeyRequest>(json!({ "name": "key" })).is_err());
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
            idempotency_fingerprint(&padded).unwrap(),
            idempotency_fingerprint(&trimmed).unwrap()
        );
    }
}
