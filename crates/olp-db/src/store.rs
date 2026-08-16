use std::time::Duration;

use sqlx::{PgPool, postgres::PgPoolOptions};

use crate::error::Error;

/// Concrete PostgreSQL handle shared by storage subsystems.
///
/// Feature-specific queries are implemented in their owning modules; this
/// module owns only pool construction, access, migration, and liveness.
#[derive(Clone)]
pub struct Store {
    pool: PgPool,
}

impl Store {
    pub async fn connect(database_url: &str, max_connections: u32) -> Result<Self, Error> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections)
            .acquire_timeout(Duration::from_secs(5))
            .connect(database_url)
            .await?;
        Ok(Self { pool })
    }

    #[must_use]
    pub fn from_pool(pool: PgPool) -> Self {
        Self { pool }
    }

    #[must_use]
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn migrate(&self) -> Result<(), Error> {
        crate::MIGRATOR.run(&self.pool).await?;
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), Error> {
        sqlx::query_scalar!("SELECT 1::int AS \"value!\"")
            .fetch_one(&self.pool)
            .await?;
        Ok(())
    }
}
