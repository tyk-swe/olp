use olp_db::authentication::SessionPrincipal;
use olp_engine::domain::{Permission, Role};
use tracing::error;

use crate::Problem;

pub(crate) fn parse_user_role(role: &str) -> Result<Role, Problem> {
    role.parse().map_err(|_| {
        Problem::field_validation("role", "Use owner, operator, developer, or viewer.")
    })
}

pub(crate) fn require_permission(
    principal: &SessionPrincipal,
    permission: Permission,
) -> Result<(), Problem> {
    let role = principal.role.parse::<Role>().map_err(|_| {
        error!(user_id = %principal.user_id, "session contains an unknown fixed role");
        Problem::forbidden(
            "permission_denied",
            "The current role cannot perform this operation.",
        )
    })?;
    if role.allows(permission) {
        Ok(())
    } else {
        Err(Problem::forbidden(
            "permission_denied",
            "The current role cannot perform this operation.",
        ))
    }
}

pub(crate) fn require_provider_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageProviders)
}

pub(crate) fn require_key_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageApiKeys)
}

pub(crate) fn require_route_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageRoutes)
}
