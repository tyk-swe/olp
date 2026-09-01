use crate::{error::Error, store::Store};

impl Store {
    pub async fn installation_id(&self) -> Result<uuid::Uuid, Error> {
        Ok(
            sqlx::query_scalar!("SELECT id FROM installation_identity WHERE singleton")
                .fetch_one(self.pool())
                .await?,
        )
    }

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
