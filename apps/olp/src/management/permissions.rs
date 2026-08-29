use olp_db::authentication::SessionPrincipal;
use olp_engine::domain::auth::{Permission, Role};
use tracing::error;

use crate::public_http::problem::Problem;

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
