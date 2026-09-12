//! OLP routing policy. Restrictions are cumulative; preferences never grant access.
use crate::ids::{ProviderId, RouteSlug};
use crate::providers::options::ConnectionOptions;
use rust_decimal::Decimal;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use utoipa::ToSchema;

#[derive(
    Clone, Copy, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq, Ord, PartialOrd,
)]
#[serde(rename_all = "snake_case")]
pub enum RoutingStrategy {
    #[default]
    Weighted,
    Price,
    Latency,
    Throughput,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct PriceCeiling {
    pub input_per_million: Option<String>,
    pub output_per_million: Option<String>,
    pub unit_price: Option<String>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct RoutingConstraints {
    pub only: Option<Vec<String>>,
    pub ignore: Vec<String>,
    pub regions: Option<BTreeSet<String>>,
    pub quantizations: Option<BTreeSet<String>>,
    pub deny_data_collection: bool,
    pub require_zero_data_retention: bool,
    pub require_parameters: bool,
    pub max_price: Option<PriceCeiling>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct RoutingPreferences {
    #[serde(skip)]
    #[schema(ignore)]
    pub cooling_slots: BTreeSet<uuid::Uuid>,
    #[serde(skip)]
    #[schema(ignore)]
    pub required_credential_version: Option<uuid::Uuid>,
    #[serde(flatten)]
    pub constraints: RoutingConstraints,
    pub order: Option<Vec<String>>,
    pub strategy: Option<RoutingStrategy>,
    pub allow_fallbacks: Option<bool>,
    pub preferred_max_latency_ms: Option<u64>,
    pub preferred_min_throughput: Option<u64>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct RoutingPolicy {
    pub constraints: RoutingConstraints,
    pub defaults: RoutingPreferences,
    pub allowed_strategies: Option<BTreeSet<RoutingStrategy>>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct RoutingConfiguration {
    pub version: u16,
    pub installation: RoutingPolicy,
    pub routes: BTreeMap<RouteSlug, RoutingPolicy>,
    pub providers: BTreeMap<ProviderId, ConnectionOptions>,
    pub credentials: BTreeMap<ProviderId, Vec<crate::providers::pool::CredentialSlot>>,
    /// Current published restrictions also constrain retained releases. Secret
    /// versions and transport configuration remain pinned to their release.
    #[serde(skip)]
    pub credential_authority:
        Option<BTreeMap<ProviderId, Vec<crate::providers::pool::CredentialSlot>>>,
    /// Explicitly revoked credential versions. A retained release keeps its
    /// pinned version and transport, but selection refuses the revoked version.
    #[serde(skip)]
    pub revoked_credential_versions: BTreeSet<uuid::Uuid>,
    #[serde(skip)]
    pub connection_limit_authority:
        Option<BTreeMap<ProviderId, crate::providers::options::ConnectionLimits>>,
    pub prices: Vec<RoutingPrice>,
}

#[derive(Clone, Debug, Deserialize, Serialize, ToSchema)]
pub struct RoutingPrice {
    pub effective_at: chrono::DateTime<chrono::Utc>,
    pub scope_priority: u8,
    pub revision: i32,
    pub revision_id: uuid::Uuid,
    pub provider_id: uuid::Uuid,
    pub model: String,
    pub operation: String,
    pub currency: String,
    pub input_per_million: Option<String>,
    pub output_per_million: Option<String>,
    pub unit_price: Option<String>,
}

pub fn decimal(value: &str) -> Result<Decimal, String> {
    crate::usage::pricing::validate_decimal(value)
        .map_err(|_| "Invalid non-negative decimal price")?;
    value.parse().map_err(|_| "Price is out of range".into())
}

pub fn validate_selector(selector: &str) -> Result<(), String> {
    if let Some(id) = selector.strip_prefix("vendor:")
        && crate::providers::catalog::vendor(id).is_some()
    {
        return Ok(());
    }
    if let Some(id) = selector.strip_prefix("provider:")
        && uuid::Uuid::parse_str(id).is_ok()
    {
        return Ok(());
    }
    Err("Use vendor:<catalog-id> or provider:<uuid> selectors".into())
}

pub fn matches(selector: &str, provider: ProviderId, vendor: Option<&str>) -> bool {
    selector
        .strip_prefix("provider:")
        .and_then(|s| uuid::Uuid::parse_str(s).ok())
        == Some(provider.as_uuid())
        || selector
            .strip_prefix("vendor:")
            .is_some_and(|s| Some(s) == vendor)
}

impl RoutingConstraints {
    pub fn validate(&self) -> Result<(), String> {
        if self.only.as_ref().is_some_and(Vec::is_empty)
            || self.regions.as_ref().is_some_and(BTreeSet::is_empty)
            || self.quantizations.as_ref().is_some_and(BTreeSet::is_empty)
        {
            return Err("An allowlist must not be empty".into());
        }
        for selectors in [self.only.as_deref().unwrap_or_default(), &self.ignore] {
            if selectors.len() > 100 {
                return Err("At most 100 selectors are allowed".into());
            }
            for selector in selectors {
                validate_selector(selector)?;
            }
        }
        if let Some(ceiling) = &self.max_price {
            for value in [
                &ceiling.input_per_million,
                &ceiling.output_per_million,
                &ceiling.unit_price,
            ]
            .into_iter()
            .flatten()
            {
                decimal(value)?;
            }
        }
        Ok(())
    }
}

impl RoutingPreferences {
    pub fn validate(&self) -> Result<(), String> {
        self.constraints.validate()?;
        if let Some(order) = &self.order {
            if order.is_empty() || order.len() > 100 {
                return Err("Order requires between one and 100 selectors".into());
            }
            for selector in order {
                validate_selector(selector)?;
            }
        }
        if self.preferred_max_latency_ms == Some(0) || self.preferred_min_throughput == Some(0) {
            return Err("Performance preferences must be positive".into());
        }
        Ok(())
    }

    pub fn from_headers(headers: &http::HeaderMap) -> Result<Self, String> {
        let values = headers.get_all("x-olp-routing");
        if values.iter().count() > 1 {
            return Err("Supply X-OLP-Routing once".into());
        }
        let Some(value) = values.iter().next() else {
            return Ok(Self::default());
        };
        let value: Self = serde_json::from_slice(value.as_bytes())
            .map_err(|_| "X-OLP-Routing must contain a valid routing preferences object")?;
        value.validate()?;
        Ok(value)
    }

    pub fn overlay(&mut self, other: &Self) {
        if other.order.is_some() {
            self.order.clone_from(&other.order);
        }
        if other.strategy.is_some() {
            self.strategy = other.strategy;
        }
        if other.allow_fallbacks.is_some() {
            self.allow_fallbacks = other.allow_fallbacks;
        }
        if other.preferred_max_latency_ms.is_some() {
            self.preferred_max_latency_ms = other.preferred_max_latency_ms;
        }
        if other.preferred_min_throughput.is_some() {
            self.preferred_min_throughput = other.preferred_min_throughput;
        }
    }
}

impl RoutingPolicy {
    pub fn validate(&self) -> Result<(), String> {
        self.constraints.validate()?;
        self.defaults.validate()?;
        if self
            .allowed_strategies
            .as_ref()
            .is_some_and(BTreeSet::is_empty)
        {
            return Err("At least one routing strategy must be allowed".into());
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn rejects_ambiguous_headers_and_invalid_selectors() {
        let mut headers = http::HeaderMap::new();
        headers.insert(
            "x-olp-routing",
            r#"{"only":["vendor:deepseek"],"strategy":"price"}"#
                .parse()
                .unwrap(),
        );
        assert!(RoutingPreferences::from_headers(&headers).is_ok());
        headers.append("x-olp-routing", "{}".parse().unwrap());
        assert!(RoutingPreferences::from_headers(&headers).is_err());
        assert!(validate_selector("vendor:made-up").is_err());
        for value in [
            r#"{"max_attempts":999}"#,
            r#"{"required_credential_version":"00000000-0000-0000-0000-000000000000"}"#,
        ] {
            let mut headers = http::HeaderMap::new();
            headers.insert("x-olp-routing", value.parse().unwrap());
            assert!(RoutingPreferences::from_headers(&headers).is_err());
        }
    }
    #[test]
    fn price_validation_preserves_decimal_precision() {
        assert_eq!(
            decimal("0.000000000001").unwrap().to_string(),
            "0.000000000001"
        );
        assert!(decimal("-1").is_err());
        assert!(decimal("NaN").is_err());
    }
}

impl RoutingConfiguration {
    pub fn price(
        &self,
        provider: uuid::Uuid,
        model: &str,
        operation: &str,
    ) -> Option<&RoutingPrice> {
        let now = chrono::Utc::now();
        self.prices
            .iter()
            .filter(|p| {
                p.provider_id == provider
                    && p.model == model
                    && p.operation == operation
                    && p.effective_at <= now
            })
            .max_by_key(|p| (p.scope_priority, p.effective_at, p.revision))
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize, ToSchema)]
pub struct AppliedRoutingPolicy {
    #[serde(default)]
    pub pricing_pinned: bool,
    pub vendor_id: Option<String>,
    pub strategy: RoutingStrategy,
    /// SHA-256 of the effective policy inputs; request preferences are never persisted.
    pub digest: String,
}
