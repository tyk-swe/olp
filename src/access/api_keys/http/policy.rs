use std::collections::BTreeSet;
use std::num::NonZeroU32;
use std::num::NonZeroU64;

use crate::access::api_keys::records::ApiKeyRecord;
use crate::access::api_keys::records::UpdateApiKeyInput;
use crate::access::policy::ApiKeyLimits;
use crate::access::policy::ApiKeyScope;
use crate::ids::RouteSlug;
use chrono::DateTime;
use chrono::Utc;
use rust_decimal::Decimal;

use crate::http::problem::FieldErrors;
use crate::http::problem::Problem;

use crate::access::api_keys::http::create::CreateApiKeyRequest;
use crate::access::api_keys::http::manage::UpdateApiKeyRequest;

const MAX_NAME_CHARACTERS: usize = 100;
const MAX_U32_DATABASE_LIMIT: u32 = i32::MAX as u32;
const MAX_U64_DATABASE_LIMIT: u64 = i64::MAX as u64;
const COST_LIMIT_ERROR: &str =
    "Use a positive decimal with at most 12 integer and 12 fractional digits, or null.";

pub(crate) struct RawApiKeyPolicy<'a> {
    name: &'a str,
    scopes: &'a [String],
    allowed_routes: &'a [String],
    requests_per_minute: Option<u32>,
    tokens_per_minute: Option<u64>,
    max_concurrency: Option<u32>,
    daily_cost_limit: Option<&'a str>,
    monthly_cost_limit: Option<&'a str>,
    expires_at: Option<DateTime<Utc>>,
}

pub(crate) enum ExpirationValidation {
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
            daily_cost_limit: request.daily_cost_limit.as_deref(),
            monthly_cost_limit: request.monthly_cost_limit.as_deref(),
            expires_at: request.expires_at,
        }
    }
}

/// The stored policy a `PATCH` merges into.
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct ApiKeyPolicySnapshot {
    pub name: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub daily_cost_limit: Option<String>,
    pub monthly_cost_limit: Option<String>,
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
            daily_cost_limit: record.daily_cost_limit.map(|limit| limit.to_string()),
            monthly_cost_limit: record.monthly_cost_limit.map(|limit| limit.to_string()),
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
            daily_cost_limit: policy.daily_cost_limit.as_deref(),
            monthly_cost_limit: policy.monthly_cost_limit.as_deref(),
            expires_at: policy.expires_at,
        }
    }
}

/// Applies a merge patch to the stored policy. Fields the caller omitted keep
/// their stored value; an explicit `null` clears one. The flag reports whether
/// the caller asked for an expiry different from the stored one, which is the
/// only case where the expiry has to be in the future.
pub(crate) fn merge_api_key_policy(
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
        daily_cost_limit: request
            .daily_cost_limit
            .clone()
            .unwrap_or(stored.daily_cost_limit),
        monthly_cost_limit: request
            .monthly_cost_limit
            .clone()
            .unwrap_or(stored.monthly_cost_limit),
        expires_at: request.expires_at.unwrap_or(stored.expires_at),
    };
    (merged, expiration_changed)
}

#[derive(Debug, PartialEq)]
pub(crate) struct NormalizedApiKeyPolicy {
    pub name: String,
    pub scopes: Vec<ApiKeyScope>,
    pub allowed_routes: Vec<RouteSlug>,
    pub limits: ApiKeyLimits,
    pub expires_at: Option<DateTime<Utc>>,
}

impl NormalizedApiKeyPolicy {
    pub(crate) fn into_update_input(self) -> UpdateApiKeyInput {
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
            daily_cost_limit: self.limits.daily_cost_limit,
            monthly_cost_limit: self.limits.monthly_cost_limit,
            expires_at: self.expires_at,
        }
    }
}

