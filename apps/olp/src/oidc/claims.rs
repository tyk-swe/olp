use chrono::Utc;
use jsonwebtoken::{
    Algorithm, DecodingKey, Validation, decode, decode_header,
    jwk::{Jwk, JwkSet, KeyOperations, PublicKeyUse},
};
use olp_storage::{OidcConfiguration, SessionMaterial};
use serde_json::Value;

use super::error::invalid_id_token;
use super::session::FLOW_TTL;
use crate::Problem;

#[derive(Debug)]
pub(super) struct ValidatedIdentity {
    pub(super) subject: String,
    pub(super) email: Option<String>,
    pub(super) email_verified: bool,
    pub(super) display_name: Option<String>,
    pub(super) groups: Vec<String>,
}

pub(super) fn validate_id_token(
    id_token: &str,
    jwks: &JwkSet,
    configuration: &OidcConfiguration,
    expected_nonce: &str,
) -> Result<ValidatedIdentity, Problem> {
    let header = decode_header(id_token).map_err(|_| invalid_id_token())?;
    if matches!(
        header.alg,
        Algorithm::HS256 | Algorithm::HS384 | Algorithm::HS512
    ) {
        return Err(invalid_id_token());
    }
    let kid = header.kid.as_deref().ok_or_else(invalid_id_token)?;
    let matching_keys = jwks
        .keys
        .iter()
        .filter(|key| key.common.key_id.as_deref() == Some(kid))
        .collect::<Vec<_>>();
    if matching_keys.len() != 1 {
        return Err(invalid_id_token());
    }
    let jwk = matching_keys[0];
    validate_jwk_for_signature(jwk, header.alg)?;
    let key = DecodingKey::from_jwk(jwk).map_err(|_| invalid_id_token())?;
    let mut validation = Validation::new(header.alg);
    validation.set_required_spec_claims(&["exp", "iss", "aud", "sub"]);
    validation.set_issuer(&[configuration.issuer.as_str()]);
    validation.set_audience(&[configuration.client_id.as_str()]);
    validation.validate_nbf = true;
    validation.leeway = 60;
    let claims = decode::<Value>(id_token, &key, &validation)
        .map_err(|_| invalid_id_token())?
        .claims;
    let issued_at = claims
        .get("iat")
        .and_then(Value::as_i64)
        .ok_or_else(invalid_id_token)?;
    let expires_at = claims
        .get("exp")
        .and_then(Value::as_i64)
        .ok_or_else(invalid_id_token)?;
    let now = Utc::now().timestamp();
    if issued_at > now + 60 || issued_at < now - FLOW_TTL.num_seconds() || expires_at <= issued_at {
        return Err(invalid_id_token());
    }
    let nonce = claims
        .get("nonce")
        .and_then(Value::as_str)
        .ok_or_else(invalid_id_token)?;
    let expected_nonce_digest = SessionMaterial::digest_token(expected_nonce);
    if !SessionMaterial::verify_csrf(nonce, &expected_nonce_digest) {
        return Err(invalid_id_token());
    }
    let audience_count = match claims.get("aud") {
        Some(Value::Array(values)) => values.len(),
        Some(Value::String(_)) => 1,
        _ => return Err(invalid_id_token()),
    };
    if audience_count > 1
        && claims.get("azp").and_then(Value::as_str) != Some(configuration.client_id.as_str())
    {
        return Err(invalid_id_token());
    }
    let subject = bounded_claim(&claims, "sub", 255)?.ok_or_else(invalid_id_token)?;
    let email = bounded_claim(&claims, &configuration.email_claim, 254)?;
    let email_verified = claims
        .get("email_verified")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    let display_name = bounded_claim(&claims, "name", 100)?;
    let groups = match claims.get(&configuration.groups_claim) {
        None => Vec::new(),
        Some(Value::Array(values)) if values.len() <= 200 => values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .filter(|group| {
                        !group.is_empty()
                            && group.len() <= 256
                            && !group.chars().any(char::is_control)
                    })
                    .map(str::to_owned)
                    .ok_or_else(invalid_id_token)
            })
            .collect::<Result<Vec<_>, _>>()?,
        Some(_) => return Err(invalid_id_token()),
    };
    Ok(ValidatedIdentity {
        subject,
        email,
        email_verified,
        display_name,
        groups,
    })
}

fn validate_jwk_for_signature(jwk: &Jwk, algorithm: Algorithm) -> Result<(), Problem> {
    if !matches!(
        jwk.common.public_key_use,
        None | Some(PublicKeyUse::Signature)
    ) || jwk
        .common
        .key_operations
        .as_ref()
        .is_some_and(|operations| !operations.contains(&KeyOperations::Verify))
        || jwk
            .common
            .key_algorithm
            .is_some_and(|declared| declared.to_string() != format!("{algorithm:?}"))
    {
        return Err(invalid_id_token());
    }
    Ok(())
}

pub(super) fn bounded_claim(
    claims: &Value,
    name: &str,
    maximum: usize,
) -> Result<Option<String>, Problem> {
    match claims.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if !value.is_empty()
                && value.len() <= maximum
                && !value.chars().any(char::is_control) =>
        {
            Ok(Some(value.clone()))
        }
        Some(_) => Err(invalid_id_token()),
    }
}

pub(super) fn is_allowed_algorithm_name(value: &str) -> bool {
    matches!(
        value,
        "RS256" | "RS384" | "RS512" | "PS256" | "PS384" | "PS512" | "ES256" | "ES384" | "EdDSA"
    )
}
