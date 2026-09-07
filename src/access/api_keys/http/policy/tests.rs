use crate::database::idempotency::fingerprint;
use chrono::Duration;
use serde_json::json;

use crate::access::api_keys::http::policy::*;

fn create_request() -> CreateApiKeyRequest {
    CreateApiKeyRequest {
        name: "  SDK key  ".to_owned(),
        scopes: vec!["inference".to_owned(), "models_read".to_owned()],
        allowed_routes: vec!["primary-route".to_owned()],
        requests_per_minute: Some(60),
        tokens_per_minute: Some(10_000),
        max_concurrency: Some(4),
        daily_cost_limit: Some("0.25".to_owned()),
        monthly_cost_limit: Some("5.00".to_owned()),
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
        daily_cost_limit: Some(request.daily_cost_limit.clone()),
        monthly_cost_limit: Some(request.monthly_cost_limit.clone()),
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
        daily_cost_limit: Some("0.10".to_owned()),
        monthly_cost_limit: Some("2.50".to_owned()),
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
    assert_eq!(
        create_policy.limits.daily_cost_limit,
        Some(Decimal::new(25, 2))
    );
    assert_eq!(
        create_policy.limits.monthly_cost_limit,
        Some(Decimal::new(500, 2))
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
        daily_cost_limit: Some("0".to_owned()),
        monthly_cost_limit: Some("-1".to_owned()),
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
        create_problem.errors["name"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Use between 1 and 100 characters."]
    );
    assert_eq!(
        create_problem.errors["scopes"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Unknown scope admin."]
    );
    assert_eq!(
        create_problem.errors["allowed_routes"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Route allowlist entries must be unique."]
    );
    for field in [
        "requests_per_minute",
        "tokens_per_minute",
        "max_concurrency",
    ] {
        assert_eq!(
            create_problem.errors[field]
                .iter()
                .map(|error| error.message.as_str())
                .collect::<Vec<_>>(),
            ["Use a positive limit or null."]
        );
    }
    for field in ["daily_cost_limit", "monthly_cost_limit"] {
        assert_eq!(
            create_problem.errors[field]
                .iter()
                .map(|error| error.message.as_str())
                .collect::<Vec<_>>(),
            [COST_LIMIT_ERROR]
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
        update_problem.errors["expires_at"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
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
            problem.errors["name"]
                .iter()
                .map(|error| error.message.as_str())
                .collect::<Vec<_>>(),
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
        normalize_create(&create).unwrap_err().errors["scopes"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Scope entries must be unique."]
    );

    create.scopes = vec!["inference".to_owned()];
    create.allowed_routes = vec!["Invalid Route".to_owned()];
    assert_eq!(
        normalize_create(&create).unwrap_err().errors["allowed_routes"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["route slug must contain lowercase ASCII letters, digits, and single internal hyphens"]
    );

    create.allowed_routes.clear();
    create.scopes.clear();
    assert_eq!(
        normalize_create(&create).unwrap_err().errors["scopes"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
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
        create_problem.errors["requests_per_minute"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Use a limit no greater than 2147483647 or null."]
    );
    assert_eq!(
        create_problem.errors["tokens_per_minute"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
        ["Use a limit no greater than 9223372036854775807 or null."]
    );
    assert_eq!(
        create_problem.errors["max_concurrency"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
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
    assert_eq!(patch.daily_cost_limit, None);
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
    assert_eq!(merged.daily_cost_limit.as_deref(), Some("0.10"));
    assert_eq!(merged.monthly_cost_limit.as_deref(), Some("2.50"));

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
        "daily_cost_limit": null,
        "allowed_routes": [],
        "expires_at": null
    }))
    .unwrap();
    let (merged, expiration_changed) = merge_api_key_policy(stored, &request);

    assert!(expiration_changed);
    assert_eq!(merged.name, "stored key");
    assert_eq!(merged.requests_per_minute, None);
    assert_eq!(merged.tokens_per_minute, Some(5_000));
    assert_eq!(merged.daily_cost_limit, None);
    assert_eq!(merged.monthly_cost_limit.as_deref(), Some("2.50"));
    assert!(merged.allowed_routes.is_empty());
    assert_eq!(merged.expires_at, None);
}

#[test]
fn cost_limits_reject_values_postgres_would_round_or_overflow() {
    for value in ["0", "-0.01", "0.0000000000001", "1000000000000"] {
        let mut create = create_request();
        create.daily_cost_limit = Some(value.to_owned());
        let update = update_request(&create);
        let create_problem = normalize_create(&create).unwrap_err();
        let update_problem = normalize_update(&update, Utc::now()).unwrap_err();
        assert_eq!(create_problem.status, 422, "{value}");
        assert_eq!(create_problem.errors, update_problem.errors, "{value}");
        assert_eq!(
            create_problem.errors["daily_cost_limit"]
                .iter()
                .map(|error| error.message.as_str())
                .collect::<Vec<_>>(),
            [COST_LIMIT_ERROR],
            "{value}"
        );
    }
    assert!(
        serde_json::from_value::<CreateApiKeyRequest>(json!({
            "name": "key",
            "daily_cost_limit": 0.01
        }))
        .is_err()
    );
}

#[test]
fn cost_limit_patch_preserves_omitted_and_distinguishes_null() {
    let request: UpdateApiKeyRequest = serde_json::from_value(json!({
        "daily_cost_limit": null,
        "monthly_cost_limit": "3.75"
    }))
    .unwrap();
    let (merged, _) = merge_api_key_policy(stored_policy(), &request);
    assert_eq!(merged.daily_cost_limit, None);
    assert_eq!(merged.monthly_cost_limit.as_deref(), Some("3.75"));
    let policy = normalize_patch(stored_policy(), &request, Utc::now()).unwrap();
    assert_eq!(policy.limits.daily_cost_limit, None);
    assert_eq!(policy.limits.monthly_cost_limit, Some(Decimal::new(375, 2)));
}

#[test]
fn rotate_cost_limit_patch_uses_the_same_validation() {
    assert_eq!(
        normalize_cost_limit_patch(Some(None), Some(Some("3.75"))).unwrap(),
        (Some(None), Some(Some(Decimal::new(375, 2))))
    );
    let problem = normalize_cost_limit_patch(Some(Some("1e2")), None).unwrap_err();
    assert_eq!(problem.status, 422);
    assert!(problem.errors.contains_key("daily_cost_limit"));
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
        normalize_patch(stored, &moved, now).unwrap_err().errors["expires_at"]
            .iter()
            .map(|error| error.message.as_str())
            .collect::<Vec<_>>(),
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
