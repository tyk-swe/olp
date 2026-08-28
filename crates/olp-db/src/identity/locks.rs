use uuid::Uuid;

/// The user row every account mutation locks first. A superset of the
/// columns the callers compare, so each site takes the same `FOR UPDATE` read.
pub(crate) struct LockedUser {
    pub(crate) etag: Uuid,
    pub(crate) email: String,
    pub(crate) display_name: String,
    pub(crate) role: String,
    pub(crate) active: bool,
    pub(crate) security_version: i64,
    pub(crate) has_local_password: bool,
}

/// Locks a user row for the rest of the transaction; `None` when it does not
/// exist, so the caller picks its own not-found error.
pub(crate) async fn lock_user(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: Uuid,
) -> Result<Option<LockedUser>, sqlx::Error> {
    sqlx::query_as!(
        LockedUser,
        "SELECT etag, email, display_name, role::text AS \"role!\", active, security_version, \
                password_hash IS NOT NULL AS \"has_local_password!\" \
         FROM users WHERE id = $1 FOR UPDATE",
        user_id
    )
    .fetch_optional(&mut **transaction)
    .await
}