pub(crate) fn normalize_api_key_policy(
    raw: RawApiKeyPolicy<'_>,
    expiration_validation: ExpirationValidation,
) -> Result<NormalizedApiKeyPolicy, Problem> {
    let mut errors = FieldErrors::new();
    let name = raw.name.trim().to_owned();
    if name.is_empty() || raw.name.chars().count() > MAX_NAME_CHARACTERS {
        errors.insert(
            "name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned().into()],
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
            vec!["Select at least one scope.".to_owned().into()],
        );
    } else if let Some(scope) = unknown_scope {
        errors.insert(
            "scopes".to_owned(),
            vec![format!("Unknown scope {scope}.").into()],
        );
    } else if scopes.iter().copied().collect::<BTreeSet<_>>().len() != scopes.len() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Scope entries must be unique.".to_owned().into()],
        );
    }

    let allowed_routes = normalize_allowed_routes(raw.allowed_routes, &mut errors);

    let requests_per_minute =
        normalize_u32_limit(&mut errors, "requests_per_minute", raw.requests_per_minute);
    let tokens_per_minute =
        normalize_u64_limit(&mut errors, "tokens_per_minute", raw.tokens_per_minute);
    let max_concurrency = normalize_u32_limit(&mut errors, "max_concurrency", raw.max_concurrency);
    let daily_cost_limit =
        normalize_cost_limit(&mut errors, "daily_cost_limit", raw.daily_cost_limit);
    let monthly_cost_limit =
        normalize_cost_limit(&mut errors, "monthly_cost_limit", raw.monthly_cost_limit);

    if let ExpirationValidation::RequireFuture(now) = expiration_validation
        && raw.expires_at.is_some_and(|expiration| expiration <= now)
    {
        errors.insert(
            "expires_at".to_owned(),
            vec![
                "Expiration must be in the future or null."
                    .to_owned()
                    .into(),
            ],
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
            daily_cost_limit,
            monthly_cost_limit,
        },
        expires_at: raw.expires_at,
    })
}

fn normalize_allowed_routes(routes: &[String], errors: &mut FieldErrors) -> Vec<RouteSlug> {
    let mut allowed_routes = Vec::with_capacity(routes.len());
    let mut invalid_route = None;
    for route in routes {
        match RouteSlug::parse(route.clone()) {
            Ok(route) => allowed_routes.push(route),
            Err(error) if invalid_route.is_none() => invalid_route = Some(error),
            Err(_) => {}
        }
    }
    if let Some(error) = invalid_route {
        errors.insert("allowed_routes".to_owned(), vec![error.to_string().into()]);
    } else if allowed_routes
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .len()
        != allowed_routes.len()
    {
        errors.insert(
            "allowed_routes".to_owned(),
            vec!["Route allowlist entries must be unique.".to_owned().into()],
        );
    }

    allowed_routes
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
                vec!["Use a positive limit or null.".to_owned().into()],
            );
            None
        }
        Some(value) if value > MAX_U32_DATABASE_LIMIT => {
            errors.insert(
                field.to_owned(),
                vec![
                    format!("Use a limit no greater than {MAX_U32_DATABASE_LIMIT} or null.").into(),
                ],
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
                vec!["Use a positive limit or null.".to_owned().into()],
            );
            None
        }
        Some(value) if value > MAX_U64_DATABASE_LIMIT => {
            errors.insert(
                field.to_owned(),
                vec![
                    format!("Use a limit no greater than {MAX_U64_DATABASE_LIMIT} or null.").into(),
                ],
            );
            None
        }
        value => value.and_then(NonZeroU64::new),
    }
}

fn normalize_cost_limit(
    errors: &mut FieldErrors,
    field: &str,
    value: Option<&str>,
) -> Option<Decimal> {
    let value = value?.trim();
    let mut parts = value.split('.');
    let integer = parts.next().unwrap_or_default();
    let fraction = parts.next();
    let valid = !value.is_empty()
        && !value.starts_with('-')
        && parts.next().is_none()
        && !integer.is_empty()
        && integer.len() <= 12
        && integer.bytes().all(|byte| byte.is_ascii_digit())
        && fraction.is_none_or(|part| {
            !part.is_empty() && part.len() <= 12 && part.bytes().all(|byte| byte.is_ascii_digit())
        });
    let parsed = valid.then(|| value.parse::<Decimal>().ok()).flatten();
    if parsed.is_some_and(|limit| limit > Decimal::ZERO) {
        return parsed;
    }
    errors.insert(field.to_owned(), vec![COST_LIMIT_ERROR.to_owned().into()]);
    None
}

type CostLimitPatch = Option<Option<Decimal>>;

pub(crate) fn normalize_cost_limit_patch(
    daily: Option<Option<&str>>,
    monthly: Option<Option<&str>>,
) -> Result<(CostLimitPatch, CostLimitPatch), Problem> {
    let mut errors = FieldErrors::new();
    let daily = daily.map(|value| {
        value.and_then(|value| normalize_cost_limit(&mut errors, "daily_cost_limit", Some(value)))
    });
    let monthly = monthly.map(|value| {
        value.and_then(|value| normalize_cost_limit(&mut errors, "monthly_cost_limit", Some(value)))
    });
    if errors.is_empty() {
        Ok((daily, monthly))
    } else {
        Err(Problem::validation(errors))
    }
}

#[cfg(test)]
mod tests;
