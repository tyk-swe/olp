use crate::{error::Error, store::Store};

impl Store {
    /// Operator-chosen name for this installation. `None` until first-run
    /// setup creates the single installation row.
    pub async fn installation_name(&self) -> Result<Option<String>, Error> {
        Ok(
            sqlx::query_scalar!("SELECT installation_name FROM installation WHERE singleton")
                .fetch_optional(self.pool())
                .await?,
        )
    }
}
