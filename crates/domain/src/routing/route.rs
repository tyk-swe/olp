use std::{
    collections::HashSet,
    num::{NonZeroU16, NonZeroU32},
};

use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{DurationMs, OperationKind, ProviderId, RouteId, RouteSlug, TargetId};

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Target {
    pub id: TargetId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub routing_id: Option<TargetId>,
    pub provider_id: ProviderId,
    #[serde(rename = "provider_model")]
    pub upstream_model: String,
    pub priority: u16,
    pub weight: NonZeroU32,
    pub timeout: DurationMs,
}

impl Target {
    /// Identity that remains stable when a runtime revision replaces this
    /// target's configuration record.
    #[must_use]
    pub fn stable_id(&self) -> TargetId {
        self.routing_id.unwrap_or(self.id)
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Route {
    pub id: RouteId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub routing_id: Option<RouteId>,
    pub slug: RouteSlug,
    #[serde(default)]
    pub operations: std::collections::BTreeSet<OperationKind>,
    pub overall_timeout: DurationMs,
    pub max_attempts: NonZeroU16,
    pub targets: Vec<Target>,
}

impl Route {
    pub fn validate(&self) -> Result<(), RouteValidationError> {
        if self.overall_timeout.is_zero() {
            return Err(RouteValidationError::ZeroOverallTimeout);
        }
        if self.targets.is_empty() {
            return Err(RouteValidationError::NoTargets);
        }
        if usize::from(self.max_attempts.get()) > self.targets.len() {
            return Err(RouteValidationError::AttemptsExceedTargets);
        }

        let mut target_ids = HashSet::with_capacity(self.targets.len());
        for target in &self.targets {
            if target.timeout.is_zero() {
                return Err(RouteValidationError::ZeroTargetTimeout {
                    target_id: target.id,
                });
            }
            if target.timeout.get() > self.overall_timeout.get() {
                return Err(RouteValidationError::TargetTimeoutExceedsRoute {
                    target_id: target.id,
                });
            }
            if !target_ids.insert(target.id) {
                return Err(RouteValidationError::DuplicateTarget {
                    target_id: target.id,
                });
            }
        }

        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum RouteValidationError {
    #[error("route must contain at least one target")]
    NoTargets,
    #[error("route maximum attempts cannot exceed its target count")]
    AttemptsExceedTargets,
    #[error("route overall timeout must be greater than zero")]
    ZeroOverallTimeout,
    #[error("target {target_id} timeout must be greater than zero")]
    ZeroTargetTimeout { target_id: TargetId },
    #[error("target {target_id} timeout exceeds the route overall timeout")]
    TargetTimeoutExceedsRoute { target_id: TargetId },
    #[error("target ID {target_id} appears more than once")]
    DuplicateTarget { target_id: TargetId },
}
